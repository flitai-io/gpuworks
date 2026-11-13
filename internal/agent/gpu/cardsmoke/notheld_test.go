package cardsmoke

import (
	"context"
	"errors"
	"testing"
)

// An attempt that ends in NotHeld never touched the card: the round records the
// skip with its reason and goes on to the other cards (unlike Inconclusive,
// which is about the host and stops it), and a card that answered before keeps
// that answer.
func TestNotHeldSkipsTheCardAndTheRoundGoesOn(t *testing.T) {
	now, advance := clock(t)
	r := &recorder{now: now}
	notHeld := map[string]bool{"b": true}
	try := func(ctx context.Context, card Card) error {
		if notHeld[card.Ident] {
			return NotHeld("Kubernetes may give " + card.Ident + " to another pod")
		}
		return r.try(ctx, card)
	}
	runner := New(Config{Cards: func(context.Context) []Card { return cards("a", "b", "c") }, Try: try, Now: now})
	runner.RunOnce(context.Background())
	if got := r.idents(); len(got) != 2 || got[0] != "a" || got[1] != "c" {
		t.Fatalf("cards asked = %v, want a and c (the round must go on past b)", got)
	}
	skips := runner.Skips()
	if skips["b"].Reason != SkipNotHeld || len(skips) != 1 {
		t.Fatalf("skips = %+v", skips)
	}
	if _, answered := runner.Outcomes()["b"]; answered {
		t.Fatal("a card that was never asked has an outcome")
	}

	// b answers once the platform holds it; the skip is then replaced.
	notHeld["b"] = false
	advance(DefaultInterval)
	runner.RunOnce(context.Background())
	if _, ok := runner.Skips()["b"]; ok || !runner.Outcomes()["b"].OK {
		t.Fatalf("after b was held: skips %+v outcomes %+v", runner.Skips(), runner.Outcomes())
	}
	// ... and when it is lost again, its last answer stands next to the skip.
	notHeld["b"] = true
	advance(DefaultInterval)
	runner.RunOnce(context.Background())
	if !runner.Outcomes()["b"].OK || runner.Skips()["b"].Reason != SkipNotHeld {
		t.Fatalf("a card no longer held lost its answer or its skip: %+v / %+v", runner.Outcomes()["b"], runner.Skips()["b"])
	}
}

// Asked directly (a card recheck), a card the platform does not hold is
// reported as such, not as an unavailable host.
func TestCheckCardSaysWhenTheCardIsNotHeld(t *testing.T) {
	now, _ := clock(t)
	runner := New(Config{
		Cards: func(context.Context) []Card { return cards("a") },
		Try:   func(context.Context, Card) error { return NotHeld("") },
		Now:   now,
	})
	if outcome, reason := runner.CheckCard(context.Background(), "a"); reason != SkipNotHeld || outcome != (Outcome{}) {
		t.Fatalf("CheckCard = %+v, %q", outcome, reason)
	}
}

func TestNotHeldReason(t *testing.T) {
	if got := NotHeldReason(NotHeld("  why ")); got != "why" {
		t.Fatalf("reason = %q", got)
	}
	if got := NotHeldReason(NotHeld("")); got == "" {
		t.Fatal("an empty reason must still say the card is not held")
	}
	wrapped := errors.Join(errors.New("context"), NotHeld("why"))
	if NotHeldReason(wrapped) != "why" || NotHeldReason(errors.New("other")) != "" || NotHeldReason(nil) != "" || inconclusiveReason(NotHeld("x")) != "" {
		t.Fatal("NotHeld is told apart from other errors, including Inconclusive")
	}
}
