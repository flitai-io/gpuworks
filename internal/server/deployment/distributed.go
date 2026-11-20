package deployment

import (
	"errors"
	"fmt"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/netplan"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// PrepareDistributedDeployRequest checks a multi-node request's nodes - one
// per rank, distinct, online - and its parallelism and master port, and
// points the request's leader fields at its first node.
func (s *Service) PrepareDistributedDeployRequest(req *dto.DeployRequest, dp int) ([]*store.Node, error) {
	nodes, placement, err := s.distributedNodes(req)
	if err != nil {
		return nil, err
	}
	if err := setNodeParallelism(req, nodes, placement, dp); err != nil {
		return nil, err
	}
	if _, err := s.prepareRankNetwork(req, nodes); err != nil {
		return nil, err
	}
	return nodes, nil
}

// setNodeParallelism writes the node-derived default of an unstated TP or PP.
func setNodeParallelism(req *dto.DeployRequest, nodes []*store.Node, placement planning.Placement, dp int) error {
	parallel, err := nodeParallelism(nodes, placement, dp)
	if err != nil {
		return err
	}
	setDistributedDegrees(req, parallel.TP, parallel.PP)
	return nil
}

// setDistributedDegrees writes back the parallelism that takes effect, so the
// stored deployment always states it.
func setDistributedDegrees(req *dto.DeployRequest, tp, pp int) {
	if req.Placement == nil {
		req.Placement = map[string]any{}
	}
	req.Placement["tp"], req.Placement["pp"] = tp, pp
}

func nodeParallelism(nodes []*store.Node, placement planning.Placement, dp int) (planning.ParallelismSpec, error) {
	gpuCounts := make([]int, len(nodes))
	for i, n := range nodes {
		gpuCounts[i] = n.GPUCount
	}
	// An unstated TP and PP default from the nodes; what takes effect is
	// written back, so the stored deployment always states its parallelism.
	// The caller has already materialized the launch. Do not re-read raw
	// template flags here: they may have been overridden or unset.
	parallel, err := placement.ParallelismOnDP(gpuCounts, dp)
	if err != nil {
		return planning.ParallelismSpec{}, distributedParallelismError(parallel, err)
	}
	perNode, err := parallelGPUsPerNode(parallel)
	if err != nil {
		return planning.ParallelismSpec{}, err
	}
	for _, n := range nodes {
		if n.GPUCount < perNode {
			return planning.ParallelismSpec{}, distributedNodeCapacityError(n, perNode)
		}
	}
	return parallel, nil
}

// distributedNodes checks a multi-node request's node count, placement,
// master port and nodes, in that order, and returns the nodes and the parsed
// placement.
func (s *Service) distributedNodes(req *dto.DeployRequest) ([]*store.Node, planning.Placement, error) {
	if len(req.NodeUUIDs) != req.NNodes {
		return nil, planning.Placement{}, productError(messages.ErrorDistributedNodeCount, "node_uuids count must equal nnodes", map[string]any{"actual": len(req.NodeUUIDs), "required": req.NNodes})
	}
	placement, err := planning.ParsePlacement(req.Placement)
	if err != nil {
		return nil, placement, err
	}
	if req.MasterPort == 0 {
		req.MasterPort = deploy.DefaultMasterPort
	}
	if req.MasterPort < 1024 || req.MasterPort > 65535 {
		return nil, placement, productError(messages.ErrorMasterPortInvalid, "master_port must be between 1024 and 65535", map[string]any{"minimum": 1024, "maximum": 65535})
	}
	seen := map[string]bool{}
	nodes := make([]*store.Node, 0, len(req.NodeUUIDs))
	for _, uuid := range req.NodeUUIDs {
		if seen[uuid] {
			return nil, placement, productError(messages.ErrorDistributedDuplicateNode, "distributed node list contains duplicates", map[string]any{"node": uuid})
		}
		seen[uuid] = true
		n, err := s.d.Store.NodeByUUID(uuid)
		if err != nil {
			return nil, placement, fmt.Errorf("node %s: %w", uuid, err)
		}
		if n.State != store.NodeStateOnline {
			return nil, placement, productError(messages.ErrorDistributedNodeOffline, fmt.Sprintf("node %s is not online", uuid), map[string]any{"node": uuid})
		}
		nodes = append(nodes, n)
	}
	return nodes, placement, nil
}

func (s *Service) prepareRankNetwork(req *dto.DeployRequest, nodes []*store.Node) (netplan.Group, error) {
	group, err := s.ComputeGroup(nodes)
	if err != nil {
		return group, err
	}
	if req.MasterAddr == "" {
		req.MasterAddr = group.Ranks[nodes[0].ID].IP
	} else {
		host, err := MasterHost(req.MasterAddr)
		if err != nil {
			return group, err
		}
		req.MasterAddr = host
	}
	// The ordered node list defines ranks; the deployment row itself represents
	// the group and always points at rank zero.
	req.NodeUUID = req.NodeUUIDs[0]
	req.NodeRank = 0
	req.DistRole = "leader"
	return group, nil
}

// parallelGPUsPerNode is how many GPUs each node runs for a deployment's
// parallelism, with the failures as the product errors the form shows on the
// TP and PP fields.
func parallelGPUsPerNode(parallel planning.ParallelismSpec) (int, error) {
	perNode, err := parallel.GPUsPerNode()
	return perNode, distributedParallelismError(parallel, err)
}

func distributedParallelismError(parallel planning.ParallelismSpec, err error) error {
	switch {
	case errors.Is(err, planning.ErrParallelismTPRequired):
		return productError(messages.ErrorDistributedTPRequired, "distributed deployments require placement.tp", nil)
	case errors.Is(err, planning.ErrParallelismIndivisible):
		return productError(messages.ErrorDistributedParallelIndivisible, err.Error(),
			map[string]any{"tp": parallel.TP, "pp": parallel.PP, "nodes": parallel.NNodes})
	}
	return err
}

func MasterHost(addr string) (string, error) {
	host, err := planning.PeerHost(addr)
	if err != nil {
		return "", fmt.Errorf("invalid master_addr: %w", err)
	}
	return host, nil
}

func distributedNodeCapacityError(n *store.Node, perNode int) error {
	return productError(messages.ErrorDistributedGPUCount, fmt.Sprintf("node %s has %d GPUs, the parallelism needs %d per node", n.UUID, n.GPUCount, perNode), map[string]any{"node": n.UUID, "available": n.GPUCount, "required": perNode})
}
