package hub

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/prepobs"
)

// setPeerRelayQueue changes how h's relay admission waits. It is set under
// the manager lock, so waiters already queued read it race-free.
func setPeerRelayQueue(h *Hub, change func(*peerRelayQueue)) {
	m := h.peerManager()
	m.mu.Lock()
	defer m.mu.Unlock()
	change(&m.queue)
}

func shortenPeerRelayQueue(h *Hub, wait time.Duration) {
	setPeerRelayQueue(h, func(q *peerRelayQueue) { q.queueWait = wait })
}

func onePeerRelaySlot(t *testing.T) *Hub {
	t.Helper()
	h := newTestHub(t)
	p := prepobs.DefaultPeerRelayPolicy()
	p.MaxActive = 1
	if err := h.SetPeerRelayPolicy(p); err != nil {
		t.Fatal(err)
	}
	return h
}

type acquired struct {
	id    string
	lease *peerRelayLease
	err   error
}

// beginQueued registers a copy and starts its admission; the result arrives
// on out once it is admitted or gives up.
func beginQueued(t *testing.T, h *Hub, id, resource, source, target string, out chan<- acquired) {
	t.Helper()
	beginQueuedOp(t, h, "copy", id, resource, source, target, out)
}

func beginQueuedOp(t *testing.T, h *Hub, op, id, resource, source, target string, out chan<- acquired) {
	t.Helper()
	ctx, finish := h.beginPeerTransfer(context.Background(), prepobs.PeerTransferSpec{TransferID: id, Kind: "model", Operation: op, ResourceKey: resource, SourceNodeUUID: source, TargetNodeUUID: target})
	// finish cancels ctx, which ends a waiter the test did not collect.
	t.Cleanup(func() { finish("canceled", "") })
	go func() {
		l, err := h.peerManager().acquire(ctx, id, source, target, 0)
		out <- acquired{id, l, err}
	}()
	waitFor(t, 2*time.Second, func() bool {
		v := h.PeerTransferSnapshotFor(id)
		return len(v.Active) == 1 && v.Active[0].Phase == "waiting"
	}, id+" waiting")
}

func TestPeerRelayAdmitsWaitersInArrivalOrder(t *testing.T) {
	h := onePeerRelaySlot(t)
	shortenPeerRelayQueue(h, 10*time.Second)
	held, err := h.peerManager().acquire(context.Background(), "held", "s0", "t0", 0)
	if err != nil {
		t.Fatal(err)
	}
	const waiters = 6
	out := make(chan acquired, waiters)
	for i := range waiters {
		id := fmt.Sprintf("w%d", i)
		beginQueued(t, h, id, "model-"+id, "s-"+id, "t-"+id, out)
	}
	if v := h.PeerTransferSnapshotFor("w0"); v.Active[0].Reason != "capacity_busy" {
		t.Fatalf("waiting reason = %q", v.Active[0].Reason)
	}
	// Every release wakes all waiters at once; only the earliest may win.
	held.release()
	for i := range waiters {
		got := <-out
		if got.err != nil || got.id != fmt.Sprintf("w%d", i) {
			t.Fatalf("admission %d = %+v, want w%d", i, got, i)
		}
		select {
		case early := <-out:
			t.Fatalf("%s admitted while %s holds the only slot", early.id, got.id)
		case <-time.After(30 * time.Millisecond):
		}
		got.lease.release()
	}
}

// A retry carries a new transfer ID but the same task; it keeps the place its
// first attempt took, ahead of a task that queued after it.
func TestPeerRelayRetryKeepsItsPlaceInQueue(t *testing.T) {
	h := onePeerRelaySlot(t)
	shortenPeerRelayQueue(h, 300*time.Millisecond)
	held, err := h.peerManager().acquire(context.Background(), "held", "s0", "t0", 0)
	if err != nil {
		t.Fatal(err)
	}
	out := make(chan acquired, 3)
	beginQueued(t, h, "a-attempt-1", "model-a", "s1", "t1", out)
	if got := <-out; !errors.Is(got.err, messages.ErrPeerRelayBusy) {
		t.Fatalf("queue wait did not end in busy: %+v", got)
	}
	shortenPeerRelayQueue(h, 5*time.Second)
	beginQueued(t, h, "b-attempt-1", "model-b", "s2", "t2", out)
	beginQueued(t, h, "a-attempt-2", "model-a", "s1", "t1", out)
	held.release()
	if got := <-out; got.err != nil || got.id != "a-attempt-2" {
		t.Fatalf("admitted %+v, want the retry of the earlier task", got)
	}
}

// A waiter whose nodes are busy does not hold back one whose nodes are free.
func TestPeerRelayQueueHasNoHeadOfLineBlockingAcrossNodes(t *testing.T) {
	h := newTestHub(t) // two slots, one per node
	shortenPeerRelayQueue(h, 5*time.Second)
	held, err := h.peerManager().acquire(context.Background(), "held", "busy-source", "t0", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer held.release()
	out := make(chan acquired, 2)
	beginQueued(t, h, "blocked", "model-a", "busy-source", "t1", out)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	free, err := h.peerManager().acquire(ctx, "free", "s2", "t2", 0)
	if err != nil {
		t.Fatalf("a waiter on a busy node blocked unrelated nodes: %v", err)
	}
	free.release()
}

func TestPeerRelayQueueWaitEndsOnCancel(t *testing.T) {
	h := onePeerRelaySlot(t)
	shortenPeerRelayQueue(h, time.Minute)
	held, err := h.peerManager().acquire(context.Background(), "held", "s0", "t0", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer held.release()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := h.peerManager().acquire(ctx, "w", "s1", "t1", 0); done <- err }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled wait returned %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancel did not end the queue wait")
	}
	m := h.peerManager()
	m.mu.Lock()
	defer m.mu.Unlock()
	for key, ticket := range m.tickets {
		if ticket.active != 0 {
			t.Fatalf("ticket %q still counts a canceled waiter", key)
		}
	}
}

// Two slots free at once wake every waiter together. When the later waiter
// rechecks first it stands aside for the earlier one; the earlier one's
// admission must wake it again, or it waits out its deadline beside a free
// slot and returns busy.
func TestPeerRelayWaiterStandingAsideIsWokenWhenTheEarlierOneIsAdmitted(t *testing.T) {
	for round := range 20 {
		h := newTestHub(t)
		p := prepobs.DefaultPeerRelayPolicy()
		p.MaxActive = 3
		p.QuantumBytes = p.BurstBytes / 8
		if err := h.SetPeerRelayPolicy(p); err != nil {
			t.Fatal(err)
		}
		shortenPeerRelayQueue(h, 2*time.Second)
		var held []*peerRelayLease
		for i := range 3 {
			l, err := h.peerManager().acquire(context.Background(), fmt.Sprintf("h%d", i), fmt.Sprintf("hs%d", i), fmt.Sprintf("ht%d", i), 0)
			if err != nil {
				t.Fatal(err)
			}
			held = append(held, l)
		}
		out := make(chan acquired, 2)
		beginQueued(t, h, "a", "model-a", "sa", "ta", out)
		beginQueued(t, h, "b", "model-b", "sb", "tb", out)
		held[0].release()
		held[1].release()
		for range 2 {
			select {
			case got := <-out:
				if got.err != nil {
					t.Fatalf("round %d: %s not admitted beside a free slot: %v", round, got.id, got.err)
				}
				got.lease.release()
			case <-time.After(time.Second):
				t.Fatalf("round %d: a waiter slept beside a free slot", round)
			}
		}
		held[2].release()
	}
}

// Between a task's retries it holds no admission, but an arrival in that gap
// must not take the slot ahead of it.
func TestPeerRelayRetryGapDoesNotLoseTheSlotToANewArrival(t *testing.T) {
	h := onePeerRelaySlot(t)
	setPeerRelayQueue(h, func(q *peerRelayQueue) { q.queueWait, q.retryHold = 200*time.Millisecond, time.Second })
	held, err := h.peerManager().acquire(context.Background(), "held", "s0", "t0", 0)
	if err != nil {
		t.Fatal(err)
	}
	out := make(chan acquired, 3)
	beginQueued(t, h, "a-attempt-1", "model-a", "s1", "t1", out)
	if got := <-out; !errors.Is(got.err, messages.ErrPeerRelayBusy) {
		t.Fatalf("first wait = %+v, want busy", got)
	}
	// a is now between retries. The slot frees and a newcomer asks first.
	held.release()
	shortenPeerRelayQueue(h, 3*time.Second)
	beginQueued(t, h, "late", "model-late", "s2", "t2", out)
	time.Sleep(100 * time.Millisecond) // a's re-listen and redial
	ctx, finish := h.beginPeerTransfer(context.Background(), prepobs.PeerTransferSpec{TransferID: "a-attempt-2", Kind: "model", Operation: "copy", ResourceKey: "model-a", SourceNodeUUID: "s1", TargetNodeUUID: "t1"})
	defer finish("canceled", "")
	go func() {
		l, err := h.peerManager().acquire(ctx, "a-attempt-2", "s1", "t1", 0)
		out <- acquired{"a-attempt-2", l, err}
	}()
	got := <-out
	if got.err != nil || got.id != "a-attempt-2" {
		t.Fatalf("admitted %+v, want the retrying task ahead of the newcomer", got)
	}
	got.lease.release()
	if next := <-out; next.err != nil || next.id != "late" {
		t.Fatalf("newcomer = %+v, want admitted after", next)
	}
}

// A probe answers an interactive request; it gives up well before a copy.
func TestPeerRelayProbeWaitsBrieflyForASlot(t *testing.T) {
	h := onePeerRelaySlot(t)
	setPeerRelayQueue(h, func(q *peerRelayQueue) { q.queueWait, q.probeWait = time.Minute, 100*time.Millisecond })
	held, err := h.peerManager().acquire(context.Background(), "held", "s0", "t0", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer held.release()
	out := make(chan acquired, 1)
	start := time.Now()
	beginQueuedOp(t, h, "probe", "p", "model-p", "s1", "t1", out)
	select {
	case got := <-out:
		if !errors.Is(got.err, messages.ErrPeerRelayBusy) {
			t.Fatalf("probe = %+v, want busy", got)
		}
		if waited := time.Since(start); waited > 2*time.Second {
			t.Fatalf("probe waited %v", waited)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("probe waited like a copy")
	}
}

// Two admissions with one transfer ID must not both hold a lease: the second
// would overwrite the first in the lease table and leak its node counts.
func TestPeerRelaySameTransferIDIsAdmittedOnce(t *testing.T) {
	h := onePeerRelaySlot(t)
	shortenPeerRelayQueue(h, 3*time.Second)
	held, err := h.peerManager().acquire(context.Background(), "held", "s0", "t0", 0)
	if err != nil {
		t.Fatal(err)
	}
	out := make(chan acquired, 2)
	for range 2 {
		go func() {
			l, err := h.peerManager().acquire(context.Background(), "dup", "s1", "t1", 0)
			out <- acquired{"dup", l, err}
		}()
	}
	time.Sleep(100 * time.Millisecond)
	held.release()
	first, second := <-out, <-out
	if (first.err == nil) == (second.err == nil) {
		t.Fatalf("one admission expected: %v / %v", first.err, second.err)
	}
	for _, a := range []acquired{first, second} {
		if a.lease != nil {
			a.lease.release()
		}
	}
	m := h.peerManager()
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.leases) != 0 || len(m.nodes) != 0 {
		t.Fatalf("leaked leases=%d nodes=%v", len(m.leases), m.nodes)
	}
}
