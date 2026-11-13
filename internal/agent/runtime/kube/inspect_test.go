package kube

import (
	"context"
	"errors"
	"regexp"
	"slices"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/agent/runtime/kube/kubeapi"
)

// inspectingImages is a node containerd that can also inspect containers, as
// nerdctl on the host does.
type inspectingImages struct {
	*fakeImages
	asked  []string
	result runtime.WorkloadInspection
	err    error
}

func (i *inspectingImages) InspectWorkload(_ context.Context, id string) (runtime.WorkloadInspection, error) {
	i.asked = append(i.asked, id)
	return i.result, i.err
}

// inspectSpec is an Ascend workload with everything the inspection reports.
func inspectSpec(name string) runtime.WorkloadSpec {
	s := ascendSpec(name, 6)
	s.Entrypoint = []string{"/bin/serve"}
	s.Cmd = []string{"--port", "8000"}
	s.Env["MODEL"] = "/models/qwen"
	s.Mounts = append(s.Mounts, runtime.Mount{Source: "/data/models", Target: "/models", ReadOnly: true})
	s.ShmBytes = 1 << 30
	return s
}

var hex64 = regexp.MustCompile(`^[0-9a-f]{64}$`)

// TestInspectWorkloadReportsThePodsFacts: with no containerd to ask, the
// inspection holds what the pod and its status say, and leaves the rest
// empty: the process the runtime started is not in the pod.
func TestInspectWorkloadReportsThePodsFacts(t *testing.T) {
	r := ascendRig(t)
	ctx := context.Background()
	id, err := r.d.Run(ctx, inspectSpec("gpuworks-aaaaaaaaaaaa"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := r.d.InspectWorkload(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if !hex64.MatchString(got.ContainerID) {
		t.Fatalf("ContainerID = %q, want containerd's id (not the pod name %s)", got.ContainerID, id)
	}
	pod := r.srv.Pod(testNS, id)
	if pod.Status.ContainerStatuses[0].ContainerID != "containerd://"+got.ContainerID {
		t.Fatalf("ContainerID %q is not the status's %q", got.ContainerID, pod.Status.ContainerStatuses[0].ContainerID)
	}
	// The kubelet reported the image by its manifest digest: it is a lookup
	// reference for the image's metadata, not the image id.
	if got.ImageID != "" || got.ImageLookupRef != "sha256:"+strings.Repeat("ab", 32) {
		t.Fatalf("image = %q lookup %q", got.ImageID, got.ImageLookupRef)
	}
	if !slices.Equal(got.Entrypoint, []string{"/bin/serve"}) || !slices.Equal(got.Cmd, []string{"--port", "8000"}) {
		t.Fatalf("entrypoint %v cmd %v", got.Entrypoint, got.Cmd)
	}
	if got.Path != "" || len(got.Args) != 0 {
		t.Fatalf("the started process is unknown without containerd: path %q args %v", got.Path, got.Args)
	}
	if got.Env["MODEL"] != "/models/qwen" || got.Env["ASCEND_RT_VISIBLE_DEVICES"] != "0" {
		t.Fatalf("env = %v", got.Env)
	}
	wantMounts := []runtime.InspectedMount{
		{Type: "bind", Source: "/usr/local/sbin/npu-smi", Destination: "/usr/local/sbin/npu-smi", ReadOnly: true},
		{Type: "bind", Source: "/data/models", Destination: "/models", ReadOnly: true},
		{Type: "tmpfs", Destination: "/dev/shm"},
	}
	if !slices.Equal(got.Mounts, wantMounts) {
		t.Fatalf("mounts = %+v", got.Mounts)
	}
	// The devices the agent's plugin handed over for the resources the pod
	// asks: card 6's own node and the shared ones, not card 5's.
	wantDevices := []string{
		"/dev/davinci6:/dev/davinci6:rwm",
		"/dev/davinci_manager:/dev/davinci_manager:rwm", "/dev/devmm_svm:/dev/devmm_svm:rwm", "/dev/hisi_hdc:/dev/hisi_hdc:rwm",
	}
	if !slices.Equal(got.Devices, wantDevices) {
		t.Fatalf("devices = %v, want %v", got.Devices, wantDevices)
	}
	if got.NetworkMode != "host" || got.RuntimeName != "" {
		t.Fatalf("network %q runtime %q", got.NetworkMode, got.RuntimeName)
	}
}

// An NVIDIA pod runs under the cluster's runtime class; the plugin hands it
// no devices, so none are listed.
func TestInspectWorkloadNamesTheRuntimeClassAndListsNoPluginDevices(t *testing.T) {
	r := newRig(t, 2)
	r.srv.RuntimeClasses["nvidia"] = true
	ctx := context.Background()
	s := spec("gpuworks-bbbbbbbbbbbb", 0)
	s.DockerArgs = []string{"--gpus", "device=GPU-a"}
	id, err := r.d.Run(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	got, err := r.d.InspectWorkload(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.RuntimeName != "nvidia" || len(got.Devices) != 0 || got.Env["NVIDIA_VISIBLE_DEVICES"] != "GPU-a" {
		t.Fatalf("inspection = %+v", got)
	}
}

// With the node's containerd to ask, its view of the container replaces what
// only it knows, and it is asked by containerd's id, not the pod name.
func TestInspectWorkloadTakesWhatContainerdKnows(t *testing.T) {
	r := ascendRig(t)
	ctx := context.Background()
	id, err := r.d.Run(ctx, inspectSpec("gpuworks-cccccccccccc"))
	if err != nil {
		t.Fatal(err)
	}
	pod := r.srv.Pod(testNS, id)
	cid := strings.TrimPrefix(pod.Status.ContainerStatuses[0].ContainerID, "containerd://")
	images := &inspectingImages{fakeImages: r.images, result: runtime.WorkloadInspection{
		ContainerID: cid, Path: "/bin/serve", Args: []string{"--port", "8000"},
		Env:     map[string]string{"PATH": "/usr/bin", "KUBERNETES_SERVICE_HOST": "10.0.0.1", "MODEL": "/models/qwen"},
		Mounts:  []runtime.InspectedMount{{Type: "bind", Source: "/etc/hosts", Destination: "/etc/hosts"}},
		Devices: []string{"/dev/davinci6:/dev/davinci6:rwm"},
	}}
	r.d.o.Images = images
	got, err := r.d.InspectWorkload(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(images.asked, []string{cid}) {
		t.Fatalf("containerd was asked for %v, want [%s]", images.asked, cid)
	}
	if got.Path != "/bin/serve" || !slices.Equal(got.Args, []string{"--port", "8000"}) {
		t.Fatalf("process = %q %v", got.Path, got.Args)
	}
	if got.Env["KUBERNETES_SERVICE_HOST"] != "10.0.0.1" || len(got.Mounts) != 1 || got.Mounts[0].Destination != "/etc/hosts" || len(got.Devices) != 1 {
		t.Fatalf("containerd's env/mounts/devices were not taken: %+v", got)
	}
	// What the pod alone knows is kept.
	if !slices.Equal(got.Entrypoint, []string{"/bin/serve"}) || got.NetworkMode != "host" || got.ContainerID != cid {
		t.Fatalf("pod facts lost: %+v", got)
	}
}

// containerd failing, or answering for another container, leaves the pod's
// facts and reports no process.
func TestInspectWorkloadKeepsThePodsFactsWhenContainerdCannotHelp(t *testing.T) {
	for name, images := range map[string]*inspectingImages{
		"error":           {err: errors.New("permission denied")},
		"other container": {result: runtime.WorkloadInspection{ContainerID: strings.Repeat("f", 64), Path: "/not/ours", Env: map[string]string{"X": "1"}}},
	} {
		t.Run(name, func(t *testing.T) {
			r := ascendRig(t)
			ctx := context.Background()
			id, err := r.d.Run(ctx, inspectSpec("gpuworks-dddddddddddd"))
			if err != nil {
				t.Fatal(err)
			}
			images.fakeImages = r.images
			r.d.o.Images = images
			got, err := r.d.InspectWorkload(ctx, id)
			if err != nil {
				t.Fatalf("InspectWorkload = %v", err)
			}
			if got.Path != "" || got.Env["MODEL"] != "/models/qwen" || got.Env["X"] != "" || len(got.Mounts) != 3 {
				t.Fatalf("inspection = %+v", got)
			}
		})
	}
}

// A pod that is gone, or whose container has not been created, is "no such
// container", which the engine reports as not running.
func TestInspectWorkloadWithoutAContainer(t *testing.T) {
	r := newRig(t, 0)
	ctx := context.Background()
	if _, err := r.d.InspectWorkload(ctx, "gpuworks-eeeeeeeeeeee"); err == nil || !strings.Contains(err.Error(), "no such container") {
		t.Fatalf("missing pod = %v", err)
	}
	r.srv.Kubelet = func(p *kubeapi.Pod) {} // stays Pending, no container status
	if err := r.srv.Client().Create(ctx, kubeapi.PodsPath(testNS), instancePodFor(t, "gpuworks-gggggggggggg"), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := r.d.InspectWorkload(ctx, "gpuworks-gggggggggggg"); err == nil || !strings.Contains(err.Error(), "no such container") {
		t.Fatalf("pending pod = %v", err)
	}
}

func instancePodFor(t *testing.T, name string) kubeapi.Pod {
	t.Helper()
	pod, err := instancePod(spec(name), podOptions{Namespace: testNS, NodeName: testNode})
	if err != nil {
		t.Fatal(err)
	}
	return pod
}

// CRI's image id (a bare digest) is the image's id; any other spelling of the
// kubelet's imageID is unknown.
func TestPodImageIdentity(t *testing.T) {
	digest := "sha256:" + strings.Repeat("0123456789abcdef", 4)
	for _, tc := range []struct{ in, id, lookup string }{
		{digest, digest, ""},
		{"docker.io/library/img@" + digest, "", digest},
		{"img:1", "", ""},
		{"", "", ""},
		{"docker.io/library/img@sha256:short", "", ""},
	} {
		if id, lookup := podImageIdentity(tc.in); id != tc.id || lookup != tc.lookup {
			t.Errorf("podImageIdentity(%q) = %q, %q; want %q, %q", tc.in, id, lookup, tc.id, tc.lookup)
		}
	}
}
