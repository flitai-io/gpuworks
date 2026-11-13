package hub

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
	"testing/synctest"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/prepobs"
)

func TestPeerTransferObservationRequiresRegisteredCurrentSource(t *testing.T) {
	source := &Session{NodeUUID: "source"}
	target := &Session{NodeUUID: "target"}
	h := &Hub{sessions: map[string]*Session{"source": source, "target": target}}
	frame := proto.PeerTransferPhase{TransferID: "id", Transport: "direct", Phase: "connected"}
	if h.recordPeerTransferPhase(source, frame) {
		t.Fatal("unregistered frame created operation")
	}
	ctx, finish := h.beginPeerTransfer(context.Background(), prepobs.PeerTransferSpec{TransferID: "id", SourceNodeUUID: "source", TargetNodeUUID: "target", Operation: "copy"})
	defer finish("peer_confirmed", "")
	if !h.recordPeerTransferPhase(source, frame) {
		t.Fatal("registered source rejected")
	}
	if h.recordPeerTransferPhase(target, frame) {
		t.Fatal("other session updated transfer")
	}
	h.mu.Lock()
	h.sessions["source"] = &Session{NodeUUID: "source"}
	h.mu.Unlock()
	if h.recordPeerTransferPhase(source, frame) {
		t.Fatal("old source session accepted")
	}
	selected := h.PeerTransferSnapshotFor("id")
	if len(selected.Active) != 1 || selected.Active[0].Phase != "connected" || !selected.Active[0].LastProgressAt.IsZero() {
		t.Fatalf("snapshot=%+v", selected)
	}
	h.CancelPeerTransfer("id")
	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatal("owner cancel not propagated")
	}
}
func TestPeerTransferSnapshotSelectionAndOutcomeCounters(t *testing.T) {
	h := &Hub{}
	_, finishA := h.beginPeerTransfer(context.Background(), prepobs.PeerTransferSpec{TransferID: "a", Operation: "copy"})
	_, finishB := h.beginPeerTransfer(context.Background(), prepobs.PeerTransferSpec{TransferID: "b", Operation: "probe"})
	h.updatePeerTransfer("a", "direct", "confirming", "", "direct_dial_failed")
	got := h.PeerTransferSnapshotFor("a")
	if len(got.Active) != 1 || got.Active[0].TransferID != "a" || got.ActiveCount != 0 || got.Totals.Started != 1 || got.Totals.ProbeStarted != 1 {
		t.Fatalf("selected=%+v", got)
	}
	if got := h.PeerTransferSnapshotFor("absent"); len(got.Active) != 0 || got.Totals.Started != 1 {
		t.Fatalf("empty selection lost process counters: %+v", got)
	}
	finishA("peer_confirmed", "")
	finishA("failed", "duplicate")
	finishB("probe_ok", "")
	got = h.PeerTransferSnapshot()
	if len(got.Active) != 0 || got.Totals.PeerConfirmed != 1 || got.Totals.ProbeOK != 1 || got.Totals.Failed != 0 {
		t.Fatalf("outcomes=%+v", got)
	}
}
func TestPeerTransferObservationCapacityDoesNotRejectWork(t *testing.T) {
	h := &Hub{}
	m := h.peerManager()
	for i := 0; i < maxPeerObservations; i++ {
		m.entries[string(rune(i))] = &peerTransfer{}
	}
	ctx, finish := h.beginPeerTransfer(context.Background(), prepobs.PeerTransferSpec{TransferID: "overflow", Operation: "copy"})
	if ctx.Err() != nil || len(m.entries) != maxPeerObservations {
		t.Fatal("metadata bound changed functional work")
	}
	finish("peer_confirmed", "")
	if m.totals.PeerConfirmed != 1 {
		t.Fatal("unobserved completion lost process total")
	}
}
func TestPeerRelayPhaseFrameCannotStartConfirmationDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		source := &Session{NodeUUID: "s"}
		h := &Hub{sessions: map[string]*Session{"s": source}}
		ctx, finish := h.beginPeerTransfer(context.Background(), prepobs.PeerTransferSpec{TransferID: "id", SourceNodeUUID: "s", TargetNodeUUID: "t"})
		defer finish("canceled", "")
		p := testRelayPolicy()
		p.NoProgressTimeout = 10 * time.Second
		p.ConfirmationTimeout = time.Second
		if err := h.SetPeerRelayPolicy(p); err != nil {
			t.Fatal(err)
		}
		l, err := h.peerManager().acquire(ctx, "id", "s", "t", 0)
		if err != nil {
			t.Fatal(err)
		}
		defer l.release()
		if !h.recordPeerTransferPhase(source, proto.PeerTransferPhase{TransferID: "id", Transport: "relay", Phase: "confirming"}) {
			t.Fatal("source phase rejected")
		}
		time.Sleep(2 * time.Second)
		if l.ctx.Err() != nil || l.confirming {
			t.Fatal("source phase started confirmation before drain")
		}
		permit, err := l.permit(1)
		if err != nil {
			t.Fatal(err)
		}
		l.complete(permit, 1, true)
		if got := h.PeerTransferSnapshotFor("id").Active[0].Phase; got != "forwarding" {
			t.Fatalf("actual server write did not advance old-agent phase: %s", got)
		}
		l.sourceDrained()
		permit, err = l.permit(1)
		if err != nil {
			t.Fatal(err)
		}
		l.complete(permit, 1, false)
		if got := h.PeerTransferSnapshotFor("id").Active[0].Phase; got != "confirming" {
			t.Fatalf("ACK write regressed confirmation: %s", got)
		}
		time.Sleep(time.Second)
		synctest.Wait()
		if context.Cause(l.ctx) == nil || context.Cause(l.ctx).Error() != "peer relay confirmation_timeout" {
			t.Fatalf("confirmation timeout lost: %v", context.Cause(l.ctx))
		}
	})
}
func TestPeerRelayNoProgressAndConfirmationDeadlines(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := testRelayPolicy()
		p.NoProgressTimeout = time.Second
		p.ConfirmationTimeout = 2 * time.Second
		m := newPeerRelayManager(p)
		idle := acquireTestLease(t, m, "idle", 0)
		time.Sleep(time.Second)
		synctest.Wait()
		if context.Cause(idle.ctx) == nil || context.Cause(idle.ctx).Error() != "peer relay no_progress_timeout" {
			t.Fatalf("idle=%v", context.Cause(idle.ctx))
		}
		idle.release()
		l := acquireTestLease(t, m, "confirm", 0)
		l.sourceDrained()
		time.Sleep(time.Second)
		// Receiver output cannot indefinitely postpone the bounded final ACK wait.
		r, err := l.permit(1)
		if err != nil {
			t.Fatal(err)
		}
		l.complete(r, 1, false)
		time.Sleep(time.Second)
		synctest.Wait()
		if context.Cause(l.ctx) == nil || context.Cause(l.ctx).Error() != "peer relay confirmation_timeout" {
			t.Fatalf("confirmation=%v", context.Cause(l.ctx))
		}
		l.release()
	})
}
func TestPeerRelayOwnQoSWaitUsesIntervalUnionForIdleClock(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := testRelayPolicy()
		p.RateLimitBytesPerSecond = 1
		p.NoProgressTimeout = 2 * time.Second
		h := &Hub{}
		if err := h.SetPeerRelayPolicy(p); err != nil {
			t.Fatal(err)
		}
		_, finish := h.beginPeerTransfer(context.Background(), prepobs.PeerTransferSpec{TransferID: "wait"})
		defer finish("canceled", "")
		m := h.peerManager()
		l := acquireTestLease(t, m, "wait", 0)
		done := make(chan error, 2)
		for range 2 {
			go func() { _, err := l.copy(io.Discard, bytes.NewReader(make([]byte, 96)), true); done <- err }()
		}
		synctest.Wait()
		if l.waiting != 2 {
			t.Fatalf("expected overlapping waits, got %d", l.waiting)
		}
		time.Sleep(10 * time.Second)
		synctest.Wait()
		if l.ctx.Err() != nil {
			t.Fatal("server QoS wait caused false idle timeout")
		}
		// The idle clock is paused once for two concurrent pump waits.
		m.mu.Lock()
		paused := time.Since(l.pauseStart) + l.paused
		m.mu.Unlock()
		if paused != 10*time.Second {
			t.Fatalf("overlap counted twice: %s", paused)
		}
		snapshot := h.PeerTransferSnapshotFor("wait")
		if snapshot.WaitingCount != 1 || snapshot.Totals.RateWaitNS != int64(10*time.Second) || len(snapshot.Active) != 1 || snapshot.Active[0].RateWaitNS != int64(10*time.Second) {
			t.Fatalf("ongoing overlap metric must count interval union: %+v", snapshot)
		}
		if m.totals.RateWaitNS != 0 {
			t.Fatal("snapshot mutated cumulative waits")
		}
		l.cancel(context.Canceled)
		<-done
		<-done
		l.release()
		snapshot = h.PeerTransferSnapshotFor("wait")
		if snapshot.Totals.RateWaitNS != int64(10*time.Second) || snapshot.Active[0].RateWaitNS != int64(10*time.Second) {
			t.Fatalf("completed overlap counted twice: %+v", snapshot)
		}
	})
}
