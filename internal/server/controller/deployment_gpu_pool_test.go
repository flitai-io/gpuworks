package controller

import (
	"context"
	"encoding/json"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestManualGPUPoolWithTP1AllocatesOnlyTheFreeCard(t *testing.T) {
	st, err := store.Open("file:controller_manual_gpu_placement?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	node, err := st.RegisterNode("manual-gpu-node", store.HashToken("token"), "manual-gpu-node")
	if err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 2; index++ {
		if err := st.DB().Create(&store.GPU{
			NodeID: node.ID, Index: index, UUID: "manual-gpu-" + string(rune('0'+index)), Name: "NVIDIA H100",
			Vendor: "nvidia", ArchID: "sm_90", ComputeCapability: "9.0",
			MemoryTotal: 80 << 30,
		}).Error; err != nil {
			t.Fatal(err)
		}
	}
	model, err := st.UpsertModel(&store.Model{
		Source: "huggingface", SourceID: "fixture/manual-gpu", Revision: "main", Name: "manual-gpu",
		WeightBytes: 1 << 30, Meta: `{"model_type":"qwen3","num_hidden_layers":1,"hidden_size":256,"num_attention_heads":8,"num_key_value_heads":8,"max_position_embeddings":2048}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertModelFile(&store.ModelFile{ModelID: model.ID, NodeID: node.ID, State: proto.MFPresent, Path: "/models/manual-gpu"}); err != nil {
		t.Fatal(err)
	}
	holder, err := st.CreateDeployment(&store.Deployment{Name: "holder-a", ServedName: "holder-a", ModelID: model.ID, NodeID: node.ID, State: store.DeployActive, Replicas: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateInstance(&store.Instance{UUID: "holder-a", DeploymentID: holder.ID, NodeID: node.ID, State: proto.InstRunning, GPUIndexes: "[0]"}); err != nil {
		t.Fatal(err)
	}
	dep, err := createTestDeployment(t, st, &store.Deployment{
		Name: "manual-gpu", ModelID: model.ID, NodeID: node.ID, ServedName: "manual-gpu", Engine: "vllm",
		Placement: `{"tp":1,"gpus":[0,1]}`, QuickParams: `{"max_model_len":512}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	hub := &distributedTestHub{online: map[string]bool{node.UUID: true}, updates: map[string][]proto.StateUpdate{}}
	c := New(st, hub, nil)
	if err := c.reconcileDeployment(context.Background(), dep.ID); err != nil {
		t.Fatal(err)
	}
	instances, err := st.InstancesForDeployment(dep.ID)
	if err != nil || len(instances) != 1 {
		t.Fatalf("instances=%+v err=%v", instances, err)
	}
	var indexes []int
	if err := json.Unmarshal([]byte(instances[0].GPUIndexes), &indexes); err != nil || len(indexes) != 1 || indexes[0] != 1 {
		t.Fatalf("persisted GPU indexes=%v raw=%q err=%v", indexes, instances[0].GPUIndexes, err)
	}
	t.Logf("stored deployment placement=%s instance GPUs=%s", dep.Placement, instances[0].GPUIndexes)
	updates := hub.updates[node.UUID]
	if len(updates) > 0 && len(updates[0].Instances) > 0 {
		t.Logf("pushed GPU indexes=%v", updates[0].Instances[0].GPUIndexes)
	}
	if len(updates) != 1 || len(updates[0].Instances) != 1 || len(updates[0].Instances[0].GPUIndexes) != 1 || updates[0].Instances[0].GPUIndexes[0] != 1 {
		t.Fatalf("pushed state update=%+v", updates)
	}
}
