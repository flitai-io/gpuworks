package deployment

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/server/yieldpolicy"
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

// CreatedDeployment contains the persisted row and its create-time warnings.
type CreatedDeployment struct {
	*store.Deployment
	Warnings []messages.Warning
}

// Create creates a deployment from a create request. An automatic
// single-node create that names no node leaves the pick to the server, so
// when that pick is taken between the evaluation and the create's own
// re-check (another create placing replicas on the same cards a moment
// earlier), the server picks again instead of refusing with
// AUTOMATIC_PLACEMENT_STALE: there is no client choice to report as stale. A
// create that names its node keeps the conflict.
func (s *Service) Create(ctx context.Context, req dto.DeployRequest) (*CreatedDeployment, error) {
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

// CreateRowError reports conflicts detected while committing a deployment row.
func CreateRowError(err error) error {
	if errors.Is(err, store.ErrDeploymentTemplateSelectionGone) {
		return conflict(wrapProductError(messages.ErrorDeploymentTemplateSelectionGone, err.Error(), nil, err))
	}
	var inUse *store.ServedNameInUseError
	if errors.As(err, &inUse) {
		return conflict(wrapProductError(messages.ErrorServedNameInUse, err.Error(), map[string]any{
			"served_name": inUse.ServedName, "deployment_id": inUse.HolderID, "deployment": inUse.HolderName,
		}, err))
	}
	return internal(err)
}

// createOnce is one attempt at Create: its steps in order, the first error
// ending it. No deployment row is written before persist.
func (s *Service) createOnce(ctx context.Context, req dto.DeployRequest) (*CreatedDeployment, error) {
	c := &createState{s: s, ctx: ctx, req: &req}
	for _, step := range []func() error{
		c.checkRequest,
		c.resolveTemplate,
		c.materializeLaunch,
		c.runPreflight,
		c.resolveModel,
		c.resolveNodes,
		c.admitWeights,
		c.resolveImage,
		c.persist,
		c.startDownloads,
	} {
		if err := step(); err != nil {
			return nil, err
		}
	}
	return &CreatedDeployment{Deployment: c.created, Warnings: c.warnings}, nil
}

// createState is what the steps of one Create attempt share: what the steps
// so far have settled about the request. The request itself is rewritten as
// it is normalized (its units, node, ranks, image and partition backend).
type createState struct {
	s   *Service
	ctx context.Context
	req *dto.DeployRequest

	unitsAuto    bool
	pdReq        *pdRequest
	reqs         map[string][]string
	strategy     store.PlacementStrategy
	profileNode  *store.Node
	selection    templateSelection
	eng          deploy.Engine
	targetVendor string

	launch          deploy.Materialized
	dp              int
	distNodes       []*store.Node
	preflight       launchPreflight
	modelMount      string
	componentMounts []proto.MountSpec
	placement       planning.Placement

	model              *store.Model
	quick              deploy.LaunchConfig
	userExtraArgs      string
	served, name       string
	templateConfig     templateInferenceConfig
	healthCheck        bool
	healthThreshold    int
	priorityScheduling bool
	yieldPolicy        yieldpolicy.Policy

	// node is the deployment's node, rank zero of a multi-node group;
	// targets are every node the launch lands on.
	node      *store.Node
	targets   []*store.Node
	rankNodes []*store.Node

	weights        []weightTarget
	weightStorage  *store.Storage
	saveTo         uint
	components     []templateComponent
	componentPlans []componentPlan

	created  *store.Deployment
	warnings []messages.Warning
}

// persist writes the deployment row.
func (c *createState) persist() error {
	snapshot, err := catalogSelectionDocument(c.selection, c.req.Image)
	if err != nil {
		return internal(err)
	}
	dep := &store.Deployment{
		Name:                     c.name,
		ModelID:                  c.model.ID,
		TemplateID:               c.req.TemplateID,
		TemplateSpecSource:       c.req.TemplateSpecSource,
		TemplateSpecName:         c.req.TemplateSpecName,
		TemplateScenario:         c.req.Scenario,
		TemplateDeviated:         c.selection.Deviated,
		CatalogSelectionSnapshot: snapshot,
		NodeID:                   c.node.ID,
		ServedName:               c.served,
		SaveToStorageID:          c.saveTo,
		Engine:                   c.eng.Name(),
		Image:                    c.req.Image,
		QuickParams:              LaunchConfigJSON(c.quick),
		ExtraArgs:                c.userExtraArgs,
		ContainerOverrides:       jsonStr(c.req.ContainerOverrides),
		Placement:                jsonStr(StoredPlacement(c.req.Placement, IsAutomaticPlacement(c.req) && c.req.NNodes <= 1 && c.pdReq == nil)),
		PlacementStrategy:        c.strategy,
		UnitsAuto:                c.unitsAuto,
		DistGroup:                c.req.DistGroup,
		DistRole:                 c.req.DistRole,
		NNodes:                   c.req.NNodes,
		NodeRank:                 c.req.NodeRank,
		MasterAddr:               c.req.MasterAddr,
		MasterPort:               c.req.MasterPort,
		NodeIDs: func() string {
			ids := []uint{c.node.ID}
			if c.req.NNodes > 1 {
				ids = ids[:0]
			}
			for _, n := range c.distNodes {
				ids = append(ids, n.ID)
			}
			return jsonStr(ids)
		}(),
		Sleepable:               c.req.Sleepable,
		HealthCheck:             c.healthCheck,
		HealthCheckThresholdSec: c.healthThreshold,
		TokenizeDisabled:        c.req.TokenizeEnabled != nil && !*c.req.TokenizeEnabled,
		PriorityScheduling:      c.priorityScheduling,
		VRAMLimitBytes:          int64(c.req.VRAMLimitGiB * (1 << 30)),
		StartTimeoutSec:         c.req.StartTimeoutSec,
		PartitionBackend:        c.req.PartitionBackend,
		Replicas:                c.req.Replicas,
		RoutingPolicy:           c.req.RoutingPolicy,
		State:                   store.DeployActive,
	}
	dep.SetPolicy(c.yieldPolicy)
	if c.selection.Model == nil {
		c.created, err = c.s.d.Store.CreateDeploymentWithRequirements(dep, c.reqs)
	} else {
		c.created, err = c.s.d.Store.CreateDeploymentFromTemplate(dep, c.reqs, c.templateConfig.modelID, c.templateConfig.operations, c.templateConfig.inputModalities)
	}
	if err != nil {
		return CreateRowError(err)
	}
	return nil
}

// startDownloads starts the transfers the deployment owns. A transfer that
// cannot start takes the row back out.
func (c *createState) startDownloads() error {
	// Start the download only after validation and persistence: a rejected
	// create must not leave an unowned transfer running.
	for _, weight := range c.weights {
		if err := c.s.startSharedModelDownload(weight.model, c.weightStorage, weight.nodes, c.created.ID); err != nil {
			_ = c.s.d.Store.DeleteDeployment(c.created.ID)
			return err
		}
	}
	if err := c.s.startComponentDownloads(c.componentPlans, c.targets, c.created.ID); err != nil {
		_ = c.s.d.Store.DeleteDeployment(c.created.ID)
		return err
	}
	return nil
}
