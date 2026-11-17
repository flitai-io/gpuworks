package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/imagecatalog"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// Both callers finish their real reads before either may write. This controls
// a legal interleaving without replacing any database or RPC return values.
func pauseImageReads(t *testing.T, h *archiveURLHub) (<-chan struct{}, func()) {
	t.Helper()
	ready, release := make(chan struct{}, 2), make(chan struct{})
	var arrivals atomic.Int32
	var once sync.Once
	resume := func() { once.Do(func() { close(release) }) }
	t.Cleanup(resume)
	if err := h.store.DB().Callback().Query().After("gorm:query").Register("test:concurrent_image_reads", func(tx *gorm.DB) {
		if _, single := tx.Statement.Dest.(*store.ImageNode); single && arrivals.Add(1) <= 2 {
			ready <- struct{}{}
			<-release
		}
	}); err != nil {
		t.Fatal(err)
	}
	return ready, resume
}

func TestConcurrentLocalImageImportsStartOneAttempt(t *testing.T) {
	for _, pair := range [][2]string{{"local", "local"}, {"local", "url"}, {"url", "url"}, {"local", "registry"}, {"registry", "registry"}} {
		for _, existing := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s-%s/existing=%v", pair[0], pair[1], existing), func(t *testing.T) {
				calls := make(chan string, 4)
				h := newArchiveURLHub(t, "file:"+t.Name()+"?mode=memory&cache=shared", func(env proto.Envelope) string {
					var req struct {
						Method string `json:"method"`
						Params struct {
							TransferID string `json:"transfer_id"`
						} `json:"params"`
					}
					if err := json.Unmarshal(env.D, &req); err != nil {
						return `{"error":"bad RPC"}`
					}
					calls <- req.Params.TransferID
					return `{"result":{"state":"syncing"}}`
				})
				if existing {
					if _, err := h.store.UpsertImageNode(&store.ImageNode{ImageID: h.image.ID, NodeID: h.node.ID, State: store.ImageStateFailed, LastTransferID: "previous"}); err != nil {
						t.Fatal(err)
					}
				}
				ready, resume := pauseImageReads(t, h)
				type result struct {
					row *store.ImageNode
					err error
				}
				results := make(chan result, 2)
				for _, source := range pair {
					go func() {
						fetch := imagecatalog.FetchRequest{}
						switch source {
						case "local":
							fetch.Source, fetch.ArchivePath = "local", "/data/image.tar"
						case "url":
							fetch.Source, fetch.ArchiveURL = "url", h.url
						}
						row, err := h.hub.PullImage(context.Background(), h.image.ID, h.node.UUID, fetch)
						results <- result{row, err}
					}()
				}
				for range 2 {
					select {
					case <-ready:
					case <-time.After(5 * time.Second):
						t.Fatal("did not reach both reads")
					}
				}
				resume()
				var transferID string
				for range 2 {
					select {
					case got := <-results:
						if got.err != nil {
							t.Fatal(got.err)
						}
						if got.row.TransferID == "" || (transferID != "" && transferID != got.row.TransferID) {
							t.Fatalf("requests acknowledged different attempts: %q and %+v", transferID, got.row)
						}
						transferID = got.row.TransferID
					case <-time.After(5 * time.Second):
						t.Fatal("concurrent request stalled")
					}
				}
				if len(calls) != 1 {
					t.Fatalf("dispatched %d image loads, want one", len(calls))
				}
				if id := <-calls; id != transferID {
					t.Fatalf("RPC id=%q response id=%q", id, transferID)
				}
				current, err := h.store.ImageNode(h.image.ID, h.node.ID)
				if err != nil || current.TransferID != transferID {
					t.Fatalf("current=%+v err=%v", current, err)
				}
			})
		}
	}
}

func TestPeerImageStartCannotReplaceConcurrentLocalImport(t *testing.T) {
	for _, force := range []bool{false, true} {
		t.Run(fmt.Sprintf("force=%v", force), func(t *testing.T) {
			h := newArchiveURLHub(t, "file:"+t.Name()+"?mode=memory&cache=shared", nil)
			source, err := h.store.RegisterNode("peer", store.HashToken("peer-token"), "peer")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := h.store.UpsertImageNode(&store.ImageNode{ImageID: h.image.ID, NodeID: source.ID, State: store.ImageStatePresent}); err != nil {
				t.Fatal(err)
			}
			h.hub.mu.Lock()
			h.hub.sessions[source.UUID] = &Session{NodeUUID: source.UUID}
			h.hub.mu.Unlock()
			ready, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			resume := func() { once.Do(func() { close(release) }) }
			defer resume()
			if err := h.store.DB().Callback().Query().After("gorm:query").Register("test:peer_sources_checked", func(tx *gorm.DB) {
				if _, ok := tx.Statement.Dest.(*[]store.ImageNode); ok {
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
			peer := make(chan result, 1)
			go func() {
				row, err := h.hub.syncImageDirect(context.Background(), h.image.ID, h.node.UUID, source.UUID, 0, force)
				peer <- result{row, err}
			}()
			select {
			case <-ready:
			case <-time.After(5 * time.Second):
				t.Fatal("peer did not validate its source")
			}
			local, err := h.hub.PullImage(context.Background(), h.image.ID, h.node.UUID, imagecatalog.FetchRequest{Source: "local", ArchivePath: "/data/image.tar"})
			if err != nil {
				t.Fatal(err)
			}
			resume()
			select {
			case got := <-peer:
				if got.err != nil || got.row.TransferID != local.TransferID || got.row.Source != "local" {
					t.Fatalf("peer replaced local transfer: %+v %v", got.row, got.err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("peer request stalled")
			}
		})
	}
}
