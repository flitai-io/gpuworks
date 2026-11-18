package deployment

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// Bound the interactive preview even if a client leaves the request open.
const distributedRecommendationTimeout = 5 * time.Second

func needsDistributedRecommendation(req *dto.DeployRequest, autoPlaced bool) bool {
	if req.NNodes <= 1 || autoPlaced || placementPD(req) != nil {
		return false
	}
	p, err := requestPlacement(req)
	return err == nil && (p.TP <= 0 || p.PP <= 0)
}

func recommendationError(detail string) error {
	return productError(messages.ErrorDistributedRecommendation, detail, map[string]any{"detail": detail})
}

// Only drafts with an unstated degree come here. Fixed placements and edits
// keep their existing admission and may wait for cards at launch time.
func (s *Service) recommendDistributed(ctx context.Context, req *dto.DeployRequest, dp int, eng deploy.Engine, launch deploy.Materialized, selection templateSelection, model *store.Model) ([]*store.Node, error) {
	nodes, err := s.distributedNodes(req)
	if err != nil {
		return nil, err
	}
	group, err := s.prepareRankNetwork(req, nodes)
	if err != nil {
		return nil, err
	}
	p, err := requestPlacement(req)
	if err != nil {
		return nil, err
	}
	if (req.Placement["tp"] != nil && p.TP <= 0) || (req.Placement["pp"] != nil && p.PP <= 0) {
		return nodes, recommendationError("TP and PP must be positive integers when supplied")
	}
	if model.WeightBytes <= 0 && (selection.Spec == nil || max(selection.Spec.WeightBytes, selection.Spec.RuntimeWeightBytes) <= 0) {
		return nodes, recommendationError("model weight size is unknown")
	}
	overrides, err := RequestContainerOverrides(req.ContainerOverrides)
	if err != nil {
		return nodes, err
	}
	// Interactive recommendation must yield to a new draft or a fixed request.
	ctx, cancel := context.WithTimeout(ctx, distributedRecommendationTimeout)
	defer cancel()
	ranks := make([]planning.LaunchRequest, 0, len(nodes))
	capacity := math.MaxInt
	for i, n := range nodes {
		if err := ctx.Err(); err != nil {
			return nodes, err
		}
		node, err := s.d.Store.NodeWithGPUs(n.UUID)
		if err != nil {
			return nodes, err
		}
		capacity = min(capacity, len(node.OwnedGPUs()))
		image := s.rankImage(eng.Name(), selection, req.Image, node)
		if image.Blocking {
			return nodes, recommendationError(node.DisplayName() + ": incompatible image")
		}
		mount, components := previewComponentMounts(s.d.Store, selection.Components, node)
		rank := evaluationLaunchRequest(evaluatePlanEnv{
			request: req, response: &dto.EvaluateResponse{Image: image.Image}, engine: eng, model: model,
			selection: selection, placement: p, quick: launch.Quick, userExtraArgs: launch.UserResidual,
			templateResidual: launch.TemplateResidual, servedName: req.ServedName, requestOverrides: overrides,
			modelMount: mount, componentMounts: components,
		}, node)
		rank.Topology = planning.PreviewRankTopology(req.NNodes, i, group.Ranks[node.ID])
		rank.ModelRules.SaveTo = s.previewSaveToStorageID(req.ModelSource, model, node)
		rank.PartitionBackend, err = ResolvePartitionBackend(node, req.VRAMLimitGiB, req.PartitionBackend)
		if err != nil {
			return nodes, err
		}
		ranks = append(ranks, rank)
	}
	layers := 0
	if cfg := planning.LoadConfig(model); cfg != nil {
		layers = cfg.NumHiddenLayers
	}
	hint := planning.ParallelismSpec{TP: p.TP, PP: p.PP, DP: dp, NNodes: req.NNodes}
	spec, err := planning.PlanDistributedParallelism(ctx, hint, capacity, layers, ranks[0].Selection.PlacementResolver() != "", func(candidate planning.ParallelismSpec) (int64, error) {
		return s.trialDistributedRanks(ctx, ranks, candidate)
	})
	if errors.Is(err, planning.ErrNoDistributedLayout) {
		// Keep the original capacity and divisibility errors on their fields.
		if _, cause := nodeParallelism(nodes, p, dp); cause != nil {
			return nodes, cause
		}
	}
	if err != nil {
		return nodes, recommendationError(err.Error())
	}
	if req.Placement == nil {
		req.Placement = map[string]any{}
	}
	req.Placement["tp"], req.Placement["pp"] = spec.TP, spec.PP
	return nodes, nil
}

func (s *Service) trialDistributedRanks(ctx context.Context, ranks []planning.LaunchRequest, candidate planning.ParallelismSpec) (int64, error) {
	preference := int64(math.MaxInt64)
	for _, rank := range ranks {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		rank.Placement.TP, rank.Placement.PP = candidate.TP, candidate.PP
		plan, refusal, err := planning.PlanTemplateLaunch(ctx, s.d.Store, s.networks(), rank.Node, rank, rank.Selection)
		if err != nil {
			return 0, err
		}
		if refusal != nil {
			return 0, fmt.Errorf("%s: %s", rank.Node.DisplayName(), refusal.Code)
		}
		if actual, err := deploy.LaunchParallelism(rank.Engine, plan.Resolved); err == nil && (actual.TP != candidate.TP || actual.PP != candidate.PP) {
			return 0, fmt.Errorf("%s: command specifies TP%d PP%d", rank.Node.DisplayName(), actual.TP, actual.PP)
		}
		preference = min(preference, plan.Output.Result.Plan.Preference)
	}
	return preference, nil
}
