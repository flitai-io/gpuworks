package proxy

import (
	"context"

	"gpuworks.ai/gpuworks/internal/server/priority"
)

// admission is what the request being routed brings to a pick: the value of
// the class it runs at (priority.Class.Value, larger is more important), and
// whether it asked to be refused rather than wait when no replica has a free
// run slot for it (busy_reject.go). The lease taken for it holds that value
// until released.
type admission struct {
	priority   int
	rejectBusy bool
}

// admissionFrom is the request's admission: the class the gateway decided
// for it (decidePriority), and the neutral value when none was decided.
func admissionFrom(ctx context.Context) admission {
	if decided, ok := ctx.Value(priorityDecisionKey{}).(priority.Decision); ok {
		return admission{priority: decided.Class.Value}
	}
	return admission{}
}

// priorityCount is how many leases of one priority value an instance holds.
type priorityCount struct {
	value, leases int
}

// inflightCounts are the leases an instance holds, by the priority value of
// the request holding each. An instance holds leases of a few values at a
// time (one per class in use), so they are a short list kept without the
// values no lease holds.
type inflightCounts struct {
	total   int
	byValue []priorityCount
}

func (c *inflightCounts) add(value int) {
	c.total++
	for i := range c.byValue {
		if c.byValue[i].value == value {
			c.byValue[i].leases++
			return
		}
	}
	c.byValue = append(c.byValue, priorityCount{value: value, leases: 1})
}

// remove gives back one lease of value; one never added is ignored.
func (c *inflightCounts) remove(value int) {
	for i := range c.byValue {
		if c.byValue[i].value != value {
			continue
		}
		c.total--
		if c.byValue[i].leases--; c.byValue[i].leases == 0 {
			c.byValue = append(c.byValue[:i], c.byValue[i+1:]...)
		}
		return
	}
}

// atLeast is how many of the leases are held by requests of priority value
// or above: those a request of value waits behind, since the engine runs a
// lower one after it.
func (c *inflightCounts) atLeast(value int) int {
	n := 0
	for _, count := range c.byValue {
		if count.value >= value {
			n += count.leases
		}
	}
	return n
}

// take leases c for adm; the caller holds b.mu.
func (b *balancer) take(c candidate, adm admission) *lease {
	b.state(c.instanceUUID).inflight.add(adm.priority)
	l := &lease{c: c, priority: adm.priority}
	l.releaseFn = func(o attemptOutcome) { b.release(l, o) }
	return l
}
