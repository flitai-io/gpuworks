package cardsmoke

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

type attempt struct {
	ident string
	at    time.Time
}

// recorder is a Try that records what it was asked and answers as told.
type recorder struct {
	mu       sync.Mutex
	attempts []attempt
	fail     map[string]error
	block    chan struct{}
	now      func() time.Time
}

func (r *recorder) try(ctx context.Context, card Card) error {
	r.mu.Lock()
	r.attempts = append(r.attempts, attempt{ident: card.Ident, at: r.now()})
	block, err := r.block, r.fail[card.Ident]
	r.mu.Unlock()
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return err
}

func (r *recorder) idents() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.attempts))
	for _, a := range r.attempts {
		out = append(out, a.ident)
	}
	return out
}

func cards(idents ...string) []Card {
	out := make([]Card, 0, len(idents))
	for _, ident := range idents {
		out = append(out, Card{Ident: ident, Visible: "GPU-" + ident})
	}
	return out
}

func clock(t *testing.T) (func() time.Time, func(time.Duration)) {
	t.Helper()
	var mu sync.Mutex
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	return func() time.Time {
			mu.Lock()
			defer mu.Unlock()
			return now
		}, func(d time.Duration) {
			mu.Lock()
			now = now.Add(d)
			mu.Unlock()
		}
}

// Each card is asked separately: the answers differ, which is the whole
// reason for asking per card rather than once per node.
func TestEachCardIsAskedAndRemembersItsOwnAnswer(t *testing.T) {
	now, _ := clock(t)
	rec := &recorder{fail: map[string]error{"b": errors.New("cuInit=101")}, now: now}
	r := New(Config{
		Cards: func(context.Context) []Card { return cards("a", "b", "c") },
		Try:   rec.try,
		Now:   now,
	})
	r.RunOnce(context.Background())

	if got := rec.idents(); len(got) != 3 || got[0] != "a" || got[2] != "c" {
		t.Fatalf("attempts = %v, want each card once in order", got)
	}
	outcomes := r.Outcomes()
	if o, ok := outcomes["a"]; !ok || !o.OK {
		t.Fatalf("a = %+v, want a card that answered", o)
	}
	if o, ok := outcomes["b"]; !ok || o.OK || o.Detail != "cuInit=101" {
		t.Fatalf("b = %+v, want the failure it reported", o)
	}
	if o, ok := outcomes["c"]; !ok || !o.OK {
		t.Fatalf("c = %+v", o)
	}
	if !outcomes["a"].At.Equal(now()) {
		t.Fatalf("the answer carries no time: %+v", outcomes["a"])
	}
}

// A round on a slow driver can outlast the interval. Starting another would
// put a second process on the same driver lock, which is the pile-up this
// exists to avoid.
func TestARoundNeverOverlapsAnother(t *testing.T) {
	now, _ := clock(t)
	rec := &recorder{block: make(chan struct{}), now: now}
	r := New(Config{Cards: func(context.Context) []Card { return cards("a", "b") }, Try: rec.try, Now: now})

	started := make(chan struct{})
	go func() {
		close(started)
		r.RunOnce(context.Background())
	}()
	<-started
	// Wait until the first card is in flight, then try to start a round.
	deadline := time.Now().Add(2 * time.Second)
	for len(rec.idents()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the first round never started")
		}
		time.Sleep(time.Millisecond)
	}
	r.RunOnce(context.Background())
	if got := len(rec.idents()); got != 1 {
		t.Fatalf("attempts while a round is in flight = %d, want the first card only", got)
	}
	close(rec.block)
}

// A card that is serving has answered by serving, and creating another
// context on it could disturb what runs there.
func TestACardThatIsServingIsLeftAlone(t *testing.T) {
	now, _ := clock(t)
	rec := &recorder{now: now}
	r := New(Config{
		Cards: func(context.Context) []Card { return cards("a", "b") },
		Try:   rec.try,
		Busy:  func(card Card) bool { return card.Ident == "a" },
		Now:   now,
	})
	r.RunOnce(context.Background())
	if got := rec.idents(); len(got) != 1 || got[0] != "b" {
		t.Fatalf("attempts = %v, want only the idle card", got)
	}
	if _, recorded := r.Outcomes()["a"]; recorded {
		t.Fatal("a card that was not tried must not be recorded as anything")
	}
}

// A host where the check cannot run leaves every card unanswered rather
// than answered badly: "we could not ask" is not a verdict on any card.
func TestAHostThatCannotRunTheCheckAnswersForNoCard(t *testing.T) {
	now, _ := clock(t)
	rec := &recorder{now: now}
	r := New(Config{
		Cards:       func(context.Context) []Card { return cards("a") },
		Try:         rec.try,
		Unavailable: func() string { return "python3 is not installed" },
		Now:         now,
	})
	r.RunOnce(context.Background())
	if got := rec.idents(); len(got) != 0 {
		t.Fatalf("attempts = %v, want none", got)
	}
	if outcome, ok := r.Outcomes()["a"]; ok {
		t.Fatalf("outcome = %+v, want no answer recorded for an untried card", outcome)
	}
	// The reason is kept for doctor: a check that never ran must not read
	// as a check that passed.
	if got := r.Skipped(); got != "python3 is not installed" {
		t.Fatalf("skipped = %q", got)
	}
}

// What a card said while the check worked is not undone by the check later
// being unavailable.
func TestAnAnswerSurvivesTheCheckBecomingUnavailable(t *testing.T) {
	now, _ := clock(t)
	rec := &recorder{fail: map[string]error{"a": errors.New("cuInit=101")}, now: now}
	unavailable := ""
	r := New(Config{
		Cards:       func(context.Context) []Card { return cards("a") },
		Try:         rec.try,
		Unavailable: func() string { return unavailable },
		Now:         now,
	})
	r.RunOnce(context.Background())
	if o, ok := r.Outcomes()["a"]; !ok || o.OK {
		t.Fatalf("first round = %+v", o)
	}
	unavailable = "the accelerator driver is not answering"
	r.RunOnce(context.Background())
	if o, ok := r.Outcomes()["a"]; !ok || o.OK || o.Detail != "cuInit=101" {
		t.Fatalf("after the driver stopped answering = %+v, want the card's own answer kept", o)
	}
}

// An abandoned attempt still owns the driver. Neither the rest of its round
// nor a later round may start another card until that attempt has returned.
func TestATimedOutAttemptStopsThisAndLaterRounds(t *testing.T) {
	now, _ := clock(t)
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	returned := make(chan struct{})
	var mu sync.Mutex
	var attempted []string
	r := New(Config{
		Cards: func(context.Context) []Card { return cards("a", "b") },
		Try: func(_ context.Context, card Card) error {
			mu.Lock()
			attempted = append(attempted, card.Ident)
			mu.Unlock()
			if card.Ident == "a" {
				<-release
				close(returned)
			}
			return nil
		},
		Timeout: 20 * time.Millisecond,
		Now:     now,
	})
	for round := 0; round < 2; round++ {
		r.RunOnce(context.Background())
		mu.Lock()
		got := append([]string(nil), attempted...)
		mu.Unlock()
		if len(got) != 1 || got[0] != "a" {
			t.Fatalf("round %d attempts = %v, want only the original stuck attempt", round+1, got)
		}
		if outcome, found := r.Outcomes()["b"]; found {
			t.Fatalf("round %d gave the untried second card an outcome: %+v", round+1, outcome)
		}
	}
	if outcome := r.Outcomes()["a"]; outcome.OK || !strings.Contains(outcome.Detail, "has not returned") {
		t.Fatalf("first card outcome = %+v, want the still-stuck failure", outcome)
	}
	if got := r.Skipped(); !strings.Contains(got, "has not returned") {
		t.Fatalf("skipped = %q, want the reason no second card was tried", got)
	}
	unblock()
	select {
	case <-returned:
	case <-time.After(2 * time.Second):
		t.Fatal("the original attempt did not return after being released")
	}
}

// A cancelled context stops the round rather than working through the rest.
func TestACancelledRoundStops(t *testing.T) {
	now, _ := clock(t)
	rec := &recorder{now: now}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := New(Config{Cards: func(context.Context) []Card { return cards("a", "b") }, Try: rec.try, Now: now})
	r.RunOnce(ctx)
	if got := rec.idents(); len(got) != 0 {
		t.Fatalf("attempts after cancellation = %v", got)
	}
}

func TestRunDoesTheFirstRoundWithoutWaitingForTheInterval(t *testing.T) {
	now, _ := clock(t)
	rec := &recorder{now: now}
	r := New(Config{
		Cards:      func(context.Context) []Card { return cards("a") },
		Try:        rec.try,
		Interval:   time.Hour,
		FirstRound: time.Millisecond,
		Now:        now,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Run(ctx)
	deadline := time.Now().Add(2 * time.Second)
	for len(rec.idents()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("a node that just connected waited for the interval to learn its cards cannot be used")
		}
		time.Sleep(time.Millisecond)
	}
}

// A check that ignores its context must not hold the runner open. The
// real one shells out to python on the very driver this package exists
// to survive, and a process stuck in D state does not return.
func TestACheckThatIgnoresItsDeadlineDoesNotStallTheRunner(t *testing.T) {
	now, _ := clock(t)
	blocked := make(chan struct{})
	defer close(blocked)
	r := New(Config{
		Cards:   func(context.Context) []Card { return cards("a") },
		Try:     func(context.Context, Card) error { <-blocked; return nil },
		Timeout: 20 * time.Millisecond,
		Now:     now,
	})
	done := make(chan struct{})
	go func() { r.RunOnce(context.Background()); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the round never ended, so no later round can run either")
	}
	outcome, ok := r.Outcomes()["a"]
	if !ok || outcome.OK || !strings.Contains(outcome.Detail, "did not finish") {
		t.Fatalf("outcome = %+v, want it recorded as not finishing in time", outcome)
	}
	// And the runner is free for the next round.
	r.RunOnce(context.Background())
}

// A check abandoned at its deadline is still waiting on the driver. The
// next round must not start another beside it: a card stuck in D state
// would gain one python process every quarter hour for as long as it
// stays stuck. Once the first one returns, the card is asked again.
func TestAStuckCheckIsNotJoinedByAnother(t *testing.T) {
	now, advance := clock(t)
	release := make(chan struct{})
	var mu sync.Mutex
	started := 0
	r := New(Config{
		Cards: func(context.Context) []Card { return cards("a") },
		Try: func(context.Context, Card) error {
			mu.Lock()
			started++
			first := started == 1
			mu.Unlock()
			if first {
				<-release
			}
			return nil
		},
		Timeout: 20 * time.Millisecond,
		Now:     now,
	})
	r.RunOnce(context.Background())
	r.RunOnce(context.Background())
	mu.Lock()
	if started != 1 {
		t.Fatalf("checks started = %d while the first is still stuck, want 1", started)
	}
	mu.Unlock()
	if outcome := r.Outcomes()["a"]; outcome.OK || !strings.Contains(outcome.Detail, "has not returned") {
		t.Fatalf("outcome = %+v, want the card failed for the check it is stuck in", outcome)
	}

	close(release)
	// The abandoned check counted as a failure, so the card also has to reach
	// the end of that failure's backoff before it can be asked again.
	advance(16 * time.Minute)
	deadline := time.Now().Add(2 * time.Second)
	for {
		r.RunOnce(context.Background())
		if outcome := r.Outcomes()["a"]; outcome.OK {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the card was never asked again after its stuck check returned")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A round that fires before the control plane has said what this node runs
// asks no card at all, which left every card on the vendor tool's verdict
// for a whole interval: on 192.0.2.129 the desired state arrived
// forty-seven seconds after the agent registered, seventeen seconds after
// the first round, and a die the runtime could not use stayed schedulable
// for the next quarter hour. Such a round is retried once.
func TestADeferredRoundIsRetriedOnce(t *testing.T) {
	now, _ := clock(t)
	rec := &recorder{now: now}
	var mu sync.Mutex
	calls := 0
	// The retry waits here until this test has seen the first skip.
	// RetryAfter is 10ms, so a loaded machine otherwise spends that retry
	// while the node is still unknown, or reads Skipped before the round
	// publishes that the skip is over.
	proceed := make(chan struct{})
	r := New(Config{
		Cards: func(context.Context) []Card { return cards("a") },
		Try:   rec.try,
		Deferred: func() string {
			mu.Lock()
			calls++
			n := calls
			mu.Unlock()
			if n == 1 {
				return "what this node runs is not known yet"
			}
			<-proceed
			return ""
		},
		Interval:   time.Hour,
		FirstRound: time.Millisecond,
		RetryAfter: 10 * time.Millisecond,
		Now:        now,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Run(ctx)

	// The deferred round says why it asked nothing, and blames no card.
	deadline := time.Now().Add(2 * time.Second)
	for r.Skipped() == "" {
		if time.Now().After(deadline) {
			t.Fatal("a round that asked no card did not say why")
		}
		time.Sleep(time.Millisecond)
	}
	if got := rec.idents(); len(got) != 0 {
		t.Fatalf("attempts = %v, want none while what the node runs is unknown", got)
	}
	if len(r.Outcomes()) != 0 {
		t.Fatalf("outcomes = %+v, want none: a deferred round is not a verdict", r.Outcomes())
	}

	close(proceed)
	// The retry arrives without waiting out the interval, which is an hour.
	deadline = time.Now().Add(2 * time.Second)
	for len(rec.idents()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("a deferred round was not retried before the next interval")
		}
		time.Sleep(time.Millisecond)
	}
	deadline = time.Now().Add(2 * time.Second)
	for r.Skipped() != "" {
		if time.Now().After(deadline) {
			t.Fatalf("skipped = %q, want cleared once a card was asked", r.Skipped())
		}
		time.Sleep(time.Millisecond)
	}
}

// Only one retry: a round that still cannot tell what this node runs is
// waiting on something the next interval will not fix either, and retrying
// on a timer of its own would ask every card on a busy node.
func TestAStillDeferredRetryIsNotRetriedAgain(t *testing.T) {
	now, _ := clock(t)
	rec := &recorder{now: now}
	var mu sync.Mutex
	rounds := 0
	r := New(Config{
		Cards: func(context.Context) []Card { return cards("a") },
		Try:   rec.try,
		Deferred: func() string {
			mu.Lock()
			defer mu.Unlock()
			rounds++
			return "what this node runs is not known yet"
		},
		Interval:   time.Hour,
		FirstRound: time.Millisecond,
		RetryAfter: 5 * time.Millisecond,
		Now:        now,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Run(ctx)
	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		seen := rounds
		mu.Unlock()
		if seen >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("rounds = %d, want the scheduled round and its one retry", seen)
		}
		time.Sleep(time.Millisecond)
	}
	// Well past several more retry delays, the interval is what comes next.
	time.Sleep(100 * time.Millisecond)
	mu.Lock()
	seen := rounds
	mu.Unlock()
	if seen != 2 {
		t.Fatalf("rounds = %d, want exactly the round and one retry", seen)
	}
	if got := rec.idents(); len(got) != 0 {
		t.Fatalf("attempts = %v, want none", got)
	}
}

// A card an instance holds is left alone by a round, so a workload that
// could not start on a bad card would keep it marked healthy and be placed
// on it again. CheckNow asks anyway, because a failed start is not serving.
func TestCheckNowAsksACardAnInstanceHolds(t *testing.T) {
	now, _ := clock(t)
	rec := &recorder{fail: map[string]error{"b": errors.New("aclrtSetDevice=507033")}, now: now}
	r := New(Config{
		Cards:    func(context.Context) []Card { return cards("a", "b") },
		Try:      rec.try,
		Busy:     func(Card) bool { return true },
		Interval: time.Hour,
		Now:      now,
	})
	// The schedule asks nothing while every card is held.
	r.RunOnce(context.Background())
	if got := rec.idents(); len(got) != 0 {
		t.Fatalf("attempts = %v, want none: a held card is left alone", got)
	}

	r.CheckNow(context.Background(), map[string]bool{"b": true})
	if got := rec.idents(); len(got) != 1 || got[0] != "b" {
		t.Fatalf("attempts = %v, want only the card the instance failed on", got)
	}
	outcome, ok := r.Outcomes()["b"]
	if !ok || outcome.OK || !strings.Contains(outcome.Detail, "507033") {
		t.Fatalf("outcome = %+v ok=%v, want the vendor failure recorded", outcome, ok)
	}
	if _, asked := r.Outcomes()["a"]; asked {
		t.Fatal("a card the instance did not use was asked")
	}
}

// A workload that fails for its own reasons restarts over and over. The card
// it lands on is asked once, not once per restart.
func TestCheckNowDoesNotReaskWithinAnInterval(t *testing.T) {
	now, advance := clock(t)
	rec := &recorder{now: now}
	r := New(Config{
		Cards:    func(context.Context) []Card { return cards("a") },
		Try:      rec.try,
		Busy:     func(Card) bool { return true },
		Interval: time.Hour,
		Now:      now,
	})
	for i := 0; i < 5; i++ {
		r.CheckNow(context.Background(), map[string]bool{"a": true})
	}
	if got := rec.idents(); len(got) != 1 {
		t.Fatalf("attempts = %v, want one: an answer within the interval stands", got)
	}
	// Once the answer is older than an interval it is asked again.
	advance(time.Hour + time.Minute)
	r.CheckNow(context.Background(), map[string]bool{"a": true})
	if got := rec.idents(); len(got) != 2 {
		t.Fatalf("attempts = %v, want the stale answer refreshed", got)
	}
}

// A card that answers is not blamed for the workload's own failure.
func TestCheckNowLeavesAHealthyCardHealthy(t *testing.T) {
	now, _ := clock(t)
	rec := &recorder{now: now}
	r := New(Config{
		Cards: func(context.Context) []Card { return cards("a") },
		Try:   rec.try, Busy: func(Card) bool { return true },
		Interval: time.Hour, Now: now,
	})
	r.CheckNow(context.Background(), map[string]bool{"a": true})
	if outcome := r.Outcomes()["a"]; !outcome.OK || outcome.Failures != 0 {
		t.Fatalf("outcome = %+v, want a healthy card", outcome)
	}
}

// A host that cannot run the check at all says nothing about any card, the
// same as a round does.
func TestCheckNowSaysNothingWhenTheHostCannotBeAsked(t *testing.T) {
	now, _ := clock(t)
	rec := &recorder{now: now}
	r := New(Config{
		Cards: func(context.Context) []Card { return cards("a") },
		Try:   rec.try, Busy: func(Card) bool { return true },
		Unavailable: func() string { return "CANN is not installed on the host" },
		Interval:    time.Hour, Now: now,
	})
	r.CheckNow(context.Background(), map[string]bool{"a": true})
	if got := rec.idents(); len(got) != 0 {
		t.Fatalf("attempts = %v, want none on a host that cannot be asked", got)
	}
	if len(r.Outcomes()) != 0 {
		t.Fatalf("outcomes = %+v, want none", r.Outcomes())
	}
}

// A card somebody outside the platform is using is left alone by the round
// and by CheckNow alike, and keeps what it last answered: a context created
// next to their workload could disturb it, and the memory they hold says
// nothing about whether the runtime can use the card.
func TestForeignCardsAreNeverAsked(t *testing.T) {
	now, advance := clock(t)
	rec := &recorder{fail: map[string]error{"b": errors.New("cuCtxCreate=2")}, now: now}
	foreign := map[string]bool{}
	r := New(Config{
		Cards:    func(context.Context) []Card { return cards("a", "b") },
		Try:      rec.try,
		Foreign:  func(c Card) bool { return foreign[c.Ident] },
		Interval: time.Hour,
		Now:      now,
	})
	r.RunOnce(context.Background())
	if got := rec.idents(); len(got) != 2 {
		t.Fatalf("attempts = %v, want both cards asked while nobody else uses them", got)
	}
	failed := r.Outcomes()["b"]

	foreign["a"], foreign["b"] = true, true
	advance(2 * time.Hour)
	r.RunOnce(context.Background())
	r.CheckNow(context.Background(), map[string]bool{"a": true, "b": true})
	if got := rec.idents(); len(got) != 2 {
		t.Fatalf("attempts = %v, want no card asked while it is used outside the platform", got)
	}
	if got := r.Outcomes()["b"]; got != failed {
		t.Fatalf("outcome = %+v, want the last answer kept (%+v)", got, failed)
	}

	delete(foreign, "a")
	advance(2 * time.Hour)
	r.RunOnce(context.Background())
	if got := rec.idents(); len(got) != 3 || got[2] != "a" {
		t.Fatalf("attempts = %v, want the card asked again once it is free", got)
	}
}
