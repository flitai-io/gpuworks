package deployment

import (
	"context"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/server/yieldpolicy"
)

// evaluateAutomaticPlacement answers placement_mode=auto for a single-node,
// single-replica draft the way the controller will place it: the eligible
// nodes are ranked from bulk reads (planning.RankPool, the deployment's
// strategy, no replica anywhere yet), the best planning.PoolTrialBudget of
// them are planned through the preview the manual path uses, and the first
// that fits in rank order is the selection - where the controller would put
// the replica. The candidates list records each ineligible node's reason and
// each planned node's answer; eligible nodes past the budget are not planned,
// so a thousand-node cluster costs the same few plans as a three-node one. A
// create repeating the draft re-validates the selection and answers
// AUTOMATIC_PLACEMENT_STALE when the world moved - nothing is silently
// re-placed.
func (s *Service) evaluateAutomaticPlacement(ctx context.Context, req *dto.DeployRequest, engine deploy.Engine, model *store.Model, selection templateSelection, placement planning.Placement, quick deploy.LaunchConfig, userExtraArgs []string, servedName, image string, requestOverrides deploy.ContainerOverrides, eligible []dto.EligibleNode, excludeDeployment uint) (*dto.PlacementNode, []dto.PlacementCandidate) {
	if len(eligible) == 0 {
		return nil, nil
	}
	var candidates []dto.PlacementCandidate
	names := map[string]string{}
	for _, entry := range eligible {
		names[entry.UUID] = entry.Name
		if entry.Eligible {
			continue
		}
		reason := dto.ScheduleReason{Code: "ineligible"}
		if len(entry.Reasons) > 0 {
			reason = entry.Reasons[0]
		}
		candidates = append(candidates, dto.PlacementCandidate{NodeUUID: entry.UUID, NodeName: entry.Name, Reason: reason})
	}
	dp, err := deploy.PlacementDataParallel(engine.Name(), quick.DP, selection.ExtraArgs, userExtraArgs)
	if err != nil {
		return nil, candidates
	}
	parallel := placement.Parallelism(1)
	parallel.DP = dp
	ranked, err := s.rankEligible(eligible, model, req, parallel)
	if err != nil {
		return nil, candidates
	}
	modelMount, componentMounts := previewComponentMounts(s.d.Store, selection.Components, nil)
	previewSelection := snapshotTemplateSelection(selection, "")
	var selected *dto.PlacementNode
	for index, candidate := range ranked {
		if index == planning.PoolTrialBudget || selected != nil {
			break
		}
		node := candidate.Node
		entry := dto.PlacementCandidate{NodeUUID: node.UUID, NodeName: names[node.UUID]}
		backend, err := ResolvePartitionBackend(node, req.VRAMLimitGiB, req.PartitionBackend)
		if err != nil {
			entry.Reason = dto.ScheduleReason{Code: "partition_unavailable", Message: err.Error()}
			candidates = append(candidates, entry)
			continue
		}
		plan, planErr := planning.PlanLaunch(ctx, s.d.Store, s.networks(), planning.LaunchRequest{
			StartTimeoutSec: req.StartTimeoutSec,
			Preview:         true, AllowUnresolvedImage: true, ExcludeDeploymentID: excludeDeployment, Engine: engine.Name(), Model: model, Node: node,
			Selection: previewSelection, Placement: placement, Quick: quick, ExtraArgs: userExtraArgs,
			Topology:   planning.DistTopology{NNodes: 1, Role: "standalone"},
			Source:     planning.LaunchSourceFromRequest(image, requestOverrides),
			ModelMount: modelMount, ComponentMounts: componentMounts,
			ServedName: servedName, Sleepable: req.Sleepable, YieldPolicy: requestYield(req.YieldPolicy), ExplainHolders: true,
			MasterAddr: req.MasterAddr, MasterPort: req.MasterPort,
			VRAMLimitBytes: int64(req.VRAMLimitGiB * (1 << 30)), PartitionBackend: backend,
			APIContract: previewSelection.APIContract,
			ModelRules:  store.SourceRules{Pinned: store.StoragePins(req.NodeRequirements), SaveTo: s.previewSaveToStorageID(req.ModelSource, model, node)},
		})
		if refusal, ok := planning.ImageSourceRefusal(planErr, node, image); ok {
			entry.Reason = dto.ScheduleReason{Code: refusal.Code, Params: refusal.Params}
			candidates = append(candidates, entry)
			continue
		}
		if planErr != nil {
			entry.Reason = dto.ScheduleReason{Code: "plan_failed", Message: planErr.Error()}
			candidates = append(candidates, entry)
			continue
		}
		entry.Plan = plan.Output.Result
		entry.Reason = scheduleReasonOf(plan.Output, node, requestYield(req.YieldPolicy))
		if plan.Output.Result.OK && plan.Output.Result.Plan != nil {
			entry.OK = true
			selected = &dto.PlacementNode{UUID: node.UUID, Name: names[node.UUID], Reasons: selectedReasons(plan.Output.Result)}
		}
		candidates = append(candidates, entry)
	}
	return selected, candidates
}

// rankEligible orders the eligible nodes the way the controller orders a
// pool, from one read each of the nodes, the GPUs awake instances hold and
// the model's files.
func (s *Service) rankEligible(eligible []dto.EligibleNode, model *store.Model, req *dto.DeployRequest, parallel planning.ParallelismSpec) ([]planning.PoolCandidate, error) {
	nodes, err := s.d.Store.ListNodes()
	if err != nil {
		return nil, err
	}
	held, err := planning.HeldGPUs(s.d.Store, requestYield(req.YieldPolicy))
	if err != nil {
		return nil, err
	}
	files, err := s.d.Store.ModelFilesForModel(model.ID)
	if err != nil {
		return nil, err
	}
	fileOn := make(map[uint]*store.ModelFile, len(files))
	for i := range files {
		fileOn[files[i].NodeID] = &files[i]
	}
	ok := map[string]bool{}
	for _, entry := range eligible {
		ok[entry.UUID] = entry.Eligible
	}
	var candidates []planning.PoolCandidate
	for i := range nodes {
		if !ok[nodes[i].UUID] {
			continue
		}
		candidates = append(candidates, planning.PoolCandidate{
			Node: &nodes[i], FreeGPUs: planning.FreeHealthyGPUs(&nodes[i], held[nodes[i].ID]), Model: planning.ModelLocalityOf(fileOn[nodes[i].ID]),
		})
	}
	strategy, _ := planning.PlacementStrategyOf(req.PlacementStrategy)
	need := 0
	if req.VRAMLimitGiB <= 0 {
		need, _ = parallel.GPUsPerNode()
		if !parallel.Stated() {
			need = parallel.MinimumGPUs()
		}
	}
	return planning.RankPool(candidates, strategy, need), nil
}

// scheduleReasonOf renders a plan's explanation on node in the wire shape the
// manual preview publishes: a plan that does not fit says why as the
// controller would (planning.Output.Refusal), so a launch that waits for
// holders to yield, or only for sleepers' cards, says so here too.
func scheduleReasonOf(out planning.Output, node *store.Node, yield string) dto.ScheduleReason {
	reason := out.Result.Message
	if !out.Fits() {
		reason = out.Refusal(node, yieldpolicy.Policy{Yield: yield})
	}
	return dto.ScheduleReason{Code: reason.Code, Params: reason.Params, Message: reason.Message, Score: reason.Score}
}

// selectedReasons explains the automatic choice with the chosen plan's own
// reasons; a plan that carries none still says it was the selection and what
// it scored.
func selectedReasons(result planning.ScheduleResult) []dto.ScheduleReason {
	var out []dto.ScheduleReason
	for _, reason := range result.Plan.Reasons {
		out = append(out, dto.ScheduleReason{Code: reason.Code, Params: reason.Params, Message: reason.Message, Score: reason.Score})
	}
	if len(out) == 0 {
		out = append(out, dto.ScheduleReason{Code: "auto_selected", Params: map[string]any{"score": result.Plan.Score}})
	}
	return out
}

// revalidateAutomaticPlacement re-runs the plan the evaluation answered with
// at create time. Resources move - another deployment may have taken the VRAM
// between the evaluation the deployer confirmed and the create - and the
// answer is the conflict the client already knows how to handle, never a
// silent re-placement or a row the controller will fail to schedule.
func (s *Service) revalidateAutomaticPlacement(ctx context.Context, req *dto.DeployRequest, engine deploy.Engine, model *store.Model, selection templateSelection, placement planning.Placement, quick deploy.LaunchConfig, userExtraArgs []string, servedName string, node *store.Node, modelMount string, componentMounts []proto.MountSpec, requestOverrides deploy.ContainerOverrides, weightStorageID uint) error {
	previewSelection := snapshotTemplateSelection(selection, req.Image)
	plan, planErr := planning.PlanLaunch(ctx, s.d.Store, s.networks(), planning.LaunchRequest{
		StartTimeoutSec: req.StartTimeoutSec,
		Preview:         true, Engine: engine.Name(), Model: model, Node: node,
		Selection: previewSelection, Placement: placement, Quick: quick, ExtraArgs: userExtraArgs,
		Topology:   planning.DistTopology{NNodes: 1, Role: "standalone"},
		Source:     planning.LaunchSourceFromRequest(req.Image, requestOverrides),
		ModelMount: modelMount, ComponentMounts: componentMounts,
		ServedName: servedName, Sleepable: req.Sleepable, YieldPolicy: requestYield(req.YieldPolicy),
		MasterAddr: req.MasterAddr, MasterPort: req.MasterPort,
		VRAMLimitBytes: int64(req.VRAMLimitGiB * (1 << 30)), PartitionBackend: req.PartitionBackend,
		APIContract: previewSelection.APIContract,
		ModelRules:  store.SourceRules{Pinned: store.StoragePins(req.NodeRequirements), SaveTo: weightStorageID},
	})
	detail := "the evaluated placement no longer holds"
	if planErr != nil {
		detail = planErr.Error()
	} else if plan.Output.PlacementError != nil {
		detail = plan.Output.PlacementError.Error()
	} else if plan.Output.Fits() {
		return nil
	} else if refusal := plan.Output.Refusal(node, yieldpolicy.Policy{Yield: requestYield(req.YieldPolicy)}); refusal.Code != "" {
		detail = refusal.Message
		if detail == "" {
			detail = refusal.Code
		}
	}
	return wrapProductError(messages.ErrorAutomaticPlacementStale,
		"the evaluated placement no longer fits the node",
		map[string]any{"node": node.UUID, "detail": detail}, nil)
}
