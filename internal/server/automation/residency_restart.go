package automation

import (
	"slices"
	"strings"
	"time"
)

// What the residency arbiter's memory loses when the server stops, and what a
// server that has just started tells again from what the store keeps. Nothing
// is written for it: the event log the arbiter writes as it acts, and the
// instance events, already say it.
//
//   - The resume list (residencyState.preempted): a deployment whose latest
//     sleep was a yield to another, and that still sleeps for it, takes its
//     cards back once they are free, as it would have without the restart.
//     The guard that keeps it from taking them back while the one it yielded
//     to still wakes is read back with it.
//   - The start of a time-sliced holder's turn: its instance events say when
//     it began serving, so a restart does not hand it a new turn.
//
// Idle time is not told again: a request the restart cut off was never
// recorded as having run, so the store cannot show a holder idle before this
// process began watching. It counts from the start, as before.

// awakeRecheck is how long the arbiter keeps an answer of AwakeSince before
// asking again: the turn of a holder it did not see wake is looked at on
// every look that reaches it, and the events change only when the holder
// sleeps or wakes.
const awakeRecheck = 10 * time.Second

// seenAwake is an answer of AwakeSince for a holder.
type seenAwake struct {
	at, read  time.Time
	ok        bool
	instances string // the holder's instances the answer is for
}

// restorePreempted reads the resume list back once, on the first look. A
// deployment already on it, or already guarded, keeps what the arbiter has.
func (e *Evaluator) restorePreempted() {
	r := e.res
	if r.restored {
		return
	}
	list, err := r.source.Preempted()
	if err != nil {
		e.log.Warn("automation: read the deployments put to sleep for another", "err", err)
		return
	}
	r.restored = true
	now := e.now()
	for _, p := range list {
		if _, ok := r.preempted[p.DeploymentID]; !ok {
			r.preempted[p.DeploymentID] = p.Since
		}
		// A guard past its longest wait would end at once: it is not read
		// back, and the turn of the one it was for is left to that one's
		// events.
		if _, ok := r.yieldedFor[p.DeploymentID]; !ok && p.For != 0 && now.Sub(p.Since) < startGuard {
			r.yieldedFor[p.DeploymentID] = yielded{to: p.For, at: p.Since, restored: true}
		}
	}
	if len(list) > 0 {
		e.log.Info("automation: resume list read back from the event log", "deployments", len(list))
	}
}

// turnStart is when the holder's turn began: since (the arbiter's memory, no
// earlier than the start) unless the holder takes turns (a time-sliced one,
// the only kind the turn bounds) and its instance events say it began serving
// at some other time the arbiter did not see. A wake the arbiter saw that is
// later than the events' answer is the start: it is the one that counts.
func (e *Evaluator) turnStart(now time.Time, h Holder, since time.Time) time.Time {
	turn := h.Policy.Turn()
	if turn <= 0 || len(h.Instances) == 0 {
		return since
	}
	r := e.res
	key := strings.Join(slices.Sorted(slices.Values(h.Instances)), ",")
	seen, ok := r.awakeSeen[h.DeploymentID]
	if !ok || seen.instances != key || now.Sub(seen.read) >= awakeRecheck {
		at, found := r.source.AwakeSince(h.DeploymentID, h.Instances, turn)
		seen = seenAwake{at: at, read: now, ok: found, instances: key}
		r.awakeSeen[h.DeploymentID] = seen
	}
	if !seen.ok || seen.at.After(now) {
		return since
	}
	start := seen.at
	if at, ok := r.awakeAt[h.DeploymentID]; ok && at.After(start) {
		start = at
	}
	return start
}
