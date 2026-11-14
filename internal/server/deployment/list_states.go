package deployment

import (
	"sync"
	"time"
)

// listStates is ListRows.States' memo, shared by the copies Fresh makes.
type listStates struct {
	mu     sync.Mutex
	states map[uint]string
	// until is when the states stop holding: the first time a deployment
	// waiting for its first instance passes the pending timeout (the one
	// state change no event announces). Zero holds them for good.
	until time.Time
}

// States is the state every deployment of the rows shows (EffectiveState,
// the state the deployment list, the dashboard, the monitor and the batches
// show), worked out once for the rows and handed to every reader, who must
// not change it.
//
// A deployment refused a placement for longer than the pending timeout reads
// failed rather than pending once the timeout passes, with no change
// announced: the states are worked out again then.
func (l *ListRows) States() map[uint]string {
	c := l.states
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.states != nil && (c.until.IsZero() || time.Now().Before(c.until)) {
		return c.states
	}
	// Worked out from the rows as built, not a page Fresh read again: the
	// states are shared by every copy.
	base := *l
	base.fresh = nil
	rows := &base
	states := make(map[uint]string, len(l.deploymentRows))
	var until time.Time
	for i := range l.deploymentRows {
		d := &l.deploymentRows[i]
		insts := l.instancesByDeployment[d.ID]
		states[d.ID] = EffectiveState(d, RuntimeState(rows, d, insts, RecordedModelProgress(rows, d)), FactsFor(rows, d, insts))
		if len(insts) > 0 {
			continue
		}
		// scheduleStalled measures from the first scheduling event.
		if ev, found := l.FirstScheduleEvent(d.ID); found {
			if at := ev.CreatedAt.Add(l.timeout); time.Now().Before(at) && (until.IsZero() || at.Before(until)) {
				until = at
			}
		}
	}
	c.states, c.until = states, until
	return states
}
