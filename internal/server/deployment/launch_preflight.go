package deployment

import (
	"fmt"

	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// rankImageResolver names the runtime image one node's launch will run, as
// evaluate shows it and create persists it.
type rankImageResolver func(*store.Node) string

// preflightCreateRuntimes replays the launch of every rank on its own node
// before any model, image or deployment write, with the image that rank
// resolves to. The controller still performs its own authoritative per-rank
// check later; this early pass lets the refusals that no choice of the user can
// avoid fail the create atomically. Warnings are for evaluate and are dropped
// here. An unpinned node (automatic placement) has no runtime yet, so the
// checks that depend on one wait for the controller.
func preflightCreateRuntimes(base deploy.ResolveLaunchInput, rawOverrides map[string]any, components []deploy.ComponentBinding, modelMount string, pinned *store.Node, distNodes []*store.Node, pdReq *pdRequest, imageFor rankImageResolver) error {
	resolve := func(input deploy.ResolveLaunchInput, node *store.Node, context string) error {
		if node != nil {
			input.Params.RuntimeDriver = node.RuntimeDriver
			input.Params.Host = deploy.RuntimeBindHost(node.RuntimeDriver)
			if node.GPUVendor != "" {
				input.Params.Vendor = node.GPUVendor
			}
			if image := imageFor(node); image != "" {
				input.Image = image
			}
		}
		if _, _, err := resolveRequestLaunch(input, rawOverrides, components, modelMount); err != nil {
			if context == "" {
				return err
			}
			return fmt.Errorf("%s: %w", context, err)
		}
		return nil
	}
	if pdReq != nil {
		return preflightCreatePDRuntimes(base, pdReq, resolve)
	}
	if len(distNodes) == 0 {
		return resolve(base, pinned, "")
	}
	for rank, node := range distNodes {
		input := base
		input.Params.NNodes = len(distNodes)
		input.Params.NodeRank = rank
		input.Params.Headless = rank > 0
		input.Params.NodeAddress = planning.RoutableHost(node)
		if input.Params.Headless {
			input.Params.APIPort = 0
		}
		if err := resolve(input, node, fmt.Sprintf("node %s rank %d", node.UUID, rank)); err != nil {
			return err
		}
	}
	return nil
}

type createRankResolver func(deploy.ResolveLaunchInput, *store.Node, string) error

func preflightCreatePDRuntimes(base deploy.ResolveLaunchInput, pdReq *pdRequest, resolve createRankResolver) error {
	byUUID := make(map[string]*store.Node, len(pdReq.nodes))
	for _, node := range pdReq.nodes {
		byUUID[node.UUID] = node
	}
	for _, role := range pdReq.mode.Roles {
		for _, unit := range pdReq.pd.Units(role) {
			ranks := unit.Ranks()
			leaderAddress := ""
			for index, rank := range ranks {
				node := byUUID[rank.NodeUUID]
				if node == nil {
					return fmt.Errorf("PD %s rank %d names unknown node %s", role, index, rank.NodeUUID)
				}
				topology := planning.DistTopology{
					NNodes: len(ranks), NodeRank: index, Headless: index > 0,
					Role: "leader", NodeAddress: planning.RoutableHost(node),
				}
				if index > 0 {
					topology.Role = "follower"
				}
				ports := planning.PDRankPorts{API: planning.PreviewAPIPort, SideChannel: planning.PreviewAPIPort + 1, LeaderAddress: leaderAddress}
				if len(ranks) > 1 {
					ports.Master = deploy.DefaultMasterPort
				}
				launch := planning.LaunchRequest{Quick: base.Params.Quick}
				pdReq.pd.RankLaunch(pdReq.mode, role, unit, index, topology, ports).Apply(&launch)
				if index == 0 {
					leaderAddress = topology.NodeAddress
				}
				input := base
				input.Params.Quick = launch.Quick
				input.Params.NNodes = launch.Topology.NNodes
				input.Params.NodeRank = launch.Topology.NodeRank
				input.Params.Headless = launch.Topology.Headless
				input.Params.NodeAddress = launch.Topology.NodeAddress
				input.Params.TensorParallel = launch.Placement.TP
				input.Params.PipelineParallel = launch.Placement.PP
				input.Params.AssignedGPUs = len(launch.Placement.GPUIndexes)
				if input.Params.AssignedGPUs == 0 {
					perNode, err := launch.Placement.Parallelism(len(ranks)).GPUsPerNode()
					if err != nil {
						return fmt.Errorf("PD %s rank %d: %w", role, index, err)
					}
					input.Params.AssignedGPUs = perNode
				}
				input.Params.APIPort = launch.APIPort
				if input.Params.Headless {
					input.Params.APIPort = 0
				}
				input.Params.MasterAddr = launch.MasterAddr
				input.Params.MasterPort = launch.MasterPort
				input.Params.PD = launch.PD
				context := fmt.Sprintf("PD %s rank %d node %s", role, index, node.UUID)
				if err := resolve(input, node, context); err != nil {
					return err
				}
			}
		}
	}
	return nil
}
