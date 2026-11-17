package app

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/conn"
	"gpuworks.ai/gpuworks/internal/proto"
)

// Log batches the node cannot send (no connection, or a data lane that has
// not attached yet or is down) wait here and go out in order once it
// can. The buffer is bounded; when it is full the oldest batches go, and a
// marker line per instance and source reports what was dropped, so the log
// center shows a gap instead of silently missing it. The buffer lives in
// memory: an agent restart loses what it held.
const (
	logOutboxMaxBytes = 16 << 20
	// logLineWireOverhead approximates one line's JSON beyond its text:
	// field names, instance UUID, timestamp, level and source.
	logLineWireOverhead = 96
	// logRetryInterval resends held batches while the node is connected but
	// its sends fail (the data lane has not attached yet or is reconnecting).
	logRetryInterval = 2 * time.Second
)

func (n *NodeRuntime) logOutbox() *logOutbox {
	n.logOutOnce.Do(func() {
		if n.logOut == nil {
			n.logOut = newLogOutbox(logOutboxMaxBytes)
		}
	})
	return n.logOut
}

// flushLogs sends held log batches on the bound connection, if any.
func (n *NodeRuntime) flushLogs() {
	n.sendMu.Lock()
	send := n.send
	n.sendMu.Unlock()
	if send == nil {
		return
	}
	n.logOutbox().flush(send, n.log)
}

// retryLogs resends held log batches while the node stays connected but its
// sends fail, until ctx ends.
func (n *NodeRuntime) retryLogs(ctx context.Context, every time.Duration) {
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if n.logOutbox().hasWork() {
				n.flushLogs()
			}
		}
	}
}

type logDropKey struct{ instance, source string }

type logDropRun struct {
	lines           int
	firstMS, lastMS int64
}

type queuedLogBatch struct {
	seq   uint64
	batch proto.LogBatch
	bytes int
}

// logOutbox is a FIFO of log batches with a byte bound.
type logOutbox struct {
	max int

	mu       sync.Mutex
	queue    []queuedLogBatch
	bytes    int
	nextSeq  uint64
	inflight uint64 // seq of the head batch being sent; 0 when none
	dropped  map[logDropKey]*logDropRun

	// flushMu serializes senders. A sender holds it while its send blocks on
	// the link, so emitters wait behind it and backpressure reaches the
	// container log pipes exactly as when they sent directly.
	flushMu sync.Mutex
	failing bool // guarded by flushMu: the last flush ended on a send error
}

func newLogOutbox(max int) *logOutbox {
	return &logOutbox{max: max, dropped: map[logDropKey]*logDropRun{}}
}

func logBatchBytes(b proto.LogBatch) int {
	total := 0
	for _, line := range b.Lines {
		total += len(line.Line) + logLineWireOverhead
	}
	return total
}

func (o *logOutbox) enqueue(b proto.LogBatch) {
	if len(b.Lines) == 0 {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	o.nextSeq++
	q := queuedLogBatch{seq: o.nextSeq, batch: b, bytes: logBatchBytes(b)}
	o.queue = append(o.queue, q)
	o.bytes += q.bytes
	for o.bytes > o.max {
		// The batch being sent may already be on the wire; drop the next
		// oldest instead so the drop count stays honest. The batch just
		// queued is never dropped by its own arrival: one batch larger than
		// the whole bound (64 long lines) still goes out on a healthy link
		// unless newer batches arrive before it does, and the bound holds
		// again once it has gone.
		victim := 0
		if o.inflight != 0 && o.queue[0].seq == o.inflight {
			victim = 1
		}
		if victim >= len(o.queue)-1 {
			break
		}
		o.dropLocked(o.queue[victim])
		o.queue = append(o.queue[:victim], o.queue[victim+1:]...)
	}
}

func (o *logOutbox) dropLocked(q queuedLogBatch) {
	o.bytes -= q.bytes
	for _, line := range q.batch.Lines {
		key := logDropKey{instance: line.Instance, source: line.Source}
		run := o.dropped[key]
		if run == nil {
			run = &logDropRun{firstMS: line.TimeMS}
			o.dropped[key] = run
		}
		run.lines++
		run.firstMS = min(run.firstMS, line.TimeMS)
		run.lastMS = max(run.lastMS, line.TimeMS)
	}
}

// pending reports how many batches are held, for tests and diagnostics.
func (o *logOutbox) pending() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.queue)
}

// hasWork reports whether a flush has anything to send: held batches or a
// drop marker (which can outlive an empty queue).
func (o *logOutbox) hasWork() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.queue) > 0 || len(o.dropped) > 0
}

// flush sends held batches in order until it has sent those queued when it
// was called, the queue is empty, or a send fails; batches queued during the
// pass are left to their own emitter's flush, so a sender never waits on an
// endless stream of others' logs. Drop markers go first, so they land next to
// the gap they describe rather than after the lines that followed it.
func (o *logOutbox) flush(send conn.SendFunc, log *slog.Logger) {
	o.flushMu.Lock()
	defer o.flushMu.Unlock()
	o.mu.Lock()
	var last uint64
	if len(o.queue) > 0 {
		last = o.queue[len(o.queue)-1].seq
	}
	o.mu.Unlock()
	for {
		if marker, ok := o.takeMarker(); ok {
			if err := send(proto.TLogs, marker.batch); err != nil {
				o.restoreMarker(marker.runs)
				o.noteFailure(log, err)
				return
			}
			continue
		}
		head, ok := o.takeHead()
		if !ok || head.seq > last {
			o.inflightDone()
			o.failing = false
			return
		}
		err := send(proto.TLogs, head.batch)
		o.finishHead(head.seq, err == nil)
		if err != nil {
			o.noteFailure(log, err)
			return
		}
	}
}

func (o *logOutbox) noteFailure(log *slog.Logger, err error) {
	if !o.failing && log != nil {
		log.Warn("log batches held until the server can receive them", "err", err)
	}
	o.failing = true
}

type logMarker struct {
	batch proto.LogBatch
	runs  map[logDropKey]*logDropRun
}

func (o *logOutbox) takeMarker() (logMarker, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(o.dropped) == 0 {
		return logMarker{}, false
	}
	runs := o.dropped
	o.dropped = map[logDropKey]*logDropRun{}
	keys := make([]logDropKey, 0, len(runs))
	for key := range runs {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].instance != keys[j].instance {
			return keys[i].instance < keys[j].instance
		}
		return keys[i].source < keys[j].source
	})
	lines := make([]proto.LogLine, 0, len(keys))
	for _, key := range keys {
		run := runs[key]
		lines = append(lines, proto.LogLine{
			Instance: key.instance,
			Source:   key.source,
			Level:    "warn",
			TimeMS:   run.lastMS,
			Line: fmt.Sprintf("gpuworks: %d log lines from %s to %s were dropped: the agent could not reach the server and its %d MiB log buffer was full",
				run.lines, time.UnixMilli(run.firstMS).UTC().Format(time.RFC3339), time.UnixMilli(run.lastMS).UTC().Format(time.RFC3339), o.max>>20),
		})
	}
	return logMarker{batch: proto.LogBatch{Lines: lines}, runs: runs}, true
}

func (o *logOutbox) restoreMarker(runs map[logDropKey]*logDropRun) {
	o.mu.Lock()
	defer o.mu.Unlock()
	for key, run := range runs {
		if cur := o.dropped[key]; cur != nil {
			cur.lines += run.lines
			cur.firstMS = min(cur.firstMS, run.firstMS)
			cur.lastMS = max(cur.lastMS, run.lastMS)
			continue
		}
		o.dropped[key] = run
	}
}

func (o *logOutbox) takeHead() (queuedLogBatch, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(o.queue) == 0 {
		return queuedLogBatch{}, false
	}
	o.inflight = o.queue[0].seq
	return o.queue[0], true
}

// inflightDone clears a head taken but not sent.
func (o *logOutbox) inflightDone() {
	o.mu.Lock()
	o.inflight = 0
	o.mu.Unlock()
}

func (o *logOutbox) finishHead(seq uint64, sent bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.inflight = 0
	if sent && len(o.queue) > 0 && o.queue[0].seq == seq {
		o.bytes -= o.queue[0].bytes
		o.queue = o.queue[1:]
	}
}
