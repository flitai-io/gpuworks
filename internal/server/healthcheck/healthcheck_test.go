package healthcheck

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"
)

type fakeSource struct {
	mu      sync.Mutex
	targets []Target
	err     error
}

func (s *fakeSource) Targets(context.Context) ([]Target, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Target(nil), s.targets...), s.err
}

type sent struct {
	id      uint
	path    string
	payload map[string]any
}

// fakeSender answers each deployment as told, advancing the clock by the
// reply's time; a deployment with a gate blocks until the gate closes or the
// check gives up.
type fakeSender struct {
	mu      sync.Mutex
	clock   *fakeClock
	replies map[uint]Reply
	took    map[uint]time.Duration
	gates   map[uint]chan struct{}
	sent    []sent
}

func (f *fakeSender) Probe(ctx context.Context, id uint, path string, payload map[string]any) Reply {
	f.mu.Lock()
	f.sent = append(f.sent, sent{id, path, payload})
	gate, reply, took := f.gates[id], f.replies[id], f.took[id]
	f.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return Reply{Message: ctx.Err().Error()}
		}
	}
	f.clock.advance(took)
	return reply
}

func (f *fakeSender) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sent)
}

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func newChecker(src Source, send *fakeSender) *Checker {
	c := New(src, send)
	c.now = send.clock.now
	// One check at a time: they share the clock their replies advance.
	c.concurrency = 1
	return c
}

func round(c *Checker) {
	c.Start(context.Background())
	c.Wait()
}

func TestChecksAreTheSmallestRequestOfTheirKind(t *testing.T) {
	chat, _ := json.Marshal(Payload(KindChat))
	if string(chat) != `{"max_tokens":1,"messages":[{"content":"1","role":"user"}],"stream":false}` {
		t.Errorf("chat payload = %s", chat)
	}
	emb, _ := json.Marshal(Payload(KindEmbeddings))
	if string(emb) != `{"input":"1"}` {
		t.Errorf("embeddings payload = %s", emb)
	}
	completion, _ := json.Marshal(Payload(KindCompletions))
	if string(completion) != `{"max_tokens":1,"prompt":"1","stream":false}` {
		t.Errorf("completions payload = %s", completion)
	}
	// One yes/no question: Intern-Decision's validate_request takes a state
	// and 1-16 questions, and a noul question needs no criteria.
	decision, _ := json.Marshal(Payload(KindDecision))
	if string(decision) != `{"questions":{"q":{"instructions":"Is the state a number?","type":"noul"}},"state":"1"}` {
		t.Errorf("decision payload = %s", decision)
	}
}

func TestEachOutcomeIsRecordedWithItsTimeAndReplica(t *testing.T) {
	clock := &fakeClock{t: time.UnixMilli(1_000_000)}
	send := &fakeSender{clock: clock,
		replies: map[uint]Reply{
			1: {HTTPStatus: 200, InstanceUUID: "i-1"},
			2: {HTTPStatus: 500, InstanceUUID: "i-2", Message: "engine error"},
			3: {HTTPStatus: 200, InstanceUUID: "i-3"},
			4: {NoReplica: true},
			6: {HTTPStatus: 0, Message: "dial: connection refused"},
		},
		took: map[uint]time.Duration{1: 120 * time.Millisecond, 2: 30 * time.Millisecond, 3: 5 * time.Second},
	}
	src := &fakeSource{targets: []Target{
		{DeploymentID: 1, Kind: KindChat, Path: "/v1/chat/completions", Threshold: time.Minute, Active: true},
		{DeploymentID: 2, Kind: KindChat, Path: "/v1/chat/completions", Threshold: time.Minute, Active: true},
		// Answered, but after the threshold: a timeout at the threshold.
		{DeploymentID: 3, Kind: KindEmbeddings, Path: "/v1/embeddings", Threshold: 2 * time.Second, Active: true},
		{DeploymentID: 4, Kind: KindChat, Path: "/v1/chat/completions", Threshold: time.Minute, Active: true},
		// Stopped: nothing is sent.
		{DeploymentID: 5, Kind: KindChat, Path: "/v1/chat/completions", Threshold: time.Minute},
		{DeploymentID: 6, Kind: KindChat, Path: "/v1/chat/completions", Threshold: time.Minute, Active: true},
	}}
	c := newChecker(src, send)
	round(c)

	if got := c.Health(1); got.Status != StatusOK || len(got.Recent) != 1 || got.Recent[0] != (Probe{AtMS: got.Recent[0].AtMS, Status: StatusOK, LatencyMS: 120, HTTPStatus: 200, InstanceUUID: "i-1"}) {
		t.Errorf("ok: %+v", got)
	}
	if got, _ := c.Health(2).Last(); got.Status != StatusFailed || got.HTTPStatus != 500 || got.Message != "engine error" || got.InstanceUUID != "i-2" {
		t.Errorf("failed: %+v", got)
	}
	if got, _ := c.Health(3).Last(); got.Status != StatusTimeout || got.LatencyMS != 2000 {
		t.Errorf("timeout: %+v", got)
	}
	if got := c.Health(4); got.Status != StatusNoReplica || len(got.Recent) != 0 {
		t.Errorf("no replica: %+v", got)
	}
	if got := c.Health(5); got.Status != StatusNoReplica || got.Threshold != time.Minute {
		t.Errorf("stopped: %+v", got)
	}
	if got, _ := c.Health(6).Last(); got.Status != StatusFailed || got.HTTPStatus != 0 || got.Message != "dial: connection refused" {
		t.Errorf("unreachable: %+v", got)
	}
	if got := c.Health(99); got.Status != StatusOff {
		t.Errorf("unlisted: %+v", got)
	}
	for _, s := range send.sent {
		if s.id == 5 {
			t.Error("a stopped deployment was sent a check")
		}
		if s.id == 3 && s.path != "/v1/embeddings" {
			t.Errorf("embeddings check posted to %s", s.path)
		}
	}
}

func TestATimedOutCheckGivesUpAtTheThreshold(t *testing.T) {
	clock := &fakeClock{t: time.UnixMilli(0)}
	send := &fakeSender{clock: clock, gates: map[uint]chan struct{}{1: make(chan struct{})}}
	src := &fakeSource{targets: []Target{{DeploymentID: 1, Kind: KindChat, Threshold: 20 * time.Millisecond, Active: true}}}
	c := newChecker(src, send)
	round(c)
	if got, _ := c.Health(1).Last(); got.Status != StatusTimeout || got.LatencyMS != 20 {
		t.Fatalf("got %+v", got)
	}
}

func TestRecentKeepsTheLastTenAndTheMeanCountsOnlyAnswers(t *testing.T) {
	clock := &fakeClock{t: time.UnixMilli(0)}
	send := &fakeSender{clock: clock, replies: map[uint]Reply{1: {HTTPStatus: 200}}, took: map[uint]time.Duration{}}
	src := &fakeSource{targets: []Target{{DeploymentID: 1, Kind: KindChat, Threshold: time.Minute, Active: true}}}
	c := newChecker(src, send)
	for i := 1; i <= 12; i++ {
		send.mu.Lock()
		send.took[1] = time.Duration(i*100) * time.Millisecond
		if i == 12 {
			send.replies[1] = Reply{HTTPStatus: 503}
		}
		send.mu.Unlock()
		round(c)
	}
	h := c.Health(1)
	if len(h.Recent) != RecentSize || h.Recent[0].LatencyMS != 300 || h.Status != StatusFailed {
		t.Fatalf("recent = %+v status %s", h.Recent, h.Status)
	}
	// 300..1100 answered: their mean; the failed 1200 is not in it.
	if ms, n := h.MeanOK(); ms != 700 || n != 9 {
		t.Errorf("mean = %d over %d", ms, n)
	}
}

func TestACheckStillWaitingIsNotSentAgainAndTurningOffForgets(t *testing.T) {
	clock := &fakeClock{t: time.UnixMilli(0)}
	gate := make(chan struct{})
	send := &fakeSender{clock: clock, replies: map[uint]Reply{1: {HTTPStatus: 200}}, gates: map[uint]chan struct{}{1: gate}}
	src := &fakeSource{targets: []Target{{DeploymentID: 1, Kind: KindChat, Threshold: time.Minute, Active: true}}}
	c := newChecker(src, send)
	c.Start(context.Background())
	for send.count() == 0 {
		time.Sleep(time.Millisecond)
	}
	if got := c.Health(1); got.Status != StatusPending {
		t.Errorf("before the first reply: %+v", got)
	}
	c.Start(context.Background())
	close(gate)
	c.Wait()
	if send.count() != 1 {
		t.Fatalf("sent %d checks while one was waiting", send.count())
	}
	if got := c.Health(1); got.Status != StatusOK {
		t.Fatalf("after the reply: %+v", got)
	}

	src.mu.Lock()
	src.targets = nil
	src.mu.Unlock()
	round(c)
	if got := c.Health(1); got.Status != StatusOff || len(got.Recent) != 0 {
		t.Errorf("turned off: %+v", got)
	}
}

func TestAStoppingServerRecordsNothingAndAListingErrorKeepsTheLastHealth(t *testing.T) {
	clock := &fakeClock{t: time.UnixMilli(0)}
	send := &fakeSender{clock: clock, replies: map[uint]Reply{1: {HTTPStatus: 200}}, gates: map[uint]chan struct{}{}}
	src := &fakeSource{targets: []Target{{DeploymentID: 1, Kind: KindChat, Threshold: time.Minute, Active: true}}}
	c := newChecker(src, send)
	round(c)

	send.mu.Lock()
	send.gates[1] = make(chan struct{})
	send.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()
	for send.count() < 2 {
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-done
	if h := c.Health(1); h.Status != StatusOK || len(h.Recent) != 1 {
		t.Fatalf("a check cut short by shutdown was recorded: %+v", h)
	}

	src.mu.Lock()
	src.err = errors.New("database is locked")
	src.mu.Unlock()
	round(c)
	if h := c.Health(1); h.Status != StatusOK || len(h.Recent) != 1 {
		t.Errorf("a failed listing changed the health: %+v", h)
	}
}
