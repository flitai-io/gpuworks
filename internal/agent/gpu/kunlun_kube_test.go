package gpu

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
)

// The grant names the plain runtime and the selected cards' nodes whichever
// runtime it is for. What a runtime makes of the plain runtime is its own:
// docker and nerdctl name runc, and a pod has no class for it.
func TestKunlunGrantsThePlainRuntimeAndTheSelectedNodes(t *testing.T) {
	for _, tc := range []struct {
		driver  string
		indexes []int
		want    []string
	}{
		{"docker", []int{1}, []string{"/dev/xpu1", "/dev/xpuctrl"}},
		{"nerdctl", []int{1}, []string{"/dev/xpu1", "/dev/xpuctrl"}},
		{"kube", []int{1}, []string{"/dev/xpu1", "/dev/xpuctrl"}},
		{"kube", []int{1, 0}, []string{"/dev/xpu1", "/dev/xpu0", "/dev/xpuctrl"}},
	} {
		p, _ := kunlunFixture(t)
		if _, err := p.Probe(t.Context()); err != nil {
			t.Fatal(err)
		}
		got := p.RuntimeInjectionForDriver(tc.indexes, runtime.TargetNamed(tc.driver))
		if got.UnsupportedReason != "" {
			t.Fatalf("%s %v: %s", tc.driver, tc.indexes, got.UnsupportedReason)
		}
		if got.Grant.RuntimeClass != runtime.PlainRuntimeClass {
			t.Fatalf("%s %v: runtime class = %q, want the plain runtime", tc.driver, tc.indexes, got.Grant.RuntimeClass)
		}
		var paths []string
		for _, device := range got.Grant.Devices {
			if device.Access != runtime.DeviceAccessAll {
				t.Fatalf("%s %v: %s is granted with access %q", tc.driver, tc.indexes, device.Path, device.Access)
			}
			paths = append(paths, device.Path)
		}
		if !reflect.DeepEqual(paths, tc.want) {
			t.Fatalf("%s %v: nodes = %v, want %v", tc.driver, tc.indexes, paths, tc.want)
		}
	}
}

// A Kunlun node's driver may finish initializing after the agent pod
// started, so the device nodes are checked on the host's live /dev, and
// xpu-smi is found where the XRE SDK installs it on the host.
func TestKunlunOnKubernetesReadsTheHost(t *testing.T) {
	root := withHostRoot(t, "/usr/local/xpu/bin/xpu-smi")
	p := &KunlunProber{}
	if exe, err := p.executable(); err != nil || exe != "/usr/local/xpu/bin/xpu-smi" {
		t.Fatalf("xpu-smi = %q, %v", exe, err)
	}

	// Only the host has /dev/xpu0; the agent's own copy does not.
	if err := os.MkdirAll(filepath.Join(root, "dev"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "dev/xpu0"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := p.inspect("/dev/xpu0"); err != nil {
		t.Fatalf("the host's /dev/xpu0 was not found: %v", err)
	}
	cliOnHost.Store(false)
	if _, err := p.inspect("/dev/xpu0"); err == nil {
		t.Fatal("a file under the host root was found in the agent's own /dev")
	}
}

// /dev/xdrdrv must be a real character device: a link is refused, not
// followed, on the host as well.
func TestKunlunXDRRejectsALinkOnTheHost(t *testing.T) {
	root := withHostRoot(t)
	if err := os.MkdirAll(filepath.Join(root, "dev"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "dev/real"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real", filepath.Join(root, "dev/xdrdrv")); err != nil {
		t.Fatal(err)
	}
	err := (&KunlunProber{}).verifyKunlunXDR()
	if err == nil || !strings.Contains(err.Error(), "not a real character device") {
		t.Fatalf("verifyKunlunXDR = %v, want the link refused rather than missing", err)
	}
}
