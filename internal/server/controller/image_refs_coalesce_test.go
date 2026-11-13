package controller

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// countingHub records the refs handed to each node. Handing them to a node in
// stalled blocks until release is closed, as a hub that waited on the agent's
// write would; entered gets that node's uuid when it blocks.
type countingHub struct {
	*distributedTestHub
	mu      sync.Mutex
	pushes  map[string][]proto.StateUpdate
	stalled map[string]bool
	release chan struct{}
	entered chan string
}

func (h *countingHub) RequestImageRefs(uuid string, refs []string) {
	h.mu.Lock()
	stalled := h.stalled[uuid]
	h.mu.Unlock()
	if stalled {
		h.entered <- uuid
		<-h.release
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.pushes[uuid] = append(h.pushes[uuid], proto.StateUpdate{ImageRefs: refs})
}

func (h *countingHub) pushesTo(uuid string) []proto.StateUpdate {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]proto.StateUpdate(nil), h.pushes[uuid]...)
}

func imageRefsFixture(t *testing.T, name string, nodes int) (*store.Store, *countingHub, []string) {
	t.Helper()
	st, err := store.Open("file:"+name+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	hub := &countingHub{
		distributedTestHub: &distributedTestHub{online: map[string]bool{}, updates: map[string][]proto.StateUpdate{}},
		pushes:             map[string][]proto.StateUpdate{}, stalled: map[string]bool{},
		release: make(chan struct{}), entered: make(chan string, nodes),
	}
	var uuids []string
	for i := 0; i < nodes; i++ {
		uuid := fmt.Sprintf("%s-node-%02d", name, i)
		if _, err := st.RegisterNode(uuid, store.HashToken(uuid), uuid); err != nil {
			t.Fatal(err)
		}
		hub.online[uuid] = true
		uuids = append(uuids, uuid)
	}
	return st, hub, uuids
}

// startImageRefsWorker runs the controller's refs worker until the test ends.
func startImageRefsWorker(t *testing.T, c *Controller) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); c.imageRefsWorker(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
}

// importImages upserts images from the first to the last index, then hands
// the controller every event they published at once, as its event loop gets a
// burst: the timing of the upserts does not split the import.
func importImages(t *testing.T, st *store.Store, c *Controller, first, last int) int {
	t.Helper()
	sub := st.Bus.Subscribe(1024, "image")
	defer sub.Close()
	for i := first; i <= last; i++ {
		if _, err := st.UpsertImage(&store.Image{ImageRef: fmt.Sprintf("registry.example.com/vllm/vllm-openai:v0.%d.0", i), Engine: "vllm", Source: "custom", Enabled: true}); err != nil {
			t.Fatal(err)
		}
	}
	var events []bus.Event
	for {
		select {
		case ev := <-sub.C():
			events = append(events, ev)
			continue
		case <-time.After(100 * time.Millisecond):
		}
		break
	}
	for _, ev := range events {
		c.onEvent(ev)
	}
	return len(events)
}

// waitPushes waits until the node has n pushes, failing after a while.
func waitPushes(t *testing.T, hub *countingHub, uuid string, n int) []proto.StateUpdate {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		got := hub.pushesTo(uuid)
		if len(got) >= n || time.Now().After(deadline) {
			return got
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Importing a batch of images comes to one push of the whole list per node,
// not one per image and series event: before, 40 images into 50 nodes made
// 4000 pushes, each re-reading every image and node.
func TestAnImageImportPushesTheRefsOncePerNode(t *testing.T) {
	const images = 40
	st, hub, nodes := imageRefsFixture(t, "refs-import", 5)
	c := New(st, hub, nil)
	startImageRefsWorker(t, c)

	if n := importImages(t, st, c, 0, images-1); n < images {
		t.Fatalf("the import published %d events, want at least one per image", n)
	}
	for _, uuid := range nodes {
		waitPushes(t, hub, uuid, 1)
		time.Sleep(2 * imageRefsSettle) // a second push would have come by now
		got := hub.pushesTo(uuid)
		if len(got) != 1 || len(got[0].ImageRefs) != images {
			t.Fatalf("node %s got %d refs pushes (last with %d refs) for one import, want 1 with all %d", uuid, len(got), len(got[len(got)-1].ImageRefs), images)
		}
	}
	// A later change is pushed too: the worker goes on after a push.
	importImages(t, st, c, images, images)
	got := waitPushes(t, hub, nodes[0], 2)
	if len(got) != 2 || len(got[1].ImageRefs) != images+1 {
		t.Fatalf("after a second import node got %d pushes, last with %d refs; want 2, %d", len(got), len(got[len(got)-1].ImageRefs), images+1)
	}
}

// The push happens off the controller's event loop: even a hand-off that
// blocks holds up the refs worker, not the handling of events. Before, the
// push ran inside onEvent, so each stalled write held every event behind it
// for up to its 30 s timeout.
func TestAStalledRefsPushDoesNotHoldUpEvents(t *testing.T) {
	st, hub, nodes := imageRefsFixture(t, "refs-stall", 2)
	hub.stalled[nodes[0]] = true
	c := New(st, hub, nil)
	startImageRefsWorker(t, c)
	t.Cleanup(func() { close(hub.release) })

	handle := func(evs ...bus.Event) {
		t.Helper()
		handled := make(chan struct{})
		go func() {
			for _, ev := range evs {
				c.onEvent(ev)
			}
			close(handled)
		}()
		select {
		case <-handled:
		case <-time.After(time.Second):
			t.Fatal("events waited for a stalled refs push")
		}
	}
	handle(bus.Event{Topic: "image", Key: "example.com/a:v1", Reason: "created"})
	select {
	case <-hub.entered: // the stalled node's write is under way
	case <-time.After(3 * time.Second):
		t.Fatal("no refs push started")
	}
	handle(bus.Event{Topic: "image", Key: "example.com/b:v1", Reason: "created"},
		bus.Event{Topic: "deployment", Key: "1", Reason: "updated"})
}
