package automation

import (
	"errors"
	"slices"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/yieldpolicy"
)

// restartLog is what the store tells a restarted arbiter: what AwakeSince
// answers per deployment (absent: the events do not say), and what the event
// log says the resume list was (preemptedErr: a failed read of it).
type restartLog struct {
	awake        map[uint]time.Time
	awakeAsks    int
	preemptedLog []Preemption
	preemptedErr error
	preemptReads int
	clock        *clock // the evaluator's, once restarted sets it
}

func (w *residencyWorld) AwakeSince(id uint, _ []string, within time.Duration) (time.Time, bool) {
	w.awakeAsks++
	at, ok := w.awake[id]
	if !ok {
		return time.Time{}, false
	}
	if floor := w.clockNow().Add(-within); at.Before(floor) {
		at = floor // a stretch longer than within reads as within
	}
	return at, true
}

func (w *residencyWorld) Preempted() ([]Preemption, error) {
	w.preemptReads++
	if w.preemptedErr != nil {
		return nil, w.preemptedErr
	}
	return w.preemptedLog, nil
}

// clockNow is the evaluator's clock as the world sees it, for AwakeSince's
// window.
func (w *residencyWorld) clockNow() time.Time {
	if w.clock == nil {
		return t0
	}
	return w.clock.t
}

// restarted is the server starting again over the same world: the store and
// the engines keep their state, the arbiter's memory is gone.
func restarted(w *residencyWorld, at time.Time) (*Evaluator, *clock) {
	e, c := newResidencyEvaluator(w, at)
	w.clock = c
	return e, c
}

// A server killed after B yielded to A reads B back onto the resume list: once
// A's cards are free, B takes them back with no request, as it would have
// without the restart (TestAResumeTakesFreeCards).
func TestARestartedArbiterResumesFromTheEventLog(t *testing.T) {
	w, _, _ := sharedPair(t)
	const a, b = 1, 2
	w.preemptedLog = []Preemption{{DeploymentID: b, Since: t0.Add(-time.Hour), For: a}}
	e, c := restarted(w, t0.Add(5*time.Second))
	w.blocking[b] = Blocking{}
	w.fitsNow[b] = true
	c.t = t0.Add(10 * time.Second)
	e.residency(c.t)
	if !slices.Equal(w.wakeCalls, []uint{b}) || len(w.yields) != 0 {
		t.Fatalf("wakes %v yields %v, want B resumed on its free cards", w.wakeCalls, w.yields)
	}
	if got := w.eventsFor(b); !slices.Equal(got, []string{"2 AUTO_WAKE resume/<nil>"}) {
		t.Fatalf("B events = %v", got)
	}
	if _, ok := e.res.preempted[b]; ok {
		t.Fatal("B is still on the resume list after waking")
	}
}

// The resume list read back keeps the guard of a recent yield: B does not
// take back the cards it gave A while A still wakes, even though they are free.
func TestARestoredYieldStillGuardsTheCardsWhileTheOtherWakes(t *testing.T) {
	w, _, _ := sharedPair(t)
	const a, b = 1, 2
	w.asleep[a] = true // A's wake has not landed
	w.blocking[b] = Blocking{}
	w.fitsNow[b] = true
	e, c := restarted(w, t0)
	w.preemptedLog = []Preemption{{DeploymentID: b, Since: t0.Add(-30 * time.Second), For: a}}
	c.t = t0.Add(time.Second)
	e.residency(c.t)
	if len(w.wakeCalls) != 0 {
		t.Fatalf("B took its cards back while A still woke: wakes %v", w.wakeCalls)
	}
	// A wakes on its own: B, its cards free, takes them back - the guard ends
	// without starting A's turn over.
	w.asleep[a] = false
	c.t = t0.Add(2 * time.Second)
	e.residency(c.t)
	if !slices.Equal(w.wakeCalls, []uint{b}) {
		t.Fatalf("wakes %v, want B resumed once A woke", w.wakeCalls)
	}
	if at, ok := e.res.awakeAt[a]; ok {
		t.Fatalf("a restored guard started A's turn at %v", at)
	}
}

// A yield older than the longest guard is not guarded again: B resumes at
// once on free cards.
func TestAnOldRestoredYieldIsNotGuarded(t *testing.T) {
	w, _, _ := sharedPair(t)
	const a, b = 1, 2
	w.asleep[a] = true
	w.blocking[b] = Blocking{}
	w.fitsNow[b] = true
	w.preemptedLog = []Preemption{{DeploymentID: b, Since: t0.Add(-startGuard), For: a}}
	e, c := restarted(w, t0)
	c.t = t0.Add(time.Second)
	e.residency(c.t)
	if _, ok := e.res.yieldedFor[b]; ok {
		t.Fatal("a yield past the guard was guarded again")
	}
	if !slices.Equal(w.wakeCalls, []uint{b}) {
		t.Fatalf("wakes %v, want B resumed", w.wakeCalls)
	}
}

// The event log is read once, on the first look; a failed read is tried on
// the next look, and the list is not read again once it was.
func TestTheResumeListIsReadBackOnce(t *testing.T) {
	w, _, _ := sharedPair(t)
	const b = 2
	w.blocking[b] = Blocking{}
	w.fitsNow[b] = true
	w.preemptedErr = errors.New("database is locked")
	w.preemptedLog = []Preemption{{DeploymentID: b, Since: t0.Add(-time.Hour)}}
	e, c := restarted(w, t0)
	c.t = t0.Add(time.Second)
	e.residency(c.t)
	if len(w.wakeCalls) != 0 || w.preemptReads != 1 {
		t.Fatalf("after a failed read: wakes %v reads %d", w.wakeCalls, w.preemptReads)
	}
	w.preemptedErr = nil
	for i := 2; i <= 4; i++ {
		c.t = t0.Add(time.Duration(i) * time.Second)
		e.residency(c.t)
	}
	if w.preemptReads != 2 {
		t.Fatalf("read the event log %d times, want twice (the failure, then once)", w.preemptReads)
	}
	if !slices.Equal(w.wakeCalls, []uint{b}) {
		t.Fatalf("wakes %v, want B resumed after the read succeeded", w.wakeCalls)
	}
}

// A request waits for B; A (time-sliced, max_serve 120 s) has served since t0
// and is busy. Without a restart A drains for B at t0+120s; a server that
// restarts at t0+119s tells A's turn from A's events, so A drains then too
// rather than serving a whole new turn. It is the time a turn can be told
// from; idle time is not (TestARestartDoesNotTellIdleTimeFromBeforeIt).
func TestARestartDoesNotHandATimeSlicedHolderANewTurn(t *testing.T) {
	drainsAt := func(restartAt time.Duration, eventsSay bool) time.Duration {
		w, e, c := sharedPair(t)
		const a, b = 1, 2
		w.clock = c
		w.inFlight["a-1"] = 1
		w.waiters = []Waiter{{DeploymentID: b, Policy: pol(yieldpolicy.TimeSlice), Since: t0.Add(100 * time.Second)}}
		if eventsSay {
			w.awake[a] = t0
		}
		for s := 100 * time.Second; s <= 400*time.Second; s += time.Second {
			if restartAt > 0 && s == restartAt {
				e, c = restarted(w, t0.Add(s))
			}
			c.t = t0.Add(s)
			w.activity[a] = Activity{InFlight: 1, LastRequest: c.t}
			e.residency(c.t)
			if len(w.drains) > 0 {
				return s
			}
		}
		return -1
	}
	if got := drainsAt(0, true); got != 120*time.Second {
		t.Fatalf("without a restart A drained at t0+%v, want t0+2m", got)
	}
	if got := drainsAt(119*time.Second, true); got != 120*time.Second {
		t.Fatalf("after a restart at t0+119s A drained at t0+%v, want t0+2m (its turn read back)", got)
	}
	// Events that do not say leave the turn to start with the process, as
	// before.
	if got := drainsAt(119*time.Second, false); got != 239*time.Second {
		t.Fatalf("with no events A drained at t0+%v, want t0+3m59s", got)
	}
}

// A time-sliced holder an operator woke after the server started serves its
// turn from that wake, not from the server's start: its events say when.
func TestAHolderWokenByAnOperatorServesItsTurnFromThatWake(t *testing.T) {
	w := newResidencyWorld()
	const a, b = 1, 2
	w.levels[a], w.levels[b] = pol(yieldpolicy.TimeSlice), pol(yieldpolicy.TimeSlice)
	w.asleep[b] = true
	w.blocking[b] = Blocking{Holders: []Holder{{DeploymentID: a, Policy: pol(yieldpolicy.TimeSlice), Instances: []string{"a-1"}}}}
	e, c := restarted(w, t0)
	w.awake[a] = t0.Add(100 * time.Second) // the operator's wake
	w.inFlight["a-1"] = 1
	w.waiters = []Waiter{{DeploymentID: b, Policy: pol(yieldpolicy.TimeSlice), Since: t0.Add(150 * time.Second)}}
	var drainedAt time.Duration
	for s := 150 * time.Second; s <= 300*time.Second; s += time.Second {
		c.t = t0.Add(s)
		w.activity[a] = Activity{InFlight: 1, LastRequest: c.t}
		e.residency(c.t)
		if len(w.drains) > 0 {
			drainedAt = s
			break
		}
	}
	if drainedAt != 220*time.Second {
		t.Fatalf("A drained at t0+%v, want t0+3m40s (its turn from the wake at t0+1m40s)", drainedAt)
	}
}

// Idle time is not told from the events: a request a restart cut off left no
// record, so an idle-policy holder awake for an hour before the restart still
// counts its idle time from the restart.
func TestARestartDoesNotTellIdleTimeFromBeforeIt(t *testing.T) {
	w := newResidencyWorld()
	const a, b = 1, 2
	idle := pol(yieldpolicy.Idle) // idle_sec 60
	w.levels[a], w.levels[b] = idle, pol(yieldpolicy.Idle)
	w.asleep[b] = true
	w.blocking[b] = Blocking{Holders: []Holder{{DeploymentID: a, Policy: idle, Instances: []string{"a-1"}}}}
	w.awake[a] = t0.Add(-time.Hour)
	w.waiters = []Waiter{{DeploymentID: b, Policy: pol(yieldpolicy.Idle), Since: t0}}
	e, c := restarted(w, t0)
	c.t = t0.Add(30 * time.Second)
	e.residency(c.t)
	if len(w.yields) != 0 {
		t.Fatalf("A yielded %v 30 s after the restart, idle by events from before it", w.yields)
	}
	c.t = t0.Add(time.Minute)
	e.residency(c.t)
	if !slices.Equal(w.yields, []string{"1 abort=false"}) {
		t.Fatalf("yields %v, want A to yield once idle 60 s from the restart", w.yields)
	}
	if w.awakeAsks != 0 {
		t.Fatalf("asked the events %d times for a holder that does not take turns", w.awakeAsks)
	}
}

// The events' answer is kept for awakeRecheck, so a look on every busy poll
// does not read the timeline each time.
func TestTheTurnAnswerIsKeptBetweenLooks(t *testing.T) {
	w, _, _ := sharedPair(t)
	const a, b = 1, 2
	w.awake[a] = t0
	w.inFlight["a-1"] = 1
	w.waiters = []Waiter{{DeploymentID: b, Policy: pol(yieldpolicy.TimeSlice), Since: t0}}
	e, c := restarted(w, t0)
	for s := time.Second; s <= 9*time.Second; s += time.Second {
		c.t = t0.Add(s)
		w.activity[a] = Activity{InFlight: 1, LastRequest: c.t}
		e.residency(c.t)
	}
	if w.awakeAsks != 1 {
		t.Fatalf("asked the events %d times within %v, want once", w.awakeAsks, awakeRecheck)
	}
	c.t = t0.Add(awakeRecheck + time.Second)
	e.residency(c.t)
	if w.awakeAsks != 2 {
		t.Fatalf("asked the events %d times after %v, want twice", w.awakeAsks, awakeRecheck)
	}
}
