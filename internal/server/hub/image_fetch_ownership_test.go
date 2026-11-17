package hub

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/imagecatalog"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestLateImageStartFailureCannotFailRetry(t *testing.T) {
	firstRPC, release := make(chan string, 1), make(chan struct{})
	var calls atomic.Int32
	var once sync.Once
	resume := func() { once.Do(func() { close(release) }) }
	defer resume()
	h := newArchiveURLHub(t, "file:"+t.Name()+"?mode=memory&cache=shared", func(env proto.Envelope) string {
		var req struct {
			Params proto.ImageLoadPathParams `json:"params"`
		}
		if err := json.Unmarshal(env.D, &req); err != nil {
			return `{"error":"bad RPC"}`
		}
		if calls.Add(1) == 1 {
			firstRPC <- req.Params.TransferID
			<-release
			return `{"error":"late failure from first request"}`
		}
		return `{"result":{"state":"syncing"}}`
	})
	fetch := imagecatalog.FetchRequest{Source: "local", ArchivePath: "/data/image.tar"}
	type result struct {
		row *store.ImageNode
		err error
	}
	first, second := make(chan result, 1), make(chan result, 1)
	go func() {
		row, err := h.hub.PullImage(context.Background(), h.image.ID, h.node.UUID, fetch)
		first <- result{row, err}
	}()
	var firstID string
	select {
	case firstID = <-firstRPC:
	case <-time.After(5 * time.Second):
		t.Fatal("first RPC not reached")
	}
	if _, err := h.store.CancelImageNodeTransferIfActive(h.image.ID, h.node.ID, firstID, "cancelled", ""); err != nil {
		t.Fatal(err)
	}
	claimed := make(chan struct{}, 1)
	if err := h.store.DB().Callback().Update().After("gorm:update").Register("test:retry_claimed", func(tx *gorm.DB) {
		if fields, ok := tx.Statement.Dest.(map[string]any); ok && fields["state"] == store.ImageStateSyncing && fields["transfer_id"] != firstID && tx.RowsAffected == 1 {
			claimed <- struct{}{}
		}
	}); err != nil {
		t.Fatal(err)
	}
	go func() {
		row, err := h.hub.PullImage(context.Background(), h.image.ID, h.node.UUID, fetch)
		second <- result{row, err}
	}()
	select {
	case <-claimed:
	case <-time.After(5 * time.Second):
		t.Fatal("retry not claimed")
	}
	resume()
	select {
	case got := <-first:
		if got.err == nil || !strings.Contains(got.err.Error(), "late failure") {
			t.Fatalf("first result=%+v %v", got.row, got.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("first RPC did not return")
	}
	select {
	case got := <-second:
		if got.err != nil {
			t.Fatal(got.err)
		}
		current, err := h.store.ImageNode(h.image.ID, h.node.ID)
		if err != nil || current.State != store.ImageStateSyncing || current.TransferID != got.row.TransferID || current.TransferID == firstID {
			t.Fatalf("retry overwritten: current=%+v result=%+v err=%v", current, got.row, err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("retry RPC did not return")
	}
}

func TestOldImagePullWatcherCannotAdvanceReplacement(t *testing.T) {
	for _, state := range []string{store.ImageStatePulling, store.ImageStateFailed} {
		t.Run(state, func(t *testing.T) {
			var calls atomic.Int32
			h := newArchiveURLHub(t, "file:"+t.Name()+"?mode=memory&cache=shared", func(proto.Envelope) string { calls.Add(1); return `{"result":{}}` })
			row := &store.ImageNode{ImageID: h.image.ID, NodeID: h.node.ID, State: state, TransferID: "new", Source: "local"}
			if state == store.ImageStateFailed {
				row.TransferID = ""
				row.LastTransferID = "new"
			}
			if _, err := h.store.UpsertImageNode(row); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			h.hub.watchImagePullCandidates(ctx, h.image.ID, h.node.ID, h.node.UUID, h.image.ImageRef, "old", []imagePullCandidate{
				{ImagePullCandidate: proto.ImagePullCandidate{Ref: "registry.example/first:1"}, Source: "first"},
				{ImagePullCandidate: proto.ImagePullCandidate{Ref: "registry.example/second:1"}, Source: "second"},
			}, 0)
			current, err := h.store.ImageNode(h.image.ID, h.node.ID)
			if err != nil || calls.Load() != 0 || current.Source != "local" || current.State != state {
				t.Fatalf("old watcher changed replacement: %+v calls=%d err=%v", current, calls.Load(), err)
			}
		})
	}
}

func TestCancelImagePullAlsoStopsItsCandidateWatcher(t *testing.T) {
	var methods []string
	var mu sync.Mutex
	h := newArchiveURLHub(t, "file:"+t.Name()+"?mode=memory&cache=shared", func(env proto.Envelope) string {
		var req struct {
			Method string `json:"method"`
		}
		_ = json.Unmarshal(env.D, &req)
		mu.Lock()
		methods = append(methods, req.Method)
		mu.Unlock()
		return `{"result":{}}`
	})
	if _, err := h.store.UpsertImageNode(&store.ImageNode{ImageID: h.image.ID, NodeID: h.node.ID, State: store.ImageStatePulling, TransferID: "attempt"}); err != nil {
		t.Fatal(err)
	}
	ctx, release := h.hub.ownImagePullFallback(context.Background(), "attempt")
	defer release()
	if _, err := h.hub.CancelImageTransfer(context.Background(), h.image.ID, h.node.UUID); err != nil {
		t.Fatal(err)
	}
	if ctx.Err() != context.Canceled {
		t.Fatal("cancel did not reach the candidate owner")
	}
	h.hub.watchImagePullCandidates(ctx, h.image.ID, h.node.ID, h.node.UUID, h.image.ImageRef, "attempt", []imagePullCandidate{
		{ImagePullCandidate: proto.ImagePullCandidate{Ref: "registry.example/first:1"}},
		{ImagePullCandidate: proto.ImagePullCandidate{Ref: "registry.example/second:1"}},
	}, 0)
	mu.Lock()
	defer mu.Unlock()
	if len(methods) != 1 || methods[0] != proto.RPCImageCancel {
		t.Fatalf("cancelled watcher started another candidate: %v", methods)
	}
}

func TestImageFetchReadFailureDoesNotDispatch(t *testing.T) {
	var calls atomic.Int32
	h := newArchiveURLHub(t, "file:"+t.Name()+"?mode=memory&cache=shared", func(proto.Envelope) string { calls.Add(1); return `{"result":{}}` })
	if err := h.store.DB().Callback().Query().Before("gorm:query").Register("test:read_failure", func(tx *gorm.DB) {
		if tx.Statement.Schema != nil && tx.Statement.Schema.Name == "ImageNode" {
			tx.AddError(context.DeadlineExceeded)
		}
	}); err != nil {
		t.Fatal(err)
	}
	_, err := h.hub.PullImage(context.Background(), h.image.ID, h.node.UUID, imagecatalog.FetchRequest{Source: "local", ArchivePath: "/data/image.tar"})
	if err == nil || calls.Load() != 0 {
		t.Fatalf("read error became a start: %v calls=%d", err, calls.Load())
	}
}

func TestImageFetchRechecksHeartbeatBeforeTakingOver(t *testing.T) {
	var calls atomic.Int32
	h := newArchiveURLHub(t, "file:"+t.Name()+"?mode=memory&cache=shared", func(proto.Envelope) string {
		calls.Add(1)
		return `{"result":{}}`
	})
	if _, err := h.store.UpsertImageNode(&store.ImageNode{ImageID: h.image.ID, NodeID: h.node.ID, State: store.ImageStateSyncing, TransferID: "alive"}); err != nil {
		t.Fatal(err)
	}
	if err := h.store.DB().Model(&store.ImageNode{}).Where("node_id = ?", h.node.ID).Update("updated_at", time.Now().Add(-time.Hour)).Error; err != nil {
		t.Fatal(err)
	}
	ready, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	resume := func() { once.Do(func() { close(release) }) }
	defer resume()
	var paused atomic.Bool
	if err := h.store.DB().Callback().Query().After("gorm:query").Register("test:stale_observation", func(tx *gorm.DB) {
		if _, ok := tx.Statement.Dest.(*store.ImageNode); ok && paused.CompareAndSwap(false, true) {
			close(ready)
			<-release
		}
	}); err != nil {
		t.Fatal(err)
	}
	type result struct {
		row *store.ImageNode
		err error
	}
	done := make(chan result, 1)
	go func() {
		row, err := h.hub.PullImage(context.Background(), h.image.ID, h.node.UUID, imagecatalog.FetchRequest{Source: "local", ArchivePath: "/data/image.tar"})
		done <- result{row, err}
	}()
	select {
	case <-ready:
	case <-time.After(5 * time.Second):
		t.Fatal("request did not observe the stale row")
	}
	if err := h.store.ApplyImageNodeStatus(h.image.ID, h.node.ID, proto.ImageNodeStatus{State: store.ImageStateSyncing, TransferID: "alive"}); err != nil {
		t.Fatal(err)
	}
	resume()
	select {
	case got := <-done:
		if got.err != nil || got.row.TransferID != "alive" || calls.Load() != 0 {
			t.Fatalf("heartbeat lost to stale admission: %+v %v calls=%d", got.row, got.err, calls.Load())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("request did not reconsider the fresh heartbeat")
	}
}
