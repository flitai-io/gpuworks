package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/netplan"
	"gpuworks.ai/gpuworks/internal/server/store"
)

type modelSyncHub struct {
	online map[string]bool

	mu         sync.Mutex
	requests   []proto.ModelSyncCopyRequest
	called     chan struct{}
	release    chan struct{}
	copyFn     func(context.Context, proto.ModelSyncCopyRequest) (proto.ModelSyncCopyResult, error)
	copyErr    error
	copyResult *proto.ModelSyncCopyResult
	stops      int
	syncs      int
	updates    []proto.StateUpdate
	// onStop runs inside StopModelSync, before it returns.
	onStop func()
}

func (h *modelSyncHub) Online(uuid string) bool { return h.online[uuid] }

// RequestImageRefs records the refs as the push the hub would send.
func (h *modelSyncHub) RequestImageRefs(uuid string, refs []string) {
	_ = h.PushToNode(uuid, proto.StateUpdate{ImageRefs: refs})
}

func (h *modelSyncHub) PushToNode(_ string, update proto.StateUpdate) error {
	h.mu.Lock()
	h.updates = append(h.updates, update)
	h.mu.Unlock()
	return nil
}
func (h *modelSyncHub) SyncNode(string) error {
	h.mu.Lock()
	h.syncs++
	h.mu.Unlock()
	return nil
}
func (h *modelSyncHub) StopModelSync(context.Context, string, string, string) error {
	h.mu.Lock()
	h.stops++
	onStop := h.onStop
	h.mu.Unlock()
	if onStop != nil {
		onStop()
	}
	return nil
}
func (h *modelSyncHub) CopyModelPath(ctx context.Context, req proto.ModelSyncCopyRequest) (proto.ModelSyncCopyResult, error) {
	h.mu.Lock()
	h.requests = append(h.requests, req)
	if len(h.requests) == 1 && h.called != nil {
		close(h.called)
	}
	copyFn := h.copyFn
	h.mu.Unlock()
	if copyFn != nil {
		return copyFn(ctx, req)
	}
	if h.release != nil {
		select {
		case <-ctx.Done():
			return proto.ModelSyncCopyResult{TransferID: req.TransferID}, ctx.Err()
		case <-h.release:
		}
	}
	if h.copyErr != nil {
		return proto.ModelSyncCopyResult{TransferID: req.TransferID}, h.copyErr
	}
	if h.copyResult != nil {
		return *h.copyResult, nil
	}
	return proto.ModelSyncCopyResult{TransferID: req.TransferID, Bytes: 123, SHA256: "confirmed"}, nil
}

func (h *modelSyncHub) requestCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.requests)
}

func TestPushModelFilePreservesSparseRepositoryIdentity(t *testing.T) {
	st, err := store.Open("file:controller_sparse_model_task?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	hub := &modelSyncHub{}
	c := New(st, hub, nil)
	model := &store.Model{ID: 9, Source: "modelscope", SourceID: "MiniMax/MiniMax-H3", Revision: "commit", ArtifactSubfolder: "FL2VA"}
	file := &store.ModelFile{ModelID: 9, NodeID: 2, Path: "modelscope/h3-sparse", State: proto.MFPending}
	_ = c.pushModelFile(&store.Node{UUID: "node"}, file, model)
	if len(hub.updates) != 1 || len(hub.updates[0].ModelFiles) != 1 {
		t.Fatalf("updates=%+v", hub.updates)
	}
	task := hub.updates[0].ModelFiles[0]
	if task.SourceID != model.SourceID || task.Revision != model.Revision || task.Subfolder != "FL2VA" || task.TargetPath != file.Path {
		t.Fatalf("sparse model task=%+v", task)
	}
}

// reportManagementAddress records what a real agent reports: the address it
// reaches the server from, which the management network carries copies on.
func reportManagementAddress(t *testing.T, st *store.Store, uuid, ip string) {
	t.Helper()
	inv := proto.NetInventory{
		Interfaces: []proto.NetInterface{{Name: "eth0", Addrs: []string{ip + "/24"}}},
		Management: &proto.ManagementAddress{Interface: "eth0", IP: ip},
	}
	if err := st.UpdateNodeStatus(uuid, proto.NodeStatus{Network: &inv}); err != nil {
		t.Fatal(err)
	}
}

// managementInventoryJSON is the stored inventory of a node whose agent
// reported ip as its management address.
func managementInventoryJSON(t *testing.T, ip string) string {
	t.Helper()
	b, err := json.Marshal(proto.NetInventory{
		Interfaces: []proto.NetInterface{{Name: "eth0", Addrs: []string{ip + "/24"}}},
		Management: &proto.ManagementAddress{Interface: "eth0", IP: ip},
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func setupModelSyncController(t *testing.T) (*store.Store, *store.Model, *store.Node, *store.Node, *store.ModelFile) {
	t.Helper()
	st, err := store.Open("file:controller_modelsync_"+t.Name()+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "Qwen/Qwen3-8B", Revision: "main", Name: "Qwen3"})
	if err != nil {
		t.Fatal(err)
	}
	src, err := st.RegisterNode("src", store.HashToken("t1"), "src")
	if err != nil {
		t.Fatal(err)
	}
	dst, err := st.RegisterNode("dst", store.HashToken("t2"), "dst")
	if err != nil {
		t.Fatal(err)
	}
	reportManagementAddress(t, st, "src", "10.0.0.1")
	reportManagementAddress(t, st, "dst", "10.0.0.2")
	if _, err := st.UpsertModelFile(&store.ModelFile{
		ModelID: model.ID, NodeID: src.ID, Path: "/models/Qwen3", State: proto.MFPresent,
		SizeBytes: 12, Downloaded: 12, Acquisition: proto.ModelAcquireRegistered,
	}); err != nil {
		t.Fatal(err)
	}
	dstMF, err := st.UpsertModelFile(&store.ModelFile{
		ModelID: model.ID, NodeID: dst.ID, Path: "huggingface/Qwen3", State: proto.MFPending, Acquisition: proto.ModelAcquireAuto,
	})
	if err != nil {
		t.Fatal(err)
	}
	return st, model, src, dst, dstMF
}

func TestTrySyncModelFromPeerStartsAsyncAndWaitsForTargetStatus(t *testing.T) {
	st, model, src, dst, dstMF := setupModelSyncController(t)
	hub := &modelSyncHub{online: map[string]bool{"src": true, "dst": true}, called: make(chan struct{}), release: make(chan struct{})}
	c := New(st, hub, nil)
	if !c.trySyncModelFromPeer(context.Background(), dst, model, dstMF) {
		t.Fatal("expected peer sync to be accepted")
	}
	select {
	case <-hub.called:
	case <-time.After(2 * time.Second):
		t.Fatal("async peer copy did not start")
	}
	got, err := st.ModelFileByModelNode(model.ID, dst.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != proto.MFSyncing || got.Acquisition != proto.ModelAcquirePeer || got.TransferID == "" || got.TransferSourceNodeID != src.ID {
		t.Fatalf("syncing model file = %+v", got)
	}
	if !c.trySyncModelFromPeer(context.Background(), dst, model, got) || hub.requestCount() != 1 {
		t.Fatalf("duplicate reconcile launched %d copies", hub.requestCount())
	}
	close(hub.release)
	waitForHubSync(t, hub)
	assertModelSyncActive(t, c, testModelFileKey(t, st, model.ID, dst.ID))
	got, err = st.ModelFileByModelNode(model.ID, dst.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != proto.MFSyncing {
		t.Fatalf("source confirmation incorrectly marked target present: %+v", got)
	}
	if !c.trySyncModelFromPeer(context.Background(), dst, model, got) || hub.requestCount() != 1 {
		t.Fatalf("post-ACK reconcile launched %d copies before target status", hub.requestCount())
	}
	if err := st.UpdateModelFileStatus(dst.ID, proto.ModelFileStatus{
		Key: testModelFileKey(t, st, model.ID, dst.ID), State: proto.MFPresent, Path: "/models/Qwen3-copy",
		BytesTotal: 12, BytesDone: 12, TransferID: got.TransferID, Acquisition: proto.ModelAcquirePeer,
	}); err != nil {
		t.Fatal(err)
	}
	c.onEvent(bus.Event{Topic: "modelfile", Key: testModelFileKey(t, st, model.ID, dst.ID)})
	waitForModelSyncIdle(t, c, testModelFileKey(t, st, model.ID, dst.ID))
	got, _ = st.ModelFileByModelNode(model.ID, dst.ID)
	if got.State != proto.MFPresent || got.Downloaded != 12 {
		t.Fatalf("target completion was not persisted: %+v", got)
	}
}

func TestSyncModelToNodeDoesNotReuseAbsolutePathFromPreviousDownload(t *testing.T) {
	st, model, src, dst, dstMF := setupModelSyncController(t)
	dstMF.Path = "/another-node/models/huggingface/Qwen_Qwen3-8B-main"
	dstMF.State = proto.MFFailed
	dstMF.Acquisition = proto.ModelAcquireDownload
	if _, err := st.UpsertModelFile(dstMF); err != nil {
		t.Fatal(err)
	}
	hub := &modelSyncHub{online: map[string]bool{"src": true, "dst": true}, called: make(chan struct{}), release: make(chan struct{})}
	c := New(st, hub, nil)
	if _, err := c.SyncModelToNodeWithOptions(context.Background(), model.ID, dst.UUID, src.UUID, -1); err != nil {
		t.Fatal(err)
	}
	select {
	case <-hub.called:
	case <-time.After(2 * time.Second):
		t.Fatal("peer copy did not start")
	}
	hub.mu.Lock()
	request := hub.requests[0]
	hub.mu.Unlock()
	want := "huggingface/Qwen_Qwen3-8B-main"
	if request.TargetPath != want {
		t.Fatalf("peer target path = %q, want target-relative %q", request.TargetPath, want)
	}
	close(hub.release)
}

func TestTargetPresentBeforeSourceRPCResponseDoesNotCancelSuccessfulSync(t *testing.T) {
	st, model, _, dst, dstMF := setupModelSyncController(t)
	hub := &modelSyncHub{online: map[string]bool{"src": true, "dst": true}, called: make(chan struct{}), release: make(chan struct{})}
	c := New(st, hub, nil)
	if !c.trySyncModelFromPeer(context.Background(), dst, model, dstMF) {
		t.Fatal("expected peer sync")
	}
	select {
	case <-hub.called:
	case <-time.After(2 * time.Second):
		t.Fatal("peer copy did not start")
	}
	got, err := st.ModelFileByModelNode(model.ID, dst.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateModelFileStatus(dst.ID, proto.ModelFileStatus{
		Key: testModelFileKey(t, st, model.ID, dst.ID), State: proto.MFPresent, TransferID: got.TransferID,
		BytesTotal: 12, BytesDone: 12, Acquisition: proto.ModelAcquirePeer,
	}); err != nil {
		t.Fatal(err)
	}
	c.onEvent(bus.Event{Topic: "modelfile", Key: testModelFileKey(t, st, model.ID, dst.ID)})
	close(hub.release)
	waitForHubSync(t, hub)
	got, err = st.ModelFileByModelNode(model.ID, dst.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != proto.MFPresent || got.Downloaded != 12 {
		t.Fatalf("successful target status regressed after source confirmation: %+v", got)
	}
}

// failPeerSync runs one peer sync to dst whose copy fails, and returns what
// the target's model file records once the coordinator has let go.
func failPeerSync(t *testing.T, st *store.Store, model *store.Model, dst *store.Node, dstMF *store.ModelFile) (*store.ModelFile, *modelSyncHub) {
	t.Helper()
	hub := &modelSyncHub{
		online: map[string]bool{"src": true, "dst": true}, called: make(chan struct{}),
		copyErr: errors.New("send peer model archive: dial model sync target: "),
	}
	c := New(st, hub, nil)
	if !c.trySyncModelFromPeer(context.Background(), dst, model, dstMF) {
		t.Fatal("expected peer attempt")
	}
	select {
	case <-hub.called:
	case <-time.After(2 * time.Second):
		t.Fatal("peer copy did not start")
	}
	waitForModelSyncIdle(t, c, testModelFileKey(t, st, model.ID, dst.ID))
	got, err := st.ModelFileByModelNode(model.ID, dst.ID)
	if err != nil {
		t.Fatal(err)
	}
	return got, hub
}

// A source the controller chose itself is no one's recorded choice: when the
// copy from it fails (two rented hosts whose management addresses do not
// route to each other), the model falls back to the public download as it
// does when no peer holds it, instead of waiting on an operator's retry.
func TestAutomaticPeerSyncFailureFallsBackToPublicDownload(t *testing.T) {
	st, model, _, dst, dstMF := setupModelSyncController(t)
	got, hub := failPeerSync(t, st, model, dst, dstMF)
	hub.mu.Lock()
	updates, transferID := len(hub.updates), hub.requests[0].TransferID
	var pushed []proto.ModelFileTask
	if updates == 1 {
		pushed = hub.updates[0].ModelFiles
	}
	hub.mu.Unlock()
	// The fallback is a new transfer of its own, named in the pushed task.
	if got.State != proto.MFPending || got.Acquisition != proto.ModelAcquireDownload ||
		got.TransferSourceNodeID != 0 || got.TransferID == "" || got.TransferID == transferID || got.Path != "huggingface/Qwen3" {
		t.Fatalf("after a failed automatic peer sync = %+v, want a pending public download at the same path", got)
	}
	if updates != 1 {
		t.Fatalf("pushes to the target = %d, want the download pushed once", updates)
	}
	if len(pushed) != 1 || pushed[0].Acquisition != proto.ModelAcquireDownload || pushed[0].TransferID != got.TransferID {
		t.Fatalf("pushed task = %+v, want the download named %q", pushed, got.TransferID)
	}
	// The failed transfer's own late report must not undo the fallback.
	if err := st.UpdateModelFileStatus(dst.ID, proto.ModelFileStatus{
		Key: fmt.Sprintf("%d-%d", model.ID, dst.ID), State: proto.MFFailed, TransferID: transferID, Message: "context canceled",
	}); err != nil {
		t.Fatal(err)
	}
	if after, _ := st.ModelFileByModelNode(model.ID, dst.ID); after.State != proto.MFPending {
		t.Fatalf("late report of the failed transfer = %+v, want the download kept", after)
	}
}

// With no public source there is nothing to fall back to: the failure stays
// on the model file with its error, for an operator to act on.
func TestAutomaticPeerSyncFailureWithoutPublicSourceRequiresAction(t *testing.T) {
	st, model, _, dst, dstMF := setupModelSyncController(t)
	if err := st.DB().Model(&store.Model{}).Where("id = ?", model.ID).Updates(map[string]any{"source": "", "source_id": ""}).Error; err != nil {
		t.Fatal(err)
	}
	got, hub := failPeerSync(t, st, model, dst, dstMF)
	if got.State != proto.MFDownloadRequired || got.Acquisition != proto.ModelAcquirePeer || !strings.Contains(got.Message, "dial model sync target") {
		t.Fatalf("failed peer sync = %+v, want download_required carrying the error", got)
	}
	if len(hub.updates) != 0 {
		t.Fatalf("pushed %d updates, want none", len(hub.updates))
	}
}

// A source the user picked is kept even when the copy from it fails: the
// model file waits for an explicit retry or choice, never a silent download.
func TestSelectedPeerSyncFailureRequiresExplicitAction(t *testing.T) {
	st, model, src, dst, _ := setupModelSyncController(t)
	if err := st.SetModelSyncSource(store.FileRef{ModelID: model.ID, NodeID: dst.ID}, src.ID); err != nil {
		t.Fatal(err)
	}
	dstMF, _ := st.ModelFileByModelNode(model.ID, dst.ID)
	got, hub := failPeerSync(t, st, model, dst, dstMF)
	if got.State != proto.MFDownloadRequired || got.Acquisition != proto.ModelAcquirePeer || got.TransferSourceNodeID != src.ID || got.Message == "" {
		t.Fatalf("failed selected-source sync = %+v, want download_required keeping the source", got)
	}
	if len(hub.updates) != 0 {
		t.Fatalf("pushed %d updates, want no silent download", len(hub.updates))
	}
}

func TestTargetCompletionWinsRaceWithSourceRPCError(t *testing.T) {
	st, model, src, dst, _ := setupModelSyncController(t)
	const transferID = "target-completed-first"
	if err := st.BeginModelSync(store.FileRef{ModelID: model.ID, NodeID: dst.ID}, src.ID, transferID, 12); err != nil {
		t.Fatal(err)
	}
	dstMF, _ := st.ModelFileByModelNode(model.ID, dst.ID)
	if err := st.UpdateModelFileStatus(dst.ID, proto.ModelFileStatus{
		Key: testModelFileKey(t, st, model.ID, dst.ID), State: proto.MFPresent, Path: "/models/Qwen3-copy",
		BytesTotal: 12, BytesDone: 12, TransferID: transferID, Acquisition: proto.ModelAcquirePeer,
	}); err != nil {
		t.Fatal(err)
	}

	copyCtx, cancel := context.WithCancel(context.Background())
	cancel()
	hub := &modelSyncHub{online: map[string]bool{"src": true, "dst": true}, copyErr: context.Canceled}
	c := New(st, hub, nil)
	key := testModelFileKey(t, st, model.ID, dst.ID)
	run := &modelSyncRun{transferID: transferID, cancel: func() {}}
	c.modelSyncs[key] = run
	c.runModelSync(copyCtx, key, run, peerSource{node: src, path: "/models/Qwen3", sizeBytes: 12},
		dst, dstMF, netplan.StoragePath{Network: netplan.ManagementName, SourceIP: "10.0.0.1", TargetIP: "10.0.0.2"})

	got, err := st.ModelFileByModelNode(model.ID, dst.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != proto.MFPresent || got.Downloaded != 12 {
		t.Fatalf("late source RPC error regressed target completion: %+v", got)
	}
}

func TestSyncingTransferIsReconciledAfterControllerRestart(t *testing.T) {
	st, model, src, dst, _ := setupModelSyncController(t)
	if err := st.BeginModelSync(store.FileRef{ModelID: model.ID, NodeID: dst.ID}, src.ID, "persisted-transfer", 12); err != nil {
		t.Fatal(err)
	}
	dstMF, _ := st.ModelFileByModelNode(model.ID, dst.ID)
	hub := &modelSyncHub{online: map[string]bool{"src": true, "dst": true}, called: make(chan struct{}), release: make(chan struct{})}
	c := New(st, hub, nil)
	if !c.trySyncModelFromPeer(context.Background(), dst, model, dstMF) {
		t.Fatal("persisted syncing transfer was not reconciled")
	}
	select {
	case <-hub.called:
	case <-time.After(2 * time.Second):
		t.Fatal("persisted sync did not restart coordination")
	}
	hub.mu.Lock()
	req := hub.requests[0]
	hub.mu.Unlock()
	if req.TransferID != "persisted-transfer" {
		t.Fatalf("transfer ID = %q, want persisted-transfer", req.TransferID)
	}
	close(hub.release)
	waitForHubSync(t, hub)
	got, _ := st.ModelFileByModelNode(model.ID, dst.ID)
	if err := st.UpdateModelFileStatus(dst.ID, proto.ModelFileStatus{
		Key: testModelFileKey(t, st, model.ID, dst.ID), State: proto.MFPresent, TransferID: got.TransferID,
		BytesTotal: 12, BytesDone: 12, Acquisition: proto.ModelAcquirePeer,
	}); err != nil {
		t.Fatal(err)
	}
	c.onEvent(bus.Event{Topic: "modelfile", Key: testModelFileKey(t, st, model.ID, dst.ID)})
	waitForModelSyncIdle(t, c, testModelFileKey(t, st, model.ID, dst.ID))
}

// Canceling stops the copy and leaves the choice to the operator. The copy's
// own goroutine sees its context canceled; it must not take that for a failed
// automatic sync and fall back to the public download. The hub holds the stop
// until that goroutine is done, which is the order that used to lose.
func TestCancelModelSyncStopsCoordinatorAndTarget(t *testing.T) {
	st, model, _, dst, dstMF := setupModelSyncController(t)
	hub := &modelSyncHub{online: map[string]bool{"src": true, "dst": true}, called: make(chan struct{}), release: make(chan struct{})}
	c := New(st, hub, nil)
	if !c.trySyncModelFromPeer(context.Background(), dst, model, dstMF) {
		t.Fatal("expected peer sync")
	}
	select {
	case <-hub.called:
	case <-time.After(2 * time.Second):
		t.Fatal("peer copy did not start")
	}
	got, _ := st.ModelFileByModelNode(model.ID, dst.ID)
	hub.mu.Lock()
	pushedBefore := len(hub.updates)
	hub.onStop = func() { waitForModelSyncIdle(t, c, testModelFileKey(t, st, model.ID, dst.ID)) }
	hub.mu.Unlock()
	canceled, err := c.CancelModelSync(context.Background(), model.ID, dst.UUID)
	if err != nil {
		t.Fatal(err)
	}
	if canceled.State != proto.MFDownloadRequired || canceled.TransferID != got.TransferID {
		t.Fatalf("canceled model file = %+v", canceled)
	}
	waitForModelSyncIdle(t, c, testModelFileKey(t, st, model.ID, dst.ID))
	hub.mu.Lock()
	stops := hub.stops
	hub.mu.Unlock()
	if stops != 1 {
		t.Fatalf("target stop calls = %d, want 1", stops)
	}
	hub.mu.Lock()
	pushed := hub.updates[pushedBefore:]
	hub.mu.Unlock()
	if len(pushed) != 0 {
		t.Fatalf("canceling pushed %d state updates to the node (a public download?): %+v", len(pushed), pushed)
	}
}

func TestControllerAdoptsTargetActiveTransferID(t *testing.T) {
	st, model, src, dst, dstMF := setupModelSyncController(t)
	hub := &modelSyncHub{
		online: map[string]bool{"src": true, "dst": true}, called: make(chan struct{}),
		copyResult: &proto.ModelSyncCopyResult{TransferID: "target-active-transfer", InProgress: true},
	}
	c := New(st, hub, nil)
	if !c.trySyncModelFromPeer(context.Background(), dst, model, dstMF) {
		t.Fatal("expected peer sync")
	}
	select {
	case <-hub.called:
	case <-time.After(2 * time.Second):
		t.Fatal("peer copy did not start")
	}
	deadline := time.Now().Add(2 * time.Second)
	var got *store.ModelFile
	for {
		got, _ = st.ModelFileByModelNode(model.ID, dst.ID)
		if got.TransferID == "target-active-transfer" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("target transfer was not adopted: %+v", got)
		}
		time.Sleep(time.Millisecond)
	}
	if got.State != proto.MFSyncing || got.TransferID != "target-active-transfer" || got.TransferSourceNodeID != src.ID {
		t.Fatalf("adopted target transfer = %+v", got)
	}
	assertModelSyncActive(t, c, testModelFileKey(t, st, model.ID, dst.ID))
	if err := st.UpdateModelFileStatus(dst.ID, proto.ModelFileStatus{
		Key: testModelFileKey(t, st, model.ID, dst.ID), State: proto.MFPresent, TransferID: got.TransferID,
		BytesTotal: 12, BytesDone: 12, Acquisition: proto.ModelAcquirePeer,
	}); err != nil {
		t.Fatal(err)
	}
	c.onEvent(bus.Event{Topic: "modelfile", Key: testModelFileKey(t, st, model.ID, dst.ID)})
	waitForModelSyncIdle(t, c, testModelFileKey(t, st, model.ID, dst.ID))
}

func TestNoPeerFallsBackToPublicDownloadAndExplicitDownloadSkipsPeer(t *testing.T) {
	st, model, _, dst, dstMF := setupModelSyncController(t)
	hub := &modelSyncHub{online: map[string]bool{"src": false, "dst": true}}
	c := New(st, hub, nil)
	if !c.trySyncModelFromPeer(context.Background(), dst, model, dstMF) {
		t.Fatal("no-peer state should fall back to public download")
	}
	got, _ := st.ModelFileByModelNode(model.ID, dst.ID)
	if got.State != proto.MFPending || got.Acquisition != proto.ModelAcquireDownload || hub.requestCount() != 0 || len(hub.updates) != 1 {
		t.Fatalf("no-peer result = %+v, requests=%d", got, hub.requestCount())
	}
	got.Acquisition = proto.ModelAcquireDownload
	if _, err := st.UpsertModelFile(got); err != nil {
		t.Fatal(err)
	}
	if c.trySyncModelFromPeer(context.Background(), dst, model, got) {
		t.Fatal("explicit public-download choice should skip peer sync")
	}
}

func TestNoPeerWithoutSourceRequiresSource(t *testing.T) {
	st, model, _, dst, dstMF := setupModelSyncController(t)
	model.Source, model.SourceID = "", ""
	if err := st.DB().Model(&store.Model{}).Where("id = ?", model.ID).Updates(map[string]any{"source": "", "source_id": ""}).Error; err != nil {
		t.Fatal(err)
	}
	c := New(st, &modelSyncHub{online: map[string]bool{"dst": true}}, nil)
	if !c.trySyncModelFromPeer(context.Background(), dst, model, dstMF) {
		t.Fatal("expected source-required terminal state")
	}
	got, _ := st.ModelFileByModelNode(model.ID, dst.ID)
	if got.State != proto.MFDownloadRequired || !strings.Contains(got.Message, "source_required") {
		t.Fatalf("state=%+v", got)
	}
}

func waitForModelSyncIdle(t *testing.T, c *Controller, key string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		c.syncMu.Lock()
		_, active := c.modelSyncs[key]
		c.syncMu.Unlock()
		if !active {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("model sync did not finish")
		}
		time.Sleep(time.Millisecond)
	}
}

func assertModelSyncActive(t *testing.T, c *Controller, key string) {
	t.Helper()
	c.syncMu.Lock()
	_, active := c.modelSyncs[key]
	c.syncMu.Unlock()
	if !active {
		t.Fatal("model sync activity lock was released before target terminal status")
	}
}

func waitForHubSync(t *testing.T, hub *modelSyncHub) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		hub.mu.Lock()
		syncs := hub.syncs
		hub.mu.Unlock()
		if syncs > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("controller did not request target status replay")
		}
		time.Sleep(time.Millisecond)
	}
}

// A holder that shares no storage-sync network with the target cannot serve
// it: the target downloads from the model's source instead of trying a copy
// that could never connect.
func TestPeerSyncSkipsAHolderWithoutAStoragePath(t *testing.T) {
	st, model, _, dst, dstMF := setupModelSyncController(t)
	if err := st.SetNodeManagementRolesOff("dst", []string{netplan.RoleStorageSync}); err != nil {
		t.Fatal(err)
	}
	hub := &modelSyncHub{online: map[string]bool{"src": true, "dst": true}}
	c := New(st, hub, nil)
	c.trySyncModelFromPeer(context.Background(), dst, model, dstMF)
	if hub.requestCount() != 0 {
		t.Fatalf("started %d copies without a storage path", hub.requestCount())
	}
	got, err := st.ModelFileByModelNode(model.ID, dst.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State == proto.MFSyncing || got.Acquisition == proto.ModelAcquirePeer {
		t.Fatalf("target = %+v, want a download from the source", got)
	}

	// Asked to probe a copy between them, the controller explains why not.
	err = c.ProbeModelSync(context.Background(), model.ID, "dst", "src")
	var product interface{ Code() string }
	if !errors.As(err, &product) || product.Code() != messages.ErrorNoStoragePath {
		t.Fatalf("probe err = %v", err)
	}
}

// testModelFileKey is the protocol key of a model's row on a node in the
// default local storage.
func testModelFileKey(t *testing.T, st *store.Store, modelID, nodeID uint) string {
	t.Helper()
	mf, err := st.ModelFileAt(store.FileRef{ModelID: modelID, NodeID: nodeID})
	if err != nil {
		t.Fatal(err)
	}
	return store.ModelFileKey(mf.ID)
}

// probingHub records the probe a controller sends.
type probingHub struct {
	*modelSyncHub
	probed []proto.ModelSyncCopyRequest
}

func (h *probingHub) ProbeModelPath(_ context.Context, req proto.ModelSyncCopyRequest) error {
	h.probed = append(h.probed, req)
	return nil
}

// A target row left by a path registration holds an absolute path, which the
// receiver refuses. The probe asks for the relative path a real copy would
// use, so it tests the copy that would actually run.
func TestProbeModelSyncAsksForTheCopysTargetPath(t *testing.T) {
	st, model, _, dst, dstMF := setupModelSyncController(t)
	dstMF.Path = "/Users/me/agent/models/huggingface/Qwen3"
	if _, err := st.UpsertModelFile(dstMF); err != nil {
		t.Fatal(err)
	}
	hub := &probingHub{modelSyncHub: &modelSyncHub{online: map[string]bool{"src": true, "dst": true}}}
	c := New(st, hub, nil)
	if err := c.ProbeModelSync(context.Background(), model.ID, dst.UUID, "src"); err != nil {
		t.Fatal(err)
	}
	if len(hub.probed) != 1 || hub.probed[0].TargetPath != relModelPath(model) || !hub.probed[0].AllowRelay {
		t.Fatalf("probe = %+v, want target path %q", hub.probed, relModelPath(model))
	}
}
