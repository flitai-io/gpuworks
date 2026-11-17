package planning

import (
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/netplan"
	"gpuworks.ai/gpuworks/internal/server/nodelabel"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// LoadUnitSuggestFacts must not charge a node twice for a replica that is
// physically still there.
//
// holding[] counts the instances a node already carries so a listed rank that
// an existing instance occupies is matched to it rather than claiming new
// cards (claimed[node] only grows for ranks holding[] cannot cover). This is a
// question about hardware occupancy, not about routing: an instance ordered to
// sleep still holds its GPUs — store.AwakeGPUHolders keeps excluding it on the
// reported level for exactly that reason — so it must keep counting in
// holding[]. Judging it asleep here counts the same card twice and reports the
// node as full when it has a free GPU, which blocks the failover placement that
// this function exists to suggest.
func TestUnitSuggestDoesNotDoubleChargeACardHeldByAnOrderedSleeper(t *testing.T) {
	st, err := store.Open("file:unit_suggest_intent?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	node, err := st.RegisterNode("unit-node", store.HashToken("token-unit"), "unit-node")
	if err != nil {
		t.Fatal(err)
	}
	node.GPUs = []store.GPU{
		{NodeID: node.ID, Index: 0, UUID: "gpu-0", Vendor: "nvidia", ComputeCapability: "8.0", MemoryTotal: 48 << 30},
		{NodeID: node.ID, Index: 1, UUID: "gpu-1", Vendor: "nvidia", ComputeCapability: "8.0", MemoryTotal: 48 << 30},
	}
	for i := range node.GPUs {
		if err := st.DB().Create(&node.GPUs[i]).Error; err != nil {
			t.Fatal(err)
		}
	}
	model, err := st.UpsertModel(&store.Model{Name: "unit-model", Source: "huggingface", SourceID: "org/unit-model", WeightBytes: 1 << 30})
	if err != nil {
		t.Fatal(err)
	}
	dep, err := st.CreateDeployment(&store.Deployment{Name: "unit", ModelID: model.ID, NodeID: node.ID, ServedName: "unit-served", Engine: "vllm", Sleepable: true, State: "active"})
	if err != nil {
		t.Fatal(err)
	}
	// The deployment's one existing replica sits on GPU 0.
	if _, err := st.CreateInstance(&store.Instance{
		UUID: "unit-1", DeploymentID: dep.ID, NodeID: node.ID, State: proto.InstRunning,
		GPUIndexes: "[0]", NNodes: 1, DesiredStateJSON: sleepSpec("unit-1", 0),
	}); err != nil {
		t.Fatal(err)
	}
	// Order it to sleep and leave the reported level at 0: the drain window.
	if err := st.OrderInstancesSleep(dep.ID, []string{"unit-1"}, proto.SleepLevelL1, "wait"); err != nil {
		t.Fatal(err)
	}
	inst, err := st.InstanceByUUID("unit-1")
	if err != nil {
		t.Fatal(err)
	}
	if inst.SleepIntent() == 0 || inst.SleepLevel != 0 {
		t.Fatalf("fixture intent=%d level=%d, want an order in flight (>0, 0)", inst.SleepIntent(), inst.SleepLevel)
	}

	resolve := func([]store.Node) (netplan.Resolution, error) { return netplan.Resolution{}, nil }
	// The deployment lists one rank taking one card on this node: the rank the
	// existing replica already fills.
	listed := []ListedRank{{NodeUUID: node.UUID, GPUs: 1, Leader: true}}
	facts, err := LoadUnitSuggestFacts(st, resolve, dep, nodelabel.Requirements{}, listed, func(string) bool { return true }, nil, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	// The existing replica still holds GPU 0 (AwakeGPUHolders excludes only
	// reported sleepers), so the listed rank must be matched to it and claim
	// nothing new.
	if facts.Claimed[node.UUID] != 0 {
		t.Fatalf("claimed = %d, want 0: the listed rank is already filled by the instance holding GPU 0, so charging it again counts one card twice",
			facts.Claimed[node.UUID])
	}
	if free := FreeHealthyGPUs(node, facts.Held[node.ID]) - facts.Claimed[node.UUID]; free != 1 {
		t.Fatalf("free GPUs = %d, want 1: GPU 1 is unused, and a failover suggestion needs to see it", free)
	}
}
