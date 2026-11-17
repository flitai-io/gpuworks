package planning

import (
	"encoding/json"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// FleetAllocations charges the rows the caller holds, of any state, with the
// figures the per-node ledger gives: a replica awake on two cards, a sleeper
// beside it with its residue, and a stopped instance that charges nothing.
func TestFleetAllocationsMatchThePerNodeLedger(t *testing.T) {
	c := newYieldCards(t, 2)
	a := c.deployment(t, "a", "anytime")
	b := c.deployment(t, "b", "timeslice")
	c.instance(t, a, "a-1", "[0,1]", 0)
	c.instance(t, b, "b-1", "[1]", 1)
	c.instance(t, b, "b-gone", "[0]", 0)
	if err := c.st.DB().Model(&store.Instance{}).Where("uuid = ?", "b-gone").Update("state", proto.InstStopped).Error; err != nil {
		t.Fatal(err)
	}
	// The replica was admitted with 10 GiB of each card, which leaves room
	// for the sleeper's residue beside it.
	claim, _ := json.Marshal(proto.InstanceDesiredState{UUID: "a-1", DesiredState: "running", Sleepable: true, MemoryClaimBytes: 10 << 30})
	if err := c.st.DB().Model(&store.Instance{}).Where("uuid = ?", "a-1").Select("DesiredStateJSON").Updates(&store.Instance{DesiredStateJSON: string(claim)}).Error; err != nil {
		t.Fatal(err)
	}
	idle, err := c.st.RegisterNode("idle-node", store.HashToken("idle"), "idle-node")
	if err != nil {
		t.Fatal(err)
	}
	idle.GPUs = append(idle.GPUs, store.GPU{Index: 0, UUID: "idle-0", Vendor: "nvidia", MemoryTotal: 16 << 30})

	instances, err := c.st.ListInstances()
	if err != nil {
		t.Fatal(err)
	}
	deployments, err := c.st.ListDeployments()
	if err != nil {
		t.Fatal(err)
	}
	nodes := []store.Node{*c.node, *idle}
	got, err := FleetAllocations(c.st, nodes, instances, deployments)
	if err != nil {
		t.Fatal(err)
	}
	for i := range nodes {
		want := GPUAllocations(c.st, &nodes[i])
		fleet := got[nodes[i].ID]
		if len(fleet) != len(want) {
			t.Fatalf("node %s: %d cards charged, per-node ledger %d", nodes[i].Name, len(fleet), len(want))
		}
		for index, w := range want {
			g := fleet[index]
			if g.AllocatedBytes != w.AllocatedBytes || g.SleepingBytes != w.SleepingBytes || len(g.Instances) != len(w.Instances) {
				t.Errorf("node %s card %d: fleet %d/%d/%d instances, per-node %d/%d/%d", nodes[i].Name, index,
					g.AllocatedBytes, g.SleepingBytes, len(g.Instances), w.AllocatedBytes, w.SleepingBytes, len(w.Instances))
			}
		}
	}
	if card1 := got[c.node.ID][1]; card1.AllocatedBytes == 0 || card1.SleepingBytes == 0 || len(card1.Instances) != 2 {
		t.Fatalf("card 1 = %+v, want the replica awake and the sleeper's residue, nothing for the stopped row", card1)
	}
	if len(got[idle.ID]) != 0 {
		t.Fatalf("a node with no instances charges %v, want nothing", got[idle.ID])
	}
}

// A memory-limited slice is charged its limit plus the engine's reserve,
// sized by its deployment's gpu_memory_utilization. The fleet pass reads that from the
// deployments it is handed; the figures are the per-node ledger's, for a
// deployment that sets it, one that does not, and one whose quick params do
// not parse.
func TestFleetAllocationsChargeSlicesAsThePerNodeLedger(t *testing.T) {
	c := newYieldCards(t, 3)
	for i, quick := range []string{`{"gpu_memory_utilization":0.55}`, ``, `not json`} {
		dep := c.deployment(t, "slice-"+string(rune('a'+i)), "never")
		if err := c.st.DB().Model(&store.Deployment{}).Where("id = ?", dep.ID).Update("quick_params", quick).Error; err != nil {
			t.Fatal(err)
		}
		uuid := "slice-inst-" + string(rune('a'+i))
		c.instance(t, dep, uuid, "["+string(rune('0'+i))+"]", 0)
		spec, _ := json.Marshal(proto.InstanceDesiredState{UUID: uuid, DesiredState: "running", Engine: "vllm", VRAMLimitBytes: 8 << 30})
		if err := c.st.DB().Model(&store.Instance{}).Where("uuid = ?", uuid).Select("DesiredStateJSON").Updates(&store.Instance{DesiredStateJSON: string(spec)}).Error; err != nil {
			t.Fatal(err)
		}
	}
	instances, err := c.st.ListInstances()
	if err != nil {
		t.Fatal(err)
	}
	deployments, err := c.st.ListDeployments()
	if err != nil {
		t.Fatal(err)
	}
	got, err := FleetAllocations(c.st, []store.Node{*c.node}, instances, deployments)
	if err != nil {
		t.Fatal(err)
	}
	want := GPUAllocations(c.st, c.node)
	for index := 0; index < 3; index++ {
		g, w := got[c.node.ID][index], want[index]
		if w.AllocatedBytes == 0 {
			t.Fatalf("card %d: the per-node ledger charges the slice nothing; the fixture is wrong", index)
		}
		if g.AllocatedBytes != w.AllocatedBytes {
			t.Errorf("card %d: fleet charges %d, per-node ledger %d", index, g.AllocatedBytes, w.AllocatedBytes)
		}
	}
	if want[0].AllocatedBytes == want[1].AllocatedBytes {
		t.Fatal("gpu_memory_utilization 0.55 and the default charge the same: the case does not exercise the deployment's setting")
	}
}
