package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func installDPTemplate(t *testing.T, r *autoRig, degree int) uint {
	t.Helper()
	model := store.InferenceModel{ModelID: "org/auto", DisplayTitle: "DP recipe", Source: "upstream",
		Specs: []store.InferenceSpecRow{{Name: "dp-recipe", Engine: "vllm", Scenario: "balanced",
			Image: "vllm/vllm-openai:v0.25.1", Quantization: "bf16", WeightBytes: 1 << 30,
			WeightSources: `[{"provider":"huggingface","repo_id":"org/auto","revision":"main"}]`,
			GPUs:          `{"vendor":"nvidia"}`, Args: fmt.Sprintf(`["--data-parallel-size","%d"]`, degree)}}}
	if err := r.st.ReconcileInferenceCatalog([]store.InferenceModel{model}); err != nil {
		t.Fatal(err)
	}
	stored, err := r.st.InferenceModelByModelID(model.ModelID)
	if err != nil {
		t.Fatal(err)
	}
	return stored.ID
}

func TestAutoMultiNodeCountsTemplateDataParallelBeforePickingNodes(t *testing.T) {
	for _, named := range []bool{true, false} {
		t.Run(fmt.Sprintf("named=%v", named), func(t *testing.T) {
			r := newAutoRig(t, 4)
			templateID := installDPTemplate(t, r, 2)
			r.occupy(t, "au-3", 0)
			extra := fmt.Sprintf(`,"template_id":%d`, templateID)
			if named {
				extra += `,"template_spec_name":"dp-recipe"`
			}
			body := r.multiBody("template-dp", extra)
			out := r.mustCreate(t, body)
			if slices.Contains(out.NodeUUIDs, "au-3") {
				t.Fatalf("template DP2 needs two free cards on each node, selected busy node: %v", out.NodeUUIDs)
			}
			if !slices.Equal(out.NodeUUIDs, []string{"au-1", "au-2"}) {
				t.Fatalf("selected %v, want the first two fully free nodes", out.NodeUUIDs)
			}
		})
	}
}

func TestAutomaticSingleNodeUsesEffectiveTemplateDP(t *testing.T) {
	for _, override := range []string{`"quick_params":{"data_parallel_size":1}`, `"quick_params_unset":["data_parallel_size"]`} {
		t.Run(override, func(t *testing.T) {
			r := newAutoRig(t, 1)
			templateID := installDPTemplate(t, r, 4)
			body := fmt.Sprintf(`{"model_id":%d,"engine":"vllm","template_id":%d,"template_spec_name":"dp-recipe","placement_mode":"auto",%s}`, r.modelID, templateID, override)
			rec := r.do(r.session, http.MethodPost, "/api/v1/evaluate", body)
			var preview dto.EvaluateResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &preview); err != nil || rec.Code != http.StatusOK || !preview.OK {
				t.Fatalf("evaluate=%d %s, %v", rec.Code, rec.Body.String(), err)
			}
		})
	}
}

func TestAutoMultiNodeTemplateDPMaySupplyTheNodeRanks(t *testing.T) {
	r := newAutoRig(t, 2)
	templateID := installDPTemplate(t, r, 2)
	// Each node runs one independent TP1 replica. Before resolving the
	// template, TP1 alone cannot be divided over two nodes.
	body := fmt.Sprintf(`{"name":"dp-ranks","model_id":%d,"engine":"vllm","template_id":%d,"nnodes":2,"placement_mode":"auto","placement":{"tp":1,"pp":1}}`, r.modelID, templateID)
	out := r.mustCreate(t, body)
	if !slices.Equal(out.NodeUUIDs, []string{"au-1", "au-2"}) {
		t.Fatalf("TP1 DP2 selected nodes=%v", out.NodeUUIDs)
	}
}

func TestMultiNodeEditDefaultsTPFromRetainedDP(t *testing.T) {
	r := newAutoRig(t, 2)
	body := fmt.Sprintf(`{"name":"retained-dp","model_id":%d,"engine":"vllm","image":"vllm/vllm-openai:v0.25.1","nnodes":2,"node_uuids":["au-1","au-2"],"placement":{"tp":2,"pp":1},"quick_params":{"data_parallel_size":2}}`, r.modelID)
	out := r.mustCreate(t, body)
	rec := r.do(r.session, http.MethodGet, fmt.Sprintf("/api/v1/deployments/%d/draft", out.ID), "")
	var draft dto.DeploymentDraft
	if err := json.Unmarshal(rec.Body.Bytes(), &draft); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("draft=%d %s, %v", rec.Code, rec.Body.String(), err)
	}
	draft.Deployment.QuickParams = nil
	draft.Deployment.Placement = map[string]any{"pp": 1}
	edit := jsonOf(t, dto.DeploymentEditRequest{Deployment: draft.Deployment, BaseRevision: draft.Revision})
	rec = r.do(r.session, http.MethodPut, fmt.Sprintf("/api/v1/deployments/%d?dry_run=1", out.ID), edit)
	if rec.Code != http.StatusOK {
		t.Fatalf("edit=%d %s", rec.Code, rec.Body.String())
	}
	plan := decodePlan(t, rec.Body.String())
	if len(plan.Changes) != 0 {
		t.Fatalf("omitted DP must retain TP2/DP2, changes=%+v", plan.Changes)
	}
}

func TestMultiNodeCreateUsesTheEvaluatedDataParallelOverride(t *testing.T) {
	r := newAutoRig(t, 2)
	templateID := installDPTemplate(t, r, 4)
	body := fmt.Sprintf(`{"name":"dp-override","model_id":%d,"engine":"vllm","template_id":%d,"template_spec_name":"dp-recipe","nnodes":2,"node_uuids":["au-1","au-2"],"placement":{"tp":2,"pp":1},"quick_params":{"data_parallel_size":1}}`, r.modelID, templateID)
	rec := r.do(r.session, http.MethodPost, "/api/v1/evaluate", body)
	var preview dto.EvaluateResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &preview); err != nil || rec.Code != http.StatusOK || !preview.OK {
		t.Fatalf("evaluate=%d %s, %v", rec.Code, rec.Body.String(), err)
	}
	out := r.mustCreate(t, body)
	dep := r.storedDeployment(t, out.ID)
	var quick map[string]any
	if err := json.Unmarshal([]byte(dep.QuickParams), &quick); err != nil || quick["data_parallel_size"] != float64(1) {
		t.Fatalf("stored quick=%s, %v", dep.QuickParams, err)
	}
}
