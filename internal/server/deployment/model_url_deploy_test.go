package deployment

import (
	"testing"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// A directory model is matched by its repository id alone, which is what makes
// an inference-catalog preset apply to weights served from a private mirror.
func TestTemplateMatchingFindsADirectoryModelByRepositoryID(t *testing.T) {
	st, err := store.Open("file:api_template_match_url_model?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	model, err := st.UpsertModel(&store.Model{
		Source: "url", SourceID: "Qwen/Qwen3-0.6B", SourceURL: "https://hf-mirror.example.com/Qwen/Qwen3-0.6B", Name: "Qwen3-0.6B",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.ReconcileInferenceCatalog([]store.InferenceModel{{
		ModelID: "qwen/qwen3-0.6b", DisplayTitle: "Qwen3 0.6B", ReleaseDate: "2025-04-03", Source: "upstream", Unsupported: `[]`,
		Specs: []store.InferenceSpecRow{{
			Name: "qwen3-0.6b-vllm", Scenario: "balanced", Engine: "vllm", Image: "vllm/vllm-openai:v0.11.0", Quantization: "bf16",
			WeightSources: `[{"provider":"huggingface","repo_id":"Qwen/Qwen3-0.6B"}]`,
			GPUs:          `{"vendor":"nvidia","cc":">=7.5"}`, Args: `[]`, Env: `{}`, PreStartArgv: `[]`,
		}},
	}}); err != nil {
		t.Fatal(err)
	}

	// The template's weight source names a Hugging Face repository; the model
	// is served from a mirror. They are the same weights, and the matcher must
	// say so.
	if !TemplateWeightMatchesModel(weightSource{Provider: "huggingface", RepoID: "Qwen/Qwen3-0.6B"}, model) {
		t.Fatal("a directory model was not matched by its repository id")
	}
	if TemplateWeightMatchesModel(weightSource{Provider: "huggingface", RepoID: "Qwen/Qwen3-4B"}, model) {
		t.Fatal("a different repository id was matched")
	}
	entry, err := st.InferenceModelByModelID(model.SourceID)
	if err != nil || entry == nil || entry.ModelID != "qwen/qwen3-0.6b" {
		t.Fatalf("catalog lookup by source id = %+v err = %v", entry, err)
	}
}
