package seed

import (
	"slices"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/inferenceapi"
)

// MinerU is a remote VLM consumer. A generic document-parser/callable recipe
// would expose the wrong API and leave model acquisition outside the platform.
func TestMinerURemoteVisionRecipe(t *testing.T) {
	catalog, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}
	for _, model := range catalog.Models {
		if model.ModelID != "opendatalab/mineru2.5-2509-1.2b" {
			continue
		}
		if len(model.Specs) == 0 {
			t.Fatal("MinerU has no deployable recipe")
		}
		for _, spec := range model.Specs {
			if spec.Engine != "vllm" || spec.API == nil {
				t.Fatalf("MinerU must expose its VLM through the managed vLLM runtime")
			}
			if len(spec.Weights.Sources) == 0 || spec.WeightBytes <= 0 {
				t.Fatal("MinerU weights must be acquired and accounted for separately from the image")
			}
			for _, source := range spec.Weights.Sources {
				if source.RepoID != "opendatalab/MinerU2.5-2509-1.2B" || source.Revision == "" {
					t.Fatalf("unexpected unpinned model source: %+v", source)
				}
			}
			if !slices.Contains(spec.Args, "mineru_logits_processor:VllmV1NoRepeatNGramLogitsProcessor") {
				t.Fatal("MinerU no-repeat request arguments would be ignored without the logits processor")
			}
			if len(spec.Assets) != 1 || spec.Assets[0].UnpackTo != spec.Env["PYTHONPATH"] {
				t.Fatal("logits processor must be shipped as a validated offline Python module")
			}
			if spec.Env["VLLM_USE_V2_MODEL_RUNNER"] != "0" {
				t.Fatal("the vendored MinerU processor uses the V1 interface; vLLM's default V2 loader rejects it")
			}
			if len(spec.API.Operations) != 1 || spec.API.Operations[0].Type != inferenceapi.OperationChat || spec.API.Operations[0].Endpoint != "/v1/chat/completions" {
				t.Fatal("MinerU remote client requires the existing chat-completions gateway")
			}
		}
		return
	}
	t.Fatal("MinerU remote vision model is missing from the built-in catalog")
}
