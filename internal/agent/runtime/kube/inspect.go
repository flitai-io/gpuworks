package kube

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/agent/runtime/kube/kubeapi"
)

// InspectWorkload implements runtime.WorkloadInspector. The facts come from
// the instance pod and its container's status, which the API server answers
// for any pod; when the node's containerd can also be asked (nerdctl on the
// host, in the k8s.io namespace) its view of the container replaces the
// parts only it knows: the process the runtime actually started, the
// environment with what the image and the kubelet added, and the mounts and
// devices the container really has. What neither knows stays empty rather
// than guessed: a Pod has no separate entrypoint and cmd once created, so
// those are only what the pod spec overrides.
func (d *Driver) InspectWorkload(ctx context.Context, id string) (runtime.WorkloadInspection, error) {
	var p kubeapi.Pod
	if err := d.o.API.Get(ctx, kubeapi.PodPath(d.o.Namespace, id), &p); err != nil {
		if kubeapi.IsNotFound(err) {
			return runtime.WorkloadInspection{}, fmt.Errorf("no such container: pod %s not found", id)
		}
		return runtime.WorkloadInspection{}, err
	}
	cs := engineStatus(p)
	containerID := containerdID(cs)
	if containerID == "" {
		return runtime.WorkloadInspection{}, fmt.Errorf("no such container: pod %s has no container yet", id)
	}
	spec := engineSpec(p)
	out := runtime.WorkloadInspection{
		ContainerID: containerID,
		Entrypoint:  append([]string{}, spec.Command...),
		Cmd:         append([]string{}, spec.Args...),
		Args:        []string{},
		Env:         specEnv(spec),
		Mounts:      specMounts(p, spec),
		Devices:     []string{},
	}
	out.ImageID, out.ImageLookupRef = podImageIdentity(cs.ImageID)
	if p.Spec.RuntimeClassName != nil {
		out.RuntimeName = *p.Spec.RuntimeClassName
	}
	if p.Spec.HostNetwork {
		out.NetworkMode = "host"
	}
	out.Devices = d.pluginDevices(ctx, spec)

	// The node's containerd knows what the pod spec cannot say. A failure
	// leaves the pod's facts as they are.
	if inspector, ok := d.o.Images.(runtime.WorkloadInspector); ok {
		if real, err := inspector.InspectWorkload(ctx, containerID); err != nil {
			d.o.Log.Debug("containerd inspect of an instance container failed", "pod", id, "err", err)
		} else if real.ContainerID == containerID {
			out.Path, out.Args = real.Path, real.Args
			if len(real.Env) > 0 {
				out.Env = real.Env
			}
			if len(real.Mounts) > 0 {
				out.Mounts = real.Mounts
			}
			if len(real.Devices) > 0 {
				out.Devices = real.Devices
			}
		}
	}
	return out, nil
}

// WorkloadHandleIsName implements runtime.NamedWorkloads: Run returns the
// pod's name, which is how every later call addresses it.
func (d *Driver) WorkloadHandleIsName() bool { return true }

var (
	_ runtime.WorkloadInspector = (*Driver)(nil)
	_ runtime.NamedWorkloads    = (*Driver)(nil)
)

// containerdID is the container's id in containerd ("containerd://<id>").
func containerdID(cs *kubeapi.ContainerStatus) string {
	if cs == nil {
		return ""
	}
	_, id, ok := strings.Cut(cs.ContainerID, "://")
	if !ok {
		return ""
	}
	return id
}

// engineSpec is the engine container of the pod.
func engineSpec(p kubeapi.Pod) kubeapi.Container {
	for _, c := range p.Spec.Containers {
		if c.Name == ContainerName {
			return c
		}
	}
	return kubeapi.Container{}
}

var (
	bareDigest       = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	repoDigestSuffix = regexp.MustCompile(`@(sha256:[0-9a-f]{64})$`)
)

// podImageIdentity reads the image the kubelet reports a container to run.
// A bare digest is CRI's image id, the digest of the image's config, which
// is what an image inspect reports as the id. "repo@sha256:…" names the image
// by the digest of its manifest, which an image inspect can look up but is
// not the id. Anything else is unknown.
func podImageIdentity(imageID string) (id, lookup string) {
	switch {
	case bareDigest.MatchString(imageID):
		return imageID, ""
	case repoDigestSuffix.MatchString(imageID):
		return "", repoDigestSuffix.FindStringSubmatch(imageID)[1]
	}
	return "", ""
}

func specEnv(c kubeapi.Container) map[string]string {
	env := make(map[string]string, len(c.Env))
	for _, e := range c.Env {
		env[e.Name] = e.Value
	}
	return env
}

// specMounts are the volume mounts the pod spec asks for; the kubelet adds
// more (hosts file, service account), which only containerd shows.
func specMounts(p kubeapi.Pod, c kubeapi.Container) []runtime.InspectedMount {
	vols := map[string]kubeapi.Volume{}
	for _, v := range p.Spec.Volumes {
		vols[v.Name] = v
	}
	out := make([]runtime.InspectedMount, 0, len(c.VolumeMounts))
	for _, m := range c.VolumeMounts {
		v := vols[m.Name]
		mount := runtime.InspectedMount{Destination: m.MountPath, ReadOnly: m.ReadOnly}
		switch {
		case v.HostPath != nil:
			mount.Type, mount.Source = "bind", v.HostPath.Path
		case v.EmptyDir != nil && v.EmptyDir.Medium == "Memory":
			mount.Type = "tmpfs"
		case v.EmptyDir != nil:
			mount.Type = "volume"
		default:
			continue // a volume kind this runtime never creates
		}
		out = append(out, mount)
	}
	return out
}

// pluginDevices lists the device nodes the agent's device plugin hands to the
// container for the resources it requests, in docker's HOST:CONTAINER:PERMS
// form. Devices an NVIDIA runtime class injects are not listed: the plugin
// does not know them (NVIDIA_VISIBLE_DEVICES in the environment says which).
func (d *Driver) pluginDevices(ctx context.Context, c kubeapi.Container) []string {
	out := []string{}
	if c.Resources == nil {
		return out
	}
	plan, err := d.devicePlan(ctx)
	if err != nil {
		return out
	}
	names := make([]string, 0, len(c.Resources.Limits))
	for name := range c.Resources.Limits {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		for _, r := range plan.resources {
			if r.Name != name {
				continue
			}
			for _, dev := range r.Devices {
				out = append(out, dev.HostPath+":"+dev.ContainerPath+":"+dev.Permissions)
			}
		}
	}
	return out
}
