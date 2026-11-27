package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// GWKS-516: each catalog image option says which engine and version it runs.
// The engine's own images come first; another engine's image comes last and
// says it selects that engine. A vllm-omni build is an image family, not an
// engine a draft can switch to, so the list does not offer it.
func TestEvaluateImageOptionsStateTheirEngine(t *testing.T) {
	st, err := store.Open("file:api_image_options_engine?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	model, err := st.UpsertModel(&store.Model{Source: "modelscope", SourceID: "org/qwen", Revision: "master", Name: "Qwen", WeightBytes: 1 << 30})
	if err != nil {
		t.Fatal(err)
	}
	for _, image := range []store.Image{
		{ImageRef: "catalog/omni:v1", Engine: "vllm-omni", EngineVersion: "0.34.0"},
		{ImageRef: "catalog/sglang:v1", Engine: "sglang", EngineVersion: "0.5.21"},
		{ImageRef: "catalog/vllm:v1", Engine: "vllm", EngineVersion: "0.30.0"},
	} {
		image.Framework, image.Source, image.Enabled = "cann", "official", true
		if _, err := st.UpsertImage(&image); err != nil {
			t.Fatal(err)
		}
	}
	options := func(extra string) string {
		t.Helper()
		body := fmt.Sprintf(`{"model_id":%d,"target_vendor":"ascend"%s}`, model.ID, extra)
		rec := httptest.NewRecorder()
		(&apiHandlers{d: Deps{Store: st}}).evaluate(rec, httptest.NewRequest(http.MethodPost, "/api/v1/evaluate", strings.NewReader(body)))
		if rec.Code != http.StatusOK {
			t.Fatalf("status/body = %d %s", rec.Code, rec.Body.String())
		}
		var got struct {
			Acquisition struct {
				Image struct {
					Options json.RawMessage `json:"options"`
				} `json:"image"`
			} `json:"acquisition"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		return string(got.Acquisition.Image.Options)
	}
	const vllm = `{"image":"catalog/vllm:v1","reason":"catalog_match","compatible":true,"engine":"vllm","engine_version":"0.30.0"`
	const sglang = `{"image":"catalog/sglang:v1","reason":"catalog_match","compatible":true,"engine":"sglang","engine_version":"0.5.21"`
	if got, want := options(`,"engine":"vllm"`), "["+vllm+"},"+sglang+`,"selects_engine":"sglang"}]`; got != want {
		t.Fatalf("options for vllm = %s, want %s", got, want)
	}
	if got, want := options(`,"engine":"sglang"`), "["+sglang+"},"+vllm+`,"selects_engine":"vllm"}]`; got != want {
		t.Fatalf("options for sglang = %s, want %s", got, want)
	}
	if got, want := options(""), options(`,"engine":"vllm"`); got != want {
		t.Fatalf("options without an engine = %s, want the default engine's %s", got, want)
	}
	// A template holds the list to its engine: its own image, nothing that
	// would switch the engine.
	if err := st.ReconcileInferenceCatalog([]store.InferenceModel{{
		ModelID: "org/qwen", DisplayTitle: "Qwen", Source: "upstream", Architecture: "qwen3", ContextLength: 32768,
		Specs: []store.InferenceSpecRow{{
			Name: "ascend-sglang", Scenario: "balanced", Engine: "sglang", Image: "catalog/sglang:v1", Quantization: "bf16", WeightBytes: 1 << 30,
			WeightSources: `[{"provider":"modelscope","repo_id":"org/qwen"}]`, GPUs: `{"vendor":"ascend"}`,
		}},
	}}); err != nil {
		t.Fatal(err)
	}
	template, err := st.InferenceModelByModelID("org/qwen")
	if err != nil {
		t.Fatal(err)
	}
	withTemplate := options(fmt.Sprintf(`,"template_id":%d,"template_spec_source":"upstream","template_spec_name":"ascend-sglang"`, template.ID))
	if want := `[{"image":"catalog/sglang:v1","reason":"template_image","compatible":true,"engine":"sglang","engine_version":"0.5.21"}]`; withTemplate != want {
		t.Fatalf("options with a template = %s, want %s", withTemplate, want)
	}
}
