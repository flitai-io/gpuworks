package controller

import (
	"context"
	"testing"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// A replica an operator asked to start on a named node goes there when it
// fits, even when it fits only beside processes outside the platform and
// another node has a clean card: the clean-card preference ranks nodes the
// scheduler chooses, not one the operator named (ADR 0068).
func TestPoolPlacementHintWinsOverTheCleanCardPreference(t *testing.T) {
	st, err := store.Open("file:foreign_pool_hint?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	model, err := st.UpsertModel(&store.Model{
		Source: "huggingface", SourceID: "fixture/pool-hint", Revision: "main", Name: "pool-hint", WeightBytes: 15 << 30,
		Meta: `{"model_type":"qwen3","num_hidden_layers":1,"hidden_size":256,"num_attention_heads":8,"num_key_value_heads":8,"max_position_embeddings":2048}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	shared := foreignHeldH20(t, st, "hint-shared", proto.PartitionSafetyForeignTracked)
	if _, _, _, err := st.SetNodePlacementPolicy(shared.UUID, func(p store.NodePlacementPolicy) (store.NodePlacementPolicy, error) {
		p.ShareWithForeign = true
		return p, nil
	}); err != nil {
		t.Fatal(err)
	}
	clean := foreignHeldH20(t, st, "hint-clean", proto.PartitionSafetyForeignTracked)
	if err := st.DB().Model(&store.GPU{}).Where("node_id = ?", clean.ID).Update("foreign_processes", "").Error; err != nil {
		t.Fatal(err)
	}
	for _, n := range []*store.Node{shared, clean} {
		if _, err := st.UpsertModelFile(&store.ModelFile{ModelID: model.ID, NodeID: n.ID, State: proto.MFPresent, Path: "/models/pool-hint"}); err != nil {
			t.Fatal(err)
		}
	}
	other, err := createTestDeployment(t, st, &store.Deployment{Name: "whole-card", ModelID: model.ID, NodeID: shared.ID, ServedName: "whole-card", Engine: "vllm", State: "active", Replicas: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateInstance(&store.Instance{UUID: "hint-whole-card-1", DeploymentID: other.ID, NodeID: shared.ID, State: proto.InstRunning, GPUIndexes: "[1]"}); err != nil {
		t.Fatal(err)
	}
	dep, err := createTestDeploymentWithRequirements(t, st, &store.Deployment{
		Name: "pool-hint", ModelID: model.ID, NodeID: shared.ID, ServedName: "pool-hint", Engine: "vllm", State: "active",
		// Spread ranks the node with the most room first: the clean one.
		Placement: `{"tp":1}`, QuickParams: `{"max_model_len":512}`, Replicas: 1, PlacementStrategy: store.PlacementSpread,
		VRAMLimitBytes: 16 << 30, PartitionBackend: accelerator.BackendNVIDIALogical,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	hub := &distributedTestHub{online: map[string]bool{shared.UUID: true, clean.UUID: true}, updates: map[string][]proto.StateUpdate{}}
	c := New(st, hub, nil)
	c.Orders().HintPlacement(dep.ID, []string{shared.UUID})
	if err := c.reconcileDeployment(context.Background(), dep.ID); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	instances, err := st.InstancesForDeployment(dep.ID)
	if err != nil || len(instances) != 1 {
		t.Fatalf("instances = %+v (%v), want one", instances, err)
	}
	if instances[0].NodeID != shared.ID {
		t.Fatalf("placed on node %d GPUs %s, want the hinted node %d", instances[0].NodeID, instances[0].GPUIndexes, shared.ID)
	}
}
