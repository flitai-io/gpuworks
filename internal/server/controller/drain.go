package controller

import (
	"sync"
	"time"

	"gpuworks.ai/gpuworks/internal/server/store"
)

// DrainProbe reports how much work an instance still holds at the gateway:
// requests it answers and, for a prefill instance, KV it keeps for a decoder
// that has not pulled it yet. The app wires the gateway in; without one,
// draining waits only for the minimum below.
type DrainProbe interface {
	InFlight(instanceUUID string) int
}

// SetDrainProbe wires the gateway's view of in-flight work.
func (c *Controller) SetDrainProbe(probe DrainProbe) { c.drainProbe = probe }

// Draining bounds. The gateway stops routing new work to a draining instance
// as soon as its route table rebuilds; drainMin covers that rebuild. drainMax
// bounds the wait for a request that never finishes.
const (
	drainMin  = 3 * time.Second
	drainMax  = 5 * time.Minute
	drainPoll = 2 * time.Second
)

// drainedUnits returns the draining instances that may be removed now: those
// that have drained for at least drainMin and hold no work, or have drained
// for drainMax. The first time an instance is seen draining only starts its
// clock. The clock lives in memory: after a server restart the wait starts
// over, which only lengthens it.
func (c *Controller) drainedUnits(depID uint, draining []store.Instance) []store.Instance {
	uuids := make([]string, len(draining))
	for i := range draining {
		uuids[i] = draining[i].UUID
	}
	busy := func(uuid string) bool { return c.drainProbe != nil && c.drainProbe.InFlight(uuid) > 0 }
	done, waiting := c.drainClocks.advance(depID, uuids, c.now(), busy)
	var ready []store.Instance
	for i := range draining {
		if done[draining[i].UUID] {
			ready = append(ready, draining[i])
		}
	}
	if waiting {
		c.q.addAfter(depKey(depID), drainPoll)
	}
	return ready
}

// drainClocks is when each draining instance was first seen draining, by
// deployment. Its zero value is ready to use.
type drainClocks struct {
	mu    sync.Mutex
	byDep map[uint]map[string]time.Time // by deployment, then instance
}

// advance moves dep's clocks to now for the instances still draining and
// says which may be removed (drained for drainMax, or for drainMin while not
// busy) and whether any is still waiting. The clocks are rebuilt each pass
// from the instances still draining, so one that left some other way leaves
// no clock behind.
func (d *drainClocks) advance(dep uint, draining []string, now time.Time, busy func(string) bool) (ready map[string]bool, waiting bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	clocks := d.byDep[dep]
	next := map[string]time.Time{}
	ready = map[string]bool{}
	for _, uuid := range draining {
		since, seen := clocks[uuid]
		if !seen {
			next[uuid] = now
			waiting = true
			continue
		}
		elapsed := now.Sub(since)
		if elapsed >= drainMax || (elapsed >= drainMin && !busy(uuid)) {
			ready[uuid] = true
			continue
		}
		next[uuid] = since
		waiting = true
	}
	if len(next) == 0 {
		delete(d.byDep, dep)
		return ready, waiting
	}
	if d.byDep == nil {
		d.byDep = map[uint]map[string]time.Time{}
	}
	d.byDep[dep] = next
	return ready, waiting
}

// forget drops the clocks of dep's instances.
func (d *drainClocks) forget(dep uint) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.byDep, dep)
}
