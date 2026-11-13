package controller

import (
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func pendingKeys(c *Controller) []string {
	c.q.mu.Lock()
	defer c.q.mu.Unlock()
	keys := make([]string, 0, len(c.q.pending))
	for key := range c.q.pending {
		keys = append(keys, key)
	}
	return keys
}

func drainQueue(c *Controller) {
	c.q.mu.Lock()
	c.q.pending = map[string]time.Time{}
	c.q.mu.Unlock()
}

// A downloading model, a pulling image and a instance mid-transfer must not
// re-plan anything. Before this, one node downloading weights re-queued every
// deployment in the cluster four times a second.
func TestProgressNotificationsDoNotQueueReconciles(t *testing.T) {
	st, err := store.Open("file:controller_progress_reason?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	node, err := st.RegisterNode("progress-node", store.HashToken("progress-token"), "progress-node")
	if err != nil {
		t.Fatal(err)
	}
	dep, err := st.CreateDeployment(&store.Deployment{ModelID: 1, NodeID: node.ID, State: "active"})
	if err != nil {
		t.Fatal(err)
	}
	inst, err := st.CreateInstance(&store.Instance{UUID: "progress-instance", DeploymentID: dep.ID, NodeID: node.ID, State: proto.InstPulling})
	if err != nil {
		t.Fatal(err)
	}
	hub := &distributedTestHub{online: map[string]bool{node.UUID: true}, updates: map[string][]proto.StateUpdate{}}
	c := New(st, hub, nil)

	for _, ev := range []bus.Event{
		{Topic: "instance", Key: inst.UUID, Reason: bus.ReasonProgress},
		{Topic: "modelfile", Key: "1:1", Reason: bus.ReasonProgress},
		{Topic: "image", Key: "1", Reason: bus.ReasonProgress},
	} {
		c.onEvent(ev)
		c.flushEventRoutes()
		if keys := pendingKeys(c); len(keys) != 0 {
			t.Fatalf("%s progress queued %v", ev.Topic, keys)
		}
	}
	if got := len(hub.updates[node.UUID]); got != 0 {
		t.Fatalf("image progress repushed the catalog refs %d times", got)
	}

	// The same topics still reconcile when the state itself moved.
	c.onEvent(bus.Event{Topic: "instance", Key: inst.UUID, Reason: proto.InstRunning})
	if keys := pendingKeys(c); len(keys) != 1 {
		t.Fatalf("instance state change queued %v, want its deployment", keys)
	}
	drainQueue(c)
	c.onEvent(bus.Event{Topic: "modelfile", Key: "1:1", Reason: proto.MFPresent})
	c.flushEventRoutes()
	if keys := pendingKeys(c); len(keys) != 1 {
		t.Fatalf("model presence queued %v, want the waiting deployment", keys)
	}
}
