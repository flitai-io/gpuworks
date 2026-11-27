package gpu

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestMetaXMissingCLIReportsPCIRoster(t *testing.T) {
	missing := errors.New("mx-smi unavailable")
	p := &MetaXProber{hostRoot: t.TempDir(), lookPath: func(string) (string, error) { return "", missing }}
	cards, err := p.Probe(t.Context())
	if err != nil || len(cards) != 0 {
		t.Fatalf("Probe with unavailable CLI and PCI roster: cards=%+v err=%v", cards, err)
	}
	metrics, err := p.Metrics(t.Context())
	if !errors.Is(err, missing) || len(metrics) != 0 {
		t.Fatalf("Metrics with unavailable CLI: metrics=%+v err=%v", metrics, err)
	}
	p.hostRoot = pciHost(t, pciFunction{bdf: "0000:0f:00.0", vendor: "0x9999", device: "0x4000", class: "0x038000", driver: "metax"})
	cards, err = p.Probe(t.Context())
	if err != nil || len(cards) != 1 || cards[0].PCIeBDF != "0000:0f:00.0" || cards[0].Healthy {
		t.Fatalf("Probe should retain unhealthy PCI fallback: cards=%+v err=%v", cards, err)
	}
}

func TestMetaXReadsHostNamespace(t *testing.T) {
	root := withHostRoot(t, "/usr/bin/mx-smi")
	t.Setenv("PATH", t.TempDir())
	file := filepath.Join(root, "var/tmp/metax-host-test")
	if err := os.MkdirAll(filepath.Dir(file), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("host"), 0644); err != nil {
		t.Fatal(err)
	}
	p := &MetaXProber{}
	if got, err := p.lookup("mx-smi"); err != nil || got != "/usr/bin/mx-smi" {
		t.Fatalf("lookup: %q %v", got, err)
	}
	if got, err := p.read("/var/tmp/metax-host-test"); err != nil || string(got) != "host" {
		t.Fatalf("read: %q %v", got, err)
	}
}

func TestMetaXMetricsWritesOutputInHostNamespace(t *testing.T) {
	root := withHostRoot(t, "/usr/bin/mx-smi")
	if err := os.MkdirAll(filepath.Join(root, "tmp"), 0755); err != nil {
		t.Fatal(err)
	}
	p := &MetaXProber{run: func(_ context.Context, args ...string) (string, error) {
		if args[0] == "-L" {
			return "GPU#0 MXC550 0000:0f:00.0 Available (UUID: GPU-a)\n", nil
		}
		if args[0] != "-o" {
			return "", fmt.Errorf("unexpected command")
		}
		path := filepath.Join(root, args[1])
		if _, err := os.Stat(path); err != nil {
			return "", fmt.Errorf("host temporary file not created: %w", err)
		}
		return "", os.WriteFile(path, []byte("deviceId,bdfId,utilization.GPU [%]\nGPU#0,0000:0f:00.0,12\n"), 0600)
	}}
	metrics, err := p.Metrics(t.Context())
	if err != nil || len(metrics) != 1 || metrics[0].UUID != "GPU-a" {
		t.Fatalf("metrics=%+v err=%v", metrics, err)
	}
}

func TestMetaXSmokePrerequisiteFindsHostRuntime(t *testing.T) {
	withHostRoot(t, "/usr/bin/python3", "/opt/maca/lib/libmcruntime.so")
	t.Setenv("PATH", t.TempDir())
	t.Setenv("MACA_PATH", "")
	if reason := MetaXSmokePrerequisite(); reason != "" {
		t.Fatalf("host runtime not found: %s", reason)
	}
}
