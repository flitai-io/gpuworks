package inferencecatalog

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"gpuworks.ai/gpuworks/pkg/modelconfig"
)

func flatFixture() FlatCatalog {
	return FlatCatalog{Models: []InferenceModel{{
		ModelID: "Org/Model", DisplayTitle: "Model", ParameterCount: "70B", ReleaseDate: "2026-08-13",
		// The estimation facts travel to the estimator as JSON through the two
		// store rows, so the fixture carries every shape the block can hold.
		ModelConfig: &modelconfig.Config{
			ModelType: "qwen3", NumHiddenLayers: 64, HiddenSize: 5120,
			NumAttentionHeads: 64, NumKeyValueHeads: 8, HeadDim: 128,
			MaxPositionEmbeddings: 40960, SlidingWindow: 1024,
			LayerTypes:   []string{"full_attention", "linear_attention"},
			VisionConfig: &modelconfig.TowerConfig{ImageSize: 896, PatchSize: 14},
		},
		Specs: []InferenceSpec{{Name: "bf16-balanced", DisplayNameZHCN: " Ampere 及以上 · BF16 ", DisplayNameEN: "Ampere+ · BF16", Scenario: "balanced", Engine: "vLLM", Image: "vllm/vllm-openai:latest",
			Weights:     Weights{Quantization: "BF16", Sources: []WeightSource{{Provider: "modelscope", RepoID: "Org/Model"}, {Provider: "huggingface", RepoID: "Org/Model"}}},
			WeightBytes: 140_000_000_000, RuntimeWeightBytes: 70_000_000_000,
			GPUs: GPUSelector{Vendor: "NVIDIA", CC: ">=8.9"}, Args: []string{"--max-model-len", "32768"},
			Env: map[string]string{"VLLM_TEST": "1"}, PreStartArgv: []string{"apply", "--patch"},
		}},
	}}}
}

func TestFlatCatalogYAMLParseAndCompactSourceRoundTrip(t *testing.T) {
	raw := []byte(`models:
  - model_id: Org/Model
    display_title: Model
    release_date: "2026-08-13"
    specs:
      - name: bf16-balanced
        display_name_zh_cn: Ada 及以上 · BF16
        display_name_en: Ada+ · BF16
        scenario: balanced
        engine: vllm
        image: vllm/vllm-openai:latest
        weights:
          quantization: bf16
          sources:
            - modelscope: Org/Model
            - huggingface:
                repo_id: Org/Model
                revision: 0123456789abcdef
                subfolder: FL2VA
        gpus: {vendor: nvidia, cc: ">=8.9"}
`)
	catalog, err := ParseYAML(raw)
	if err != nil {
		t.Fatal(err)
	}
	if catalog.Models[0].ModelID != "org/model" || catalog.Models[0].Specs[0].Engine != "vllm" {
		t.Fatalf("normalization failed: %+v", catalog.Models[0])
	}
	if got := catalog.Models[0].Specs[0].Weights.Sources[0]; got.Provider != "modelscope" || got.RepoID != "Org/Model" {
		t.Fatalf("weight source failed: %+v", got)
	}
	if got := catalog.Models[0].Specs[0].Weights.Sources[1]; got.Provider != "huggingface" || got.RepoID != "Org/Model" || got.Revision != "0123456789abcdef" || got.Subfolder != "FL2VA" {
		t.Fatalf("revisioned weight source failed: %+v", got)
	}
	encoded, err := yaml.Marshal(catalog)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseYAML(encoded); err != nil {
		t.Fatalf("marshaled catalog did not parse: %v\n%s", err, encoded)
	}
	if !strings.Contains(string(encoded), "revision: 0123456789abcdef") || !strings.Contains(string(encoded), "subfolder: FL2VA") {
		t.Fatalf("revision or subfolder disappeared from compact YAML:\n%s", encoded)
	}
	if !strings.Contains(string(encoded), "display_name_zh_cn: Ada 及以上 · BF16") || !strings.Contains(string(encoded), "display_name_en: Ada+ · BF16") {
		t.Fatalf("display names disappeared from YAML:\n%s", encoded)
	}
}

func TestFlatCatalogRejectsUnknownRevisionedWeightSourceField(t *testing.T) {
	raw := []byte("models:\n- model_id: org/model\n  kinds: [text]\n  specs:\n  - name: default\n    engine: vllm\n    image: image:tag\n    weights:\n      quantization: bf16\n      sources:\n      - huggingface: {repo_id: org/model, branch: main}\n    gpus: {vendor: nvidia}\n")
	if _, err := ParseYAML(raw); err == nil || !strings.Contains(err.Error(), "unknown weight source field") {
		t.Fatalf("unknown nested field error = %v", err)
	}
}

func TestFlatCatalogValidationRejectsRetiredAndUnsafeShapes(t *testing.T) {
	cases := []struct {
		name string
		edit func(*FlatCatalog)
	}{
		{"duplicate spec", func(c *FlatCatalog) { c.Models[0].Specs = append(c.Models[0].Specs, c.Models[0].Specs[0]) }},
		{"invalid cc", func(c *FlatCatalog) { c.Models[0].Specs[0].GPUs.CC = "sm90" }},
		{"multi-line display name", func(c *FlatCatalog) { c.Models[0].Specs[0].DisplayNameEN = "Ampere+\nBF16" }},
		{"overlong zh display name", func(c *FlatCatalog) { c.Models[0].Specs[0].DisplayNameZHCN = strings.Repeat("卡", 57) }},
		{"overlong en display name", func(c *FlatCatalog) { c.Models[0].Specs[0].DisplayNameEN = strings.Repeat("x", 73) }},
		{"blank architecture ID", func(c *FlatCatalog) { c.Models[0].Specs[0].GPUs.ArchID = "   " }},
		{"blank unsupported architecture ID", func(c *FlatCatalog) {
			c.Models[0].Unsupported = []UnsupportedGPU{{GPUs: GPUSelector{Vendor: "amd", ArchID: " \t"}, Reason: "unsupported"}}
		}},
		{"missing weight source", func(c *FlatCatalog) { c.Models[0].Specs[0].Weights.Sources = nil }},
		{"negative runtime weight", func(c *FlatCatalog) { c.Models[0].Specs[0].RuntimeWeightBytes = -1 }},
		{"runtime weight exceeds storage", func(c *FlatCatalog) { c.Models[0].Specs[0].RuntimeWeightBytes = c.Models[0].Specs[0].WeightBytes + 1 }},
		{"unsafe weight subfolder", func(c *FlatCatalog) { c.Models[0].Specs[0].Weights.Sources[0].Subfolder = "../escape" }},
		{"both pre start forms", func(c *FlatCatalog) { c.Models[0].Specs[0].PreStartScript = "echo hi" }},
		{"oversized script", func(c *FlatCatalog) {
			c.Models[0].Specs[0].PreStartArgv = nil
			c.Models[0].Specs[0].PreStartScript = string(make([]byte, 64*1024+1))
		}},
		{"unsupported without reason", func(c *FlatCatalog) {
			c.Models[0].Unsupported = []UnsupportedGPU{{GPUs: GPUSelector{Vendor: "nvidia"}}}
		}},
		// A shipped spec names weights every installation can reach. An HTTP
		// directory is one operator's private mirror, so a spec that declared
		// one would be unusable everywhere else; such weights reach the
		// platform by registering the directory as a model instead.
		{"private http directory as a weight source", func(c *FlatCatalog) {
			c.Models[0].Specs[0].Weights.Sources[0].Provider = "url"
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			catalog := flatFixture()
			tc.edit(&catalog)
			if err := ValidateFlatCatalog(&catalog); err == nil {
				t.Fatal("invalid catalog was accepted")
			}
		})
	}
	for _, expression := range []string{"8.9", ">=8.9", "<8.9", "8.0..8.9"} {
		catalog := flatFixture()
		catalog.Models[0].Specs[0].GPUs.CC = expression
		if err := ValidateFlatCatalog(&catalog); err != nil {
			t.Fatalf("valid cc expression %q rejected: %v", expression, err)
		}
	}
	catalog := flatFixture()
	catalog.Models[0].Specs[0].GPUs.CC = "8.9..8.0"
	if err := ValidateFlatCatalog(&catalog); err == nil {
		t.Fatal("reversed cc range was accepted")
	}
	if _, err := ParseYAML([]byte("models:\n- model_id: org/m\n  retired_profiles: []\n")); err == nil {
		t.Fatal("retired field was accepted")
	}
}

func TestFlatCatalogPreStartScriptLimitUsesBytes(t *testing.T) {
	catalog := flatFixture()
	catalog.Models[0].Specs[0].PreStartArgv = nil
	catalog.Models[0].Specs[0].PreStartScript = string(make([]byte, 64*1024))
	if err := ValidateFlatCatalog(&catalog); err != nil {
		t.Fatal(err)
	}
}

func TestFlatCatalogConvertsToTwoStoreRows(t *testing.T) {
	rows, err := ToFlatStoreModels(flatFixture())
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || len(rows[0].Specs) != 1 {
		t.Fatalf("rows = %+v", rows)
	}
	if rows[0].ModelID != "org/model" || rows[0].Specs[0].ModelID != 0 || rows[0].Specs[0].Engine != "vllm" || rows[0].Specs[0].DisplayNameZHCN != "Ampere 及以上 · BF16" || rows[0].Specs[0].DisplayNameEN != "Ampere+ · BF16" {
		t.Fatalf("unexpected row identity: %+v", rows[0])
	}
	if rows[0].Specs[0].WeightSources == "" || rows[0].Specs[0].GPUs == "" || rows[0].Specs[0].WeightBytes != 140_000_000_000 || rows[0].Specs[0].RuntimeWeightBytes != 70_000_000_000 {
		t.Fatal("nested fields were not persisted as JSON")
	}
	// The projection is JSON in config.json's own field names, not Go field names.
	if !strings.Contains(rows[0].ModelConfig, `"num_hidden_layers":64`) || !strings.Contains(rows[0].ModelConfig, `"vision_config"`) {
		t.Fatalf("model_config was not persisted in config.json field names: %q", rows[0].ModelConfig)
	}
}

func TestFlatCatalogStoreProjectionCanBecomePortableAgain(t *testing.T) {
	rows, err := ToFlatStoreModels(flatFixture())
	if err != nil {
		t.Fatal(err)
	}
	restored, err := FromFlatStoreModel(rows[0])
	if err != nil {
		t.Fatal(err)
	}
	if restored.ModelID != "org/model" || len(restored.Specs) != 1 {
		t.Fatalf("restored model = %+v", restored)
	}
	spec := restored.Specs[0]
	if spec.DisplayNameZHCN != "Ampere 及以上 · BF16" || spec.DisplayNameEN != "Ampere+ · BF16" || len(spec.Weights.Sources) != 2 || spec.Env["VLLM_TEST"] != "1" || len(spec.PreStartArgv) != 2 {
		t.Fatalf("restored spec = %+v", spec)
	}
	if restored.ModelConfig == nil || restored.ModelConfig.NumHiddenLayers != 64 || restored.ModelConfig.VisionConfig == nil ||
		len(restored.ModelConfig.LayerTypes) != 2 || restored.ModelConfig.SlidingWindow != 1024 {
		t.Fatalf("restored model_config = %+v", restored.ModelConfig)
	}
	if err := ValidateFlatCatalog(&FlatCatalog{Models: []InferenceModel{restored}}); err != nil {
		t.Fatalf("restored catalog is not portable: %v", err)
	}

	rows[0].Specs[0].Placement = `{broken`
	if _, err := FromFlatStoreModel(rows[0]); err == nil || !strings.Contains(err.Error(), "placement") {
		t.Fatalf("corrupt projection error = %v", err)
	}
}

func TestGPUSelectorArchIDRoundTrip(t *testing.T) {
	catalog := flatFixture()
	catalog.Models[0].Specs[0].GPUs = GPUSelector{Vendor: "amd", Name: `Radeon 8060S`, ArchID: " gfx1151 "}
	if err := ValidateFlatCatalog(&catalog); err != nil {
		t.Fatal(err)
	}
	if got := catalog.Models[0].Specs[0].GPUs.ArchID; got != "gfx1151" {
		t.Fatalf("arch_id was not trimmed: %q", got)
	}
	encoded, err := yaml.Marshal(catalog)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := ParseYAML(encoded)
	if err != nil {
		t.Fatalf("round-trip catalog did not parse: %v", err)
	}
	if got := decoded.Models[0].Specs[0].GPUs.ArchID; got != "gfx1151" {
		t.Fatalf("round-trip lost arch_id: %q", got)
	}
}
