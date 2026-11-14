// launch_plan.go assembles one deployment's launch: the estimate and schedule
// pass, the engine parameters, the image facts and the resolved command. The
// api's preview and the controller's scheduler both used to do this by hand,
// which is how the two ended up estimating from different inputs.

package planning

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/netplan"
	"gpuworks.ai/gpuworks/internal/server/network"
	"gpuworks.ai/gpuworks/internal/server/parallelism"
	"gpuworks.ai/gpuworks/internal/server/placement"
	"gpuworks.ai/gpuworks/internal/server/scheduler"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// PreviewAPIPort is the placeholder a preview shows where a real launch has the
// port the scheduler reserved for it.
const PreviewAPIPort = 30000

// LaunchRequest is one launch assembly's input: the rows and choices the caller
// owns. Both callers build it from what they have - the controller from stored
// rows, the preview from a request - and neither assembles a launch itself.
type LaunchRequest struct {
	StartTimeoutSec        int
	DefaultStartTimeoutSec int
	// Preview marks an offline preview: no port is reserved and no GPU assigned,
	// and a placement that cannot be resolved is still previewed symbolically.
	Preview bool
	// AllowUnresolvedImage permits a resource-only form preview before its
	// runtime image is chosen. It never applies to an actual launch and does
	// not invent image acquisition facts or an entrypoint.
	AllowUnresolvedImage bool
	// Unloaded previews the node as if nothing ran on it (Input.Unloaded).
	Unloaded bool
	// ExcludeDeploymentID previews the node without that deployment's
	// instances (Input.ExcludeDeploymentID).
	ExcludeDeploymentID uint
	// APIPort is the reserved host port of a real launch; a preview passes zero
	// and gets PreviewAPIPort.
	APIPort int

	Engine    string
	Model     *store.Model
	Node      *store.Node
	Selection deploy.CatalogSelection
	Placement Placement
	Topology  DistTopology
	Source    LaunchSource
	Quick     deploy.LaunchConfig
	ExtraArgs []string

	// ModelMount is the primary component's container path; ModelHostPath is the
	// host directory behind it (empty in a preview, which mounts nothing).
	ModelMount      string
	ModelHostPath   string
	ComponentMounts []proto.MountSpec
	// ModelPaths is where each model an extra mount row names is on the node
	// (deploy.ResolveLaunchInput.ModelPaths); empty in a preview.
	ModelPaths map[uint]string

	ServedName string
	Sleepable  bool
	// YieldPolicy is the deployment's yield policy (package yieldpolicy):
	// the cards of deployments it does not share with stay reserved against
	// it even while they sleep. Empty reads as never.
	YieldPolicy string
	// ExplainHolders asks a refusal to name the awake holders yield policy
	// keeps the launch from (Input.ExplainHolders): the deploy form's
	// evaluation sets it, the controller does not.
	ExplainHolders     bool
	MasterAddr         string
	MasterPort         int
	VRAMLimitBytes     int64
	PartitionBackend   string
	RuntimeImageCached *bool
	// APIContract is the contract the resolved launch advertises, already
	// reduced by the caller's rule for model operations versus the snapshot.
	APIContract json.RawMessage
	// PD is set on a PD disaggregated instance: its role and side channel
	// (ADR 0028). Quick is then already the role's configuration.
	PD *deploy.PDLaunch
	// ModelRules are where the model comes from on the node: the storages
	// the deployment's requirements limit it to and the one it saves into
	// (store.ModelSourceOn).
	ModelRules store.SourceRules
}

// LaunchPlan is one assembled launch and everything it was built from, so the
// caller persists or renders it without assembling anything again.
type LaunchPlan struct {
	// Input is the estimate input the plan was built from. It is a read-only
	// projection for callers that need to compare two paths (the preview and the
	// controller); nobody may edit it and feed it back, or the comparison would
	// stop describing what the server actually planned.
	Input    Input
	Output   Output
	Params   deploy.Params
	Resolved deploy.ResolvedLaunch
	// LaunchInput is the resolver input Resolved was built from, kept so a
	// caller that derives another launch from this one (the manual command
	// draft re-resolves it in engine mode) starts from what was planned, not
	// from a copy it assembled itself. Read-only, like Input.
	LaunchInput deploy.ResolveLaunchInput
	// Warnings are what the platform cannot prove about the resolved launch.
	// They are for the preview to show; a plan never fails on them.
	Warnings  []messages.Warning
	ImagePull ImagePullPlan
	Topology  DistTopology
	// Source is the launch-side half the plan was built from, kept so a caller
	// persists exactly what it assembled.
	Source LaunchSource
	// Profile is the engine profile the launch uses (health path, sleep support,
	// metrics, shared memory).
	Profile deploy.Profile
}

// LaunchResolveError is a launch the plan scheduled but could not resolve
// into a command on its node: the stated parallelism does not match the
// cards the node gives it, say. It is about that node, not the request as a
// whole, so a pool check (EverFits) reads it as the node refusing.
type LaunchResolveError struct {
	DeploymentID uint
	Err          error
}

func (e *LaunchResolveError) Error() string {
	return fmt.Sprintf("deployment %d resolve launch: %v", e.DeploymentID, e.Err)
}

func (e *LaunchResolveError) Unwrap() error { return e.Err }

// PlanLaunch estimates, schedules and assembles one launch on one node.
//
// A schedule that does not fit is not an error: the caller reads
// LaunchPlan.Output and decides what to say. The one exception is a placement
// error on a real launch, which no caller can act on, and a preview that cannot
// resolve its placement is still assembled with the symbolic values the form
// shows.
func PlanLaunch(ctx context.Context, st *store.Store, networks Networks, req LaunchRequest) (LaunchPlan, error) {
	_ = ctx
	if req.Model == nil || req.Node == nil {
		return LaunchPlan{}, fmt.Errorf("launch plan requires a model and a node")
	}
	if req.DefaultStartTimeoutSec <= 0 {
		req.DefaultStartTimeoutSec = st.DeploymentPolicy().StartTimeoutSec
	}
	input, err := launchInput(st, req)
	if err != nil {
		return LaunchPlan{}, err
	}
	input.ServingUnavailable = !networks.CanServe(*req.Node)
	plan := LaunchPlan{Input: input, Output: input.Evaluate(st), Topology: req.Topology, Source: req.Source}
	if plan.Output.PlacementError != nil && !req.Preview {
		return plan, fmt.Errorf("deployment %d placement: %w", req.Source.DeploymentID, plan.Output.PlacementError)
	}
	if (!plan.Output.Result.OK || plan.Output.Result.Plan == nil) && !req.Preview {
		return plan, nil
	}

	gpus, assigned, parallel := previewPlacement(req, plan.Output, input.Parallel)
	if !(req.Preview && req.AllowUnresolvedImage && strings.TrimSpace(req.Source.Image) == "") {
		imagePull, err := ImagePullPlanFor(st, nodeResolverOf(networks), req.Source, req.Node, req.Selection)
		if err != nil {
			return plan, err
		}
		plan.ImagePull = imagePull
	}

	apiPort := req.APIPort
	if req.Preview {
		apiPort = PreviewAPIPort
	}
	if req.Topology.Headless {
		apiPort = 0
	}
	resolver := req.Selection.PlacementResolver()
	// A placement the scheduler could not settle is still previewed: the
	// variables the topology resolver would fill stay symbolic, which is what
	// the form shows and what the persisted launch replaces once a node is
	// chosen.
	unresolved := !plan.Output.Result.OK || plan.Output.Result.Plan == nil
	var variables map[string]any
	if resolver != "" && !unresolved {
		variables = placementVariables(plan.Output, len(gpus), parallel.TP, req.ModelMount)
	}
	allocated, err := ResolveAllocatedLaunch(req, gpus, assigned, parallel, apiPort, variables, plan.Output.Breakdown.WeightBytes, req.Preview && unresolved)
	if err != nil {
		if refusal := targetLaunchRefusal(err); refusal != nil {
			plan.Output.Result = scheduler.Result{OK: false, Message: *refusal, Explain: scheduler.Explain{Filters: []scheduler.Reason{*refusal}}}
			return plan, nil
		}
		return plan, &LaunchResolveError{DeploymentID: req.Source.DeploymentID, Err: err}
	}
	plan.Params, plan.Profile, plan.LaunchInput = allocated.Params, allocated.Profile, allocated.LaunchInput
	plan.Resolved, plan.Warnings = allocated.Resolved, allocated.Warnings
	plan.Resolved.StartupEstimate = launchStartupEstimate(req, plan.Resolved)
	return plan, nil
}

// launchParams is shared by the symbolic admission preview and final launch.
// Only allocation and the API port differ; both use ResolveLaunch for command
// precedence, image entrypoints, environment and engine hooks.
func launchParams(req LaunchRequest, gpu store.GPU, assigned int, parallel parallelism.Spec, apiPort int) deploy.Params {
	vendor := req.Node.VendorOf(gpu)
	modelType := ""
	if req.Model != nil {
		modelType = req.Model.Arch
	}
	arch := req.Node.ArchOf(gpu)
	params := deploy.Params{
		ModelPath: req.ModelMount, ServedName: req.ServedName, APIPort: apiPort,
		AssignedGPUs: assigned, TensorParallel: parallel.TP, PipelineParallel: parallel.PP, DataParallel: parallel.DP,
		Sleepable: req.Sleepable, Quick: req.Quick,
		TemplateArgs: req.Selection.ExtraArgs, TemplateEnv: req.Selection.Env, ExtraArgs: req.ExtraArgs,
		Arch: arch.Family, ArchID: arch.ID, ComputeCapability: gpu.ComputeCapability, Vendor: vendor,
		ModelType: modelType,
		NNodes:    req.Topology.NNodes, NodeRank: req.Topology.NodeRank,
		DistGroup: req.Topology.Group, DistRole: req.Topology.Role, DistAttempt: req.Topology.Attempt,
		MasterAddr: req.MasterAddr, MasterPort: req.MasterPort,
		Headless: req.Topology.Headless, NodeAddress: req.Topology.NodeAddress,
		ComputeNetwork: req.Topology.Network,
		PD:             req.PD,
		Host:           deploy.RuntimeBindHost(req.Node.RuntimeDriver),
		RuntimeDriver:  req.Node.RuntimeDriver, VRAMLimitBytes: req.VRAMLimitBytes, Partitioned: req.PartitionBackend != "", PartitionBackend: req.PartitionBackend,
	}
	if params.PD != nil && LoadConfig(req.Model).HasRecurrentLayers() {
		// A copy: the PD launch is the rank's, shared with the request.
		pd := *params.PD
		pd.RecurrentLayers = true
		params.PD = &pd
	}
	if params.NNodes <= 1 && params.ComputeNetwork.Type == "" && deploy.RequiresCollectiveInterface(params) {
		if management, ok := network.ManagementAddress(*req.Node); ok {
			params.ComputeNetwork = deploy.ComputeNetwork{Type: netplan.TypeHostEthernet, Interface: management.Interface}
		}
	}
	return params
}

func previewRequestedFeatures(req LaunchRequest) (*proto.RequestedFeatures, error) {
	gpu, err := launchGPU(req.Node, req.Placement.GPUIndexes)
	if err != nil {
		return nil, err
	}
	dp, err := deploy.PlacementDataParallel(req.Engine, req.Quick.DP, req.Selection.ExtraArgs, req.ExtraArgs)
	if err != nil {
		return nil, err
	}
	spec := req.Placement.Parallelism(req.Topology.NNodes)
	spec.DP = dp
	_, assigned, parallel := previewPlacement(req, Output{}, spec)
	params := launchParams(req, gpu, assigned, parallel, PreviewAPIPort)
	params.AssignedGPUs = 0
	params.TensorParallel = 0
	engine, err := deploy.SelectEngine(req.Engine)
	if err != nil {
		return nil, err
	}
	resolved, err := deploy.ResolveLaunch(deploy.ResolveLaunchInput{
		Engine: engine, Params: params, Image: req.Source.Image, ContainerOverrides: req.Source.Overrides,
		ComponentModelMount: componentModelMount(req), Entrypoint: req.Selection.Entrypoint,
		PreStartArgv: req.Selection.PreStartArgv, PreStartScript: req.Selection.PreStartScript,
		RuntimeAssets: req.Selection.RuntimeAssets, ComponentMounts: req.ComponentMounts, ModelPaths: req.ModelPaths,
		TemplateShmBytes: req.Selection.ShmBytes, TemplateMemlockUnlimited: req.Selection.MemlockUnlimited,
		APIContract: req.APIContract, AllowUnresolved: true, TargetValidation: deploy.CollectTargetCapabilities,
	})
	if err != nil {
		return nil, err
	}
	return resolved.RequestedFeatures, nil
}

// launchInput is the estimate and schedule input for one launch. It is the same
// for the preview and the controller by construction: both spend the catalog's
// spec and runtime weights, its measured KV cost, and the node's cache fact.
func launchInput(st *store.Store, req LaunchRequest) (Input, error) {
	componentWeightBytes, componentArtifacts := componentSum(req.Selection.Components)
	validator, err := TemplateGPUValidator(req.Selection)
	if err != nil {
		return Input{}, err
	}
	requests, features := targetRequestsFor(req, validator, targetRequestCache{}, previewRequestedFeatures)
	dp, err := deploy.PlacementDataParallel(req.Engine, req.Quick.DP, req.Selection.ExtraArgs, req.ExtraArgs)
	if err != nil {
		return Input{}, err
	}
	parallel := req.Placement.Parallelism(req.Topology.NNodes)
	parallel.DP = dp
	return Input{
		Model: req.Model, Node: req.Node, Engine: req.Engine, Quick: req.Quick,
		SpecWeightBytes:      req.Selection.WeightBytes,
		RuntimeWeightBytes:   req.Selection.RuntimeWeightBytes,
		ComponentWeightBytes: componentWeightBytes,
		ComponentArtifacts:   componentArtifacts,
		KVBytesPerToken:      catalogKVBytesPerTokenForSelection(st, req.Selection.TemplateID, req.Selection.ModelID, req.Model, req.Engine),
		RuntimeImageBytes:    CatalogImageBytes(st, req.Source.Image),
		RuntimeImageCached:   runtimeImageCached(st, req),
		Unloaded:             req.Unloaded,
		ExcludeDeploymentID:  req.ExcludeDeploymentID,
		DeploymentID:         req.Source.DeploymentID,
		YieldPolicy:          req.YieldPolicy,
		ExplainHolders:       req.ExplainHolders,
		Parallel:             parallel,
		GPUIndexes:           req.Placement.GPUIndexes,
		VRAMLimitBytes:       req.VRAMLimitBytes,
		PartitionBackend:     req.PartitionBackend,
		Sleepable:            req.Sleepable,
		PD:                   req.PD != nil,
		PlacementResolver:    req.Selection.PlacementResolver(),
		RequestedFeatures:    features, TargetRequests: requests, ValidateGPU: validator,
		ModelRules: req.ModelRules,
	}, nil
}

// runtimeImageCached is the node's cache fact for the runtime image. A caller
// that asked the node passes what it learned; otherwise the store's image_nodes
// record answers, which is all the controller has.
func runtimeImageCached(st *store.Store, req LaunchRequest) *bool {
	if req.RuntimeImageCached != nil {
		return req.RuntimeImageCached
	}
	if st == nil || strings.TrimSpace(req.Source.Image) == "" {
		return nil
	}
	image, err := st.ResolveImageRef(req.Source.Image)
	if err != nil {
		return nil
	}
	node, err := st.ImageNode(image.ID, req.Node.ID)
	if err != nil {
		return nil
	}
	cached := node.State == store.ImageStatePresent
	return &cached
}

// previewPlacement is what one rank launches with: its GPUs, how many GPUs
// that is, and the deployment's parallelism. A schedule that resolved gives
// the GPUs; a preview without one keeps the GPUs the request pinned and the
// per-node count the stated parallelism implies (or one).
func previewPlacement(req LaunchRequest, out Output, spec parallelism.Spec) ([]int, int, parallelism.Spec) {
	if out.Result.OK && out.Result.Plan != nil {
		return out.Result.Plan.GPUIndexes, len(out.Result.Plan.GPUIndexes), spec.Assigned(out.Result.Plan.TP)
	}
	perNode, err := spec.GPUsPerNode()
	if err != nil || perNode < 1 {
		perNode = 1
	}
	if len(req.Placement.GPUIndexes) > 0 {
		perNode = len(req.Placement.GPUIndexes)
	}
	return req.Placement.GPUIndexes, perNode, spec.Assigned(perNode)
}

// launchGPU resolves the card a launch was assigned. The scheduler names GPUs
// by hardware index - the card's own identity, not its position in the node's
// report - so a card that has since disappeared is an error, never a wrong
// card. A preview with no assigned index takes the first card the node reports,
// which is the best fact an unresolved placement has.
func launchGPU(node *store.Node, indexes []int) (store.GPU, error) {
	if node == nil {
		return store.GPU{}, fmt.Errorf("launch requires a node")
	}
	if len(indexes) == 0 {
		owned := node.OwnedGPUs()
		if len(owned) == 0 {
			return store.GPU{}, nil
		}
		return owned[0], nil
	}
	gpu, ok := node.GPUByIndex(indexes[0])
	if !ok {
		return store.GPU{}, fmt.Errorf("node %s has no GPU with index %d", node.UUID, indexes[0])
	}
	return gpu, nil
}

func componentSum(components []deploy.ComponentBinding) (int64, []Artifact) {
	var weightBytes int64
	var artifacts []Artifact
	for _, component := range components {
		if component.Role == "primary" || component.WeightBytes <= 0 {
			continue
		}
		weightBytes += component.WeightBytes
		artifacts = append(artifacts, Artifact{ModelID: component.StoreModelID, StorageBytes: component.WeightBytes})
	}
	return weightBytes, artifacts
}

func placementVariables(out Output, gpuCount, tp int, modelMount string) map[string]any {
	return placement.RuntimeVariables(out.Placement, gpuCount, tp, modelMount)
}

// componentModelMount is the template component's primary mount a launch's
// weights go to, or "" when the selection has no components and the default
// mount applies.
func componentModelMount(req LaunchRequest) string {
	if len(req.Selection.Components) == 0 {
		return ""
	}
	return req.ModelMount
}
