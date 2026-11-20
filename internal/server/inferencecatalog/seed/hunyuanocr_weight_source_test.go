package seed

import "testing"

// ModelScope's Tencent-Hunyuan/HunyuanOCR still publishes the first-release
// layout, whose tokenizer_config.json has no image_token mapping. The vLLM
// HunYuanVLProcessor refuses to start on it ("Tokenizer is missing required
// attribute 'image_token'"), so the specs may only list the Hugging Face
// repository, whose root carries the current tokenizer.
func TestHunyuanOCRDoesNotOfferTheStaleModelScopeSnapshot(t *testing.T) {
	catalog, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}
	for _, model := range catalog.Models {
		if model.ModelID != "tencent/hunyuanocr" {
			continue
		}
		if len(model.Specs) == 0 {
			t.Fatal("HunyuanOCR has no specs")
		}
		for _, spec := range model.Specs {
			hasHuggingFace := false
			for _, source := range spec.Weights.Sources {
				switch source.Provider {
				case "modelscope":
					t.Errorf("%s lists ModelScope source %s@%s, which lacks the image_token tokenizer mapping", spec.Name, source.RepoID, source.Revision)
				case "huggingface":
					hasHuggingFace = source.RepoID == "tencent/HunyuanOCR"
				}
			}
			if !hasHuggingFace {
				t.Errorf("%s lost its Hugging Face tencent/HunyuanOCR source", spec.Name)
			}
		}
		return
	}
	t.Fatal("HunyuanOCR model is missing")
}
