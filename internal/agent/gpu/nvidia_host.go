package gpu

import (
	"context"
	"os/exec"
	"path/filepath"
)

// nvidiaDriverContainerRoot is where the NVIDIA GPU Operator's driver
// DaemonSet publishes its userspace driver and nvidia-smi for the rest of
// the node, bind-mounted onto the host's own /run (not a separate mount
// namespace: no chroot is needed to read a file under it, but nvidia-smi
// itself needs to run as if rooted there — its own ld.so.cache and
// configuration live inside that tree). cuda_smoke.go's cudaLibraryPaths
// already looks for libcuda.so.1 there.
const nvidiaDriverContainerRoot = "/run/nvidia/driver"

// nvidiaSMILocation is where lookCLI found nvidia-smi and how to run it.
type nvidiaSMILocation struct {
	// path is nvidia-smi's location: a host path (when root is "") or a
	// path inside root (when root is nvidiaDriverContainerRoot), or the
	// agent's own PATH (when inContainer is set).
	path        string
	root        string
	inContainer bool
}

// locateNVIDIASMI finds nvidia-smi in the order a Kubernetes node without
// vendor mounts needs: the host's own PATH (an agent sharing the host's PID
// and mount namespaces finds the host's driver package there directly),
// the GPU Operator's driver container root (a managed cluster's default
// runtime puts no nvidia-smi on the host's own root at all, only inside the
// driver DaemonSet's published tree), and finally the agent's own container
// PATH (the NVIDIA Container Runtime's injection, when the agent runs under
// no RuntimeClass and the node's default runtime is NVIDIA's).
func locateNVIDIASMI() (nvidiaSMILocation, error) {
	for _, dir := range hostCLIDirs {
		path := filepath.Join(dir, "nvidia-smi")
		if fi, err := statOnHost(path); err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0 {
			return nvidiaSMILocation{path: path}, nil
		}
	}
	for _, dir := range hostCLIDirs {
		path := filepath.Join(dir, "nvidia-smi")
		if _, fi, err := resolveUnderRoot(nvidiaDriverContainerRoot, path); err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0 {
			return nvidiaSMILocation{path: path, root: nvidiaDriverContainerRoot}, nil
		}
	}
	if path, err := exec.LookPath("nvidia-smi"); err == nil {
		return nvidiaSMILocation{path: path, inContainer: true}, nil
	}
	return nvidiaSMILocation{}, &exec.Error{Name: "nvidia-smi", Err: exec.ErrNotFound}
}

// nvidiaSMICommand builds nvidia-smi's command once CLIs run on the host:
// plain, in the agent's own container, when the NVIDIA Container Runtime
// injected a copy there; nsentered into the host's mount namespace and
// chrooted to the driver container's published root, when that is the only
// place it was found; nsentered alone otherwise. A location that cannot be
// found at all still runs the bare name, so the caller gets the ordinary
// "not found" error from the process instead of a silently empty command.
func nvidiaSMICommand(ctx context.Context, args ...string) *exec.Cmd {
	loc, err := locateNVIDIASMI()
	if err != nil {
		return exec.CommandContext(ctx, "nvidia-smi", args...)
	}
	if loc.inContainer {
		return exec.CommandContext(ctx, loc.path, args...)
	}
	full := []string{"-t", "1", "-m", "--"}
	if loc.root != "" {
		full = append(full, "chroot", loc.root)
	}
	full = append(full, loc.path)
	full = append(full, args...)
	return exec.CommandContext(ctx, "nsenter", full...)
}
