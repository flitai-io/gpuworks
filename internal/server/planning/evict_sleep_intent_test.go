package planning

import (
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/scheduler"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// suggestEviction must skip a replica by the same predicate every other reader
// uses. The intent and the reported level were spelled out by hand here
// (InstanceSleepIntent > 0 || SleepLevel > 0), which happened to agree with
// planning.InstanceAsleep — the point of this test is that it keeps agreeing
// through ONE spelling. An instance ordered to sleep is already refused by the
// gateway, so suggesting it for eviction offers the operator a replica that is
// on its way out anyway, and the card it would release is already spoken for.
//
// The drain window (order given, engine silent) is the case that separates the
// two spellings: intent 1, reported level 0.
func TestEvictionSkipsAnInstanceOrderedAsleepBeforeTheEngineReports(t *testing.T) {
	st, err := store.Open("file:planning_evict_intent_"+strings.ReplaceAll(t.Name(), "/", "_")+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	node, err := st.RegisterNode("evict-intent-node", store.HashToken("token"), "evict-intent-node")
	if err != nil {
		t.Fatal(err)
	}
	node.GPUs = []store.GPU{{Index: 0, UUID: "gpu-0", Vendor: "nvidia", ArchID: "sm_80", ComputeCapability: "8.0", MemoryTotal: 16 << 30}}
	dep, err := st.CreateDeployment(&store.Deployment{Name: "sleepable-intent", ModelID: 1, NodeID: node.ID, ServedName: "intent", Engine: "vllm", Sleepable: true, State: "active"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateInstance(&store.Instance{UUID: "sleep-intent", DeploymentID: dep.ID, NodeID: node.ID, State: "running", GPUIndexes: "[0]", DesiredStateJSON: sleepSpec("sleep-intent", 0)}); err != nil {
		t.Fatal(err)
	}
	req := scheduler.Request{WeightBytes: 8 << 30, HeadCount: 8, GPUMemUtil: deploy.DefaultGPUMemUtil("vllm")}
	// Awake first: the instance is a legitimate eviction candidate, which is
	// what makes its disappearance below meaningful.
	if got := suggestEviction(st, node, req, ledgerView{}); len(got) != 1 || got[0].InstanceUUID != "sleep-intent" {
		t.Fatalf("awake instance not suggested: %+v", got)
	}

	// Now order the sleep and leave the reported level at 0: the drain window.
	if err := st.OrderInstancesSleep(dep.ID, []string{"sleep-intent"}, 1, "wait"); err != nil {
		t.Fatal(err)
	}
	inst, err := st.InstanceByUUID("sleep-intent")
	if err != nil {
		t.Fatal(err)
	}
	if inst.SleepIntent() != 1 || inst.SleepLevel != 0 {
		t.Fatalf("fixture intent=%d level=%d, want an order in flight (1, 0)", inst.SleepIntent(), inst.SleepLevel)
	}
	if got := suggestEviction(st, node, req, ledgerView{}); got != nil {
		t.Fatalf("suggested evicting an instance already ordered to sleep: %+v", got)
	}
}
