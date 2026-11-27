package deployment

import (
	"context"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	placementeval "gpuworks.ai/gpuworks/internal/server/placement"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/server/yieldpolicy"
)

// evaluatePlanEnv is the state the preview's single plan call reads.
type evaluatePlanEnv struct {
	ctx               context.Context
	out               *evaluation
	request           *dto.DeployRequest
	excludeDeployment uint
	response          *dto.EvaluateResponse
	engine            deploy.Engine
	model             *store.Model
	node              *store.Node
	nodeFound         bool
	profileNode       *store.Node
	imageCached       *bool
	nodeImageCached   *bool
	selection         templateSelection
	placement         planning.Placement
	quick             deploy.LaunchConfig
	userExtraArgs     []string
	servedName        string
	requestOverrides  deploy.ContainerOverrides
	modelMount        string
	componentMounts   []proto.MountSpec
	gpuCount          int
	resolver          string
	templateResidual  []string
	errs              *evaluateFieldErrors
}

// evaluatePlanResult is what the plan leaves behind: the pieces of evaluation
// state the projection updates, in one place.
type evaluatePlanResult struct {
	nodeImageCached    *bool
	placement          placementeval.Result
	placementResolved  bool
	modelWeightBytes   int64
	placementVariables map[string]any
	allowUnresolved    bool
	dynamicTPPreview   bool
	gpuCount           int
	tp                 int
	quick              deploy.LaunchConfig
}

// evaluatePlan runs the one plan this preview shows and projects its output
// onto the response. It reports false when it has already written an error.
func (s *Service) evaluatePlan(env evaluatePlanEnv) (evaluatePlanResult, bool) {
	req, resp, node := env.request, env.response, env.node
	// tp is the deployment's tensor-parallel degree: stated, or completed
	// from the GPUs the schedule picks (package parallelism).
	out := evaluatePlanResult{nodeImageCached: env.nodeImageCached, gpuCount: env.gpuCount, tp: env.placement.TP, quick: env.quick}
	if env.nodeFound {
		if env.profileNode != nil && env.profileNode.UUID == node.UUID {
			out.nodeImageCached = env.imageCached
		}
		specWeightBytes := int64(0)
		if env.selection.Spec != nil {
			specWeightBytes = env.selection.Spec.WeightBytes
		}
		s.persistCatalogWeight(env.model, specWeightBytes)
		request := evaluationLaunchRequest(env, node)
		request.RuntimeImageCached = out.nodeImageCached
		request.ModelRules.SaveTo = s.previewSaveToStorageID(req.ModelSource, env.model, node)
		previewPlan, planErr := s.planLaunch(env.ctx, request)

		if items := previewPlan.Output.GPUCandidates; !IsAutomaticPlacement(req) && req.NNodes <= 1 && items != nil {
			resp.GPUCandidates = &dto.GPUCandidates{NodeUUID: node.UUID, Items: make([]dto.GPUCandidate, 0, len(items))}
			for _, item := range items {
				card := dto.GPUCandidate{Index: item.Index, Available: item.Available}
				if reason := item.Reason; reason != nil {
					card.Reason = &dto.ScheduleReason{Code: reason.Code, Params: reason.Params, Message: reason.Message}
				}
				if note := item.Note; note != nil {
					card.Note = &dto.ScheduleReason{Code: note.Code, Params: note.Params}
				}
				resp.GPUCandidates.Items = append(resp.GPUCandidates.Items, card)
			}
		}
		if planErr != nil {
			// Launch-assembly failures name no form control; they belong on the
			// page-level error line, not beside a parameter.
			env.errs.add("", planErr)
			env.out.fieldErrors(env.response, env.errs)
			return evaluatePlanResult{}, false
		}
		if previewPlan.Output.PlacementError != nil {
			env.errs.add("", previewPlan.Output.PlacementError)
			env.out.fieldErrors(env.response, env.errs)
			return evaluatePlanResult{}, false
		}
		// Everything the form shows about the estimate comes from the plan the
		// launch would use: one input, one breakdown, one schedule.
		out.placement = previewPlan.Output.Placement
		out.modelWeightBytes = previewPlan.Output.Breakdown.WeightBytes
		resp.Breakdown = previewPlan.Output.Breakdown
		result := previewPlan.Output.Result
		if !previewPlan.Output.Fits() {
			// Why it does not fit, as the controller would say it: waiting for
			// holders to yield, or for sleepers' cards, reads as such.
			result.Message = previewPlan.Output.Refusal(node, yieldpolicy.Policy{Yield: requestYield(req.YieldPolicy)})
		}
		resp.Plan = result
		if len(previewPlan.Output.Eviction) > 0 {
			resp.Eviction = make([]dto.EvictionSuggestion, 0, len(previewPlan.Output.Eviction))
			for _, suggestion := range previewPlan.Output.Eviction {
				resp.Eviction = append(resp.Eviction, dto.EvictionSuggestion{
					InstanceUUID: suggestion.InstanceUUID,
					DeploymentID: suggestion.DeploymentID,
					Deployment:   suggestion.Deployment,
					GPUIndexes:   suggestion.GPUIndexes,
					FreesGiB:     suggestion.FreesGiB,
				})
			}
		}
		resp.Warnings = append(resp.Warnings, previewPlan.Output.Warnings...)
		if !previewPlan.Output.Result.OK {
			resp.OK = false
		}
		if previewPlan.Output.Result.OK && previewPlan.Output.Result.Plan != nil {
			out.gpuCount = len(previewPlan.Output.Result.Plan.GPUIndexes)
			out.tp = previewPlan.Input.Parallel.Assigned(out.gpuCount).TP
			out.placementResolved = true
		}
		out.quick.CPUCores = node.CPUCores
		out.quick.EstimatedGPUKVGiB = float64(previewPlan.Output.Breakdown.KVTotalBytes) / float64(int64(1)<<30)
	}
	if env.resolver != "" && env.nodeFound && out.placementResolved {
		out.placementVariables = placementeval.RuntimeVariables(out.placement, out.gpuCount, out.tp, env.modelMount)
	}
	out.allowUnresolved = !out.placementResolved
	out.dynamicTPPreview = out.tp <= 0 && (!env.nodeFound || !out.placementResolved)
	resp.Warnings = append(resp.Warnings, deploy.PerformanceWarnings(out.quick, deploy.FormatArgsText(env.userExtraArgs), env.model.Arch, out.gpuCount)...)
	if out.tp <= 0 {
		out.tp = 1
	}
	return out, true
}

// planRanks estimates and schedules the draft, then previews the launch of
// each rank.
func (r *evaluateState) planRanks() bool {
	arch, archID, vendor := r.image.arch, r.image.archID, r.image.vendor
	imageCached := r.image.imageCached
	// nodeImageCached is the same fact narrowed to the node the estimate lands
	// on; evaluatePlan fills it in for the per-rank assembly.
	var nodeImageCached *bool

	// Estimate + schedule (shared with the controller).
	gpuCount := len(r.placement.GPUIndexes)
	if r.errs.add("placement.gpus", RequestCardsOnNode(r.req, r.node)) {
		r.writeFieldErrors()
		return false
	}
	resolver := templatePlacementResolver(r.selection.Placement)
	templateArgs := append([]string(nil), r.launch.TemplateResidual...)
	templateEnv := cloneStringMap(r.selection.Env)
	templateEntrypoint := append([]string(nil), r.selection.Entrypoint...)
	// The launch inputs a request owns: the container override layer it states,
	// and the template layer the write boundary would keep (the residual, not
	// the spec's raw argument list).
	var overrideErr error
	r.requestOverrides, overrideErr = RequestContainerOverrides(r.req.ContainerOverrides)
	if r.errs.add("", overrideErr) {
		// The override layer is a section, not one control: the page-level
		// line carries its decode failures.
		r.writeFieldErrors()
		return false
	}
	r.modelMount, r.componentMounts = previewComponentMounts(r.s.d.Store, r.selection.Components, r.node)
	plan, ok := r.s.evaluatePlan(evaluatePlanEnv{
		ctx: r.ctx, out: r.out, request: r.req, excludeDeployment: r.excludeDeployment, response: &r.resp, engine: r.eng, model: r.model,
		node: r.node, nodeFound: r.nodeFound, profileNode: r.profileNode, imageCached: imageCached,
		nodeImageCached: nodeImageCached, selection: r.selection, placement: r.placement, quick: r.quick,
		userExtraArgs: r.launch.UserResidual, servedName: r.servedName, requestOverrides: r.requestOverrides,
		modelMount: r.modelMount, componentMounts: r.componentMounts, gpuCount: gpuCount,
		resolver: resolver, templateResidual: r.launch.TemplateResidual, errs: r.errs,
	})
	if !ok {
		return false
	}
	nodeImageCached, r.quick = plan.nodeImageCached, plan.quick
	tp := plan.tp
	parallelismSummary, parallelismErr := evaluateParallelism(r.placement, r.req.NNodes, tp, plan.placementResolved, r.quick, r.launch.TemplateResidual, r.launch.UserResidual, r.eng.Name())
	if r.errs.add("data_parallel_size", parallelismErr) {
		r.writeFieldErrors()
		return false
	}
	r.resp.Parallelism = parallelismSummary
	placementVariables, allowUnresolvedPlacement := plan.placementVariables, plan.allowUnresolved
	dynamicTPPreview := plan.dynamicTPPreview

	rankSelection := snapshotTemplateSelection(r.selection, r.resp.Image)
	// The template layer a launch resolves against is the residual the write
	// boundary kept, not the spec's own argument list: dictionary flags are
	// edited through the form and the engine dictionary, and a spec token that
	// happens to name one is what the stored row no longer carries either.
	rankSelection.ExtraArgs = r.launch.TemplateResidual
	return r.s.evaluateRanks(evaluateRankEnv{
		ctx: r.ctx, out: r.out, request: r.req, excludeDeployment: r.excludeDeployment, response: &r.resp, engine: r.eng, model: r.model, launch: r.launch,
		selection: r.selection, rankSelection: rankSelection, rankNodes: r.rankNodes, placement: r.placement,
		quick: r.quick, userExtraArgs: r.launch.UserResidual, dynamicTPPreview: dynamicTPPreview,
		nodeImageCached: nodeImageCached, requestOverrides: r.requestOverrides,
		arch: arch, archID: archID, vendor: vendor, servedName: r.servedName,
		placementVariables: placementVariables, allowUnresolved: allowUnresolvedPlacement,
		modelWeightBytes: plan.modelWeightBytes, templateArgs: templateArgs, templateEnv: templateEnv,
		templateEntrypoint: templateEntrypoint, tp: tp, errs: r.errs,
	})
}
