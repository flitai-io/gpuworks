package app

import (
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
)

// logSink records what a bound connection sent and can be made to fail.
type logSink struct {
	mu      sync.Mutex
	batches []proto.LogBatch
	fail    error
}

func (s *logSink) send(t string, d any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail != nil {
		return s.fail
	}
	if t == proto.TLogs {
		s.batches = append(s.batches, d.(proto.LogBatch))
	}
	return nil
}

func (s *logSink) setFail(err error) {
	s.mu.Lock()
	s.fail = err
	s.mu.Unlock()
}

func (s *logSink) lines() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, b := range s.batches {
		for _, l := range b.Lines {
			out = append(out, l.Line)
		}
	}
	return out
}

func (s *logSink) snapshot() []proto.LogBatch {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]proto.LogBatch(nil), s.batches...)
}

// waitBatches waits for Bind's background flush to deliver n batches.
func (s *logSink) waitBatches(t *testing.T, n int) []proto.LogBatch {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		got := s.snapshot()
		if len(got) >= n || time.Now().After(deadline) {
			return got
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func quietNode() *NodeRuntime {
	return &NodeRuntime{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

func batchOf(instance string, timeMS int64, lines ...string) proto.LogBatch {
	b := proto.LogBatch{}
	for _, l := range lines {
		b.Lines = append(b.Lines, proto.LogLine{Instance: instance, Source: "instance", TimeMS: timeMS, Line: l})
	}
	return b
}

// Lines emitted while the node has no connection are held and go out, in
// order, as soon as one is bound.
func TestLogsEmittedOfflineAreSentInOrderOnReconnect(t *testing.T) {
	n := quietNode()
	n.emit(proto.TLogs, batchOf("inst-1", 1, "a", "b"))
	n.emit(proto.TLogs, batchOf("inst-1", 2, "c"))
	sink := &logSink{}
	n.Bind(sink.send)
	sink.waitBatches(t, 2)
	if got := strings.Join(sink.lines(), ","); got != "a,b,c" {
		t.Fatalf("sent %q after reconnect, want a,b,c", got)
	}
	n.emit(proto.TLogs, batchOf("inst-1", 3, "d"))
	if got := strings.Join(sink.lines(), ","); got != "a,b,c,d" {
		t.Fatalf("sent %q once online, want a,b,c,d", got)
	}
	if n.logOutbox().pending() != 0 {
		t.Fatal("sent batches were still held")
	}
}

// A failed send keeps the batch and everything after it; the retry loop
// sends them once sends succeed again, without repeating what already went.
func TestLogsHeldWhileSendsFailAndRetriedInOrder(t *testing.T) {
	n := quietNode()
	sink := &logSink{}
	n.Bind(sink.send)
	n.emit(proto.TLogs, batchOf("inst-1", 1, "sent"))
	sink.setFail(errors.New("data lane detached"))
	n.emit(proto.TLogs, batchOf("inst-1", 2, "held-1"))
	n.emit(proto.TLogs, batchOf("inst-1", 3, "held-2"))
	if n.logOutbox().pending() != 2 {
		t.Fatalf("held %d batches, want 2", n.logOutbox().pending())
	}
	sink.setFail(nil)
	ctx := t.Context()
	go n.retryLogs(ctx, 5*time.Millisecond)
	deadline := time.Now().Add(2 * time.Second)
	for n.logOutbox().pending() > 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := strings.Join(sink.lines(), ","); got != "sent,held-1,held-2" {
		t.Fatalf("sent %q, want sent,held-1,held-2", got)
	}
}

// A full buffer drops its oldest batches; the next flush sends one marker
// per instance and source describing them, ahead of the surviving lines.
func TestFullLogBufferDropsOldestAndReportsTheGap(t *testing.T) {
	n := quietNode()
	one := logBatchBytes(batchOf("inst-1", 0, "line-x"))
	n.logOut = newLogOutbox(3 * one)
	n.emit(proto.TLogs, batchOf("inst-1", 1000, "line-1"))
	n.emit(proto.TLogs, batchOf("inst-2", 2000, "line-2"))
	n.emit(proto.TLogs, batchOf("inst-1", 3000, "line-3"))
	n.emit(proto.TLogs, batchOf("inst-1", 4000, "line-4"))
	n.emit(proto.TLogs, batchOf("inst-1", 5000, "line-5"))

	sink := &logSink{}
	n.Bind(sink.send)
	batches := sink.waitBatches(t, 4)
	if len(batches) != 4 {
		t.Fatalf("sent %d batches, want a marker batch and three survivors: %+v", len(batches), batches)
	}
	marker := batches[0].Lines
	if len(marker) != 2 {
		t.Fatalf("marker lines = %+v, want one per dropped instance", marker)
	}
	want := []struct {
		instance string
		count    string
		timeMS   int64
	}{{"inst-1", "1 log lines", 1000}, {"inst-2", "1 log lines", 2000}}
	for i, w := range want {
		m := marker[i]
		if m.Instance != w.instance || m.Source != "instance" || m.Level != "warn" || m.TimeMS != w.timeMS || !strings.Contains(m.Line, w.count) || !strings.Contains(m.Line, "dropped") {
			t.Fatalf("marker %d = %+v, want %s %q at %d", i, m, w.instance, w.count, w.timeMS)
		}
	}
	if got := strings.Join(sink.lines()[2:], ","); got != "line-3,line-4,line-5" {
		t.Fatalf("survivors = %q, want the three newest in order", got)
	}
}

// A marker that fails to send is kept and merged with later drops of the
// same instance, so no drop goes unreported.
func TestUnsentDropMarkerIsMergedWithLaterDrops(t *testing.T) {
	n := quietNode()
	one := logBatchBytes(batchOf("inst-1", 0, "line-x"))
	n.logOut = newLogOutbox(one)
	n.emit(proto.TLogs, batchOf("inst-1", 1000, "line-1"))
	n.emit(proto.TLogs, batchOf("inst-1", 2000, "line-2"))
	failing := &logSink{fail: errors.New("detached")}
	n.logOutbox().flush(failing.send, nil)
	n.emit(proto.TLogs, batchOf("inst-1", 3000, "line-3"))

	sink := &logSink{}
	n.Bind(sink.send)
	batches := sink.waitBatches(t, 2)
	if len(batches) != 2 || len(batches[0].Lines) != 1 {
		t.Fatalf("sent %+v, want one merged marker then line-3", batches)
	}
	m := batches[0].Lines[0]
	if !strings.HasPrefix(m.Line, "gpuworks: 2 log lines from 1970-01-01T00:00:01Z to 1970-01-01T00:00:02Z were dropped") {
		t.Fatalf("merged marker = %q", m.Line)
	}
	if got := batches[1].Lines[0].Line; got != "line-3" {
		t.Fatalf("after the marker sent %q, want line-3", got)
	}
}

// The batch on the wire is never the one an overflow drops: it may already
// have reached the server, and counting it as dropped would be a lie.
func TestOverflowSparesTheBatchBeingSent(t *testing.T) {
	one := logBatchBytes(batchOf("inst-1", 0, "line-x"))
	o := newLogOutbox(2 * one)
	o.enqueue(batchOf("inst-1", 1, "line-w"))
	head, ok := o.takeHead()
	if !ok {
		t.Fatal("no head")
	}
	o.enqueue(batchOf("inst-1", 2, "line-2"))
	o.enqueue(batchOf("inst-1", 3, "line-3"))
	o.finishHead(head.seq, true)
	if o.pending() != 1 || o.queue[0].batch.Lines[0].Line != "line-3" {
		t.Fatalf("queue after overflow = %+v, want only line-3", o.queue)
	}
	if run := o.dropped[logDropKey{"inst-1", "instance"}]; run == nil || run.lines != 1 || run.firstMS != 2 {
		t.Fatalf("dropped = %+v, want the second batch", o.dropped)
	}
}

// While a send is blocked on the link, an emitter waits rather than
// returning, so a slow link still backpressures the log followers.
func TestLogEmitterWaitsBehindABlockedSend(t *testing.T) {
	n := quietNode()
	release := make(chan struct{})
	entered := make(chan struct{}, 4)
	n.Bind(func(t string, d any) error {
		entered <- struct{}{}
		<-release
		return nil
	})
	go n.emit(proto.TLogs, batchOf("inst-1", 1, "first"))
	<-entered
	returned := make(chan struct{})
	go func() {
		n.emit(proto.TLogs, batchOf("inst-1", 2, "second"))
		close(returned)
	}()
	select {
	case <-returned:
		t.Fatal("an emitter returned while the link was blocked")
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	select {
	case <-returned:
	case <-time.After(2 * time.Second):
		t.Fatal("the emitter never returned after the link drained")
	}
}

// Metric batches stay best-effort: dropped while offline, never held.
func TestMetricsAreNotHeldOffline(t *testing.T) {
	n := quietNode()
	n.emit(proto.TMetrics, proto.MetricBatch{Samples: []proto.MetricSample{{Metric: "m"}}})
	var got []string
	n.Bind(func(t string, d any) error { got = append(got, t); return nil })
	if len(got) != 0 {
		t.Fatalf("sent %v after reconnect, want nothing", got)
	}
}

// Bind hands a backlog to the background: the connection's own loops start
// at once even when the link holds the first send.
func TestBindDoesNotWaitForTheLogBacklog(t *testing.T) {
	n := quietNode()
	n.emit(proto.TLogs, batchOf("inst-1", 1, "held"))
	release := make(chan struct{})
	defer close(release)
	bound := make(chan struct{})
	go func() {
		n.Bind(func(string, any) error { <-release; return nil })
		close(bound)
	}()
	select {
	case <-bound:
	case <-time.After(time.Second):
		t.Fatal("Bind waited for the log backlog to drain")
	}
}

// One flush pass sends what was queued when it began; batches queued while
// it runs are not its to drain, so no sender waits on an endless stream of
// other sources' logs.
func TestFlushPassStopsAtTheBatchesQueuedWhenItBegan(t *testing.T) {
	o := newLogOutbox(1 << 20)
	o.enqueue(batchOf("inst-1", 1, "before-1"))
	o.enqueue(batchOf("inst-1", 2, "before-2"))
	sink := &logSink{}
	sent := 0
	o.flush(func(t string, d any) error {
		sent++
		if sent == 1 {
			o.enqueue(batchOf("inst-1", 3, "during"))
		}
		return sink.send(t, d)
	}, nil)
	if got := strings.Join(sink.lines(), ","); got != "before-1,before-2" {
		t.Fatalf("first pass sent %q, want only the two batches queued before it", got)
	}
	if o.pending() != 1 {
		t.Fatalf("held %d batches after the pass, want the one queued during it", o.pending())
	}
	o.flush(sink.send, nil)
	if got := strings.Join(sink.lines(), ","); got != "before-1,before-2,during" {
		t.Fatalf("second pass left %q", got)
	}
}

// A drop marker can outlive an empty queue: a failed send restores the
// marker while the batches behind it have already gone out. The retry loop
// still delivers it.
func TestRetrySendsADropMarkerWithNothingElseQueued(t *testing.T) {
	n := quietNode()
	one := logBatchBytes(batchOf("inst-1", 0, "line-x"))
	n.logOut = newLogOutbox(one)
	n.logOut.enqueue(batchOf("inst-1", 1000, "line-1"))
	n.logOut.enqueue(batchOf("inst-1", 2000, "line-2")) // drops line-1
	n.logOut.mu.Lock()
	n.logOut.queue = nil // line-2 went out on an earlier connection
	n.logOut.bytes = 0
	n.logOut.mu.Unlock()
	if n.logOutbox().pending() != 0 || !n.logOutbox().hasWork() {
		t.Fatal("setup: want an empty queue with a pending drop marker")
	}
	sink := &logSink{}
	// Bound directly: Bind's own background flush could deliver the marker
	// and hide whether the retry loop does.
	n.sendMu.Lock()
	n.send = sink.send
	n.sendMu.Unlock()
	go n.retryLogs(t.Context(), 5*time.Millisecond)
	batches := sink.waitBatches(t, 1)
	if len(batches) != 1 || !strings.Contains(batches[0].Lines[0].Line, "were dropped") {
		t.Fatalf("sent %+v, want the drop marker", batches)
	}
}

// One batch larger than the whole bound is sent as it is on a healthy link,
// not dropped: the bound is for the backlog an outage builds up.
func TestABatchLargerThanTheBoundIsSentWhole(t *testing.T) {
	n := quietNode()
	n.logOut = newLogOutbox(1 << 10)
	sink := &logSink{}
	n.sendMu.Lock()
	n.send = sink.send
	n.sendMu.Unlock()
	n.emit(proto.TLogs, batchOf("inst-1", 1, strings.Repeat("x", 4<<10)))
	batches := sink.snapshot()
	if len(batches) != 1 || len(batches[0].Lines) != 1 || len(batches[0].Lines[0].Line) != 4<<10 {
		t.Fatalf("sent %d batches, want the oversized batch itself", len(batches))
	}
	if n.logOutbox().hasWork() {
		t.Fatal("a drop was recorded for a batch that was sent")
	}
}
