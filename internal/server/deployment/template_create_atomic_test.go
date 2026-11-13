package deployment

import (
	"encoding/json"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestInitTemplateModelInferenceConfigBuildsATransientCandidate(t *testing.T) {
	selection := &templateSelection{
		Model:       &store.InferenceModel{InputModalities: `["text"]`},
		APIContract: json.RawMessage(`{"operations":[{"type":"openai.rerank","endpoint":"/v1/rerank"}]}`),
	}
	model := &store.Model{ID: 7, Operations: `[]`}
	candidate, config, err := (New(Deps{})).initTemplateModelInferenceConfig(selection, model)
	if err != nil {
		t.Fatal(err)
	}
	if candidate == model {
		t.Fatal("the candidate aliases the stored model")
	}
	if candidate.Operations != `[{"inputs":null,"type":"openai.rerank","endpoint":"/v1/rerank"}]` || candidate.InputModalities != `["text"]` {
		t.Fatalf("candidate = %+v", candidate)
	}
	if model.Operations != `[]` || model.InputModalities != "" {
		t.Fatalf("the source model was mutated: %+v", model)
	}
	if config.modelID != model.ID || config.operations != candidate.Operations || config.inputModalities != `["text"]` {
		t.Fatalf("declaration = %+v", config)
	}
}
