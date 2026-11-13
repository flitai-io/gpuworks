package app

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/logstore"
)

func newTestLines(instance string, n int, startMS int64) []proto.LogLine {
	lines := make([]proto.LogLine, 0, n)
	for i := 0; i < n; i++ {
		lines = append(lines, proto.LogLine{
			Instance: instance, TimeMS: startMS + int64(i),
			Line: "line " + instance, Level: "info", Source: "instance",
		})
	}
	return lines
}

// TestLogIngestFlushesToStore pins the happy path through the queue: lines
// ingested from a hub callback become searchable via the batcher without the
// caller ever touching the store.
func TestLogIngestFlushesToStore(t *testing.T) {
	st, err := logstore.OpenSQLite(filepath.Join(t.TempDir(), "logs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	o := &observability{logs: st, logQueue: make(chan []logstore.Entry, logIngestQueueChunks)}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go o.logIngestLoop(ctx)

	o.ingestLogs("node-1", newTestLines("inst-a", 5, time.Now().UnixMilli()))

	deadline := time.Now().Add(3 * time.Second)
	for {
		res, err := st.Search(logstore.Filter{Instance: "inst-a", Limit: 10})
		if err != nil {
			t.Fatalf("search: %v", err)
		}
		if len(res.Entries) == 5 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("queued lines never reached the store: %d rows", len(res.Entries))
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// blockingStore stalls Append until released, to prove ingestLogs stays
// non-blocking when the store does not keep up.
type blockingStore struct {
	release chan struct{}
	appends chan []logstore.Entry // every Append attempt is recorded
}

func (b *blockingStore) Append(entries []logstore.Entry) error {
	cp := make([]logstore.Entry, len(entries))
	copy(cp, entries)
	b.appends <- cp
	<-b.release
	return nil
}
func (b *blockingStore) Search(logstore.Filter) (logstore.Result, error) {
	return logstore.Result{}, nil
}
func (b *blockingStore) Tail(string, int) ([]logstore.Entry, error) { return nil, nil }
func (b *blockingStore) Context(_, _ string, _, _ int64, _, _ int) ([]logstore.Entry, int, error) {
	return nil, 0, nil
}
func (b *blockingStore) Histogram(logstore.Filter, int64) ([]logstore.Bucket, error) {
	return nil, nil
}
func (b *blockingStore) Retain(context.Context, int64) error     { return nil }
func (b *blockingStore) Checkpoint() (bool, int64, int64, error) { return false, 0, 0, nil }
func (b *blockingStore) Close() error                            { return nil }

// TestLogIngestNeverBlocksCallerWhenStoreStalls is the regression the queue
// exists for: with the store wedged, a hub ctrl loop calling ingestLogs must
// still return immediately — heartbeats ride the same loop — and the backlog
// beyond the queue bound must be dropped with a counter, not buffered
// unboundedly.
func TestLogIngestNeverBlocksCallerWhenStoreStalls(t *testing.T) {
	block := &blockingStore{release: make(chan struct{}), appends: make(chan []logstore.Entry, 1024)}
	o := &observability{logs: block, logQueue: make(chan []logstore.Entry, 8)}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go o.logIngestLoop(ctx)

	// Overwhelm the queue: the batcher is stuck in Append on the first chunk,
	// so the queue fills and every later chunk must drop fast.
	for i := 0; i < 64; i++ {
		start := time.Now()
		o.ingestLogs("node-1", newTestLines("inst-a", 8, time.Now().UnixMilli()))
		if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
			t.Fatalf("ingestLogs blocked for %s behind a stalled store; the ctrl loop would stall with it", elapsed)
		}
	}
	if o.logDropped.Load() == 0 {
		t.Fatal("overflow beyond the queue bound was not counted as dropped")
	}

	// Unblock and stop: the loop must drain what it holds and exit.
	close(block.release)
	cancel()
	// Nothing to assert beyond "no deadlock"; give the loop a moment.
	time.Sleep(150 * time.Millisecond)
	if o.logDropped.Load() == 0 {
		t.Fatal("drop counter reset unexpectedly")
	}
}

// captureStore records appended batches for the final-flush assertion.
type captureStore struct {
	mu      sync.Mutex
	appends [][]logstore.Entry
}

func (c *captureStore) Append(entries []logstore.Entry) error {
	cp := make([]logstore.Entry, len(entries))
	copy(cp, entries)
	c.mu.Lock()
	c.appends = append(c.appends, cp)
	c.mu.Unlock()
	return nil
}
func (c *captureStore) rows() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, b := range c.appends {
		n += len(b)
	}
	return n
}
func (c *captureStore) Search(logstore.Filter) (logstore.Result, error) {
	return logstore.Result{}, nil
}
func (c *captureStore) Tail(string, int) ([]logstore.Entry, error) { return nil, nil }
func (c *captureStore) Context(_, _ string, _, _ int64, _, _ int) ([]logstore.Entry, int, error) {
	return nil, 0, nil
}
func (c *captureStore) Histogram(logstore.Filter, int64) ([]logstore.Bucket, error) {
	return nil, nil
}
func (c *captureStore) Retain(context.Context, int64) error     { return nil }
func (c *captureStore) Checkpoint() (bool, int64, int64, error) { return false, 0, 0, nil }
func (c *captureStore) Close() error                            { return nil }

// TestLogIngestFinalFlushOnCancel: shutdown must not lose lines already
// accepted from agents.
func TestLogIngestFinalFlushOnCancel(t *testing.T) {
	cap := &captureStore{}
	o := &observability{logs: cap, logQueue: make(chan []logstore.Entry, logIngestQueueChunks)}
	ctx, cancel := context.WithCancel(context.Background())
	go o.logIngestLoop(ctx)

	o.ingestLogs("node-1", newTestLines("inst-a", 10, time.Now().UnixMilli()))
	// The flush interval is 200ms; cancel before it fires.
	cancel()

	deadline := time.Now().Add(2 * time.Second)
	for cap.rows() < 10 {
		if time.Now().After(deadline) {
			t.Fatalf("final flush lost queued lines: %d of 10 rows appended", cap.rows())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// The telemetry stream's waiting sink keeps every line a full queue would
// drop: it waits for room, and gives up (counted) only when its context ends.
func TestLogIngestWaitHoldsTheBatchUntilThereIsRoom(t *testing.T) {
	block := &blockingStore{release: make(chan struct{}), appends: make(chan []logstore.Entry, 1024)}
	o := &observability{logs: block, logQueue: make(chan []logstore.Entry, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go o.logIngestLoop(ctx)

	// Feed chunks until one waits: the batcher wedges in Append and the
	// queue fills behind it.
	var waited chan error
	for i := 0; waited == nil; i++ {
		if i == 64 {
			t.Fatal("ingestLogsWait never waited behind a wedged store")
		}
		call := make(chan error, 1)
		// A full flush's worth of rows sends the batcher straight into Append.
		go func() {
			call <- o.ingestLogsWait(ctx, "node-1", newTestLines("inst-a", logIngestFlushRows, time.Now().UnixMilli()))
		}()
		select {
		case err := <-call:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(100 * time.Millisecond):
			waited = call
		}
	}
	close(block.release)
	select {
	case err := <-waited:
		if err != nil {
			t.Fatalf("ingestLogsWait = %v after room opened", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("ingestLogsWait never got room")
	}
	if o.logDropped.Load() != 0 {
		t.Fatalf("dropped %d lines while waiting", o.logDropped.Load())
	}

	full := &observability{logs: block, logQueue: make(chan []logstore.Entry)}
	stop, cancelStop := context.WithCancel(context.Background())
	cancelStop()
	if err := full.ingestLogsWait(stop, "node-1", newTestLines("inst-a", 3, time.Now().UnixMilli())); err == nil {
		t.Fatal("ingestLogsWait succeeded after its context ended with no room")
	}
	if full.logDropped.Load() != 3 {
		t.Fatalf("dropped %d lines on a canceled wait, want 3", full.logDropped.Load())
	}
}
