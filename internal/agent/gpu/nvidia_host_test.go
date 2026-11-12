package gpu

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestLocateNVIDIASMIPrefersTheHostsOwnPATH: a node whose kubelet's default
// runtime is not NVIDIA's (k3s with runc) still has the driver package's own
// nvidia-smi on the host, which is tried before the GPU Operator's driver
// container and before the agent's own PATH.
func TestLocateNVIDIASMIPrefersTheHostsOwnPATH(t *testing.T) {
	root := withHostRoot(t, "/usr/bin/nvidia-smi")
	// A decoy under the driver container root must not be preferred.
	driver := filepath.Join(root, nvidiaDriverContainerRoot, "usr/bin/nvidia-smi")
	if err := os.MkdirAll(filepath.Dir(driver), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(driver, nil, 0o755); err != nil {
		t.Fatal(err)
	}
	loc, err := locateNVIDIASMI()
	if err != nil || loc.path != "/usr/bin/nvidia-smi" || loc.root != "" || loc.inContainer {
		t.Fatalf("locateNVIDIASMI = %+v, %v", loc, err)
	}
	cmd := nvidiaSMICommand(context.Background(), "-L")
	if got := strings.Join(cmd.Args, " "); got != "nsenter -t 1 -m -- /usr/bin/nvidia-smi -L" {
		t.Fatalf("command = %s", got)
	}
}

// TestLocateNVIDIASMIFallsBackToTheDriverContainerRoot: a managed cluster's
// GPU Operator mounts no nvidia-smi onto the host's own root at all, only
// into the driver DaemonSet's published tree at
// nvidiaDriverContainerRoot — found by nsenter-ing into the host's mount
// namespace and chrooting there.
func TestLocateNVIDIASMIFallsBackToTheDriverContainerRoot(t *testing.T) {
	root := withHostRoot(t)
	driver := filepath.Join(root, nvidiaDriverContainerRoot, "usr/bin/nvidia-smi")
	if err := os.MkdirAll(filepath.Dir(driver), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(driver, nil, 0o755); err != nil {
		t.Fatal(err)
	}
	loc, err := locateNVIDIASMI()
	if err != nil || loc.path != "/usr/bin/nvidia-smi" || loc.root != nvidiaDriverContainerRoot || loc.inContainer {
		t.Fatalf("locateNVIDIASMI = %+v, %v", loc, err)
	}
	cmd := nvidiaSMICommand(context.Background(), "-L")
	if got := strings.Join(cmd.Args, " "); got != "nsenter -t 1 -m -- chroot /run/nvidia/driver /usr/bin/nvidia-smi -L" {
		t.Fatalf("command = %s", got)
	}
}

// TestLocateNVIDIASMIFallsBackToTheAgentsOwnPATH: an agent run under the
// cluster's nvidia RuntimeClass (or a node whose default runtime is
// NVIDIA's) has nvidia-smi injected into its own container by the NVIDIA
// Container Runtime, nowhere on the host's root at all.
func TestLocateNVIDIASMIFallsBackToTheAgentsOwnPATH(t *testing.T) {
	withHostRoot(t)
	dir := fakeCLI(t, "nvidia-smi", "echo injected\n")
	loc, err := locateNVIDIASMI()
	want := filepath.Join(dir, "nvidia-smi")
	if err != nil || loc.path != want || loc.root != "" || !loc.inContainer {
		t.Fatalf("locateNVIDIASMI = %+v, %v, want path=%s inContainer", loc, err, want)
	}
	cmd := nvidiaSMICommand(context.Background(), "-L")
	if got := strings.Join(cmd.Args, " "); got != want+" -L" {
		t.Fatalf("command = %s, want a plain exec of the agent's own copy", got)
	}
}

// TestLocateNVIDIASMINotFoundAnywhere: no mount, no driver container, no
// injection — a node this build genuinely cannot query.
func TestLocateNVIDIASMINotFoundAnywhere(t *testing.T) {
	withHostRoot(t)
	t.Setenv("PATH", t.TempDir())
	if _, err := locateNVIDIASMI(); !errors.Is(err, exec.ErrNotFound) {
		t.Fatalf("locateNVIDIASMI = %v, want exec.ErrNotFound", err)
	}
	// cliCommand still returns a runnable (failing) command rather than a
	// nil one or one that panics.
	cmd := cliCommand(context.Background(), "nvidia-smi", "-L")
	if cmd == nil || cmd.Path == "" {
		t.Fatalf("cliCommand(nvidia-smi) with nothing found = %+v", cmd)
	}
}

// TestLookCLINVIDIASMIUsesTheSameThreeTierSearch confirms lookCLI, which
// Available()/Probe() use only to check whether nvidia-smi exists at all,
// agrees with locateNVIDIASMI.
func TestLookCLINVIDIASMIUsesTheSameThreeTierSearch(t *testing.T) {
	withHostRoot(t, "/usr/bin/nvidia-smi")
	path, err := lookCLI("nvidia-smi")
	if err != nil || path != "/usr/bin/nvidia-smi" {
		t.Fatalf("lookCLI(nvidia-smi) = %q, %v", path, err)
	}
}
