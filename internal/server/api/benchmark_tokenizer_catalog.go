package api

import (
	"encoding/json"
	"strings"

	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/deployment"
	"gpuworks.ai/gpuworks/internal/server/inferenceapi"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// declaredBenchmarkTokenizer reads the explicit tokenizer/weight association.
// A newer declaration can fill an old snapshot's missing annotation only when
// the selected spec still describes precisely the artifact actually mounted.
// Neither a model name nor a README base_model is a tokenizer identity.
func (a *apiHandlers) declaredBenchmarkTokenizer(dep *store.Deployment, model *store.Model) string {
	if dep == nil || model == nil || dep.TemplateID == 0 || dep.TemplateDeviated {
		return ""
	}
	selected, err := deploy.DecodeCatalogSelection([]byte(dep.CatalogSelectionSnapshot))
	if err != nil || selected.TemplateID != dep.TemplateID || selected.ModelID == "" ||
		selected.SpecSource != dep.TemplateSpecSource || selected.SpecName != dep.TemplateSpecName ||
		selected.Engine != dep.Engine || selected.Image != dep.Image ||
		selected.WeightProvider != model.Source || selected.WeightRepoID != model.SourceID ||
		selected.WeightRevision != model.Revision || !subfolderNamesModel(selected.WeightSubfolder, model) {
		return ""
	}
	var frozen inferenceapi.Contract
	if json.Unmarshal(selected.APIContract, &frozen) != nil {
		return ""
	}
	if strings.TrimSpace(frozen.TokenizerRef) != "" {
		if normalized, err := inferenceapi.NormalizeContract(frozen); err == nil {
			return normalized.TokenizerRef
		}
		return ""
	}
	catalog, err := a.d.Store.InferenceModelByID(dep.TemplateID)
	if err != nil || catalog.ModelID != selected.ModelID {
		return ""
	}
	for _, spec := range catalog.Specs {
		if spec.Source != selected.SpecSource || spec.Name != selected.SpecName || spec.Engine != dep.Engine || spec.Image != dep.Image {
			continue
		}
		for _, weight := range deployment.SpecWeightSources(&spec) {
			if weight.Provider != model.Source || weight.RepoID != model.SourceID || weight.Revision != model.Revision || !subfolderNamesModel(weight.Subfolder, model) {
				continue
			}
			var declared inferenceapi.Contract
			if json.Unmarshal([]byte(spec.APIContract), &declared) == nil {
				if normalized, err := inferenceapi.NormalizeContract(declared); err == nil {
					return normalized.TokenizerRef
				}
			}
		}
	}
	return ""
}

// subfolderNamesModel reports whether a template subfolder is the deployment
// model's artifact. A rank-scoped subfolder stays the template's in the
// snapshot and the catalog, while the deployment's model is rank 0's
// artifact (ADR 0076), so it is compared as rank 0 reads it.
func subfolderNamesModel(subfolder string, model *store.Model) bool {
	return deploy.ExpandRankSubfolder(subfolder, 0) == model.ArtifactSubfolder
}
