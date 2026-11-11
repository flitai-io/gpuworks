package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
)

func TestRouterEvaluateEditUsesStoredLaunchBase(t *testing.T) {
	_, st := automaticPlacementFixture(t)
	modelID := automaticPlacementModel(t, st)
	router, token := kunlunRouter(t, st)
	status, body := callRouter(t, router, token, http.MethodPost, "/api/v1/deployments", fmt.Sprintf(
		`{"name":"edit-eager","model_id":%d,"engine":"vllm","image":"example/vllm:test","node_uuid":"auto-big","quick_params":{"enforce_eager":false}}`, modelID))
	if status != http.StatusCreated {
		t.Fatalf("create = %d %s", status, body)
	}
	var dep dto.Deployment
	if err := json.Unmarshal([]byte(body), &dep); err != nil {
		t.Fatal(err)
	}
	status, body = callRouter(t, router, token, http.MethodGet, fmt.Sprintf("/api/v1/deployments/%d/draft", dep.ID), "")
	if status != http.StatusOK {
		t.Fatalf("draft = %d %s", status, body)
	}
	var draft dto.DeploymentDraft
	if err := json.Unmarshal([]byte(body), &draft); err != nil {
		t.Fatal(err)
	}
	if draft.Deployment.QuickParams["enforce_eager"] != false {
		t.Fatal("fixture lost the stored explicit Off")
	}
	draft.Deployment.ExtraArgs = "--enforce-eager"
	evaluate := func(id uint) dto.EvaluateResponse {
		t.Helper()
		raw, err := json.Marshal(dto.EvaluateRequest{DeployRequest: draft.Deployment, DeploymentID: id})
		if err != nil {
			t.Fatal(err)
		}
		status, body := callRouter(t, router, token, http.MethodPost, "/api/v1/evaluate", string(raw))
		if status != http.StatusOK {
			t.Fatalf("evaluate = %d %s", status, body)
		}
		var response dto.EvaluateResponse
		if err := json.Unmarshal([]byte(body), &response); err != nil {
			t.Fatal(err)
		}
		return response
	}
	// These exact fields disagree on a new deployment. Only a real stored
	// base makes the restated Off an untouched field of an edit.
	if response := evaluate(0); response.OK || len(response.Errors) == 0 || response.Errors[0].Code != "LAUNCH_CONFIG_CONFLICT" {
		t.Fatalf("create preview swallowed an explicit conflict: %+v", response.Errors)
	}
	response := evaluate(dep.ID)
	if !response.OK || len(response.Errors) != 0 || response.EffectiveParams["enforce_eager"] != true || !slices.Contains(response.Argv, "--enforce-eager") {
		t.Fatalf("edit preview disagrees with the stored base: ok=%v errors=%+v params=%v argv=%v", response.OK, response.Errors, response.EffectiveParams, response.Argv)
	}
	raw, err := json.Marshal(dto.DeploymentEditRequest{Deployment: draft.Deployment, BaseRevision: draft.Revision})
	if err != nil {
		t.Fatal(err)
	}
	status, body = callRouter(t, router, token, http.MethodPut, fmt.Sprintf("/api/v1/deployments/%d?dry_run=1", dep.ID), string(raw))
	if status != http.StatusOK {
		t.Fatalf("edit dry run = %d %s", status, body)
	}
	plan := decodePlan(t, body)
	if !plan.Restart || !slices.ContainsFunc(plan.Changes, func(change dto.DeploymentChange) bool {
		return change.Field == "quick_params.enforce_eager" && change.From == "false" && change.To == "true"
	}) {
		t.Fatalf("edit dry run did not agree on Off to On: %s", body)
	}
	stored, err := st.GetDeployment(dep.ID)
	if err != nil {
		t.Fatal(err)
	}
	quick, _ := storedObject(stored.QuickParams)
	if quick["enforce_eager"] != false || stored.ExtraArgs != "" {
		t.Fatal("preview or dry run mutated the deployment")
	}
	raw, err = json.Marshal(dto.EvaluateRequest{DeployRequest: draft.Deployment, DeploymentID: dep.ID + 1000})
	if err != nil {
		t.Fatal(err)
	}
	status, body = callRouter(t, router, token, http.MethodPost, "/api/v1/evaluate", string(raw))
	if status != http.StatusOK {
		t.Fatalf("missing edit base = %d %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &response); err != nil {
		t.Fatal(err)
	}
	if response.OK || len(response.Errors) == 0 || response.Errors[0].Code != "LAUNCH_CONFIG_CONFLICT" {
		t.Fatal("missing base must retain create-preview conflict validation")
	}
}
