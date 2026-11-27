package api

import (
	"encoding/json"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// A rank-scoped deployment's snapshot and catalog keep the template's
// subfolder ("rank${NODE_RANK}") while its model is rank 0's artifact
// ("rank0", ADR 0076). The tokenizer the template declares is still that
// deployment's, from the frozen snapshot and from the catalog fallback alike;
// another subfolder of the repository is still not.
func TestRankScopedDeploymentKeepsItsDeclaredTokenizer(t *testing.T) {
	st, err := store.Open("file:api_rank_scoped_tokenizer?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	const ref = "ms://Acme/Tokenizer@2741eec155d03a8ce151b993ccce1a7b1e398d6b"
	contract := `{"tokenizer_ref":"` + ref + `","operations":[{"type":"openai.chat"}]}`
	if err := st.ReconcileInferenceCatalog([]store.InferenceModel{{ModelID: "acme/ranked", Source: "upstream", Specs: []store.InferenceSpecRow{{
		Name: "tp2", Engine: "vllm", Image: "vllm/vllm-openai:latest", Quantization: "bf16",
		WeightSources: `[{"provider":"modelscope","repo_id":"Acme/Ranked-TP2","revision":"r1","subfolder":"rank${NODE_RANK}"}]`,
		GPUs:          `{"vendor":"nvidia"}`, APIContract: contract,
	}}}}); err != nil {
		t.Fatal(err)
	}
	template, err := st.InferenceModelByModelID("acme/ranked")
	if err != nil {
		t.Fatal(err)
	}
	spec := template.Specs[0]
	rank0 := &store.Model{ID: 5, Source: "modelscope", SourceID: "Acme/Ranked-TP2", Revision: "r1", ArtifactSubfolder: "rank0"}
	deployment := func(apiContract string) *store.Deployment {
		snapshot, err := deploy.CatalogSelection{
			TemplateID: template.ID, ModelID: template.ModelID, SpecSource: spec.Source, SpecName: spec.Name, Engine: spec.Engine, Image: spec.Image,
			WeightProvider: "modelscope", WeightRepoID: "Acme/Ranked-TP2", WeightRevision: "r1", WeightSubfolder: "rank${NODE_RANK}",
			APIContract: json.RawMessage(apiContract),
		}.Encode()
		if err != nil {
			t.Fatal(err)
		}
		return &store.Deployment{TemplateID: template.ID, TemplateSpecSource: spec.Source, TemplateSpecName: spec.Name, Engine: spec.Engine, Image: spec.Image, CatalogSelectionSnapshot: string(snapshot)}
	}
	h := &apiHandlers{d: Deps{Store: st}}
	if got := h.declaredBenchmarkTokenizer(deployment(contract), rank0); got != ref {
		t.Fatalf("frozen declaration = %q, want %q", got, ref)
	}
	if got := h.declaredBenchmarkTokenizer(deployment(`{"operations":[{"type":"openai.chat"}]}`), rank0); got != ref {
		t.Fatalf("catalog declaration = %q, want %q", got, ref)
	}
	rank1 := *rank0
	rank1.ArtifactSubfolder = "rank1"
	if got := h.declaredBenchmarkTokenizer(deployment(contract), &rank1); got != "" {
		t.Fatalf("rank 1's artifact as the deployment's model took the declaration %q", got)
	}
}
