package automation

import (
	"sort"
	"time"

	"gpuworks.ai/gpuworks/internal/server/yieldpolicy"
)

// Triggers of the residency arbiter's orders.
const (
	// TriggerRequest: a request through the gateway waits for the
	// deployment.
	TriggerRequest = "request"
	// TriggerResume: the deployment was put to sleep for another and takes
	// its cards back once no one holds them.
	TriggerResume = "resume"
	// TriggerPreempt: the deployment gave its cards to one waiting for them.
	TriggerPreempt = "preempt"
)

// DefaultDemandSettle gathers the demand of a burst of requests for sleeping
// deployments into one look.
const DefaultDemandSettle = 100 * time.Millisecond

// residencyState is the arbiter's memory. It is not persisted; what a server
// that has just started can tell again from the event log it reads back
// (residency_restart.go): the resume list, and the start of a time-sliced
// holder's turn.
type residencyState struct {
	source  ResidencySource
	actions ResidencyActions
	demand  chan struct{}
	// awakeAt is when the arbiter woke each deployment, the start of its
	// turn as the arbiter saw it begin; awakeSeen, as the instance events
	// say it began, for a turn the arbiter did not see.
	awakeAt   map[uint]time.Time
	awakeSeen map[uint]seenAwake
	// preempted are deployments put to sleep for another, since when: they
	// take their cards back once they may.
	preempted map[uint]time.Time
	// draining are holders whose requests run on before they sleep.
	draining map[uint]drain
	// wokeAt is when a wake was last ordered for a waiter, so a waiter whose
	// wake is in flight is not ordered again on every pass.
	wokeAt map[uint]time.Time
	// wakeTrigger is why the arbiter ordered a wake that waits for room:
	// the waiting-wakes pass records it when the wake lands.
	wakeTrigger map[uint]string
	// yieldedFor is whom each holder gave its cards to, and when: it does
	// not take them back while that one is still waking, or it would take the
	// room the yield made before the waiter could use it.
	yieldedFor map[uint]yielded
	// next is when the last look found the next yield due, zero when none
	// is (Evaluator.residencyDue).
	next time.Time
	// poll is when to look next whether a yield has landed, zero while
	// none is landing (yieldPoll): that look is at the waiting wakes the
	// yields made room for alone (Evaluator.yieldLanding).
	poll time.Time
	// starting are holders last seen still starting: once one serves, its
	// turn and idle time count from then.
	starting map[uint]bool
	// restored says the resume list was read back from the event log, which
	// the first look does (restorePreempted); a failed read is tried again
	// on the next.
	restored bool
}

type yielded struct {
	to uint
	at time.Time
	// restored says the yield was read back from the event log: its end
	// leaves the turn of the one it was for to that one's own events.
	restored bool
}

// yieldGuard is how long a holder leaves the cards it gave up to the one it
// gave them to, at most, while that one wakes; startGuard, while that one
// is placed and starts, which loading a model can take minutes of.
const (
	yieldGuard = 2 * time.Minute
	startGuard = 10 * time.Minute
)

type drain struct {
	since time.Time
	// limit is how long requests may run on.
	limit     time.Duration
	instances []string
	params    map[string]any
}

// yieldPoll is how often the arbiter looks while a yield it ordered lands,
// so the waiter wakes as soon as the room is there. Such a look is at the
// waiter's waiting wake alone: nothing else changes because a sleep landed,
// and the instance changes it makes nudge the evaluator for the rest.
const yieldPoll = 250 * time.Millisecond

// busyPoll is how often the arbiter looks again at a holder that yields
// once idle but is busy: no clock says when its requests end.
const busyPoll = 2 * time.Second

// startingPoll is how often the arbiter looks again at a holder still
// starting: nothing tells it when the engine is ready.
const startingPoll = 2 * time.Second

// wakeRepeat is how long after ordering a waiter's wake the arbiter leaves it
// alone.
const wakeRepeat = 30 * time.Second

// WithResidency turns the residency arbiter on.
func (e *Evaluator) WithResidency(source ResidencySource, actions ResidencyActions) *Evaluator {
	e.res = &residencyState{
		source: source, actions: actions, demand: make(chan struct{}, 1),
		awakeAt: map[uint]time.Time{}, preempted: map[uint]time.Time{}, draining: map[uint]drain{}, wokeAt: map[uint]time.Time{},
		yieldedFor: map[uint]yielded{}, wakeTrigger: map[uint]string{}, starting: map[uint]bool{}, awakeSeen: map[uint]seenAwake{},
	}
	return e
}

// Demand says a deployment has a request held for its wake: the arbiter
// looks soon.
func (e *Evaluator) Demand(uint) {
	if e.res == nil {
		return
	}
	select {
	case e.res.demand <- struct{}{}:
	default:
	}
}

func (e *Evaluator) demandChan() <-chan struct{} {
	if e.res == nil {
		return nil
	}
	return e.res.demand
}

// residency wakes the deployments requests wait for, and makes room for
// them by putting holders to sleep when their yield policies say they must
// give way (yieldpolicy.Yields). It notes when the next yield falls due.
func (e *Evaluator) residency(now time.Time) {
	r := e.res
	if r == nil {
		return
	}
	r.next = time.Time{}
	e.restorePreempted()
	e.finishDrains(now)
	waiters, err := r.source.Waiters()
	if err != nil {
		e.log.Warn("automation: read waiting requests", "err", err)
		return
	}
	var queue []waiting
	listed := map[uint]bool{}
	for _, w := range waiters {
		listed[w.DeploymentID] = true
		queue = append(queue, waiting{id: w.DeploymentID, policy: w.Policy, since: w.Since, place: w.Place})
	}
	for id, since := range r.preempted {
		if listed[id] {
			continue
		}
		policy, ok := r.source.Wakeable(id)
		if !ok {
			delete(r.preempted, id)
			continue
		}
		if e.stillWakingFor(now, id) {
			continue
		}
		queue = append(queue, waiting{id: id, policy: policy, since: since, resume: true})
	}
	sort.SliceStable(queue, func(a, b int) bool { return queueBefore(queue[a], queue[b]) })
	if len(queue) > 0 {
		e.log.Debug("automation: residency look", "queue", queueText(queue))
	}
	yielding := map[uint]bool{}
	for id := range r.draining {
		yielding[id] = true
	}
	for _, w := range queue {
		ordered := false
		if at, ok := r.wokeAt[w.id]; ok && now.Sub(at) < wakeRepeat {
			// Its wake is in flight; one that waits for room still has its
			// blockers looked at, as their yields fall due.
			if _, waits := r.wakeTrigger[w.id]; !waits {
				continue
			}
			ordered = true
		}
		// A holder that gave its cards to one still waking leaves them be,
		// whether it waits to resume or has requests of its own waiting.
		if !w.resume && e.stillWakingFor(now, w.id) {
			continue
		}
		// A resume answers no one: it takes its cards back only once no one
		// holds them, and puts no holder to sleep, busy or idle.
		if w.resume && e.cardsHeld(w.id) {
			continue
		}
		e.serveWaiter(now, w, ordered, yielding)
	}
	for id, at := range r.wokeAt {
		if now.Sub(at) >= wakeRepeat {
			delete(r.wokeAt, id)
		}
	}
	// While a yield lands, look often: the waiter's wake waits on it, and a
	// nudge would add its settle on top of the sleep.
	e.pollLanding(now, e.landing(now))
}

// landing are the deployments yields were ordered for within yieldGuard: the
// ones whose wake waits for those yields to land.
func (e *Evaluator) landing(now time.Time) map[uint]bool {
	out := map[uint]bool{}
	for _, y := range e.res.yieldedFor {
		if now.Sub(y.at) < yieldGuard {
			out[y.to] = true
		}
	}
	return out
}

// pollLanding sets the yield poll while one of the deployments the yields
// were for is still asleep, and clears it once none is.
func (e *Evaluator) pollLanding(now time.Time, landing map[uint]bool) {
	r := e.res
	r.poll = time.Time{}
	for id := range landing {
		if _, asleep := r.source.Wakeable(id); asleep {
			r.poll = now.Add(yieldPoll)
			return
		}
	}
}

// due acts on the evaluator's next look falling due. A look due for the
// yield poll alone wakes the waiting wakes the yields were for once they fit
// (yieldLanding); any other is a full look - the waiting wakes, then the
// arbiter.
func (e *Evaluator) due(now time.Time) {
	if r := e.res; r != nil && !r.poll.IsZero() && !now.Before(r.poll) && (r.next.IsZero() || now.Before(r.next)) {
		e.yieldLanding(now)
		return
	}
	// A yield may have landed: the waiting wake it made room for goes
	// first, as on a nudge, without the nudge's settle.
	e.waitingWakes(now)
	e.residency(now)
}

// yieldLanding looks at the waiting wakes of the deployments yields were
// ordered for, and only those: on a fleet with many waiting wakes, a look at
// every one on each yield poll would take the lifecycle lock for each,
// four times a second.
func (e *Evaluator) yieldLanding(now time.Time) {
	landing := e.landing(now)
	if len(landing) > 0 {
		e.waitingWakesOf(now, landing)
	}
	e.pollLanding(now, landing)
}

// serveWaiter wakes a waiter, or, when its wake waits for room, puts to
// sleep what blocks it once that must give way. ordered says its wake was
// ordered already and waits for room: only the blockers are looked at, as
// for a replica waiting to be placed.
func (e *Evaluator) serveWaiter(now time.Time, w waiting, ordered bool, yielding map[uint]bool) {
	if !ordered && !w.place && !e.orderWake(now, w) {
		return
	}
	e.makeRoom(now, w, yielding)
}

// orderWake wakes the waiter's sleepers that fit; it says whether some
// wait for room.
func (e *Evaluator) orderWake(now time.Time, w waiting) bool {
	r := e.res
	trigger := TriggerRequest
	if w.resume {
		trigger = TriggerResume
	}
	result, err := e.actions.Wake(w.id, false)
	if err != nil {
		e.log.Warn("automation: wake a deployment a request waits for", "deployment", w.id, "err", err)
		return false
	}
	r.wokeAt[w.id] = now
	if len(result.Woken) > 0 {
		e.actions.Record(w.id, EventAutoWake, map[string]any{"trigger": trigger, "instances": result.Woken})
		r.awakeAt[w.id] = now
		delete(r.preempted, w.id)
		return false
	}
	if len(result.Waiting) == 0 {
		return false
	}
	r.wakeTrigger[w.id] = trigger
	return true
}

// makeRoom puts to sleep the holders on the cards the waiter needs, all of
// them or none, once each must give way; else it notes when one falls due.
func (e *Evaluator) makeRoom(now time.Time, w waiting, yielding map[uint]bool) {
	r := e.res
	blocking, err := r.source.Blocking(w.id)
	if err != nil {
		e.log.Warn("automation: read what keeps a deployment asleep", "deployment", w.id, "err", err)
		return
	}
	if blocking.Blocked || len(blocking.Holders) == 0 {
		return
	}
	waiter := yieldpolicy.Waiter{Policy: w.policy}
	reasons := make([]yieldpolicy.Reason, len(blocking.Holders))
	for i, h := range blocking.Holders {
		if yielding[h.DeploymentID] || h.DeploymentID == w.id {
			return
		}
		// Put to sleep for a waiter already: the sleep has not landed, and
		// the cards it frees are that waiter's.
		if y, ok := r.yieldedFor[h.DeploymentID]; ok && now.Sub(y.at) < yieldGuard {
			return
		}
		// A holder still starting has served nothing and is asked nothing:
		// look again once it may have started.
		if h.Starting {
			r.starting[h.DeploymentID] = true
			r.dueAt(now.Add(startingPoll))
			return
		}
		state := e.holderState(now, h)
		reason, ok := yieldpolicy.Yields(state, waiter)
		if !ok {
			e.noteDue(now, state, waiter)
			return
		}
		reasons[i] = reason
	}
	// Every holder gives way, or none does: putting some to sleep and not
	// the others would take service away without making room.
	for i, h := range blocking.Holders {
		yielding[h.DeploymentID] = true
		e.yield(now, h, reasons[i], w)
	}
	// The waiter's wake waits for room; the waiting-wakes pass finishes it
	// once the holders sleep.
}

// holderState is a holder as the yield rule reads it.
func (e *Evaluator) holderState(now time.Time, h Holder) yieldpolicy.Holder {
	r := e.res
	if r.starting[h.DeploymentID] {
		delete(r.starting, h.DeploymentID)
		r.awakeAt[h.DeploymentID] = now
	}
	// Idle time counts from no earlier than this process began watching: a
	// request cut off by a restart left no record of having run, so the time
	// before it cannot be told idle.
	since := e.started
	if at, ok := r.awakeAt[h.DeploymentID]; ok && at.After(since) {
		since = at
	}
	out := yieldpolicy.Holder{Policy: h.Policy, AwakeFor: now.Sub(e.turnStart(now, h, since))}
	act := r.source.Activity(h.DeploymentID)
	out.Busy = act.InFlight > 0 || act.Held > 0
	if !out.Busy {
		last := since
		if act.LastRequest.After(last) {
			last = act.LastRequest
		}
		out.IdleFor = now.Sub(last)
	}
	return out
}

// noteDue moves the next look up to when holder must give way to waiter, or
// to the next busy poll when it gives way once idle and is busy now.
func (e *Evaluator) noteDue(now time.Time, holder yieldpolicy.Holder, waiter yieldpolicy.Waiter) {
	after, ok := yieldpolicy.Due(holder, waiter)
	if !ok {
		if !holder.Busy || holder.Policy.IdleRelease() == 0 {
			return
		}
		after = busyPoll
	}
	e.res.dueAt(now.Add(after))
}

// dueAt moves the next look up to at.
func (r *residencyState) dueAt(at time.Time) {
	if r.next.IsZero() || at.Before(r.next) {
		r.next = at
	}
}

// residencyDue is how long until the arbiter must look again, ok false when
// nothing falls due (the tick still looks).
func (e *Evaluator) residencyDue() (time.Duration, bool) {
	if e.res == nil {
		return 0, false
	}
	next := e.res.next
	if poll := e.res.poll; !poll.IsZero() && (next.IsZero() || poll.Before(next)) {
		next = poll
	}
	if next.IsZero() {
		return 0, false
	}
	after := next.Sub(e.now())
	if after < 0 {
		after = 0
	}
	return after, true
}

// yield puts a holder to sleep for a waiter. A holder idle for the reason has
// nothing running and sleeps at once. Otherwise its policy says what becomes
// of the requests still running on the replica (Policy.Drain): cut off at
// once, or new ones go elsewhere (or are held) and those running get the
// time its policy gives them.
func (e *Evaluator) yield(now time.Time, h Holder, reason yieldpolicy.Reason, w waiting) {
	r := e.res
	params := map[string]any{"trigger": TriggerPreempt, "reason": string(reason), "for_deployment": w.id, "instances": h.Instances}
	abort, limit := h.Policy.Drain()
	if reason == yieldpolicy.ReasonIdle {
		e.sleepHolder(h.DeploymentID, h.Instances, false, params)
		return
	}
	if !abort && r.source.InstancesInFlight(h.Instances) > 0 {
		r.actions.Drain(h.DeploymentID, h.Instances, true)
		r.draining[h.DeploymentID] = drain{since: now, limit: limit, instances: h.Instances, params: params}
		r.dueAt(now.Add(min(limit, busyPoll)))
		return
	}
	e.sleepHolder(h.DeploymentID, h.Instances, abort, params)
}

func (e *Evaluator) sleepHolder(id uint, instances []string, abort bool, params map[string]any) {
	r := e.res
	params["abort"] = abort
	level, err := r.actions.Yield(id, instances, abort)
	if err != nil {
		failed := copyParams(params)
		failed["action"] = "sleep"
		failed["reason"] = err.Error()
		if code, ok := codeOf(err); ok {
			failed["reason_code"] = code
		}
		e.actions.Record(id, EventScheduleActionFailed, failed)
		return
	}
	params["level"] = level
	e.actions.Record(id, EventAutoSleep, params)
	delete(r.awakeAt, id)
	delete(r.awakeSeen, id)
	if to, ok := params["for_deployment"].(uint); ok {
		r.yieldedFor[id] = yielded{to: to, at: e.now()}
	}
	if policy, ok := r.source.Wakeable(id); ok && policy.Preemptible() {
		r.preempted[id] = e.now()
	}
}

// finishDrains puts each draining holder to sleep once its requests are done
// or its drain time is up, cutting off what still runs.
func (e *Evaluator) finishDrains(now time.Time) {
	r := e.res
	for id, d := range r.draining {
		inFlight := r.source.InstancesInFlight(d.instances)
		if inFlight > 0 && now.Sub(d.since) < d.limit {
			// Its requests may end sooner than the limit: look again soon.
			r.dueAt(d.since.Add(d.limit))
			r.dueAt(now.Add(busyPoll))
			continue
		}
		delete(r.draining, id)
		e.sleepHolder(id, d.instances, inFlight > 0, d.params)
		r.actions.Drain(id, d.instances, false)
	}
}

// stillWakingFor says the deployment gave its cards to one that has not woken
// yet: it leaves them alone until that one is awake, or for yieldGuard.
func (e *Evaluator) stillWakingFor(now time.Time, id uint) bool {
	r := e.res
	y, ok := r.yieldedFor[id]
	if !ok {
		return false
	}
	if now.Sub(y.at) < yieldGuard {
		if _, asleep := r.source.Wakeable(y.to); asleep {
			return true
		}
	}
	if now.Sub(y.at) < startGuard && r.source.Settling(y.to) {
		return true
	}
	// The one it gave way to serves now: its turn, and its idle time,
	// count from here. A yield read back from the event log ended while the
	// server was down, or is first seen ending long after it did: the one it
	// was for is told by its own events (turnStart).
	if at, ok := r.awakeAt[y.to]; !y.restored && (!ok || at.Before(y.at)) {
		r.awakeAt[y.to] = now
	}
	delete(r.yieldedFor, id)
	return false
}

// cardsHeld says something awake, or something nothing can put to sleep, holds
// the cards the deployment's sleepers need. A read that fails counts as held:
// a resume waits for the next look rather than ordering a wake on a guess.
func (e *Evaluator) cardsHeld(id uint) bool {
	blocking, err := e.res.source.Blocking(id)
	if err != nil {
		e.log.Warn("automation: read what keeps a deployment asleep", "deployment", id, "err", err)
		return true
	}
	return blocking.Blocked || len(blocking.Holders) > 0
}
