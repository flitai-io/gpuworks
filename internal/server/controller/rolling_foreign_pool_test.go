package controller

import (
	"context"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// A pool replica's replacement is placed across the whole pool, so a node
// that has stopped allowing sharing with processes outside the platform does
// not hold the roll when another pool node takes the replacement: only a
// replica that cannot move is kept (ADR 0068).
func TestRollingRestartDrainsAPoolReplicaOnASwitchedOffNode(t *testing.T) {
	st, err := store.Open("file:rolling_foreign_pool?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	model, err := st.UpsertModel(&store.Model{
		Source: "huggingface", SourceID: "fixture/rolling-foreign-pool", Revision: "main", Name: "rolling-foreign-pool", WeightBytes: 15 << 30,
		Meta: `{"model_type":"qwen3","num_hidden_layers":1,"hidden_size":256,"num_attention_heads":8,"num_key_value_heads":8,"max_position_embeddings":2048}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	shared := foreignHeldH20(t, st, "roll-shared", proto.PartitionSafetyForeignTracked)
	clean := foreignHeldH20(t, st, "roll-clean", proto.PartitionSafetyForeignTracked)
	if err := st.DB().Model(&store.GPU{}).Where("node_id = ?", clean.ID).Update("foreign_processes", "").Error; err != nil {
		t.Fatal(err)
	}
	for _, n := range []*store.Node{shared, clean} {
		if _, err := st.UpsertModelFile(&store.ModelFile{ModelID: model.ID, NodeID: n.ID, State: proto.MFPresent, Path: "/models/rolling-foreign-pool"}); err != nil {
			t.Fatal(err)
		}
	}
	other, err := createTestDeployment(t, st, &store.Deployment{Name: "whole-card", ModelID: model.ID, NodeID: shared.ID, ServedName: "whole-card", Engine: "vllm", State: "active", Replicas: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateInstance(&store.Instance{UUID: "roll-whole-card-1", DeploymentID: other.ID, NodeID: shared.ID, State: proto.InstRunning, GPUIndexes: "[1]"}); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := st.SetNodePlacementPolicy(shared.UUID, func(p store.NodePlacementPolicy) (store.NodePlacementPolicy, error) {
		p.ShareWithForeign = true
		return p, nil
	}); err != nil {
		t.Fatal(err)
	}
	dep, err := createTestDeploymentWithRequirements(t, st, &store.Deployment{
		Name: "roll-pool", ModelID: model.ID, NodeID: shared.ID, ServedName: "roll-pool", Engine: "vllm", State: "active",
		Placement: `{"tp":1}`, QuickParams: `{"max_model_len":512}`, Replicas: 1, PlacementStrategy: store.PlacementBinpack,
		VRAMLimitBytes: 16 << 30, PartitionBackend: accelerator.BackendNVIDIALogical,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Placed while only the shared node was online and allowed sharing.
	hub := &distributedTestHub{online: map[string]bool{shared.UUID: true}, updates: map[string][]proto.StateUpdate{}}
	c := New(st, hub, nil)
	c.SetDrainProbe(fakeDrainProbe{})
	if err := c.reconcileDeployment(context.Background(), dep.ID); err != nil {
		t.Fatal(err)
	}
	insts, err := st.InstancesForDeployment(dep.ID)
	if err != nil || len(insts) != 1 || insts[0].NodeID != shared.ID || insts[0].GPUIndexes != "[0]" {
		t.Fatalf("instances = %+v %v, want one on the shared node's held card", insts, err)
	}
	replica := insts[0].UUID
	_ = st.SetInstanceState(replica, proto.InstRunning, "")
	if err := st.DB().Model(&store.Instance{}).Where("uuid = ?", replica).Update("created_at", time.Now().UTC().Add(-time.Hour)).Error; err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := st.SetNodePlacementPolicy(shared.UUID, func(p store.NodePlacementPolicy) (store.NodePlacementPolicy, error) {
		p.ShareWithForeign = false
		return p, nil
	}); err != nil {
		t.Fatal(err)
	}
	hub.online[clean.UUID] = true
	if err := st.RequestRollingRestart(dep.ID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := c.reconcileDeployment(context.Background(), dep.ID); err != nil {
		t.Fatal(err)
	}
	got, _ := st.InstanceByUUID(replica)
	if got.State != proto.InstDraining {
		last, _, _ := st.LatestScheduleEvent(dep.ID)
		t.Fatalf("replica %s (last schedule event %+v), want draining: the clean node takes the replacement", got.State, last.Data)
	}
}
