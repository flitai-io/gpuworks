package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/nodelabel"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// The automatic-placement world: a small node the model does not fit, a big
// one it does, and a stale-labelled one for the create conflict case.
func automaticPlacementFixture(t *testing.T) (*apiHandlers, *store.Store) {
	t.Helper()
	st, err := store.Open("file:auto_placement_test?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	register := func(uuid string, gpu proto.GPUInfo) {
		t.Helper()
		if _, err := st.RegisterNode(uuid, store.HashToken("token"), uuid); err != nil {
			t.Fatal(err)
		}
		if err := st.UpdateNodeStatus(uuid, proto.NodeStatus{
			Sys:  proto.SysInfo{Hostname: uuid, Arch: "amd64", GPUVendor: "nvidia", Framework: "cuda", FrameworkVersion: "12.4"},
			GPUs: []proto.GPUInfo{gpu},
		}); err != nil {
			t.Fatal(err)
		}
	}
	register("auto-small", proto.GPUInfo{Index: 0, Name: "NVIDIA A100", Vendor: "nvidia", ArchID: "sm_80", ComputeCapability: "8.0", MemoryTotalBytes: 16 << 30, Healthy: true})
	register("auto-big", proto.GPUInfo{Index: 0, Name: "NVIDIA H100", Vendor: "nvidia", ArchID: "sm_90", ComputeCapability: "9.0", MemoryTotalBytes: 80 << 30, Healthy: true})
	register("auto-labelled", proto.GPUInfo{Index: 0, Name: "NVIDIA H100", Vendor: "nvidia", ArchID: "sm_90", ComputeCapability: "9.0", MemoryTotalBytes: 80 << 30, Healthy: true})
	if _, err := st.SetNodeLabels("auto-labelled", map[string]string{"pool": "inference"}); err != nil {
		t.Fatal(err)
	}
	deploy.SetDefaultImage("vllm", "example/vllm:test")
	return &apiHandlers{d: Deps{Store: st}}, st
}

func automaticPlacementModel(t *testing.T, st *store.Store) uint {
	t.Helper()
	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "org/auto-place", Revision: "main", Name: "auto-place", WeightBytes: 20 << 30})
	if err != nil {
		t.Fatal(err)
	}
	return model.ID
}

// A node the model cannot fit is answered as a candidate with its reason, and
// the selection lands on a node that fits - with the chosen plan's own reasons
// attached, so the form can say why.
func TestAutomaticPlacementSkipsTooSmallNodeAndExplainsItself(t *testing.T) {
	h, st := automaticPlacementFixture(t)
	modelID := automaticPlacementModel(t, st)
	rec := httptest.NewRecorder()
	h.evaluate(rec, httptest.NewRequest(http.MethodPost, "/api/v1/evaluate", strings.NewReader(fmt.Sprintf(`{"model_id":%d,"engine":"vllm","placement_mode":"auto","target_vendor":"nvidia"}`, modelID))))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	var response struct {
		OK           bool `json:"ok"`
		SelectedNode *struct {
			UUID    string `json:"uuid"`
			Reasons []struct {
				Code string `json:"code"`
			} `json:"reasons"`
		} `json:"selected_node"`
		Candidates []struct {
			NodeUUID string          `json:"node_uuid"`
			OK       bool            `json:"ok"`
			Reason   json.RawMessage `json:"reason"`
			Plan     *struct {
				Plan *struct {
					GPUs []int `json:"gpus"`
					TP   int   `json:"tp"`
				} `json:"plan"`
			} `json:"plan"`
		} `json:"candidates"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if !response.OK || response.SelectedNode == nil {
		t.Fatalf("auto placement must select: %s", rec.Body.String())
	}
	if response.SelectedNode.UUID != "auto-big" && response.SelectedNode.UUID != "auto-labelled" {
		t.Fatalf("selected %q, want one of the nodes the model fits", response.SelectedNode.UUID)
	}
	if len(response.SelectedNode.Reasons) == 0 || response.SelectedNode.Reasons[0].Code == "" {
		t.Fatalf("the selection carries the chosen plan's reasons: %+v", response.SelectedNode.Reasons)
	}
	byNode := map[string]struct {
		OK     bool
		Reason json.RawMessage
		Plan   *struct {
			Plan *struct {
				GPUs []int `json:"gpus"`
				TP   int   `json:"tp"`
			} `json:"plan"`
		}
	}{}
	for _, candidate := range response.Candidates {
		byNode[candidate.NodeUUID] = struct {
			OK     bool
			Reason json.RawMessage
			Plan   *struct {
				Plan *struct {
					GPUs []int `json:"gpus"`
					TP   int   `json:"tp"`
				} `json:"plan"`
			}
		}{OK: candidate.OK, Reason: candidate.Reason, Plan: candidate.Plan}
	}
	small := byNode["auto-small"]
	if small.OK {
		t.Fatalf("the 16 GiB node cannot fit a 20 GiB model: %+v", small)
	}
	if !strings.Contains(string(small.Reason), "no_fit") && !strings.Contains(string(small.Reason), "insufficient") && !strings.Contains(string(small.Reason), "required_gib") {
		t.Fatalf("the too-small candidate names its reason: %s", small.Reason)
	}
	big := byNode["auto-big"]
	if !big.OK || big.Plan == nil || big.Plan.Plan == nil || len(big.Plan.Plan.GPUs) == 0 || big.Plan.Plan.TP != 1 {
		t.Fatalf("the feasible candidate carries its plan: %+v", big)
	}
}

// A node the pull source rules leave nothing to pull the image from is turned
// down with the refusal a launch there records, not a planner error.
func TestAutomaticPlacementTurnsDownANodeWithNoAllowedPullSource(t *testing.T) {
	h, st := automaticPlacementFixture(t)
	modelID := automaticPlacementModel(t, st)
	if _, err := st.SetNodeLabels("auto-big", map[string]string{"pool": "air-gapped"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreatePullSourceRule(store.PullSourceRuleSpec{Match: "docker.io", Requirements: nodelabel.Requirements{"pool": {"air-gapped"}}}); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	h.evaluate(rec, httptest.NewRequest(http.MethodPost, "/api/v1/evaluate", strings.NewReader(fmt.Sprintf(`{"model_id":%d,"engine":"vllm","placement_mode":"auto","target_vendor":"nvidia"}`, modelID))))
	var response struct {
		SelectedNode *struct {
			UUID string `json:"uuid"`
		} `json:"selected_node"`
		Candidates []struct {
			NodeUUID string             `json:"node_uuid"`
			OK       bool               `json:"ok"`
			Reason   dto.ScheduleReason `json:"reason"`
		} `json:"candidates"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s err=%v", rec.Code, rec.Body.String(), err)
	}
	if response.SelectedNode == nil || response.SelectedNode.UUID != "auto-labelled" {
		t.Fatalf("the node that can pull is selected: %s", rec.Body.String())
	}
	for _, candidate := range response.Candidates {
		if candidate.NodeUUID != "auto-big" {
			continue
		}
		want := dto.ScheduleReason{Code: "image_source_forbidden", Params: map[string]any{"node": "auto-big", "image": "example/vllm:test"}}
		if candidate.OK || !reflect.DeepEqual(candidate.Reason, want) {
			t.Fatalf("auto-big reason = %+v, want %+v", candidate.Reason, want)
		}
		return
	}
	t.Fatalf("auto-big was not answered as a candidate: %s", rec.Body.String())
}

// The acquisition panel narrows to the node the draft lands on: after an
// automatic pick the image cache answer is the selected node's, never
// node_unselected beside a selected_node that already names the node.
func TestAutomaticPlacementAnswersImageCacheForTheSelectedNode(t *testing.T) {
	h, st := automaticPlacementFixture(t)
	modelID := automaticPlacementModel(t, st)
	h.d.Online = func(string) bool { return true }
	var asked []string
	h.d.ImageExists = func(_ context.Context, _, nodeUUID string) (bool, error) {
		asked = append(asked, nodeUUID)
		return true, nil
	}
	rec := httptest.NewRecorder()
	h.evaluate(rec, httptest.NewRequest(http.MethodPost, "/api/v1/evaluate", strings.NewReader(fmt.Sprintf(`{"model_id":%d,"engine":"vllm","placement_mode":"auto","target_vendor":"nvidia"}`, modelID))))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	var response struct {
		SelectedNode *struct {
			UUID string `json:"uuid"`
		} `json:"selected_node"`
		Acquisition *struct {
			Image struct {
				Image       string `json:"image"`
				Cached      *bool  `json:"cached"`
				CacheReason string `json:"cache_reason"`
			} `json:"image"`
			Nodes []struct {
				NodeUUID    string `json:"node_uuid"`
				ImageCached *bool  `json:"image_cached"`
				ImagePlan   string `json:"image_plan"`
			} `json:"nodes"`
		} `json:"acquisition"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.SelectedNode == nil {
		t.Fatalf("auto placement must select: %s", rec.Body.String())
	}
	if response.Acquisition == nil {
		t.Fatalf("the acquisition panel must ride along: %s", rec.Body.String())
	}
	if response.Acquisition.Image.CacheReason == "node_unselected" {
		t.Fatalf("a draft with a selected node must not claim no node was selected: %s", rec.Body.String())
	}
	if response.Acquisition.Image.Cached == nil || !*response.Acquisition.Image.Cached {
		t.Fatalf("the cache fact is the selected node's: %+v", response.Acquisition.Image)
	}
	// Only the selected node is ever asked; its forecast rides along because
	// the selection names it.
	if len(asked) == 0 {
		t.Fatal("the image cache was never asked")
	}
	for _, uuid := range asked {
		if uuid != response.SelectedNode.UUID {
			t.Fatalf("ImageExists asked %v, want only the selected node %q", asked, response.SelectedNode.UUID)
		}
	}
	nodes := response.Acquisition.Nodes
	if len(nodes) != 1 || nodes[0].NodeUUID != response.SelectedNode.UUID || nodes[0].ImagePlan != "present" || nodes[0].ImageCached == nil || !*nodes[0].ImageCached {
		t.Fatalf("forecast = %+v, want one entry for the selected node with the image present", nodes)
	}
}

// An automatic draft that no node can take names no node, so there is no
// forecast: where it lands is decided at launch.
func TestAutomaticPlacementWithoutASelectionForecastsNoNode(t *testing.T) {
	h, st := automaticPlacementFixture(t)
	huge, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "org/auto-huge", Revision: "main", Name: "auto-huge", WeightBytes: 400 << 30})
	if err != nil {
		t.Fatal(err)
	}
	h.d.Online = func(string) bool { return true }
	h.d.ImageExists = func(context.Context, string, string) (bool, error) {
		t.Fatal("no node is selected, so no node's image cache may be asked")
		return false, nil
	}
	rec := httptest.NewRecorder()
	h.evaluate(rec, httptest.NewRequest(http.MethodPost, "/api/v1/evaluate", strings.NewReader(fmt.Sprintf(`{"model_id":%d,"engine":"vllm","placement_mode":"auto","target_vendor":"nvidia"}`, huge.ID))))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	var wire map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &wire); err != nil {
		t.Fatal(err)
	}
	if _, selected := wire["selected_node"]; selected {
		t.Fatalf("a 400 GiB model was placed: %s", rec.Body.String())
	}
	if raw, ok := wire["acquisition"]; ok {
		if _, has := wireObject(t, raw)["nodes"]; has {
			t.Fatalf("an unplaced draft got a node forecast: %s", raw)
		}
	}
}

// A create repeating an automatic placement re-validates it: when the world
// moved (the node no longer satisfies the requirements), the answer is 409
// AUTOMATIC_PLACEMENT_STALE - never a silent re-placement.
func TestAutomaticPlacementCreateRejectsStaleSelection(t *testing.T) {
	h, st := automaticPlacementFixture(t)
	modelID := automaticPlacementModel(t, st)
	rec := httptest.NewRecorder()
	h.createDeployment(rec, httptest.NewRequest(http.MethodPost, "/api/v1/deployments", strings.NewReader(fmt.Sprintf(`{"name":"auto-stale","model_id":%d,"engine":"vllm","image":"example/vllm:test","placement_mode":"auto","node_uuid":"auto-labelled","node_requirements":{"pool":["gone"]},"quick_params":{}}`, modelID))))
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), ErrorAutomaticPlacementStale) {
		t.Fatalf("body must carry AUTOMATIC_PLACEMENT_STALE: %s", rec.Body.String())
	}
}

// A manual create with the same shape is an ordinary 400, not the stale
// conflict: the automatic contract only covers placement_mode=auto.
func TestManualCreateKeepsTheOrdinaryRequirementError(t *testing.T) {
	h, st := automaticPlacementFixture(t)
	modelID := automaticPlacementModel(t, st)
	rec := httptest.NewRecorder()
	h.createDeployment(rec, httptest.NewRequest(http.MethodPost, "/api/v1/deployments", strings.NewReader(fmt.Sprintf(`{"name":"manual-labels","model_id":%d,"engine":"vllm","image":"example/vllm:test","node_uuid":"auto-labelled","node_requirements":{"pool":["gone"]},"quick_params":{}}`, modelID))))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), ErrorNodeRequirementsUnmet) {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
}

// A create stores where the deployment may run as its requirements: the
// stated ones, the vendor, and for a manual placement the node it names. The
// keys an older form put in the placement are refused or dropped, never
// stored as if they meant something.
func TestCreateStoresNodeRequirements(t *testing.T) {
	h, st := automaticPlacementFixture(t)
	modelID := automaticPlacementModel(t, st)
	create := func(body string) (int, dto.Deployment, string) {
		t.Helper()
		rec := httptest.NewRecorder()
		h.createDeployment(rec, httptest.NewRequest(http.MethodPost, "/api/v1/deployments", strings.NewReader(fmt.Sprintf(body, modelID))))
		var got dto.Deployment
		if rec.Code == http.StatusCreated {
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
		}
		return rec.Code, got, rec.Body.String()
	}

	status, manual, body := create(`{"name":"manual","model_id":%d,"engine":"vllm","image":"example/vllm:test","placement_mode":"manual","node_uuid":"auto-labelled","target_vendor":"NVIDIA","placement_strategy":"binpack","node_requirements":{"pool":["inference"]},"placement":{"mode":"manual","node_uuid":"auto-labelled","tp":1},"quick_params":{}}`)
	if status != http.StatusCreated {
		t.Fatalf("manual create = %d %s", status, body)
	}
	want := map[string][]string{"gpuworks.ai/node": {"auto-labelled"}, "gpuworks.ai/vendor": {"nvidia"}, "pool": {"inference"}}
	if !reflect.DeepEqual(manual.NodeRequirements, want) || manual.PlacementStrategy != "binpack" {
		t.Fatalf("manual: requirements=%v strategy=%q, want %v binpack", manual.NodeRequirements, manual.PlacementStrategy, want)
	}
	stored, err := st.GetDeployment(manual.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Placement != `{"tp":1}` {
		t.Fatalf("stored placement = %s, want the dead keys dropped", stored.Placement)
	}

	// A create that leaves the vendor unstated gets the vendor of the node
	// it lands on, and no other requirement it did not state.
	status, plain, body := create(`{"name":"plain","served_name":"plain","model_id":%d,"engine":"vllm","image":"example/vllm:test","node_uuid":"auto-big","quick_params":{}}`)
	if status != http.StatusCreated {
		t.Fatalf("plain create = %d %s", status, body)
	}
	if want := map[string][]string{"gpuworks.ai/node": {"auto-big"}, "gpuworks.ai/vendor": {"nvidia"}}; !reflect.DeepEqual(plain.NodeRequirements, want) || plain.PlacementStrategy != "spread" {
		t.Fatalf("plain: requirements=%v strategy=%q", plain.NodeRequirements, plain.PlacementStrategy)
	}

	for name, c := range map[string]struct{ body, code string }{
		"a selector still in the placement":          {`{"name":"x","model_id":%d,"engine":"vllm","image":"example/vllm:test","node_uuid":"auto-labelled","placement":{"node_labels":{"pool":"inference"}},"quick_params":{}}`, "node_requirements"},
		"a misspelt system key":                      {`{"name":"x","model_id":%d,"engine":"vllm","image":"example/vllm:test","node_uuid":"auto-labelled","node_requirements":{"gpuworks.ai/gpu_model":["H100"]},"quick_params":{}}`, ErrorNodeRequirementsInvalid},
		"the named node outside a stated node list":  {`{"name":"x","model_id":%d,"engine":"vllm","image":"example/vllm:test","node_uuid":"auto-labelled","node_requirements":{"gpuworks.ai/node":["auto-big"]},"quick_params":{}}`, ErrorNodeRequirementsInvalid},
		"a vendor that disagrees with target_vendor": {`{"name":"x","model_id":%d,"engine":"vllm","image":"example/vllm:test","node_uuid":"auto-labelled","target_vendor":"nvidia","node_requirements":{"gpuworks.ai/vendor":["ascend"]},"quick_params":{}}`, ErrorNodeRequirementsInvalid},
		"an unknown strategy":                        {`{"name":"x","model_id":%d,"engine":"vllm","image":"example/vllm:test","node_uuid":"auto-labelled","placement_strategy":"random","quick_params":{}}`, ErrorPlacementStrategyInvalid},
	} {
		if status, _, body := create(c.body); status != http.StatusBadRequest || !strings.Contains(body, c.code) {
			t.Errorf("%s: %d %s, want 400 naming %s", name, status, body, c.code)
		}
	}
}

// The resource-change scenario: the evaluation selected a node whose GPU was
// free; another deployment takes that GPU before the create, and the create is
// rejected with the same conflict code instead of silently re-placing or
// storing a row the controller cannot schedule.
func TestAutomaticPlacementCreateRejectsWhenVRAMWasTaken(t *testing.T) {
	h, st := automaticPlacementFixture(t)
	big, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "org/auto-big", Revision: "main", Name: "auto-big", WeightBytes: 40 << 30})
	if err != nil {
		t.Fatal(err)
	}
	small, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "org/auto-small", Revision: "main", Name: "auto-small", WeightBytes: 20 << 30})
	if err != nil {
		t.Fatal(err)
	}
	// The evaluation: the big model fits while the GPU is free, auto picks it.
	eval := httptest.NewRecorder()
	h.evaluate(eval, httptest.NewRequest(http.MethodPost, "/api/v1/evaluate", strings.NewReader(fmt.Sprintf(`{"model_id":%d,"engine":"vllm","image":"example/vllm:test","placement_mode":"auto","target_vendor":"nvidia"}`, big.ID))))
	if eval.Code != http.StatusOK {
		t.Fatalf("evaluate status = %d body = %s", eval.Code, eval.Body.String())
	}
	var evaluated struct {
		SelectedNode *struct {
			UUID string `json:"uuid"`
		} `json:"selected_node"`
	}
	if err := json.Unmarshal(eval.Body.Bytes(), &evaluated); err != nil {
		t.Fatal(err)
	}
	if evaluated.SelectedNode == nil {
		t.Fatalf("auto placement must select while the GPU is free: %s", eval.Body.String())
	}
	selected := evaluated.SelectedNode.UUID

	// Another deployment takes the selected node's only GPU after the
	// evaluation (the ledger counts non-terminal instances).
	nodeRow, err := st.NodeByUUID(selected)
	if err != nil {
		t.Fatal(err)
	}
	deploy.SetDefaultImage("vllm", "example/vllm:test")
	occupier, err := st.CreateDeployment(&store.Deployment{Name: "occupier", ModelID: small.ID, NodeID: nodeRow.ID, State: "active"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateInstance(&store.Instance{UUID: "occupier-instance", DeploymentID: occupier.ID, NodeID: nodeRow.ID, State: "running", GPUIndexes: "[0]", APIPort: 30001}); err != nil {
		t.Fatal(err)
	}

	// The create repeating the evaluated draft is rejected, not re-placed.
	create := httptest.NewRecorder()
	h.createDeployment(create, httptest.NewRequest(http.MethodPost, "/api/v1/deployments", strings.NewReader(fmt.Sprintf(`{"name":"auto-vram","model_id":%d,"engine":"vllm","image":"example/vllm:test","placement_mode":"auto","node_uuid":%q,"placement":{"tp":1,"gpus":[0]},"quick_params":{}}`, big.ID, selected))))
	if create.Code != http.StatusConflict {
		t.Fatalf("create status = %d body = %s", create.Code, create.Body.String())
	}
	if !strings.Contains(create.Body.String(), ErrorAutomaticPlacementStale) {
		t.Fatalf("body must carry AUTOMATIC_PLACEMENT_STALE: %s", create.Body.String())
	}
}

// A GPU index the node does not have is the card control's error, not a
// launch-assembly failure: it names placement.gpus with
// PLACEMENT_GPU_NOT_ON_NODE (GWKS-522). Generic launch-assembly failures stay
// page-level (empty field), pinned by
// TestCreateAndEvaluateRefuseAManualScriptThatLeavesAssignedGPUsIdle.
func TestEvaluateReportsACardTheNodeLacksOnTheCardField(t *testing.T) {
	h, st := automaticPlacementFixture(t)
	modelID := automaticPlacementModel(t, st)
	rec := httptest.NewRecorder()
	h.evaluate(rec, httptest.NewRequest(http.MethodPost, "/api/v1/evaluate", strings.NewReader(fmt.Sprintf(`{"model_id":%d,"engine":"vllm","image":"example/vllm:test","node_uuid":"auto-big","placement":{"gpus":[7]},"placement_mode":"manual"}`, modelID))))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	var response struct {
		OK     bool             `json:"ok"`
		Errors []dto.FieldError `json:"errors"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.OK || len(response.Errors) != 1 {
		t.Fatalf("a card the node lacks must answer ok=false with one error: %s", rec.Body.String())
	}
	if got := response.Errors[0]; got.Field != "placement.gpus" || got.Code != ErrorPlacementGPUNotOnNode || got.Params["gpu_index"] != float64(7) {
		t.Fatalf("error = %+v, want placement.gpus %s for index 7", got, ErrorPlacementGPUNotOnNode)
	}
}

// Automatic placement with an explicitly selected template spec: the vendor
// hint must not become a chipmatch rejection, the spec resolves as the
// contract it is, and per-node feasibility comes from the candidates list.
func TestAutomaticPlacementWithExplicitSpecResolvesWithoutHardwareFacts(t *testing.T) {
	h, st := automaticPlacementFixture(t)
	fixture := deploymentFlatFixture()
	fixture.ModelID = "acme/explicit-auto"
	acme, err := st.UpsertModel(&store.Model{Source: "modelscope", SourceID: "Acme/Deploy-FP8", Revision: "main", Name: "Acme/Deploy-FP8", WeightBytes: 16 << 30})
	if err != nil {
		t.Fatal(err)
	}
	modelID := acme.ID
	if err := st.ReconcileInferenceCatalog([]store.InferenceModel{fixture}); err != nil {
		t.Fatal(err)
	}
	template, err := st.InferenceModelByModelID(fixture.ModelID)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	h.evaluate(rec, httptest.NewRequest(http.MethodPost, "/api/v1/evaluate", strings.NewReader(fmt.Sprintf(`{"model_id":%d,"template_id":%d,"template_spec_source":"upstream","template_spec_name":"fp8-balanced","engine":"vllm","image":"example/vllm:test","placement_mode":"auto","target_vendor":"nvidia"}`, modelID, template.ID))))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	var response struct {
		OK           bool   `json:"ok"`
		TemplateSpec string `json:"template_spec_name"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if !response.OK || response.TemplateSpec != "fp8-balanced" {
		t.Fatalf("explicit spec must resolve in auto mode: ok=%v spec=%q body=%s", response.OK, response.TemplateSpec, rec.Body.String()[:400])
	}
}

// A create the pool could never run - not even on an empty node - is refused
// at once instead of waiting forever; one the pool could run once something
// else frees up is created and waits. The pool is judged by what its nodes
// are, not by what currently runs on them.
func TestCreateRefusesADeploymentNoNodeCouldEverFit(t *testing.T) {
	h, st := automaticPlacementFixture(t)
	modelID := automaticPlacementModel(t, st) // 20 GiB of weights
	create := func(name, node string, reqs string) (int, string) {
		t.Helper()
		rec := httptest.NewRecorder()
		body := fmt.Sprintf(`{"name":%q,"model_id":%d,"engine":"vllm","image":"example/vllm:test","node_uuid":%q,"node_requirements":%s,"quick_params":{}}`, name, modelID, node, reqs)
		h.createDeployment(rec, httptest.NewRequest(http.MethodPost, "/api/v1/deployments", strings.NewReader(body)))
		return rec.Code, rec.Body.String()
	}
	// Pinned to the 16 GiB card: never.
	if status, body := create("never", "auto-small", `{}`); status != http.StatusBadRequest || !strings.Contains(body, ErrorDeploymentNeverFits) || !strings.Contains(body, "auto-small") {
		t.Fatalf("never-fitting create = %d %s", status, body)
	}
	// A pool of only small cards: never, whichever node the request names.
	if status, body := create("small-pool", "auto-small", `{"gpuworks.ai/node":["auto-small"],"gpuworks.ai/gpu-model":["A100"]}`); status != http.StatusBadRequest || !strings.Contains(body, ErrorDeploymentNeverFits) {
		t.Fatalf("small-card pool create = %d %s", status, body)
	}
	// A create names its node - automatic placement names the one its
	// evaluation picked - and one that names none, or a node that does not
	// exist, is told which field is wrong.
	for _, node := range []string{"", "ghost"} {
		status, body := create("no-node", node, `{}`)
		if status != http.StatusBadRequest || !strings.Contains(body, `"code":"`+ErrorFieldInvalid+`"`) || !strings.Contains(body, `"field":"node_uuid"`) {
			t.Fatalf("create naming node %q = %d %s", node, status, body)
		}
	}
	// The 80 GiB card, fully taken by another instance right now: it waits.
	big, err := st.NodeByUUID("auto-big")
	if err != nil {
		t.Fatal(err)
	}
	occupant, err := st.CreateDeployment(&store.Deployment{Name: "occupant", ModelID: modelID, NodeID: big.ID, Engine: "vllm"})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.DB().Create(&store.Instance{UUID: "occupant-1", DeploymentID: occupant.ID, NodeID: big.ID, State: proto.InstRunning, GPUIndexes: "[0]"}).Error; err != nil {
		t.Fatal(err)
	}
	if status, body := create("waits", "auto-big", `{}`); status != http.StatusCreated {
		t.Fatalf("a create that only has to wait = %d %s", status, body)
	}
}
