package kube

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/agent/runtime/kube/kubeapi"
)

func TestTranslateDockerArgs(t *testing.T) {
	tr, err := translateDockerArgs([]string{"--gpus", `"device=1,3"`, "--ipc=host", "--ulimit", "memlock=-1", "--cap-add", "IPC_LOCK",
		"--shm-size=8g", "--group-add", "44", "-e", "A=b", "--network=host", "--device=/dev/infiniband"})
	if err != nil {
		t.Fatal(err)
	}
	if tr.Env["NVIDIA_VISIBLE_DEVICES"] != "1,3" || tr.Env["A"] != "b" || tr.RuntimeClass != "nvidia" || !tr.HostIPC || !tr.Privileged ||
		len(tr.CapAdd) != 1 || tr.CapAdd[0] != "IPC_LOCK" || tr.ShmBytes != 8<<30 || len(tr.Groups) != 1 || tr.Groups[0] != 44 ||
		!slices.Equal(tr.Devices, []string{"/dev/infiniband"}) {
		t.Fatalf("translation = %+v", tr)
	}
	tr, err = translateDockerArgs([]string{"--runtime", "ascend", "--pid", "host"})
	if err != nil || tr.RuntimeClass != "ascend" || !tr.HostPID || tr.Privileged {
		t.Fatalf("ascend translation = %+v, %v", tr, err)
	}
	for _, bad := range [][]string{{"--memory=1g"}, {"--network", "bridge"}, {"--gpus"}, {"-v", "relative:/x"}, {"-v", "/a:/b:z"}, {"-v", "/a"}, {"positional"}, {"--shm-size", "lots"}} {
		if _, err := translateDockerArgs(bad); err == nil {
			t.Fatalf("translateDockerArgs(%q) accepted", bad)
		}
	}
}

func TestInstancePod(t *testing.T) {
	spec := runtime.WorkloadSpec{
		Name: "gpuworks-0123456789ab", Image: "vllm/vllm-openai:v0.11", Entrypoint: []string{"vllm"}, Cmd: []string{"serve", "/model"},
		Env:        map[string]string{"Z": "1", "CUDA_VISIBLE_DEVICES": "0"},
		Mounts:     []runtime.Mount{{Source: "/opt/gpuworks/models/qwen", Target: "/model", ReadOnly: true}},
		DockerArgs: []string{"--gpus", `"device=2"`}, HostNetwork: true, ShmBytes: 1 << 30,
	}
	owner := &kubeapi.OwnerReference{APIVersion: "apps/v1", Kind: "DaemonSet", Name: DaemonSetName, UID: "ds"}
	pod, err := instancePod(spec, podOptions{Namespace: "gw", NodeName: "n1", RuntimeClass: "nvidia-cdi", PriorityClass: "gpuworks-inference", Owner: owner, Cards: []string{"GPU-2"}})
	if err != nil {
		t.Fatal(err)
	}
	s := pod.Spec
	c := s.Containers[0]
	if pod.Metadata.Name != spec.Name || pod.Metadata.Namespace != "gw" || pod.Metadata.Labels[LabelRole] != RoleInstance || pod.Metadata.Labels[LabelNode] != "n1" ||
		pod.Metadata.Annotations[AnnotationCards] != "GPU-2" || len(pod.Metadata.OwnerReferences) != 1 {
		t.Fatalf("metadata = %+v", pod.Metadata)
	}
	if s.NodeName != "n1" || s.RestartPolicy != "Never" || !s.HostNetwork || s.DNSPolicy != "ClusterFirstWithHostNet" ||
		s.RuntimeClassName == nil || *s.RuntimeClassName != "nvidia-cdi" || s.PriorityClassName != "gpuworks-inference" {
		t.Fatalf("spec = %+v", s)
	}
	if c.Name != ContainerName || c.Image != spec.Image || strings.Join(c.Command, " ") != "vllm" || strings.Join(c.Args, " ") != "serve /model" || c.SecurityContext != nil {
		t.Fatalf("container = %+v", c)
	}
	var env []string
	for _, e := range c.Env {
		env = append(env, e.Name+"="+e.Value)
	}
	if strings.Join(env, " ") != "CUDA_VISIBLE_DEVICES=0 NVIDIA_VISIBLE_DEVICES=2 Z=1" {
		t.Fatalf("env = %v", env)
	}
	if len(s.Volumes) != 2 || s.Volumes[0].HostPath.Path != "/opt/gpuworks/models/qwen" || !c.VolumeMounts[0].ReadOnly ||
		s.Volumes[1].EmptyDir == nil || s.Volumes[1].EmptyDir.Medium != "Memory" || s.Volumes[1].EmptyDir.SizeLimit != "1073741824" || c.VolumeMounts[1].MountPath != "/dev/shm" {
		t.Fatalf("volumes = %+v mounts = %+v", s.Volumes, c.VolumeMounts)
	}
	// No NVIDIA runtime class configured: the default runtime is NVIDIA's.
	pod, _ = instancePod(spec, podOptions{Namespace: "gw", NodeName: "n1"})
	if pod.Spec.RuntimeClassName != nil {
		t.Fatalf("runtime class without one configured = %v", *pod.Spec.RuntimeClassName)
	}
	spec.DockerArgs = []string{"--memory", "1g"}
	if _, err := instancePod(spec, podOptions{}); err == nil {
		t.Fatal("an untranslatable docker option was dropped")
	}
}

// TestInstancePodHygonShape: Hygon's injection (devices, a driver volume and
// the video group by name) becomes device plugin requests, a host path and a
// numeric supplementary group; devices the plugin does not serve refuse the
// pod rather than start it without its cards.
func TestInstancePodHygonShape(t *testing.T) {
	spec := runtime.WorkloadSpec{
		Name: "gpuworks-0123456789ab", Image: "img:1",
		DockerArgs: []string{"--device", "/dev/kfd:/dev/kfd:rwm", "--device", "/dev/dri/renderD128:/dev/dri/renderD128:rwm",
			"--volume", "/opt/hyhal:/opt/hyhal:ro", "--group-add", "video"},
	}
	groups := func(name string) (int64, error) {
		if name == "video" {
			return 39, nil
		}
		return 0, errors.New("no such group")
	}
	if _, err := instancePod(spec, podOptions{Namespace: "gw", NodeName: "n1", Group: groups}); err == nil || !strings.Contains(err.Error(), "device plugin") {
		t.Fatalf("devices the plugin does not serve = %v", err)
	}
	reqs := map[string]string{"gpuworks.ai/accelerator-0": "1", "gpuworks.ai/accelerator-shared": "1"}
	pod, err := instancePod(spec, podOptions{Namespace: "gw", NodeName: "n1", Group: groups, DeviceRequests: reqs})
	if err != nil {
		t.Fatal(err)
	}
	if r := pod.Spec.Containers[0].Resources; r.Limits["gpuworks.ai/accelerator-0"] != "1" || r.Requests["gpuworks.ai/accelerator-shared"] != "1" || r.Requests["cpu"] != "500m" {
		t.Fatalf("resources = %+v", r)
	}
	if pod.Spec.SecurityContext == nil || !slices.Equal(pod.Spec.SecurityContext.SupplementalGroups, []int64{39}) {
		t.Fatalf("pod security = %+v", pod.Spec.SecurityContext)
	}
	c := pod.Spec.Containers[0]
	if c.SecurityContext != nil {
		t.Fatalf("container security = %+v", c.SecurityContext)
	}
	if len(c.VolumeMounts) != 1 || c.VolumeMounts[0].MountPath != "/opt/hyhal" || !c.VolumeMounts[0].ReadOnly || pod.Spec.Volumes[0].HostPath.Path != "/opt/hyhal" {
		t.Fatalf("mounts = %+v, volumes %+v", c.VolumeMounts, pod.Spec.Volumes)
	}
	spec.DockerArgs = append(spec.DockerArgs, "--group-add", "render")
	if _, err := instancePod(spec, podOptions{Group: groups, DeviceRequests: reqs}); err == nil || !strings.Contains(err.Error(), "render") {
		t.Fatalf("unknown group = %v", err)
	}
	// A privileged workload sees every device already and asks for none.
	spec.DockerArgs = []string{"--privileged", "--device", "/dev/kfd"}
	if pod, err := instancePod(spec, podOptions{DeviceRequests: reqs}); err != nil || pod.Spec.Containers[0].Resources.Limits != nil {
		t.Fatalf("privileged = %+v, %v", pod.Spec.Containers[0].Resources, err)
	}
}

// The hold and instance pods take the PriorityClass the manifest renders for
// the cluster's namespace; a manifest that names none leaves the default.
func TestPodsTakeTheManifestsPriorityClass(t *testing.T) {
	for _, tc := range []struct{ given, want string }{
		{"gpuworks-inference-gw", "gpuworks-inference-gw"},
		{"", DefaultPriorityClass},
	} {
		d := New(Options{Namespace: "gw", NodeName: "n1", PodName: "agent-x", PriorityClass: tc.given})
		if got := d.cards.baseHoldPod("hold-0").Spec.PriorityClassName; got != tc.want {
			t.Errorf("hold pod with %q = %q, want %q", tc.given, got, tc.want)
		}
		if d.o.PriorityClass != tc.want {
			t.Errorf("instance pods with %q get %q, want %q", tc.given, d.o.PriorityClass, tc.want)
		}
	}
}
