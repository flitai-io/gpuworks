package deployment

import (
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
)

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
		ServedName: env.servedName, Sleepable: req.Sleepable, YieldPolicy: requestYield(req.YieldPolicy), ExplainHolders: true,
		MasterAddr: req.MasterAddr, MasterPort: req.MasterPort,
		VRAMLimitBytes: int64(req.VRAMLimitGiB * (1 << 30)), PartitionBackend: req.PartitionBackend,
		RuntimeImageCached: env.nodeImageCached, APIContract: selection.APIContract,
		ModelRules: store.SourceRules{Pinned: store.StoragePins(req.NodeRequirements)},
	}
}
