package hub

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func reportTestSession(h *Hub, node *store.Node) *Session {
	return &Session{hub: h, NodeUUID: node.UUID, nodeID: node.ID, closed: make(chan struct{}),
		initialPushDone: make(chan struct{}), pending: map[uint64]chan proto.Envelope{},
		assetDigests: map[string]struct{}{}, sendFn: func(string, uint64, any) error { return nil }}
}

func within(t *testing.T, what string, d time.Duration, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { defer close(done); fn() }()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatalf("%s blocked for %v", what, d)
	}
}

// With the store's single writer held — the 2026-10-04 incident's shape — an
// instance report must neither stall the read loop nor hold hub.mu: register
// (the exclusive side) and the gateway's session lookups stay immediate. The
// report still lands, in order, once the writer frees.
func TestInstanceStatusDoesNotHoldReadLoopOrHubLockAcrossStoreWrite(t *testing.T) {
	st, err := store.Open(t.TempDir()+"/reports.db", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	node := store.Node{UUID: "report-node", Name: "report-node"}
	if err := st.DB().Create(&node).Error; err != nil {
		t.Fatal(err)
	}
	inst, err := st.CreateInstance(&store.Instance{UUID: "report-inst", NodeID: node.ID, State: proto.InstStarting})
	if err != nil {
		t.Fatal(err)
	}
	h := New(st, quietLog())
	s := reportTestSession(h, &node)
	h.sessions[node.UUID] = s

	held, release := make(chan struct{}), make(chan struct{})
	writerDone := make(chan error, 1)
	go func() {
		writerDone <- st.DB().Transaction(func(tx *gorm.DB) error {
			if err := tx.Exec("UPDATE nodes SET name = name WHERE id = ?", node.ID).Error; err != nil {
				return err
			}
			close(held)
			<-release
			return nil
		})
	}()
	<-held

	for _, state := range []string{proto.InstPulling, proto.InstRunning} {
		raw, _ := json.Marshal(proto.InstanceStatus{UUID: inst.UUID, State: state})
		within(t, "read loop handle", time.Second, func() { s.handle(proto.Envelope{T: proto.TStatusInstance, D: raw}) })
	}
	within(t, "register's hub.mu.Lock", time.Second, func() {
		h.mu.Lock()
		h.mu.Unlock() //nolint:staticcheck // SA2001: acquiring the exclusive lock is the behavior measured here.
	})
	within(t, "hub.session lookup", time.Second, func() { _ = h.session(node.UUID) })

	close(release)
	if err := <-writerDone; err != nil {
		t.Fatal(err)
	}
	s.drainReports()
	got, err := st.InstanceByUUID(inst.UUID)
	if err != nil || got.State != proto.InstRunning {
		t.Fatalf("instance after drain = %+v, err=%v; want the later report (running) applied last", got, err)
	}
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

// Ordered reports apply in receipt order even when one session sends more than
// its worker buffers, and draining applies everything received before the
// read loop ends.
func TestSessionReportsApplyInReceiptOrder(t *testing.T) {
	h := New(nil, quietLog())
	s := reportTestSession(h, &store.Node{UUID: "order-node"})
	var got []int
	for i := 0; i < sessionReportDepth*2; i++ {
		s.submitReport(func() { got = append(got, i) })
	}
	s.drainReports()
	if len(got) != sessionReportDepth*2 {
		t.Fatalf("applied %d reports, want %d", len(got), sessionReportDepth*2)
	}
	for i, v := range got {
		if v != i {
			t.Fatalf("report %d applied as %d: order lost", i, v)
		}
	}
}

// A storage report is a full snapshot: it coalesces per node, and one from a
// session that was replaced before the worker reached it does not persist.
func TestStorageStatusQueuedAndFenced(t *testing.T) {
	st, err := store.Open(t.TempDir()+"/storage.db", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	node := store.Node{UUID: "storage-node", Name: "storage-node"}
	if err := st.DB().Create(&node).Error; err != nil {
		t.Fatal(err)
	}
	storage, err := st.CreateStorage(&store.Storage{Name: "nvme", Type: proto.StorageLocal, Enabled: true, AllNodes: true, Path: "/nvme"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	h := New(st, quietLog())
	s := reportTestSession(h, &node)
	h.sessions[node.UUID] = s
	report := func(state string) json.RawMessage {
		raw, _ := json.Marshal(proto.StorageStatusReport{Storages: []proto.StorageStatus{{ID: storage.ID, State: state, Path: "/nvme"}}})
		return raw
	}
	applyQueued := func() {
		for {
			uuid, queued, ok := h.storageQ.pop()
			if !ok {
				return
			}
			if err := h.storageQ.apply(uuid, queued); err != nil {
				t.Fatal(err)
			}
		}
	}
	stateOf := func() string {
		rows, err := st.StorageNodeStatesForNode(node.ID)
		if err != nil || len(rows) != 1 {
			t.Fatalf("storage states = %+v, err=%v", rows, err)
		}
		return rows[0].State
	}

	s.handle(proto.Envelope{T: proto.TStatusStorage, D: report(proto.StorageStateFailed)})
	s.handle(proto.Envelope{T: proto.TStatusStorage, D: report(proto.StorageStateReady)})
	if n := len(h.storageQ.order); n != 1 {
		t.Fatalf("pending storage reports = %d, want the two coalesced into 1", n)
	}
	applyQueued()
	if got := stateOf(); got != proto.StorageStateReady {
		t.Fatalf("storage state = %q, want the newest snapshot", got)
	}

	s.handle(proto.Envelope{T: proto.TStatusStorage, D: report(proto.StorageStateFailed)})
	replacement := reportTestSession(h, &node)
	h.mu.Lock()
	h.sessions[node.UUID] = replacement
	h.mu.Unlock()
	applyQueued()
	if got := stateOf(); got != proto.StorageStateReady {
		t.Fatalf("storage state = %q: a replaced session's report persisted", got)
	}

	// Receipt never waits on a write in flight, even one holding the fence.
	replacementWriting, finishReplacementWrite := make(chan struct{}), make(chan struct{})
	go func() {
		_, _ = replacement.applyCurrent(func(uint) error { close(replacementWriting); <-finishReplacementWrite; return nil })
	}()
	<-replacementWriting
	within(t, "status.storage receipt during a write", time.Second, func() {
		replacement.handle(proto.Envelope{T: proto.TStatusStorage, D: report(proto.StorageStateReady)})
	})
	close(finishReplacementWrite)
	applyQueued()

	// Storage reports are sent only on change, so the replacement's pending
	// snapshot must survive a late report from the session it replaced.
	replacement.handle(proto.Envelope{T: proto.TStatusStorage, D: report(proto.StorageStateMounting)})
	s.handle(proto.Envelope{T: proto.TStatusStorage, D: report(proto.StorageStateFailed)})
	applyQueued()
	if got := stateOf(); got != proto.StorageStateMounting {
		t.Fatalf("storage state = %q: a replaced session's late report displaced the current one", got)
	}
}
