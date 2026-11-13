package deployment

import (
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/chipmatch"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func flatMatchModel() store.InferenceModel {
	return store.InferenceModel{
		ModelID:     "org/model",
		Unsupported: `[{"gpus":{"vendor":"nvidia","cc":"<8.9"},"quantization":"fp8","reason":"FP8 requires Ada or newer"}]`,
		Specs: []store.InferenceSpecRow{
			{Name: "generic", Engine: "vllm", Scenario: "balanced", Quantization: "bf16", GPUs: `{"vendor":"nvidia","cc":">=7.5"}`},
			{Name: "ada-fp8", Engine: "vllm", Scenario: "balanced", Quantization: "fp8", GPUs: `{"vendor":"nvidia","cc":">=8.9"}`},
			{Name: "blackwell", Engine: "vllm", Scenario: "throughput", Quantization: "fp8", GPUs: `{"vendor":"nvidia","cc":"10.0..10.3"}`},
		},
	}
}

func TestSelectInferenceSpecUsesInlineSpecificity(t *testing.T) {
	model := flatMatchModel()
	spec, selection, err := selectInferenceSpec(model, "", "", chipmatch.GPU{Name: "NVIDIA B200", Vendor: "nvidia", CC: 10}, "vllm", "")
	if err != nil {
		t.Fatal(err)
	}
	if spec.Name != "blackwell" || !strings.Contains(selection.MatchedBy, "spec:blackwell") {
		t.Fatalf("spec=%+v selection=%+v", spec, selection)
	}

	spec, _, err = selectInferenceSpec(model, "", "ada-fp8", chipmatch.GPU{Name: "GeForce RTX 4090", Vendor: "nvidia", CC: 8.9}, "vllm", "balanced")
	if err != nil || spec.Name != "ada-fp8" {
		t.Fatalf("explicit spec=%+v err=%v", spec, err)
	}
}

func TestSelectInferenceSpecByNameDoesNotRequirePreviewHardware(t *testing.T) {
	spec, selection, err := selectInferenceSpec(flatMatchModel(), "", "ada-fp8", chipmatch.GPU{}, "vllm", "balanced")
	if err != nil || spec == nil || spec.Name != "ada-fp8" || !selection.Matched || selection.MatchedBy != "spec-name" {
		t.Fatalf("spec=%+v selection=%+v err=%v", spec, selection, err)
	}
}

func TestSelectInferenceSpecUnsupportedAllowsAlternativeQuantization(t *testing.T) {
	model := flatMatchModel()
	spec, _, err := selectInferenceSpec(model, "", "", chipmatch.GPU{Name: "NVIDIA V100", Vendor: "nvidia", CC: 7}, "vllm", "")
	if err == nil || spec != nil || !strings.Contains(err.Error(), "FP8 requires Ada or newer") {
		t.Fatalf("V100 result spec=%+v err=%v", spec, err)
	}
	spec, _, err = selectInferenceSpec(model, "", "", chipmatch.GPU{Name: "GeForce RTX 2080 Ti", Vendor: "nvidia", CC: 7.5}, "vllm", "")
	if err != nil || spec.Name != "generic" {
		t.Fatalf("2080 Ti result spec=%+v err=%v", spec, err)
	}
}

func TestSelectInferenceSpecRequiresNameForEqualPredicates(t *testing.T) {
	model := store.InferenceModel{
		ModelID: "org/model",
		Specs: []store.InferenceSpecRow{
			{Name: "bf16-original", Engine: "vllm", Scenario: "balanced", Quantization: "bf16", GPUs: `{"vendor":"nvidia","cc":">=8.0"}`},
			{Name: "bf16-republished", Engine: "vllm", Scenario: "balanced", Quantization: "bf16", GPUs: `{"vendor":"nvidia","cc":">=8.0"}`},
		},
	}

	spec, selection, err := selectInferenceSpec(model, "", "", chipmatch.GPU{Name: "NVIDIA H20", Vendor: "nvidia", CC: 9}, "vllm", "balanced")
	if err == nil || spec != nil || !selection.Ambiguous || !strings.Contains(err.Error(), "template_spec_name") {
		t.Fatalf("implicit result spec=%+v selection=%+v err=%v", spec, selection, err)
	}

	spec, selection, err = selectInferenceSpec(model, "", "bf16-republished", chipmatch.GPU{Name: "NVIDIA H20", Vendor: "nvidia", CC: 9}, "vllm", "balanced")
	if err != nil || spec == nil || spec.Name != "bf16-republished" || selection.Ambiguous {
		t.Fatalf("explicit result spec=%+v selection=%+v err=%v", spec, selection, err)
	}
}

func TestFlatSelectionDistinguishesSameNameByCatalogSource(t *testing.T) {
	model := store.InferenceModel{ModelID: "acme/source-identity", Specs: []store.InferenceSpecRow{
		{Source: "upstream", Name: "general", Engine: "vllm", Scenario: "balanced", Image: "upstream:image", Quantization: "bf16", GPUs: `{"vendor":"nvidia","cc":">=8.0"}`},
		{Source: "user", Name: "general", Engine: "vllm", Scenario: "balanced", Image: "user:image", Quantization: "int8", GPUs: `{"vendor":"nvidia","cc":">=8.0"}`},
	}}
	gpu := chipmatch.GPU{Name: "NVIDIA H20", Vendor: "nvidia", CC: 9}
	if spec, selection, err := selectInferenceSpec(model, "", "general", gpu, "vllm", "balanced"); err == nil || spec != nil || !selection.Ambiguous || !strings.Contains(err.Error(), "template_spec_source") {
		t.Fatalf("source-less duplicate selection spec=%+v selection=%+v err=%v", spec, selection, err)
	}
	spec, selection, err := selectInferenceSpec(model, "user", "general", gpu, "vllm", "balanced")
	if err != nil || spec == nil || spec.Source != "user" || spec.Image != "user:image" || selection.Ambiguous {
		t.Fatalf("explicit user selection spec=%+v selection=%+v err=%v", spec, selection, err)
	}
}
