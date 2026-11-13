package hub

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// nodeQueueCapacity bounds the distinct nodes with one pending report. Every
// payload routed through a nodeQueue is a full snapshot of that node's
// reported fact, so the queue keeps only the newest per node: a node
// reporting twice before the worker catches up costs one persistence call,
// not two. Past capacity the oldest pending node is dropped — that node's
// next periodic report supersedes it, which is always better than blocking
// the session read loop.
const nodeQueueCapacity = 8192

// nodeQueue decouples per-node persistence from the session read loop.
// Status reports, heartbeats and image-presence reports are all transactional
// writes that under a fleet-wide storm serialize behind the store's single
// writer for hundreds of milliseconds each; running them on the read loop
// back-pressures the whole ctrl stream, so heartbeats and RPC replies queue
// behind database work and management RPCs blow their deadlines (measured as
// the 2026-09-21 l2-fleet regression). The read loop only submits here; one
// worker per queue applies reports serially, which is exactly the store's
// own concurrency anyway. Reports that are deltas rather than snapshots take
// the session's ordered worker instead (session_reports.go).
type nodeQueue[P any] struct {
	log      *slog.Logger
	apply    func(nodeUUID string, payload P) error
	capacity int
	// applyBatch, when set, replaces apply: the worker hands it up to
	// batchMax pending reports at once, so a fleet's reports share one
	// store transaction instead of each queuing for the writer alone.
	applyBatch func(map[string]P) error

	mu      sync.Mutex
	pending map[string]P
	order   []string // submission order of pending keys, oldest first
	notify  chan struct{}

	droppedNodes uint64
}

func newNodeQueue[P any](log *slog.Logger, apply func(nodeUUID string, payload P) error) *nodeQueue[P] {
	return &nodeQueue[P]{
		log:      log,
		apply:    apply,
		capacity: nodeQueueCapacity,
		pending:  map[string]P{},
		notify:   make(chan struct{}, 1),
	}
}

// submit records payload as the node's newest pending snapshot and wakes the
// worker. It never blocks and never allocates unboundedly: one entry per
// node, capacity-bounded across nodes.
func (q *nodeQueue[P]) submit(nodeUUID string, payload P) {
	q.mu.Lock()
	if _, ok := q.pending[nodeUUID]; !ok {
		if len(q.order) >= q.capacity {
			oldest := q.order[0]
			q.order = q.order[1:]
			delete(q.pending, oldest)
			q.droppedNodes++
			q.warnDropLocked()
		}
		q.order = append(q.order, nodeUUID)
	}
	q.pending[nodeUUID] = payload
	q.mu.Unlock()
	select {
	case q.notify <- struct{}{}:
	default:
	}
}

// warnDropLocked stays cheap: one line per 1024 evictions regardless of how
// many nodes a storm drops.
func (q *nodeQueue[P]) warnDropLocked() {
	if q.droppedNodes%1024 != 1 {
		return
	}
	q.log.Warn("node report queue at capacity; dropped a node's pending report (its next periodic report supersedes it)",
		"dropped_total", q.droppedNodes)
}

// run applies pending reports until ctx ends. Reports apply in submission
// order so different nodes make progress fairly; within one node the newest
// snapshot always wins.
func (q *nodeQueue[P]) run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-q.notify:
		}
		for {
			if q.applyBatch != nil {
				batch := q.popBatch(nodeQueueBatchMax)
				if len(batch) == 0 {
					break
				}
				if err := q.applyBatch(batch); err != nil {
					q.log.Warn("apply node reports failed", "nodes", len(batch), "err", err)
				}
			} else {
				nodeUUID, payload, ok := q.pop()
				if !ok {
					break
				}
				if err := q.apply(nodeUUID, payload); err != nil {
					q.log.Warn("apply node report failed", "node", nodeUUID, "err", err)
				}
			}
			select {
			case <-ctx.Done():
				return
			default:
			}
		}
	}
}

// forget drops the node's pending report, if any.
func (q *nodeQueue[P]) forget(nodeUUID string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if _, ok := q.pending[nodeUUID]; !ok {
		return
	}
	delete(q.pending, nodeUUID)
	for i, key := range q.order {
		if key == nodeUUID {
			q.order = append(q.order[:i], q.order[i+1:]...)
			break
		}
	}
}

// nodeQueueBatchMax bounds one batch: large enough that a whole fleet's
// heartbeats take a handful of transactions, small enough that one never
// holds the store's writer for long.
const nodeQueueBatchMax = 256

// popBatch removes and returns up to max of the oldest pending reports.
func (q *nodeQueue[P]) popBatch(max int) map[string]P {
	q.mu.Lock()
	defer q.mu.Unlock()
	n := min(max, len(q.order))
	if n == 0 {
		return nil
	}
	out := make(map[string]P, n)
	for _, nodeUUID := range q.order[:n] {
		out[nodeUUID] = q.pending[nodeUUID]
		delete(q.pending, nodeUUID)
	}
	q.order = q.order[n:]
	return out
}

// pop removes and returns the oldest pending report.
func (q *nodeQueue[P]) pop() (string, P, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.order) == 0 {
		var zero P
		return "", zero, false
	}
	nodeUUID := q.order[0]
	q.order = q.order[1:]
	payload := q.pending[nodeUUID]
	delete(q.pending, nodeUUID)
	return nodeUUID, payload, true
}

// The queues differ only in payload and persistence call. They are separate
// instances so a slow status transaction never delays a heartbeat, and each
// drains in submission order.

func newStatusQueue(h *Hub) *nodeQueue[queuedNodeStatus] {
	return newNodeQueue(h.log, h.applyQueuedNodeStatus)
}

// newTouchQueue persists heartbeats without the session fence: a heartbeat
// only moves last_seen_at to a time the server really received one, which
// stays true after the session ends, and it never changes the node's state.
func newTouchQueue(log *slog.Logger, st *store.Store) *nodeQueue[time.Time] {
	q := newNodeQueue[time.Time](log, nil)
	q.applyBatch = st.TouchNodesAt
	return q
}

// queuedStorageStatus is a storage report waiting in the coalescing queue,
// with the session that received it for the current-session fence.
type queuedStorageStatus struct {
	report  proto.StorageStatusReport
	session *Session
}

func newStorageQueue(h *Hub) *nodeQueue[queuedStorageStatus] {
	return newNodeQueue(h.log, func(_ string, queued queuedStorageStatus) error {
		_, err := queued.session.applyCurrent(func(nodeID uint) error {
			return h.store.ApplyStorageStatus(nodeID, queued.report)
		})
		return err
	})
}

// handleHeartbeat records the liveness timestamp and queues the persisted
// bump: a fleet's heartbeats must never queue behind the store on the read
// loop.
func (s *Session) handleHeartbeat() {
	sawHeartbeat := time.Now()
	s.lastSeen.Store(sawHeartbeat.UnixMilli())
	s.hub.touchQ.submit(s.NodeUUID, sawHeartbeat.UTC())
}

// handleStatusNode queues the report for the status queue: a report storm
// must not back-pressure heartbeats and RPC replies behind store writes.
func (s *Session) handleStatusNode(raw json.RawMessage) {
	var st proto.NodeStatus
	if err := json.Unmarshal(raw, &st); err != nil {
		s.hub.log.Warn("bad status.node", "node", s.NodeUUID, "err", err)
		return
	}
	s.nvidiaIsolation.Store(st.NVIDIAIsolation)
	if st.RuntimeProxy != nil {
		s.runtimeProxy.Store(st.RuntimeProxy)
	}
	// Every report, in memory: the card checks are never queued behind the
	// store, and a report the queue coalesces away still counts.
	if s.hub.onCardChecks != nil {
		s.hub.onCardChecks(s.NodeUUID, s.nodeID, s.hub.now(), st.GPUs)
	}
	s.hub.statusQ.submit(s.NodeUUID, queuedNodeStatus{
		status: st, session: s, imageRevision: s.imageStatusRevision.Load(),
	})
}
