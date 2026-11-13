package controller

import (
	"fmt"
	"sort"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// Routine node status reports - every agent, once a minute - are acted on in
// batches and only for deployments that could gain from them: one waiting for
// a node, one whose replica is starting, a stopped one still winding down, a
// multi-node one. A deployment running exactly its replicas is left alone.
// Any other node change reconciles, at the next routing tick, the
// deployments with a replica on that node and every unsettled one.
func TestNodeStatusReportsReconcileOnlyUnsettledDeployments(t *testing.T) {
	st, err := store.Open("file:controller_node_status?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	node, err := st.RegisterNode("status-node", store.HashToken("status-node"), "status-node")
	if err != nil {
		t.Fatal(err)
	}
	deployment := func(name string, d store.Deployment, states ...string) uint {
		t.Helper()
		d.Name, d.ModelID, d.NodeID = name, 1, node.ID
		if d.State == "" {
			d.State = store.DeployActive
		}
		dep, err := st.CreateDeployment(&d)
		if err != nil {
			t.Fatal(err)
		}
		for i, state := range states {
			if _, err := st.CreateInstance(&store.Instance{UUID: fmt.Sprintf("%s-%d", name, i), DeploymentID: dep.ID, NodeID: node.ID, State: state}); err != nil {
				t.Fatal(err)
			}
		}
		return dep.ID
	}
	settled := deployment("settled", store.Deployment{Replicas: 2}, proto.InstRunning, proto.InstRunning, proto.InstFailed)
	stopped := deployment("stopped", store.Deployment{State: store.DeployStopped}, proto.InstStopped)
	waiting := deployment("waiting", store.Deployment{Replicas: 2}, proto.InstRunning)
	starting := deployment("starting", store.Deployment{Replicas: 1}, proto.InstStarting)
	winding := deployment("winding", store.Deployment{State: store.DeployStopped}, proto.InstRunning)
	multi := deployment("multi", store.Deployment{Replicas: 1, NNodes: 2}, proto.InstRunning, proto.InstRunning)
	pd := deployment("pd", store.Deployment{Replicas: 1, Placement: `{"pd":{"prefill":{"instances":[{"node_uuid":"status-node"}]},"decode":{"instances":[{"node_uuid":"status-node"}]}}}`}, proto.InstRunning)
	legacy := deployment("legacy", store.Deployment{Replicas: 0}, proto.InstRunning)

	c := New(st, &distributedTestHub{online: map[string]bool{}, updates: map[string][]proto.StateUpdate{}}, nil)
	c.onEvent(bus.Event{Topic: "node", Key: node.UUID, Reason: store.NodeChangeStatus})
	if keys := pendingKeys(c); len(keys) != 0 {
		t.Fatalf("a status report queued %v at once", keys)
	}
	c.settleNodeStatus()
	got := pendingKeys(c)
	sort.Strings(got)
	want := []string{depKey(waiting), depKey(starting), depKey(winding), depKey(multi), depKey(pd)}
	sort.Strings(want)
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("queued %v, want the unsettled %v (settled %d, stopped %d, legacy %d left alone)", got, want, settled, stopped, legacy)
	}

	drainQueue(c)
	c.settleNodeStatus()
	if keys := pendingKeys(c); len(keys) != 0 {
		t.Fatalf("a tick with no report since queued %v", keys)
	}

	c.onEvent(bus.Event{Topic: "node", Key: node.UUID, Reason: "state:online"})
	c.flushEventRoutes()
	if keys := pendingKeys(c); len(keys) != 8 {
		t.Fatalf("a node coming online queued %d deployments, want all 8 (each has an instance on it)", len(keys))
	}
}

// At the largest scale the product supports, a batch of reports costs two
// queries, not a reconcile of every deployment: three thousand settled
// deployments are left alone and the few waiting ones are queued.
func TestNodeStatusSettleAtThreeThousandDeployments(t *testing.T) {
	st, err := store.Open("file:controller_node_status_scale?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	deps := make([]store.Deployment, 3000)
	for i := range deps {
		deps[i] = store.Deployment{Name: fmt.Sprintf("d-%d", i), ModelID: 1, NodeID: 1, State: store.DeployActive, Replicas: 1}
	}
	if err := st.DB().CreateInBatches(&deps, 500).Error; err != nil {
		t.Fatal(err)
	}
	insts := make([]store.Instance, 0, len(deps))
	for i, d := range deps {
		if i%100 == 0 {
			continue // every hundredth deployment waits for a node
		}
		insts = append(insts, store.Instance{UUID: fmt.Sprintf("i-%d", i), DeploymentID: d.ID, NodeID: 1, State: proto.InstRunning})
	}
	if err := st.DB().CreateInBatches(&insts, 500).Error; err != nil {
		t.Fatal(err)
	}
	c := New(st, &distributedTestHub{online: map[string]bool{}, updates: map[string][]proto.StateUpdate{}}, nil)
	for i := 0; i < 1000; i++ {
		c.onEvent(bus.Event{Topic: "node", Key: fmt.Sprintf("n-%d", i), Reason: store.NodeChangeStatus})
	}
	start := time.Now()
	c.settleNodeStatus()
	took := time.Since(start)
	if keys := pendingKeys(c); len(keys) != 30 {
		t.Fatalf("queued %d deployments, want the 30 waiting", len(keys))
	}
	t.Logf("a thousand reports over 3000 deployments settled in %v", took)
	if took > 2*time.Second {
		t.Fatalf("settling took %v", took)
	}
}
