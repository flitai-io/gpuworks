//go:build linux && dqlite

package app

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"gpuworks.ai/gpuworks/internal/server/store"
)

const (
	// A proof may spend one membership grant timeout and still leave that
	// same timeout of freshness. The deadline and the proof context both
	// start before the first read. A proof that exceeds the window is
	// discarded instead of stretching the age of bytes it already read; it
	// publishes nothing, and by itself it does not close the member.
	materialEligibilityWindow = 10*time.Second + store.ClusterMembershipGrantTimeout
	// materialReproofInterval paces a proof that did not succeed, and bounds
	// how long warm verification and readiness wait behind a renewal.
	materialReproofInterval = 2 * time.Second
	// materialIdleReproofRest is how long a member rests after a successful
	// proof before it starts the next one. Each proof costs replicated grant
	// commits on this member and on the peers it asks, so the rest sets the
	// idle write rate of the whole cluster. successRest never lets it reach
	// into the last two grant timeouts of the proof just published, so the
	// renewal starts with at least 2*ClusterMembershipGrantTimeout left: one
	// proof as slow as a whole grant timeout, or one failed attempt retried
	// after materialReproofInterval and followed by a proof of up to three
	// seconds, still leaves the grant timeout of lifetime that readiness
	// requires. Two failed attempts in a row may drop below it. A value above
	// materialEligibilityWindow - 2*ClusterMembershipGrantTimeout changes
	// nothing; four seconds keeps one more second while proofs are fast.
	materialIdleReproofRest = 4 * time.Second
	// materialUnconfirmedLimit bounds how long a member keeps admitting
	// requests on renewals that got no answer. A renewal that only ran out of
	// time is not a refusal and extends admission, which is what carries a
	// member through a storage stall; without a bound a member whose every
	// renewal times out would answer forever from a view nothing confirms:
	// no request takes a grant of its own. Past the bound the member closes
	// until a renewal succeeds.
	materialUnconfirmedLimit = 4 * materialEligibilityWindow
)

// materialProofPhase spreads member starts within the existing cadence. Mix
// the full ID so nearby IDs and IDs with the same parity do not share a bucket.
// Collisions affect scheduling only; every member still needs its own proof.
func materialProofPhase(id uint64) time.Duration {
	if id == 0 {
		return 0
	}
	id = (id ^ (id >> 30)) * 0xbf58476d1ce4e5b9
	id = (id ^ (id >> 27)) * 0x94d049bb133111eb
	id ^= id >> 31
	return time.Duration(id % uint64(materialReproofInterval))
}

// materialEligibility holds a short-lived, in-memory result of the full
// candidate material proof. The deadline starts before the proof reads its
// first byte, so a slow proof cannot extend the age of its earliest read.
// An ordinary request reads Eligible only; it never scans material files.
type materialEligibility struct {
	mu        sync.Mutex
	proofSlot chan struct{}
	now       func() time.Time
	// prove runs one proof. A refusal of a member (store.CauseMemberRefused)
	// in its error closes admission at once, so it must return only the
	// refusal of the proving member itself; thisMembersVerdict is the exit
	// that guarantees it.
	prove    func(context.Context) (bool, error)
	interval time.Duration
	idleRest time.Duration
	deadline time.Time
	bridging bool
	provedAt time.Time
	// answered is set when an answer other than success, an invalidation or
	// Stop was seen since the last successful proof. From then on an attempt
	// that brings no answer extends nothing; only a successful proof clears it.
	answered bool
	// refusal is the refusal code of the database's refusal of this member
	// that a proof ended on, cleared by the next successful proof. While it
	// stands the closed member answers with it, not "no proof yet".
	refusal       string
	expiry        *time.Timer
	epoch         uint64
	cancel        context.CancelFunc
	proofCtx      context.Context
	proofParent   context.Context
	proofEpoch    uint64
	runCtx        context.Context
	running       bool
	stopped       bool
	onInvalidated func(admissionCloseCause)
	// onOutcome is told, after a proof was decided and any invalidation it
	// caused was delivered, whether it was accepted as a fresh confirmation.
	onOutcome func(proved bool)
	proving   atomic.Bool
	phase     time.Duration
}

func newMaterialEligibility(now func() time.Time, prove func(context.Context) (bool, error), interval time.Duration) *materialEligibility {
	if now == nil {
		now = time.Now
	}
	if interval <= 0 {
		interval = materialReproofInterval
	}
	slot := make(chan struct{}, 1)
	slot <- struct{}{}
	return &materialEligibility{proofSlot: slot, now: now, prove: prove, interval: interval, idleRest: interval}
}

// setIdleRest replaces the rest after a successful proof. It is a no-op after
// Run starts. The retry cadence after a failed proof stays the interval.
func (e *materialEligibility) setIdleRest(rest time.Duration) {
	if e == nil || rest <= 0 {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.running || e.stopped {
		return
	}
	e.idleRest = rest
}

func (e *materialEligibility) setOnInvalidated(fn func(admissionCloseCause)) {
	e.mu.Lock()
	e.onInvalidated = fn
	e.mu.Unlock()
}

func (e *materialEligibility) setOnOutcome(fn func(proved bool)) {
	e.mu.Lock()
	e.onOutcome = fn
	e.mu.Unlock()
}

// setProofPhase delays only the first proof. It is a no-op after Run starts,
// so the default of zero still proves immediately.
func (e *materialEligibility) setProofPhase(phase time.Duration) {
	if e == nil || phase <= 0 {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.running || e.stopped {
		return
	}
	e.phase = phase
}

func (e *materialEligibility) provingNow() bool {
	return e != nil && e.proving.Load()
}

// Eligible is the request-path check. Expiry is latched so a wall-clock
// correction cannot resurrect a result that has already expired.
func (e *materialEligibility) Eligible() bool {
	return e.EligibleFor(0)
}

// refusedAs is the refusal code of the database's refusal a proof last ended
// on, or empty when the last proof was not refused.
func (e *materialEligibility) refusedAs() string {
	if e == nil {
		return ""
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.refusal
}

// EligibleFor additionally requires enough remaining proof lifetime for the
// caller's next bounded operation. Insufficient headroom does not expire the
// proof or change ordinary business admission.
func (e *materialEligibility) EligibleFor(minRemaining time.Duration) bool {
	ready, _, _ := e.eligibleForResult(minRemaining)
	return ready
}

// Diagnostics use the same decision and clock sample, without a second probe.
func (e *materialEligibility) eligibleForResult(minRemaining time.Duration) (bool, string, time.Duration) {
	if e == nil {
		return false, "proof_missing", 0
	}
	e.mu.Lock()
	if e.stopped || e.deadline.IsZero() || (e.runCtx != nil && e.runCtx.Err() != nil) {
		reason := "proof_missing"
		if e.stopped {
			reason = "stopped"
		} else if !e.deadline.IsZero() {
			reason = "run_canceled"
		} else if e.refusal != "" {
			reason = e.refusal
		}
		e.mu.Unlock()
		return false, reason, 0
	}
	if e.proofCtx != nil && e.outlivedWindow(e.proofParent, e.proofCtx, e.proofEpoch) && e.confirmedRecently(e.now()) {
		// The proof in progress only ran out of its own window; see Reprove.
		// This check may run before the proof's own cancellation callback.
		e.admitUntil(e.unconfirmedDeadline(e.now()))
		e.proofCtx = nil
	}
	if e.proofCtx != nil && e.proofCtx.Err() != nil {
		e.closeAdmission()
		onInvalidated := e.onInvalidated
		e.mu.Unlock()
		if onInvalidated != nil {
			onInvalidated(closeProofCanceled)
		}
		return false, "proof_canceled", 0
	}
	now := e.now()
	remaining := e.deadline.Sub(now)
	if !e.deadline.IsZero() && !now.Before(e.deadline) {
		// A reproof that has not finished is not a negative answer. A successor
		// proof that started while the last result was still valid, or one that
		// is already proving, keeps ordinary admission until that attempt's own
		// context ends. Proofs are serialized, so a slow renewal would otherwise
		// close every member together. A grant that needs headroom still waits
		// for a published proof.
		if (e.bridging || e.proving.Load()) && e.proofCtx != nil && e.proofCtx.Err() == nil && e.confirmedRecently(now) {
			e.mu.Unlock()
			if minRemaining <= 0 {
				return true, "", 0
			}
			return false, "headroom", 0
		}
		e.admitUntil(time.Time{})
		e.bridging = false
		onInvalidated := e.onInvalidated
		e.mu.Unlock()
		if onInvalidated != nil {
			onInvalidated(closeProofExpired)
		}
		return false, "expired", remaining
	}
	ready := minRemaining <= 0 || !now.Add(minRemaining).After(e.deadline)
	e.mu.Unlock()
	if !ready {
		return false, "headroom", remaining
	}
	return true, "", remaining
}

// waitForActiveProof yields warm verification to one captured renewal, for at
// most one existing reproof interval or the current result's remaining lifetime.
// This bounds scheduling only; the caller still obtains every fresh grant.
func (e *materialEligibility) waitForActiveProof(ctx context.Context, maxWait time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if e == nil {
		return nil
	}
	e.mu.Lock()
	proofCtx := e.proofCtx
	now := e.now()
	waitFor := e.interval
	if remaining := e.deadline.Sub(now); remaining < waitFor {
		waitFor = remaining
	}
	if maxWait > 0 && maxWait < waitFor {
		waitFor = maxWait
	}
	wait := !e.stopped && !e.deadline.IsZero() &&
		(e.runCtx == nil || e.runCtx.Err() == nil) &&
		proofCtx != nil && proofCtx.Err() == nil &&
		waitFor > 0 && now.Before(e.deadline)
	e.mu.Unlock()
	if wait {
		timer := time.NewTimer(waitFor)
		defer timer.Stop()
		select {
		case <-ctx.Done():
		case <-proofCtx.Done():
		case <-timer.C:
		case <-e.proofSlot:
			e.proofSlot <- struct{}{}
		}
	}
	return ctx.Err()
}

// Invalidate closes eligibility immediately and cancels a proof in progress.
// A late success from that proof cannot reopen the gate. cause is why, as
// the caller knows it.
func (e *materialEligibility) Invalidate(cause admissionCloseCause) {
	if e == nil {
		return
	}
	e.mu.Lock()
	e.epoch++
	e.closeAdmission()
	cancel := e.cancel
	onInvalidated := e.onInvalidated
	e.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if onInvalidated != nil {
		onInvalidated(cause)
	}
}

// Reprove performs one full proof outside the request path. A proof may renew
// eligibility only if it finished within the same invalidation epoch and
// within the eligibility window measured from its start.
func (e *materialEligibility) Reprove(ctx context.Context) (bool, error) {
	if e == nil || e.prove == nil {
		return false, nil
	}
	if err := ctx.Err(); err != nil {
		e.Invalidate(closeProofRunCanceled)
		return false, err
	}
	select {
	case <-ctx.Done():
		e.Invalidate(closeProofRunCanceled)
		return false, ctx.Err()
	case <-e.proofSlot:
	}
	defer func() { e.proofSlot <- struct{}{} }()
	if err := ctx.Err(); err != nil {
		e.Invalidate(closeProofRunCanceled)
		return false, err
	}
	e.mu.Lock()
	if e.stopped || (e.runCtx != nil && e.runCtx.Err() != nil) {
		e.mu.Unlock()
		return false, nil
	}
	proofCtx, cancel := context.WithTimeout(ctx, materialEligibilityWindow)
	e.cancel = cancel
	e.proofCtx = proofCtx
	epoch := e.epoch
	e.proofParent, e.proofEpoch = ctx, epoch
	started := e.now()
	e.bridging = !e.deadline.IsZero() && started.Before(e.deadline)
	e.mu.Unlock()
	callbackDone := make(chan struct{})
	stopOnCancel := context.AfterFunc(proofCtx, func() {
		defer close(callbackDone)
		e.mu.Lock()
		if e.proofCtx != proofCtx || e.epoch != epoch {
			e.mu.Unlock()
			return
		}
		if e.outlivedWindow(ctx, proofCtx, epoch) && e.confirmedRecently(e.now()) {
			// The proof only ran out of its own window. That is no answer
			// about this member, so admission continues on the last
			// confirmation, up to the unconfirmed limit.
			e.admitUntil(e.unconfirmedDeadline(e.now()))
			e.proofCtx = nil
			e.mu.Unlock()
			return
		}
		e.epoch++
		e.closeAdmission()
		onInvalidated := e.onInvalidated
		e.mu.Unlock()
		if onInvalidated != nil {
			onInvalidated(closeProofContextEnd)
		}
	})

	// Hold this across completion so a transport refresh does not take the
	// leader writer while this proof is still publishing its result.
	e.proving.Store(true)
	defer e.proving.Store(false)
	ready, err := e.prove(proofCtx)
	e.mu.Lock()
	e.cancel = nil
	e.proofCtx = nil
	e.bridging = false
	// An answer of "not ready" is a verdict even when it arrives late.
	verdict := err == nil && !ready
	now := e.now()
	attemptDeadline := started.Add(materialEligibilityWindow)
	// The window is over when the clock says so, whether or not the proof
	// context's own timer has run yet: a proof whose stages time out at the
	// window's end, or a process that was not scheduled across it, returns
	// here first.
	pastWindow := !now.Before(attemptDeadline)
	// A success that arrives after its window proves nothing about now; it
	// is no answer, exactly as when the context had already ended.
	lateSuccess := err == nil && ready && pastWindow
	if err == nil && proofCtx.Err() != nil {
		err = proofCtx.Err()
	}
	// Three kinds of outcome besides success:
	//   - no answer: every cause is an expired deadline, no leader to ask, or
	//     a leader not read in time. It keeps a result still inside its
	//     window; past it, the attempt's own start, or for a proof that
	//     outlived its window one more window, bounds admission, never beyond
	//     the unconfirmed limit. Otherwise one blip closes every member.
	//   - a refusal of this member by the database: closes at once, also
	//     inside a live window and also when it arrives after the window.
	//   - any other answer ("not ready", an error nothing classifies): a read
	//     cut by a leadership change looks the same, so the earlier result
	//     keeps its own window, but the answer is latched:
	//     no later attempt without an answer extends admission.
	// Cancellation and an invalidation close at once. Only a successful proof
	// reopens what an answer or an invalidation closed.
	noAnswer := lateSuccess || (!verdict && store.ProofUnanswered.Unanswered(err))
	refused := store.Causes(err, store.CauseMemberRefused) != 0
	live := proofCtx.Err() == nil && !pastWindow && e.epoch == epoch && !e.stopped
	unexpired := !e.deadline.IsZero() && now.Before(e.deadline)
	outlived := noAnswer && (e.outlivedWindow(ctx, proofCtx, epoch) || (pastWindow && e.undisturbed(ctx, proofCtx, epoch))) && e.confirmedRecently(now)
	proved := err == nil && ready && live && now.Before(attemptDeadline)
	keepEarlier := !proved && !refused && live && unexpired
	renewAttempt := !keepEarlier && noAnswer && live && now.Before(attemptDeadline) && e.confirmedRecently(now)
	invalidated := !proved && !keepEarlier && !renewAttempt && !outlived
	notifyInvalidated := invalidated && e.epoch == epoch
	switch {
	case proved:
		e.provedAt, e.answered, e.refusal = started, false, ""
		e.admitUntil(attemptDeadline)
	case keepEarlier:
		e.answered = e.answered || !noAnswer
		ready = false
	case renewAttempt:
		// Not a successful proof: provedAt stays, so a write gate stops
		// treating the closure as transient before this deadline ends.
		limit := e.provedAt.Add(materialUnconfirmedLimit)
		if limit.After(attemptDeadline) {
			limit = attemptDeadline
		}
		e.admitUntil(limit)
		ready = false
	case outlived:
		e.admitUntil(e.unconfirmedDeadline(now))
		ready = false
	default:
		if e.epoch == epoch {
			e.closeAdmission()
		}
		if refused {
			e.refusal = refusalMemberRevoked
			if store.Causes(err, store.CauseIdentityConflict) != 0 {
				e.refusal = refusalIdentityConflict
			}
		}
		ready = false
	}
	onInvalidated, onOutcome := e.onInvalidated, e.onOutcome
	e.mu.Unlock()
	// An already-started callback must finish before the slot can admit a
	// replacement proof; it may still be withdrawing the previous routes.
	if !stopOnCancel() {
		<-callbackDone
	}
	if notifyInvalidated && onInvalidated != nil {
		onInvalidated(closeProofFailed)
	}
	if onOutcome != nil {
		onOutcome(proved)
	}
	cancel()
	return ready, err
}

// successRest computes scheduling headroom only; it never publishes authority.
func (e *materialEligibility) successRest() time.Duration {
	e.mu.Lock()
	defer e.mu.Unlock()
	remaining := e.deadline.Sub(e.now())
	// Check before subtracting: a zero deadline can saturate Sub at MinInt64.
	if remaining <= 2*store.ClusterMembershipGrantTimeout {
		return 0
	}
	return min(e.idleRest, remaining-2*store.ClusterMembershipGrantTimeout)
}

// Run proves on startup and refreshes at a bounded cadence. A definitive
// failure closes eligibility. A deadline exceeded while an earlier proof is
// still inside its window leaves that proof in place. Later successful
// proofs may restore a closed member. The caller must cancel ctx before
// releasing the Store used by prove.
func (e *materialEligibility) Run(ctx context.Context) {
	if e == nil {
		return
	}
	e.mu.Lock()
	if e.running || e.stopped {
		e.mu.Unlock()
		return
	}
	e.running = true
	e.runCtx = ctx
	e.mu.Unlock()
	defer e.Stop()
	ticker := time.NewTicker(e.interval)
	defer ticker.Stop()
	for {
		ready, err := e.Reprove(ctx)
		if ready && err == nil {
			// A slow success can leave an old tick pending. Rest only inside
			// the existing proof lifetime, reserving two bounded grants; this
			// does not extend eligibility or promise another proof will finish.
			quiet := e.successRest()
			if quiet > 0 {
				timer := time.NewTimer(quiet)
				select {
				case <-ctx.Done():
					timer.Stop()
					return
				case <-timer.C:
				}
			}
			ticker.Reset(e.interval)
			continue
		}
		if !e.Eligible() {
			ticker.Reset(e.interval)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Stop permanently closes this App incarnation's result and cancels its
// in-flight proof. A restarted App needs a fresh component and full proof.
func (e *materialEligibility) Stop() {
	if e == nil {
		return
	}
	e.mu.Lock()
	e.stopped = true
	e.epoch++
	e.closeAdmission()
	cancel := e.cancel
	onInvalidated := e.onInvalidated
	e.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if onInvalidated != nil {
		onInvalidated(closeProofStopped)
	}
}
