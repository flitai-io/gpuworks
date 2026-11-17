package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
)

// A new deployment runs priority scheduling unless the request turns it off
// or its launch cannot: the create's answer and the stored row agree, and
// asking for it where the launch cannot is refused with the reason.
func TestCreateDeploymentPriorityScheduling(t *testing.T) {
	h, st := automaticPlacementFixture(t)
	modelID := automaticPlacementModel(t, st)
	creates := 0
	create := func(extra string) (int, string, map[string]any) {
		rec := httptest.NewRecorder()
		creates++
		body := fmt.Sprintf(`{"name":"prio","served_name":"prio-%d","model_id":%d,"engine":"vllm","image":"example/vllm:test","node_uuid":"auto-big","quick_params":{}%s}`, creates, modelID, extra)
		h.createDeployment(rec, httptest.NewRequest(http.MethodPost, "/api/v1/deployments", strings.NewReader(body)))
		var out map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("body is not JSON: %s", rec.Body)
		}
		return rec.Code, rec.Body.String(), out
	}
	for _, tc := range []struct {
		extra string
		want  bool
	}{
		{extra: "", want: true},
		{extra: `,"priority_scheduling":true`, want: true},
		{extra: `,"priority_scheduling":false`, want: false},
		// The user's own fcfs wins over the platform: nothing to turn on.
		{extra: `,"extra_args":"--scheduling-policy fcfs"`, want: false},
	} {
		status, body, out := create(tc.extra)
		if status != http.StatusCreated {
			t.Fatalf("%q: create = %d %s", tc.extra, status, body)
		}
		row, err := st.GetDeployment(uint(out["id"].(float64)))
		if err != nil {
			t.Fatal(err)
		}
		if row.PriorityScheduling != tc.want || out["priority_scheduling"] != tc.want {
			t.Fatalf("%q: stored=%v answer=%v", tc.extra, row.PriorityScheduling, out["priority_scheduling"])
		}
	}
	status, body, _ := create(`,"priority_scheduling":true,"extra_args":"--scheduling-policy fcfs"`)
	if status != http.StatusBadRequest || !strings.Contains(body, "PRIORITY_SCHEDULING_POLICY_CONFLICT") || !strings.Contains(body, `"field":"priority_scheduling"`) {
		t.Fatalf("a conflicting ask = %d %s", status, body)
	}
	if status, body, _ := create(`,"priority_scheduling":"yes"`); status != http.StatusBadRequest {
		t.Fatalf("a non-boolean switch = %d %s", status, body)
	}
}

// Evaluation says whether the draft may turn priority scheduling on, on the
// arguments the engine would receive.
func TestEvaluatePriorityScheduling(t *testing.T) {
	h, st := automaticPlacementFixture(t)
	modelID := automaticPlacementModel(t, st)
	evaluate := func(extra string) string {
		rec := httptest.NewRecorder()
		body := fmt.Sprintf(`{"model_id":%d,"engine":"vllm","image":"example/vllm:test","node_uuid":"auto-big","placement_mode":"manual","quick_params":{}%s}`, modelID, extra)
		h.evaluate(rec, httptest.NewRequest(http.MethodPost, "/api/v1/evaluate", strings.NewReader(body)))
		if rec.Code != http.StatusOK {
			t.Fatalf("evaluate = %d %s", rec.Code, rec.Body)
		}
		return rec.Body.String()
	}
	if body := evaluate(""); !strings.Contains(body, `"priority_scheduling":{"ok":true}`) {
		t.Fatalf("an allowed draft: %s", body)
	}
	body := evaluate(`,"extra_args":"--scheduling-policy fcfs"`)
	if !strings.Contains(body, `"priority_scheduling":{"ok":false,"refusal":{"code":"PRIORITY_SCHEDULING_POLICY_CONFLICT","params":{"flag":"--scheduling-policy","value":"fcfs"}}}`) {
		t.Fatalf("a conflicting draft: %s", body)
	}
}

// Turning priority scheduling off (or on) changes the engine command, so the
// edit is a restart; an edit that does not mention it keeps it.
func TestEditDeploymentPrioritySchedulingRestarts(t *testing.T) {
	h, st := automaticPlacementFixture(t)
	dep := editableDeployment(t, h, automaticPlacementModel(t, st))
	if !dep.PriorityScheduling {
		t.Fatalf("a new deployment = %+v", dep)
	}
	draft, raw := getDraft(t, h, dep.ID)
	if !strings.Contains(raw, `"priority_scheduling":true`) {
		t.Fatalf("draft lacks the switch: %s", raw)
	}
	edit := draft.Deployment
	edit.PriorityScheduling = nil
	status, body := putEdit(t, h, dep.ID, dto.DeploymentEditRequest{Deployment: edit, BaseRevision: draft.Revision}, true)
	if status != http.StatusOK || strings.Contains(body, `"priority_scheduling"`) {
		t.Fatalf("an edit that says nothing = %d %s", status, body)
	}
	off := false
	edit.PriorityScheduling = &off
	status, body = putEdit(t, h, dep.ID, dto.DeploymentEditRequest{Deployment: edit, BaseRevision: draft.Revision}, false)
	if status != http.StatusOK {
		t.Fatalf("edit = %d %s", status, body)
	}
	result := decodeEditResult(t, body)
	found := false
	for _, change := range result.Plan.Changes {
		if change.Field == "priority_scheduling" {
			found = change.Layer == dto.DeploymentChangeRestart
		}
	}
	if !found || !result.Plan.Restart {
		t.Fatalf("plan = %s", body)
	}
	if row, _ := st.GetDeployment(dep.ID); row.PriorityScheduling {
		t.Fatal("the edit did not store the switch")
	}
	// Asking for it again where the edited launch cannot is refused.
	draft, _ = getDraft(t, h, dep.ID)
	edit = draft.Deployment
	on := true
	edit.PriorityScheduling, edit.ExtraArgs = &on, "--scheduling-policy fcfs"
	if status, body := putEdit(t, h, dep.ID, dto.DeploymentEditRequest{Deployment: edit, BaseRevision: draft.Revision}, true); status != http.StatusBadRequest || !strings.Contains(body, "PRIORITY_SCHEDULING_POLICY_CONFLICT") {
		t.Fatalf("conflicting edit = %d %s", status, body)
	}
}
