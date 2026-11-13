package cardsmoke

import (
	"context"
	"time"
)

// Checks made outside the runner's rounds, and what the rounds left unasked.

// Why a card was left unasked.
const (
	// SkipBusy: an instance serves on the card.
	SkipBusy = "busy"
	// SkipForeign: somebody outside the platform uses the card.
	SkipForeign = "foreign"
	// SkipDeferred: what the node runs is not known yet.
	SkipDeferred = "deferred"
	// SkipNotHeld: the platform does not hold the card (on a Kubernetes node,
	// it is the kubelet's to give to any pod), so it is not asked.
	SkipNotHeld = "not_held"
	// SkipUnavailable: the host cannot be asked right now.
	SkipUnavailable = "unavailable"
)

// Record takes an answer the card gave to a check made outside the runner
// (Doctor's) as if a round had asked it: a pass clears the card's failure and
// its backoff straight away, a failure counts and backs off like one of the
// runner's own. Without it a card that failed would stay unusable until its
// backoff — up to MaxBackoffRounds intervals — let a round ask it again,
// however clearly a diagnosis had just shown it working.
//
// Only an answer about the card belongs here. A check that could not reach
// the runtime, or was cut off by the caller's own budget, has no verdict and
// is not recorded.
func (r *Runner) Record(ident string, err error) {
	r.RecordTimed(ident, err, 0)
}

// RecordTimed is Record with how long the check took.
func (r *Runner) RecordTimed(ident string, err error, took time.Duration) {
	if r == nil || ident == "" {
		return
	}
	at := r.cfg.Now()
	r.mu.Lock()
	failures := r.outcomes[ident].Failures
	r.mu.Unlock()
	outcome := Outcome{OK: err == nil, At: at, Duration: took}
	if err != nil {
		outcome.Detail = err.Error()
		outcome.Failures = failures + 1
	}
	r.record(ident, outcome)
	r.scheduleNext(ident, outcome, at)
}

// CheckCard asks about one card now, whatever its schedule or backoff, and
// returns what it answered, or why it was not asked (a Skip* reason). A card an instance serves on, or somebody outside the
// platform uses, is left alone like a round leaves it; so is every card while
// the host cannot be asked.
func (r *Runner) CheckCard(ctx context.Context, ident string) (Outcome, string) {
	if r == nil {
		return Outcome{}, SkipUnavailable
	}
	if reason := r.unavailable(); reason != "" {
		return Outcome{}, SkipUnavailable
	}
	if r.deferredReason() != "" {
		return Outcome{}, SkipDeferred
	}
	for _, card := range r.cfg.Cards(ctx) {
		if card.Ident != ident {
			continue
		}
		switch {
		case r.cfg.Foreign != nil && r.cfg.Foreign(card):
			return Outcome{}, SkipForeign
		case r.busy(card):
			return Outcome{}, SkipBusy
		}
		before, heldBefore := r.answerCount(ident), r.notHeldCount(ident)
		r.tryUnlessBusy(ctx, card, false)
		if r.notHeldCount(ident) != heldBefore {
			return Outcome{}, SkipNotHeld
		}
		after, ok := r.Outcomes()[ident]
		if !ok || r.answerCount(ident) == before {
			// Inconclusive, cancelled, stuck or claimed while asked:
			// nothing was decided about the card.
			return Outcome{}, SkipUnavailable
		}
		return after, ""
	}
	return Outcome{}, SkipUnavailable
}

// Skips is why each card was last left unasked, for the cards whose last
// skip is newer than their last answer.
func (r *Runner) Skips() map[string]Skip {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]Skip, len(r.skips))
	for ident, skip := range r.skips {
		out[ident] = skip
	}
	return out
}

func (r *Runner) skip(ident, reason string) {
	r.mu.Lock()
	r.skips[ident] = Skip{Reason: reason, At: r.cfg.Now()}
	r.mu.Unlock()
}

func (r *Runner) skipAll(cards []Card, reason string) {
	for _, card := range cards {
		r.skip(card.Ident, reason)
	}
}

func (r *Runner) notHeldCount(ident string) uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.notHeld[ident]
}

func (r *Runner) answerCount(ident string) uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.answers[ident]
}
