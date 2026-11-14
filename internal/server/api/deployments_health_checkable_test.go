package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/store"
)

// healthCheckableEvaluate evaluates a draft and returns the answer's raw wire
// JSON, so a key is judged by name and value as a client reads it.
func healthCheckableEvaluate(t *testing.T, h *apiHandlers, body map[string]any) map[string]json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	h.evaluate(rec, httptest.NewRequest(http.MethodPost, "/api/v1/evaluate", strings.NewReader(string(raw))))
	if rec.Code != http.StatusOK {
		t.Fatalf("evaluate=%d: %s", rec.Code, rec.Body.String())
	}
	var out map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("body is not JSON: %s", rec.Body)
	}
	return out
}

// A model registered without a task has empty operations until a template
// deployment seeds them. The deploy form asks the evaluation whether the draft
// can be health checked, and the answer must be the one the create will give:
// it judges the operations the template declares, not the empty row. The
// create and the evaluation agree for every contract the template can declare.
func TestEvaluateHealthCheckableFollowsTheTemplateContract(t *testing.T) {
	for _, tc := range []struct {
		name       string
		operations string
		want       bool
	}{
		{"decision", `{"type":"systemone.decide","endpoint":"/v1/systemone"}`, true},
		{"embeddings", `{"type":"openai.embeddings","endpoint":"/v1/embeddings"}`, true},
		{"chat", `{"type":"openai.chat","endpoint":"/v1/chat/completions"}`, true},
		{"rerank_only", `{"type":"openai.rerank","endpoint":"/v1/rerank"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			name := "health_checkable_" + tc.name
			st, h, templateID := templateOperationsFixture(t, name)
			if err := st.DB().Model(&store.InferenceSpecRow{}).
				Where("model_id = ? AND name = ?", templateID, "embed").
				Update("api_contract", fmt.Sprintf(`{"health_endpoint":"/health","operations":[%s]}`, tc.operations)).Error; err != nil {
				t.Fatal(err)
			}
			entity := &store.Model{Source: "modelscope", SourceID: "Acme/Embed", Revision: "0123456789abcdef0123456789abcdef01234567", Name: "Acme/Embed"}
			initRegisteredModelContract(entity)
			if entity.Operations != `[]` {
				t.Fatalf("precondition: registration stored %q, not empty operations", entity.Operations)
			}
			model, err := st.UpsertModel(entity)
			if err != nil {
				t.Fatal(err)
			}
			draft := map[string]any{"template_id": templateID, "template_spec_name": "embed", "model_id": model.ID, "node_uuid": name + "-node"}

			evaluated := healthCheckableEvaluate(t, h, draft)
			if got := string(evaluated["health_checkable"]); got != fmt.Sprint(tc.want) {
				t.Fatalf("evaluate health_checkable = %q, want %v", got, tc.want)
			}

			if tc.want {
				// The create, stating nothing, turns the check on exactly when
				// the evaluation said it could be checked.
				created := createTemplateDeployment(t, h, draft)
				row, err := st.GetDeployment(created.ID)
				if err != nil {
					t.Fatal(err)
				}
				if !row.HealthCheck {
					t.Fatalf("evaluation said checkable but the create left the check off: %+v", row)
				}
				if !created.HealthCheckable {
					t.Fatal("the created deployment's health_checkable is false")
				}
				return
			}
			draft["health_check"] = true
			raw, _ := json.Marshal(draft)
			rec := httptest.NewRecorder()
			h.createDeployment(rec, httptest.NewRequest(http.MethodPost, "/api/v1/deployments", strings.NewReader(string(raw))))
			if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "health_check") {
				t.Fatalf("evaluation said uncheckable but the create did not refuse: %d %s", rec.Code, rec.Body.String())
			}
		})
	}
}

// Operations someone set on the model are its contract; the template does not
// override them, so the answer follows the model.
func TestEvaluateHealthCheckableKeepsOperationsSetOnTheModel(t *testing.T) {
	st, h, templateID := templateOperationsFixture(t, "health_checkable_kept")
	model, err := st.UpsertModel(&store.Model{Source: "modelscope", SourceID: "Acme/Embed", Revision: "0123456789abcdef0123456789abcdef01234567", Name: "Acme/Embed", Operations: `[{"type":"openai.rerank","endpoint":"/v1/rerank"}]`})
	if err != nil {
		t.Fatal(err)
	}
	evaluated := healthCheckableEvaluate(t, h, map[string]any{"template_id": templateID, "template_spec_name": "embed", "model_id": model.ID, "node_uuid": "health_checkable_kept-node"})
	if got := string(evaluated["health_checkable"]); got != "false" {
		t.Fatalf("health_checkable = %q, want false", got)
	}
}

// A draft with a field error still says whether it can be checked, so the
// switch does not go unknown while the deployer fixes the field.
func TestEvaluateHealthCheckableSurvivesAFieldError(t *testing.T) {
	st, h, templateID := templateOperationsFixture(t, "health_checkable_field_error")
	model, err := st.UpsertModel(&store.Model{Source: "modelscope", SourceID: "Acme/Embed", Revision: "0123456789abcdef0123456789abcdef01234567", Name: "Acme/Embed", Operations: `[]`})
	if err != nil {
		t.Fatal(err)
	}
	evaluated := healthCheckableEvaluate(t, h, map[string]any{
		"template_id": templateID, "template_spec_name": "embed", "model_id": model.ID, "node_uuid": "health_checkable_field_error-node",
		"health_check_threshold_sec": 9999,
	})
	if got := string(evaluated["ok"]); got != "false" {
		t.Fatalf("precondition: the draft should carry a field error, ok = %q", got)
	}
	if got := string(evaluated["health_checkable"]); got != "true" {
		t.Fatalf("health_checkable = %q, want true", got)
	}
}

// A draft whose model does not exist has no contract to judge: the key is
// absent (unknown), never false.
func TestEvaluateHealthCheckableAbsentWithoutAModel(t *testing.T) {
	_, h, _ := templateOperationsFixture(t, "health_checkable_unknown")
	rec := httptest.NewRecorder()
	h.evaluate(rec, httptest.NewRequest(http.MethodPost, "/api/v1/evaluate", strings.NewReader(`{"model_id":999999,"engine":"vllm","node_uuid":"health_checkable_unknown-node"}`)))
	var out map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("body is not JSON: %s", rec.Body)
	}
	if raw, present := out["health_checkable"]; present {
		t.Fatalf("health_checkable = %s for a draft whose model does not exist, want it absent", raw)
	}
}

// The deployment details carry health_checkable under that exact key, judged
// from the model's operations.
func TestDeploymentWireCarriesHealthCheckable(t *testing.T) {
	h, st := automaticPlacementFixture(t)
	modelID := automaticPlacementModel(t, st)
	for i, tc := range []struct{ operations, want string }{
		{`[{"type":"openai.chat","endpoint":"/v1/chat/completions"}]`, `"health_checkable":true`},
		{`[{"type":"systemone.decide","endpoint":"/v1/systemone"}]`, `"health_checkable":true`},
		{`[{"type":"openai.rerank","endpoint":"/v1/rerank"}]`, `"health_checkable":false`},
		{`[]`, `"health_checkable":false`},
	} {
		setModelOperations(t, st, modelID, tc.operations)
		name := fmt.Sprintf("wire-%d", i)
		dep, err := st.CreateDeployment(&store.Deployment{Name: name, ServedName: name, ModelID: modelID, Engine: "vllm", Image: "example/vllm:test", Replicas: 1})
		if err != nil {
			t.Fatal(err)
		}
		raw, err := json.Marshal(h.deploymentDTO(dep, false))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(raw), tc.want) {
			t.Fatalf("operations %s: deployment JSON lacks %s: %s", tc.operations, tc.want, raw)
		}
	}
}
