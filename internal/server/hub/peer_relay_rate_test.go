package hub

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"gpuworks.ai/gpuworks/internal/server/prepobs"
)

type relayWriteEvent struct {
	at time.Time
	n  int
}
type relayMeasuredWriter struct {
	mu     sync.Mutex
	events []relayWriteEvent
	total  int64
}

func (w *relayMeasuredWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.events = append(w.events, relayWriteEvent{time.Now(), len(p)})
	w.total += int64(len(p))
	return len(p), nil
}
func (w *relayMeasuredWriter) count() int64 { w.mu.Lock(); defer w.mu.Unlock(); return w.total }
func testRelayPolicy() prepobs.PeerRelayPolicy {
	p := prepobs.DefaultPeerRelayPolicy()
	p.RateLimitBytesPerSecond = 1024
	p.BurstBytes = 64
	p.QuantumBytes = 16
	return p
}
func acquireTestLease(t *testing.T, m *peerRelayManager, id string, rate int64) *peerRelayLease {
	t.Helper()
	l, err := m.acquire(context.Background(), id, id+"s", id+"t", rate)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func TestPeerRelayAggregateBudgetFairnessAndIdleBorrowing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := testRelayPolicy()
		m := newPeerRelayManager(p)
		a := acquireTestLease(t, m, "a", 0)
		b := acquireTestLease(t, m, "b", 0)
		var wa, wb relayMeasuredWriter
		start := time.Now()
		done := make(chan struct{}, 2)
		go func() { a.copy(&wa, bytes.NewReader(make([]byte, 1024)), true); done <- struct{}{} }()
		go func() { b.copy(&wb, bytes.NewReader(make([]byte, 8192)), false); done <- struct{}{} }()
		synctest.Wait()
		time.Sleep(time.Second)
		synctest.Wait()
		if delta := wa.count() - wb.count(); delta > p.BurstBytes || delta < -p.BurstBytes {
			t.Fatalf("unfair active shares: a=%d b=%d", wa.count(), wb.count())
		}
		time.Sleep(2 * time.Second)
		synctest.Wait()
		if wa.count() != 1024 || wb.count() < 1900 {
			t.Fatalf("idle share not borrowed: a=%d b=%d", wa.count(), wb.count())
		}
		m.mu.Lock()
		written := m.totals.SourceToTargetBytes + m.totals.TargetToSourceBytes
		m.mu.Unlock()
		if written > p.BurstBytes+p.RateLimitBytesPerSecond*int64(time.Since(start)/time.Second) {
			t.Fatalf("aggregate budget exceeded: %d", written)
		}
		a.cancel(context.Canceled)
		b.cancel(context.Canceled)
		<-done
		<-done
		a.release()
		b.release()
	})
}

func TestPeerRelayEveryWriteRespectsAggregateEnvelopeAndLowerFlowCap(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := testRelayPolicy()
		m := newPeerRelayManager(p)
		a := acquireTestLease(t, m, "a", 256)
		b := acquireTestLease(t, m, "b", 0)
		var w, lower relayMeasuredWriter
		start := time.Now()
		done := make(chan struct{}, 3)
		for _, task := range []struct {
			l       *peerRelayLease
			forward bool
		}{{a, true}, {a, false}, {b, true}} {
			go func() {
				writer := io.Writer(&w)
				if task.l == a {
					writer = io.MultiWriter(&lower, &w)
				}
				task.l.copy(writer, bytes.NewReader(make([]byte, 4096)), task.forward)
				done <- struct{}{}
			}()
		}
		time.Sleep(3 * time.Second)
		synctest.Wait()
		var total int64
		for _, event := range w.events {
			total += int64(event.n)
			bound := p.BurstBytes + int64(event.at.Sub(start))*p.RateLimitBytesPerSecond/int64(time.Second)
			if total > bound {
				t.Fatalf("at %s wrote=%d > %d", event.at.Sub(start), total, bound)
			}
		}
		if lower.count() > 64+3*256 || lower.count() < 3*256 {
			t.Fatalf("combined directions did not honor lower flow cap: %d", lower.count())
		}
		a.cancel(context.Canceled)
		b.cancel(context.Canceled)
		for range 3 {
			<-done
		}
		a.release()
		b.release()
	})
}

type relayBlockedWriter struct {
	started chan struct{}
	release <-chan struct{}
	once    sync.Once
}

func (w *relayBlockedWriter) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.started) })
	<-w.release
	return len(p), nil
}
func TestPeerRelayBlockedWritesReserveBurstWithoutBlockingOtherTransfer(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := testRelayPolicy()
		m := newPeerRelayManager(p)
		a := acquireTestLease(t, m, "a", 0)
		b := acquireTestLease(t, m, "b", 0)
		unblock := make(chan struct{})
		blocked := &relayBlockedWriter{started: make(chan struct{}), release: unblock}
		var fast relayMeasuredWriter
		done := make(chan struct{}, 3)
		for range 2 {
			go func() { a.copy(blocked, bytes.NewReader(make([]byte, 16)), true); done <- struct{}{} }()
		}
		<-blocked.started
		go func() { b.copy(&fast, bytes.NewReader(make([]byte, 8192)), true); done <- struct{}{} }()
		time.Sleep(2 * time.Second)
		synctest.Wait()
		if fast.count() < 1900 {
			t.Fatalf("slow writer starved another transfer: %d", fast.count())
		}
		m.mu.Lock()
		reserved := m.reserved
		available := m.bucket.tokens
		m.mu.Unlock()
		if reserved != 32 || available+reserved > p.BurstBytes {
			t.Fatalf("unwritten permits escaped burst: reserved=%d tokens=%d", reserved, available)
		}
		close(unblock)
		synctest.Wait()
		b.cancel(context.Canceled)
		for range 3 {
			<-done
		}
		a.release()
		b.release()
	})
}

type relayPartialWriter struct{}

func (relayPartialWriter) Write([]byte) (int, error) { return 5, io.ErrUnexpectedEOF }
func TestPeerRelayPartialWriteRefundsOnlyUnusedBytes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := newPeerRelayManager(testRelayPolicy())
		l := acquireTestLease(t, m, "a", 0)
		n, err := l.copy(relayPartialWriter{}, bytes.NewReader(make([]byte, 16)), true)
		if n != 5 || !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("n=%d err=%v", n, err)
		}
		m.mu.Lock()
		tokens, reserved, written := m.bucket.tokens, m.reserved, m.totals.SourceToTargetBytes
		m.mu.Unlock()
		if tokens != 59 || reserved != 0 || written != 5 {
			t.Fatalf("tokens=%d reserved=%d actual=%d", tokens, reserved, written)
		}
		l.release()
	})
}
func TestPeerRelayCanceledPermitReturnsQuotaAndJoins(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := testRelayPolicy()
		p.RateLimitBytesPerSecond = 1
		m := newPeerRelayManager(p)
		l := acquireTestLease(t, m, "a", 0)
		done := make(chan error, 1)
		go func() { _, err := l.copy(io.Discard, bytes.NewReader(make([]byte, 128)), true); done <- err }()
		synctest.Wait()
		if l.waiting != 1 {
			t.Fatal("did not reach byte-budget wait")
		}
		l.cancel(context.Canceled)
		if !errors.Is(<-done, context.Canceled) {
			t.Fatal("cancel not propagated")
		}
		l.release()
		synctest.Wait()
		m.mu.Lock()
		defer m.mu.Unlock()
		if len(m.leases) != 0 || m.reserved != 0 || m.running || len(l.pending) != 0 || l.waiting != 0 {
			t.Fatal("permit/lease/scheduler leaked")
		}
	})
}
