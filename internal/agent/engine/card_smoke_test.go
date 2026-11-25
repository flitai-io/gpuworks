package engine

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/agent/gpu"
	"gpuworks.ai/gpuworks/internal/proto"
)

// A start waits for a runtime check on its cards before it launches the
// container: on a card only one process may open, the two would fail each
// other.
func TestStartWaitsForARuntimeCheckOnItsCards(t *testing.T) {
	h := newSleepHTTPServer(t)
	driver := &imageToleranceDriver{headlessTestDriver: headlessTestDriver{name: "docker"}, exists: []bool{true}}
	prober := &gpu.FixedProber{VendorName: "ascend", GPUs: []proto.GPUInfo{{Index: 3, UUID: "ascend-3", PCIeBDF: "0000:41:00.0"}}}
	e := NewWithProber(driver, func(proto.InstanceStatus) {}, slog.Default(), t.TempDir(), prober)
	ident := accelerator.CardIdent("ascend-3", "0000:41:00.0")
	var gated []string
	runsAtGate := -1
	e.SetCardGate(func(_ context.Context, idents []string, limit time.Duration) bool {
		gated, runsAtGate = idents, driver.runCalls
		if limit != cardGateLimit {
			t.Errorf("limit = %s", limit)
		}
		return true
	})
	e.startOrAdopt(context.Background(), proto.InstanceDesiredState{
		UUID: "gated", Image: "repo:v1", GPUIndexes: []int{3}, GPUIdents: []string{ident},
		APIPort: h.port(t), Ports: []int{h.port(t)}, StartTimeout: 1,
	})
	if len(gated) != 1 || gated[0] != ident || runsAtGate != 0 || driver.runCalls != 1 {
		t.Fatalf("gate saw %v with %d runs before it; runs after = %d", gated, runsAtGate, driver.runCalls)
	}
}

// A check that does not finish within the limit does not hold the start.
func TestStartGoesOnWhenTheCheckDoesNotFinish(t *testing.T) {
	e := New(&headlessTestDriver{}, nil, slog.Default(), t.TempDir())
	e.waitForCardChecks(context.Background(), "u", []string{"x"}) // no gate: no-op
	called := false
	e.SetCardGate(func(context.Context, []string, time.Duration) bool { called = true; return false })
	e.waitForCardChecks(context.Background(), "u", nil)
	if called {
		t.Fatal("an instance without bound cards waited on the gate")
	}
	e.waitForCardChecks(context.Background(), "u", []string{"x"})
	if !called {
		t.Fatal("the gate was not consulted")
	}
}
