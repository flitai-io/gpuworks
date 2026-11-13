package automation

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/yieldpolicy"
)

// residencyWorld is the arbiter's view of a few deployments on shared cards.
type residencyWorld struct {
	*fakeWorld
	levels    map[uint]yieldpolicy.Policy
	asleep    map[uint]bool // asleep by the platform's choice
	waiters   []Waiter
	blocking  map[uint]Blocking
	activity  map[uint]Activity
	fitsNow   map[uint]bool // Wake wakes it at once
	yields    []string
	drains    []string
	inFlight  map[string]int // requests running, by instance
	yieldErr  error
	wakeCalls []uint
	// heldWakes are the wakes asked to wake held sleepers too: the arbiter
	// never asks that, a request or a resume wakes only the platform's.
	heldWakes []uint
	settling  map[uint]bool // waiting to be placed, or starting
	restartLog
}

func newResidencyWorld() *residencyWorld {
	return &residencyWorld{
		fakeWorld: newFakeWorld(), levels: map[uint]yieldpolicy.Policy{}, asleep: map[uint]bool{}, blocking: map[uint]Blocking{},
		activity: map[uint]Activity{}, fitsNow: map[uint]bool{}, settling: map[uint]bool{}, inFlight: map[string]int{}, restartLog: restartLog{awake: map[uint]time.Time{}},
	}
}

func (w *residencyWorld) Waiters() ([]Waiter, error) { return w.waiters, nil }
func (w *residencyWorld) Wakeable(id uint) (yieldpolicy.Policy, bool) {
	return w.levels[id], w.asleep[id]
}
func (w *residencyWorld) Blocking(id uint) (Blocking, error) { return w.blocking[id], nil }
func (w *residencyWorld) Activity(id uint) Activity          { return w.activity[id] }
func (w *residencyWorld) Settling(id uint) bool              { return w.settling[id] }
func (w *residencyWorld) Yield(id uint, instances []string, abort bool) (int, error) {
	w.yields = append(w.yields, fmt.Sprintf("%d abort=%v", id, abort))
	if w.yieldErr != nil {
		return 0, w.yieldErr
	}
	w.asleep[id] = true
	return 1, nil
}
func (w *residencyWorld) InstancesInFlight(instances []string) int {
	n := 0
	for _, uuid := range instances {
		n += w.inFlight[uuid]
	}
	return n
}
func (w *residencyWorld) Drain(id uint, instances []string, on bool) {
	w.drains = append(w.drains, fmt.Sprintf("%d %s %v", id, strings.Join(instances, ","), on))
}
func (w *residencyWorld) Wake(id uint, held bool) (WakeResult, error) {
	w.wakeCalls = append(w.wakeCalls, id)
	if held {
		w.heldWakes = append(w.heldWakes, id)
	}
	if w.fitsNow[id] {
		w.asleep[id] = false
		return WakeResult{Woken: []string{fmt.Sprintf("i-%d", id)}}, nil
	}
	return WakeResult{Waiting: []string{fmt.Sprintf("i-%d", id)}}, nil
}

func newResidencyEvaluator(w *residencyWorld, start time.Time) (*Evaluator, *clock) {
	e, c := newEvaluator(w.fakeWorld, start)
	e.actions = w
	e.WithResidency(w, w)
	e.started = start
	return e, c
}

func (w *residencyWorld) eventsFor(id uint) []string {
	var out []string
	prefix := fmt.Sprintf("%d ", id)
	for i, ev := range w.events {
		if strings.HasPrefix(ev, prefix) {
			out = append(out, fmt.Sprintf("%s %v/%v", ev, w.params[i]["trigger"], w.params[i]["reason"]))
		}
	}
	return out
}

// pol is a yield policy with its defaults.
func pol(yield string) yieldpolicy.Policy {
	p, _ := yieldpolicy.Parse(yield, yieldpolicy.Overrides{})
	return p
}

var t0 = time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC)

func TestARequestWakesADeploymentThatFits(t *testing.T) {
	w := newResidencyWorld()
	w.levels[1] = pol(yieldpolicy.Never)
	w.asleep[1] = true
	w.fitsNow[1] = true
	w.waiters = []Waiter{{DeploymentID: 1, Policy: pol(yieldpolicy.Never), Since: t0}}
	e, c := newResidencyEvaluator(w, t0)
	e.residency(c.t)
	if !slices.Equal(w.events, []string{"1 AUTO_WAKE"}) || w.params[0]["trigger"] != TriggerRequest {
		t.Fatalf("events = %v %v", w.events, w.params)
	}
	// The wake is in flight: the next passes leave it alone for a while.
	c.t = c.t.Add(5 * time.Second)
	e.residency(c.t)
	if len(w.wakeCalls) != 1 {
		t.Fatalf("woke again while the first wake was in flight: %v", w.wakeCalls)
	}
	c.t = c.t.Add(wakeRepeat)
	e.residency(c.t)
	if len(w.wakeCalls) != 2 {
		t.Fatalf("a request still waiting after the repeat time was not looked at: %v", w.wakeCalls)
	}
}

func TestOnlineTakesItsCardsFromOfflineAndOfflineTakesThemBackOnARequest(t *testing.T) {
	w := newResidencyWorld()
	const online, offline = 1, 2
	w.levels[online], w.levels[offline] = pol(yieldpolicy.Idle), pol(yieldpolicy.Anytime)
	w.asleep[online] = true
	w.activity[offline] = Activity{InFlight: 3, LastRequest: t0}
	w.waiters = []Waiter{{DeploymentID: online, Policy: pol(yieldpolicy.Idle), Since: t0}}
	w.blocking[online] = Blocking{Holders: []Holder{{DeploymentID: offline, Policy: pol(yieldpolicy.Anytime), Instances: []string{"off-1"}}}}
	e, c := newResidencyEvaluator(w, t0)
	e.residency(c.t)
	if !slices.Equal(w.yields, []string{"2 abort=true"}) {
		t.Fatalf("offline was not cut off at once for online: %v", w.yields)
	}
	if got := w.eventsFor(offline); !slices.Equal(got, []string{"2 AUTO_SLEEP preempt/rank"}) {
		t.Fatalf("offline events = %v", got)
	}

	// Online wakes (its wake waited for the room) and serves; offline wants
	// its cards back but online is busy.
	w.waiters = nil
	w.asleep[online] = false
	w.fitsNow[offline] = false
	w.blocking[offline] = Blocking{Holders: []Holder{{DeploymentID: online, Policy: pol(yieldpolicy.Idle), Instances: []string{"on-1"}}}}
	w.activity[online] = Activity{InFlight: 1, LastRequest: c.t}
	c.t = c.t.Add(time.Minute)
	e.residency(c.t)
	if len(w.yields) != 1 {
		t.Fatalf("busy online was put to sleep for offline: %v", w.yields)
	}
	// Online idles past its minute: a resume answers no one, so online
	// keeps its cards.
	w.activity[online] = Activity{LastRequest: c.t}
	c.t = c.t.Add(10 * time.Minute)
	e.residency(c.t)
	if len(w.yields) != 1 {
		t.Fatalf("idle online gave way to offline only resuming: %v", w.yields)
	}
	// A request for offline is held (its client retries the cut-off work):
	// now someone waits for it, and idle online gives way.
	w.waiters = []Waiter{{DeploymentID: offline, Policy: pol(yieldpolicy.Anytime), Since: c.t}}
	e.residency(c.t)
	if !slices.Equal(w.yields, []string{"2 abort=true", "1 abort=false"}) {
		t.Fatalf("idle online did not give way to a request for offline: %v", w.yields)
	}
	if got := w.eventsFor(online); !slices.Equal(got, []string{"1 AUTO_SLEEP preempt/idle"}) {
		t.Fatalf("online events = %v", got)
	}
	// Online is not preemptible: it does not try to resume on its own, only
	// when a request comes.
	if _, ok := e.res.preempted[online]; ok {
		t.Fatal("online was queued to resume without a request")
	}
	if _, ok := e.res.preempted[offline]; !ok {
		t.Fatal("offline left the resume queue before it woke")
	}
}

func TestSharedDeploymentsTakeTurnsAndDrain(t *testing.T) {
	w := newResidencyWorld()
	const a, b = 1, 2
	w.levels[a], w.levels[b] = pol(yieldpolicy.TimeSlice), pol(yieldpolicy.TimeSlice)
	w.asleep[b] = true
	w.activity[a] = Activity{InFlight: 2, LastRequest: t0}
	w.inFlight["a-1"] = 2
	w.blocking[b] = Blocking{Holders: []Holder{{DeploymentID: a, Policy: pol(yieldpolicy.TimeSlice), Instances: []string{"a-1"}}}}
	e, c := newResidencyEvaluator(w, t0)
	e.res.awakeAt[a] = t0
	w.waiters = []Waiter{{DeploymentID: b, Policy: pol(yieldpolicy.TimeSlice), Since: t0}}

	c.t = t0.Add(time.Minute)
	e.residency(c.t)
	if len(w.yields)+len(w.drains) != 0 {
		t.Fatalf("a took b's cards before its turn: yields %v drains %v", w.yields, w.drains)
	}
	c.t = t0.Add(2*time.Minute + wakeRepeat)
	e.residency(c.t)
	if !slices.Equal(w.drains, []string{"1 a-1 true"}) || len(w.yields) != 0 {
		t.Fatalf("a busy shared holder must drain first: drains %v yields %v", w.drains, w.yields)
	}
	// Its requests finish within the drain: it sleeps without cutting any off.
	w.activity[a] = Activity{LastRequest: c.t}
	w.inFlight["a-1"] = 0
	c.t = c.t.Add(10 * time.Second)
	e.residency(c.t)
	if !slices.Equal(w.yields, []string{"1 abort=false"}) || !slices.Equal(w.drains, []string{"1 a-1 true", "1 a-1 false"}) {
		t.Fatalf("after draining: yields %v drains %v", w.yields, w.drains)
	}
	if got := w.eventsFor(a); !slices.Equal(got, []string{"1 AUTO_SLEEP preempt/turn"}) {
		t.Fatalf("a's events = %v", got)
	}
}

func TestADrainThatRunsOverCutsTheRestOff(t *testing.T) {
	w := newResidencyWorld()
	const a, b = 1, 2
	w.levels[a], w.levels[b] = pol(yieldpolicy.TimeSlice), pol(yieldpolicy.Idle)
	w.asleep[b] = true
	w.activity[a] = Activity{InFlight: 1, LastRequest: t0}
	w.inFlight["a-1"] = 1
	w.blocking[b] = Blocking{Holders: []Holder{{DeploymentID: a, Policy: pol(yieldpolicy.TimeSlice), Instances: []string{"a-1"}}}}
	w.waiters = []Waiter{{DeploymentID: b, Policy: pol(yieldpolicy.Idle), Since: t0}}
	e, c := newResidencyEvaluator(w, t0)
	e.residency(c.t)
	if !slices.Equal(w.drains, []string{"1 a-1 true"}) {
		t.Fatalf("drains = %v", w.drains)
	}
	c.t = c.t.Add(29 * time.Second)
	e.residency(c.t)
	if len(w.yields) != 0 {
		t.Fatalf("cut off before the drain time: %v", w.yields)
	}
	c.t = c.t.Add(time.Second)
	e.residency(c.t)
	if !slices.Equal(w.yields, []string{"1 abort=true"}) {
		t.Fatalf("an overrun drain was not cut off: %v", w.yields)
	}
}

func TestNoOneYieldsUnlessEveryHolderMay(t *testing.T) {
	for _, tc := range []struct {
		name     string
		blocking Blocking
	}{
		{"a resident holder", Blocking{Holders: []Holder{{DeploymentID: 2, Policy: pol(yieldpolicy.Anytime)}, {DeploymentID: 3, Policy: pol(yieldpolicy.Never)}}}},
		{"a busy online holder", Blocking{Holders: []Holder{{DeploymentID: 2, Policy: pol(yieldpolicy.Anytime)}, {DeploymentID: 3, Policy: pol(yieldpolicy.Idle)}}}},
		{"an occupant nothing can move", Blocking{Holders: []Holder{{DeploymentID: 2, Policy: pol(yieldpolicy.Anytime)}}, Blocked: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newResidencyWorld()
			w.levels[1] = pol(yieldpolicy.TimeSlice)
			w.asleep[1] = true
			w.activity[3] = Activity{InFlight: 1}
			w.blocking[1] = tc.blocking
			w.waiters = []Waiter{{DeploymentID: 1, Policy: pol(yieldpolicy.TimeSlice), Since: t0}}
			e, c := newResidencyEvaluator(w, t0)
			c.t = t0.Add(time.Hour)
			e.residency(c.t)
			if len(w.yields)+len(w.drains) != 0 {
				t.Fatalf("yields %v drains %v", w.yields, w.drains)
			}
		})
	}
}

func TestHigherRankWaitersAreServedFirstAndAHolderYieldsOnce(t *testing.T) {
	w := newResidencyWorld()
	// Offline 3 holds the cards both shared 1 (waiting longer) and online 2
	// want: online goes first, and the holder is not put to sleep twice.
	w.levels[1], w.levels[2], w.levels[3] = pol(yieldpolicy.TimeSlice), pol(yieldpolicy.Idle), pol(yieldpolicy.Anytime)
	w.asleep[1], w.asleep[2] = true, true
	holder := Blocking{Holders: []Holder{{DeploymentID: 3, Policy: pol(yieldpolicy.Anytime), Instances: []string{"off"}}}}
	w.blocking[1], w.blocking[2] = holder, holder
	w.waiters = []Waiter{{DeploymentID: 1, Policy: pol(yieldpolicy.TimeSlice), Since: t0}, {DeploymentID: 2, Policy: pol(yieldpolicy.Idle), Since: t0.Add(time.Minute)}}
	w.activity[3] = Activity{InFlight: 4}
	e, c := newResidencyEvaluator(w, t0)
	c.t = t0.Add(2 * time.Minute)
	e.residency(c.t)
	if !slices.Equal(w.wakeCalls, []uint{2, 1}) {
		t.Fatalf("wake order = %v, want online first", w.wakeCalls)
	}
	if len(w.heldWakes) > 0 {
		t.Fatalf("the arbiter woke held sleepers of %v: a request wakes only the platform's", w.heldWakes)
	}
	if !slices.Equal(w.yields, []string{"3 abort=true"}) {
		t.Fatalf("yields = %v", w.yields)
	}
	if w.params[0]["for_deployment"] != uint(2) {
		t.Fatalf("holder yielded for %v, want online", w.params[0]["for_deployment"])
	}
}

func TestAFailedYieldIsRecorded(t *testing.T) {
	w := newResidencyWorld()
	w.levels[1], w.levels[2] = pol(yieldpolicy.Idle), pol(yieldpolicy.Anytime)
	w.asleep[1] = true
	w.yieldErr = fmt.Errorf("agent unreachable")
	w.blocking[1] = Blocking{Holders: []Holder{{DeploymentID: 2, Policy: pol(yieldpolicy.Anytime)}}}
	w.waiters = []Waiter{{DeploymentID: 1, Policy: pol(yieldpolicy.Idle), Since: t0}}
	e, c := newResidencyEvaluator(w, t0)
	e.residency(c.t)
	if got := w.eventsFor(2); len(got) != 1 || w.params[0]["action"] != "sleep" || w.params[0]["reason"] != "agent unreachable" {
		t.Fatalf("events = %v params %v", got, w.params)
	}
	if _, ok := e.res.preempted[2]; ok {
		t.Fatal("a holder that did not sleep was queued to resume")
	}
}

func TestOffWithoutResidency(t *testing.T) {
	w := newFakeWorld()
	e, c := newEvaluator(w, t0)
	e.Demand(1)
	e.residency(c.t)
	if len(w.orders) != 0 {
		t.Fatalf("orders = %v", w.orders)
	}
}

// A holder that gave its cards up does not take them straight back while the
// one it gave them to is still waking: that would undo the yield.
func TestAYieldedHolderLeavesTheCardsToTheWaiterWhileItWakes(t *testing.T) {
	w := newResidencyWorld()
	const online, offline = 1, 2
	w.levels[online], w.levels[offline] = pol(yieldpolicy.Idle), pol(yieldpolicy.Anytime)
	w.asleep[online] = true
	w.waiters = []Waiter{{DeploymentID: online, Policy: pol(yieldpolicy.Idle), Since: t0}}
	w.blocking[online] = Blocking{Holders: []Holder{{DeploymentID: offline, Policy: pol(yieldpolicy.Anytime), Instances: []string{"off"}}}}
	e, c := newResidencyEvaluator(w, t0)
	e.residency(c.t)
	if !slices.Equal(w.yields, []string{"2 abort=true"}) {
		t.Fatalf("yields = %v", w.yields)
	}
	// The card is free now, and online's wake is still on its way: offline
	// would fit, but must not take it back.
	w.fitsNow[offline] = true
	c.t = c.t.Add(5 * time.Second)
	e.residency(c.t)
	if slices.Contains(w.wakeCalls, offline) {
		t.Fatalf("offline took the cards back while online woke: %v", w.wakeCalls)
	}
	// Online is awake: offline may try again (and finds online holding).
	w.asleep[online] = false
	w.waiters = nil
	c.t = c.t.Add(5 * time.Second)
	e.residency(c.t)
	if !slices.Contains(w.wakeCalls, offline) {
		t.Fatalf("offline never tried to resume after online woke: %v", w.wakeCalls)
	}
}

func TestAYieldGuardDoesNotOutliveItsTime(t *testing.T) {
	w := newResidencyWorld()
	w.levels[1], w.levels[2] = pol(yieldpolicy.Idle), pol(yieldpolicy.Anytime)
	w.asleep[1], w.asleep[2] = true, true
	e, c := newResidencyEvaluator(w, t0)
	e.res.preempted[2] = t0
	e.res.yieldedFor[2] = yielded{to: 1, at: t0}
	w.fitsNow[2] = true
	c.t = t0.Add(yieldGuard)
	e.residency(c.t)
	if !slices.Contains(w.wakeCalls, 2) {
		t.Fatalf("a holder stayed asleep past the guard for a waiter that never woke: %v", w.wakeCalls)
	}
}

// A wake the arbiter ordered that had to wait for a holder to yield is
// recorded with the arbiter's reason when it lands, not as a plain waiting
// wake.
func TestAWakeThatWaitedForAYieldKeepsItsTrigger(t *testing.T) {
	w := newResidencyWorld()
	w.levels[1], w.levels[2] = pol(yieldpolicy.Idle), pol(yieldpolicy.Anytime)
	w.asleep[1] = true
	w.activity[2] = Activity{InFlight: 1}
	w.waiters = []Waiter{{DeploymentID: 1, Policy: pol(yieldpolicy.Idle), Since: t0}}
	w.blocking[1] = Blocking{Holders: []Holder{{DeploymentID: 2, Policy: pol(yieldpolicy.Anytime), Instances: []string{"off"}}}}
	e, c := newResidencyEvaluator(w, t0)
	e.residency(c.t)
	w.waits = []WaitingWake{{DeploymentID: 1, Instances: []string{"i-1"}, Since: t0}}
	w.fits[1] = []string{"i-1"}
	c.t = c.t.Add(2 * time.Second)
	e.waitingWakes(c.t)
	last := len(w.events) - 1
	if w.events[last] != "1 AUTO_WAKE" || w.params[last]["trigger"] != TriggerRequest {
		t.Fatalf("last event %s %v, want AUTO_WAKE trigger request", w.events[last], w.params[last])
	}
}

// A holder with requests of its own waiting (a batch queued against it) does
// not wake back onto the cards it just gave up before the one it gave them to
// is awake.
func TestAYieldedHolderWithItsOwnRequestsLeavesTheCardsWhileTheWaiterWakes(t *testing.T) {
	w := newResidencyWorld()
	const online, offline = 1, 2
	w.levels[online], w.levels[offline] = pol(yieldpolicy.Idle), pol(yieldpolicy.Anytime)
	w.asleep[online] = true
	w.activity[offline] = Activity{InFlight: 4}
	w.waiters = []Waiter{{DeploymentID: online, Policy: pol(yieldpolicy.Idle), Since: t0}}
	w.blocking[online] = Blocking{Holders: []Holder{{DeploymentID: offline, Policy: pol(yieldpolicy.Anytime), Instances: []string{"off"}}}}
	e, c := newResidencyEvaluator(w, t0)
	e.residency(c.t)
	if len(w.yields) != 1 {
		t.Fatalf("yields = %v", w.yields)
	}
	// Offline's batch requests are now held for it, and its card reads free
	// while online's wake is on its way.
	w.fitsNow[offline] = true
	w.waiters = append(w.waiters, Waiter{DeploymentID: offline, Policy: pol(yieldpolicy.Anytime), Since: c.t})
	c.t = c.t.Add(3 * time.Second)
	e.residency(c.t)
	if slices.Contains(w.wakeCalls, offline) {
		t.Fatalf("offline woke back for its own requests while online woke: %v", w.wakeCalls)
	}
	w.asleep[online] = false
	w.waiters = w.waiters[1:]
	c.t = c.t.Add(wakeRepeat)
	e.residency(c.t)
	if !slices.Contains(w.wakeCalls, offline) {
		t.Fatalf("offline's requests were never served after online woke: %v", w.wakeCalls)
	}
}

// The arbiter notes when a turn ends and, looking again then, gives the cards
// over even though the waiter's wake was ordered moments before: a wake that
// waits for room is no wake in flight.
func TestATurnEndingFallsDueAndIsActedOnAtOnce(t *testing.T) {
	w := newResidencyWorld()
	const a, b = 1, 2
	w.levels[a], w.levels[b] = pol(yieldpolicy.TimeSlice), pol(yieldpolicy.TimeSlice)
	w.asleep[b] = true
	w.activity[a] = Activity{InFlight: 1, LastRequest: t0}
	w.inFlight["a-1"] = 1
	w.blocking[b] = Blocking{Holders: []Holder{{DeploymentID: a, Policy: pol(yieldpolicy.TimeSlice), Instances: []string{"a-1"}}}}
	e, c := newResidencyEvaluator(w, t0)
	e.res.awakeAt[a] = t0
	w.waiters = []Waiter{{DeploymentID: b, Policy: pol(yieldpolicy.TimeSlice), Since: t0.Add(100 * time.Second)}}

	c.t = t0.Add(100 * time.Second)
	e.residency(c.t)
	if len(w.drains) != 0 {
		t.Fatalf("drained before the turn ended: %v", w.drains)
	}
	if after, ok := e.residencyDue(); !ok || after != 20*time.Second {
		t.Fatalf("next look in %v (%v), want 20 s, when a's turn ends", after, ok)
	}
	c.t = t0.Add(120 * time.Second)
	e.residency(c.t)
	if !slices.Equal(w.drains, []string{"1 a-1 true"}) {
		t.Fatalf("a did not give way when its turn ended: drains %v", w.drains)
	}
	if got := len(w.wakeCalls); got != 1 {
		t.Fatalf("the waiting wake was ordered %d times, want once", got)
	}
	// Draining: the next look comes before the 30 s drain runs out, as the
	// requests may end sooner.
	if after, ok := e.residencyDue(); !ok || after != busyPoll {
		t.Fatalf("next look in %v (%v), want the busy poll", after, ok)
	}
}

// A holder that gives way once idle, while busy, has the arbiter look again
// on the busy poll; with nothing waiting nothing falls due.
func TestABusyIdleHolderIsPolled(t *testing.T) {
	w := newResidencyWorld()
	w.levels[1], w.levels[2] = pol(yieldpolicy.Idle), pol(yieldpolicy.Anytime)
	w.asleep[2] = true
	w.activity[1] = Activity{InFlight: 1, LastRequest: t0}
	w.blocking[2] = Blocking{Holders: []Holder{{DeploymentID: 1, Policy: pol(yieldpolicy.Idle), Instances: []string{"a"}}}}
	w.waiters = []Waiter{{DeploymentID: 2, Policy: pol(yieldpolicy.Anytime), Since: t0}}
	e, c := newResidencyEvaluator(w, t0)
	e.residency(c.t)
	if after, ok := e.residencyDue(); !ok || after != busyPoll {
		t.Fatalf("next look in %v (%v), want the busy poll", after, ok)
	}
	w.waiters = nil
	e.residency(c.t)
	if _, ok := e.residencyDue(); ok {
		t.Fatal("something fell due with nothing waiting")
	}
}

// A replica waiting to be placed is served like a request held for a sleeper:
// the holder on its cards yields by the same rule, without anything to wake;
// the holder then leaves the cards alone while the newcomer is placed and
// starts, and the newcomer's idle time counts from when it serves.
func TestAReplicaWaitingToBePlacedHasTheHolderYield(t *testing.T) {
	w := newResidencyWorld()
	const batch, chat = 1, 2
	w.levels[batch], w.levels[chat] = pol(yieldpolicy.Anytime), pol(yieldpolicy.Idle)
	w.activity[batch] = Activity{InFlight: 2, LastRequest: t0}
	w.blocking[chat] = Blocking{Holders: []Holder{{DeploymentID: batch, Policy: pol(yieldpolicy.Anytime), Instances: []string{"b-1"}}}}
	w.waiters = []Waiter{{DeploymentID: chat, Policy: pol(yieldpolicy.Idle), Since: t0, Place: true}}
	w.settling[chat] = true
	e, c := newResidencyEvaluator(w, t0)
	e.residency(c.t)
	if len(w.wakeCalls) != 0 {
		t.Fatalf("a replica waiting to be placed was woken: %v", w.wakeCalls)
	}
	if !slices.Equal(w.yields, []string{"1 abort=true"}) {
		t.Fatalf("batch did not give way to the new online replica at once: %v", w.yields)
	}
	if got := w.eventsFor(batch); !slices.Equal(got, []string{"1 AUTO_SLEEP preempt/rank"}) {
		t.Fatalf("batch events = %v", got)
	}

	// The replica is placed and starts; batch wants its cards back and
	// waits while chat settles, however long past the wake guard.
	w.waiters = nil
	w.blocking[batch] = Blocking{Holders: []Holder{{DeploymentID: chat, Policy: pol(yieldpolicy.Idle), Instances: []string{"c-1"}}}}
	c.t = t0.Add(5 * time.Minute)
	e.residency(c.t)
	if len(w.yields) != 1 || len(w.wakeCalls) != 0 {
		t.Fatalf("batch took the cards back while chat started: yields %v wakes %v", w.yields, w.wakeCalls)
	}
	// Chat serves and then idles: batch only waits to resume, which puts no
	// holder to sleep - it is not woken and chat keeps its cards.
	w.settling[chat] = false
	e.residency(c.t)
	c.t = c.t.Add(10 * time.Minute)
	e.residency(c.t)
	if len(w.wakeCalls) != 0 || len(w.yields) != 1 {
		t.Fatalf("batch's resume took chat's cards: wakes %v yields %v", w.wakeCalls, w.yields)
	}
	// Chat goes away: the cards are free and batch takes them back.
	w.blocking[batch] = Blocking{}
	c.t = c.t.Add(10 * time.Second)
	e.residency(c.t)
	if !slices.Equal(w.wakeCalls, []uint{batch}) || len(w.yields) != 1 {
		t.Fatalf("batch did not resume on free cards: wakes %v yields %v", w.wakeCalls, w.yields)
	}
}

// A holder still starting is asked nothing, whatever ranks the waiter has:
// it has served nothing yet. Once it serves, its idle time counts from then.
func TestAHolderStillStartingIsNotAskedToYield(t *testing.T) {
	w := newResidencyWorld()
	const batch, nightly = 1, 2
	w.levels[batch], w.levels[nightly] = pol(yieldpolicy.Anytime), pol(yieldpolicy.TimeSlice)
	w.asleep[batch] = true
	starting := Holder{DeploymentID: nightly, Policy: pol(yieldpolicy.TimeSlice), Instances: []string{"n-1"}, Starting: true}
	w.blocking[batch] = Blocking{Holders: []Holder{starting}}
	w.waiters = []Waiter{{DeploymentID: batch, Policy: pol(yieldpolicy.Anytime), Since: t0}}
	e, c := newResidencyEvaluator(w, t0)
	c.t = t0.Add(time.Hour)
	e.residency(c.t)
	if len(w.yields) != 0 {
		t.Fatalf("a starting holder was put to sleep: %v", w.yields)
	}
	if after, ok := e.residencyDue(); !ok || after != startingPoll {
		t.Fatalf("next look in %v (%v), want the starting poll", after, ok)
	}
	// It serves now: idle for its 30 s from here, not from an hour ago.
	starting.Starting = false
	w.blocking[batch] = Blocking{Holders: []Holder{starting}}
	e.residency(c.t)
	c.t = c.t.Add(29 * time.Second)
	e.residency(c.t)
	if len(w.yields) != 0 {
		t.Fatalf("yielded before 30 s of serving idle: %v", w.yields)
	}
	c.t = c.t.Add(time.Second)
	e.residency(c.t)
	if !slices.Equal(w.yields, []string{"2 abort=false"}) {
		t.Fatalf("did not yield once idle 30 s after starting: %v", w.yields)
	}
}

// A schedule names its IANA zone, or the server's own when it names none; a
// zone the server does not know is an error, never another zone.
func TestScheduleLocation(t *testing.T) {
	if loc, err := ScheduleLocation(""); err != nil || loc != time.Local {
		t.Fatalf("no zone = %v %v, want the server's own", loc, err)
	}
	if loc, err := ScheduleLocation("Asia/Shanghai"); err != nil || loc.String() != "Asia/Shanghai" {
		t.Fatalf("named zone = %v %v", loc, err)
	}
	if loc, err := ScheduleLocation("Nowhere/Never"); err == nil {
		t.Fatalf("unknown zone = %v, want an error", loc)
	}
}

// A holder whose deployment scales up gives way by its running replica's own
// state: another of its replicas starting or waiting to be placed does not
// keep the running one from yielding (or two deployments could each wait for
// the other).
func TestAHolderScalingUpStillYieldsItsRunningReplica(t *testing.T) {
	w := newResidencyWorld()
	const batch, chat = 1, 2
	w.levels[batch], w.levels[chat] = pol(yieldpolicy.Anytime), pol(yieldpolicy.Idle)
	w.activity[batch] = Activity{InFlight: 1, LastRequest: t0}
	// batch's second replica is starting, or waits for chat's cards.
	w.settling[batch] = true
	w.blocking[chat] = Blocking{Holders: []Holder{{DeploymentID: batch, Policy: pol(yieldpolicy.Anytime), Instances: []string{"b-1"}}}}
	w.waiters = []Waiter{{DeploymentID: chat, Policy: pol(yieldpolicy.Idle), Since: t0, Place: true}}
	e, c := newResidencyEvaluator(w, t0)
	e.residency(c.t)
	if !slices.Equal(w.yields, []string{"1 abort=true"}) {
		t.Fatalf("batch's running replica did not give way while another replica of it started: %v", w.yields)
	}
}

// A holder put to sleep for one waiter is not put to sleep again for
// another while that sleep lands: the cards are the first one's.
func TestAHolderYieldsOnceForTheFirstWaiter(t *testing.T) {
	w := newResidencyWorld()
	const batch, chat, nightly = 1, 2, 3
	w.levels[batch], w.levels[chat], w.levels[nightly] = pol(yieldpolicy.Anytime), pol(yieldpolicy.Idle), pol(yieldpolicy.TimeSlice)
	w.asleep[batch] = true
	w.activity[chat] = Activity{LastRequest: t0}
	holder := Blocking{Holders: []Holder{{DeploymentID: chat, Policy: pol(yieldpolicy.Idle), Instances: []string{"c-1"}}}}
	w.blocking[batch], w.blocking[nightly] = holder, holder
	e, c := newResidencyEvaluator(w, t0)
	e.res.preempted[batch] = t0
	for i := 1; i <= 13; i++ {
		c.t = t0.Add(time.Duration(i*5) * time.Second)
		if i == 4 {
			w.waiters = []Waiter{{DeploymentID: nightly, Policy: pol(yieldpolicy.TimeSlice), Since: c.t, Place: true}}
		}
		e.residency(c.t)
	}
	if !slices.Equal(w.yields, []string{"2 abort=false"}) || w.params[len(w.params)-1]["for_deployment"] != uint(nightly) {
		t.Fatalf("yields %v params %v, want chat to yield once, for the replica waiting to be placed", w.yields, w.params)
	}
}

// Once a holder is put to sleep for a waiter, the arbiter looks again on the
// yield poll until the waiter is awake, rather than waiting for a nudge.
func TestAYieldInFlightIsPolledUntilTheWaiterWakes(t *testing.T) {
	w := newResidencyWorld()
	const online, batch = 1, 2
	w.levels[online], w.levels[batch] = pol(yieldpolicy.Idle), pol(yieldpolicy.Anytime)
	w.asleep[online] = true
	w.activity[batch] = Activity{InFlight: 1, LastRequest: t0}
	w.blocking[online] = Blocking{Holders: []Holder{{DeploymentID: batch, Policy: pol(yieldpolicy.Anytime), Instances: []string{"b-1"}}}}
	w.waiters = []Waiter{{DeploymentID: online, Policy: pol(yieldpolicy.Idle), Since: t0}}
	e, c := newResidencyEvaluator(w, t0)
	e.residency(c.t)
	if !slices.Equal(w.yields, []string{"2 abort=true"}) {
		t.Fatalf("yields %v", w.yields)
	}
	if after, ok := e.residencyDue(); !ok || after != yieldPoll {
		t.Fatalf("next look in %v (%v), want the yield poll", after, ok)
	}
	// The waiter is awake: no more polling on its account.
	w.asleep[online] = false
	w.waiters = nil
	e.residency(c.t.Add(time.Second))
	if after, ok := e.residencyDue(); ok && after <= yieldPoll {
		t.Fatalf("still polling every %v after the waiter woke", after)
	}
}

// A yield-poll look is at the waiting wake the yield made room for alone:
// another deployment's waiting wake is left to the nudges and the tick, so
// a fleet with many waiting wakes is not walked four times a second. Once
// the waiter wakes the poll stops; a look due for anything else is whole.
func TestAYieldPollLooksOnlyAtTheWaiterItIsFor(t *testing.T) {
	w := newResidencyWorld()
	const online, batch, other = 1, 2, 5
	w.levels[online], w.levels[batch] = pol(yieldpolicy.Idle), pol(yieldpolicy.Anytime)
	w.asleep[online] = true
	w.activity[batch] = Activity{InFlight: 1, LastRequest: t0}
	w.blocking[online] = Blocking{Holders: []Holder{{DeploymentID: batch, Policy: pol(yieldpolicy.Anytime), Instances: []string{"b-1"}}}}
	w.waiters = []Waiter{{DeploymentID: online, Policy: pol(yieldpolicy.Idle), Since: t0}}
	w.waits = []WaitingWake{
		{DeploymentID: other, Instances: []string{"o-1"}, Since: t0},
		{DeploymentID: online, Instances: []string{"i-1"}, Since: t0},
	}
	e, c := newResidencyEvaluator(w, t0)
	e.residency(c.t)
	if !slices.Equal(w.yields, []string{"2 abort=true"}) {
		t.Fatalf("yields %v", w.yields)
	}

	// The sleep has not landed: the poll looks at the waiter's wake, only.
	w.orders = nil
	c.t = c.t.Add(yieldPoll)
	e.due(c.t)
	if !slices.Equal(w.orders, []string{"wake-waiting 1 [i-1]"}) {
		t.Fatalf("orders on a yield poll = %v, want the waiter's waiting wake alone", w.orders)
	}
	if after, ok := e.residencyDue(); !ok || after != yieldPoll {
		t.Fatalf("next look in %v (%v), want the yield poll again", after, ok)
	}

	// It lands and the waiter wakes: the poll stops.
	w.orders = nil
	w.fits[online] = []string{"i-1"}
	w.asleep[online] = false
	c.t = c.t.Add(yieldPoll)
	e.due(c.t)
	if !slices.Equal(w.orders, []string{"wake-waiting 1 [i-1]"}) {
		t.Fatalf("orders = %v", w.orders)
	}
	if got := w.eventsFor(online); len(got) == 0 || !strings.HasPrefix(got[len(got)-1], "1 AUTO_WAKE") {
		t.Fatalf("waiter events %v, want its wake recorded", got)
	}
	if after, ok := e.residencyDue(); ok && after <= yieldPoll {
		t.Fatalf("still polling every %v after the waiter woke", after)
	}

	// A look due for something else is whole: every waiting wake.
	w.orders = nil
	e.res.next = c.t
	e.due(c.t)
	if !slices.Contains(w.orders, "wake-waiting 5 [o-1]") {
		t.Fatalf("orders on a full look = %v, want every waiting wake", w.orders)
	}
}
