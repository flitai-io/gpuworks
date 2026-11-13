package hub

import (
	"errors"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func reportTestSession(h *Hub, node *store.Node) *Session {
	return &Session{hub: h, NodeUUID: node.UUID, nodeID: node.ID, closed: make(chan struct{}),
		initialPushDone: make(chan struct{}), pending: map[uint64]chan proto.Envelope{},
		assetDigests: map[string]struct{}{}, sendFn: func(string, uint64, any) error { return nil }}
}

// A replaced session's reports never land: neither one still queued, nor one
// whose write the old session validated before the swap — register waits that
// write out before it returns.
func TestReplacedSessionReportsAreFenced(t *testing.T) {
	h := New(nil, quietLog())
	node := &store.Node{UUID: "fence-node"}
	node.ID = 7
	old := reportTestSession(h, node)
	h.sessions[node.UUID] = old

	inWrite, finishWrite := make(chan struct{}), make(chan struct{})
	writeReturned := make(chan bool, 1)
	go func() {
		applied, _ := old.applyCurrent(func(nodeID uint) error {
			if nodeID != 7 {
				t.Errorf("write got node %d, want the session's authenticated 7", nodeID)
			}
			close(inWrite)
			<-finishWrite
			return nil
		})
		writeReturned <- applied
	}()
	<-inWrite

	replaced := make(chan struct{})
	go func() {
		h.mu.Lock()
		h.sessions[node.UUID] = reportTestSession(h, node)
		h.mu.Unlock()
		old.fenceReports()
		close(replaced)
	}()
	select {
	case <-replaced:
		t.Fatal("fence returned while the old session's validated write was in flight")
	case <-time.After(50 * time.Millisecond):
	}
	close(finishWrite)
	<-replaced
	if !<-writeReturned {
		t.Fatal("the write validated before the swap should report applied")
	}

	ran := false
	if applied, err := old.applyCurrent(func(uint) error { ran = true; return nil }); applied || err != nil || ran {
		t.Fatalf("replaced session applied=%v err=%v ran=%v", applied, err, ran)
	}
	failing := errors.New("store down")
	cur := h.sessions[node.UUID]
	if applied, err := cur.applyCurrent(func(uint) error { return failing }); applied || !errors.Is(err, failing) {
		t.Fatalf("failed write applied=%v err=%v; want not applied with the store error", applied, err)
	}
}
