package hub

import (
	"encoding/json"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestQueuedImageSnapshotCannotOverwriteNewerOperation(t *testing.T) {
	for _, alreadyPopped := range []bool{false, true} {
		for _, completed := range []string{store.ImageStatePresent, store.ImageStateAbsent} {
			t.Run(completed+map[bool]string{false: "/pending", true: "/already-popped"}[alreadyPopped], func(t *testing.T) {
				h := newTestHub(t)
				uuid, _ := newTestNode(t, h.store)
				node, err := h.store.NodeByUUID(uuid)
				if err != nil {
					t.Fatal(err)
				}
				image, err := h.store.UpsertImage(&store.Image{ImageRef: "owned:order", Engine: "vllm", Enabled: true})
				if err != nil {
					t.Fatal(err)
				}
				if _, err = h.store.UpsertImageNode(&store.ImageNode{ImageVersionID: image.ID, ImageID: image.ID, NodeID: node.ID, State: store.ImageStateAbsent}); err != nil {
					t.Fatal(err)
				}
				sess := &Session{hub: h, NodeUUID: uuid, closed: make(chan struct{})}
				h.register(sess)
				// Do not start the worker: pop is the deterministic barrier between
				// receipt and persistence, including an item already removed from pending.
				old := proto.NodeStatus{Sys: proto.SysInfo{Hostname: "other-facts-survive"}, Images: []proto.ImagePresence{{ImageRef: image.ImageRef, Present: completed == store.ImageStateAbsent}}}
				raw, err := json.Marshal(old)
				if err != nil {
					t.Fatal(err)
				}
				sess.handleStatusNode(raw)
				apply := func() {
					id, payload, ok := h.statusQ.pop()
					if !ok {
						t.Fatal("missing queued report")
					}
					if err := h.statusQ.apply(id, payload); err != nil {
						t.Fatal(err)
					}
				}
				if alreadyPopped {
					id, payload, ok := h.statusQ.pop()
					if !ok {
						t.Fatal("missing queued report")
					}
					apply = func() {
						if err := h.statusQ.apply(id, payload); err != nil {
							t.Fatal(err)
						}
					}
				}
				event, err := json.Marshal(proto.ImageNodeStatus{ImageRef: image.ImageRef, State: completed, ImageID: "sha256:owned"})
				if err != nil {
					t.Fatal(err)
				}
				sess.handleImageStatus(event)
				apply()
				sess.drainReports() // the operation persists after the queued inventory
				row, err := h.store.ImageNode(image.ID, node.ID)
				if err != nil {
					t.Fatal(err)
				}
				if row.State != completed {
					t.Fatalf("queued inventory overwrote %s with %s", completed, row.State)
				}
				saved, err := h.store.NodeByUUID(uuid)
				if err != nil {
					t.Fatal(err)
				}
				if saved.Hostname != "other-facts-survive" {
					t.Fatalf("unrelated status lost: %q", saved.Hostname)
				}
				// A genuinely fresh inventory is still allowed to change presence.
				sess.handleStatusNode(raw)
				id, payload, ok := h.statusQ.pop()
				if !ok {
					t.Fatal("missing fresh report")
				}
				if err := h.statusQ.apply(id, payload); err != nil {
					t.Fatal(err)
				}
				row, err = h.store.ImageNode(image.ID, node.ID)
				if err != nil {
					t.Fatal(err)
				}
				want := store.ImageStateAbsent
				if completed == store.ImageStateAbsent {
					want = store.ImageStatePresent
				}
				if row.State != want {
					t.Fatalf("fresh inventory state=%s want=%s", row.State, want)
				}
			})
		}
	}
}

func TestStatusReceiptDoesNotWaitForImagePersistence(t *testing.T) {
	h := newTestHub(t)
	s := &Session{hub: h, NodeUUID: "owned"}
	s.imageStatusMu.Lock()
	defer s.imageStatusMu.Unlock()
	done := make(chan struct{})
	go func() { s.handleStatusNode(json.RawMessage(`{"sys":{"hostname":"nonblocking"}}`)); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("status receipt blocked behind image persistence")
	}
	_, queued, ok := h.statusQ.pop()
	if !ok || queued.status.Sys.Hostname != "nonblocking" {
		t.Fatalf("receipt lost payload: %+v", queued)
	}
}

func TestQueuedStatusCannotApplyToReplacementSession(t *testing.T) {
	h := newTestHub(t)
	uuid, _ := newTestNode(t, h.store)
	old := &Session{hub: h, NodeUUID: uuid, closed: make(chan struct{})}
	h.register(old)
	old.handleStatusNode(json.RawMessage(`{"sys":{"hostname":"old-session"}}`))
	id, queued, ok := h.statusQ.pop()
	if !ok {
		t.Fatal("missing pending report")
	}
	h.deregister(old)
	current := &Session{hub: h, NodeUUID: uuid, closed: make(chan struct{})}
	h.register(current)
	if err := h.statusQ.apply(id, queued); err != nil {
		t.Fatal(err)
	}
	saved, err := h.store.NodeByUUID(uuid)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Hostname == "old-session" {
		t.Fatal("replaced session overwrote current node facts")
	}
}
