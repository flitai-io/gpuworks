package deployment

import (
	"encoding/json"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestURLModelOffersNoTemplateProviders(t *testing.T) {
	h := New(Deps{})
	selection := templateSelection{Spec: &store.InferenceSpecRow{
		WeightSources: `[{"provider":"huggingface","repo_id":"Acme/Model"},{"provider":"modelscope","repo_id":"Acme/Model"}]`,
	}}
	facts, err := h.evaluateWeightFacts(&store.Model{Source: "url", SourceID: "https://models.example/weights"}, selection, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(facts.Providers) != 0 {
		t.Fatalf("URL registration offered template providers: %+v", facts.Providers)
	}
}

func jsonUint(id uint) string {
	encoded, _ := json.Marshal(id)
	return string(encoded)
}
