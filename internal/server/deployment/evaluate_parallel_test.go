package deployment

import (
	"errors"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// What create stores: the parallelism that takes effect, stated.
func TestDistributedRequestStatesItsParallelism(t *testing.T) {
	st, err := store.Open("file:"+t.Name()+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	for i, uuid := range []string{"par-a", "par-b"} {
		launchConfigNode(t, st, uuid)
		if err := st.UpdateNodeStatus(uuid, nodeStatusTwoGPUs(uuid, i)); err != nil {
			t.Fatal(err)
		}
	}
	h := New(Deps{Store: st})
	for _, tc := range []struct {
		placement map[string]any
		tp, pp    int
	}{
		{nil, 2, 2},
		{map[string]any{"pp": float64(1)}, 4, 1},
		{map[string]any{"tp": float64(2)}, 2, 1},
	} {
		req := dto.DeployRequest{NodeUUIDs: []string{"par-a", "par-b"}, NNodes: 2, Placement: tc.placement}
		if _, err := h.PrepareDistributedDeployRequest(&req, 1); err != nil {
			t.Fatalf("%v: %v", tc.placement, err)
		}
		if req.Placement["tp"] != tc.tp || req.Placement["pp"] != tc.pp {
			t.Fatalf("%v stored as %v, want tp %d pp %d", tc.placement, req.Placement, tc.tp, tc.pp)
		}
	}
}

func TestDistributedDPDefaultsTPAfterCountingReplicas(t *testing.T) {
	st, err := store.Open("file:"+t.Name()+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for i, uuid := range []string{"dp-default-a", "dp-default-b"} {
		launchConfigNode(t, st, uuid)
		if err := st.UpdateNodeStatus(uuid, nodeStatusTwoGPUs(uuid, i)); err != nil {
			t.Fatal(err)
		}
	}
	h := New(Deps{Store: st})
	req := dto.DeployRequest{Engine: "vllm", NodeUUIDs: []string{"dp-default-a", "dp-default-b"}, NNodes: 2, QuickParams: map[string]any{"data_parallel_size": 2}}
	if _, err := h.PrepareDistributedDeployRequest(&req, 2); err != nil {
		t.Fatal(err)
	}
	if req.Placement["tp"] != 1 || req.Placement["pp"] != 2 {
		t.Fatalf("wrong per-copy topology: %v", req.Placement)
	}
}

func TestDistributedDPRequiresReplicaCardsOnEachNode(t *testing.T) {
	st, err := store.Open("file:"+t.Name()+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for i, uuid := range []string{"dp-a", "dp-b"} {
		launchConfigNode(t, st, uuid)
		status := nodeStatusTwoGPUs(uuid, i)
		status.GPUs = status.GPUs[:1]
		if err := st.UpdateNodeStatus(uuid, status); err != nil {
			t.Fatal(err)
		}
	}
	h := New(Deps{Store: st})
	req := dto.DeployRequest{NodeUUIDs: []string{"dp-a", "dp-b"}, NNodes: 2, Placement: map[string]any{"tp": float64(2)}}
	_, err = h.PrepareDistributedDeployRequest(&req, 2)
	var refusal *messages.ProductError
	if !errors.As(err, &refusal) || refusal.Code() != messages.ErrorDistributedGPUCount || refusal.Params()["required"] != 2 || refusal.Params()["available"] != 1 {
		t.Fatalf("TP2 DP2 on one-card nodes must report required=2, available=1: %v", err)
	}
}
