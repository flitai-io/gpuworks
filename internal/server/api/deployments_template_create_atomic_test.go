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

func postTemplateDeployment(t *testing.T, h *apiHandlers, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	h.createDeployment(rec, httptest.NewRequest(http.MethodPost, "/api/v1/deployments", strings.NewReader(string(raw))))
	return rec
}

func TestTemplateDeploymentRepairsLegacyDefaultChatOperations(t *testing.T) {
	for name, operations := range map[string]string{
		"bare":        `[{"type":"openai.chat"}]`,
		"null_inputs": `[{"inputs":null,"type":"openai.chat"}]`,
	} {
		t.Run(name, func(t *testing.T) {
			st, h, templateID := templateOperationsFixture(t, "template_ops_legacy_chat_"+name)
			model, err := st.UpsertModel(&store.Model{
				Source: "modelscope", SourceID: "Acme/Embed",
				Revision: "0123456789abcdef0123456789abcdef01234567",
				Name:     "Acme/Embed", Operations: operations,
			})
			if err != nil {
				t.Fatal(err)
			}
			deployment := createTemplateDeployment(t, h, map[string]any{
				"template_id": templateID, "template_spec_name": "embed", "model_id": model.ID,
				"node_uuid": "template_ops_legacy_chat_" + name + "-node",
			})
			want := fmt.Sprint([]string{"openai.embeddings"})
			if got := fmt.Sprint(modelOperationTypes(t, st, model.ID)); got != want {
				t.Fatalf("model row operations=%s want %s", got, want)
			}
			if got := fmt.Sprint(deploymentOperationTypes(deployment)); got != want {
				t.Fatalf("deployment operations=%s want %s", got, want)
			}
		})
	}
}

func TestTemplateCreateHealthRefusalLeavesModelContractUntouched(t *testing.T) {
	st, h, templateID := templateOperationsFixture(t, "template_ops_health_atomic")
	if err := st.DB().Model(&store.InferenceSpecRow{}).
		Where("model_id = ? AND name = ?", templateID, "embed").
		Update("api_contract", `{"health_endpoint":"/health","operations":[{"type":"openai.rerank","endpoint":"/v1/rerank"}]}`).Error; err != nil {
		t.Fatal(err)
	}
	model, err := st.UpsertModel(&store.Model{
		Source: "modelscope", SourceID: "Acme/Embed",
		Revision: "0123456789abcdef0123456789abcdef01234567",
		Name:     "Acme/Embed", Operations: `[{"type":"openai.chat"}]`,
	})
	if err != nil {
		t.Fatal(err)
	}
	sub := st.Bus.Subscribe(8, "model", "deployment")
	defer sub.Close()
	rec := postTemplateDeployment(t, h, map[string]any{
		"name": "rerank-health", "served_name": "rerank-health", "health_check": true,
		"template_id": templateID, "template_spec_name": "embed", "model_id": model.ID,
		"node_uuid": "template_ops_health_atomic-node",
	})
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), ErrorFieldInvalid) || !strings.Contains(rec.Body.String(), "health_check") {
		t.Fatalf("uncheckable rerank create = %d %s", rec.Code, rec.Body)
	}
	stored, err := st.GetModel(model.ID)
	if err != nil || stored.Operations != model.Operations || stored.InputModalities != model.InputModalities {
		t.Fatalf("refused health check changed the model: before=%+v after=%+v err=%v", model, stored, err)
	}
	deployments, err := st.ListDeployments()
	if err != nil || len(deployments) != 0 {
		t.Fatalf("refused health check left deployments: %+v, %v", deployments, err)
	}
	select {
	case event := <-sub.C():
		t.Fatalf("refused health check published %+v", event)
	default:
	}
}

func TestTemplateCreateServedNameConflictLeavesModelContractUntouched(t *testing.T) {
	st, h, templateID := templateOperationsFixture(t, "template_ops_served_atomic")
	holder, err := st.CreateDeploymentWithRequirements(&store.Deployment{Name: "holder", ServedName: "held"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	model, err := st.UpsertModel(&store.Model{
		Source: "modelscope", SourceID: "Acme/Embed",
		Revision: "0123456789abcdef0123456789abcdef01234567",
		Name:     "Acme/Embed", Operations: `[{"type":"openai.chat"}]`,
	})
	if err != nil {
		t.Fatal(err)
	}
	sub := st.Bus.Subscribe(8, "model", "deployment")
	defer sub.Close()
	rec := postTemplateDeployment(t, h, map[string]any{
		"name": "new-holder", "served_name": holder.ServedName,
		"template_id": templateID, "template_spec_name": "embed", "model_id": model.ID,
		"node_uuid": "template_ops_served_atomic-node",
	})
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), ErrorServedNameInUse) {
		t.Fatalf("held served name create = %d %s", rec.Code, rec.Body)
	}
	stored, err := st.GetModel(model.ID)
	if err != nil || stored.Operations != model.Operations || stored.InputModalities != model.InputModalities {
		t.Fatalf("served-name conflict changed the model: before=%+v after=%+v err=%v", model, stored, err)
	}
	deployments, err := st.ListDeployments()
	if err != nil || len(deployments) != 1 || deployments[0].ID != holder.ID {
		t.Fatalf("served-name conflict left deployments: %+v, %v", deployments, err)
	}
	select {
	case event := <-sub.C():
		t.Fatalf("served-name conflict published %+v", event)
	default:
	}
}
