package deployment

import (
	"context"
	"errors"
	"math"
	"strings"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/scheduler"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func needsDistributedRecommendation(req *dto.DeployRequest, autoPlaced bool) bool {
	if req.NNodes <= 1 || autoPlaced || placementPD(req) != nil {
		return false
	}
	p, err := requestPlacement(req)
	return err == nil && (p.TP <= 0 || p.PP <= 0)
}

// rankRefusal is why one rank cannot run a candidate layout: a scheduler
// refusal code or a messages.RecommendationReason, with that reason's params.
type rankRefusal struct {
	node   string
	reason string
	params map[string]any
	cause  error
}

func (r *rankRefusal) Error() string {
	if r.cause != nil {
		return r.node + ": " + r.reason + ": " + r.cause.Error()
	}
	return r.node + ": " + r.reason
}

// The reason's params stay apart from node and reason: a scheduler reason
// may carry a reason or node param of its own.
func (r *rankRefusal) productError() error {
	params := map[string]any{"node": r.node, "reason": r.reason}
	if len(r.params) > 0 {
		params["reason_params"] = r.params
	}
	return productError(messages.ErrorDistributedParallelismUnavailable, "cannot recommend parallelism: "+r.Error(), params)
}

func invalidDistributedDegree(degree string) error {
	return productError(messages.ErrorDistributedDegreeInvalid, degree+" must be a positive integer", map[string]any{"degree": degree})
}

// Only drafts with an unstated degree come here. Fixed placements and edits
// keep their existing admission and may wait for cards at launch time.
func (s *Service) recommendDistributed(ctx context.Context, req *dto.DeployRequest, dp int, eng deploy.Engine, launch deploy.Materialized, selection templateSelection, model *store.Model) ([]*store.Node, error) {
	// The trials judge the placement as planning does (requestPlacement below),
	// not as parsed here.
	nodes, _, err := s.distributedNodes(req)
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
	if req.Placement["tp"] != nil && p.TP <= 0 {
		return nodes, invalidDistributedDegree("tp")
	}
	if req.Placement["pp"] != nil && p.PP <= 0 {
		return nodes, invalidDistributedDegree("pp")
	}
	// A draft the trials cannot plan for a reason another check owns keeps
	// the node-derived default, and that check reports the input as it does
	// for a fixed layout: weights of unknown size, a container override, an
	// image a node cannot run, or a partition backend a node cannot use.
	overrides, overrideErr := RequestContainerOverrides(req.ContainerOverrides)
	if overrideErr != nil || model.WeightBytes <= 0 && (selection.Spec == nil || max(selection.Spec.WeightBytes, selection.Spec.RuntimeWeightBytes) <= 0) {
		return nodes, setNodeParallelism(req, nodes, p, dp)
	}
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
		image := s.rankImage(eng.Name(), selection, req.Image, node)
		backend, backendErr := ResolvePartitionBackend(node, req.VRAMLimitGiB, req.PartitionBackend)
		if image.Blocking || backendErr != nil {
			return nodes, setNodeParallelism(req, nodes, p, dp)
		}
		capacity = min(capacity, len(node.OwnedGPUs()))
		mount, components := previewComponentMounts(s.d.Store, selection.Components, node)
		rank := evaluationLaunchRequest(evaluatePlanEnv{
			request: req, response: &dto.EvaluateResponse{Image: image.Image}, engine: eng, model: model,
			selection: selection, placement: p, quick: launch.Quick, userExtraArgs: launch.UserResidual,
			templateResidual: launch.TemplateResidual, servedName: req.ServedName, requestOverrides: overrides,
			modelMount: mount, componentMounts: components,
		}, node)
		rank.Topology = planning.PreviewRankTopology(req.NNodes, i, group.Ranks[node.ID])
		rank.ModelRules.SaveTo = s.previewSaveToStorageID(req.ModelSource, model, node)
		rank.PartitionBackend = backend
		rank.ImplicitServedName = s.implicitServedName(node)
		// Holder explanations only word a refusal; a trial needs the verdict.
		rank.ExplainHolders = false
		ranks = append(ranks, rank)
	}
	layers := 0
	if cfg := planning.LoadConfig(model); cfg != nil {
		layers = cfg.NumHiddenLayers
	}
	hint := planning.ParallelismSpec{TP: p.TP, PP: p.PP, DP: dp, NNodes: req.NNodes}
	spec, err := planning.PlanDistributedParallelism(ctx, hint, capacity, layers, ranks[0].Selection.PlacementResolver() != "", func(candidate planning.ParallelismSpec) (planning.DistributedTrial, error) {
		return s.trialDistributedRanks(ctx, ranks, candidate)
	})
	var refusal *rankRefusal
	switch {
	case errors.As(err, &refusal):
		return nodes, refusal.productError()
	case errors.Is(err, planning.ErrNoDistributedLayout):
		// Keep the original capacity and divisibility errors on their fields.
		if _, cause := nodeParallelism(nodes, p, dp); cause != nil {
			return nodes, cause
		}
		return nodes, (&rankRefusal{node: nodeNames(nodes), reason: messages.RecommendationReasonNoLayout}).productError()
	case err != nil:
		return nodes, err
	}
	setDistributedDegrees(req, spec.TP, spec.PP)
	return nodes, nil
}

func (s *Service) trialDistributedRanks(ctx context.Context, ranks []planning.LaunchRequest, candidate planning.ParallelismSpec) (planning.DistributedTrial, error) {
	result := planning.DistributedTrial{Preference: math.MaxInt64}
	for _, rank := range ranks {
		if err := ctx.Err(); err != nil {
			return planning.DistributedTrial{}, err
		}
		rank.Placement.TP, rank.Placement.PP = candidate.TP, candidate.PP
		node := rank.Node.DisplayName()
		plan, refusal, err := planning.PlanTemplateLaunch(ctx, s.d.Store, s.networks(), rank.Node, rank, rank.Selection)
		if err != nil {
			return planning.DistributedTrial{}, err
		}
		if refusal != nil {
			return planning.DistributedTrial{}, &rankRefusal{node: node, reason: refusal.Code, params: refusal.Params}
		}
		// A catalog placement can still reject the GPU set the scheduler chose.
		if plan.Output.PlacementError != nil {
			return planning.DistributedTrial{}, &rankRefusal{node: node, reason: scheduler.ReasonPlacementRejected, cause: plan.Output.PlacementError}
		}
		actual, err := deploy.RecommendationLaunchParallelism(rank.Engine, plan.Resolved)
		if err != nil {
			return planning.DistributedTrial{}, &rankRefusal{node: node, reason: messages.RecommendationReasonCommandUnverified, cause: err}
		}
		if actual.TP != candidate.TP || actual.PP != candidate.PP {
			return planning.DistributedTrial{}, &rankRefusal{node: node, reason: messages.RecommendationReasonCommandMismatch, params: map[string]any{"tp": actual.TP, "pp": actual.PP}}
		}
		result.Preference = min(result.Preference, plan.Output.Result.Plan.Preference)
		result.ContextShort = result.ContextShort || plan.Output.ContextShort()
	}
	return result, nil
}

func nodeNames(nodes []*store.Node) string {
	names := make([]string, len(nodes))
	for i, n := range nodes {
		names[i] = n.DisplayName()
	}
	return strings.Join(names, ", ")
}
