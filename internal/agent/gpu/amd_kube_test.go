package gpu

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/accelerator"
)

// A pod's own /dev is a copy made at container start: on Kubernetes the AMD
// device nodes and the ROCm version are read through the host's root.
func TestAMDOnKubernetesReadsTheHost(t *testing.T) {
	root := withHostRoot(t)
	for path, content := range map[string]string{
		"dev/kfd":                          "",
		"dev/dri/by-path/pci-0000:43:00.0": "",
		"opt/rocm/.info/version":           "7.2.3-1\n",
	} {
		full := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	p := &AMDProber{}
	if _, err := p.inspect("/dev/kfd"); err != nil {
		t.Fatalf("the host's /dev/kfd was not found: %v", err)
	}
	got, err := p.matchDev("/dev/dri/by-path/*")
	if err != nil || len(got) != 1 || got[0] != "/dev/dri/by-path/pci-0000:43:00.0" {
		t.Fatalf("by-path aliases = %v, %v (paths must be the host's, without the root)", got, err)
	}
	if framework, version := p.Framework(context.Background()); framework != "rocm" || version != "7.2.3-1" {
		t.Fatalf("Framework = %q %q", framework, version)
	}

	cliOnHost.Store(false)
	if _, err := p.inspect("/dev/kfd"); err == nil {
		t.Fatal("a file under the host root was found in the agent's own /dev")
	}
	// (The build host's own /dev may have aliases; the fixture's must not be there.)
	for _, alias := range func() []string { got, _ := p.matchDev("/dev/dri/by-path/*"); return got }() {
		if strings.Contains(alias, "0000:43:00.0") {
			t.Fatalf("the host root's alias %s found off the host", alias)
		}
	}
	if _, version := p.Framework(context.Background()); version != "" {
		t.Fatalf("ROCm version %q read off the host", version)
	}
}

// The whole-card injection is the same under Kubernetes as under Docker.
func TestAMDRuntimeInjectionOnKubernetes(t *testing.T) {
	p := runtimeAMDFixture()
	if _, err := p.Probe(context.Background()); err != nil {
		t.Fatal(err)
	}
	inj := p.RuntimeInjectionForDriver([]int{7}, "kube")
	if inj.UnsupportedReason != "" {
		t.Fatal(inj.UnsupportedReason)
	}
	if got := strings.Join(inj.DockerArgs, " "); got != "--device /dev/kfd:/dev/kfd:rwm --device /dev/dri/renderD135:/dev/dri/renderD135:rwm" {
		t.Fatal(got)
	}
	if len(inj.Env) != 0 {
		t.Fatalf("a pod sees only its render node; no visible-device env is set: %v", inj.Env)
	}
	// Only whole cards: partitioning (the dGPU backend) stays off Kubernetes.
	if got := (accelerator.RuntimeMismatchParams(accelerator.VendorAMD, accelerator.HostRuntime{OS: "linux", Arch: "amd64", Driver: "kube"})); got != nil {
		t.Fatalf("kube refused for whole AMD cards: %v", got)
	}
}
