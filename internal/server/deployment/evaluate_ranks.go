package deployment

import (
	"context"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/netplan"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// evaluateRankEnv is everything the per-rank preview needs that the evaluation
// has already worked out. It exists so the rank loop reads as one step of the
// evaluation rather than as the evaluation.
type evaluateRankEnv struct {
	ctx                context.Context
	out                *evaluation
	request            *dto.DeployRequest
	excludeDeployment  uint
	response           *dto.EvaluateResponse
	engine             deploy.Engine
	model              *store.Model
	launch             deploy.Materialized
	selection          templateSelection
	rankSelection      deploy.CatalogSelection
	rankNodes          []*store.Node
	placement          planning.Placement
	quick              deploy.LaunchConfig
	userExtraArgs      []string
	dynamicTPPreview   bool
	nodeImageCached    *bool
	requestOverrides   deploy.ContainerOverrides
	arch, archID       string
	vendor             string
	servedName         string
	placementVariables map[string]any
	allowUnresolved    bool
	modelWeightBytes   int64
	templateArgs       []string
	templateEnv        map[string]string
	templateEntrypoint []string
	tp                 int
	errs               *evaluateFieldErrors
}

// evaluateRanks previews one command per rank through the shared launch
// assembly. It reports false when it has already written an error response.
func (s *Service) evaluateRanks(env evaluateRankEnv) bool {
	ctx, out, req, resp, eng, model, launch := env.ctx, env.out, env.request, env.response, env.engine, env.model, env.launch
	selection, rankSelection, rankNodes, placement := env.selection, env.rankSelection, env.rankNodes, env.placement
	quick, userExtraArgs, dynamicTPPreview := env.quick, env.userExtraArgs, env.dynamicTPPreview
	nodeImageCached, requestOverrides := env.nodeImageCached, env.requestOverrides
	servedName := env.servedName
	var group netplan.Group
	if req.NNodes > 1 {
		var err error
		if group, err = s.ComputeGroup(rankNodes); err != nil {
			env.errs.add("", err)
			out.fieldErrors(resp, env.errs)
			return false
		}
		resp.Warnings = append(resp.Warnings, computeFallbackWarning(group)...)
	}
	var draftInputs []deploy.ResolveLaunchInput
	for rank, rankNode := range rankNodes {
		modelMount, componentMounts := previewComponentMounts(s.d.Store, selection.Components, rankNode)
		topology := planning.DistTopology{NNodes: req.NNodes, NodeRank: rank, Role: "standalone"}
		if req.NNodes > 1 {
			topology = planning.PreviewRankTopology(req.NNodes, rank, group.Ranks[rankNode.ID])
		}
		resolution := s.rankImage(eng.Name(), selection, req.Image, rankNode)
		rankImage := resolution.Image
		if rankImage == "" {
			rankImage = resp.Image
		}
		plan, planErr := planning.PlanLaunch(ctx, s.d.Store, s.networks(), planning.LaunchRequest{
			StartTimeoutSec: req.StartTimeoutSec,
			Preview:         true, AllowUnresolvedImage: true, ExcludeDeploymentID: env.excludeDeployment, Engine: eng.Name(), Model: model, Node: rankNode,
			Selection: rankSelection, Placement: placement, Quick: quick, ExtraArgs: userExtraArgs,
			Topology:   topology,
			Source:     planning.LaunchSourceFromRequest(rankImage, requestOverrides),
			ModelMount: modelMount, ComponentMounts: componentMounts,
			ServedName: servedName, Sleepable: req.Sleepable, YieldPolicy: requestYield(req.YieldPolicy),
			MasterAddr: req.MasterAddr, MasterPort: req.MasterPort,
			VRAMLimitBytes: int64(req.VRAMLimitGiB * (1 << 30)), PartitionBackend: req.PartitionBackend,
			RuntimeImageCached: nodeImageCached, APIContract: rankSelection.APIContract,
			ModelRules: store.SourceRules{Pinned: store.StoragePins(req.NodeRequirements), SaveTo: s.previewSaveToStorageID(req.ModelSource, model, rankNode)},
		})
		if planErr != nil {
			env.errs.add("", planErr)
			out.fieldErrors(resp, env.errs)
			return false
		}
		res := plan.Resolved
		draftInputs = append(draftInputs, plan.LaunchInput)
		resp.Warnings = append(resp.Warnings, plan.Warnings...)
		resp.Warnings = append(resp.Warnings, plan.Output.Warnings...)
		if dynamicTPPreview {
			// No GPU has been chosen yet: the engine builder's single-GPU
			// default is a placeholder, and the rank that validates the
			// template owns the flags that can claim it.
			res = deploy.SymbolizePreviewTP(eng, res, launch.TemplateResidual, launch.UserResidual)
		}
		resp.Warnings = append(resp.Warnings, resolution.Warnings...)
		if resolution.Blocking {
			resp.OK = false
		}
		rankCommand := dto.EvaluateRankCommand{
			PreStartArgv: res.PreStartArgv, PreStartScript: res.PreStartScript,
			NodeUUID: rankNode.UUID, NodeName: rankNode.Hostname, RankAddress: topology.NodeAddress,
			NodeRank: rank, Role: topology.Role, Headless: topology.Headless,
			Image:      resolution.Image,
			Entrypoint: res.Entrypoint, Argv: res.Argv, Command: launchCommand(res), Env: res.Env,
			CommandMode: res.CommandMode, ModelMount: res.ModelMount, DockerArgs: res.DockerArgs, ShmBytes: res.ShmBytes, Mounts: evaluateMounts(res.Mounts), EnvLayers: evaluateEnvLayers(res),
		}
		resp.RankCommands = append(resp.RankCommands, rankCommand)
		if rank == 0 {
			setEvaluateLaunch(resp, res)
			resp.Image = rankCommand.Image
		}
	}
	if len(resp.RankCommands) > 0 {
		if req.NNodes > 1 {
			setManualCommandDraft(resp, draftInputs)
		}
		return true
	}
	return s.evaluateRankFallback(env)
}

// evaluateRankFallback preserves a useful command preview if the selected node
// disappeared between request validation and evaluation.
func (s *Service) evaluateRankFallback(env evaluateRankEnv) bool {
	out, req, resp, eng, model, launch := env.out, env.request, env.response, env.engine, env.model, env.launch
	selection, quick, userExtraArgs := env.selection, env.quick, env.userExtraArgs
	arch, archID, vendor, servedName := env.arch, env.archID, env.vendor, env.servedName
	placementVariables, allowUnresolvedPlacement := env.placementVariables, env.allowUnresolved
	modelWeightBytes := env.modelWeightBytes
	templateArgs, templateEnv, templateEntrypoint := env.templateArgs, env.templateEnv, env.templateEntrypoint
	tp := env.tp
	dynamicTPPreview := env.dynamicTPPreview
	modelMount, componentMounts := previewComponentMounts(s.d.Store, selection.Components, nil)
	// tp is the degree the plan settled on; an unstated spec takes it.
	parallel := env.placement.Parallelism(req.NNodes)
	dp, err := deploy.PlacementDataParallel(eng.Name(), quick.DP, templateArgs, userExtraArgs)
	if err != nil {
		env.errs.add("", err)
		out.fieldErrors(resp, env.errs)
		return false
	}
	parallel.DP = dp
	if !parallel.Stated() {
		parallel.TP = tp
	}
	perNode, err := parallel.GPUsPerNode()
	if err != nil || perNode < 1 {
		perNode = 1
	}
	params := deploy.Params{
		ModelPath: modelMount, ServedName: servedName, APIPort: planning.PreviewAPIPort,
		AssignedGPUs: perNode, TensorParallel: parallel.TP, PipelineParallel: parallel.PP, Sleepable: req.Sleepable, Quick: quick, TemplateArgs: templateArgs,
		TemplateEnv: templateEnv, ExtraArgs: userExtraArgs,
		Arch: arch, ArchID: archID, Vendor: vendor, ModelType: model.Arch,
	}
	res, launchWarnings, resolveErr := resolveRequestLaunch(deploy.ResolveLaunchInput{
		StartTimeoutSec: req.StartTimeoutSec, DefaultStartTimeoutSec: s.d.Store.DeploymentPolicy().StartTimeoutSec,
		Engine: eng, Params: params, Image: env.rankSelection.Image,
		Entrypoint:   templateEntrypoint,
		PreStartArgv: selection.PreStartArgv, PreStartScript: selection.PreStartScript,
		RuntimeAssets: selection.Assets, ComponentMounts: componentMounts, APIContract: selection.APIContract,
		PlacementVariables: placementVariables, AllowUnresolved: allowUnresolvedPlacement,
		ModelWeightBytes: modelWeightBytes, TemplateShmBytes: selection.shmBytes(), TemplateMemlockUnlimited: selection.memlockUnlimited(),
	}, req.ContainerOverrides, selection.Components, modelMount)
	if resolveErr != nil {
		env.errs.add("", resolveErr)
		out.fieldErrors(resp, env.errs)
		return false
	}
	if dynamicTPPreview {
		res = deploy.SymbolizePreviewTP(eng, res, launch.TemplateResidual, launch.UserResidual)
	}
	resp.Warnings = append(resp.Warnings, launchWarnings...)
	setEvaluateLaunch(resp, res)
	return true
}
