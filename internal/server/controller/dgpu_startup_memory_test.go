package controller

import (
	"strconv"
	"testing"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestReconcileLaunchesStoredAscendDGPUWithStartupMemoryWarning(t *testing.T) {
	st, err := store.Open("file:controller_dgpu_startup_memory?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	const backend = accelerator.BackendAscendLogical
	const image = "gpuworks/gpuworks-runner:cann9.0-910b-sglang0.5.18-r2"
	node, err := st.RegisterNode("ascend", store.HashToken("test-token"), "ascend")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateNodeStatus(node.UUID, proto.NodeStatus{
		Sys: proto.SysInfo{
			Hostname: "ascend", Arch: "arm64", GPUVendor: "ascend", Framework: "cann",
			FrameworkVersion: "9.0.0", RuntimeDriver: "udocker", PartitionSafetyProtocol: 1,
			PartitionAssets: []proto.AssetReport{{Backend: backend, Root: "/assets", Files: []string{"libvruntime.so"}}},
			PartitionProbes: []proto.PartitionProbeResult{{Backend: backend, GPUIndex: new(int), Status: proto.PartitionProbePassed}},
		},
		GPUs: []proto.GPUInfo{{Index: 0, Name: "Ascend 910B4", Vendor: "ascend", ArchID: "910B4", MemoryTotalBytes: 32 << 30, Healthy: true}},
	}); err != nil {
		t.Fatal(err)
	}
	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "Qwen/Qwen3-0.6B", Name: "qwen3", Arch: "qwen3", WeightBytes: 1519209873})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertModelFile(&store.ModelFile{ModelID: model.ID, NodeID: node.ID, State: proto.MFPresent, Path: "/models/qwen3"}); err != nil {
		t.Fatal(err)
	}
	dep, err := createTestDeployment(t, st, &store.Deployment{
		Name: "stored-qwen3", ServedName: "stored-qwen3", ModelID: model.ID, NodeID: node.ID, Engine: "sglang", State: "active",
		Image: image, VRAMLimitBytes: 8 << 30, PartitionBackend: backend, Placement: `{"tp":1,"gpus":[0]}`,
		QuickParams:              `{"gpu_memory_utilization":0.90}`,
		CatalogSelectionSnapshot: `{"model_id":"qwen/qwen3-0.6b","quantization":"bf16","extra_args":["--dtype","bfloat16"]}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	hub := &distributedTestHub{online: map[string]bool{node.UUID: true}, updates: map[string][]proto.StateUpdate{}}
	if err := New(st, hub, nil).reconcileDeployment(t.Context(), dep.ID); err != nil {
		t.Fatal(err)
	}
	instances, err := st.InstancesForDeployment(dep.ID)
	if err != nil || len(instances) != 1 {
		t.Fatalf("instances = %v, err = %v, want scheduled deployment", instances, err)
	}
	for _, update := range hub.updates[node.UUID] {
		for _, desired := range update.Instances {
			if desired.UUID != instances[0].UUID || desired.DesiredState != "running" {
				continue
			}
			args, _, _ := deploy.LaunchArgs(desired.ResolvedLaunch)
			raw, _ := deploy.LaunchFlag(args, "--mem-fraction-static")
			fraction, err := strconv.ParseFloat(raw, 64)
			if err != nil || fraction != 0.90 || desired.VRAMLimitBytes != 8<<30 {
				t.Fatalf("scheduled launch has mem-fraction-static %q and quota %d", raw, desired.VRAMLimitBytes)
			}
			return
		}
	}
	t.Fatal("agent did not receive a running instance")
}
