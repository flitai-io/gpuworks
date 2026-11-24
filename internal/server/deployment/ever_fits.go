package deployment

import (
	"context"
	"fmt"
	"strings"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/scheduler"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// checkCreateCanEverFit applies checkPoolCanEverFit to a create: only a
// single-node deployment is placed from a pool; a multi-node or PD one names
// its nodes rank by rank and is checked where they are.
func (s *Service) checkCreateCanEverFit(ctx context.Context, req *dto.DeployRequest, pd bool, reqs map[string][]string, engine deploy.Engine, model *store.Model, selection templateSelection, placement planning.Placement, quick deploy.LaunchConfig, userExtraArgs []string, modelMount string, componentMounts []proto.MountSpec) error {
	if pd || req.NNodes > 1 {
		return nil
	}
	overrides, _ := RequestContainerOverrides(req.ContainerOverrides)
	return s.checkPoolCanEverFit(ctx, req, reqs, engine, model, selection, placement, quick, userExtraArgs, strings.TrimSpace(req.ServedName), modelMount, componentMounts, overrides)
}

// checkPoolCanEverFit refuses a single-node deployment that no node of its
// pool could run even with nothing else on it: too few cards, too little
// memory per card, a dtype the cards do not run, a node that may not serve.
// Such a deployment would wait forever; the request is the moment to say so.
// What only waiting fixes - cards taken, a card unhealthy, a disk full, a node
// offline - never refuses here (planning.Input.Unloaded).
func (s *Service) checkPoolCanEverFit(ctx context.Context, req *dto.DeployRequest, reqs map[string][]string, engine deploy.Engine, model *store.Model, selection templateSelection, placement planning.Placement, quick deploy.LaunchConfig, userExtraArgs []string, servedName, modelMount string, componentMounts []proto.MountSpec, overrides deploy.ContainerOverrides) error {
	if _, pinned := planning.PinnedNode(reqs); !pinned {
		placement.GPUIndexes = nil // the pool's scheduler picks cards
	}
	return s.checkEverFits(ctx, reqs, s.poolLaunch(req, engine, model, selection, placement, quick, userExtraArgs, servedName, modelMount, componentMounts, overrides))
}

// poolLaunch is the launch a single-node request would run on a node of its
// pool: what the create's refusal (checkPoolCanEverFit) and the evaluation's
// node list (markNeverFitting) plan unloaded, so both judge "could this ever
// fit" on one request.
func (s *Service) poolLaunch(req *dto.DeployRequest, engine deploy.Engine, model *store.Model, selection templateSelection, placement planning.Placement, quick deploy.LaunchConfig, userExtraArgs []string, servedName, modelMount string, componentMounts []proto.MountSpec, overrides deploy.ContainerOverrides) func(*store.Node) (planning.LaunchRequest, deploy.CatalogSelection, error) {
	previewSelection := snapshotTemplateSelection(selection, req.Image)
	return func(node *store.Node) (planning.LaunchRequest, deploy.CatalogSelection, error) {
		// A memory-limited request that names no dGPU backend takes the one
		// the node it lands on offers, as the trial of that node settles it
		// (evaluateAutomaticPlacement). A node that offers none keeps the
		// request's and is refused for it.
		partitionBackend := req.PartitionBackend
		if settled, err := ResolvePartitionBackend(node, req.VRAMLimitGiB, req.PartitionBackend); err == nil {
			partitionBackend = settled
		}
		return planning.LaunchRequest{
			StartTimeoutSec: req.StartTimeoutSec,
			Engine:          engine.Name(), Model: model, Node: node,
			Selection: previewSelection, Placement: placement, Quick: quick, ExtraArgs: userExtraArgs,
			Topology:   planning.DistTopology{NNodes: 1, Role: "standalone"},
			Source:     planning.LaunchSourceFromRequest(req.Image, overrides),
			ModelMount: modelMount, ComponentMounts: componentMounts,
			ServedName: servedName, Sleepable: req.Sleepable, PriorityScheduling: requestsPriorityScheduling(req), YieldPolicy: requestYield(req.YieldPolicy),
			VRAMLimitBytes: int64(req.VRAMLimitGiB * (1 << 30)), PartitionBackend: partitionBackend,
			APIContract: previewSelection.APIContract,
			// A model larger than the storage's cache cap never fits; the
			// storage's other refusals only wait (planning.Input.Unloaded).
			ModelRules: store.SourceRules{Pinned: store.StoragePins(req.NodeRequirements), SaveTo: s.previewSaveToStorageID(req.ModelSource, model, node)},
		}, previewSelection, nil
	}
}

// markNeverFitting turns the eligible nodes that could not run build's launch
// even with nothing else on them ineligible, each with the reason a create
// naming it is refused for (checkPoolCanEverFit), so the node list does not
// offer a node the create refuses. fleet is the nodes eligible was read from
// (evaluateEligibleNodes), so no node is read again. Nodes are judged by kind
// (planning.NeverFitting): one unloaded plan per hardware shape and set of
// storages, at most planning.NeverFittingBudget of them; what is not judged
// stays on offer. fits reports that some node judged could run the launch,
// which is all the pool's own check (checkPoolCanEverFit) would establish.
func (s *Service) markNeverFitting(ctx context.Context, eligible []dto.EligibleNode, fleet []*store.Node, build func(*store.Node) (planning.LaunchRequest, deploy.CatalogSelection, error)) (fits bool) {
	offered := make(map[string]bool, len(eligible))
	for _, entry := range eligible {
		offered[entry.UUID] = entry.Eligible
	}
	nodes := make([]*store.Node, 0, len(fleet))
	for _, node := range fleet {
		if offered[node.UUID] {
			nodes = append(nodes, node)
		}
	}
	if len(nodes) == 0 {
		return false
	}
	// The storages that apply to a node, and the cache cap each has there,
	// are the node's own: a model larger than a cap never fits that node,
	// whatever a node of the same hardware answers.
	storages, err := s.d.Store.EffectiveStorages(nodes)
	if err != nil {
		if s.d.Log != nil {
			s.d.Log.Warn("evaluate left the node list unjudged: the nodes' storages could not be read", "err", err)
		}
		return false
	}
	ownStorages := func(node *store.Node) string {
		var key strings.Builder
		for _, storage := range storages[node.ID] {
			fmt.Fprintf(&key, "%d:%d;", storage.Storage.ID, storage.CacheMaxBytes)
		}
		return key.String()
	}
	never, fits, failed := planning.NeverFitting(ctx, s.networks(), nodes, ownStorages, func(node *store.Node) (*scheduler.Reason, error) {
		launch, layer, err := build(node)
		if err != nil {
			return nil, err
		}
		plan, refusal, err := planning.PlanTemplateLaunch(ctx, s.d.Store, s.networks(), node, launch.UnloadedPreview(), layer)
		return planning.UnloadedRefusal(node, launch, plan, refusal, err)
	})
	if failed != nil && ctx.Err() == nil && s.d.Log != nil {
		s.d.Log.Warn("evaluate left some nodes unjudged: their launch could not be planned", "err", failed)
	}
	for i := range eligible {
		if refusal, ok := never[eligible[i].UUID]; ok {
			eligible[i].Eligible = false
			eligible[i].Reasons = append(eligible[i].Reasons, dto.ScheduleReason{Code: refusal.Code, Params: refusal.Params, Message: refusal.Message})
		}
	}
	return fits
}

// checkEverFits is planning.EverFits over every node, as the product error a
// request is refused with.
func (s *Service) checkEverFits(ctx context.Context, reqs map[string][]string, build func(*store.Node) (planning.LaunchRequest, deploy.CatalogSelection, error)) error {
	nodes, err := s.CandidateNodes(reqs)
	if err != nil {
		return err
	}
	never, err := planning.EverFits(ctx, s.d.Store, s.networks(), nodes, s.resolveForRequirements(nodes, reqs), reqs, build)
	if err != nil || never == nil {
		return err
	}
	if err := dgpuStartupMemoryError(never.Reason); err != nil {
		return err
	}
	params := map[string]any{"pool": never.Pool, "shapes": never.Shapes}
	if never.Node != "" {
		params["node"], params["reason"], params["detail"], params["reason_params"] = never.Node, never.Reason.Code, never.Reason.Message, never.Reason.Params
	}
	// The scheduler decided the estimate is what refused the node and no
	// template contract replaced it; the refusal lifts that to its own param
	// so a client states the hint without opening the node's reason.
	if hint, _ := never.Reason.Params[scheduler.ParamMemoryContractHint].(bool); hint {
		params[scheduler.ParamMemoryContractHint] = true
	}
	return productError(messages.ErrorDeploymentNeverFits, "no node the deployment may run on could fit a replica even with nothing else on it", params)
}

// CandidateNodes reads the nodes reqs may admit, which is every node unless
// they name nodes: then no other node can satisfy them, and reading a whole
// fleet per request only to discard all but the named ones made each create
// cost a full node scan.
func (s *Service) CandidateNodes(reqs map[string][]string) ([]*store.Node, error) {
	var rows []store.Node
	var err error
	if named := planning.NamedNodes(reqs); len(named) > 0 {
		rows, err = s.d.Store.NodesByUUID(named)
	} else {
		rows, err = s.d.Store.ListNodes()
	}
	if err != nil {
		return nil, err
	}
	nodes := make([]*store.Node, 0, len(rows))
	for i := range rows {
		nodes = append(nodes, &rows[i])
	}
	return nodes, nil
}

// CheckStoredCanEverFit is checkPoolCanEverFit for a stored deployment whose
// requirements are about to become reqs: its replicas launch with the stored
// configuration, exactly as the controller trials a pool candidate
// (planning.DeploymentLaunch).
func (s *Service) CheckStoredCanEverFit(ctx context.Context, dep *store.Deployment, reqs map[string][]string) error {
	model, err := s.d.Store.GetModel(dep.ModelID)
	if err != nil {
		return err
	}
	return s.checkEverFits(ctx, reqs, func(node *store.Node) (planning.LaunchRequest, deploy.CatalogSelection, error) {
		launch, layer, _, err := planning.DeploymentLaunch(dep, reqs, node, model, nil)
		if err != nil {
			return launch, layer, err
		}
		// The configuration is being written, so it is judged as a create's
		// is; only a launch of what is already stored runs as written.
		launch.AsStored = false
		launch.Topology, err = planning.RankTopology(node, nil, planning.GroupRank{Ranks: 1})
		return launch, layer, err
	})
}
