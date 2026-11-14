package deployment

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// ValidateReplicaShape gives evaluation and create the same replica and rank
// contract before either path can register a model or acquire an image.
func ValidateReplicaShape(req *dto.DeployRequest) error {
	req.RoutingPolicy = deploy.NormalizeRoutingPolicy(req.RoutingPolicy)
	if !deploy.ValidRoutingPolicy(req.RoutingPolicy) {
		return productError(messages.ErrorRoutingPolicyInvalid, "routing_policy is invalid", map[string]any{"routing_policy": req.RoutingPolicy})
	}
	if req.NNodes <= 0 {
		req.NNodes = 1
	}
	if req.Replicas < 0 {
		return productError(messages.ErrorReplicasNegative, "replicas must be non-negative", nil)
	}
	if req.Replicas == 0 {
		req.Replicas = 1
	}
	if req.Replicas > planning.MaxReplicas {
		return productError(messages.ErrorReplicasTooMany, fmt.Sprintf("replicas must be at most %d", planning.MaxReplicas), map[string]any{"maximum": planning.MaxReplicas})
	}
	if req.NNodes > 1 && req.Replicas != 1 {
		return productError(messages.ErrorDistributedReplicaCount, "distributed deployments support exactly one replica", map[string]any{"required": 1})
	}
	return nil
}

// ValidateStartTimeout gives evaluation and create the same rule for a
// deployment's own health budget: unset, or inside the policy's bounds.
func ValidateStartTimeout(req *dto.DeployRequest) error {
	if req.StartTimeoutSec == 0 || store.ValidStartTimeoutSec(req.StartTimeoutSec) {
		return nil
	}
	return productError(messages.ErrorFieldInvalid, "start_timeout_sec is out of range", map[string]any{
		"field":  "start_timeout_sec",
		"detail": fmt.Sprintf("start_timeout_sec must be 0 (use the cluster default) or between %d and %d seconds", store.DeploymentStartTimeoutSecLo, store.DeploymentStartTimeoutSecHi),
	})
}

// AutomaticPickAttempts bounds how often a create that named no node picks
// again when the node it picked was taken before the create re-checked it.
const AutomaticPickAttempts = 3

// Create creates a deployment from a create request. An automatic
// single-node create that names no node leaves the pick to the server, so
// when that pick is taken between the evaluation and the create's own
// re-check (another create placing replicas on the same cards a moment
// earlier), the server picks again instead of refusing with
// AUTOMATIC_PLACEMENT_STALE: there is no client choice to report as stale. A
// create that names its node keeps the conflict.
func (s *Service) Create(ctx context.Context, req dto.DeployRequest) (*store.Deployment, error) {
	if !leavesNodeToServer(&req) {
		return s.createOnce(ctx, req)
	}
	for attempt := 1; ; attempt++ {
		// Each attempt starts from the request as it was sent: an attempt
		// normalizes and fills in the request it works on.
		created, err := s.createOnce(ctx, cloneRequest(req))
		if attempt >= AutomaticPickAttempts || !pickWentStale(err) {
			return created, err
		}
		select {
		case <-ctx.Done():
		case <-time.After(time.Duration(attempt) * 200 * time.Millisecond):
		}
		if ctx.Err() != nil {
			return created, err
		}
	}
}

// pickWentStale reports whether a create was refused AUTOMATIC_PLACEMENT_STALE.
func pickWentStale(err error) bool {
	return KindOf(err) == KindConflict && containsProductCode(err, messages.ErrorAutomaticPlacementStale)
}

// CreateRowError is a refused deployment row as its caller reads it: a served
// name another deployment holds is a conflict, anything else the store's
// failure.
func CreateRowError(err error) error {
	var inUse *store.ServedNameInUseError
	if errors.As(err, &inUse) {
		return conflict(wrapProductError(messages.ErrorServedNameInUse, err.Error(), map[string]any{
			"served_name": inUse.ServedName, "deployment_id": inUse.HolderID, "deployment": inUse.HolderName,
		}, err))
	}
	return internal(err)
}

// createOnce is one attempt at Create.
func (s *Service) createOnce(ctx context.Context, req dto.DeployRequest) (*store.Deployment, error) {
	if err := validateNames(req.Name, req.ServedName); err != nil {
		return nil, invalid(err)
	}
	if strings.TrimSpace(req.TargetVendor) != "" && accelerator.NormalizeVendor(req.TargetVendor) == "" {
		return nil, invalid(productError(messages.ErrorTargetVendorInvalid, "target_vendor is not supported", map[string]any{"vendor": req.TargetVendor}))
	}
	unitsAuto, err := s.autoPlaceRequest(&req)
	if err != nil {
		return nil, unitPlacementKind(err)
	}
	defaultUnitStrategy(&req)
	if err := ValidateReplicaShape(&req); err != nil {
		return nil, invalid(err)
	}
	if err := ValidateStartTimeout(&req); err != nil {
		return nil, invalid(err)
	}
	if err := ValidateHealthCheckThreshold(&req); err != nil {
		return nil, invalid(err)
	}
	if err := s.ValidateMountModels(&req); err != nil {
		return nil, invalid(err)
	}
	if err := checkManualCommandRanks(&req); err != nil {
		return nil, invalid(err)
	}
	pdReq, err := s.preparePDDeployRequest(&req, "")
	if err == nil && pdReq != nil {
		_, err = s.pdGroup(pdReq)
	}
	if err != nil {
		return nil, invalid(err)
	}
	if err := s.pickAutomaticNode(ctx, &req); err != nil {
		return nil, conflict(err)
	}
	reqs, strategy, err := s.RequestPlacement(&req)
	if err != nil {
		return nil, err
	}
	profileNode, _ := s.d.Store.NodeWithGPUs(templateTargetNodeUUID(req.NodeUUID, req.NodeUUIDs))
	selection, err := s.applyTemplateDefaults(&req, nodeTemplateHardware(profileNode))
	if err == nil {
		err = checkPDSpec(selection, placementPD(&req))
	}
	if err != nil {
		return nil, invalid(err)
	}
	eng, err := deploy.SelectEngine(req.Engine)
	if err == nil {
		err = pdReq.checkEngine(eng.Name())
	}
	if err != nil {
		return nil, invalid(err)
	}
	preserveEngineModelName(&req)
	// The same materialization the deploy form previewed. What is stored from
	// here on is the launch configuration itself, never the four inputs it was
	// assembled from.
	targetVendor := accelerator.NormalizeVendor(req.TargetVendor)
	if targetVendor == "" {
		targetVendor = planning.RequirementVendor(reqs)
	}
	launch, err := s.materializeCreateLaunch(eng.Name(), &req, selection, DeployTargetCards(profileNode, req.Placement), s.requestEngineVersion(eng.Name(), &req, selection, targetVendor, profileNode))
	if err != nil {
		return nil, invalid(err)
	}
	if pdReq != nil {
		if err := planning.CheckPDArgs(eng.Name(), launch.TemplateResidual, launch.UserResidual); err != nil {
			return nil, invalid(err)
		}
	}
	// A multi-node group settles its nodes and parallelism first: an
	// unstated TP and PP default from the nodes, and the launch preflight
	// below measures the result. Nothing is persisted by this step.
	dp, err := deploy.PlacementDataParallel(eng.Name(), launch.Quick.DP, launch.TemplateResidual, launch.UserResidual)
	if err != nil {
		return nil, invalid(err)
	}
	var distNodes []*store.Node
	if req.NNodes > 1 {
		distNodes, err = s.PrepareDistributedDeployRequest(&req, dp)
		if err != nil {
			return nil, invalid(err)
		}
	}
	// Resolve request-owned command, environment and mounts before template
	// model registration or image acquisition can mutate persistent state.
	// Evaluate runs the same preflight (evaluateLaunchPreflight).
	preflight, err := s.prepareLaunchPreflight(eng, &req, selection, launch, dp, targetVendor, profileNode, pdReq)
	if err != nil {
		return nil, invalid(err)
	}
	modelMount, componentMounts, placement := preflight.modelMount, preflight.componentMounts, preflight.placement
	imageNodes := append([]*store.Node{profileNode}, distNodes...)
	if pdReq != nil {
		imageNodes = append(imageNodes, pdReq.nodes...)
	}
	if err := s.admitImagePlatforms(eng.Name(), selection, req.Image, imageNodes); err != nil {
		return nil, invalid(err)
	}
	imageFor := func(node *store.Node) string {
		return s.rankImage(eng.Name(), selection, req.Image, node).Image
	}
	if err := preflight.run(&req, selection, profileNode, distNodes, pdReq, imageFor); err != nil {
		return nil, invalid(err)
	}
	model, err := s.resolveDeploymentModelFromTemplate(ctx, req.ModelID, &selection, req.TemplateID)
	if err != nil {
		return nil, invalid(err)
	}
	launch, err = materializeDeployRequest(eng.Name(), &req, selection, DeployTargetCards(profileNode, req.Placement), s.requestEngineVersion(eng.Name(), &req, selection, targetVendor, profileNode))
	if err != nil {
		return nil, invalid(err)
	}
	selection.ExtraArgs = launch.TemplateResidual
	quick, userExtraArgs := launch.Quick, deploy.FormatArgsText(launch.UserResidual)
	served := req.ServedName
	if served == "" {
		served = model.Name
	}
	name := req.Name
	if name == "" {
		name = served
	}
	// The served name is judged first, as the name defaults to it.
	if err := TextFieldError("served_name", served, MaxNameRunes); err != nil {
		return nil, invalid(err)
	}
	if err := TextFieldError("name", name, MaxNameRunes); err != nil {
		return nil, invalid(err)
	}
	model, templateConfig, err := s.initTemplateModelInferenceConfig(&selection, model)
	if err != nil {
		return nil, internal(err)
	}
	// The candidate's operations say what a check asks.
	healthCheck, healthThreshold, err := CreateHealthCheck(&req, model)
	if err != nil {
		return nil, invalid(err)
	}
	// Warnings are surfaced by evaluate before submit; keep the same helper in
	// this path as a guard for API clients that submit without evaluating.
	_ = deploy.PerformanceWarnings(quick, userExtraArgs, model.Arch, 0)
	if pdReq != nil {
		distNodes = pdReq.otherNodes()
	}
	var node *store.Node
	if sleepErr := planning.SleepDeclarable(req.Sleepable, eng.Name(), targetVendor); sleepErr != nil {
		return nil, invalid(SleepDeclarationError(sleepErr))
	}
	yieldPolicy, err := createYieldPolicy(req, eng.Name(), pdReq != nil)
	if err != nil {
		return nil, invalid(err)
	}
	if req.NNodes > 1 && pdReq == nil {
		node, err = s.d.Store.NodeWithGPUs(distNodes[0].UUID)
		if err != nil {
			return nil, invalid(err)
		}
	} else {
		node, err = s.RequestNode(req.NodeUUID)
		if err != nil {
			return nil, invalid(err)
		}
	}
	req.PartitionBackend, err = ResolvePartitionBackend(node, req.VRAMLimitGiB, req.PartitionBackend)
	if err != nil {
		return nil, invalid(wrapProductError(messages.ErrorDistributedPartitionBackendUnavailable, err.Error(), map[string]any{"node": node.UUID, "detail": err.Error()}, err))
	}
	for _, distNode := range distNodes {
		if _, err := ResolvePartitionBackend(distNode, req.VRAMLimitGiB, req.PartitionBackend); err != nil {
			message := fmt.Sprintf("node %s: %s", distNode.UUID, err)
			return nil, invalid(wrapProductError(messages.ErrorDistributedPartitionBackendUnavailable, message, map[string]any{"node": distNode.UUID, "detail": err.Error()}, err))
		}
	}
	if err := s.admitImagePlatforms(eng.Name(), selection, req.Image, append([]*store.Node{node}, distNodes...)); err != nil {
		return nil, invalid(err)
	}
	targets := append([]*store.Node{node}, distNodes...)
	weightStorage, err := s.resolveSaveToStorage(req.ModelSource, model, targets)
	if err != nil {
		return nil, conflict(err)
	}
	// What the request names is what the deployment keeps: 0 leaves where a
	// node saves a model it must fetch to the default order.
	saveTo := s.previewSaveToStorageID(req.ModelSource, model, node)
	components, err := s.templateComponents(selection.Components)
	if err != nil {
		return nil, internal(err)
	}
	admitted := []*store.Model{model}
	for _, component := range components {
		if _, err := s.resolveSaveToStorage(req.ModelSource, component.model, targets); err != nil {
			return nil, conflict(err)
		}
		admitted = append(admitted, component.model)
	}
	// A template component no target can read yet follows the primary into
	// the shared storage it already sits on, rather than into every node's
	// local storage; the evaluate forecast shows the same plans. The
	// deployment still keeps only what the request named.
	componentPlans, err := s.planComponents(req.ModelSource, model, components, weightStorage, store.StoragePins(reqs), targets)
	if err != nil {
		return nil, internal(err)
	}
	rules := store.SourceRules{Pinned: store.StoragePins(reqs), SaveTo: saveTo}
	for _, admitted := range admitted {
		if err := s.admitDeploymentModel(ctx, admitted, rules, targets); err != nil {
			return nil, conflict(err)
		}
	}
	imageResolution := s.rankImage(eng.Name(), selection, req.Image, node)
	if strings.TrimSpace(imageResolution.Image) == "" {
		code, params := messages.ErrorAcquisitionImageRequired, map[string]any(nil)
		if len(imageResolution.Warnings) > 0 {
			code, params = imageResolution.Warnings[0].Code, imageResolution.Warnings[0].Params
		}
		return nil, invalid(productError(code, "no runtime image is available for the target node", params))
	}
	req.Image = imageResolution.Image
	reqs = planning.WithNodeVendor(reqs, node)
	if err := s.checkCreateCanEverFit(ctx, &req, pdReq != nil, reqs, eng, model, selection, placement, quick, launch.UserResidual, modelMount, componentMounts); err != nil {
		return nil, invalid(err)
	}
	// An automatically placed create re-runs the plan the evaluation answered
	// with: resources may have moved since, and the answer is the same
	// conflict the client handles for stale labels - not a doomed row.
	if IsAutomaticPlacement(&req) && req.NNodes <= 1 && pdReq == nil {
		requestOverrides, _ := RequestContainerOverrides(req.ContainerOverrides)
		if err := s.revalidateAutomaticPlacement(ctx, &req, eng, model, selection, placement, quick, launch.UserResidual, strings.TrimSpace(req.ServedName), node, modelMount, componentMounts, requestOverrides, saveTo); err != nil {
			return nil, conflict(err)
		}
	}
	snapshot, err := catalogSelectionDocument(selection, req.Image)
	if err != nil {
		return nil, internal(err)
	}
	dep := &store.Deployment{
		Name:                     name,
		ModelID:                  model.ID,
		TemplateID:               req.TemplateID,
		TemplateSpecSource:       req.TemplateSpecSource,
		TemplateSpecName:         req.TemplateSpecName,
		TemplateScenario:         req.Scenario,
		TemplateDeviated:         selection.Deviated,
		CatalogSelectionSnapshot: snapshot,
		NodeID:                   node.ID,
		ServedName:               served,
		SaveToStorageID:          saveTo,
		Engine:                   eng.Name(),
		Image:                    req.Image,
		QuickParams:              LaunchConfigJSON(quick),
		ExtraArgs:                userExtraArgs,
		ContainerOverrides:       jsonStr(req.ContainerOverrides),
		Placement:                jsonStr(StoredPlacement(req.Placement, IsAutomaticPlacement(&req) && req.NNodes <= 1 && pdReq == nil)),
		PlacementStrategy:        strategy,
		UnitsAuto:                unitsAuto,
		DistGroup:                req.DistGroup,
		DistRole:                 req.DistRole,
		NNodes:                   req.NNodes,
		NodeRank:                 req.NodeRank,
		MasterAddr:               req.MasterAddr,
		MasterPort:               req.MasterPort,
		NodeIDs: func() string {
			ids := []uint{node.ID}
			if req.NNodes > 1 {
				ids = ids[:0]
			}
			for _, n := range distNodes {
				ids = append(ids, n.ID)
			}
			return jsonStr(ids)
		}(),
		Sleepable:               req.Sleepable,
		HealthCheck:             healthCheck,
		HealthCheckThresholdSec: healthThreshold,
		TokenizeDisabled:        req.TokenizeEnabled != nil && !*req.TokenizeEnabled,
		VRAMLimitBytes:          int64(req.VRAMLimitGiB * (1 << 30)),
		StartTimeoutSec:         req.StartTimeoutSec,
		PartitionBackend:        req.PartitionBackend,
		Replicas:                req.Replicas,
		RoutingPolicy:           req.RoutingPolicy,
		State:                   store.DeployActive,
	}
	dep.SetPolicy(yieldPolicy)
	var created *store.Deployment
	if templateConfig.modelID == 0 {
		created, err = s.d.Store.CreateDeploymentWithRequirements(dep, reqs)
	} else {
		created, err = s.d.Store.CreateDeploymentWithTemplateInferenceConfig(dep, reqs, templateConfig.modelID, templateConfig.operations, templateConfig.inputModalities)
	}
	if err != nil {
		return nil, CreateRowError(err)
	}
	// Start the download only after validation and persistence: a rejected
	// create must not leave an unowned transfer running.
	if err := s.startSharedModelDownload(model, weightStorage, targets, created.ID); err != nil {
		_ = s.d.Store.DeleteDeployment(created.ID)
		return nil, err
	}
	if err := s.startComponentDownloads(componentPlans, targets, created.ID); err != nil {
		_ = s.d.Store.DeleteDeployment(created.ID)
		return nil, err
	}
	return created, nil
}
