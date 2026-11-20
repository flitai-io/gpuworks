package seed

import (
	"testing"

	"gpuworks.ai/gpuworks/internal/server/inferenceapi"
)

func TestQwen3Reranker8BContract(t *testing.T) {
	catalog, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}
	for _, model := range catalog.Models {
		if model.ModelID != "qwen/qwen3-reranker-8b" {
			continue
		}
		if len(model.Specs) == 0 {
			t.Fatal("Qwen3-Reranker-8B has no deployable specs")
		}
		for _, spec := range model.Specs {
			if spec.API == nil || len(spec.API.Operations) != 1 {
				t.Errorf("%s: expected exactly one rerank operation", spec.Name)
				continue
			}
			if spec.Engine == "sglang" && !containsArgumentPair(spec.Args, "--chat-template", "/sgl-workspace/sglang/examples/chat_template/qwen3_reranker.jinja") {
				t.Errorf("%s: expected the SGLang template that accepts query and document as positional messages", spec.Name)
			}
			op := spec.API.Operations[0]
			if op.Type != inferenceapi.OperationRerank || op.Endpoint != "/v1/rerank" || op.Mode != "sync" {
				t.Errorf("%s: expected synchronous openai.rerank at /v1/rerank, got type=%q endpoint=%q mode=%q", spec.Name, op.Type, op.Endpoint, op.Mode)
			}
		}
		return
	}
	t.Fatal("Qwen3-Reranker-8B is missing from the built-in catalog")
}
