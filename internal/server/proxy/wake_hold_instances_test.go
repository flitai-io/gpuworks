package proxy

import (
	"io"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// Holding is per instance: a deployment with one replica the platform put to
// sleep and one an operator holds asleep holds a request for the former's
// wake; once both are held, a request is refused at once.
func TestARequestIsHeldWhileOneSleeperWakesOnRequest(t *testing.T) {
	p, st, key, depID, front := newSleepingProxy(t, false)
	if _, err := st.CreateInstance(&store.Instance{
		UUID: "omni-held", DeploymentID: depID, State: proto.InstRunning, APIPort: 30001, Ports: "[30001]", SleepLevel: 2, SleepHeld: true,
	}); err != nil {
		t.Fatal(err)
	}
	var asked atomic.Int32
	p.SetWakeDemand(func(id uint) {
		if asked.Add(1) != 1 {
			return
		}
		go func() {
			time.Sleep(100 * time.Millisecond)
			// The arbiter wakes the platform's sleeper, not the held one.
			if err := updateInstanceRow(st, "omni-instance", map[string]any{"sleep_level": 0}); err != nil {
				t.Error(err)
			}
		}()
	})
	resp := postChat(t, front, key)
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || asked.Load() == 0 {
		t.Fatalf("status = %d body = %s demand = %d, want the request held for the platform's sleeper", resp.StatusCode, body, asked.Load())
	}

	// Every sleeper held: refused at once, and nothing is asked to wake.
	asked.Store(0)
	if err := updateInstanceRow(st, "omni-instance", map[string]any{"sleep_level": 1, "sleep_held": true}); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if resp := postChat(t, front, key); resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("every sleeper held: status = %d, want 503", resp.StatusCode)
	}
	if asked.Load() != 0 || time.Since(start) > 5*time.Second {
		t.Fatalf("a request for held sleepers asked for a wake (%d) or waited %v", asked.Load(), time.Since(start))
	}
}
