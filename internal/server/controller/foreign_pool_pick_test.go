package controller

import (
	"context"
	"encoding/json"
	"testing"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// A pool replica goes to a node where it fits on a card free of processes
// outside the platform, even when a better-ranked node fits it only beside
// them (planning.FitPicker); with no such node it takes the shared card
// rather than wait.
func TestPoolPlacementPrefersANodeWithACleanCard(t *testing.T) {
	for i, tc := range []struct {
		name      string
		withClean bool
		want      string
	}{
		{"a clean node further down the ranking", true, "clean"},
		{"only the shared node", false, "shared"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, err := store.Open("file:foreign_pool_pick_"+string(rune('a'+i))+"?mode=memory&cache=shared", bus.New())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = st.Close() })
			model, err := st.UpsertModel(&store.Model{
				Source: "huggingface", SourceID: "fixture/pool-pick", Revision: "main", Name: "pool-pick", WeightBytes: 15 << 30,
				Meta: `{"model_type":"qwen3","num_hidden_layers":1,"hidden_size":256,"num_attention_heads":8,"num_key_value_heads":8,"max_position_embeddings":2048}`,
			})
			if err != nil {
				t.Fatal(err)
			}
			// The shared node's free card is held outside the platform; its
			// other card runs a whole-card deployment.
			shared := foreignHeldH20(t, st, "shared", proto.PartitionSafetyForeignTracked)
			if _, _, _, err := st.SetNodePlacementPolicy(shared.UUID, func(p store.NodePlacementPolicy) (store.NodePlacementPolicy, error) {
				p.ShareWithForeign = true
				return p, nil
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := st.UpsertModelFile(&store.ModelFile{ModelID: model.ID, NodeID: shared.ID, State: proto.MFPresent, Path: "/models/pool-pick"}); err != nil {
				t.Fatal(err)
			}
			other, err := createTestDeployment(t, st, &store.Deployment{Name: "whole-card", ModelID: model.ID, NodeID: shared.ID, ServedName: "whole-card", Engine: "vllm", State: "active", Replicas: 1})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := st.CreateInstance(&store.Instance{UUID: "whole-card-1", DeploymentID: other.ID, NodeID: shared.ID, State: proto.InstRunning, GPUIndexes: "[1]"}); err != nil {
				t.Fatal(err)
			}
			online := map[string]bool{shared.UUID: true}
			if tc.withClean {
				clean := foreignHeldH20(t, st, "clean", proto.PartitionSafetyForeignTracked)
				if err := st.DB().Model(&store.GPU{}).Where("node_id = ?", clean.ID).Update("foreign_processes", "").Error; err != nil {
					t.Fatal(err)
				}
				online[clean.UUID] = true
				if _, err := st.UpsertModelFile(&store.ModelFile{ModelID: model.ID, NodeID: clean.ID, State: proto.MFPresent, Path: "/models/pool-pick"}); err != nil {
					t.Fatal(err)
				}
			}
			dep, err := createTestDeploymentWithRequirements(t, st, &store.Deployment{
				Name: "pool-partition", ModelID: model.ID, NodeID: shared.ID, ServedName: "pool-partition", Engine: "vllm", State: "active",
				// Binpack ranks the node with the least room first: the shared one.
				Placement: `{"tp":1}`, QuickParams: `{"max_model_len":512}`, Replicas: 1, PlacementStrategy: store.PlacementBinpack,
				VRAMLimitBytes: 16 << 30, PartitionBackend: accelerator.BackendNVIDIALogical,
			}, nil)
			if err != nil {
				t.Fatal(err)
			}
			hub := &distributedTestHub{online: online, updates: map[string][]proto.StateUpdate{}}
			c := New(st, hub, nil)
			if err := c.reconcileDeployment(context.Background(), dep.ID); err != nil {
				t.Fatalf("reconcile: %v", err)
			}
			instances, err := st.InstancesForDeployment(dep.ID)
			if err != nil || len(instances) != 1 {
				last, _, _ := st.LatestScheduleEvent(dep.ID)
				raw, _ := json.Marshal(last.Data)
				t.Fatalf("instances = %+v (%v), last refusal %s", instances, err, raw)
			}
			node, err := st.NodeByID(instances[0].NodeID)
			if err != nil || node.UUID != tc.want {
				t.Fatalf("placed on %s GPUs %s (%v), want %s", node.UUID, instances[0].GPUIndexes, err, tc.want)
			}
		})
	}
}
