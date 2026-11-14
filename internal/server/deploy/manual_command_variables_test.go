package deploy

import (
	"slices"
	"testing"
)

func TestManualCommandVariablesListsWhatTheLaunchesState(t *testing.T) {
	engine, err := SelectEngine(EngineVLLM)
	if err != nil {
		t.Fatal(err)
	}
	single := ResolveLaunchInput{Engine: engine, Params: Params{APIPort: 30000, AssignedGPUs: 1, TensorParallel: 1}, PlacementVariables: map[string]any{"EXPERT_SLOTS": int64(4)}}
	got := ManualCommandVariables([]ResolveLaunchInput{single})
	want := []string{"GPUWORKS_API_HOST", "GPUWORKS_API_PORT", "GPUWORKS_EXPERT_SLOTS", "GPUWORKS_GPU_COUNT", "GPUWORKS_HEADLESS", "GPUWORKS_MODEL_PATH", "GPUWORKS_NNODES", "GPUWORKS_NODE_RANK", "GPUWORKS_TP_SIZE"}
	if !slices.Equal(got, want) {
		t.Fatalf("single launch = %v, want %v", got, want)
	}

	// A name only one launch states is listed; an unresolved preview has no
	// GPU count or degree to state yet.
	decode := ResolveLaunchInput{Engine: engine, AllowUnresolved: true, Params: Params{APIPort: 30000, NNodes: 2, MasterAddr: "192.0.2.10", MasterPort: 29500, PD: &PDLaunch{Role: "decode", SideChannelPort: 30001}}}
	prefill := decode
	prefill.Params.PD = &PDLaunch{Role: "prefill"}
	got = ManualCommandVariables([]ResolveLaunchInput{prefill, decode})
	for _, name := range []string{"GPUWORKS_PD_ROLE", "GPUWORKS_PD_SIDE_CHANNEL_PORT", "GPUWORKS_MASTER_ADDR", "GPUWORKS_MASTER_PORT"} {
		if !slices.Contains(got, name) {
			t.Fatalf("PD launches = %v, lack %s", got, name)
		}
	}
	for _, name := range []string{"GPUWORKS_GPU_COUNT", "GPUWORKS_TP_SIZE", "GPUWORKS_PD_SIDE_CHANNEL_HOST", "GPUWORKS_NODE_ADDR"} {
		if slices.Contains(got, name) {
			t.Fatalf("PD launches = %v, state %s none of them has", got, name)
		}
	}
}
