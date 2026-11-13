package batch

import (
	"context"
	"reflect"
	"sync"
	"testing"
	"time"
)

// switchModels is a model table a test changes while a batch runs.
type switchModels struct {
	mu      sync.Mutex
	states  map[string]ModelState
	lookups int
}

func (m *switchModels) set(name string, state ModelState) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.states[name] = state
}

func (m *switchModels) Lookup(name string) (ModelEntry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lookups++
	return fakeModels{states: m.states}.entry(name), nil
}

func (m *switchModels) List() ([]ModelEntry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return fakeModels{states: m.states}.List()
}

func (m *switchModels) lookupCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lookups
}

const unknownModel = `{"error":{"message":"no deployment serves model m","type":"invalid_request_error"}}`

// A model served only by a stopped deployment is refused by the gateway as
// unknown (404); the batch was accepted with a warning, so its requests wait
// for a start instead of failing, the wait board says why, and once the model
// is started they complete and the board empties.
func TestRequestsForAStoppedModelWaitForItsStart(t *testing.T) {
	models := &switchModels{states: map[string]ModelState{"m": ModelStopped}}
	var mu sync.Mutex
	serving := false
	r := newRig(t, Config{Models: models}, func(context.Context, Owner, string, []byte) Reply {
		mu.Lock()
		defer mu.Unlock()
		if serving {
			return okReply(`{"choices":[{"finish_reason":"stop"}]}`)
		}
		return Reply{Status: 404, Body: []byte(unknownModel)}
	})
	r.start()
	b := r.batchOf(3)
	if len(b.Warnings) != 1 || b.Warnings[0].Code != WarningModelWaits || b.Warnings[0].State != ModelStopped {
		t.Fatalf("warnings = %+v", b.Warnings)
	}
	eventually(t, "every request waiting", func() bool {
		w := r.svc.Waits(b.ID)
		return len(w) == 1 && w[0].Requests == 3
	})
	w := r.svc.Waits(b.ID)[0]
	if w.Model != "m" || w.Status != 404 || w.Message != "no deployment serves model m" || !w.Since.Equal(r.clock.Now()) {
		t.Fatalf("wait = %+v", w)
	}
	if st, _ := r.repo.GetBatch(b.ID); st.Failed != 0 || st.Status != StatusInProgress {
		t.Fatalf("a waiting request counted: %+v", st)
	}
	// Three refusals within the cache's life cost one lookup at most beyond
	// the create's.
	if n := models.lookupCount(); n > 2 {
		t.Fatalf("lookups = %d", n)
	}

	models.set("m", ModelRunning)
	mu.Lock()
	serving = true
	mu.Unlock()
	r.clock.Advance(time.Minute)
	done := r.await(b.ID, StatusCompleted)
	if done.Completed != 3 || done.Failed != 0 {
		t.Fatalf("batch = %+v", done)
	}
	if got := r.svc.Waits(b.ID); len(got) != 0 {
		t.Fatalf("waits after the batch = %+v", got)
	}
}

// overlapModels counts how many Lookups run at once.
type overlapModels struct {
	fakeModels
	mu  sync.Mutex
	in  int
	max int
}

func (m *overlapModels) Lookup(name string) (ModelEntry, error) {
	m.mu.Lock()
	m.in++
	if m.in > m.max {
		m.max = m.in
	}
	m.mu.Unlock()
	time.Sleep(50 * time.Millisecond)
	m.mu.Lock()
	m.in--
	m.mu.Unlock()
	return m.fakeModels.Lookup(name)
}

func (m *overlapModels) maxInFlight() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.max
}

// Three refusals that are in flight together still share one lookup.
func TestConcurrentRefusalsShareOneStoppedLookup(t *testing.T) {
	models := &overlapModels{fakeModels: fakeModels{states: map[string]ModelState{"m": ModelStopped}}}
	r := newRig(t, Config{Models: models}, func(context.Context, Owner, string, []byte) Reply {
		return Reply{Status: 404, Body: []byte(unknownModel)}
	})
	r.start()
	b := r.batchOf(3)
	eventually(t, "every request waiting", func() bool {
		w := r.svc.Waits(b.ID)
		return len(w) == 1 && w[0].Requests == 3
	})
	if n := models.maxInFlight(); n != 1 {
		t.Fatalf("overlapping lookups = %d", n)
	}
}

// A 404 for a model that is not stopped is final: the request asked for
// something the model does not serve.
func TestA404ForAServedModelFails(t *testing.T) {
	models := &switchModels{states: map[string]ModelState{"m": ModelRunning}}
	r := newRig(t, Config{Models: models}, func(context.Context, Owner, string, []byte) Reply {
		return Reply{Status: 404, Body: []byte(`{"error":{"message":"deployment m does not expose POST /v1/chat/completions"}}`)}
	})
	r.start()
	b := r.batchOf(1)
	done := r.await(b.ID, StatusCompleted)
	if done.Completed != 0 || done.Failed != 1 {
		t.Fatalf("batch = %+v", done)
	}
}

// The board counts each waiting request once however often it is retried,
// keeps the first wait's start and the last answer, and lets a model go with
// its last waiting request.
func TestWaitBoard(t *testing.T) {
	var w waitBoard
	t0 := time.Date(2026, 11, 3, 3, 0, 0, 0, time.UTC)
	w.retrying("a", true, Reply{Status: 503, Body: []byte(`{"error":{"message":"deployment for a failed to start"}}`)}, t0)
	w.retrying("a", true, Reply{Status: 503, Body: []byte(`{"error":{"message":"deployment for a failed to start"}}`)}, t0.Add(time.Second))
	w.retrying("a", false, Reply{Err: context.DeadlineExceeded}, t0.Add(time.Minute))
	w.retrying("b", true, Reply{Status: 200, Body: []byte(`{"choices":[{"finish_reason":"abort"}]}`)}, t0)
	got := w.list()
	want := []ModelWait{
		{Model: "a", Status: 0, Message: context.DeadlineExceeded.Error(), Since: t0, Requests: 2},
		{Model: "b", Status: 200, Message: "the response was cut off by a preemption (finish_reason abort)", Since: t0, Requests: 1},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("board = %+v\nwant %+v", got, want)
	}
	w.settled("a")
	w.settled("b")
	w.settled("ghost")
	if got := w.list(); len(got) != 1 || got[0].Model != "a" || got[0].Requests != 1 {
		t.Fatalf("after settling = %+v", got)
	}
	w.settled("a")
	if got := w.list(); len(got) != 0 {
		t.Fatalf("after the last = %+v", got)
	}
}

// A batch that is not running has no waits.
func TestWaitsOfABatchNotRunning(t *testing.T) {
	r := newRig(t, Config{}, nil)
	if got := r.svc.Waits("batch_000000000000000000000000"); got != nil {
		t.Fatalf("waits = %+v", got)
	}
}
