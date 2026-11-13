package deployment

import (
	"encoding/json"
	"fmt"
	"strings"

	"gpuworks.ai/gpuworks/internal/server/acquisition"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/inferencecatalog"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// TemplateWeightMatchesModel decides whether a registered model can serve a
// template weight source. The repository id is the identity: a provider is a
// mirror of the same repository and a revision is a point in its history, so
// neither may turn a model the user already registered into "the template
// needs a different model". A different repository id — including a separately
// published quantization such as Acme/X-Int8 — is a different model and never
// matches. The subfolder rule keeps the one asymmetry that is a fact about the
// bytes on disk: a complete repository contains the requested subfolder, but a
// subfolder-only copy does not contain the whole repository.
//
// The relaxation is safe because it only decides which registered row a
// deployment starts from; the resolved model's real provider and revision are
// then written back into the deployment snapshot, so what gets mounted is
// always described by the artifact identity rather than by the template's
// preference.
func TemplateWeightMatchesModel(source weightSource, model *store.Model) bool {
	// A local source names registered private bytes, not a public repository
	// that happens to have the same name. Public sources retain mirror reuse.
	if strings.EqualFold(strings.TrimSpace(source.Provider), "local") &&
		(model == nil || !strings.EqualFold(strings.TrimSpace(model.Source), "local")) {
		return false
	}
	return model != nil &&
		strings.EqualFold(strings.TrimSpace(model.SourceID), strings.TrimSpace(source.RepoID)) &&
		modelArtifactSatisfiesSubfolder(model.ArtifactSubfolder, source.Subfolder)
}

// templateWeightMatchesAnySource accepts a model that satisfies any declared
// weight source. A spec's providers may publish the same weights under
// different repository ids (hf nvidia/X-NVFP4 vs ms nv-community/X-NVFP4), so
// comparing only against the currently preferred source would report a
// conflict for a model the spec itself declares.
func templateWeightMatchesAnySource(sources []weightSource, model *store.Model) bool {
	for _, source := range sources {
		if TemplateWeightMatchesModel(source, model) {
			return true
		}
	}
	return false
}

// SpecWeightSources parses the sources a spec declares. A spec whose sources
// cannot be parsed yields none, which leaves the caller comparing against the
// already-selected weight only.
func SpecWeightSources(spec *store.InferenceSpecRow) []inferencecatalog.WeightSource {
	if spec == nil {
		return nil
	}
	sources, _ := inferencecatalog.DecodeWeightSources(spec.WeightSources)
	return sources
}

// WeightSourceRepoIDs lists the repository ids the sources name, the key a
// registered model is looked up by (see templateWeightMatchesModel).
func WeightSourceRepoIDs(sources []inferencecatalog.WeightSource) []string {
	ids := make([]string, 0, len(sources))
	for _, source := range sources {
		ids = append(ids, source.RepoID)
	}
	return ids
}

// specDeclaresProvider reports whether the spec lists a weight source at the
// given provider.
func specDeclaresProvider(spec *store.InferenceSpecRow, provider string) bool {
	provider = strings.TrimSpace(provider)
	for _, source := range SpecWeightSources(spec) {
		if strings.EqualFold(source.Provider, provider) {
			return true
		}
	}
	return false
}

func modelArtifactSatisfiesSubfolder(actual, requested string) bool {
	actual = strings.TrimSpace(actual)
	requested = strings.TrimSpace(requested)
	return actual == requested || (actual == "" && requested != "")
}

func (s *Service) createTemplateWeightModel(source weightSource, original *store.InferenceModel, engine, quantization string, weightBytes int64) (*store.Model, error) {
	return s.d.Store.UpsertModel(&store.Model{
		Source: source.Provider, SourceID: source.RepoID, Revision: source.Revision, ArtifactSubfolder: source.Subfolder, Name: source.RepoID,
		Arch: original.Architecture, QuantLabel: quantization, WeightBytes: weightBytes,
		ContextLength: original.ContextLength,
	})
}

func (s *Service) resolveWeightRevision(source weightSource, nodeID uint) (weightSource, error) {
	if strings.TrimSpace(source.Revision) != "" {
		return source, nil
	}
	model, err := s.d.Store.FindModelArtifactAnyRevision(source.RepoID, source.Provider, source.Subfolder, nodeID)
	if err == store.ErrNotFound {
		return source, nil
	}
	if err != nil {
		return source, err
	}
	source.Revision = model.Revision
	return source, nil
}

func templateAcquisitionProviders(selection templateSelection) []acquisition.WeightSource {
	sources, _ := inferencecatalog.DecodeWeightSources(selection.Spec.WeightSources)
	out := make([]acquisition.WeightSource, 0, len(sources))
	for _, source := range sources {
		out = append(out, acquisition.WeightSource{Provider: source.Provider, RepoID: source.RepoID, Revision: source.Revision,
			Suggested: strings.EqualFold(source.Provider, selection.SuggestedWeight.Provider) && strings.EqualFold(source.RepoID, selection.SuggestedWeight.RepoID)})
	}
	return out
}

func (s *Service) selectExplicitWeight(selection *templateSelection, choice *dto.ModelSource) error {
	if choice == nil || strings.TrimSpace(choice.Provider) == "" {
		return nil
	}
	sources, err := inferencecatalog.DecodeWeightSources(selection.Spec.WeightSources)
	if err != nil {
		return err
	}
	for _, source := range sources {
		if !strings.EqualFold(strings.TrimSpace(choice.Provider), source.Provider) {
			continue
		}
		resolved, err := s.resolveWeightRevision(source, selection.NodeID)
		if err != nil {
			return err
		}
		selection.Weight = resolved
		selection.ExplicitProvider = true
		for i := range selection.Components {
			if selection.Components[i].Role == "primary" {
				c := &selection.Components[i]
				c.Provider, c.RepoID, c.Revision, c.Subfolder = resolved.Provider, resolved.RepoID, resolved.Revision, resolved.Subfolder
			}
		}
		return s.selectExplicitComponentWeights(selection, choice.Provider)
	}
	return productError(messages.ErrorAcquisitionCandidateStale, "selected provider is not declared by this spec", map[string]any{"provider": choice.Provider})
}

// selectExplicitComponentWeights moves every other component (a speculative
// draft, a verifier) to the provider the request chose, when the component
// declares one there. A node that reaches only that provider would otherwise
// fetch the primary from it and then fail on a draft still bound to the
// spec's first source. A component that declares no source at that provider
// keeps the one selectTemplateComponents gave it. The moved components are
// recorded in ExplicitComponents, because a repository published under the
// same id at both providers would otherwise resolve to whichever mirror row
// is already registered.
func (s *Service) selectExplicitComponentWeights(selection *templateSelection, provider string) error {
	if selection.Spec == nil || len(selection.Components) == 0 {
		return nil
	}
	var declared []dto.ModelComponent
	if raw := strings.TrimSpace(selection.Spec.Components); raw != "" && raw != "null" {
		if err := json.Unmarshal([]byte(raw), &declared); err != nil {
			return fmt.Errorf("inference spec %s components: %w", selection.Spec.Name, err)
		}
	}
	for _, component := range declared {
		if strings.EqualFold(component.Role, "primary") {
			continue
		}
		encoded, err := json.Marshal(component.Weights.Sources)
		if err != nil {
			return fmt.Errorf("inference spec %s component %s sources: %w", selection.Spec.Name, component.Name, err)
		}
		sources, err := inferencecatalog.DecodeWeightSources(string(encoded))
		if err != nil {
			return fmt.Errorf("inference spec %s component %s sources: %w", selection.Spec.Name, component.Name, err)
		}
		for _, source := range sources {
			if !strings.EqualFold(strings.TrimSpace(provider), source.Provider) {
				continue
			}
			resolved, err := s.resolveWeightRevision(source, selection.NodeID)
			if err != nil {
				return err
			}
			for i := range selection.Components {
				if c := &selection.Components[i]; c.Name == component.Name {
					c.Provider, c.RepoID, c.Revision, c.Subfolder = resolved.Provider, resolved.RepoID, resolved.Revision, resolved.Subfolder
					if selection.ExplicitComponents == nil {
						selection.ExplicitComponents = map[string]bool{}
					}
					selection.ExplicitComponents[component.Name] = true
				}
			}
			break
		}
	}
	return nil
}
