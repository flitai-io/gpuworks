package controller

import (
	"maps"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/store"
)

// of is a copy of dep's clocks, nil when it has none.
func (d *drainClocks) of(dep uint) map[string]time.Time {
	d.mu.Lock()
	defer d.mu.Unlock()
	return maps.Clone(d.byDep[dep])
}

// startedAt moves the clock of an instance already seen draining.
func (d *drainClocks) startedAt(dep uint, instance string, at time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.byDep[dep][instance] = at
}

// An instance is removed after drainMin when idle, or after drainMax however
// busy; the first sight only starts its clock.
func TestDrainClocksBounds(t *testing.T) {
	start := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	busy := map[string]bool{"busy": true}
	isBusy := func(uuid string) bool { return busy[uuid] }
	var d drainClocks
	both := []string{"idle", "busy"}
	for _, step := range []struct {
		at      time.Duration
		ready   []string
		waiting bool
	}{
		{0, nil, true},
		{drainMin - time.Nanosecond, nil, true},
		{drainMin, []string{"idle"}, true},
		{drainMax - time.Nanosecond, nil, true},
		{drainMax, []string{"busy"}, false},
	} {
		ready, waiting := d.advance(1, both, start.Add(step.at), isBusy)
		if len(ready) != len(step.ready) || waiting != step.waiting {
			t.Fatalf("at %s: ready %v, waiting %v; want %v, %v", step.at, ready, waiting, step.ready, step.waiting)
		}
		for _, uuid := range step.ready {
			if !ready[uuid] {
				t.Fatalf("at %s: %s not ready (%v)", step.at, uuid, ready)
			}
			both = removeString(both, uuid)
		}
	}
	if d.of(1) != nil {
		t.Fatalf("clocks left after every instance drained: %v", d.of(1))
	}
}

func removeString(list []string, s string) []string {
	out := list[:0:0]
	for _, v := range list {
		if v != s {
			out = append(out, v)
		}
	}
	return out
}

// drainedUnits returns the ready instances in the order it was given them,
// each as often as it was given.
func TestDrainedUnitsKeepTheirOrder(t *testing.T) {
	start := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	c := New(nil, nil, nil)
	c.now = func() time.Time { return start }
	draining := []store.Instance{{UUID: "c"}, {UUID: "a"}, {UUID: "b"}, {UUID: "a"}}
	if got := c.drainedUnits(1, draining); len(got) != 0 {
		t.Fatalf("removed on first sight: %v", got)
	}
	c.now = func() time.Time { return start.Add(drainMin) }
	got := c.drainedUnits(1, draining)
	var order []string
	for _, inst := range got {
		order = append(order, inst.UUID)
	}
	if strings.Join(order, ",") != "c,a,b,a" {
		t.Fatalf("drained %v, want c,a,b,a", order)
	}
}
