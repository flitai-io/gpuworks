package controller

import (
	"context"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func activeInstances(t *testing.T, st *store.Store, depID uint) []store.Instance {
	t.Helper()
	insts, err := st.InstancesForDeployment(depID)
	if err != nil {
		t.Fatal(err)
	}
	var out []store.Instance
	for _, inst := range insts {
		if planning.InstanceActive(inst.State) || inst.State == proto.InstDraining {
			out = append(out, inst)
		}
	}
	return out
}

func setRunning(t *testing.T, st *store.Store, uuid string) {
	t.Helper()
	if err := st.DB().Model(&store.Instance{}).Where("uuid = ?", uuid).Update("state", proto.InstRunning).Error; err != nil {
		t.Fatal(err)
	}
}

// Draining a node moves a pool replica without a moment of lower capacity:
// the replica keeps serving while its replacement starts on another node,
// and only once the replacement runs does it drain. The replacement never
// lands on the draining node.
func TestDrainReplacesAPoolReplicaBeforeRetiringIt(t *testing.T) {
	st, hub, nodes, model := poolFixture(t, "drain_room", 2, 1)
	dep := poolDeployment(t, st, model, nodes[0].ID, 1, store.PlacementSpread, map[string][]string{"gpuworks.ai/vendor": {"nvidia"}})
	c := New(st, hub, nil)
	ctx := context.Background()
	if err := c.reconcileDeployment(ctx, dep.ID); err != nil {
		t.Fatal(err)
	}
	insts := activeInstances(t, st, dep.ID)
	if len(insts) != 1 {
		t.Fatalf("placed %d replicas", len(insts))
	}
	original := insts[0]
	setRunning(t, st, original.UUID)
	host, err := st.NodeByID(original.NodeID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.SetNodeScheduling(host.UUID, store.NodeDraining); err != nil {
		t.Fatal(err)
	}

	if err := c.reconcileDeployment(ctx, dep.ID); err != nil {
		t.Fatal(err)
	}
	insts = activeInstances(t, st, dep.ID)
	var replacement *store.Instance
	for i := range insts {
		switch {
		case insts[i].UUID == original.UUID && insts[i].State != proto.InstRunning:
			t.Fatalf("the original stopped serving before its replacement ran: %s", insts[i].State)
		case insts[i].UUID != original.UUID:
			replacement = &insts[i]
		}
	}
	if replacement == nil || replacement.NodeID == host.ID || len(insts) != 2 {
		t.Fatalf("after the drain began: %+v, want the original plus a replacement on another node", insts)
	}

	// Still starting: the original keeps serving and nothing more launches.
	if err := c.reconcileDeployment(ctx, dep.ID); err != nil {
		t.Fatal(err)
	}
	if insts = activeInstances(t, st, dep.ID); len(insts) != 2 {
		t.Fatalf("a starting replacement was surged again: %+v", insts)
	}

	setRunning(t, st, replacement.UUID)
	if err := c.reconcileDeployment(ctx, dep.ID); err != nil {
		t.Fatal(err)
	}
	moved, err := st.InstanceByUUID(original.UUID)
	if err != nil {
		t.Fatal(err)
	}
	if moved.State != proto.InstDraining || moved.Message != drainMessage {
		t.Fatalf("original = %s %q, want draining once its replacement runs", moved.State, moved.Message)
	}
	if again, _ := st.InstanceByUUID(replacement.UUID); again.State != proto.InstRunning {
		t.Fatalf("replacement = %s", again.State)
	}
}

// With no other node that can take it - a one-node pool, or a deployment
// pinned to the draining node - the replica keeps serving where it is and
// the wait says the node is cordoned.
func TestDrainWithNowhereToGoKeepsTheReplicaServing(t *testing.T) {
	for name, pinned := range map[string]bool{"drain_alone": false, "drain_pinned": true} {
		t.Run(name, func(t *testing.T) {
			nodeCount := 1
			if pinned {
				nodeCount = 2
			}
			st, hub, nodes, model := poolFixture(t, name, nodeCount, 1)
			reqs := map[string][]string{"gpuworks.ai/vendor": {"nvidia"}}
			if pinned {
				reqs["gpuworks.ai/node"] = []string{nodes[0].UUID}
			}
			dep := poolDeployment(t, st, model, nodes[0].ID, 1, store.PlacementSpread, reqs)
			c := New(st, hub, nil)
			ctx := context.Background()
			if err := c.reconcileDeployment(ctx, dep.ID); err != nil {
				t.Fatal(err)
			}
			insts := activeInstances(t, st, dep.ID)
			if len(insts) != 1 || insts[0].NodeID != nodes[0].ID {
				t.Fatalf("placed %+v", insts)
			}
			setRunning(t, st, insts[0].UUID)
			if _, err := st.SetNodeScheduling(nodes[0].UUID, store.NodeDraining); err != nil {
				t.Fatal(err)
			}
			for pass := 0; pass < 2; pass++ {
				if err := c.reconcileDeployment(ctx, dep.ID); err != nil {
					t.Fatal(err)
				}
			}
			after := activeInstances(t, st, dep.ID)
			if len(after) != 1 || after[0].UUID != insts[0].UUID || after[0].State != proto.InstRunning {
				t.Fatalf("after draining with nowhere to go: %+v", after)
			}
			event, found, err := st.LatestScheduleEvent(dep.ID)
			if err != nil || !found || !strings.Contains(event.Data, `"code":"pool_empty"`) || !strings.Contains(event.Data, `"cordoned":1`) {
				t.Fatalf("event = %+v found=%v err=%v", event, found, err)
			}
		})
	}
}

// A cordon only keeps new replicas off the node: the one there stays, and
// a new one goes elsewhere even when binpack would have chosen the cordoned
// node.
func TestCordonKeepsNewReplicasOffTheNode(t *testing.T) {
	st, hub, nodes, model := poolFixture(t, "cordon_binpack", 2, 2)
	dep := poolDeployment(t, st, model, nodes[0].ID, 1, store.PlacementBinpack, map[string][]string{"gpuworks.ai/vendor": {"nvidia"}})
	c := New(st, hub, nil)
	ctx := context.Background()
	if err := c.reconcileDeployment(ctx, dep.ID); err != nil {
		t.Fatal(err)
	}
	first := activeInstances(t, st, dep.ID)
	if len(first) != 1 {
		t.Fatalf("placed %+v", first)
	}
	setRunning(t, st, first[0].UUID)
	host, _ := st.NodeByID(first[0].NodeID)
	if _, err := st.SetNodeScheduling(host.UUID, store.NodeCordoned); err != nil {
		t.Fatal(err)
	}
	if err := st.DB().Model(&store.Deployment{}).Where("id = ?", dep.ID).Update("replicas", 2).Error; err != nil {
		t.Fatal(err)
	}
	if err := c.reconcileDeployment(ctx, dep.ID); err != nil {
		t.Fatal(err)
	}
	got := replicaNodes(t, st, dep.ID)
	if got[host.ID] != 1 || len(got) != 2 {
		t.Fatalf("with %s cordoned replicas run on %v, want the first kept and the second elsewhere", host.UUID, got)
	}
	if again, _ := st.InstanceByUUID(first[0].UUID); again.State != proto.InstRunning {
		t.Fatalf("a cordon moved the replica: %s", again.State)
	}
}

// A replica that names its node - a multi-node rank - waits while the node
// is cordoned, and says so.
func TestNamedRankWaitsOnACordonedNode(t *testing.T) {
	st, hub, nodes, model := poolFixture(t, "cordon_named", 2, 1)
	dep := poolDeployment(t, st, model, nodes[0].ID, 1, store.PlacementSpread, nil)
	cordoned, err := st.SetNodeScheduling(nodes[1].UUID, store.NodeCordoned)
	if err != nil {
		t.Fatal(err)
	}
	c := New(st, hub, nil)
	deployment := &unitDeployment{dep: dep, nodes: map[string]*store.Node{nodes[0].UUID: nodes[0], nodes[1].UUID: cordoned}}
	unit := planning.Unit{Ranks: []planning.UnitRank{{Nodes: []string{nodes[0].UUID}}, {Nodes: []string{nodes[1].UUID}}}}
	if picked, ok := c.unitNodes(context.Background(), dep, deployment, model, unit); ok {
		t.Fatalf("a rank was placed on cordoned %s: %v", nodes[1].UUID, picked)
	}
	event, found, err := st.LatestScheduleEvent(dep.ID)
	if err != nil || !found || !strings.Contains(event.Data, `"code":"node_cordoned"`) || !strings.Contains(event.Data, cordoned.DisplayName()) {
		t.Fatalf("event = %+v found=%v err=%v", event, found, err)
	}
}

// A replica waiting because its only node is cordoned starts as soon as the
// node is schedulable again: the change reconciles, at the next routing tick,
// every deployment it could unblock.
func TestUncordonStartsWaitingReplicas(t *testing.T) {
	st, hub, nodes, model := poolFixture(t, "uncordon", 1, 1)
	if _, err := st.SetNodeScheduling(nodes[0].UUID, store.NodeCordoned); err != nil {
		t.Fatal(err)
	}
	dep := poolDeployment(t, st, model, nodes[0].ID, 1, store.PlacementSpread, nil)
	c := New(st, hub, nil)
	ctx := context.Background()
	if err := c.reconcileDeployment(ctx, dep.ID); err != nil {
		t.Fatal(err)
	}
	if got := replicaNodes(t, st, dep.ID); len(got) != 0 {
		t.Fatalf("placed %v on a cordoned node", got)
	}
	if _, err := st.SetNodeScheduling(nodes[0].UUID, store.NodeSchedulable); err != nil {
		t.Fatal(err)
	}
	drainQueue(c)
	c.onEvent(bus.Event{Topic: "node", Key: nodes[0].UUID, Reason: store.NodeChangeScheduling})
	c.flushEventRoutes()
	queued := false
	for _, key := range pendingKeys(c) {
		queued = queued || key == depKey(dep.ID)
	}
	if !queued {
		t.Fatal("making the node schedulable did not retry the waiting deployment")
	}
	if err := c.reconcileDeployment(ctx, dep.ID); err != nil {
		t.Fatal(err)
	}
	if got := replicaNodes(t, st, dep.ID); got[nodes[0].ID] != 1 {
		t.Fatalf("after the node took replicas again: %v", got)
	}
}
