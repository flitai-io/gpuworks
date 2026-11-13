package kube

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/agent/runtime/kube/kubeapi"
)

// Labels and annotations the runtime puts on its pods.
const (
	LabelManagedBy = "app.kubernetes.io/managed-by"
	LabelRole      = "gpuworks.ai/role"
	LabelNode      = "gpuworks.ai/node"
	RoleInstance   = "instance"
	RoleHold       = "hold"
	// AnnotationCards lists the card UUIDs an instance pod runs on, so a
	// restarted agent knows which borrowed cards are in use.
	AnnotationCards = "gpuworks.ai/cards"
	// AnnotationCard is the card a hold pod was given.
	AnnotationCard = "gpuworks.ai/card"
	// ContainerName is an instance pod's only container.
	ContainerName = "engine"
)

// podOptions are the parts of an instance pod that come from the node, not
// the workload.
type podOptions struct {
	Namespace    string
	NodeName     string
	RuntimeClass string // the RuntimeClass that injects NVIDIA devices; "" uses the default runtime
	// PriorityClass is the instance pod's PriorityClass (Options.PriorityClass).
	PriorityClass string
	Owner         *kubeapi.OwnerReference
	Cards         []string // card UUIDs, recorded on the pod
	// DeviceRequests are the agent's device plugin resources holding the
	// workload's --device nodes (devices.go); UncoveredDevices: some node
	// is not the plugin's, so the pod runs privileged.
	DeviceRequests   map[string]string
	UncoveredDevices bool
	// Group resolves a --group-add name on the node.
	Group func(name string) (int64, error)
	// NVIDIAVisible rewrites the cards --gpus selects (NVIDIA_VISIBLE_DEVICES)
	// into the names the node's NVIDIA runtime resolves (nvidia_cdi.go);
	// nil keeps them.
	NVIDIAVisible func(string) (string, error)
}

// dockerTranslation is what a workload's docker-style arguments amount to
// in a pod.
type dockerTranslation struct {
	Env          map[string]string
	RuntimeClass string // from --runtime; "nvidia" means the node's NVIDIA class
	Privileged   bool
	Devices      []string // --device values, handed over by the device plugin
	Volumes      []runtime.Mount
	GroupNames   []string // --group-add names, resolved on the node
	HostIPC      bool
	HostPID      bool
	CapAdd       []string
	Groups       []int64
	ShmBytes     int64
}

// dockerOptionsWithValues are the translated options that take a value.
var dockerOptionsWithValues = map[string]bool{"--gpus": true, "--device": true, "--runtime": true, "--ipc": true, "--pid": true, "--ulimit": true,
	"--cap-add": true, "--shm-size": true, "--security-opt": true, "--network": true, "--net": true, "--group-add": true, "-e": true, "--env": true,
	"-v": true, "--volume": true}

// translateDockerArgs turns the docker run options a workload carries into
// pod settings. Every option the platform or a template is known to pass is
// covered; anything else is refused rather than dropped, because a pod
// silently missing an option would run differently from its docker twin.
func translateDockerArgs(args []string) (dockerTranslation, error) {
	t := dockerTranslation{Env: map[string]string{}}
	for i := 0; i < len(args); i++ {
		opt, val, hasVal := strings.Cut(args[i], "=")
		if !strings.HasPrefix(opt, "-") {
			return t, fmt.Errorf("docker argument %q is not an option", args[i])
		}
		if dockerOptionsWithValues[opt] && !hasVal {
			if i+1 >= len(args) {
				return t, fmt.Errorf("docker option %s has no value", opt)
			}
			i++
			val = args[i]
		}
		val = strings.Trim(val, `"'`)
		switch opt {
		case "--gpus":
			// The NVIDIA runtime class does what --gpus does in docker: the
			// selected host devices, renumbered from zero in the container.
			if devices, ok := strings.CutPrefix(val, "device="); ok {
				t.Env["NVIDIA_VISIBLE_DEVICES"] = devices
			} else {
				t.Env["NVIDIA_VISIBLE_DEVICES"] = val
			}
			if t.RuntimeClass == "" {
				t.RuntimeClass = "nvidia"
			}
		case "--runtime":
			t.RuntimeClass = val
		case "--device":
			t.Devices = append(t.Devices, val)
		case "--privileged":
			t.Privileged = true
		case "--ipc":
			t.HostIPC = val == "host"
		case "--pid":
			t.HostPID = val == "host"
		case "--ulimit":
			// Kubernetes has no per-pod ulimit; a privileged container runs
			// with the node's limits, which is what memlock=-1 is after.
			t.Privileged = true
		case "--cap-add":
			t.CapAdd = append(t.CapAdd, strings.ToUpper(strings.TrimPrefix(val, "CAP_")))
		case "--shm-size":
			n, err := parseDockerBytes(val)
			if err != nil {
				return t, err
			}
			t.ShmBytes = n
		case "--security-opt":
			t.Privileged = true
		case "--network", "--net":
			if val != "host" {
				return t, fmt.Errorf("docker option --network=%s cannot be expressed in a host-network pod", val)
			}
		case "--group-add":
			if g, err := strconv.ParseInt(val, 10, 64); err == nil {
				t.Groups = append(t.Groups, g)
			} else {
				t.GroupNames = append(t.GroupNames, val)
			}
		case "-v", "--volume":
			parts := strings.Split(val, ":")
			if len(parts) < 2 || len(parts) > 3 || !strings.HasPrefix(parts[0], "/") || !strings.HasPrefix(parts[1], "/") || (len(parts) == 3 && parts[2] != "ro" && parts[2] != "rw") {
				return t, fmt.Errorf("docker option --volume=%s: only HOST:CONTAINER[:ro|rw] bind mounts can be given to a pod", val)
			}
			t.Volumes = append(t.Volumes, runtime.Mount{Source: parts[0], Target: parts[1], ReadOnly: len(parts) == 3 && parts[2] == "ro"})
		case "-e", "--env":
			k, v, _ := strings.Cut(val, "=")
			t.Env[k] = v
		default:
			return t, fmt.Errorf("docker option %s has no Kubernetes equivalent", opt)
		}
	}
	return t, nil
}

func parseDockerBytes(s string) (int64, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	mult := int64(1)
	for suffix, m := range map[string]int64{"k": 1 << 10, "m": 1 << 20, "g": 1 << 30, "t": 1 << 40} {
		if strings.HasSuffix(s, suffix) || strings.HasSuffix(s, suffix+"b") {
			s = strings.TrimSuffix(strings.TrimSuffix(s, "b"), suffix)
			mult = m
			break
		}
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("docker size %q is not a size", s)
	}
	return n * mult, nil
}

// instancePod builds the pod a workload runs as.
func instancePod(spec runtime.WorkloadSpec, o podOptions) (kubeapi.Pod, error) {
	tr, err := translateDockerArgs(spec.DockerArgs)
	if err != nil {
		return kubeapi.Pod{}, err
	}
	for _, name := range tr.GroupNames {
		if o.Group == nil {
			return kubeapi.Pod{}, fmt.Errorf("docker option --group-add=%s: the node's groups cannot be read", name)
		}
		g, err := o.Group(name)
		if err != nil {
			return kubeapi.Pod{}, fmt.Errorf("docker option --group-add=%s: %w", name, err)
		}
		tr.Groups = append(tr.Groups, g)
	}
	if len(tr.Devices) > 0 && !tr.Privileged {
		if o.UncoveredDevices {
			tr.Privileged = true
		} else if len(o.DeviceRequests) == 0 {
			return kubeapi.Pod{}, errors.New("the workload needs device nodes and the agent's device plugin hands out none of them")
		}
	}
	env := map[string]string{}
	for k, v := range tr.Env {
		env[k] = v
	}
	for k, v := range spec.Env {
		env[k] = v
	}
	if v, ok := tr.Env["NVIDIA_VISIBLE_DEVICES"]; ok && o.NVIDIAVisible != nil && env["NVIDIA_VISIBLE_DEVICES"] == v {
		q, err := o.NVIDIAVisible(v)
		if err != nil {
			return kubeapi.Pod{}, err
		}
		env["NVIDIA_VISIBLE_DEVICES"] = q
	}
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	c := kubeapi.Container{
		Name: ContainerName, Image: spec.Image, ImagePullPolicy: "IfNotPresent",
		Command: spec.Entrypoint, Args: spec.Cmd,
		Resources: &kubeapi.Resources{Requests: map[string]string{"cpu": "500m", "memory": "1Gi"}},
	}
	if len(tr.Devices) > 0 && !tr.Privileged {
		c.Resources.Limits = map[string]string{}
		for r, n := range o.DeviceRequests {
			c.Resources.Requests[r], c.Resources.Limits[r] = n, n
		}
	}
	for _, k := range keys {
		c.Env = append(c.Env, kubeapi.EnvVar{Name: k, Value: env[k]})
	}
	var volumes []kubeapi.Volume
	for i, m := range append(append([]runtime.Mount(nil), spec.Mounts...), tr.Volumes...) {
		name := fmt.Sprintf("m%d", i)
		volumes = append(volumes, kubeapi.Volume{Name: name, HostPath: &kubeapi.HostPath{Path: m.Source}})
		c.VolumeMounts = append(c.VolumeMounts, kubeapi.VolumeMount{Name: name, MountPath: m.Target, ReadOnly: m.ReadOnly})
	}
	shm := spec.ShmBytes
	if tr.ShmBytes > 0 {
		shm = tr.ShmBytes
	}
	if shm > 0 && !tr.HostIPC {
		volumes = append(volumes, kubeapi.Volume{Name: "dshm", EmptyDir: &kubeapi.EmptyDir{Medium: "Memory", SizeLimit: strconv.FormatInt(shm, 10)}})
		c.VolumeMounts = append(c.VolumeMounts, kubeapi.VolumeMount{Name: "dshm", MountPath: "/dev/shm"})
	}
	if tr.Privileged || len(tr.CapAdd) > 0 {
		sc := &kubeapi.SecurityContext{}
		if tr.Privileged {
			yes := true
			sc.Privileged = &yes
		}
		if len(tr.CapAdd) > 0 {
			sc.Capabilities = &kubeapi.Capabilities{Add: tr.CapAdd}
		}
		c.SecurityContext = sc
	}
	grace := int64(30)
	pod := kubeapi.Pod{
		APIVersion: "v1", Kind: "Pod",
		Metadata: kubeapi.ObjectMeta{
			Name: spec.Name, Namespace: o.Namespace,
			Labels: map[string]string{LabelManagedBy: "gpuworks", LabelRole: RoleInstance, LabelNode: o.NodeName},
		},
		Spec: kubeapi.PodSpec{
			NodeName: o.NodeName, RestartPolicy: "Never", HostIPC: tr.HostIPC, HostPID: tr.HostPID,
			PriorityClassName: o.PriorityClass, TerminationGracePeriodSeconds: &grace,
			Tolerations: []kubeapi.Toleration{{Operator: "Exists"}},
			Containers:  []kubeapi.Container{c}, Volumes: volumes,
		},
	}
	if spec.HostNetwork {
		pod.Spec.HostNetwork, pod.Spec.DNSPolicy = true, "ClusterFirstWithHostNet"
	}
	if rc := tr.RuntimeClass; rc != "" {
		if rc == "nvidia" {
			rc = o.RuntimeClass // the node's NVIDIA class; "" when its default runtime is NVIDIA's
		}
		if rc != "" {
			pod.Spec.RuntimeClassName = &rc
		}
	}
	if len(tr.Groups) > 0 {
		pod.Spec.SecurityContext = &kubeapi.PodSecurity{SupplementalGroups: tr.Groups}
	}
	if o.Owner != nil {
		pod.Metadata.OwnerReferences = []kubeapi.OwnerReference{*o.Owner}
	}
	if len(o.Cards) > 0 {
		pod.Metadata.Annotations = map[string]string{AnnotationCards: strings.Join(o.Cards, ",")}
	}
	return pod, nil
}

// workloadDevices are the device nodes a workload's pod must be given.
func workloadDevices(spec runtime.WorkloadSpec) ([]string, error) {
	tr, err := translateDockerArgs(spec.DockerArgs)
	if err != nil || tr.Privileged {
		return nil, err
	}
	return tr.Devices, nil
}
