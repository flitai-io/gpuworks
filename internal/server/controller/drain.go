package controller

import (
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
	c.drainMu.Lock()
	defer c.drainMu.Unlock()
	if c.draining == nil {
		c.draining = map[uint]map[string]time.Time{}
	}
	// The deployment's clocks are rebuilt each pass from the instances still
	// draining, so one that left some other way leaves no clock behind.
	clocks := c.draining[depID]
	next := map[string]time.Time{}
	now := c.now()
	var ready []store.Instance
	waiting := false
	for _, inst := range draining {
		since, seen := clocks[inst.UUID]
		if !seen {
			next[inst.UUID] = now
			waiting = true
			continue
		}
		elapsed := now.Sub(since)
		busy := c.drainProbe != nil && c.drainProbe.InFlight(inst.UUID) > 0
		if elapsed >= drainMax || (elapsed >= drainMin && !busy) {
			ready = append(ready, inst)
			continue
		}
		next[inst.UUID] = since
		waiting = true
	}
	if len(next) == 0 {
		delete(c.draining, depID)
	} else {
		c.draining[depID] = next
	}
	if waiting {
		c.q.addAfter(depKey(depID), drainPoll)
	}
	return ready
}

// forgetDrainClocks drops the drain clocks of a deployment's instances.
func (c *Controller) forgetDrainClocks(depID uint) {
	c.drainMu.Lock()
	defer c.drainMu.Unlock()
	delete(c.draining, depID)
}
