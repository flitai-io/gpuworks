package deployment

import (
	"encoding/json"
	"fmt"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/inferenceapi"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// A spec without operations has no engine API to put in place of the inferred
// placeholder, so the deployment keeps the model's own normalized contract.
func TestInferredVideoContractSurvivesSpecWithoutOperations(t *testing.T) {
	for _, specContract := range []string{`{"health_endpoint":"/health","operations":[]}`, ``} {
		t.Run(fmt.Sprintf("spec=%q", specContract), func(t *testing.T) {
			selection := &templateSelection{Spec: &store.InferenceSpecRow{Name: "video"}, APIContract: json.RawMessage(specContract)}
			applyCurrentModelAPIContract(selection, &store.Model{Operations: `[{"inputs":null,"type":"openai.video.generate"}]`})
			var got inferenceapi.Contract
			if err := json.Unmarshal(selection.APIContract, &got); err != nil {
				t.Fatalf("contract %s: %v", selection.APIContract, err)
			}
			if len(got.Operations) != 1 || got.Operations[0].Type != "openai.video.generate" || got.Operations[0].Endpoint == "" {
				t.Fatalf("deployment lost the model's video contract: %+v", got.Operations)
			}
		})
	}
	// A spec that declares the API still supplies it over the placeholder.
	declared := `{"operations":[{"type":"openai.video.generate","endpoint":"/v1/videos/sync","mode":"sync"}]}`
	selection := &templateSelection{Spec: &store.InferenceSpecRow{Name: "video"}, APIContract: json.RawMessage(declared)}
	applyCurrentModelAPIContract(selection, &store.Model{Operations: `[{"inputs":null,"type":"openai.video.generate"}]`})
	if string(selection.APIContract) != declared {
		t.Fatalf("placeholder replaced the declared template contract: %s", selection.APIContract)
	}
}
