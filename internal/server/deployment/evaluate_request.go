package deployment

import (
	"fmt"
	"strings"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// collectEvaluationRequestErrors runs every independent request rule in order.
// Some rules also normalize defaults; keep those effects before template and
// launch resolution, even when an earlier rule already found a field error.
func (s *Service) collectEvaluationRequestErrors(req *dto.DeployRequest, errs *evaluateFieldErrors) map[string][]string {
	errs.add("replicas", ValidateReplicaShape(req))
	errs.add("start_timeout_sec", ValidateStartTimeout(req))
	errs.add("health_check_threshold_sec", ValidateHealthCheckThreshold(req))
	_, pdErr := s.preparePDDeployRequest(req, "")
	errs.add(fieldPDPlacement, pdErr)
	_, placementErr := planning.ParsePlacement(req.Placement)
	errs.add("placement", placementErr)
	reqs, reqsErr := requestRequirements(req)
	if !errs.add("node_requirements", reqsErr) {
		errs.add("node_requirements", s.validateRequestNodes(req, reqs))
	}
	_, strategyErr := planning.PlacementStrategyOf(req.PlacementStrategy)
	errs.add("placement_strategy", strategyErr)
	return reqs
}

// evaluationLaunchRequest is the DTO boundary shared by the displayed plan
// and per-card eligibility. All command/platform facts are resolved afterward
// by planning; this layer never constructs Params or command fragments.
func evaluationLaunchRequest(env evaluatePlanEnv, node *store.Node) planning.LaunchRequest {
	req := env.request
	selection := snapshotTemplateSelection(env.selection, env.response.Image)
	selection.ExtraArgs = env.templateResidual
	return planning.LaunchRequest{
		StartTimeoutSec: req.StartTimeoutSec,
		Preview:         true, AllowUnresolvedImage: true, ExcludeDeploymentID: env.excludeDeployment, Engine: env.engine.Name(), Model: env.model, Node: node,
		Selection: selection, Placement: env.placement, Quick: env.quick, ExtraArgs: env.userExtraArgs,
		Topology:   planning.DistTopology{NNodes: max(req.NNodes, 1), Role: "standalone"},
		Source:     planning.LaunchSourceFromRequest(env.response.Image, env.requestOverrides),
		ModelMount: env.modelMount, ComponentMounts: env.componentMounts,
		ServedName: env.servedName, Sleepable: req.Sleepable, PriorityScheduling: requestsPriorityScheduling(req), YieldPolicy: requestYield(req.YieldPolicy), ExplainHolders: true,
		MasterAddr: req.MasterAddr, MasterPort: req.MasterPort,
		VRAMLimitBytes: int64(req.VRAMLimitGiB * (1 << 30)), PartitionBackend: req.PartitionBackend,
		RuntimeImageCached: env.nodeImageCached, APIContract: selection.APIContract,
		ModelRules: store.SourceRules{Pinned: store.StoragePins(req.NodeRequirements)},
	}
}

func (s *Service) evaluateTemplateSelection(req *dto.DeployRequest, hardware templateHardware, placementFailed bool) (templateSelection, error) {
	// Keep the candidate predicates before the leader determines the final spec.
	if placementFailed && req.TemplateID != 0 && strings.TrimSpace(req.TemplateSpecName) == "" {
		model, err := s.d.Store.InferenceModelByID(req.TemplateID)
		if err != nil {
			return templateSelection{}, err
		}
		if _, err := decodeUnsupported(*model); err != nil {
			return templateSelection{}, err
		}
		_, candidates, err := matchingSpecCandidates(*model, req.TemplateSpecSource, "", req.Engine, req.Scenario)
		if err != nil {
			return templateSelection{}, err
		}
		if len(candidates) == 0 {
			return templateSelection{}, fmt.Errorf("no inference specs match the selected source, engine and scenario")
		}
		return templateSelection{Model: model, CandidateSpecs: candidates}, nil
	}
	if placementFailed && req.TemplateID != 0 {
		// The named contract can be resolved independently of a partial pin.
		hardware = templateHardware{}
	}
	selection, err := s.applyTemplateDefaults(req, hardware)
	if err != nil {
		return templateSelection{}, err
	}
	return selection, nil
}

// Feature extraction uses model architecture but does not need persistent
// artifact identities. A failed placement must never register weights.
func (s *Service) featurePreviewModel(id uint, selection templateSelection) *store.Model {
	if id != 0 {
		if model, err := s.d.Store.GetModel(id); err == nil {
			return model
		}
	}
	if selection.Model != nil {
		return &store.Model{Arch: selection.Model.Architecture}
	}
	return nil
}
