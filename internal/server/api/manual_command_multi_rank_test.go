package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/messages"
)

// manualOverrides is the container_overrides a form in manual mode sends: the
// script, the mode, and that the platform keeps running the preparation.
func manualOverrides(t *testing.T, script string) string {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"command_mode": "manual", "cmd_display": script, "command_includes_preparation": false})
	if err != nil {
		t.Fatal(err)
	}
	return `,"container_overrides":` + string(raw)
}

func (r *autoRig) evaluateBody(t *testing.T, body string) dto.EvaluateResponse {
	t.Helper()
	rec := r.do(r.session, http.MethodPost, "/api/v1/evaluate", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("evaluate = %d %s", rec.Code, rec.Body)
	}
	var out dto.EvaluateResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// pinnedMultiBody is a two-rank deployment on named nodes.
func (r *autoRig) pinnedMultiBody(name, extra string) string {
	return fmt.Sprintf(`{"name":%q,"served_name":%q,"model_id":%d,"engine":"vllm","image":"vllm/vllm-openai:v0.25.1","quick_params":{"max_model_len":4096},"nnodes":2,"node_uuids":["au-1","au-2"],"placement":{"tp":2}%s}`, name, name, r.modelID, extra)
}

// pinnedPDBody is a 1P1D deployment on named nodes.
func (r *autoRig) pinnedPDBody(name, extra string) string {
	return fmt.Sprintf(`{"name":%q,"served_name":%q,"model_id":%d,"engine":"vllm","image":"vllm/vllm-openai:v0.25.1","quick_params":{"max_model_len":4096}%s,"placement":{"pd":{"mode":"vllm-nixl","roles":{"prefill":{"tp":1,"units":[{"node_uuid":"au-1"}]},"decode":{"tp":1,"units":[{"node_uuid":"au-2"}]}}}}}`, name, name, r.modelID, extra)
}

func rankBlindError(resp dto.EvaluateResponse) *dto.FieldError {
	for index, item := range resp.Errors {
		if item.Code == messages.ErrorManualCommandRankBlind {
			return &resp.Errors[index]
		}
	}
	return nil
}

// A multi-node draft gets a manual command script with the platform
// variables in it; submitted back as the manual command, every rank's launch
// is that script with the rank's own values.
func TestEvaluateMultiNodeManualCommandDraftExpandsPerRank(t *testing.T) {
	r := newAutoRig(t, 2)
	engine := r.evaluateBody(t, r.pinnedMultiBody("multi", ""))
	if !engine.OK || len(engine.RankCommands) != 2 {
		t.Fatalf("engine preview = ok %t errors %+v ranks %d", engine.OK, engine.Errors, len(engine.RankCommands))
	}
	draft := engine.ManualCommandDraft
	for _, want := range []string{"set -eu", `case "${GPUWORKS_HEADLESS}" in`, "--node-rank ${GPUWORKS_NODE_RANK}", "--master-port ${GPUWORKS_MASTER_PORT}", "--headless"} {
		if !strings.Contains(draft, want) {
			t.Fatalf("draft lacks %q:\n%s", want, draft)
		}
	}
	for _, name := range []string{"GPUWORKS_HEADLESS", "GPUWORKS_MASTER_ADDR", "GPUWORKS_MASTER_PORT", "GPUWORKS_MODEL_PATH", "GPUWORKS_NODE_RANK"} {
		if !slices.Contains(engine.ManualCommandVariables, name) {
			t.Fatalf("variables %v lack %s", engine.ManualCommandVariables, name)
		}
	}
	if !slices.IsSorted(engine.ManualCommandVariables) || slices.Contains(engine.ManualCommandVariables, "GPUWORKS_PD_ROLE") {
		t.Fatalf("variables = %v, want sorted and only what these launches state", engine.ManualCommandVariables)
	}

	manual := r.evaluateBody(t, r.pinnedMultiBody("multi", manualOverrides(t, draft)))
	if !manual.OK || len(manual.RankCommands) != 2 {
		t.Fatalf("manual preview = ok %t errors %+v ranks %d", manual.OK, manual.Errors, len(manual.RankCommands))
	}
	if manual.ManualCommandDraft != draft {
		t.Fatalf("the draft of a manual request differs from the engine one:\n%s\n---\n%s", manual.ManualCommandDraft, draft)
	}
	for rank, command := range manual.RankCommands {
		script := strings.Join(command.Command, " ")
		headless := map[bool]string{false: "0", true: "1"}[rank > 0]
		for _, want := range []string{
			`case "` + headless + `" in`,
			fmt.Sprintf("--node-rank %d", rank),
			"--master-addr " + engine.RankCommands[0].RankAddress,
		} {
			if !strings.Contains(script, want) {
				t.Fatalf("rank %d script lacks %q:\n%s", rank, want, script)
			}
		}
		if strings.Contains(script, "${GPUWORKS_") {
			t.Fatalf("rank %d script keeps a stated variable:\n%s", rank, script)
		}
		if command.CommandMode != "manual" || command.Env["VLLM_HOST_IP"] != command.RankAddress {
			t.Fatalf("rank %d mode %q env %v, want manual with the rank's system environment", rank, command.CommandMode, command.Env)
		}
	}
	if out := r.mustCreate(t, r.pinnedMultiBody("multi", manualOverrides(t, draft))); out.NNodes != 2 {
		t.Fatalf("created = %+v", out)
	}
}

// A manual command that cannot tell its ranks apart is refused by evaluation
// and creation alike, for a multi-node and a PD deployment; the draft passes.
func TestManualCommandRankBlindRefusedByEvaluateAndCreate(t *testing.T) {
	r := newAutoRig(t, 2)
	for _, tc := range []struct {
		name    string
		body    func(name, extra string) string
		missing []string
	}{
		{"multi-node", r.pinnedMultiBody, []string{"GPUWORKS_NODE_RANK"}},
		{"multi-node automatic", r.multiBody, []string{"GPUWORKS_NODE_RANK"}},
		{"pd", r.pinnedPDBody, []string{"GPUWORKS_PD_ROLE"}},
		{"pd automatic", func(name, extra string) string { return r.pdAutoBody(name, `{}`, `{}`, extra) }, []string{"GPUWORKS_PD_ROLE"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			blind := manualOverrides(t, "vllm serve ${GPUWORKS_MODEL_PATH} --port ${GPUWORKS_API_PORT}")
			resp := r.evaluateBody(t, tc.body("blind", blind))
			refusal := rankBlindError(resp)
			if resp.OK || refusal == nil || refusal.Field != "container_cmd" || fmt.Sprint(refusal.Params["missing"]) != fmt.Sprint(tc.missing) {
				t.Fatalf("evaluate = ok %t errors %+v, want a container_cmd %s missing %v", resp.OK, resp.Errors, messages.ErrorManualCommandRankBlind, tc.missing)
			}
			// The refusal rides with the preview, so the form still shows the
			// launches and the draft that would satisfy it.
			if resp.ManualCommandDraft == "" || len(resp.ManualCommandVariables) == 0 {
				t.Fatalf("refused evaluate lacks the draft: %q %v (warnings %+v)", resp.ManualCommandDraft, resp.ManualCommandVariables, resp.Warnings)
			}
			rec := r.do(r.session, http.MethodPost, "/api/v1/deployments", tc.body("blind", blind))
			code, params := errorBody(t, rec.Body.String())
			if rec.Code != http.StatusBadRequest || code != messages.ErrorManualCommandRankBlind || fmt.Sprint(params["missing"]) != fmt.Sprint(tc.missing) {
				t.Fatalf("create = %d %s, want 400 %s", rec.Code, rec.Body, messages.ErrorManualCommandRankBlind)
			}

			name := strings.ReplaceAll(tc.name, " ", "-")
			withDraft := tc.body(name, manualOverrides(t, resp.ManualCommandDraft))
			if again := r.evaluateBody(t, withDraft); !again.OK {
				t.Fatalf("evaluate with the draft = %+v", again.Errors)
			}
			r.mustCreate(t, withDraft)
		})
	}
}

// One launch keeps the manual command as it was: no draft, no refusal, and a
// bare ${MODEL_PATH} is the user's shell variable, not the platform's.
func TestSingleNodeManualCommandUnchanged(t *testing.T) {
	r := newAutoRig(t, 1)
	script := "export MODEL_PATH=/elsewhere\nexec vllm serve ${MODEL_PATH} --port 8000"
	body := fmt.Sprintf(`{"name":"single","served_name":"single","model_id":%d,"engine":"vllm","image":"vllm/vllm-openai:v0.25.1","quick_params":{"max_model_len":4096},"node_uuid":"au-1"%s}`, r.modelID, manualOverrides(t, script))
	resp := r.evaluateBody(t, body)
	if !resp.OK || resp.ManualCommandDraft != "" || resp.ManualCommandVariables != nil || rankBlindError(resp) != nil {
		t.Fatalf("single-node manual preview = ok %t errors %+v draft %q", resp.OK, resp.Errors, resp.ManualCommandDraft)
	}
	for _, warning := range resp.Warnings {
		if warning.Code == messages.WarningManualDraftUnavailable {
			t.Fatalf("single launch warns %s", warning.Code)
		}
	}
	command := strings.Join(resp.Command, " ")
	if !strings.Contains(command, "exec vllm serve ${MODEL_PATH}") {
		t.Fatalf("command = %q, want the bare ${MODEL_PATH} left to the shell", command)
	}
	if _, ok := resp.Env["VLLM_HOST_IP"]; ok {
		t.Fatalf("single-node manual env = %v, want no system layer", resp.Env)
	}
	r.mustCreate(t, body)
}
