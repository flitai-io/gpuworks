package deployment

import (
	"fmt"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
)

func nodeStatusTwoGPUs(hostname string, index int) proto.NodeStatus {
	gpus := func() []proto.GPUInfo {
		out := []proto.GPUInfo{}
		for index := 0; index < 2; index++ {
			gpu := proto.GPUInfo{Index: index, Name: "NVIDIA H100", Vendor: "nvidia", ArchID: "sm_90", ComputeCapability: "9.0", MemoryTotalBytes: 80 << 30, Healthy: true}
			out = append(out, gpu)
		}
		return out
	}()
	return proto.NodeStatus{
		Sys: proto.SysInfo{Hostname: hostname, Arch: "amd64", GPUVendor: "nvidia", Framework: "cuda", FrameworkVersion: "13.0"}, Network: testManagement(fmt.Sprintf("10.0.0.%d", index+1)),
		GPUs: gpus,
	}
}

// Before any model row exists, no source reports registered - the projection
// reads the store instead of assuming the template resolves.
func TestEvaluateWeightSourceRegistrationProjection(t *testing.T) {
	h, _ := eligibleNodeFixture(t)
	st := h.d.Store
	models, err := st.ListModels()
	if err != nil {
		t.Fatal(err)
	}
	for _, model := range models {
		if model.SourceID == "Acme/Deploy-FP8" {
			if err := st.DeleteModel(model.ID); err != nil {
				t.Fatal(err)
			}
		}
	}
	spec := deploymentFlatFixture().Specs[0]
	selection := templateSelection{Spec: &spec}
	options := h.evaluateWeightSourceOptions(selection)
	if len(options) == 0 {
		t.Fatalf("options = %+v", options)
	}
	for _, option := range options {
		if option.Registered {
			t.Fatalf("no model row exists, so nothing reports registered: %+v", option)
		}
	}
}
