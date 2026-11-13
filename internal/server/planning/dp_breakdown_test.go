package planning

import (
	"testing"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/parallelism"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestAutomaticDPBreakdownMatchesExplicitParallelism(t *testing.T) {
	st, err := store.Open("file:"+t.Name()+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	node := &store.Node{UUID: "dp-breakdown"}
	for i := 0; i < 4; i++ {
		node.GPUs = append(node.GPUs, store.GPU{Index: i, MemoryTotal: 80 << 30, Vendor: "nvidia", ArchID: "sm_80", ComputeCapability: "8.0"})
	}
	in := Input{Model: &store.Model{Name: "dp-model", WeightBytes: 8 << 30}, Node: node,
		Engine: deploy.EngineVLLM, Quick: deploy.LaunchConfig{GPUMemUtil: 0.9}, Parallel: parallelism.Spec{DP: 2}}
	auto := in.Evaluate(st)
	in.Parallel.TP = 1
	explicit := in.Evaluate(st)
	if !auto.Result.OK || !explicit.Result.OK {
		t.Fatalf("auto=%+v explicit=%+v", auto.Result, explicit.Result)
	}
	if auto.Breakdown.PerGPUBytes != explicit.Breakdown.PerGPUBytes {
		t.Fatalf("auto weight=%d explicit weight=%d", auto.Breakdown.PerGPUBytes, explicit.Breakdown.PerGPUBytes)
	}
}
