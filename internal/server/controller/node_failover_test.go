package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// failoverFixture is nodes with one 24 GiB GPU each on the management
// network, the model present on every one, and a controller whose hub has
// them all online.
type failoverFixture struct {
	st    *store.Store
	nodes []*store.Node
	model *store.Model
	hub   *distributedTestHub
	c     *Controller
	// at is the controller's clock; settle moves it on.
	at time.Time
}

func newFailoverFixture(t *testing.T, name string, count int) *failoverFixture {
	t.Helper()
	st, err := store.Open("file:"+name+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "org/" + name, Revision: "main", Name: name, WeightBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	f := &failoverFixture{st: st, model: model, hub: &distributedTestHub{online: map[string]bool{}, updates: map[string][]proto.StateUpdate{}}}
	for i := 0; i < count; i++ {
		uuid := fmt.Sprintf("%s-%c", name, 'a'+i)
		// A hostname apart from the UUID, so an event naming a node by UUID shows.
		node, err := st.RegisterNode(uuid, store.HashToken(uuid), uuid+"-host")
		if err != nil {
			t.Fatal(err)
		}
		node.NetworkInventory = managementInventoryJSON(t, fmt.Sprintf("10.0.2.%d", i+1))
		seen := time.Now()
		node.LastSeenAt = &seen
		node.State = store.NodeStateOnline
		node.GPUCount = 1
		if err := st.DB().Save(node).Error; err != nil {
			t.Fatal(err)
		}
		if err := st.DB().Create(&store.GPU{NodeID: node.ID, Index: 0, UUID: uuid + "-gpu", Name: "GPU", Vendor: "nvidia", ComputeCapability: "8.6", MemoryTotal: 24 << 30}).Error; err != nil {
			t.Fatal(err)
		}
		if _, err := st.UpsertModelFile(&store.ModelFile{ModelID: model.ID, NodeID: node.ID, State: proto.MFPresent, Path: "/models/" + name}); err != nil {
			t.Fatal(err)
		}
		node, err = st.NodeWithGPUs(uuid)
		if err != nil {
			t.Fatal(err)
		}
		f.nodes = append(f.nodes, node)
		f.hub.online[uuid] = true
	}
	f.c = New(st, f.hub, nil)
	f.at = time.Now()
	f.c.now = func() time.Time { return f.at }
	f.c.SetRetryPolicy(0, 3)
	f.c.SetOfflineGrace(time.Minute)
	return f
}

func (f *failoverFixture) reconcile(t *testing.T, depID uint) {
	t.Helper()
	if err := f.c.reconcileDeployment(context.Background(), depID); err != nil {
		t.Fatal(err)
	}
}

// lose takes a node offline, last seen ago.
func (f *failoverFixture) lose(t *testing.T, node *store.Node, ago time.Duration) {
	t.Helper()
	f.hub.online[node.UUID] = false
	// As the real hub: nothing can be pushed to a node that is not connected.
	if f.hub.pushFails == nil {
		f.hub.pushFails = map[string]error{}
	}
	f.hub.pushFails[node.UUID] = errors.New("node not connected: " + node.UUID)
	if err := f.st.SetNodeState(node.UUID, store.NodeStateOffline); err != nil {
		t.Fatal(err)
	}
	seen := f.at.Add(-ago)
	if err := f.st.DB().Model(&store.Node{}).Where("id = ?", node.ID).Update("last_seen_at", seen).Error; err != nil {
		t.Fatal(err)
	}
}

// confirmStopped answers for the agents of online nodes: every instance being
// removed there has stopped.
func (f *failoverFixture) confirmStopped(t *testing.T, depID uint) {
	t.Helper()
	insts, err := f.st.InstancesForDeployment(depID)
	if err != nil {
		t.Fatal(err)
	}
	for _, inst := range insts {
		node, err := f.st.NodeByID(inst.NodeID)
		if err != nil || !f.hub.online[node.UUID] {
			continue
		}
		if inst.State == proto.InstRemoving || inst.State == proto.InstPendingRemoval {
			if err := f.st.UpdateInstanceStatus(proto.InstanceStatus{UUID: inst.UUID, State: proto.InstStopped}); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// settle reconciles, each time first confirming what the online agents were
// asked to stop.
func (f *failoverFixture) settle(t *testing.T, depID uint, passes int) {
	t.Helper()
	for i := 0; i < passes; i++ {
		f.confirmStopped(t, depID)
		f.reconcile(t, depID)
		f.at = f.at.Add(5 * time.Second)
	}
}

func (f *failoverFixture) restore(t *testing.T, node *store.Node) {
	t.Helper()
	f.hub.online[node.UUID] = true
	delete(f.hub.pushFails, node.UUID)
	if err := f.st.SetNodeState(node.UUID, store.NodeStateOnline); err != nil {
		t.Fatal(err)
	}
}

func (f *failoverFixture) multiNode(t *testing.T, name string) *store.Deployment {
	t.Helper()
	nodeIDs, _ := json.Marshal([]uint{f.nodes[0].ID, f.nodes[1].ID})
	dep, err := createTestDeployment(t, f.st, &store.Deployment{
		Name: name, ModelID: f.model.ID, NodeID: f.nodes[0].ID, NodeIDs: string(nodeIDs), ServedName: name, Engine: "vllm", State: "active",
		NNodes: 2, DistGroup: name + "-group", MasterAddr: managementIP(*f.nodes[0]), MasterPort: 29500, Placement: `{"tp":2}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	return dep
}

func failoverUnits(t *testing.T, st *store.Store, depID uint) [][]string {
	t.Helper()
	dep, err := st.GetDeployment(depID)
	if err != nil {
		t.Fatal(err)
	}
	var nodes []string
	for _, id := range dep.NodeIDList() {
		node, err := st.NodeByID(id)
		if err != nil {
			t.Fatal(err)
		}
		nodes = append(nodes, node.UUID)
	}
	var out [][]string
	for _, unit := range planning.ReplicaUnitsOf(dep, nodes) {
		var ranks []string
		for _, rank := range unit.Ranks() {
			ranks = append(ranks, rank.NodeUUID)
		}
		out = append(out, ranks)
	}
	return out
}

func eventCodes(t *testing.T, st *store.Store, depID uint, code string) []map[string]any {
	t.Helper()
	events, err := st.DeploymentEvents(depID, 500)
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]any
	for _, ev := range events {
		if ev.Code != code {
			continue
		}
		params := map[string]any{}
		_ = json.Unmarshal([]byte(ev.Data), &params)
		out = append(out, params)
	}
	return out
}

// A multi-node replica whose follower's node is lost moves that rank to the
// spare node of the same network and architecture; its leader stays.
func TestALostRankMovesToASpareNode(t *testing.T) {
	f := newFailoverFixture(t, "failover-rank", 3)
	dep := f.multiNode(t, "failover-rank")
	f.reconcile(t, dep.ID)
	f.reconcile(t, dep.ID)
	if insts, _ := f.st.InstancesForDeployment(dep.ID); len(insts) != 2 {
		t.Fatalf("initial generation = %+v", insts)
	}

	// Offline five minutes: inside the ten-minute wait, nothing moves.
	f.lose(t, f.nodes[1], 5*time.Minute)
	f.reconcile(t, dep.ID)
	if got := failoverUnits(t, f.st, dep.ID); len(got) != 1 || got[0][1] != f.nodes[1].UUID {
		t.Fatalf("moved inside the wait: %v", got)
	}

	f.lose(t, f.nodes[1], 11*time.Minute)
	for i := 0; i < 3; i++ {
		f.reconcile(t, dep.ID)
	}
	got := failoverUnits(t, f.st, dep.ID)
	if len(got) != 1 || got[0][0] != f.nodes[0].UUID || got[0][1] != f.nodes[2].UUID {
		t.Fatalf("replica after the loss = %v, want leader %s and follower %s", got, f.nodes[0].UUID, f.nodes[2].UUID)
	}
	events := eventCodes(t, f.st, dep.ID, EventNodeReplaced)
	if len(events) != 1 || events[0]["from"] != f.nodes[1].DisplayName() || events[0]["to"] != f.nodes[2].DisplayName() {
		t.Fatalf("NODE_REPLACED events = %v", events)
	}
	// The event keeps the nodes it moved between, by UUID, for the scheduling history.
	if got := fmt.Sprint(events[0][store.EventParamNodeUUIDs]); got != fmt.Sprint([]any{f.nodes[1].UUID, f.nodes[2].UUID}) {
		t.Fatalf("NODE_REPLACED node_uuids = %s", got)
	}
	// The unit starts again on its new nodes, with a full retry budget.
	f.settle(t, dep.ID, 3)
	insts, _ := f.st.InstancesForDeployment(dep.ID)
	onSpare := false
	for _, inst := range insts {
		if inst.NodeID == f.nodes[2].ID && inst.State != proto.InstFailed {
			onSpare = true
		}
	}
	if !onSpare {
		t.Fatalf("no rank launched on the spare node: %+v", insts)
	}
}

// A lost node that comes back before the moved unit ran on its new node takes
// the rank back; one that comes back after does not.
func TestAReturningNodeTakesItsRankBackOnlyBeforeTheMoveRan(t *testing.T) {
	f := newFailoverFixture(t, "failover-revert", 3)
	dep := f.multiNode(t, "failover-revert")
	f.reconcile(t, dep.ID)
	f.reconcile(t, dep.ID)
	f.lose(t, f.nodes[1], 11*time.Minute)
	f.reconcile(t, dep.ID)
	f.reconcile(t, dep.ID)
	if got := failoverUnits(t, f.st, dep.ID); got[0][1] != f.nodes[2].UUID {
		t.Fatalf("not moved: %v", got)
	}
	f.restore(t, f.nodes[1])
	f.reconcile(t, dep.ID)
	if got := failoverUnits(t, f.st, dep.ID); got[0][1] != f.nodes[1].UUID {
		t.Fatalf("not taken back before the move ran: %v", got)
	}
	if events := eventCodes(t, f.st, dep.ID, EventNodeReplaceReverted); len(events) != 1 || fmt.Sprint(events[0][store.EventParamNodeUUIDs]) != fmt.Sprint([]any{f.nodes[2].UUID, f.nodes[1].UUID}) {
		t.Fatalf("NODE_REPLACE_REVERTED events = %v", events)
	}

	// Lose it again; this time the moved unit runs before the node returns.
	f.lose(t, f.nodes[1], 11*time.Minute)
	f.settle(t, dep.ID, 8)
	if got := failoverUnits(t, f.st, dep.ID); got[0][1] != f.nodes[2].UUID {
		t.Fatalf("not moved the second time: %v", got)
	}
	insts, _ := f.st.InstancesForDeployment(dep.ID)
	for _, inst := range insts {
		if inst.State != proto.InstStopped && inst.State != proto.InstFailed && (inst.NodeID == f.nodes[2].ID || inst.NodeID == f.nodes[0].ID) {
			if err := f.st.UpdateInstanceStatus(proto.InstanceStatus{UUID: inst.UUID, State: proto.InstRunning}); err != nil {
				t.Fatal(err)
			}
		}
	}
	f.reconcile(t, dep.ID)
	f.restore(t, f.nodes[1])
	f.reconcile(t, dep.ID)
	if got := failoverUnits(t, f.st, dep.ID); got[0][1] != f.nodes[2].UUID {
		t.Fatalf("switched back after the move ran: %v", got)
	}
}

// A deployment that never moves its replicas (wait 0) keeps them on a lost
// node, and no spare node of the same network means nothing moves either
// (recorded once).
func TestNoMoveWhenReplacementIsOffOrNoNodeFits(t *testing.T) {
	f := newFailoverFixture(t, "failover-off", 3)
	dep := f.multiNode(t, "failover-off")
	if err := f.st.SetDeploymentNodeReplaceAfter(dep.ID, 0); err != nil {
		t.Fatal(err)
	}
	f.reconcile(t, dep.ID)
	f.reconcile(t, dep.ID)
	f.lose(t, f.nodes[1], time.Hour)
	f.reconcile(t, dep.ID)
	f.reconcile(t, dep.ID)
	if got := failoverUnits(t, f.st, dep.ID); got[0][1] != f.nodes[1].UUID {
		t.Fatalf("moved with replacement off: %v", got)
	}

	g := newFailoverFixture(t, "failover-none", 2)
	dep = g.multiNode(t, "failover-none")
	g.reconcile(t, dep.ID)
	g.reconcile(t, dep.ID)
	g.lose(t, g.nodes[1], time.Hour)
	g.reconcile(t, dep.ID)
	g.reconcile(t, dep.ID)
	g.reconcile(t, dep.ID)
	if got := failoverUnits(t, g.st, dep.ID); got[0][1] != g.nodes[1].UUID {
		t.Fatalf("moved with no spare node: %v", got)
	}
	if events := eventCodes(t, g.st, dep.ID, EventNodeReplaceNoCandidate); len(events) != 1 || fmt.Sprint(events[0][store.EventParamNodeUUIDs]) != fmt.Sprint([]any{g.nodes[1].UUID}) {
		t.Fatalf("NODE_REPLACE_NO_CANDIDATE events = %v, want one", events)
	}
}

// A single-node replica on a lost node is retired without spending a retry
// and the candidate pool starts it on another node.
func TestALostSingleNodeReplicaStartsOnAnotherNode(t *testing.T) {
	f := newFailoverFixture(t, "failover-pool", 2)
	dep, err := createTestDeploymentWithRequirements(t, f.st, &store.Deployment{
		Name: "failover-pool", ModelID: f.model.ID, NodeID: f.nodes[0].ID, ServedName: "failover-pool", Engine: "vllm", State: "active", Replicas: 1,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	f.reconcile(t, dep.ID)
	f.reconcile(t, dep.ID)
	insts, _ := f.st.InstancesForDeployment(dep.ID)
	if len(insts) != 1 {
		t.Fatalf("initial replica = %+v", insts)
	}
	first := insts[0]
	lostNode, other := f.nodes[0], f.nodes[1]
	if first.NodeID == f.nodes[1].ID {
		lostNode, other = f.nodes[1], f.nodes[0]
	}
	if err := f.st.UpdateInstanceStatus(proto.InstanceStatus{UUID: first.UUID, State: proto.InstRunning}); err != nil {
		t.Fatal(err)
	}
	f.lose(t, lostNode, 11*time.Minute)
	f.settle(t, dep.ID, 8)
	insts, _ = f.st.InstancesForDeployment(dep.ID)
	var onOther, failed int
	for _, inst := range insts {
		if inst.NodeID == other.ID && inst.UUID != first.UUID {
			onOther++
		}
		if inst.State == proto.InstFailed {
			failed++
		}
	}
	if onOther != 1 || failed != 0 {
		t.Fatalf("after the loss = %+v; want one replica on the other node and no failed attempt", insts)
	}
	if events := eventCodes(t, f.st, dep.ID, EventNodeReplaced); len(events) != 1 || fmt.Sprint(events[0][store.EventParamNodeUUIDs]) != fmt.Sprint([]any{lostNode.UUID}) {
		t.Fatalf("NODE_REPLACED events = %v", events)
	}
}

// A replica pinned to its node has nowhere to go: it stays, and comes back
// with its node.
func TestAPinnedReplicaStaysOnItsLostNode(t *testing.T) {
	f := newFailoverFixture(t, "failover-pinned", 2)
	dep, err := createTestDeployment(t, f.st, &store.Deployment{
		Name: "failover-pinned", ModelID: f.model.ID, NodeID: f.nodes[0].ID, ServedName: "failover-pinned", Engine: "vllm", State: "active", Replicas: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	f.settle(t, dep.ID, 2)
	f.lose(t, f.nodes[0], time.Hour)
	f.settle(t, dep.ID, 4)
	insts, _ := f.st.InstancesForDeployment(dep.ID)
	if len(insts) != 1 || insts[0].NodeID != f.nodes[0].ID {
		t.Fatalf("pinned replica = %+v, want it kept on its node", insts)
	}
	if events := eventCodes(t, f.st, dep.ID, EventNodeReplaced); len(events) != 0 {
		t.Fatalf("NODE_REPLACED for a replica with nowhere to go: %v", events)
	}
}

// A multi-node replica on a draining node starts a replacement on other
// nodes first and moves onto it only once it runs, so it never stops
// serving; the old generation then drains.
func TestADrainingNodesReplicaMovesAfterItsReplacementRuns(t *testing.T) {
	f := newFailoverFixture(t, "failover-drain", 4)
	dep := f.multiNode(t, "failover-drain")
	f.settle(t, dep.ID, 2)
	initial, _ := f.st.InstancesForDeployment(dep.ID)
	for _, inst := range initial {
		if err := f.st.UpdateInstanceStatus(proto.InstanceStatus{UUID: inst.UUID, State: proto.InstRunning}); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.st.DB().Model(&store.Node{}).Where("id = ?", f.nodes[1].ID).Update("scheduling", store.NodeDraining).Error; err != nil {
		t.Fatal(err)
	}
	f.settle(t, dep.ID, 2)
	// The replacement runs next to the replica; the replica still serves.
	if got := failoverUnits(t, f.st, dep.ID); got[0][0] != f.nodes[0].UUID || got[0][1] != f.nodes[1].UUID {
		t.Fatalf("placement rewritten before the replacement ran: %v", got)
	}
	insts, _ := f.st.InstancesForDeployment(dep.ID)
	var surge []store.Instance
	for _, inst := range insts {
		if inst.NodeID == f.nodes[2].ID || inst.NodeID == f.nodes[3].ID {
			surge = append(surge, inst)
		}
		if (inst.NodeID == f.nodes[0].ID || inst.NodeID == f.nodes[1].ID) && inst.State != proto.InstRunning {
			t.Fatalf("the replica stopped serving before its replacement ran: %+v", inst)
		}
	}
	if len(surge) != 2 {
		t.Fatalf("replacement ranks = %+v, want two on the spare nodes", surge)
	}
	for _, inst := range surge {
		if err := f.st.UpdateInstanceStatus(proto.InstanceStatus{UUID: inst.UUID, State: proto.InstRunning}); err != nil {
			t.Fatal(err)
		}
	}
	f.settle(t, dep.ID, 6)
	got := failoverUnits(t, f.st, dep.ID)
	if len(got) != 1 || got[0][0] != f.nodes[2].UUID || got[0][1] != f.nodes[3].UUID {
		t.Fatalf("placement after the replacement ran = %v", got)
	}
	if events := eventCodes(t, f.st, dep.ID, EventNodeDrainMoved); len(events) != 1 || len(events[0][store.EventParamNodeUUIDs].([]any)) < 2 {
		t.Fatalf("NODE_DRAIN_MOVED events = %v", events)
	}
	insts, _ = f.st.InstancesForDeployment(dep.ID)
	for _, inst := range insts {
		if inst.NodeID == f.nodes[1].ID && inst.State != proto.InstStopped {
			t.Fatalf("the old generation on the draining node was not retired: %s %s", inst.UUID, inst.State)
		}
	}
}

// A failed attempt whose engine still holds its card - an L2 wake that failed
// halfway leaves the container up - makes that card read as taken. Its
// container is removed before the retry looks for a node, or the retry
// would wait for that card forever.
func TestAFailedAttemptsContainerGoesBeforeTheRetryLooksForANode(t *testing.T) {
	f := newFailoverFixture(t, "failed-attempt-cleanup", 1)
	dep, err := createTestDeployment(t, f.st, &store.Deployment{
		Name: "failed-attempt-cleanup", ModelID: f.model.ID, NodeID: f.nodes[0].ID, ServedName: "failed-attempt-cleanup", Engine: "vllm", State: "active", Replicas: 1, Sleepable: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	f.settle(t, dep.ID, 2)
	insts, _ := f.st.InstancesForDeployment(dep.ID)
	if len(insts) != 1 {
		t.Fatalf("initial replica = %+v", insts)
	}
	failed := insts[0].UUID
	if err := f.st.UpdateInstanceStatus(proto.InstanceStatus{UUID: failed, State: proto.InstFailed, Phase: proto.PhaseSleep, Message: "sleep level uncertain"}); err != nil {
		t.Fatal(err)
	}
	// The agent reports the failed engine's process on the card as not ours.
	if err := f.st.DB().Model(&store.GPU{}).Where("node_id = ?", f.nodes[0].ID).Update("foreign_processes", `[{"pid":4242,"name":"vllm"}]`).Error; err != nil {
		t.Fatal(err)
	}
	f.hub.updates = map[string][]proto.StateUpdate{}
	f.reconcile(t, dep.ID)
	removed := false
	for _, update := range f.hub.updates[f.nodes[0].UUID] {
		for _, uuid := range update.RemovedInstances {
			removed = removed || uuid == failed
		}
	}
	if !removed {
		t.Fatalf("the failed attempt's container was not removed while the retry waited for a node: %+v", f.hub.updates)
	}
}

// A replica an operator asked to start on a named node starts there first,
// whichever node the pool would rank first; the hint is used once.
func TestAHintedReplicaStartsOnItsNamedNodeOnce(t *testing.T) {
	f := newFailoverFixture(t, "place-on", 3)
	dep, err := createTestDeploymentWithRequirements(t, f.st, &store.Deployment{
		Name: "place-on", ModelID: f.model.ID, NodeID: f.nodes[0].ID, ServedName: "place-on", Engine: "vllm", State: "active", Replicas: 1,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	f.settle(t, dep.ID, 2)
	first, _ := f.st.InstancesForDeployment(dep.ID)
	if len(first) != 1 {
		t.Fatalf("first replica = %+v", first)
	}
	// Name the node the pool is least likely to pick: one that is not the
	// first replica's, and the last of the others.
	named := f.nodes[2]
	if first[0].NodeID == named.ID {
		named = f.nodes[1]
	}
	f.c.orders.HintPlacement(dep.ID, []string{named.UUID})
	if err := f.st.ScaleDeploymentReplicas(dep.ID, 1, 2, nil); err != nil {
		t.Fatal(err)
	}
	f.settle(t, dep.ID, 2)
	insts, _ := f.st.InstancesForDeployment(dep.ID)
	onNamed := 0
	for _, inst := range insts {
		if inst.NodeID == named.ID {
			onNamed++
		}
	}
	if len(insts) != 2 || onNamed != 1 {
		t.Fatalf("replicas after growing with a named node = %+v, want one on %s", insts, named.UUID)
	}
	if _, queued := f.c.orders.NextPlacementHint(dep.ID); queued {
		t.Fatal("the hint outlived the replica start it was for")
	}
}

// A PD unit whose node is lost moves to a node of the same network and
// architecture; the other units stay where they are.
// A PD role on a draining node starts on a spare node first and moves once
// that runs; the launch finds the replacement although the placement does not
// name it yet.
func TestADrainingNodesPDRoleMovesAfterItsReplacementRuns(t *testing.T) {
	f := newPDFixture(t, "drain-pd", twoPrefillOneDecode)
	// A spare node like the decode's: one RTX 4090 on the same network.
	spare, err := f.st.RegisterNode("drain-pd-n3", store.HashToken("token-n3"), "drain-pd-n3")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.st.DB().Create(&store.GPU{NodeID: spare.ID, Index: 0, UUID: "drain-pd-n3-gpu0", Name: "NVIDIA GeForce RTX 4090", Vendor: "nvidia", ArchID: "sm_89", ComputeCapability: "8.9", MemoryTotal: 24 << 30}).Error; err != nil {
		t.Fatal(err)
	}
	model, _ := f.st.GetModel(f.dep.ModelID)
	if _, err := f.st.UpsertModelFile(&store.ModelFile{ModelID: model.ID, NodeID: spare.ID, State: proto.MFPresent, Path: "/models/pd"}); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	for _, node := range []*store.Node{f.nodes["n1"], f.nodes["n2"], spare} {
		updates := map[string]any{"state": store.NodeStateOnline, "last_seen_at": now}
		if node.ID == spare.ID {
			updates["network_inventory"] = `{"interfaces":[{"name":"eth0","addrs":["10.0.0.3/24"]}],"rdma_ports":[],"management":{"interface":"eth0","ip":"10.0.0.3"}}`
			updates["gpu_count"] = 1
		}
		if err := f.st.DB().Model(&store.Node{}).Where("id = ?", node.ID).Updates(updates).Error; err != nil {
			t.Fatal(err)
		}
	}
	for _, key := range []string{"n1", "n2"} {
		count := 2
		if key == "n2" {
			count = 1
		}
		if err := f.st.DB().Model(&store.Node{}).Where("id = ?", f.nodes[key].ID).Update("gpu_count", count).Error; err != nil {
			t.Fatal(err)
		}
	}
	f.hub.online[spare.UUID] = true
	f.reconcile(t)
	f.reconcile(t)

	running := func() {
		insts, _ := f.st.InstancesForDeployment(f.dep.ID)
		for _, inst := range insts {
			if inst.State == proto.InstPending || inst.State == proto.InstStarting {
				if err := f.st.UpdateInstanceStatus(proto.InstanceStatus{UUID: inst.UUID, State: proto.InstRunning}); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	running()
	decode := f.nodes["n2"]
	if err := f.st.DB().Model(&store.Node{}).Where("id = ?", decode.ID).Update("scheduling", store.NodeDraining).Error; err != nil {
		t.Fatal(err)
	}
	f.reconcile(t)
	f.reconcile(t)
	insts, _ := f.st.InstancesForDeployment(f.dep.ID)
	var onSpare []store.Instance
	for _, inst := range insts {
		if inst.NodeID == spare.ID {
			onSpare = append(onSpare, inst)
		}
		if inst.NodeID == decode.ID && inst.State != proto.InstRunning {
			t.Fatalf("the decode stopped serving before its replacement ran: %+v", inst)
		}
	}
	if len(onSpare) != 1 || onSpare[0].Role != "decode" {
		t.Fatalf("replacement on the spare = %+v, want one decode", onSpare)
	}
	running()
	for i := 0; i < 4; i++ {
		f.reconcile(t)
	}
	dep, err := f.st.GetDeployment(f.dep.ID)
	if err != nil {
		t.Fatal(err)
	}
	pd, err := planning.PDOf(dep.Placement)
	if err != nil || pd == nil {
		t.Fatalf("placement = %s err=%v", dep.Placement, err)
	}
	if got := pd.Units("decode"); len(got) != 1 || got[0].NodeUUID != spare.UUID {
		t.Fatalf("decode units after the move = %+v, want only the spare %s", got, spare.UUID)
	}
	if events := eventCodes(t, f.st, f.dep.ID, EventNodeDrainMoved); len(events) != 1 || events[0]["role"] != "decode" {
		t.Fatalf("NODE_DRAIN_MOVED events = %v", events)
	}
}

func TestALostPDUnitMovesToASpareNode(t *testing.T) {
	f := newPDFixture(t, "failover-pd", twoPrefillOneDecode)
	// A spare node like the decode's: one RTX 4090 on the same network.
	spare, err := f.st.RegisterNode("failover-pd-n3", store.HashToken("token-n3"), "failover-pd-n3")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.st.DB().Create(&store.GPU{NodeID: spare.ID, Index: 0, UUID: "failover-pd-n3-gpu0", Name: "NVIDIA GeForce RTX 4090", Vendor: "nvidia", ArchID: "sm_89", ComputeCapability: "8.9", MemoryTotal: 24 << 30}).Error; err != nil {
		t.Fatal(err)
	}
	model, _ := f.st.GetModel(f.dep.ModelID)
	if _, err := f.st.UpsertModelFile(&store.ModelFile{ModelID: model.ID, NodeID: spare.ID, State: proto.MFPresent, Path: "/models/pd"}); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	for _, node := range []*store.Node{f.nodes["n1"], f.nodes["n2"], spare} {
		updates := map[string]any{"state": store.NodeStateOnline, "last_seen_at": now}
		if node.ID == spare.ID {
			updates["network_inventory"] = `{"interfaces":[{"name":"eth0","addrs":["10.0.0.3/24"]}],"rdma_ports":[],"management":{"interface":"eth0","ip":"10.0.0.3"}}`
			updates["gpu_count"] = 1
		}
		if err := f.st.DB().Model(&store.Node{}).Where("id = ?", node.ID).Updates(updates).Error; err != nil {
			t.Fatal(err)
		}
	}
	for _, key := range []string{"n1", "n2"} {
		count := 2
		if key == "n2" {
			count = 1
		}
		if err := f.st.DB().Model(&store.Node{}).Where("id = ?", f.nodes[key].ID).Update("gpu_count", count).Error; err != nil {
			t.Fatal(err)
		}
	}
	f.hub.online[spare.UUID] = true
	f.reconcile(t)
	f.reconcile(t)

	decode := f.nodes["n2"]
	f.hub.online[decode.UUID] = false
	if err := f.st.DB().Model(&store.Node{}).Where("id = ?", decode.ID).Updates(map[string]any{"state": store.NodeStateOffline, "last_seen_at": now.Add(-11 * time.Minute)}).Error; err != nil {
		t.Fatal(err)
	}
	f.reconcile(t)
	f.reconcile(t)

	dep, err := f.st.GetDeployment(f.dep.ID)
	if err != nil {
		t.Fatal(err)
	}
	pd, err := planning.PDOf(dep.Placement)
	if err != nil || pd == nil {
		t.Fatalf("placement = %s err=%v", dep.Placement, err)
	}
	if got := pd.Units("decode"); len(got) != 1 || got[0].NodeUUID != spare.UUID {
		t.Fatalf("decode units after the loss = %+v, want on %s", got, spare.UUID)
	}
	if got := pd.Units("prefill"); len(got) != 2 || got[0].NodeUUID != f.nodes["n1"].UUID || got[1].NodeUUID != f.nodes["n1"].UUID {
		t.Fatalf("prefill units moved: %+v", got)
	}
	events := eventCodes(t, f.st, dep.ID, EventNodeReplaced)
	if len(events) != 1 || events[0]["role"] != "decode" || events[0]["to"] != spare.DisplayName() {
		t.Fatalf("NODE_REPLACED events = %v", events)
	}
}

// A server that has just started has not seen its agents reconnect yet: a
// node it last saw before it went down counts as seen when it started, so
// neither node replacement nor a multi-node unit's reconnect grace acts on
// the downtime.
func TestAServerJustStartedDoesNotCountItsOwnDowntime(t *testing.T) {
	f := newFailoverFixture(t, "failover-restart", 2)
	dep, err := createTestDeploymentWithRequirements(t, f.st, &store.Deployment{
		Name: "failover-restart", ModelID: f.model.ID, NodeID: f.nodes[0].ID, ServedName: "failover-restart", Engine: "vllm", State: "active", Replicas: 1,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	f.reconcile(t, dep.ID)
	f.reconcile(t, dep.ID)
	insts, _ := f.st.InstancesForDeployment(dep.ID)
	if len(insts) != 1 {
		t.Fatalf("initial replica = %+v", insts)
	}
	first := insts[0]
	if err := f.st.UpdateInstanceStatus(proto.InstanceStatus{UUID: first.UUID, State: proto.InstRunning}); err != nil {
		t.Fatal(err)
	}
	lostNode := f.nodes[0]
	if first.NodeID == f.nodes[1].ID {
		lostNode = f.nodes[1]
	}
	// The server was down for eleven minutes and has just started.
	f.lose(t, lostNode, 11*time.Minute)
	f.c.watchingSince = f.at
	f.settle(t, dep.ID, 4)
	if events := eventCodes(t, f.st, dep.ID, EventNodeReplaced); len(events) != 0 {
		t.Fatalf("moved on the server's own downtime: %v", events)
	}
	if got, _ := f.st.InstanceByUUID(first.UUID); got.State != proto.InstRunning {
		t.Fatalf("the replica = %s, want it left running", got.State)
	}
	// Ten minutes after the start the node has had its chance.
	f.at = f.c.watchingSince.Add(10*time.Minute + time.Second)
	f.settle(t, dep.ID, 8)
	if events := eventCodes(t, f.st, dep.ID, EventNodeReplaced); len(events) != 1 {
		t.Fatalf("NODE_REPLACED events after the wait = %v", events)
	}
}

func TestAMultiNodeUnitsGraceStartsWithTheServer(t *testing.T) {
	f := newFailoverFixture(t, "grace-restart", 2)
	dep := f.multiNode(t, "grace-restart")
	// Replacement off: only the reconnect grace acts on the lost rank.
	if err := f.st.SetDeploymentNodeReplaceAfter(dep.ID, 0); err != nil {
		t.Fatal(err)
	}
	f.reconcile(t, dep.ID)
	f.reconcile(t, dep.ID)
	insts, _ := f.st.InstancesForDeployment(dep.ID)
	if len(insts) != 2 {
		t.Fatalf("initial generation = %+v", insts)
	}
	for _, inst := range insts {
		if err := f.st.UpdateInstanceStatus(proto.InstanceStatus{UUID: inst.UUID, State: proto.InstRunning}); err != nil {
			t.Fatal(err)
		}
	}
	failed := func() int {
		insts, _ := f.st.InstancesForDeployment(dep.ID)
		n := 0
		for _, inst := range insts {
			if inst.State == proto.InstFailed {
				n++
			}
		}
		return n
	}
	f.lose(t, f.nodes[1], 5*time.Minute)
	f.c.watchingSince = f.at
	f.reconcile(t, dep.ID)
	if n := failed(); n != 0 {
		t.Fatalf("%d ranks failed on the server's own downtime", n)
	}
	f.at = f.c.watchingSince.Add(f.c.offlineGrace + time.Second)
	f.reconcile(t, dep.ID)
	if n := failed(); n == 0 {
		t.Fatal("the generation outlived the reconnect grace counted from the start")
	}
}

// A failed attempt on a node that has gone offline does not hold its unit's
// retry: the retry starts on another node, and the offline agent drops the
// container when it reconnects.
func TestARetryDoesNotWaitForAFailedAttemptsOfflineNode(t *testing.T) {
	f := newFailoverFixture(t, "retry-offline", 2)
	dep, err := createTestDeploymentWithRequirements(t, f.st, &store.Deployment{
		Name: "retry-offline", ModelID: f.model.ID, NodeID: f.nodes[0].ID, ServedName: "retry-offline", Engine: "vllm", State: "active", Replicas: 1,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	f.reconcile(t, dep.ID)
	insts, _ := f.st.InstancesForDeployment(dep.ID)
	if len(insts) != 1 {
		t.Fatalf("initial replica = %+v", insts)
	}
	first := insts[0]
	if err := f.st.UpdateInstanceStatus(proto.InstanceStatus{UUID: first.UUID, State: proto.InstFailed, Message: "engine exited"}); err != nil {
		t.Fatal(err)
	}
	gone, other := f.nodes[0], f.nodes[1]
	if first.NodeID == f.nodes[1].ID {
		gone, other = f.nodes[1], f.nodes[0]
	}
	// Offline a minute: well inside the replacement wait, so only the retry acts.
	f.lose(t, gone, time.Minute)
	f.at = f.at.Add(time.Minute)
	for i := 0; i < 4; i++ {
		f.reconcile(t, dep.ID)
		f.at = f.at.Add(20 * time.Second)
	}
	insts, _ = f.st.InstancesForDeployment(dep.ID)
	var retried bool
	for _, inst := range insts {
		if inst.UUID != first.UUID && inst.NodeID == other.ID {
			retried = true
		}
	}
	if !retried {
		t.Fatalf("instances = %+v; want the retry on the online node", insts)
	}
}
