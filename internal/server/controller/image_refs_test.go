package controller

import (
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestPushImageRefsRefreshesCatalogAfterDelete(t *testing.T) {
	st, err := store.Open("file:controller_image_refs?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	node, err := st.RegisterNode("image-ref-node", store.HashToken("token"), "image-ref-node")
	if err != nil {
		t.Fatal(err)
	}
	image, err := st.UpsertImage(&store.Image{Name: "custom", ImageRef: "example.com/custom:v1", Source: "custom", Engine: "vllm", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	hub := &distributedTestHub{online: map[string]bool{node.UUID: true}, updates: map[string][]proto.StateUpdate{}}
	controller := New(st, hub, nil)
	controller.pushImageRefs()
	updates := hub.updates[node.UUID]
	if len(updates) != 1 || !stringSliceContains(updates[0].ImageRefs, image.ImageRef) {
		t.Fatalf("initial image refs = %+v", updates)
	}
	if err := st.DeleteImage(image.ID); err != nil {
		t.Fatal(err)
	}
	controller.pushImageRefs()
	updates = hub.updates[node.UUID]
	if len(updates) != 2 || stringSliceContains(updates[1].ImageRefs, image.ImageRef) {
		t.Fatalf("post-delete image refs = %+v", updates)
	}
}

func TestImageNodeEventsDoNotRepushUnchangedRefs(t *testing.T) {
	st, err := store.Open("file:controller_image_node_events?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	node, err := st.RegisterNode("image-event-node", store.HashToken("token"), "image-event-node")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertImage(&store.Image{Name: "custom", ImageRef: "example.com/custom:v1", Source: "custom", Engine: "vllm", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	hub := &distributedTestHub{online: map[string]bool{node.UUID: true}, updates: map[string][]proto.StateUpdate{}}
	controller := New(st, hub, nil)

	asked := func() bool {
		select {
		case <-controller.imageRefsNotify:
			return true
		default:
			return false
		}
	}
	for _, reason := range []string{"node", "syncing", bus.ReasonProgress} {
		controller.onEvent(bus.Event{Topic: "image", Key: "1", Reason: reason})
		if asked() {
			t.Fatalf("image-node %s asked for a refs push", reason)
		}
	}
	controller.onEvent(bus.Event{Topic: "image", Key: "example.com/custom:v1", Reason: "created"})
	if !asked() {
		t.Fatal("a catalog ref mutation did not ask for a refs push")
	}
	if got := len(hub.updates[node.UUID]); got != 0 {
		t.Fatalf("the event pushed %d times itself; the push is the worker's", got)
	}
}

func stringSliceContains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
