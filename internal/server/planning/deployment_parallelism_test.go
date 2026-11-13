package planning

import (
	"testing"

	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestStoredDeploymentPoolNeedIncludesDataParallel(t *testing.T) {
	dep := &store.Deployment{Engine: "vllm", QuickParams: `{"data_parallel_size":2}`, Placement: `{"tp":1}`}
	if got := ReplicaGPUNeed(dep); got != 2 {
		t.Fatalf("pool need = %d, want 2", got)
	}
	dep.ExtraArgs = "--data-parallel-size 4"
	if got := ReplicaGPUNeed(dep); got != 4 {
		t.Fatalf("explicit DP need = %d, want 4", got)
	}
	dep.Placement = `{}`
	if got := ReplicaGPUNeed(dep); got != 4 {
		t.Fatalf("automatic TP with DP4 need = %d, want 4", got)
	}
	dep.Placement = `{"pp":2}`
	dep.ExtraArgs = "--data-parallel-size 2"
	if got := ReplicaGPUNeed(dep); got != 4 {
		t.Fatalf("automatic TP with PP2 DP2 need = %d, want 4", got)
	}
}
