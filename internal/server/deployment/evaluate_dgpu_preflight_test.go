package deployment

import (
	"context"
	"fmt"
	"testing"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// dgpuPreflightRig is one dGPU-ready NVIDIA node with cards cards and a
// template of engine, image and args, and the request a draft sends for it.
func dgpuPreflightRig(t *testing.T, name string, cards int, engine, image, args string) (*Service, dto.DeployRequest) {
	t.Helper()
	st, err := store.Open("file:dgpu_preflight_"+name+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, err := st.RegisterNode("dgpu-node", store.HashToken("dgpu-node"), "dgpu-node"); err != nil {
		t.Fatal(err)
	}
	status := proto.NodeStatus{Sys: proto.SysInfo{Hostname: "dgpu-node", Arch: "amd64", GPUVendor: "nvidia", Framework: "cuda", FrameworkVersion: "12.4", RuntimeDriver: "docker",
		PartitionAssets: []proto.AssetReport{{Backend: accelerator.BackendNVIDIALogical, Root: "/assets", Files: []string{"libvgpu.so"}}}, PartitionSafetyProtocol: 1}}
	for index := 0; index < cards; index++ {
		status.Sys.PartitionProbes = append(status.Sys.PartitionProbes, proto.PartitionProbeResult{Backend: accelerator.BackendNVIDIALogical, GPUIndex: &index, Status: proto.PartitionProbePassed})
		status.GPUs = append(status.GPUs, proto.GPUInfo{Index: index, UUID: fmt.Sprintf("GPU-dgpu-%d", index), Name: "NVIDIA H100", Vendor: "nvidia", ArchID: "sm_90", ComputeCapability: "9.0", MemoryTotalBytes: 80 << 30, Healthy: true})
	}
	if err := st.UpdateNodeStatus("dgpu-node", status); err != nil {
		t.Fatal(err)
	}
	catalog := deploymentFlatFixture()
	catalog.Specs[0].Engine, catalog.Specs[0].Image, catalog.Specs[0].Args, catalog.Specs[0].PreStartArgv = engine, image, args, ""
	if err := st.ReconcileInferenceCatalog([]store.InferenceModel{catalog}); err != nil {
		t.Fatal(err)
	}
	template, err := st.InferenceModelByModelID(catalog.ModelID)
	if err != nil {
		t.Fatal(err)
	}
	model, err := st.UpsertModel(&store.Model{Source: "modelscope", SourceID: "Acme/Deploy-FP8", Name: "deploy", WeightBytes: 4 << 30})
	if err != nil {
		t.Fatal(err)
	}
	return New(Deps{Store: st, Online: func(string) bool { return true }}), dto.DeployRequest{Name: "dgpu", ModelID: model.ID, TemplateID: template.ID,
		TemplateSpecSource: "upstream", TemplateSpecName: "fp8-balanced", NodeUUID: "dgpu-node", Placement: map[string]any{}, QuickParams: map[string]any{}, VRAMLimitGiB: 20}
}

// A dGPU memory limit lets the scheduler place a draft on one shared card
// whatever parallelism it states, and the preview's plan resolves on that
// card; the create resolves on the cards the request states. Evaluate runs the
// create's launch preflight too, so placement tp=2 against a template pinning
// --tensor-parallel-size 1 is the error the create refuses with, word for
// word. What the create accepts stays accepted: an engine whose launch states
// no topology rule (callable), and a draft that leaves its parallelism to the
// scheduler.
func TestEvaluateRunsTheCreateLaunchPreflight(t *testing.T) {
	ctx := context.Background()
	t.Run("stated tp conflicts with the template", func(t *testing.T) {
		s, req := dgpuPreflightRig(t, "conflict", 1, "vllm", "vllm/vllm-openai:latest", `["--max-model-len","32768","--tensor-parallel-size","1"]`)
		req.Placement["tp"] = 2
		_, createErr := s.Create(ctx, req)
		if createErr == nil || KindOf(createErr) != KindInvalid {
			t.Fatalf("create = %v; want an invalid request", createErr)
		}
		resp, err := s.Evaluate(ctx, dto.EvaluateRequest{DeployRequest: req})
		if err != nil || resp.OK || len(resp.Errors) != 1 || resp.Errors[0].Params["detail"] != createErr.Error() {
			t.Fatalf("evaluate ok=%t errors=%+v err=%v; want the create's refusal %q", resp.OK, resp.Errors, err, createErr.Error())
		}
	})
	for _, tc := range []struct {
		name, engine, image, args string
		tp                        int
	}{
		{"callable with a stated tp", "callable", "registry.example/adapter:v1", `["--port","${API_PORT}","--tp","${TP_SIZE}"]`, 2},
		{"unstated parallelism", "vllm", "vllm/vllm-openai:latest", `["--max-model-len","32768"]`, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, req := dgpuPreflightRig(t, tc.engine+fmt.Sprint(tc.tp), 2, tc.engine, tc.image, tc.args)
			if tc.tp > 0 {
				req.Placement["tp"] = tc.tp
			}
			resp, err := s.Evaluate(ctx, dto.EvaluateRequest{DeployRequest: req})
			if err != nil || !resp.OK || len(resp.Errors) != 0 {
				t.Fatalf("evaluate ok=%t errors=%+v err=%v; want ok", resp.OK, resp.Errors, err)
			}
			if _, err := s.Create(ctx, req); err != nil {
				t.Fatalf("create = %v; want it created", err)
			}
		})
	}
}
