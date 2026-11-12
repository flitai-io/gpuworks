package hub

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/messages"
)

func TestPeerRelayRejectsLegacyChangedSessionsAndBusyNodes(t *testing.T) {
	h := newTestHub(t)
	ready := func(id string) *Session {
		ctrl, _ := yamuxPairData(t)
		pc, other := net.Pipe()
		t.Cleanup(func() { pc.Close(); other.Close() })
		s := newSession(h, ctrl, pc)
		s.NodeUUID = id
		s.caps = []string{proto.CapPeerRelayV1}
		data, _ := yamuxPairData(t)
		s.attachData(data)
		h.mu.Lock()
		h.sessions[id] = s
		h.mu.Unlock()
		return s
	}
	source := ready("source")
	target := ready("target")
	target.caps = nil
	if h.archiveRelayFor("source", "target", "model") != nil {
		t.Fatal("old agent offered relay")
	}
	target.caps = []string{proto.CapPeerRelayV1}
	if h.archiveRelayFor("source", "source", "model") != nil {
		t.Fatal("self relay offered")
	}
	relay := h.archiveRelayFor("source", "target", "model")
	if relay == nil {
		t.Fatal("dual data agents cannot relay")
	}
	shortenPeerRelayQueue(h, 50*time.Millisecond)
	manager := h.peerManager()
	held, err := manager.acquire(context.Background(), "held", "source", "another", 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := relay(context.Background(), proto.RPCModelSyncProbe, "t", nil); !errors.Is(err, messages.ErrPeerRelayBusy) {
		t.Fatalf("busy node allowed: %v", err)
	}
	held.release()
	a, err := manager.acquire(context.Background(), "a", "a", "b", 0)
	if err != nil {
		t.Fatal(err)
	}
	b, err := manager.acquire(context.Background(), "b", "c", "d", 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := relay(context.Background(), proto.RPCModelSyncProbe, "t", nil); !errors.Is(err, messages.ErrPeerRelayBusy) {
		t.Fatalf("global budget allowed: %v", err)
	}
	a.release()
	b.release()
	h.mu.Lock()
	delete(h.sessions, source.NodeUUID)
	h.mu.Unlock()
	if _, err := relay(context.Background(), proto.RPCModelSyncProbe, "t", nil); err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("stale authorization allowed: %v", err)
	}
}

func TestPeerRelayReadinessRejectsOversizeAndRemoteError(t *testing.T) {
	for _, line := range []string{`{"error":"unknown transfer"}` + "\n", strings.Repeat("x", 4096), "broken\n"} {
		a, b := net.Pipe()
		done := make(chan struct{})
		go func() { defer close(done); defer b.Close(); b.Write([]byte(line)) }()
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		err := readPeerRelayReady(ctx, a)
		cancel()
		a.Close()
		<-done
		if err == nil {
			t.Fatalf("invalid ready accepted: %.30s", line)
		}
	}
}

func TestPeerRelayRetainsStricterRateOnServer(t *testing.T) {
	for _, rate := range []int{0, 1, 64 * 1024, 128 * 1024} {
		p := proto.ModelSyncSendParams{RateLimitKBs: rate, AllowRelay: true}
		if got := peerRelayUserRate(p); got != int64(rate)*1024 {
			t.Fatalf("rate=%d got=%d", rate, got)
		}
		model := limitPeerRelay(p).(proto.ModelSyncSendParams)
		image := limitPeerRelay(proto.ImageSyncSendParams{RateLimitKBs: rate, AllowRelay: true}).(proto.ImageSyncSendParams)
		if model.RateLimitKBs != 0 || image.RateLimitKBs != 0 || !model.AllowRelay || !image.AllowRelay {
			t.Fatalf("rate=%d model=%+v image=%+v", rate, model, image)
		}
	}
}
