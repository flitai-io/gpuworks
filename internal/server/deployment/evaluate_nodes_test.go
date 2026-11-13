package deployment

import (
	"testing"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// eligibleNodeFixture seeds the five-node world the eligibility cases need:
// a plain eligible node, an offline one, a vendor-mismatched one, a chip too
// old for the template predicate, and one whose labels disagree.
func eligibleNodeFixture(t *testing.T) (*Service, uint) {
	t.Helper()
	st, err := store.Open("file:eligible_nodes_test?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	online := func(uuid string, gpu proto.GPUInfo) {
		t.Helper()
		if _, err := st.RegisterNode(uuid, store.HashToken("token"), uuid); err != nil {
			t.Fatal(err)
		}
		if err := st.UpdateNodeStatus(uuid, proto.NodeStatus{
			Sys:  proto.SysInfo{Hostname: uuid, Arch: "amd64", GPUVendor: gpu.Vendor, Framework: "cuda", FrameworkVersion: "12.4"},
			GPUs: []proto.GPUInfo{gpu},
		}); err != nil {
			t.Fatal(err)
		}
	}
	online("eligible-node", proto.GPUInfo{Index: 0, Name: "NVIDIA H100", Vendor: "nvidia", ArchID: "sm_90", ComputeCapability: "9.0", MemoryTotalBytes: 80 << 30, Healthy: true})
	online("chip-old-node", proto.GPUInfo{Index: 0, Name: "NVIDIA A100", Vendor: "nvidia", ArchID: "sm_80", ComputeCapability: "8.0", MemoryTotalBytes: 40 << 30, Healthy: true})
	online("vendor-node", proto.GPUInfo{Index: 0, Name: "Ascend 910B", Vendor: "ascend", ArchID: "910B", MemoryTotalBytes: 64 << 30, Healthy: true})
	if _, err := st.RegisterNode("offline-node", store.HashToken("token"), "offline-node"); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateNodeStatus("offline-node", proto.NodeStatus{
		Sys:  proto.SysInfo{Hostname: "offline-node", Arch: "amd64", GPUVendor: "nvidia"},
		GPUs: []proto.GPUInfo{{Index: 0, Name: "NVIDIA H100", Vendor: "nvidia", ArchID: "sm_90", ComputeCapability: "9.0", MemoryTotalBytes: 80 << 30, Healthy: true}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetNodeState("offline-node", store.NodeStateOffline); err != nil {
		t.Fatal(err)
	}
	online("unlabeled-node", proto.GPUInfo{Index: 0, Name: "NVIDIA H100", Vendor: "nvidia", ArchID: "sm_90", ComputeCapability: "9.0", MemoryTotalBytes: 80 << 30, Healthy: true})
	for _, uuid := range []string{"eligible-node", "vendor-node", "chip-old-node", "offline-node"} {
		if _, err := st.SetNodeLabels(uuid, map[string]string{"pool": "inference"}); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := st.UpsertModel(&store.Model{Source: "modelscope", SourceID: "Acme/Deploy-FP8", Revision: "main", Name: "Acme/Deploy-FP8", WeightBytes: 16 << 30}); err != nil {
		t.Fatal(err)
	}
	fixture := deploymentFlatFixture()
	fixture.ModelID = "acme/eligible"
	if err := st.ReconcileInferenceCatalog([]store.InferenceModel{fixture}); err != nil {
		t.Fatal(err)
	}
	template, err := st.InferenceModelByModelID(fixture.ModelID)
	if err != nil {
		t.Fatal(err)
	}
	deploy.SetDefaultImage("vllm", "example/vllm:test")
	return New(Deps{Store: st}), template.ID
}

// The five cases the plan names: requirement mismatch, vendor mismatch,
// chipmatch mismatch, offline, and all-eligible. One draft answers for every node at
// once, which is exactly what the dropdown renders.
func TestEvaluateEligibleNodesFilterAMDFeatureRequirements(t *testing.T) {
	h, _ := eligibleNodeFixture(t)
	if _, err := h.d.Store.RegisterNode("amd-feature-node", store.HashToken("amd-feature-token"), "amd-feature-node"); err != nil {
		t.Fatal(err)
	}
	if err := h.d.Store.UpdateNodeStatus("amd-feature-node", proto.NodeStatus{
		Sys:  proto.SysInfo{Hostname: "amd-feature-node", OS: "linux", Arch: "amd64", GPUVendor: "amd", Framework: "rocm", RuntimeDriver: "docker"},
		GPUs: []proto.GPUInfo{{Index: 0, Name: "AMD Instinct MI300X", Vendor: "amd", ArchID: "gfx942", MemoryTotalBytes: 96 << 30, Healthy: true}},
	}); err != nil {
		t.Fatal(err)
	}
	features := []accelerator.Feature{accelerator.FeatureMultiNode, accelerator.FeaturePD, accelerator.FeatureDataParallel}
	nodes, _ := h.evaluateEligibleNodes(templateSelection{}, nil, nil, true, func(node *store.Node) map[int]planning.TargetCardRequest {
		return eligibleRequestsForTest(node, planning.TargetRequest{Features: features, CommandKnown: true})
	})
	byUUID := make(map[string]struct {
		eligible bool
		reasons  map[string]bool
	}, len(nodes))
	for _, node := range nodes {
		reasons := make(map[string]bool)
		for _, reason := range node.Reasons {
			reasons[reason.Code] = true
		}
		byUUID[node.UUID] = struct {
			eligible bool
			reasons  map[string]bool
		}{node.Eligible, reasons}
	}
	amd := byUUID["amd-feature-node"]
	// AMD runs multi-node ranks, PD and data parallelism.
	if !amd.eligible || len(amd.reasons) != 0 {
		t.Fatalf("AMD feature requirements were not explained: %+v", amd)
	}
	for _, uuid := range []string{"eligible-node", "vendor-node"} {
		if node := byUUID[uuid]; !node.eligible {
			t.Fatalf("supported vendor %s was filtered by AMD feature requirements: %+v", uuid, node)
		}
	}
}

func TestEvaluateEligibleNodesSkipsAMDFeatureWorkWithoutOwnedAMD(t *testing.T) {
	h, _ := eligibleNodeFixture(t)
	called := false
	h.evaluateEligibleNodes(templateSelection{}, nil, nil, true, func(node *store.Node) map[int]planning.TargetCardRequest {
		called = true
		return eligibleRequestsForTest(node, planning.TargetRequest{Features: []accelerator.Feature{accelerator.FeatureMultiNode}, CommandKnown: true})
	})
	if !called {
		t.Fatal("resolved launch features were not evaluated for owned cards")
	}
}

func eligibleRequestsForTest(node *store.Node, request planning.TargetRequest) map[int]planning.TargetCardRequest {
	out := map[int]planning.TargetCardRequest{}
	for _, gpu := range node.OwnedGPUs() {
		out[gpu.Index] = planning.TargetCardRequest{Request: request}
	}
	return out
}
