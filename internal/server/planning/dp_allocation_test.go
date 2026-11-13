package planning

import (
	"testing"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/parallelism"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestAutomaticPlanAllocatesEveryDataParallelReplica(t *testing.T) {
	st, err := store.Open("file:planning_dp_allocation?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	node, err := st.RegisterNode("dp-node", store.HashToken("token"), "dp-node")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		node.GPUs = append(node.GPUs, store.GPU{Index: i, MemoryTotal: 80 << 30, Vendor: "nvidia", ArchID: "sm_80", ComputeCapability: "8.0"})
	}
	model := &store.Model{ID: 1, Name: "dp-model", WeightBytes: 8 << 30}
	for _, tc := range []struct{ tp, dp, nodes, want int }{{1, 2, 1, 2}, {2, 2, 1, 4}, {2, 2, 2, 2}} {
		out := (Input{Model: model, Node: node, Engine: deploy.EngineVLLM, Quick: deploy.LaunchConfig{GPUMemUtil: 0.9}, Parallel: parallelism.Spec{TP: tc.tp, DP: tc.dp, NNodes: tc.nodes}}).Evaluate(st)
		if !out.Result.OK || out.Result.Plan == nil || len(out.Result.Plan.GPUIndexes) != tc.want {
			t.Fatalf("TP%d DP%d nodes%d: %+v", tc.tp, tc.dp, tc.nodes, out.Result)
		}
	}
}
