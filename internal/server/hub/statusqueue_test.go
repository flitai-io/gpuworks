package hub

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func quietLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// A node reporting twice before the worker catches up persists one report:
// the newest snapshot. The stale one is not merely delayed — it is gone, so
// the store never sees facts the agent already replaced.
func TestStatusQueueCoalescesPerNode(t *testing.T) {
	var mu sync.Mutex
	applied := 0
	hostname := ""
	block := make(chan struct{})
	q := newNodeQueue[proto.NodeStatus](quietLog(), func(uuid string, st proto.NodeStatus) error {
		<-block
		mu.Lock()
		defer mu.Unlock()
		applied++
		hostname = st.Sys.Hostname
		return nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Both reports land before the worker starts: started first, a loaded
	// machine let it take "old" before "new" was submitted, and the test saw
	// two applies.
	q.submit("node-1", proto.NodeStatus{Sys: proto.SysInfo{Hostname: "old"}})
	q.submit("node-1", proto.NodeStatus{Sys: proto.SysInfo{Hostname: "new"}})
	go q.run(ctx)
	close(block)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		done := applied == 1 && hostname == "new"
		mu.Unlock()
		if done {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("applied=%d hostname=%q, want exactly one apply of the newest snapshot", applied, hostname)
}

// Distinct nodes are persisted in submission order, so a storm spreads its
// fairness across nodes instead of letting one hot node starve the rest.
func TestStatusQueueAppliesDistinctNodesInOrder(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	q := newNodeQueue[proto.NodeStatus](quietLog(), func(uuid string, st proto.NodeStatus) error {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, uuid)
		return nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go q.run(ctx)

	for _, name := range []string{"a", "b", "c"} {
		q.submit(name, proto.NodeStatus{})
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		got := append([]string(nil), calls...)
		mu.Unlock()
		if len(got) == 3 {
			if got[0] != "a" || got[1] != "b" || got[2] != "c" {
				t.Fatalf("applied = %v, want submission order", got)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("applied = %v, want all three in submission order", calls)
}

// Past capacity the oldest pending node's report is dropped — the read loop
// is never blocked and the submission that made room stays queued. Tested
// without the worker so pending state is observable directly.
func TestStatusQueueEvictsOldestNodeAtCapacity(t *testing.T) {
	q := newNodeQueue[proto.NodeStatus](quietLog(), func(string, proto.NodeStatus) error { return nil })
	q.capacity = 2

	q.submit("oldest", proto.NodeStatus{Sys: proto.SysInfo{Hostname: "first"}})
	q.submit("middle", proto.NodeStatus{Sys: proto.SysInfo{Hostname: "second"}})
	q.submit("newest", proto.NodeStatus{Sys: proto.SysInfo{Hostname: "third"}})

	if len(q.order) != 2 || q.order[0] != "middle" || q.order[1] != "newest" {
		t.Fatalf("order = %v, want [middle newest]", q.order)
	}
	if _, ok := q.pending["oldest"]; ok {
		t.Fatal("the oldest node's report was not evicted at capacity")
	}
	if q.pending["newest"].Sys.Hostname != "third" {
		t.Fatalf("newest snapshot missing: %+v", q.pending["newest"])
	}
	if q.droppedNodes != 1 {
		t.Fatalf("droppedNodes = %d, want 1", q.droppedNodes)
	}
}

// A coalescing update keeps the node's position in the queue: it cannot jump
// ahead of nodes that reported earlier.
func TestStatusQueueCoalesceKeepsQueuePosition(t *testing.T) {
	q := newNodeQueue[proto.NodeStatus](quietLog(), func(string, proto.NodeStatus) error { return nil })
	q.submit("a", proto.NodeStatus{})
	q.submit("b", proto.NodeStatus{})
	q.submit("a", proto.NodeStatus{}) // refresh, must not reorder
	if len(q.order) != 2 || q.order[0] != "a" || q.order[1] != "b" {
		t.Fatalf("order = %v, want [a b] with a keeping its position", q.order)
	}
}

// A failing apply is logged and swallowed; later reports still persist.
func TestStatusQueueSurvivesApplyErrors(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	q := newNodeQueue[proto.NodeStatus](quietLog(), func(uuid string, st proto.NodeStatus) error {
		mu.Lock()
		defer mu.Unlock()
		calls++
		if calls == 1 {
			return errors.New("store unavailable")
		}
		return nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go q.run(ctx)

	q.submit("node-1", proto.NodeStatus{})
	q.submit("node-2", proto.NodeStatus{})
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		done := calls >= 2
		mu.Unlock()
		if done {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("calls = %d, want the worker to continue past a failing apply", calls)
}

// The touch queue keeps the newest heartbeat per node: coalescing must never
// persist an older observation over a newer one.
func TestTouchQueueKeepsNewestHeartbeat(t *testing.T) {
	var mu sync.Mutex
	applied := time.Time{}
	q := newNodeQueue[time.Time](quietLog(), func(uuid string, at time.Time) error {
		mu.Lock()
		defer mu.Unlock()
		applied = at
		return nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go q.run(ctx)

	newer := time.Now().Add(time.Hour)
	q.submit("node-1", time.Now())
	q.submit("node-1", newer)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		done := applied.Equal(newer)
		mu.Unlock()
		if done {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("applied = %v, want the newest heartbeat %v", applied, newer)
}

// The initial replay gates incremental updates: a PushUpdate arriving while
// the replay is still building is buffered, then discarded once the replay
// completes (the replay's snapshot is newer).
func TestPushUpdateBuffersBehindInitialReplay(t *testing.T) {
	s := &Session{
		hub:             &Hub{log: quietLog()},
		initialPushDone: make(chan struct{}),
	}
	sent := make(chan string, 4)
	s.sendFn = func(t2 string, _ uint64, d any) error { sent <- t2; return nil }

	if err := s.PushUpdate(proto.StateUpdate{}); err != nil {
		t.Fatalf("buffered update: %v", err)
	}
	select {
	case got := <-sent:
		t.Fatalf("update sent before the replay completed: %s", got)
	default:
	}
	// Completing the replay with baseline 1 discards the buffered update: it
	// is older than the replay's snapshot, and re-sending it would walk
	// state backwards.
	s.finishInitialPush(1)
	select {
	case extra := <-sent:
		t.Fatalf("stale buffered update was sent after the replay: %s", extra)
	default:
	}
}

// Updates buffered after the replay's store read are newer than its snapshot
// and must still be delivered after it, in order.
func TestPushUpdateNewerThanReplayIsFlushed(t *testing.T) {
	s := &Session{
		hub:             &Hub{log: quietLog()},
		initialPushDone: make(chan struct{}),
	}
	sent := make(chan string, 4)
	s.sendFn = func(t2 string, _ uint64, d any) error { sent <- t2; return nil }

	s.finishInitialPush(0) // replay done, nothing buffered
	if err := s.PushUpdate(proto.StateUpdate{}); err != nil {
		t.Fatal(err)
	}
	if got := <-sent; got != proto.TStateUpdate {
		t.Fatalf("sent %q, want an immediate state update", got)
	}
}

// Once the replay finished, updates send immediately again.
func TestPushUpdateSendsAfterReplayDone(t *testing.T) {
	s := &Session{
		hub:             &Hub{log: quietLog()},
		initialPushDone: make(chan struct{}),
	}
	close(s.initialPushDone)
	sent := make(chan string, 1)
	s.sendFn = func(t2 string, _ uint64, d any) error { sent <- t2; return nil }
	if err := s.PushUpdate(proto.StateUpdate{}); err != nil {
		t.Fatal(err)
	}
	if got := <-sent; got != proto.TStateUpdate {
		t.Fatalf("sent %q, want an immediate state update", got)
	}
}

// A status report still queued when its session ends must not mark the node
// online again: deregister forgets it, and a report applied for a node with
// no session is skipped.
func TestQueuedStatusDoesNotReviveDeregisteredNode(t *testing.T) {
	h := newTestHub(t)
	uuid, _ := newTestNode(t, h.store)
	sess := &Session{hub: h, NodeUUID: uuid, closed: make(chan struct{})}
	h.register(sess)

	h.statusQ.submit(uuid, queuedNodeStatus{status: proto.NodeStatus{Sys: proto.SysInfo{Hostname: "late"}}, session: sess})
	h.deregister(sess)
	if _, _, ok := h.statusQ.pop(); ok {
		t.Fatal("deregister left the node's queued status behind")
	}
	// One popped before deregister and applied after it is skipped too.
	if err := h.applyQueuedNodeStatus(uuid, queuedNodeStatus{status: proto.NodeStatus{Sys: proto.SysInfo{Hostname: "late"}}, session: sess}); err != nil {
		t.Fatal(err)
	}
	n, err := h.store.NodeByUUID(uuid)
	if err != nil {
		t.Fatal(err)
	}
	if n.State != store.NodeStateOffline || n.Hostname == "late" {
		t.Fatalf("node state=%q hostname=%q, want offline and the late report unapplied", n.State, n.Hostname)
	}

	// While the session is live the report applies.
	h.register(sess)
	if err := h.applyQueuedNodeStatus(uuid, queuedNodeStatus{status: proto.NodeStatus{Sys: proto.SysInfo{Hostname: "live"}}, session: sess}); err != nil {
		t.Fatal(err)
	}
	if n, _ = h.store.NodeByUUID(uuid); n.State != store.NodeStateOnline || n.Hostname != "live" {
		t.Fatalf("node state=%q hostname=%q, want online with the live report", n.State, n.Hostname)
	}
}

// forget removes only the named node and keeps the others in order.
func TestNodeQueueForgetKeepsOrder(t *testing.T) {
	q := newNodeQueue[int](quietLog(), func(string, int) error { return nil })
	q.submit("a", 1)
	q.submit("b", 2)
	q.submit("c", 3)
	q.forget("b")
	q.forget("missing")
	var got []string
	for {
		uuid, _, ok := q.pop()
		if !ok {
			break
		}
		got = append(got, uuid)
	}
	if len(got) != 2 || got[0] != "a" || got[1] != "c" {
		t.Fatalf("popped %v, want [a c]", got)
	}
}

// A replay whose session closes while it waits for a limiter slot gives up
// without reading the store, and releases buffered updates.
func TestInitialReplayAbandonedWhenSessionCloses(t *testing.T) {
	h := newTestHub(t)
	h.pushLimiter = make(chan struct{}, 1)
	h.pushLimiter <- struct{}{} // every slot busy
	var sent []string
	sess := &Session{hub: h, NodeUUID: "gone", closed: make(chan struct{}), initialPushDone: make(chan struct{}),
		sendFn: func(t string, _ uint64, _ any) error { sent = append(sent, t); return nil }}
	if err := sess.PushUpdate(proto.StateUpdate{RemovedInstances: []string{"x"}}); err != nil {
		t.Fatal(err)
	}
	close(sess.closed)
	if err := sess.pushInitialState(); !errors.Is(err, errSessionClosed) {
		t.Fatalf("err = %v, want errSessionClosed", err)
	}
	if !sess.initialPushFinished() {
		t.Fatal("abandoned replay left updates gated")
	}
	if len(sent) != 1 || sent[0] != proto.TStateUpdate {
		t.Fatalf("sent %v, want the buffered update flushed (no snapshot reached the agent)", sent)
	}
}

// A batch queue hands its worker many nodes' reports at once, newest per
// node, in bounded batches.
func TestNodeQueueAppliesInBatches(t *testing.T) {
	var mu sync.Mutex
	var batches []map[string]int
	q := newNodeQueue[int](quietLog(), nil)
	q.applyBatch = func(batch map[string]int) error {
		mu.Lock()
		defer mu.Unlock()
		batches = append(batches, batch)
		return nil
	}
	for i := 0; i < nodeQueueBatchMax+10; i++ {
		q.submit(fmt.Sprintf("n%d", i), i)
	}
	q.submit("n0", -1) // coalesces into n0's pending entry
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go q.run(ctx)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(batches)
		mu.Unlock()
		if n == 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(batches) != 2 || len(batches[0]) != nodeQueueBatchMax || len(batches[1]) != 10 {
		t.Fatalf("batches = %d (sizes %v)", len(batches), func() []int {
			var s []int
			for _, b := range batches {
				s = append(s, len(b))
			}
			return s
		}())
	}
	if batches[0]["n0"] != -1 {
		t.Fatalf("n0 = %d, want the newest report", batches[0]["n0"])
	}
}

// The heartbeat queue persists a fleet's heartbeats through the store, each
// node at the time it was observed.
func TestTouchQueuePersistsHeartbeatsInBatches(t *testing.T) {
	h := newTestHub(t)
	a, _ := newTestNode(t, h.store)
	b, _ := newTestNode(t, h.store)
	at := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	if err := h.touchQ.applyBatch(map[string]time.Time{a: at, b: at.Add(time.Second)}); err != nil {
		t.Fatal(err)
	}
	for uuid, want := range map[string]time.Time{a: at, b: at.Add(time.Second)} {
		n, err := h.store.NodeByUUID(uuid)
		if err != nil {
			t.Fatal(err)
		}
		if n.LastSeenAt == nil || !n.LastSeenAt.Equal(want) {
			t.Fatalf("%s last_seen = %v, want %v", uuid, n.LastSeenAt, want)
		}
	}
}
