//go:build l4

package l4

import (
	"context"
	"os"
	"testing"

	"gpuworks.ai/gpuworks/internal/agent/gpu"
)

// GPUWORKS_REAL_METAX=1 go test -tags=l4 -run '^TestL4MetaXPoolInventoryProcesses$' -v ./test/l4
// This only queries inventory, process and metrics information; it does not
// create a runtime context or allocate memory on a card.
func TestL4MetaXPoolInventoryProcesses(t *testing.T) {
	if os.Getenv("GPUWORKS_REAL_METAX") != "1" {
		if os.Getenv("GPUWORKS_TEST_STRICT") == "1" {
			t.Fatal("set GPUWORKS_REAL_METAX=1 for read-only MetaX inventory")
		}
		t.Skip("set GPUWORKS_REAL_METAX=1 for read-only MetaX inventory")
	}
	p := &gpu.MetaXProber{}
	if !p.Available() {
		t.Fatal("mx-smi inventory unavailable")
	}
	cards, err := p.Probe(context.Background())
	if err != nil || len(cards) == 0 {
		t.Fatalf("probe cards=%d err=%v", len(cards), err)
	}
	processes, err := p.ComputeProcesses(context.Background())
	if err != nil {
		t.Fatalf("process query: %v", err)
	}
	metrics, err := p.Metrics(context.Background())
	if err != nil || len(metrics) != len(cards) {
		t.Fatalf("metrics=%d cards=%d err=%v", len(metrics), len(cards), err)
	}
	for _, card := range cards {
		t.Logf("index=%d model=%s memory_gib=%d healthy=%v", card.Index, card.Name, card.MemoryTotalBytes>>30, card.Healthy)
	}
	framework, version := p.Framework(context.Background())
	t.Logf("processes=%d driver=%s framework=%s/%s", len(processes), p.DriverVersion(context.Background()), framework, version)
	if result := p.RuntimeInjection([]int{cards[0].Index}); result.UnsupportedReason == "" || !result.Grant.Empty() {
		t.Fatalf("MetaX injection did not refuse: %+v", result)
	}
}
