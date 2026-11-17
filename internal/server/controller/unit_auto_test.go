package controller

import (
	"fmt"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// occupy puts another deployment's running instance on node's only card.
func (f *failoverFixture) occupy(t *testing.T, node *store.Node, name string) *store.Instance {
	t.Helper()
	other, err := createTestDeployment(t, f.st, &store.Deployment{Name: name, ModelID: f.model.ID, NodeID: node.ID, ServedName: name, Engine: "vllm", State: "active", Replicas: 1})
	if err != nil {
		t.Fatal(err)
	}
	inst, err := f.st.CreateInstance(&store.Instance{UUID: name + "-inst", DeploymentID: other.ID, NodeID: node.ID, State: proto.InstRunning, GPUIndexes: "[0]"})
	if err != nil {
		t.Fatal(err)
	}
	return inst
}

func (f *failoverFixture) auto(t *testing.T, dep *store.Deployment) {
	t.Helper()
	if err := f.st.SetDeploymentUnitsAuto(dep.ID, true, ""); err != nil {
		t.Fatal(err)
	}
}

// An automatic multi-node replica whose node's card another deployment took
// moves only that rank, to a free node, before it starts; it then starts on
// its new nodes.
func TestAnAutomaticReplicaMovesTheRankWhoseCardIsTaken(t *testing.T) {
	f := newFailoverFixture(t, "auto-repick", 3)
	dep := f.multiNode(t, "auto-repick")
	f.auto(t, dep)
	f.occupy(t, f.nodes[1], "auto-repick-other")
	f.settle(t, dep.ID, 3)

	got := failoverUnits(t, f.st, dep.ID)
	if len(got) != 1 || got[0][0] != f.nodes[0].UUID || got[0][1] != f.nodes[2].UUID {
		t.Fatalf("units = %v, want the leader kept and the follower on %s", got, f.nodes[2].UUID)
	}
	events := eventCodes(t, f.st, dep.ID, EventUnitPlaced)
	if len(events) != 1 || events[0]["reason"] == nil {
		t.Fatalf("UNIT_PLACED events = %v", events)
	}
	from, _ := events[0]["from"].([]any)
	to, _ := events[0]["to"].([]any)
	if len(from) != 2 || from[1] != f.nodes[1].DisplayName() || len(to) != 2 || to[1] != f.nodes[2].DisplayName() {
		t.Fatalf("event from=%v to=%v, want display names", from, to)
	}
	if got := fmt.Sprint(events[0][store.EventParamNodeUUIDs]); got != fmt.Sprint([]any{f.nodes[0].UUID, f.nodes[1].UUID, f.nodes[2].UUID}) {
		t.Fatalf("UNIT_PLACED node_uuids = %s, want the nodes of both placements", got)
	}
	insts, _ := f.st.InstancesForDeployment(dep.ID)
	onNew := 0
	for _, inst := range insts {
		if inst.NodeID == f.nodes[1].ID {
			t.Fatalf("an instance started on the taken node: %+v", inst)
		}
		if inst.NodeID == f.nodes[2].ID {
			onNew++
		}
	}
	if onNew != 1 {
		t.Fatalf("instances = %+v, want the follower started on the new node", insts)
	}
}

// The operator's own placement is kept: the replica waits for its card.
func TestAManualReplicaWaitsForItsNode(t *testing.T) {
	f := newFailoverFixture(t, "manual-wait", 3)
	dep := f.multiNode(t, "manual-wait")
	f.occupy(t, f.nodes[1], "manual-wait-other")
	f.settle(t, dep.ID, 3)
	if got := failoverUnits(t, f.st, dep.ID); got[0][1] != f.nodes[1].UUID {
		t.Fatalf("a manual placement moved: %v", got)
	}
	if events := eventCodes(t, f.st, dep.ID, EventUnitPlaced); len(events) != 0 {
		t.Fatalf("UNIT_PLACED on a manual deployment: %v", events)
	}
	if insts, _ := f.st.InstancesForDeployment(dep.ID); len(insts) != 0 {
		t.Fatalf("instances = %+v, want none while its card is taken", insts)
	}
}

// With no node free the unit keeps its nodes and waits; the look for others
// is not repeated on every pass.
func TestAnAutomaticReplicaWaitsWhenNoNodeFits(t *testing.T) {
	f := newFailoverFixture(t, "auto-wait", 2)
	dep := f.multiNode(t, "auto-wait")
	f.auto(t, dep)
	f.occupy(t, f.nodes[1], "auto-wait-other")
	f.settle(t, dep.ID, 3)
	if got := failoverUnits(t, f.st, dep.ID); got[0][0] != f.nodes[0].UUID || got[0][1] != f.nodes[1].UUID {
		t.Fatalf("units = %v, want them kept", got)
	}
	if events := eventCodes(t, f.st, dep.ID, EventUnitPlaced); len(events) != 0 {
		t.Fatalf("UNIT_PLACED with nowhere to go: %v", events)
	}
	f.c.failover.mu.Lock()
	_, remembered := f.c.failover.repicked[dep.ID]
	f.c.failover.mu.Unlock()
	if !remembered {
		t.Fatal("the failed look was not remembered")
	}
	if f.c.autoRepickDue(dep.ID) {
		t.Fatal("another look is due straight after one that found nothing")
	}
	f.at = f.at.Add(autoRepickRetry + time.Second)
	if !f.c.autoRepickDue(dep.ID) {
		t.Fatal("no look due after the retry interval")
	}
}

// A node that stopped taking replicas makes its rank move, as one whose card
// is taken does.
func TestAnAutomaticReplicaLeavesACordonedNode(t *testing.T) {
	f := newFailoverFixture(t, "auto-cordon", 3)
	dep := f.multiNode(t, "auto-cordon")
	f.auto(t, dep)
	if err := f.st.DB().Model(&store.Node{}).Where("id = ?", f.nodes[1].ID).Update("scheduling", store.NodeCordoned).Error; err != nil {
		t.Fatal(err)
	}
	f.settle(t, dep.ID, 3)
	if got := failoverUnits(t, f.st, dep.ID); got[0][1] != f.nodes[2].UUID {
		t.Fatalf("units = %v, want the follower off the cordoned node", got)
	}
}

// A restart leaves the replica's own instances draining on its nodes: their
// cards come back, so the replica keeps its nodes instead of being moved.
func TestAnAutomaticReplicaWaitsForItsOwnLeavingInstances(t *testing.T) {
	f := newFailoverFixture(t, "auto-own", 3)
	dep := f.multiNode(t, "auto-own")
	f.auto(t, dep)
	if _, err := f.st.CreateInstance(&store.Instance{UUID: "auto-own-old", DeploymentID: dep.ID, NodeID: f.nodes[1].ID, State: proto.InstRemoving, GPUIndexes: "[0]", Headless: true, NodeRank: 1}); err != nil {
		t.Fatal(err)
	}
	insts, _ := f.st.InstancesForDeployment(dep.ID)
	deployment, ok, err := f.c.prepareUnits(dep, nil)
	if err != nil || !ok {
		t.Fatalf("prepare = %v %v", ok, err)
	}
	deployment.insts = insts
	slot := planning.UnitSlot{Unit: deployment.units[0]}
	moved, err := f.c.autoPlaceUnit(t.Context(), deployment, f.model, slot)
	if err != nil || moved {
		t.Fatalf("moved=%v err=%v, want the unit kept while its own instance leaves", moved, err)
	}
}

// An automatic PD role whose node's cards another deployment took moves to a
// spare node of the same network; the other role stays.
func TestAnAutomaticPDRoleMovesOffATakenNode(t *testing.T) {
	f := newPDFixture(t, "auto-pd", twoPrefillOneDecode)
	spare := addPDSpare(t, f, "auto-pd-n3")
	if err := f.st.SetDeploymentUnitsAuto(f.dep.ID, true, ""); err != nil {
		t.Fatal(err)
	}
	decode := f.nodes["n2"]
	other, err := createTestDeployment(t, f.st, &store.Deployment{Name: "auto-pd-other", ModelID: f.dep.ModelID, NodeID: decode.ID, ServedName: "auto-pd-other", Engine: "vllm", State: "active", Replicas: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.CreateInstance(&store.Instance{UUID: "auto-pd-other-inst", DeploymentID: other.ID, NodeID: decode.ID, State: proto.InstRunning, GPUIndexes: "[0]"}); err != nil {
		t.Fatal(err)
	}
	f.reconcile(t)
	f.reconcile(t)
	dep, _ := f.st.GetDeployment(f.dep.ID)
	pd, err := planning.PDOf(dep.Placement)
	if err != nil || pd == nil {
		t.Fatalf("placement = %s %v", dep.Placement, err)
	}
	if got := pd.Units("decode"); len(got) != 1 || got[0].NodeUUID != spare.UUID {
		t.Fatalf("decode = %+v, want on the spare %s", got, spare.UUID)
	}
	for _, unit := range pd.Units("prefill") {
		if unit.NodeUUID != f.nodes["n1"].UUID {
			t.Fatalf("prefill moved: %+v", pd.Units("prefill"))
		}
	}
	if events := eventCodes(t, f.st, f.dep.ID, EventUnitPlaced); len(events) != 1 || events[0]["role"] != "decode" {
		t.Fatalf("UNIT_PLACED = %v", events)
	}
}

// addPDSpare adds a node like the PD fixture's decode node: one RTX 4090 on
// the same network, with the model, online.
func addPDSpare(t *testing.T, f *pdFixture, name string) *store.Node {
	t.Helper()
	return addPDSpareAt(t, f, name, "10.0.0.3")
}

// addPDSpareAt is addPDSpare with the address its management interface has,
// for a fixture that needs more than one spare.
func addPDSpareAt(t *testing.T, f *pdFixture, name, ip string) *store.Node {
	t.Helper()
	spare, err := f.st.RegisterNode(name, store.HashToken("token-"+name), name)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.st.DB().Create(&store.GPU{NodeID: spare.ID, Index: 0, UUID: name + "-gpu0", Name: "NVIDIA GeForce RTX 4090", Vendor: "nvidia", ArchID: "sm_89", ComputeCapability: "8.9", MemoryTotal: 24 << 30}).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.UpsertModelFile(&store.ModelFile{ModelID: f.dep.ModelID, NodeID: spare.ID, State: proto.MFPresent, Path: "/models/pd"}); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	for _, node := range []*store.Node{f.nodes["n1"], f.nodes["n2"], spare} {
		updates := map[string]any{"state": store.NodeStateOnline, "last_seen_at": now}
		if node.ID == spare.ID {
			updates["network_inventory"] = fmt.Sprintf(`{"interfaces":[{"name":"eth0","addrs":["%[1]s/24"]}],"rdma_ports":[],"management":{"interface":"eth0","ip":"%[1]s"}}`, ip)
			updates["gpu_count"] = 1
		}
		if err := f.st.DB().Model(&store.Node{}).Where("id = ?", node.ID).Updates(updates).Error; err != nil {
			t.Fatal(err)
		}
	}
	for key, count := range map[string]int{"n1": 2, "n2": 1} {
		if err := f.st.DB().Model(&store.Node{}).Where("id = ?", f.nodes[key].ID).Update("gpu_count", count).Error; err != nil {
			t.Fatal(err)
		}
	}
	f.hub.online[spare.UUID] = true
	return spare
}
