package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestDistributedDefaultReportsIndivisiblePPAndDP(t *testing.T) {
	evaluate, modelID := newParallelEvaluate(t)
	out := evaluate(fmt.Sprintf(`{"model_id":%d,"engine":"vllm","nnodes":2,"node_uuids":["par-a","par-b"],"placement":{"pp":4},"quick_params":{"data_parallel_size":2}}`, modelID))
	if out.OK || len(out.Errors) != 1 || out.Errors[0].Field != "placement.pp" || out.Errors[0].Code != ErrorDistributedParallelIndivisible {
		t.Fatalf("unstated TP with PP4 DP2 = ok %v errors %+v", out.OK, out.Errors)
	}
}

func TestEvaluateReportsSymbolicTemplateDPOnItsField(t *testing.T) {
	r := newAutoRig(t, 1)
	model := store.InferenceModel{ModelID: "org/auto", Source: "upstream", Specs: []store.InferenceSpecRow{{
		Name: "symbolic-dp", Engine: "vllm", Scenario: "balanced", Image: "vllm/vllm-openai:v0.25.1", Quantization: "bf16", WeightBytes: 1 << 30,
		WeightSources: `[{"provider":"huggingface","repo_id":"org/auto","revision":"main"}]`,
		GPUs:          `{"vendor":"nvidia"}`, Args: `["--data-parallel-size","${X}"]`,
	}}}
	if err := r.st.ReconcileInferenceCatalog([]store.InferenceModel{model}); err != nil {
		t.Fatal(err)
	}
	stored, err := r.st.InferenceModelByModelID(model.ModelID)
	if err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`{"model_id":%d,"engine":"vllm","template_id":%d,"template_spec_name":"symbolic-dp","placement_mode":"auto"}`, r.modelID, stored.ID)
	rec := r.do(r.session, http.MethodPost, "/api/v1/evaluate", body)
	var preview dto.EvaluateResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &preview); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK || preview.OK || len(preview.Errors) != 1 {
		t.Fatalf("evaluate = %d ok %v errors %+v", rec.Code, preview.OK, preview.Errors)
	}
	got := preview.Errors[0]
	if got.Field != "data_parallel_size" || got.Code != ErrorFieldInvalid || !strings.Contains(fmt.Sprint(got.Params["detail"]), "must be concrete") {
		t.Fatalf("symbolic template DP error = %+v", got)
	}
}

func TestAutomaticDPRanksNodesBelowMinimumAfterCapableNodes(t *testing.T) {
	r := newAutoRig(t, 9)
	status := nodeStatusTwoGPUs("au-9", 9)
	for index := 2; index < 4; index++ {
		status.GPUs = append(status.GPUs, proto.GPUInfo{Index: index, Name: "NVIDIA H100", Vendor: "nvidia", ArchID: "sm_90", ComputeCapability: "9.0", MemoryTotalBytes: 80 << 30, Healthy: true})
	}
	if err := r.st.UpdateNodeStatus("au-9", status); err != nil {
		t.Fatal(err)
	}
	templateID := installDPTemplate(t, r, 4)
	body := fmt.Sprintf(`{"model_id":%d,"engine":"vllm","template_id":%d,"template_spec_name":"dp-recipe","placement_mode":"auto"}`, r.modelID, templateID)
	rec := r.do(r.session, http.MethodPost, "/api/v1/evaluate", body)
	var preview dto.EvaluateResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &preview); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK || !preview.OK || preview.SelectedNode == nil || preview.SelectedNode.UUID != "au-9" {
		t.Fatalf("DP4 automatic preview = %d ok %v selected %+v errors %+v", rec.Code, preview.OK, preview.SelectedNode, preview.Errors)
	}
	if len(preview.Candidates) != 1 {
		t.Fatalf("DP4 planned %d candidates, want only the four-card node", len(preview.Candidates))
	}
}

func TestAutomaticMultiNodeTrialClearsEarlierTemplateError(t *testing.T) {
	r := newAutoRig(t, 3)
	status := nodeStatusTwoGPUs("au-1", 1)
	for index := range status.GPUs {
		status.GPUs[index].Name = "NVIDIA A100"
	}
	if err := r.st.UpdateNodeStatus("au-1", status); err != nil {
		t.Fatal(err)
	}
	r.occupy(t, "au-2", 0)
	r.occupy(t, "au-3", 0)
	model := store.InferenceModel{ModelID: "org/auto", Source: "upstream", Specs: []store.InferenceSpecRow{
		{Name: "symbolic", Engine: "vllm", Scenario: "balanced", Image: "vllm/vllm-openai:v0.25.1", Quantization: "bf16", WeightBytes: 1 << 30,
			WeightSources: `[{"provider":"huggingface","repo_id":"org/auto","revision":"main"}]`, GPUs: `{"vendor":"nvidia","name":"A100"}`, Args: `["--data-parallel-size","${X}"]`},
		{Name: "concrete", Engine: "vllm", Scenario: "balanced", Image: "vllm/vllm-openai:v0.25.1", Quantization: "bf16", WeightBytes: 1 << 30,
			WeightSources: `[{"provider":"huggingface","repo_id":"org/auto","revision":"main"}]`, GPUs: `{"vendor":"nvidia","name":"H100"}`, Args: `["--data-parallel-size","2"]`},
	}}
	if err := r.st.ReconcileInferenceCatalog([]store.InferenceModel{model}); err != nil {
		t.Fatal(err)
	}
	stored, err := r.st.InferenceModelByModelID(model.ModelID)
	if err != nil {
		t.Fatal(err)
	}
	extra := fmt.Sprintf(`,"template_id":%d,"placement_strategy":"spread"`, stored.ID)
	rec := r.do(r.session, http.MethodPost, "/api/v1/deployments", r.multiBody("trial-error", extra))
	code, _ := errorBody(t, rec.Body.String())
	if rec.Code != http.StatusConflict || code != ErrorUnitPlacementNone {
		t.Fatalf("later capacity failure = %d %s, want 409 %s", rec.Code, rec.Body.String(), ErrorUnitPlacementNone)
	}
}
