package api

import (
	"reflect"
	"sort"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestInferenceTemplateViewProjectsKeysLeftToTheEngine(t *testing.T) {
	model := store.InferenceModel{
		ModelID: "acme/entrypoint-matrix",
		Source:  "upstream",
		Specs: []store.InferenceSpecRow{
			{Name: "sglang-llm-catalog-on", Source: "upstream", Engine: deploy.EngineSGLang, Args: `["--enable-metrics"]`},
			{Name: "sglang-llm-catalog-silent", Source: "upstream", Engine: deploy.EngineSGLang, Args: `[]`},
			{Name: "sglang-diffusion-catalog-silent", Source: "upstream", Engine: deploy.EngineSGLang, Entrypoint: `["sglang","serve"]`, Args: `["--model-type","diffusion"]`},
			{Name: "sglang-diffusion-catalog-on", Source: "upstream", Engine: deploy.EngineSGLang, Entrypoint: `["sglang","serve"]`, Args: `["--model-type=diffusion","--enable-metrics"]`},
			{Name: "sglang-diffusion-v0.5.21", Source: "upstream", Engine: deploy.EngineSGLang, Image: "gpuworks/gpuworks-runner:cuda13.0-sglang0.5.21-r2", Entrypoint: `["sglang","serve"]`, Args: `["--model-type","diffusion"]`},
			{Name: "vllm", Source: "upstream", Engine: deploy.EngineVLLM, Entrypoint: `["vllm","serve"]`},
			{Name: "vllm-v0.30", Source: "upstream", Engine: deploy.EngineVLLM, Image: "gpuworks/gpuworks-runner:cuda13.0-vllm0.30.0-r2", Entrypoint: `["vllm","serve"]`},
		},
	}
	view := inferenceTemplateView(model)
	// The spec's image tag names the release; without one a diffusion spec
	// leaves the switches only newer diffusion servers take to the engine,
	// and vLLM native context fitting plus per-request metrics.
	llmOnly := []string{"dtype", "enable_auto_tool_choice", "enforce_eager", "gpu_memory_utilization", "kv_cache_dtype", "lora"}
	diffusionUnknown := append([]string{"enable_cache_report", "enable_metrics"}, llmOnly...)
	sort.Strings(diffusionUnknown)
	want := map[string][]string{
		"sglang-llm-catalog-on":           nil,
		"sglang-llm-catalog-silent":       nil,
		"sglang-diffusion-catalog-silent": diffusionUnknown,
		"sglang-diffusion-catalog-on":     diffusionUnknown,
		"sglang-diffusion-v0.5.21":        llmOnly,
		"vllm":                            {"enable_request_metrics", "max_model_len"},
		"vllm-v0.30":                      nil,
	}
	if len(view.Specs) != len(want) {
		t.Fatalf("spec count=%d, want %d", len(view.Specs), len(want))
	}
	for _, spec := range view.Specs {
		if !reflect.DeepEqual(spec.EngineDecidesParamKeys, want[spec.Name]) {
			t.Fatalf("spec %s engine_decides_param_keys=%v, want %v", spec.Name, spec.EngineDecidesParamKeys, want[spec.Name])
		}
	}
}
