package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// go test ./internal/server/api -run TestRawPoolingDeploymentMaterializesTask -count=1
func TestRawPoolingDeploymentMaterializesTask(t *testing.T) {
	for _, tc := range []struct{ operation, version, flag, value string }{
		{"openai.rerank", "0.23.0", "--runner", "pooling"},
		{"openai.embeddings", "0.30.0", "--runner", "pooling"},
		{"openai.embeddings,openai.rerank", "0.23.0", "--runner", "pooling"},
		{"openai.rerank", "0.9.0", "--task", "score"},
		{"openai.embeddings", "0.9.0", "--task", "embed"},
	} {
		t.Run(tc.operation+"/"+tc.version, func(t *testing.T) {
			st, err := store.Open("file:raw_pooling_task?mode=memory&cache=shared", bus.New())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = st.Close() })
			var operations []map[string]string
			for _, operation := range strings.Split(tc.operation, ",") {
				operations = append(operations, map[string]string{"type": operation})
			}
			encodedOperations, _ := json.Marshal(operations)
			model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "fixture/opaque-model", Revision: "main", Name: "opaque-model", WeightBytes: 1 << 30, Operations: string(encodedOperations)})
			if err != nil {
				t.Fatal(err)
			}
			launchConfigNode(t, st, "pooling-node")
			h := &apiHandlers{d: Deps{Store: st}}
			body := fmt.Sprintf(`{"name":"raw-pooling","model_id":%d,"node_uuid":"pooling-node","engine":"vllm","image":"vllm/vllm-openai:v%s","quick_params":{"max_model_len":512}}`, model.ID, tc.version)
			previewResponse := httptest.NewRecorder()
			h.evaluate(previewResponse, httptest.NewRequest(http.MethodPost, "/api/v1/evaluate", strings.NewReader(body)))
			if previewResponse.Code != http.StatusOK {
				t.Fatalf("evaluate=%d: %s", previewResponse.Code, previewResponse.Body.String())
			}
			var preview dto.EvaluateResponse
			if err := json.Unmarshal(previewResponse.Body.Bytes(), &preview); err != nil {
				t.Fatal(err)
			}
			if !preview.OK {
				t.Fatalf("preview not OK: %s", previewResponse.Body.String())
			}
			argv := append(append([]string(nil), preview.Entrypoint...), preview.Argv...)
			if got, ok := flagValue(argv, tc.flag); !ok || got != tc.value {
				t.Errorf("pooling preview lost task intent: %v", argv)
			}
			response := httptest.NewRecorder()
			h.createDeployment(response, httptest.NewRequest(http.MethodPost, "/api/v1/deployments", strings.NewReader(body)))
			if response.Code != http.StatusCreated {
				t.Fatalf("create=%d: %s", response.Code, response.Body.String())
			}
			var created dto.Deployment
			if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil {
				t.Fatal(err)
			}
			stored, err := st.GetDeployment(created.ID)
			if err != nil {
				t.Fatal(err)
			}
			// New task defaults must survive the persisted launch, rather
			// than being added only to the preview and lost before agent execution.
			if !strings.Contains(stored.ExtraArgs, tc.flag+" "+tc.value) {
				t.Errorf("stored launch lost task intent: %s", stored.ExtraArgs)
			}
		})
	}
}

func TestTemplatePoolingDefaultsAreStoredInCatalogLayer(t *testing.T) {
	st, h, id := templateOperationsFixture(t, "pooling_template_layer")
	created := createTemplateDeployment(t, h, map[string]any{"template_id": id, "template_spec_name": "embed", "node_uuid": "pooling_template_layer-node", "image": "vllm/vllm-openai:v0.23.0"})
	row, err := st.GetDeployment(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	var selection deploy.CatalogSelection
	if err := json.Unmarshal([]byte(row.CatalogSelectionSnapshot), &selection); err != nil {
		t.Fatal(err)
	}
	if got, ok := flagValue(selection.ExtraArgs, "--runner"); !ok || got != "pooling" {
		t.Fatalf("template task not persisted: %v", selection.ExtraArgs)
	}
	if strings.Contains(row.ExtraArgs, "--runner") {
		t.Fatalf("template default attributed to user override: %q", row.ExtraArgs)
	}
}
