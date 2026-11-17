package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
)

func TestEvaluatePreviewGatesVLLMContextAutoOnTheImageVersion(t *testing.T) {
	h, templateID := evaluateFixtureWorld(t)
	models, err := h.d.Store.ListModels()
	if err != nil {
		t.Fatal(err)
	}
	var modelID uint
	for _, m := range models {
		if m.SourceID == "Acme/Deploy-FP8" {
			modelID = m.ID
		}
	}
	if modelID == 0 {
		t.Fatal("fixture model missing")
	}
	for _, tc := range []struct{ name, image, quick, extra, want string }{
		{"first supported version", "vllm/vllm-openai:v0.14.0", "{}", "", "auto"},
		{"modern version", "vllm/vllm-openai:v0.30.0", "{}", "", "auto"},
		{"vendor version", "gpuworks/gpuworks-runner:hggc13.0-vllm0.23.0-r2", "{}", "", "auto"},
		{"old version", "vllm/vllm-openai:v0.13.0", "{}", "", ""},
		{"unknown version", "private.example/vllm:custom", "{}", "", ""},
		{"explicit limit", "vllm/vllm-openai:v0.30.0", `{"max_model_len":4096}`, "", "4096"},
		{"typed auto", "vllm/vllm-openai:v0.30.0", `{"max_model_len":-1}`, "", "auto"},
		{"extra auto", "vllm/vllm-openai:v0.30.0", "{}", "--max-model-len auto", "auto"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := fmt.Sprintf(`{"model_id":%d,"node_uuid":"fixture-node","engine":"vllm","image":%q,"quick_params":%s,"extra_args":%q}`, modelID, tc.image, tc.quick, tc.extra)
			rec := httptest.NewRecorder()
			h.evaluate(rec, httptest.NewRequest(http.MethodPost, "/api/v1/evaluate", strings.NewReader(body)))
			if rec.Code != http.StatusOK {
				t.Fatalf("evaluate=%d: %s", rec.Code, rec.Body.String())
			}
			var response dto.EvaluateResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			argv := append(append([]string(nil), response.Entrypoint...), response.Argv...)
			value, present := flagValue(argv, "--max-model-len")
			if value != tc.want || present != (tc.want != "") || countFlag(argv, "--max-model-len") > 1 {
				t.Fatalf("context=%q, present=%t, want %q: %v", value, present, tc.want, argv)
			}
			if tc.want == "auto" {
				if fmt.Sprint(response.FormValues["max_model_len"]) != "-1" || fmt.Sprint(response.EffectiveParams["max_model_len"]) != "-1" {
					t.Fatalf("auto intent diverged: %s", rec.Body.String())
				}
				if response.FormValues["extra_args"] != "" {
					t.Fatalf("auto remained in extra_args: %v", response.FormValues)
				}
			}
		})
	}
	t.Run("no eligible node still previews versioned auto", func(t *testing.T) {
		body := fmt.Sprintf(`{"model_id":%d,"template_id":%d,"template_spec_source":"upstream","template_spec_name":"future-hardware","engine":"vllm","placement_mode":"auto","image":"vllm/vllm-openai:v0.30.0"}`, modelID, templateID)
		rec := httptest.NewRecorder()
		h.evaluate(rec, httptest.NewRequest(http.MethodPost, "/api/v1/evaluate", strings.NewReader(body)))
		if rec.Code != http.StatusOK {
			t.Fatalf("evaluate=%d: %s", rec.Code, rec.Body.String())
		}
		var response dto.EvaluateResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		argv := append(append([]string(nil), response.Entrypoint...), response.Argv...)
		if value, _ := flagValue(argv, "--max-model-len"); value != "auto" || countFlag(argv, "--max-model-len") != 1 {
			t.Fatalf("node-less preview lost auto: %v", argv)
		}
	})

}

// The form confirmation, stored HTTP representation and subsequent preview
// carry the same auto intent instead of reviving a previous numeric length.
func TestCreateAndReloadPreservesVLLMContextAuto(t *testing.T) {
	h, _ := evaluateFixtureWorld(t)
	models, err := h.d.Store.ListModels()
	if err != nil {
		t.Fatal(err)
	}
	var modelID uint
	for _, m := range models {
		if m.SourceID == "Acme/Deploy-FP8" {
			modelID = m.ID
		}
	}
	body := fmt.Sprintf(`{"name":"context-auto","served_name":"context-auto","model_id":%d,"node_uuid":"fixture-node","engine":"vllm","image":"vllm/vllm-openai:v0.30.0"}`, modelID)
	created := httptest.NewRecorder()
	h.createDeployment(created, httptest.NewRequest(http.MethodPost, "/api/v1/deployments", strings.NewReader(body)))
	if created.Code != http.StatusCreated {
		t.Fatalf("create=%d: %s", created.Code, created.Body.String())
	}
	var deployment dto.Deployment
	if err := json.Unmarshal(created.Body.Bytes(), &deployment); err != nil {
		t.Fatal(err)
	}
	route := chi.NewRouteContext()
	route.URLParams.Add("id", fmt.Sprint(deployment.ID))
	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/v1/deployments/%d", deployment.ID), nil)
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, route))
	reloaded := httptest.NewRecorder()
	h.getDeployment(reloaded, req)
	if reloaded.Code != http.StatusOK {
		t.Fatalf("get=%d: %s", reloaded.Code, reloaded.Body.String())
	}
	if err := json.Unmarshal(reloaded.Body.Bytes(), &deployment); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(deployment.QuickParams["max_model_len"]) != "-1" || deployment.ExtraArgs != "" {
		t.Fatalf("stored auto diverged: %s", reloaded.Body.String())
	}
	quick, err := json.Marshal(deployment.QuickParams)
	if err != nil {
		t.Fatal(err)
	}
	replay := fmt.Sprintf(`{"model_id":%d,"node_uuid":"fixture-node","engine":"vllm","image":"vllm/vllm-openai:v0.30.0","quick_params":%s}`, modelID, quick)
	preview := httptest.NewRecorder()
	h.evaluate(preview, httptest.NewRequest(http.MethodPost, "/api/v1/evaluate", strings.NewReader(replay)))
	if preview.Code != http.StatusOK {
		t.Fatalf("re-evaluate=%d: %s", preview.Code, preview.Body.String())
	}
	var response dto.EvaluateResponse
	if err := json.Unmarshal(preview.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	argv := append(append([]string(nil), response.Entrypoint...), response.Argv...)
	if value, _ := flagValue(argv, "--max-model-len"); value != "auto" || countFlag(argv, "--max-model-len") != 1 {
		t.Fatalf("reloaded argv=%v", argv)
	}
}
