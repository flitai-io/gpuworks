package gpu

import (
	"os"
	"path/filepath"
	"testing"

	"gpuworks.ai/gpuworks/internal/accelerator"
)

// TestBusScanRootFollowsCLIsOnHost: New()'s PCI-bus fallback evidence must
// read the same root every other host-or-container lookup in this package
// does (hostexec.go), not always its own container's "/". Off the host
// (the default, and every non-Kubernetes caller) it stays "": pcicards'
// own container-safety check (hostOwnsThisBus) is what must decide whether
// a container's own bus is trustworthy, not this package overriding it
// with a path that is always "the host's" and so always trusted.
func TestBusScanRootFollowsCLIsOnHost(t *testing.T) {
	cliOnHost.Store(false)
	if got := busScanRoot(); got != "" {
		t.Fatalf("busScanRoot() off the host = %q, want \"\"", got)
	}
	oldRoot := hostRoot
	hostRoot = "/proc/1/root"
	cliOnHost.Store(true)
	t.Cleanup(func() { hostRoot = oldRoot; cliOnHost.Store(false) })
	if got := busScanRoot(); got != hostRoot {
		t.Fatalf("busScanRoot() on the host = %q, want %q", got, hostRoot)
	}
}

// writePCIFunction writes one sys/bus/pci/devices entry under root, in the
// shape pcicards.Scanner reads (see pcicards_test.go's hostTree).
func writePCIFunction(t *testing.T, root, bdf, vendor, device, class string) {
	t.Helper()
	dir := filepath.Join(root, "sys/bus/pci/devices", bdf)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{"vendor": vendor, "device": device, "class": class} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(value+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// TestNewSeesTheHostBusOnKubernetesBeforeTheDriverAnswers reproduces the
// GPU Operator race: the node has an NVIDIA card on the PCI bus, but
// neither its CLI nor its device files exist yet (the driver DaemonSet has
// not published them). On Kubernetes (RunCLIsOnHost), New() must still name
// the card's real vendor from the bus, not report NoAccelerator: without
// this, the agent's chosen prober never gets a chance to self-heal once the
// driver answers (nvidia_host_test.go covers that nvidia-smi is found as
// soon as it exists), because there is nothing NVIDIA-shaped to self-heal.
func TestNewSeesTheHostBusOnKubernetesBeforeTheDriverAnswers(t *testing.T) {
	root := withHostRoot(t) // sets hostRoot and cliOnHost(true); no CLI or device files exist
	writePCIFunction(t, root, "0000:1a:00.0", "0x10de", "0x2204", "0x030000")
	p := New()
	if _, ok := p.(NoAccelerator); ok {
		t.Fatal("New() reported NoAccelerator though the host's PCI bus shows an NVIDIA card")
	}
	if got := p.Vendor(); got != accelerator.VendorNVIDIA {
		t.Fatalf("New() = vendor %q, want %q", got, accelerator.VendorNVIDIA)
	}
}

// TestNewOffHostIgnoresItsOwnContainerBus: without RunCLIsOnHost, New() must
// keep reading pciBusVendors' default "" root (this process's own, possibly
// container-scoped, view), never the fixture passed only through hostRoot:
// an ordinary container must not have its lone rented card multiplied into
// the whole bus a fixture or another container happens to show.
func TestNewOffHostIgnoresItsOwnContainerBus(t *testing.T) {
	root := t.TempDir()
	writePCIFunction(t, root, "0000:1a:00.0", "0x10de", "0x2204", "0x030000")
	oldRoot := hostRoot
	hostRoot = root
	t.Cleanup(func() { hostRoot = oldRoot })
	cliOnHost.Store(false)
	if got := busScanRoot(); got != "" {
		t.Fatalf("busScanRoot() off the host = %q, want \"\"", got)
	}
}
