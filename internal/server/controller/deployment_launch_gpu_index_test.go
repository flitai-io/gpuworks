package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/planning"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// sparseGPUNode reproduces the hardware a node reports once some of its cards
// are passed through to other tenants: the remaining accelerators keep their
// own hardware indexes (0, 3, 4, 6), so an index is neither dense nor bounded
// by the number of cards the node still owns. The Turing cards carry a compute
// capability below 8.0, which the engine builder turns into an observable
// `--dtype float16`; the Ampere cards do not.
func sparseGPUNode(t *testing.T, st *store.Store, name string) *store.Node {
	t.Helper()
	node, err := st.RegisterNode(name, store.HashToken(name), name)
	if err != nil {
		t.Fatal(err)
	}
	cards := []struct {
		index             int
		name              string
		archID            string
		computeCapability string
		memoryTotal       int64
	}{
		{0, "NVIDIA A100-SXM4-80GB", "sm_80", "8.0", 80 << 30},
		{3, "NVIDIA Tesla T4", "sm_75", "7.5", 16 << 30},
		{4, "NVIDIA Tesla T4", "sm_75", "7.5", 16 << 30},
		{6, "NVIDIA A100-SXM4-80GB", "sm_80", "8.0", 80 << 30},
	}
	for _, card := range cards {
		if err := st.DB().Create(&store.GPU{
			NodeID: node.ID, Index: card.index, UUID: fmt.Sprintf("%s-gpu-%d", name, card.index),
			Name: card.name, Vendor: "nvidia", ArchID: card.archID,
			ComputeCapability: card.computeCapability, MemoryTotal: card.memoryTotal,
		}).Error; err != nil {
			t.Fatal(err)
		}
	}
	reloaded, err := st.NodeWithGPUs(node.UUID)
	if err != nil {
		t.Fatal(err)
	}
	return reloaded
}

// TestReconcileDeploymentReadsGPUFactsByHardwareIndex pins the launch to a card
// the scheduler names by hardware index and proves the launch carries that
// card's facts.
//
// Both cases used to read node.GPUs[index], i.e. a slice position:
//   - index 4 on a four-card node panicked the whole server. systemd restarted
//     it, the same deployment was reconciled again, and the only way out of the
//     crash loop was deleting the deployment.
//   - index 3 silently described the card at slice position 3 (hardware index
//     6) instead, so a mixed node launched with the wrong compute capability.
func TestReconcileDeploymentReadsGPUFactsByHardwareIndex(t *testing.T) {
	cases := []struct {
		name   string
		dsn    string
		node   string
		pinned int
	}{
		{name: "index_beyond_the_reported_card_count", dsn: "file:controller_sparse_gpu_index_4?mode=memory&cache=shared", node: "sparse-gpu-node-4", pinned: 4},
		{name: "index_whose_slice_position_holds_another_card", dsn: "file:controller_sparse_gpu_index_3?mode=memory&cache=shared", node: "sparse-gpu-node-3", pinned: 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st, err := store.Open(tc.dsn, bus.New())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = st.Close() })
			node := sparseGPUNode(t, st, tc.node)
			model, err := st.UpsertModel(&store.Model{
				Source: "huggingface", SourceID: "fixture/sparse-gpu", Revision: "main", Name: "sparse-gpu",
				WeightBytes: 1 << 30,
				Meta:        `{"model_type":"qwen3","num_hidden_layers":1,"hidden_size":256,"num_attention_heads":8,"num_key_value_heads":8,"max_position_embeddings":2048}`,
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := st.UpsertModelFile(&store.ModelFile{ModelID: model.ID, NodeID: node.ID, State: proto.MFPresent, Path: "/models/sparse-gpu"}); err != nil {
				t.Fatal(err)
			}
			dep, err := createTestDeployment(t, st, &store.Deployment{
				Name: "sparse-gpu", ModelID: model.ID, NodeID: node.ID, ServedName: "sparse-gpu", Engine: "vllm", State: "active",
				Placement: fmt.Sprintf(`{"tp":1,"gpus":[%d]}`, tc.pinned), QuickParams: `{"max_model_len":512}`,
			})
			if err != nil {
				t.Fatal(err)
			}
			hub := &distributedTestHub{online: map[string]bool{node.UUID: true}, updates: map[string][]proto.StateUpdate{}}
			c := New(st, hub, nil)
			if err := c.reconcileDeployment(context.Background(), dep.ID); err != nil {
				t.Fatalf("reconcile: %v", err)
			}

			updates := hub.updates[node.UUID]
			if len(updates) != 1 || len(updates[0].Instances) != 1 {
				t.Fatalf("state updates=%+v, want one instance push", updates)
			}
			spec := updates[0].Instances[0]
			if len(spec.GPUIndexes) != 1 || spec.GPUIndexes[0] != tc.pinned {
				t.Fatalf("launched on GPU indexes %v, want [%d]", spec.GPUIndexes, tc.pinned)
			}
			if !argvHasPair(spec.Argv, "--dtype", "float16") {
				t.Fatalf("launch argv %v lacks the float16 dtype the compute capability of GPU %d (7.5) requires; the launch used another card's facts", spec.Argv, tc.pinned)
			}

			instances, err := st.InstancesForDeployment(dep.ID)
			if err != nil || len(instances) != 1 {
				t.Fatalf("instances=%+v err=%v", instances, err)
			}
			var persisted []int
			if err := json.Unmarshal([]byte(instances[0].GPUIndexes), &persisted); err != nil {
				t.Fatalf("persisted GPU indexes %q: %v", instances[0].GPUIndexes, err)
			}
			if len(persisted) != 1 || persisted[0] != tc.pinned {
				t.Fatalf("persisted GPU indexes %v, want [%d]", persisted, tc.pinned)
			}
		})
	}
}

// TestReconcileDeploymentAmpereCardKeepsDefaultDtype is the control for the
// case above: the assertion there only means something because an Ampere card
// on the very same node does not get `--dtype float16`.
func TestReconcileDeploymentAmpereCardKeepsDefaultDtype(t *testing.T) {
	st, err := store.Open("file:controller_sparse_gpu_index_6?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	node := sparseGPUNode(t, st, "sparse-gpu-node-6")
	model, err := st.UpsertModel(&store.Model{
		Source: "huggingface", SourceID: "fixture/sparse-gpu-ampere", Revision: "main", Name: "sparse-gpu-ampere",
		WeightBytes: 1 << 30,
		Meta:        `{"model_type":"qwen3","num_hidden_layers":1,"hidden_size":256,"num_attention_heads":8,"num_key_value_heads":8,"max_position_embeddings":2048}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertModelFile(&store.ModelFile{ModelID: model.ID, NodeID: node.ID, State: proto.MFPresent, Path: "/models/sparse-gpu-ampere"}); err != nil {
		t.Fatal(err)
	}
	dep, err := createTestDeployment(t, st, &store.Deployment{
		Name: "sparse-gpu-ampere", ModelID: model.ID, NodeID: node.ID, ServedName: "sparse-gpu-ampere", Engine: "vllm", State: "active",
		Placement: `{"tp":1,"gpus":[6]}`, QuickParams: `{"max_model_len":512}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	hub := &distributedTestHub{online: map[string]bool{node.UUID: true}, updates: map[string][]proto.StateUpdate{}}
	c := New(st, hub, nil)
	if err := c.reconcileDeployment(context.Background(), dep.ID); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	updates := hub.updates[node.UUID]
	if len(updates) != 1 || len(updates[0].Instances) != 1 {
		t.Fatalf("state updates=%+v, want one instance push", updates)
	}
	spec := updates[0].Instances[0]
	if len(spec.GPUIndexes) != 1 || spec.GPUIndexes[0] != 6 {
		t.Fatalf("launched on GPU indexes %v, want [6]", spec.GPUIndexes)
	}
	if argvHasPair(spec.Argv, "--dtype", "float16") {
		t.Fatalf("launch argv %v forces float16 on an Ampere card (compute capability 8.0)", spec.Argv)
	}
}

func argvHasPair(argv []string, flag, value string) bool {
	for i, arg := range argv {
		if arg == flag && i+1 < len(argv) && argv[i+1] == value {
			return true
		}
		if strings.HasPrefix(arg, flag+"=") && strings.TrimPrefix(arg, flag+"=") == value {
			return true
		}
	}
	return false
}

// TestLaunchGPUReportsAnIndexTheNodeNoLongerReports covers the other half of
// the lookup: a GPU index the node does not report is a reconcile error naming
// the index and the node, never an out-of-range panic. The scheduler picks from
// the very card list read here, so this is the defensive edge - a node whose
// report changed under a reconcile pass - and it must fail like any other
// reconcile error rather than taking the process down.
func TestLaunchGPUReportsAnIndexTheNodeNoLongerReports(t *testing.T) {
	node := &store.Node{UUID: "sparse-node", GPUs: []store.GPU{
		{Index: 0, UUID: "sparse-node-gpu-0", Name: "Ascend910B4", Vendor: "ascend", ArchID: "910B4"},
		{Index: 3, UUID: "sparse-node-gpu-3", Name: "Ascend910B4", Vendor: "ascend", ArchID: "910B4"},
	}}
	gpu, err := planning.LaunchGPU(node, 3)
	if err != nil {
		t.Fatalf("GPU 3 is reported by the node: %v", err)
	}
	if gpu.UUID != "sparse-node-gpu-3" {
		t.Fatalf("GPU 3 resolved to %q", gpu.UUID)
	}
	// 4 is past the end of the slice, 1 is a valid slice position holding
	// another card; neither is a card this node reports.
	for _, index := range []int{4, 1} {
		got, err := planning.LaunchGPU(node, index)
		if err == nil {
			t.Fatalf("GPU %d is not reported by the node but resolved to %+v", index, got)
		}
		message := err.Error()
		if !strings.Contains(message, fmt.Sprintf("GPU %d", index)) || !strings.Contains(message, node.UUID) {
			t.Fatalf("error %q does not name both GPU %d and node %s", message, index, node.UUID)
		}
	}
}
