package deployment

import (
	"context"
	"fmt"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// Rank-scoped weights (ADR 0076): a spec whose weight subfolder names the
// rank resolves one model artifact per rank at create. Rank 0's artifact is
// the deployment's own model, so everything that names "the deployment's
// model" (its name, served name, operations, estimation) keeps reading one
// row; the other ranks' artifacts are bound in the catalog selection
// snapshot and read only by the rank they belong to.

// rankScoped reports whether the selected weights name the rank.
func (s templateSelection) rankScoped() bool {
	return deploy.RankScopedSubfolder(s.Weight.Subfolder)
}

// lookupSource is the source a registered artifact is looked up by: rank 0's
// subfolder for a rank-scoped source, which is the artifact the deployment's
// own model is, and the source itself otherwise.
func lookupSource(source weightSource) weightSource {
	source.Subfolder = deploy.ExpandRankSubfolder(source.Subfolder, 0)
	return source
}

// checkRankZeroModel refuses a request that names a model for a rank-scoped
// spec other than rank 0's artifact: the named model becomes the
// deployment's own, which rank 0 mounts.
func checkRankZeroModel(template weightSource, requested *store.Model) error {
	if !deploy.RankScopedSubfolder(template.Subfolder) || requested == nil {
		return nil
	}
	if want := deploy.ExpandRankSubfolder(template.Subfolder, 0); requested.ArtifactSubfolder != want {
		return productError(messages.ErrorRankWeightsModelNotRankZero,
			fmt.Sprintf("model_id %d is subfolder %q of %s, not rank 0's %q", requested.ID, requested.ArtifactSubfolder, template.RepoID, want),
			map[string]any{"model_id": requested.ID, "repo_id": template.RepoID, "subfolder": requested.ArtifactSubfolder, "want": want})
	}
	return nil
}

// readsByRank reports whether a rank reads anything of its own: the
// selection's weights or a component's are rank-scoped. A component keeps the
// template's subfolder, so this holds before its models are resolved too.
func (s templateSelection) readsByRank() bool {
	if s.rankScoped() {
		return true
	}
	for _, component := range s.Components {
		if deploy.RankScopedSubfolder(component.Subfolder) {
			return true
		}
	}
	return false
}

// checkRankWeightsPD refuses rank-scoped weights, the spec's own or a
// component's, deployed as PD: a PD deployment's ranks are numbered per role
// instance, not as one group.
func checkRankWeightsPD(selection templateSelection, pd bool) error {
	if !pd || !selection.readsByRank() || selection.Spec == nil {
		return nil
	}
	return productError(messages.ErrorRankWeightsPDUnsupported, "spec "+selection.Spec.Name+" has rank-scoped weights, which a PD deployment cannot run",
		map[string]any{"spec": selection.Spec.Name})
}

// resolveRankWeights binds every rank of a rank-scoped selection to its
// artifact. rank0 is the deployment's own model; the other ranks take the
// same provider, repository and revision with their own subfolder, so a
// group never mixes commits or mirrors.
func (s *Service) resolveRankWeights(selection *templateSelection, rank0 *store.Model, template string) error {
	selection.RankWeights = nil
	if !deploy.RankScopedSubfolder(template) || rank0 == nil {
		return nil
	}
	ranks := max(selection.WeightRanks, 1)
	selection.RankWeights = append(selection.RankWeights, deploy.RankWeight{Rank: 0, Subfolder: rank0.ArtifactSubfolder, StoreModelID: rank0.ID})
	for rank := 1; rank < ranks; rank++ {
		source := weightSource{Provider: rank0.Source, RepoID: rank0.SourceID, Revision: rank0.Revision, Subfolder: deploy.ExpandRankSubfolder(template, rank)}
		model, err := s.selectionWeightModel(selection, source, selection.Model, selection.Spec.Engine, selection.Spec.Quantization, selection.Spec.WeightBytes, 0, selection.StoragePins, true, true)
		if err != nil {
			return fmt.Errorf("rank %d weights %s: %w", rank, source.Subfolder, err)
		}
		s.applyCatalogEstimationConfig(selection, model)
		selection.RankWeights = append(selection.RankWeights, deploy.RankWeight{Rank: rank, Subfolder: model.ArtifactSubfolder, StoreModelID: model.ID})
	}
	return nil
}

// resolveComponentModel binds a non-primary component to its artifact. A
// rank-scoped one is resolved as the selection's own rank-scoped weights are
// (resolveRankWeights): rank 0's subfolder by the usual rules with its
// revision as identity, and every other rank at rank 0's provider, repository
// and revision with its own subfolder. StoreModelID is rank 0's artifact; the
// component keeps the template's subfolder.
func (s *Service) resolveComponentModel(selection *templateSelection, component *deploy.ComponentBinding, original *store.InferenceModel) error {
	component.RankWeights = nil
	template := component.Subfolder
	scoped := deploy.RankScopedSubfolder(template)
	source := weightSource{Provider: component.Provider, RepoID: component.RepoID, Revision: component.Revision, Subfolder: template}
	model, err := s.selectionWeightModel(selection, lookupSource(source), original,
		selection.Spec.Engine, component.Quantization, component.WeightBytes, 0, selection.StoragePins, scoped, selection.ExplicitComponents[component.Name])
	if err != nil {
		return fmt.Errorf("component %s weights: %w", component.Name, err)
	}
	component.StoreModelID = model.ID
	if !scoped {
		return nil
	}
	component.Provider, component.RepoID, component.Revision = model.Source, model.SourceID, model.Revision
	component.RankWeights = append(component.RankWeights, deploy.RankWeight{Rank: 0, Subfolder: model.ArtifactSubfolder, StoreModelID: model.ID})
	for rank := 1; rank < max(selection.WeightRanks, 1); rank++ {
		source := weightSource{Provider: model.Source, RepoID: model.SourceID, Revision: model.Revision, Subfolder: deploy.ExpandRankSubfolder(template, rank)}
		rankModel, err := s.selectionWeightModel(selection, source, original, selection.Spec.Engine, component.Quantization, component.WeightBytes, 0, selection.StoragePins, true, true)
		if err != nil {
			return fmt.Errorf("component %s rank %d weights %s: %w", component.Name, rank, source.Subfolder, err)
		}
		component.RankWeights = append(component.RankWeights, deploy.RankWeight{Rank: rank, Subfolder: rankModel.ArtifactSubfolder, StoreModelID: rankModel.ID})
	}
	return nil
}

// inspectWeights is inspectDeploymentModel for every weight target: the
// deployment's model on every rank node, or each rank's artifact on its own
// rank's node. The checks are concatenated; the first refusal is returned.
func (s *Service) inspectWeights(ctx context.Context, selection templateSelection, model *store.Model, rules store.SourceRules, rankNodes []*store.Node) (*dto.DiagnosticReport, error) {
	if len(selection.RankWeights) == 0 {
		return s.inspectDeploymentModel(ctx, model, rules, rankNodes, false)
	}
	targets, err := s.weightTargets(selection, model, rankNodes, rankNodes)
	if err != nil {
		return nil, err
	}
	var out *dto.DiagnosticReport
	var firstErr error
	for _, target := range targets {
		report, err := s.inspectDeploymentModel(ctx, target.model, rules, target.nodes, false)
		if firstErr == nil {
			firstErr = err
		}
		if report == nil {
			continue
		}
		if out == nil {
			out = &dto.DiagnosticReport{}
		}
		out.Checks = append(out.Checks, report.Checks...)
		out.Truncated = out.Truncated || report.Truncated
	}
	return out, firstErr
}

// weightTarget is one model a create admits and may start downloading, and
// the nodes that read it.
type weightTarget struct {
	model *store.Model
	nodes []*store.Node
}

// weightTargets is the deployment's own model read by every target, or, for
// rank-scoped weights, each rank's artifact read by its own rank's node.
// rankNodes are the group's nodes in rank order (empty for a single node).
func (s *Service) weightTargets(selection templateSelection, model *store.Model, targets, rankNodes []*store.Node) ([]weightTarget, error) {
	if len(selection.RankWeights) == 0 {
		return []weightTarget{{model: model, nodes: targets}}, nil
	}
	out := make([]weightTarget, 0, len(selection.RankWeights))
	for _, weight := range selection.RankWeights {
		rankModel := model
		if weight.StoreModelID != model.ID {
			var err error
			if rankModel, err = s.d.Store.GetModel(weight.StoreModelID); err != nil {
				return nil, err
			}
		}
		nodes := targets
		if weight.Rank < len(rankNodes) && rankNodes[weight.Rank] != nil {
			nodes = []*store.Node{rankNodes[weight.Rank]}
		}
		out = append(out, weightTarget{model: rankModel, nodes: nodes})
	}
	return out, nil
}
