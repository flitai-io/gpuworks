// Package cardsmoke decides when each accelerator is asked whether it can
// actually run work, and remembers what it answered.
//
// Listing a card and using one are different questions. The vendor CLI can
// describe a card in full while nothing can create a context on it: on
// zstone-164 nvidia-smi answers for two cards and cuInit returns 101 for
// every one of them, because the driver keeps retrying three cards it
// cannot bring up. Only trying it says so.
//
// It is all scheduling and no vendor knowledge: what the attempt is gets
// injected, so a vendor's own runtime check plugs in without this package
// learning about it, and the tests need no accelerator.
package cardsmoke

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// Defaults. One round every quarter hour costs each card a fraction of a
// second and catches a driver that broke since the last one; a card busy
// serving is skipped, so the cost does not land on a working node.
const (
	DefaultInterval = 15 * time.Minute
	DefaultTimeout  = 30 * time.Second
	// DefaultMaxBackoffRounds caps how far a card that keeps failing is
	// spaced out. A card that answers costs a fraction of a second, but one
	// that cannot be used spends the whole timeout before saying so, and on
	// a host with many of them a round would otherwise run almost back to
	// back. Four intervals is about an hour: long enough to spare the host,
	// short enough that a card repaired out of band is noticed the same day.
	DefaultMaxBackoffRounds = 4
	// DefaultFirstRound leaves the control plane time to say what this
	// node runs, so the first round is not skipped for every card.
	DefaultFirstRound = 30 * time.Second
	// DefaultRetryAfter is how long after a round that could not ask any
	// card to try again. It is the one retry: what this node runs arrives
	// seconds after registration, and a round that still cannot tell is
	// waiting on something an interval will not fix either.
	DefaultRetryAfter = time.Minute
)

// Inconclusive marks an attempt's failure as saying nothing about the card:
// the attempt never reached it. The card is then left exactly as it was, and
// the round stops because whatever stopped the attempt would stop every other
// one in it too.
func Inconclusive(reason string) error {
	return inconclusiveError{reason: strings.TrimSpace(reason)}
}

type inconclusiveError struct{ reason string }

func (e inconclusiveError) Error() string { return e.reason }

// NotHeld marks an attempt's failure as "the platform does not hold this
// card": it is not the platform's to ask (on a Kubernetes node, the kubelet may
// give it to another pod at any moment). The attempt never touched the card, so
// nothing is recorded about it except that it was skipped, and — unlike
// Inconclusive — the round goes on with the other cards.
func NotHeld(reason string) error {
	return notHeldError{reason: strings.TrimSpace(reason)}
}

type notHeldError struct{ reason string }

func (e notHeldError) Error() string {
	if e.reason == "" {
		return "the platform does not hold this card"
	}
	return e.reason
}

// NotHeldReason is why the platform does not hold the card the attempt was
// about, or "" for any other error.
func NotHeldReason(err error) string {
	var held notHeldError
	if errors.As(err, &held) {
		return held.Error()
	}
	return ""
}

func inconclusiveReason(err error) string {
	var inconclusive inconclusiveError
	if errors.As(err, &inconclusive) {
		if inconclusive.reason == "" {
			return "the runtime check could not be made"
		}
		return inconclusive.reason
	}
	return ""
}

// Card is what the runner needs to know about one accelerator: how to name
// it, how to ask the runtime for it alone, and how a caller refers to it.
type Card struct {
	// Ident is the card's stable identity (accelerator.CardIdent), which is
	// how its answer is remembered and matched back to it.
	Ident string
	// Visible is how the vendor check identifies this card and no other (for
	// NVIDIA, the value of CUDA_VISIBLE_DEVICES).
	Visible string
}

// Outcome is what one card last answered. Only a card that was actually
// tried has one: a card with no outcome was never asked, which is not the
// same as a card that failed and must never be read as one.
type Outcome struct {
	// OK is whether the runtime could use the card.
	OK bool
	// Detail is the failure as the attempt worded it.
	Detail string
	// At is when this was decided.
	At time.Time
	// Failures is how many times in a row this card has failed, which spaces
	// out the next attempt. It is reset by a card that answers.
	Failures int
	// Duration is how long the attempt took, when the runner timed it.
	Duration time.Duration
}

// Skip is why the card was last left unasked (one of the Skip* reasons), and
// when.
type Skip struct {
	Reason string
	At     time.Time
}

// Config is what a runner needs from its surroundings. Every function is
// called from the runner's own goroutine.
type Config struct {
	// Cards lists the accelerators to try, newest view each round. The
	// round's context bounds it, so a shutdown is not waited out by a
	// driver that has stopped answering.
	Cards func(ctx context.Context) []Card
	// Try asks the runtime to use one card. A nil error means it could.
	Try func(ctx context.Context, card Card) error
	// Busy reports that the card is serving, so it is left alone: creating
	// a context on a card in use can disturb what runs there, and a card
	// that is serving has already answered this question.
	Busy func(card Card) bool
	// Foreign reports that somebody outside the platform is using the
	// card. It is left alone even when asked about directly (CheckNow):
	// their workload is not ours to disturb, and a card whose memory they
	// hold says nothing about whether the runtime can use it.
	Foreign func(card Card) bool
	// Unavailable explains why no attempt can be made at all right now —
	// the driver is not answering, the runtime is missing — or returns "".
	// Checked once per round, because it is a property of the host, and
	// the round is then skipped: a host that cannot be asked says nothing
	// about any card, and each card keeps whatever the vendor tool said.
	// Try reports the same discovery through Inconclusive when it can only be
	// made by attempting the check.
	Unavailable func() string

	Interval time.Duration
	Timeout  time.Duration
	// MaxBackoffRounds caps the per-card failure backoff. One limits the wait
	// to a single interval.
	MaxBackoffRounds int
	// FirstRound is how long to wait before the first round. A node that
	// just connected should not wait a quarter hour to learn its cards
	// cannot be used, but it must wait for the control plane to say what
	// it is running: Deferred holds the round back until it knows.
	// Measured on gpu3: the desired state arrived about ten seconds after
	// the agent registered; on 192.0.2.129 it took forty-seven.
	FirstRound time.Duration
	// Deferred says why no card can be asked yet, or "". It is what this
	// node runs not being known: asking a card that is serving can disturb
	// it, so a round that cannot tell asks nothing. Unlike Unavailable it
	// resolves on its own within seconds, so such a round is retried once
	// after RetryAfter rather than waited out for a whole interval — until
	// then every card still carries the vendor tool's verdict, and a card
	// the runtime cannot use looks schedulable.
	Deferred func() string
	// RetryAfter is how long after a deferred round to try again, once.
	RetryAfter time.Duration
	Now        func() time.Time
}

// Runner runs rounds and keeps each card's last answer.
type Runner struct {
	cfg Config

	mu       sync.Mutex
	outcomes map[string]Outcome
	// running guards against rounds overlapping. A round on a slow driver
	// can outlast the interval, and starting another would put a second
	// process on the same driver lock — the pile-up this package exists to
	// avoid.
	running bool
	// stuck are the cards whose last check was abandoned at the deadline
	// and has not returned since. The process behind it is still waiting
	// on the driver; starting another would add a second one to the same
	// wait every round, which is the pile-up this package exists to avoid.
	stuck map[string]bool
	// nextAttempt is when a failed card may be tried again.
	nextAttempt map[string]time.Time
	// roundStart is when the round in flight began, so a long round does not
	// stretch the backoff of cards tried near its end.
	roundStart time.Time
	// skipped is why the last round asked no card at all, or "".
	skipped string
	// deferred is whether the last round asked no card because what this
	// node runs was not known yet, which one retry can resolve.
	deferred bool
	// attempts are the checks on each card now and a moment ago (Touched,
	// WaitIdle).
	attempts attemptLog
	// skips is why each card was left unasked since its last answer.
	skips map[string]Skip
	// answers counts each card's recorded answers, so a caller can tell an
	// attempt that decided something from one that did not.
	answers map[string]uint64
	// notHeld counts each card's attempts refused as not held, for the same
	// reason.
	notHeld map[string]uint64
}

func New(cfg Config) *Runner {
	if cfg.Interval <= 0 {
		cfg.Interval = DefaultInterval
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = DefaultTimeout
	}
	if cfg.FirstRound <= 0 {
		cfg.FirstRound = DefaultFirstRound
	}
	if cfg.MaxBackoffRounds <= 0 {
		cfg.MaxBackoffRounds = DefaultMaxBackoffRounds
	}
	if cfg.RetryAfter <= 0 {
		cfg.RetryAfter = DefaultRetryAfter
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Runner{cfg: cfg, outcomes: map[string]Outcome{}, stuck: map[string]bool{}, nextAttempt: map[string]time.Time{}, skips: map[string]Skip{}, answers: map[string]uint64{}, notHeld: map[string]uint64{}}
}

// Run keeps running rounds until ctx ends.
func (r *Runner) Run(ctx context.Context) {
	first := time.NewTimer(r.cfg.FirstRound)
	defer first.Stop()
	select {
	case <-ctx.Done():
		return
	case <-first.C:
		r.runRound(ctx)
	}
	ticker := time.NewTicker(r.cfg.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.runRound(ctx)
		}
	}
}

// runRound is one scheduled round, plus the single retry a deferred round
// earns. One retry and no more: a round that still cannot tell what this node
// runs is waiting on something the next interval will not fix either, and
// retrying on a timer of its own would ask every card on a busy node.
func (r *Runner) runRound(ctx context.Context) {
	r.RunOnce(ctx)
	if !r.deferredRound() {
		return
	}
	retry := time.NewTimer(r.cfg.RetryAfter)
	defer retry.Stop()
	select {
	case <-ctx.Done():
	case <-retry.C:
		r.RunOnce(ctx)
	}
}

func (r *Runner) deferredRound() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.deferred
}

// RunOnce tries cards in order and returns when the round is done or an
// attempt makes starting another unsafe. A round already in flight is left to
// finish and this call does nothing.
func (r *Runner) RunOnce(ctx context.Context) {
	r.mu.Lock()
	if r.running {
		r.mu.Unlock()
		return
	}
	r.running = true
	r.roundStart = r.cfg.Now()
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		r.running = false
		r.mu.Unlock()
	}()

	cards := r.cfg.Cards(ctx)
	if len(cards) == 0 {
		return
	}
	reason := r.unavailable()
	deferred := false
	if reason == "" {
		if why := r.deferredReason(); why != "" {
			reason, deferred = why, true
		}
	}
	if reason == "" {
		for i, card := range cards {
			if ctx.Err() != nil {
				break
			}
			if stopped := r.try(ctx, card); stopped != "" {
				reason = stopped
				r.skipAll(cards[i+1:], SkipUnavailable)
				break
			}
		}
	} else if deferred {
		r.skipAll(cards, SkipDeferred)
	} else {
		r.skipAll(cards, SkipUnavailable)
	}
	r.mu.Lock()
	changed := reason != r.skipped
	r.skipped = reason
	r.deferred = deferred
	r.mu.Unlock()
	if reason != "" && changed {
		// Once per change: the round is skipped every quarter hour for as
		// long as the cause lasts.
		slog.Info("accelerator runtime check cannot run", "reason", reason, "cards", len(cards))
	}
}

// CheckNow asks about the named cards straight away, outside the schedule.
//
// It exists for the one case the schedule cannot answer: a workload that could
// not start on a card. A card an instance holds is skipped by a round (Busy),
// because asking a card that is serving can disturb it — but an instance that
// failed to start is not serving, and without this the card it failed on would
// keep the vendor tool's verdict and stay schedulable, so the next attempt
// lands on it again. The workload is the better probe anyway: it really tried
// to open the device.
//
// A card whose answer is newer than one interval is left alone, so a workload
// failing for its own reasons does not re-ask every card it restarts on. The
// host-level and stuck rules are the round's, because they are about the host
// rather than this card.
func (r *Runner) CheckNow(ctx context.Context, idents map[string]bool) {
	if len(idents) == 0 {
		return
	}
	if reason := r.unavailable(); reason != "" {
		return
	}
	for _, card := range r.cfg.Cards(ctx) {
		if ctx.Err() != nil {
			return
		}
		if !idents[card.Ident] || r.answeredRecently(card.Ident) {
			continue
		}
		if stopped := r.tryUnlessBusy(ctx, card, false); stopped != "" {
			return
		}
	}
}

// answeredRecently reports whether this card answered within the last
// interval, whatever it answered.
func (r *Runner) answeredRecently(ident string) bool {
	r.mu.Lock()
	outcome, ok := r.outcomes[ident]
	r.mu.Unlock()
	return ok && r.cfg.Now().Sub(outcome.At) < r.cfg.Interval
}

// try asks about one card and records what it answered. It returns why the
// round must stop: a host-wide inconclusive answer, a timed-out attempt, or an
// older attempt that has not returned.
func (r *Runner) try(ctx context.Context, card Card) string {
	return r.tryUnlessBusy(ctx, card, true)
}

// tryUnlessBusy is try, with whether a card an instance holds is left alone.
func (r *Runner) tryUnlessBusy(ctx context.Context, card Card, skipBusy bool) string {
	r.mu.Lock()
	stuck := r.stuck[card.Ident]
	anyStuck := len(r.stuck) > 0
	failures := r.outcomes[card.Ident].Failures
	r.mu.Unlock()
	if anyStuck {
		detail := fmt.Sprintf("an earlier runtime check has not returned after more than %s; no new card can be checked", r.cfg.Timeout)
		if !stuck {
			r.skip(card.Ident, SkipUnavailable)
		}
		if stuck {
			detail = fmt.Sprintf("the previous runtime check of this card has not returned after more than %s", r.cfg.Timeout)
			r.record(card.Ident, Outcome{At: r.cfg.Now(), Failures: failures, Detail: detail})
		}
		return detail
	}
	if r.cfg.Foreign != nil && r.cfg.Foreign(card) {
		// Like a serving card, the card keeps whatever it last said.
		r.skip(card.Ident, SkipForeign)
		return ""
	}
	if skipBusy && r.busy(card) {
		// Serving work is itself the answer, so the card keeps whatever it
		// last said rather than being recorded as untried.
		r.skip(card.Ident, SkipBusy)
		return ""
	}
	if skipBusy && r.backingOff(card.Ident) {
		// Its last answer, a failure, stands until the backoff lets a
		// round ask again; that is not a skip worth stating.
		return ""
	}
	attempt, cancel := context.WithTimeout(ctx, r.cfg.Timeout)
	defer cancel()
	// The deadline is the runner's, not only the attempt's. A check that
	// ignores its context — a python stuck in D state on the very driver
	// this package exists to survive — would otherwise hold the round
	// open forever, and with it every round after it.
	type attemptAnswer struct {
		err        error
		finishedAt time.Time
	}
	done := make(chan attemptAnswer, 1)
	started := time.Now()
	end := r.Begin(card.Ident)
	go func() {
		err := r.cfg.Try(attempt, card)
		end()
		// Sent before the mark is cleared, so the runner either sees the
		// answer or finds the mark already gone, never a mark left behind.
		done <- attemptAnswer{err: err, finishedAt: time.Now()}
		r.mu.Lock()
		delete(r.stuck, card.Ident)
		r.mu.Unlock()
	}()
	deadline, _ := attempt.Deadline()
	timeoutErr := fmt.Errorf("the runtime check did not finish within %s", r.cfg.Timeout)
	// When done and the deadline are both ready, select's random choice must
	// not decide the verdict. The attempt state and completion timestamp give
	// both paths the same answer; a returned attempt is never left marked stuck.
	grade := func(answer attemptAnswer) error {
		if attempt.Err() != nil || !answer.finishedAt.Before(deadline) {
			return timeoutErr
		}
		return answer.err
	}
	var err error
	took := r.cfg.Timeout
	select {
	case answer := <-done:
		err = grade(answer)
		took = answer.finishedAt.Sub(started)
	case <-attempt.Done():
		if ctx.Err() != nil {
			// The round was cancelled (agent shutdown), not the card's
			// deadline. The attempt is discarded: no outcome, no failure
			// count, no backoff, and nothing is marked stuck because the
			// runner is going away.
			return ""
		}
		err = timeoutErr
		r.mu.Lock()
		r.stuck[card.Ident] = true
		r.mu.Unlock()
		// The check may have returned between the deadline and this mark.
		select {
		case answer := <-done:
			err = grade(answer)
			r.mu.Lock()
			delete(r.stuck, card.Ident)
			r.mu.Unlock()
		default:
		}
	}
	if err != nil && ctx.Err() != nil {
		// A failure that arrived with the round's cancellation is the
		// shutdown's, not the card's.
		return ""
	}
	if err != nil && r.claimedDuring(card, skipBusy) {
		// An instance was placed on the card, or somebody outside the
		// platform started using it, while it was being asked. On a card
		// only one process may open, the failure is theirs having got
		// there first, not the card's: nothing is recorded and no backoff
		// is earned. A round asks it again once it is free.
		if err == timeoutErr {
			return timeoutErr.Error()
		}
		return ""
	}
	if NotHeldReason(err) != "" {
		// The platform does not hold the card: it was never asked, and
		// keeps whatever it last answered.
		r.mu.Lock()
		r.skips[card.Ident] = Skip{Reason: SkipNotHeld, At: r.cfg.Now()}
		r.notHeld[card.Ident]++
		r.mu.Unlock()
		return ""
	}
	if inconclusive := inconclusiveReason(err); inconclusive != "" {
		// A host failure cannot push a card into a backoff it did not earn.
		r.skip(card.Ident, SkipUnavailable)
		return inconclusive
	}
	outcome := Outcome{OK: err == nil, At: r.cfg.Now(), Duration: took}
	if err != nil {
		outcome.Detail = err.Error()
		outcome.Failures = failures + 1
	}
	r.record(card.Ident, outcome)
	r.scheduleNext(card.Ident, outcome, r.roundFrom(outcome.At))
	if err == timeoutErr {
		return timeoutErr.Error()
	}
	return ""
}

// claimedDuring reports whether the card was taken by an instance (only when
// the attempt was one that leaves held cards alone) or by somebody outside
// the platform since the attempt began.
func (r *Runner) claimedDuring(card Card, skipBusy bool) bool {
	return (skipBusy && r.busy(card)) || (r.cfg.Foreign != nil && r.cfg.Foreign(card))
}

func (r *Runner) backingOff(ident string) bool {
	r.mu.Lock()
	due, waiting := r.nextAttempt[ident]
	r.mu.Unlock()
	return waiting && r.cfg.Now().Before(due)
}

// scheduleNext doubles the wait after each consecutive failure up to
// MaxBackoffRounds intervals. A successful answer clears it.
// from is when the wait is measured from.
func (r *Runner) scheduleNext(ident string, outcome Outcome, from time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if outcome.OK || outcome.Failures <= 0 {
		delete(r.nextAttempt, ident)
		return
	}
	rounds := 1
	for i := 1; i < outcome.Failures && rounds < r.cfg.MaxBackoffRounds; i++ {
		rounds *= 2
	}
	if rounds > r.cfg.MaxBackoffRounds {
		rounds = r.cfg.MaxBackoffRounds
	}
	// Half an interval of slack, measured from the round rather than the
	// attempt, keeps a tick that arrives a little early from adding a round.
	r.nextAttempt[ident] = from.Add(time.Duration(rounds)*r.cfg.Interval - r.cfg.Interval/2)
}

// roundFrom is when the round in flight began, or at when none has.
func (r *Runner) roundFrom(at time.Time) time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.roundStart.IsZero() {
		return at
	}
	return r.roundStart
}

// Skipped is why the last round could not ask any card or stopped partway,
// and "" when every card was asked. Doctor shows it: a check that has never
// run says nothing, and an operator has to be told that rather than read the
// silence as a pass.
func (r *Runner) Skipped() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.skipped
}

// Outcomes is each card's last answer, by identity.
func (r *Runner) Outcomes() map[string]Outcome {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]Outcome, len(r.outcomes))
	for ident, outcome := range r.outcomes {
		out[ident] = outcome
	}
	return out
}

func (r *Runner) record(ident string, outcome Outcome) {
	r.mu.Lock()
	r.outcomes[ident] = outcome
	r.answers[ident]++
	delete(r.skips, ident)
	r.mu.Unlock()
}

func (r *Runner) busy(card Card) bool {
	return r.cfg.Busy != nil && r.cfg.Busy(card)
}

func (r *Runner) deferredReason() string {
	if r.cfg.Deferred == nil {
		return ""
	}
	return r.cfg.Deferred()
}

func (r *Runner) unavailable() string {
	if r.cfg.Unavailable == nil {
		return ""
	}
	return r.cfg.Unavailable()
}
