package controller

import (
	"context"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestPeerRelayBusyRetriesSameModelSourceWithFreshOwnership(t *testing.T) {
	st, model, src, dst, file := setupModelSyncController(t)
	attempts := make(chan proto.ModelSyncCopyRequest, 4)
	hub := &modelSyncHub{online: map[string]bool{"src": true, "dst": true}}
	hub.copyFn = func(ctx context.Context, req proto.ModelSyncCopyRequest) (proto.ModelSyncCopyResult, error) {
		attempts <- req
		if hub.requestCount() == 1 {
			return proto.ModelSyncCopyResult{}, messages.PeerRelayBusy()
		}
		<-ctx.Done()
		return proto.ModelSyncCopyResult{}, ctx.Err()
	}
	c := New(st, hub, nil)
	if !c.trySyncModelFromPeer(context.Background(), dst, model, file) {
		t.Fatal("not started")
	}
	first := <-attempts
	var second proto.ModelSyncCopyRequest
	select {
	case second = <-attempts:
	case <-time.After(6 * time.Second):
		t.Fatal("capacity was treated as terminal failure")
	}
	if first.TransferID == second.TransferID || first.SourceNodeUUID != second.SourceNodeUUID || first.TargetNodeUUID != second.TargetNodeUUID || first.RateLimitKBs != second.RateLimitKBs {
		t.Fatalf("retry replaced choice or reused credentials: %+v %+v", first, second)
	}
	if c.peerCool.cooling(src.ID, dst.ID, c.now()) {
		t.Fatal("capacity rejection cooled the peer pair")
	}
	if err := st.UpdateModelFileStatus(dst.ID, proto.ModelFileStatus{Key: store.ModelFileKey(file.ID), TransferID: first.TransferID, State: proto.MFFailed, Message: "retired receiver"}); err != nil {
		t.Fatal(err)
	}
	current, _ := st.ModelFileByID(file.ID)
	if current.TransferID != second.TransferID || current.State != proto.MFSyncing {
		t.Fatalf("stale report changed retry: %+v", current)
	}
	if _, err := c.CancelModelSync(context.Background(), model.ID, dst.UUID); err != nil {
		t.Fatal(err)
	}
	waitForModelSyncIdle(t, c, store.ModelFileKey(file.ID))
}

func TestPeerRelayBusyModelWaitIsCancelableWithoutPublicFallback(t *testing.T) {
	st, model, src, dst, file := setupModelSyncController(t)
	hub := &modelSyncHub{online: map[string]bool{"src": true, "dst": true}, copyErr: messages.PeerRelayBusy(), called: make(chan struct{})}
	c := New(st, hub, nil)
	if !c.trySyncModelFromPeer(context.Background(), dst, model, file) {
		t.Fatal("not started")
	}
	<-hub.called
	deadline := time.Now().Add(2 * time.Second)
	for {
		row, _ := st.ModelFileByID(file.ID)
		if row.TransferErrorCode == messages.ErrorPeerRelayBusy {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("not waiting: %+v", row)
		}
		time.Sleep(time.Millisecond)
	}
	row, err := c.CancelModelSync(context.Background(), model.ID, dst.UUID)
	if err != nil {
		t.Fatal(err)
	}
	waitForModelSyncIdle(t, c, store.ModelFileKey(file.ID))
	if err := st.UpdateModelFileStatus(dst.ID, proto.ModelFileStatus{Key: store.ModelFileKey(file.ID), TransferID: row.TransferID, State: proto.MFSyncing}); err != nil {
		t.Fatal(err)
	}
	row, _ = st.ModelFileByID(file.ID)
	if row.State != proto.MFDownloadRequired || row.TransferErrorCode != "PEER_TRANSFER_CANCELED" || hub.requestCount() != 1 || c.peerCool.cooling(src.ID, dst.ID, c.now()) {
		t.Fatalf("cancel reopened or fell back: %+v, attempts=%d", row, hub.requestCount())
	}
}

func TestPeerRelayBusyModelRestorationRetiresPreRestartAttempt(t *testing.T) {
	st, model, src, dst, file := setupModelSyncController(t)
	ref := store.FileRef{ModelID: file.ModelID, StorageID: file.StorageID, NodeID: file.NodeID}
	if err := st.BeginModelSync(ref, src.ID, "before-restart", file.SizeBytes); err != nil {
		t.Fatal(err)
	}
	if changed, err := st.MarkModelSyncWaiting(ref, "before-restart", "busy", messages.ErrorPeerRelayBusy); err != nil || !changed {
		t.Fatalf("wait=%v %v", changed, err)
	}
	file, _ = st.ModelFileByID(file.ID)
	h := &modelSyncHub{online: map[string]bool{"src": true, "dst": true}, called: make(chan struct{}), release: make(chan struct{})}
	c := New(st, h, nil)
	if !c.trySyncModelFromPeer(context.Background(), dst, model, file) {
		t.Fatal("busy restoration not handled")
	}
	select {
	case <-h.called:
	case <-time.After(2 * time.Second):
		t.Fatal("no restored attempt")
	}
	h.mu.Lock()
	req := h.requests[0]
	h.mu.Unlock()
	if req.TransferID == "before-restart" || req.SourceNodeUUID != src.UUID {
		t.Fatalf("restored attempt=%+v", req)
	}
	_ = st.UpdateModelFileStatus(dst.ID, proto.ModelFileStatus{Key: store.ModelFileKey(file.ID), TransferID: "before-restart", State: proto.MFFailed, Message: "delayed cleanup"})
	got, _ := st.ModelFileByID(file.ID)
	if got.TransferID != req.TransferID || got.State != proto.MFSyncing {
		t.Fatalf("old cleanup corrupted restored task: %+v", got)
	}
	_, _ = c.CancelModelSync(context.Background(), model.ID, dst.UUID)
	waitForModelSyncIdle(t, c, store.ModelFileKey(file.ID))
}

func TestPeerRelayBusyRestorationPreservesFirstAutomaticGroupRate(t *testing.T) {
	st, model, src, dst, file := setupModelSyncController(t)
	groups, err := st.SyncGroups()
	if err != nil || len(groups) == 0 {
		t.Fatalf("groups=%v %v", groups, err)
	}
	if _, err := st.UpdateSyncGroup(groups[0].ID, store.SyncGroupSpec{Name: groups[0].Name, RateLimitKiB: 2048}); err != nil {
		t.Fatal(err)
	}
	firstHub := &modelSyncHub{online: map[string]bool{"src": true, "dst": true}, copyErr: messages.PeerRelayBusy(), called: make(chan struct{})}
	firstController := New(st, firstHub, nil)
	ctx, cancel := context.WithCancel(context.Background())
	if !firstController.trySyncModelFromPeer(ctx, dst, model, file) {
		t.Fatal("first automatic attempt not started")
	}
	<-firstHub.called
	deadline := time.Now().Add(time.Second)
	for {
		file, _ = st.ModelFileByID(file.ID)
		if file.TransferErrorCode == messages.ErrorPeerRelayBusy {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no waiting row: %+v", file)
		}
		time.Sleep(time.Millisecond)
	}
	cancel() // server shutdown, without operator cancellation of the ledger
	waitForModelSyncIdle(t, firstController, store.ModelFileKey(file.ID))
	if file.RateLimit != 2048 {
		t.Fatalf("first wait lost effective cap: %+v", file)
	}
	nextHub := &modelSyncHub{online: map[string]bool{"src": true, "dst": true}, called: make(chan struct{}), release: make(chan struct{})}
	nextController := New(st, nextHub, nil)
	if !nextController.trySyncModelFromPeer(context.Background(), dst, model, file) {
		t.Fatal("restoration not started")
	}
	<-nextHub.called
	nextHub.mu.Lock()
	req := nextHub.requests[0]
	nextHub.mu.Unlock()
	if req.RateLimitKBs != 2048 || req.SourceNodeUUID != src.UUID || req.TransferID == file.TransferID {
		t.Fatalf("restored cap/source/identity=%+v", req)
	}
	_, _ = nextController.CancelModelSync(context.Background(), model.ID, dst.UUID)
	waitForModelSyncIdle(t, nextController, store.ModelFileKey(file.ID))
}
