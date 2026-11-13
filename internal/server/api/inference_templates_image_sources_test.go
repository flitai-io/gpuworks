package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

const templateImageSourcesYAML = `models:
- model_id: Acme/Sources
  specs:
  - name: bf16-vllm-nvidia
    engine: vllm
    image: ghcr.io/org/runner@sha256:22789f0c1a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d5e6f708192a3b4c5
    image_sources:
    - swr.cn-southwest-2.myhuaweicloud.com/gpuworks/runner@sha256:22789f0c1a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d5e6f708192a3b4c5
    - ghcr.nju.edu.cn/org/runner:v0.6.0
    weights: {quantization: bf16, sources: [{huggingface: Acme/Sources}]}
    gpus: {vendor: nvidia}
  - name: bf16-vllm-nvidia-plain
    engine: vllm
    image: vllm/vllm-openai:v0.25.1
    weights: {quantization: bf16, sources: [{huggingface: Acme/Sources}]}
    gpus: {vendor: nvidia}
`

func importTemplateImageSources(t *testing.T, name string) (*apiHandlers, *store.Store) {
	t.Helper()
	st, err := store.Open("file:api_template_image_sources_"+name+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	h := &apiHandlers{d: Deps{Store: st}}
	rec := httptest.NewRecorder()
	h.importInferenceTemplates(rec, httptest.NewRequest(http.MethodPost, "/api/v1/inference-templates/import", strings.NewReader(templateImageSourcesYAML)))
	if rec.Code != http.StatusOK {
		t.Fatalf("import = %d: %s", rec.Code, rec.Body.String())
	}
	return h, st
}

// specWire returns the literal JSON object of each spec in a template document,
// keyed by name, so the test reads exactly the keys a client receives.
func specWire(t *testing.T, document []byte) map[string]map[string]any {
	t.Helper()
	var template struct {
		Specs []map[string]any `json:"specs"`
	}
	if err := json.Unmarshal(document, &template); err != nil {
		t.Fatal(err)
	}
	out := map[string]map[string]any{}
	for _, spec := range template.Specs {
		out[spec["name"].(string)] = spec
	}
	return out
}

// The template detail carries the spec's image sources as an ordered list of
// strings under image_sources, next to the image they are sources of, and a spec
// that declares none has no such key.
func TestInferenceTemplateDetailExposesImageSources(t *testing.T) {
	h, _ := importTemplateImageSources(t, "detail")
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("*", "acme/sources")
	rec := httptest.NewRecorder()
	h.getInferenceTemplate(rec, httptest.NewRequest(http.MethodGet, "/api/v1/inference-templates/model/acme/sources", nil).WithContext(context.WithValue(context.Background(), chi.RouteCtxKey, rctx)))
	if rec.Code != http.StatusOK {
		t.Fatalf("detail = %d: %s", rec.Code, rec.Body.String())
	}
	specs := specWire(t, rec.Body.Bytes())
	with := specs["bf16-vllm-nvidia"]
	if with["image"] != "ghcr.io/org/runner@sha256:22789f0c1a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d5e6f708192a3b4c5" {
		t.Fatalf("image = %v", with["image"])
	}
	want := []any{
		"swr.cn-southwest-2.myhuaweicloud.com/gpuworks/runner@sha256:22789f0c1a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d5e6f708192a3b4c5",
		"ghcr.nju.edu.cn/org/runner:v0.6.0",
	}
	if !reflect.DeepEqual(with["image_sources"], want) {
		t.Fatalf("image_sources = %#v, want %#v", with["image_sources"], want)
	}
	if _, present := specs["bf16-vllm-nvidia-plain"]["image_sources"]; present {
		t.Fatalf("a spec without image sources must omit the key: %v", specs["bf16-vllm-nvidia-plain"])
	}
	if !strings.Contains(rec.Body.String(), `"image_sources":["swr.cn-southwest-2.myhuaweicloud.com/gpuworks/runner@sha256:`) {
		t.Fatalf("raw wire form lost the array: %s", rec.Body.String())
	}
}

func TestInferenceTemplateListExposesImageSources(t *testing.T) {
	h, _ := importTemplateImageSources(t, "list")
	rec := httptest.NewRecorder()
	h.listInferenceTemplates(rec, httptest.NewRequest(http.MethodGet, "/api/v1/inference-templates", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("list = %d: %s", rec.Code, rec.Body.String())
	}
	var page struct {
		Items []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil || len(page.Items) != 1 {
		t.Fatalf("page = %s err=%v", rec.Body.String(), err)
	}
	specs := specWire(t, page.Items[0])
	if sources, _ := specs["bf16-vllm-nvidia"]["image_sources"].([]any); len(sources) != 2 || sources[1] != "ghcr.nju.edu.cn/org/runner:v0.6.0" {
		t.Fatalf("list image_sources = %#v", specs["bf16-vllm-nvidia"]["image_sources"])
	}
}

// Cloning a template into an editable one keeps the sources, and what the
// editor round trips is the same list.
func TestInferenceTemplateCloneKeepsImageSources(t *testing.T) {
	h, st := importTemplateImageSources(t, "clone")
	rec := httptest.NewRecorder()
	h.cloneInferenceTemplate(rec, httptest.NewRequest(http.MethodPost, "/api/v1/inference-templates/clone", strings.NewReader(`{"source_model_id":"acme/sources","model_id":"me/sources"}`)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("clone = %d: %s", rec.Code, rec.Body.String())
	}
	if sources, _ := specWire(t, rec.Body.Bytes())["bf16-vllm-nvidia"]["image_sources"].([]any); len(sources) != 2 {
		t.Fatalf("clone response image_sources = %v", specWire(t, rec.Body.Bytes())["bf16-vllm-nvidia"]["image_sources"])
	}
	stored, err := st.InferenceModelByModelID("me/sources")
	if err != nil {
		t.Fatal(err)
	}
	for _, spec := range stored.Specs {
		if spec.Name == "bf16-vllm-nvidia" && !strings.HasPrefix(spec.ImageSources, `["swr.cn-southwest-2.myhuaweicloud.com/`) {
			t.Fatalf("cloned column = %q", spec.ImageSources)
		}
	}
}

// An import with an unusable source is refused with the field named, and
// nothing is stored.
func TestInferenceTemplateImportRefusesInvalidImageSources(t *testing.T) {
	for name, tc := range map[string]struct{ sources, wantError string }{
		"url":        {"    - https://files.example/runner.tar\n", "not a URL"},
		"duplicate":  {"    - ghcr.nju.edu.cn/org/runner:v1\n    - ghcr.nju.edu.cn/org/runner:v1\n", "listed twice"},
		"whitespace": {"    - 'ghcr.nju.edu.cn/org/runner :v1'\n", "contains whitespace"},
		"blank":      {"    - ''\n", "is empty"},
	} {
		t.Run(name, func(t *testing.T) {
			st, err := store.Open("file:api_template_image_sources_invalid_"+name+"?mode=memory&cache=shared", bus.New())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = st.Close() })
			h := &apiHandlers{d: Deps{Store: st}}
			document := strings.Replace(templateImageSourcesYAML, "    image_sources:\n    - swr.cn-southwest-2.myhuaweicloud.com/gpuworks/runner@sha256:22789f0c1a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d5e6f708192a3b4c5\n    - ghcr.nju.edu.cn/org/runner:v0.6.0\n", "    image_sources:\n"+tc.sources, 1)
			rec := httptest.NewRecorder()
			h.importInferenceTemplates(rec, httptest.NewRequest(http.MethodPost, "/api/v1/inference-templates/import", strings.NewReader(document)))
			if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), tc.wantError) || !strings.Contains(rec.Body.String(), "image_sources[") {
				t.Fatalf("import = %d: %s", rec.Code, rec.Body.String())
			}
			if _, err := st.InferenceModelByModelID("acme/sources"); err == nil {
				t.Fatal("a refused import stored the model")
			}
		})
	}
}
