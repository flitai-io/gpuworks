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

	"gpuworks.ai/gpuworks/internal/server/modelmeta"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// The places that name a model source explicitly each had to decide whether an
// HTTP directory belongs there. The decisions are recorded as assertions so a
// later change has to face them rather than discover them.

// A blank source still means Hugging Face: the generic registration form is the
// repository-id form, and a directory is registered through its own endpoint.
func TestCreateModelStillDefaultsToHuggingFace(t *testing.T) {
	st := urlModelStore(t, "api_create_model_default_source")
	sources := []string{}
	h := apiHandlers{d: Deps{Outside: OutsideSources{AllowPrivateHosts: []string{"127.0.0.1"}}, Store: st, FetchModelMeta: func(_ context.Context, source, _, _, _ string, _ []string) (*modelmeta.Meta, error) {
		sources = append(sources, source)
		return &modelmeta.Meta{}, nil
	}}}
	rec := httptest.NewRecorder()
	h.createModel(rec, httptest.NewRequest(http.MethodPost, "/api/v1/models", strings.NewReader(`{"source_id":"Qwen/Qwen3-0.6B"}`)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status/body = %d %s", rec.Code, rec.Body.String())
	}
	models, _ := st.ListModels()
	if len(models) != 1 || models[0].Source != "huggingface" {
		t.Fatalf("models = %+v", models)
	}
	if len(sources) != 1 || sources[0] != "huggingface" {
		t.Fatalf("metadata fetch sources = %v", sources)
	}
}

// ModelScope's default branch is master; a directory has no branches at all, so
// the substitution must not reach it and leave an artifact path of "-master".
func TestDefaultBranchSubstitutionStaysWithModelScope(t *testing.T) {
	st := urlModelStore(t, "api_create_model_branch_substitution")
	h := apiHandlers{d: Deps{Outside: OutsideSources{AllowPrivateHosts: []string{"127.0.0.1"}}, Store: st, FetchModelMeta: func(context.Context, string, string, string, string, []string) (*modelmeta.Meta, error) {
		return &modelmeta.Meta{}, nil
	}}}
	rec := httptest.NewRecorder()
	h.createModel(rec, httptest.NewRequest(http.MethodPost, "/api/v1/models",
		strings.NewReader(`{"source":"modelscope","source_id":"Qwen/Qwen3-0.6B","revision":"main"}`)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status/body = %d %s", rec.Code, rec.Body.String())
	}
	models, _ := st.ListModels()
	if len(models) != 1 || models[0].Revision != "master" {
		t.Fatalf("models = %+v", models)
	}

	// The directory registration path writes no revision at all.
	endpoint := urlModelDirectory(t)
	register := httptest.NewRecorder()
	h.registerURLModel(register, httptest.NewRequest(http.MethodPost, "/api/v1/models/register-url",
		strings.NewReader(fmt.Sprintf(`{"source_url":%q}`, endpoint))))
	if register.Code != http.StatusCreated {
		t.Fatalf("register status/body = %d %s", register.Code, register.Body.String())
	}
	models, _ = st.ListModels()
	for _, model := range models {
		if model.Source == "url" && model.Revision != "" {
			t.Fatalf("directory model carries revision %q", model.Revision)
		}
	}
}

// Browser-supplied metadata exists because a browser can reach Hugging Face
// through an operator's own proxy when the server cannot. A directory has no
// such story — the server reads it directly — so the directory keeps the
// server-side fetch, with its own endpoint.
func TestBrowserSuppliedMetadataStaysWithHuggingFace(t *testing.T) {
	st := urlModelStore(t, "api_refresh_browser_metadata_scope")
	endpoints := []string{}
	h := apiHandlers{d: Deps{Outside: OutsideSources{AllowPrivateHosts: []string{"127.0.0.1"}}, Store: st, FetchModelMeta: func(_ context.Context, source, repo, _, _ string, candidates []string) (*modelmeta.Meta, error) {
		endpoints = append(endpoints, source+"|"+strings.Join(candidates, ","))
		return &modelmeta.Meta{Arch: "Qwen3ForCausalLM", WeightBytes: 4096, ConfigRaw: urlModelConfig}, nil
	}}}
	directory, err := st.UpsertModel(&store.Model{
		Source: "url", SourceID: "Qwen/Qwen3-0.6B", SourceURL: "https://hf-mirror.example.com/Qwen/Qwen3-0.6B", Name: "Qwen3-0.6B",
	})
	if err != nil {
		t.Fatal(err)
	}

	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", fmt.Sprint(directory.ID))
	request := httptest.NewRequest(http.MethodPost, "/api/v1/models/1/metadata/refresh",
		strings.NewReader(`{"browser_metadata":true,"config_raw":"{\"model_type\":\"llama\"}","weight_bytes":9}`))
	request = request.WithContext(context.WithValue(context.Background(), chi.RouteCtxKey, rctx))
	rec := httptest.NewRecorder()
	h.refreshModelMetadata(rec, request)
	if rec.Code != http.StatusOK {
		t.Fatalf("status/body = %d %s", rec.Code, rec.Body.String())
	}
	if len(endpoints) != 1 || endpoints[0] != "url|https://hf-mirror.example.com/Qwen/Qwen3-0.6B" {
		t.Fatalf("metadata fetches = %v, want the directory as the endpoint", endpoints)
	}
	updated, err := st.GetModel(directory.ID)
	if err != nil || updated.Arch != "Qwen3ForCausalLM" || updated.WeightBytes != 4096 {
		t.Fatalf("refreshed model = %+v err = %v", updated, err)
	}
}

// Quick registration exists for an inference-template weight source, and the
// catalog only accepts the two repository providers. A directory reaches the
// platform through its own registration, which is the only place the directory
// URL is read and stored.
func TestQuickRegisterRefusesADirectorySource(t *testing.T) {
	h := apiHandlers{d: Deps{Outside: OutsideSources{AllowPrivateHosts: []string{"127.0.0.1"}}, Store: urlModelStore(t, "api_quick_register_url")}}
	rec := httptest.NewRecorder()
	h.quickRegisterModel(rec, httptest.NewRequest(http.MethodPost, "/api/v1/models/quick-register",
		strings.NewReader(`{"source":"url","source_id":"Qwen/Qwen3-0.6B"}`)))
	var wire struct {
		Code   string         `json:"code"`
		Params map[string]any `json:"params"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &wire)
	if rec.Code != http.StatusBadRequest || wire.Code != ErrorQuickRegisterSourceUnsupported || wire.Params["source"] != "url" {
		t.Fatalf("status/body = %d %s", rec.Code, rec.Body.String())
	}
	if models, _ := h.d.Store.ListModels(); len(models) != 0 {
		t.Fatalf("a refused quick registration stored %d models", len(models))
	}

	// The default-branch substitution quick registration shares with the
	// generic form is ModelScope's alone, for the same reason: a directory has
	// no branches to substitute.
	accepted := httptest.NewRecorder()
	h.quickRegisterModel(accepted, httptest.NewRequest(http.MethodPost, "/api/v1/models/quick-register",
		strings.NewReader(`{"source":"modelscope","source_id":"Qwen/Qwen3-0.6B","revision":"main"}`)))
	if accepted.Code != http.StatusCreated {
		t.Fatalf("status/body = %d %s", accepted.Code, accepted.Body.String())
	}
	models, _ := h.d.Store.ListModels()
	if len(models) != 1 || models[0].Revision != "master" {
		t.Fatalf("quick registered models = %+v", models)
	}
}
