// deployment_launch.go turns a stored deployment into the launch request its
// replicas share, and plans one against a node with the template's hardware
// contract. The controller launches and trials pool candidates with it; the
// API checks with the same code whether changed node requirements leave any
// node a replica could ever run on, so the two can never disagree about what
// fits.

package planning

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/chipmatch"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/inferenceapi"
	"gpuworks.ai/gpuworks/internal/server/scheduler"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/server/yieldpolicy"
)

// DeploymentAPIContract returns the contract used by the runtime launch.
//
// Model operations are the editable, current source of truth. An explicitly
// non-empty value must therefore win over the API contract retained in an old
// deployment template snapshot. A model with no operations (an empty column or
// []) falls back to the snapshot, the same way a template deploy fills them.
func DeploymentAPIContract(model *store.Model, legacy json.RawMessage) json.RawMessage {
	if model != nil && !store.OperationsUnset(model.Operations) {
		contract := inferenceapi.ModelContract(model)
		encoded, err := json.Marshal(contract)
		if err == nil {
			return encoded
		}
	}
	if raw := bytes.TrimSpace(legacy); len(raw) > 0 && !bytes.Equal(raw, []byte("null")) {
		return append(json.RawMessage(nil), raw...)
	}
	return nil
}

// TemplateGPUValidator rechecks the catalog hardware contract against the
// cards that exist now. Placement itself cannot hold this: the predicate is
// template knowledge, and a node rebuilt with different hardware must stop the
// launch instead of silently re-targeting the new card. A nil validator means
// the template states no contract.
func TemplateGPUValidator(layer deploy.CatalogSelection) (func(store.GPU) error, error) {
	rawPredicate := bytes.TrimSpace(layer.GPUs)
	rawUnsupported := bytes.TrimSpace(layer.Unsupported)
	hasPredicate := len(rawPredicate) > 0 && !bytes.Equal(rawPredicate, []byte("null"))
	hasUnsupported := len(rawUnsupported) > 0 && !bytes.Equal(rawUnsupported, []byte("null"))
	if !hasPredicate && !hasUnsupported {
		return nil, nil
	}
	var predicate chipmatch.Predicate
	if hasPredicate {
		var err error
		predicate, err = chipmatch.DecodePredicate(string(rawPredicate))
		if err != nil {
			return nil, fmt.Errorf("decode template GPU predicate: %w", err)
		}
	}
	unsupported, err := chipmatch.DecodeUnsupported(string(rawUnsupported))
	if err != nil {
		return nil, fmt.Errorf("decode template unsupported rules: %w", err)
	}
	spec := chipmatch.Spec{Name: layer.SpecName, GPUs: predicate, Quantization: layer.Quantization}
	return func(gpu store.GPU) error {
		reported := chipmatch.ReportedGPU(gpu.Name, gpu.Vendor, gpu.ArchID, gpu.ComputeCapability)
		return chipmatch.ValidateSpec(reported, spec, unsupported)
	}, nil
}

// DeploymentTemplate is the catalog selection a deployment launched with.
func DeploymentTemplate(dep *store.Deployment) (deploy.CatalogSelection, error) {
	var snapshotJSON string
	if dep != nil {
		snapshotJSON = dep.CatalogSelectionSnapshot
	}
	selection, err := deploy.DecodeCatalogSelection([]byte(snapshotJSON))
	if err != nil {
		if dep == nil {
			return selection, err
		}
		return selection, fmt.Errorf("deployment %d catalog selection snapshot: %w", dep.ID, err)
	}
	// Asset bytes are a transient authenticated transport concern. Strip any
	// legacy or hand-written content before the immutable launch is persisted;
	// the hub hydrates cache misses from these package references.
	for index := range selection.RuntimeAssets {
		selection.RuntimeAssets[index].Content = nil
	}
	return selection, nil
}

// LaunchGPU resolves the card the scheduler chose for an instance.
//
// The scheduler names GPUs by hardware index, which is the card's own identity
// and not its position in node.GPUs: a host whose cards are partly passed
// through to other tenants reports a sparse set (0, 3, 4, 6). Reading that
// index as a slice position described the wrong card on a mixed node and
// panicked the whole server process on a sparse one. A GPU that has since
// disappeared from the node's report is a reconcile error like any other, never
// a crash.
func LaunchGPU(node *store.Node, index int) (store.GPU, error) {
	gpu, ok := node.GPUByIndex(index)
	if !ok {
		return store.GPU{}, fmt.Errorf("scheduler chose GPU %d that node %s no longer reports", index, node.UUID)
	}
	return gpu, nil
}

// ModelPaths are where a deployment's model files are on one node, as its
// storage resolves them (store.ModelLocation): the primary model and each
// component model by model id.
type ModelPaths struct {
	Primary    string
	Components map[uint]string
}

// DeploymentLaunch is the part of a launch every instance of a deployment
// shares: the stored launch configuration, the catalog selection, the model
// and component mounts on this node, and the served name. The caller adds the
// topology, the reserved port and anything role-specific. Nil paths ask for a
// preview of the node before the model is there: nothing is mounted.
//
// reqs are the deployment's node requirements: they say which storages the
// model may come from, and GPU indexes a single-node
// deployment pinned are kept only while it is pinned to exactly one node: an
// index names a card on one machine, and a replica the pool places elsewhere
// lets the scheduler pick its cards.
func DeploymentLaunch(dep *store.Deployment, reqs map[string][]string, node *store.Node, model *store.Model, paths *ModelPaths) (LaunchRequest, deploy.CatalogSelection, string, error) {
	quick, extra, err := deploymentLaunchSettings(dep)
	if err != nil {
		return LaunchRequest{}, deploy.CatalogSelection{}, "", err
	}
	templateLayer, err := DeploymentTemplate(dep)
	if err != nil {
		return LaunchRequest{}, templateLayer, "", err
	}
	placement, err := ParsePlacement(dep.Placement)
	if err != nil {
		return LaunchRequest{}, templateLayer, "", fmt.Errorf("deployment %d placement: %w", dep.ID, err)
	}
	if len(placement.GPUIndexes) > 0 && placement.PD == nil && dep.NNodes <= 1 {
		if _, pinned := PinnedNode(reqs); !pinned {
			placement.GPUIndexes = nil
		}
	}
	modelMount, modelHostPath, componentMounts := deploy.DefaultModelMount, "", []proto.MountSpec(nil)
	if paths != nil {
		modelHostPath = paths.Primary
	}
	if primary, ok := deploy.PrimaryComponent(templateLayer.Components); ok && paths != nil {
		modelMount = primary.MountPath
		primaryPath := paths.Components[primary.StoreModelID]
		if primaryPath == "" {
			return LaunchRequest{}, templateLayer, "", fmt.Errorf("deployment %d primary component %s is not present on node %s", dep.ID, primary.Name, node.UUID)
		}
		modelHostPath = primaryPath
	}
	for _, component := range templateLayer.Components {
		if component.Role == "primary" || paths == nil {
			continue
		}
		path := paths.Components[component.StoreModelID]
		if path == "" {
			return LaunchRequest{}, templateLayer, "", fmt.Errorf("deployment %d component %s is not present on node %s", dep.ID, component.Name, node.UUID)
		}
		componentMounts = append(componentMounts, proto.MountSpec{
			Source: path, Target: component.MountPath, ReadOnly: true,
			Purpose: "model_component:" + component.Role,
		})
	}
	served := dep.ServedName
	if served == "" && model != nil {
		served = model.Name
	}
	return LaunchRequest{
		StartTimeoutSec: dep.StartTimeoutSec,
		Engine:          dep.Engine, Model: model, Node: node,
		Selection: templateLayer, Placement: placement,
		Source: LaunchSourceFromDeployment(dep), Quick: quick, ExtraArgs: extra,
		ModelMount: modelMount, ModelHostPath: modelHostPath, ComponentMounts: componentMounts,
		ServedName: served, Sleepable: dep.Sleepable, MasterAddr: dep.MasterAddr, MasterPort: dep.MasterPort,
		YieldPolicy:    dep.YieldPolicy,
		VRAMLimitBytes: dep.VRAMLimitBytes, PartitionBackend: dep.PartitionBackend,
		APIContract: DeploymentAPIContract(model, templateLayer.APIContract),
		ModelRules:  store.SourceRules{Pinned: store.StoragePins(reqs), SaveTo: dep.SaveToStorageID, Deployment: dep.ID},
	}, templateLayer, modelHostPath, nil
}

// Fits says the plan places the launch.
func (o Output) Fits() bool { return o.Result.OK && o.Result.Plan != nil }

// Refusal is why a plan that does not fit cannot be placed on node, for a
// deployment of policy: a launch that waits for holders to yield
// (YieldRefusal), only for sleepers' cards (ReservedRefusal), or that yield
// policy keeps off awake holders' cards (HeldRefusal) says so; any
// other is the scheduler's own reason. The controller and every preview
// that shows a refusal read it here, so they say the same.
func (o Output) Refusal(node *store.Node, policy yieldpolicy.Policy) scheduler.Reason {
	switch {
	case o.Yield != nil:
		return YieldRefusal(node, policy, o.Yield)
	case o.Reserved != nil:
		return ReservedRefusal(node, o.Reserved)
	case o.Held != nil:
		return HeldRefusal(node, policy, o.Held)
	}
	return o.Result.Message
}

// YieldWait is what a launch refused for waiting on holders to yield waits
// for, nil for any other refusal: the typed facts the controller acts on,
// where the refusal's params are for display.
func (p LaunchPlan) YieldWait(refusal *scheduler.Reason) *LaunchYield {
	if refusal == nil || refusal.Code != scheduler.ReasonWaitingForYield {
		return nil
	}
	return p.Output.Yield
}

// PlanTemplateLaunch plans req on node and checks the cards it chose against
// the template's hardware contract. A non-nil refusal says why the launch
// cannot be placed; recording it is the caller's.
func PlanTemplateLaunch(ctx context.Context, st *store.Store, networks Networks, node *store.Node, req LaunchRequest, templateLayer deploy.CatalogSelection) (LaunchPlan, *scheduler.Reason, error) {
	plan, err := PlanLaunch(ctx, st, networks, req)
	if err != nil {
		return plan, nil, err
	}
	if !plan.Output.Fits() {
		reason := plan.Output.Refusal(node, yieldpolicy.Policy{Yield: req.YieldPolicy})
		return plan, &reason, nil
	}
	validateGPU, err := TemplateGPUValidator(templateLayer)
	if err != nil {
		return plan, nil, fmt.Errorf("template hardware: %w", err)
	}
	if validateGPU != nil {
		for _, index := range plan.Output.Result.Plan.GPUIndexes {
			candidate, gpuErr := LaunchGPU(node, index)
			if gpuErr != nil {
				return plan, nil, gpuErr
			}
			if err := validateGPU(candidate); err != nil {
				return plan, &scheduler.Reason{Code: scheduler.ReasonPlacementRejected, Params: map[string]any{"detail": err.Error()}}, nil //nolint:nilerr // GPU rejection is returned in the placement reason, not as an internal planning failure.
			}
		}
	}
	return plan, nil, nil
}

// deploymentLaunchSettings is the compatibility reader shared by launch and
// capacity planning. Malformed legacy quick_params has always read as zero
// values; extra_args parsing remains an explicit error.
func deploymentLaunchSettings(dep *store.Deployment) (deploy.LaunchConfig, []string, error) {
	var quick deploy.LaunchConfig
	if dep.QuickParams != "" {
		_ = json.Unmarshal([]byte(dep.QuickParams), &quick)
	}
	if dep.ExtraArgs == "" {
		return quick, nil, nil
	}
	extra, err := deploy.ParseArgsText(dep.ExtraArgs)
	if err != nil {
		return quick, nil, fmt.Errorf("deployment %d extra_args: %w", dep.ID, err)
	}
	return quick, extra, nil
}
