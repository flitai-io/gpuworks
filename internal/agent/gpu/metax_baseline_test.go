package gpu

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestMetaXProbeBaselineRejectsChangedProcessInventory(t *testing.T) {
	listing := "GPU#0 MXC550 0000:0f:00.0 Available (UUID: GPU-a)\n"
	p := &MetaXProber{hostRoot: t.TempDir(), lookPath: func(string) (string, error) { return "mx-smi", nil }}
	p.run = func(_ context.Context, args ...string) (string, error) {
		switch args[0] {
		case "-L":
			return listing, nil
		case "-s":
			return "GPU#0 MXC550 0000:0f:00.0\nvram total : 1024 KB\n", nil
		default:
			return "", nil
		}
	}
	cards, err := p.Probe(t.Context())
	if err != nil || len(cards) != 1 {
		t.Fatalf("probe = %+v, %v", cards, err)
	}
	listing = "GPU#0 MXC550 0000:34:00.0 Available (UUID: GPU-b)\n"
	if _, err := p.ComputeProcesses(t.Context()); err == nil || !strings.Contains(err.Error(), "changed since probe") {
		t.Fatalf("changed inventory accepted: %v", err)
	}
	if inj := p.RuntimeInjection([]int{0}); inj.UnsupportedReason == "" {
		t.Fatal("unverified grant accepted")
	}
}

func TestMetaXFailedListDoesNotPublishPartialCards(t *testing.T) {
	p := &MetaXProber{hostRoot: t.TempDir(), lookPath: func(string) (string, error) { return "mx-smi", nil }, run: func(context.Context, ...string) (string, error) {
		return "GPU#0 MXC550 0000:0f:00.0 Available (UUID: GPU-a)\n", errors.New("truncated")
	}}
	cards, err := p.Probe(t.Context())
	if err == nil || len(cards) != 0 {
		t.Fatalf("partial listing accepted: %+v, %v", cards, err)
	}
}
