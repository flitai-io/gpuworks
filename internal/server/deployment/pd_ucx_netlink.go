package deployment

import (
	"strings"

	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/netplan"
	"gpuworks.ai/gpuworks/internal/server/pdmode"
	"gpuworks.ai/gpuworks/internal/server/planning"
)

// pdUCXNetlinkNode is one node of a PD placement whose agent reports that its
// kernel hangs UCX, as the warnings name it.
type pdUCXNetlinkNode struct {
	NodeUUID string `json:"node_uuid"`
	NodeName string `json:"node_name"`
	Kernel   string `json:"kernel,omitempty"`
}

// checkPDUCXNetlink guards a PD placement that names its nodes against the
// kernel behaviour that hangs UCX 1.20-1.22, which NIXL bundles (ADR 0028).
// The rule is planning.UCXNetlinkBlocks, the one automatic placement leaves
// such nodes out by (planning.UnitSuggestFacts). A node the operator named is
// their choice: the placement is not refused, it is warned with the commands
// that end the hang (PD_UCX_NETLINK_HANG where the rule blocks, the softer
// PD_UCX_NETLINK_HANG_RDMA on a host RDMA network). Creating and previewing a deployment both ask
// it, after automatic placement has filled the nodes it picks. The fact is
// live: each node's agent reports it with its status, and nothing is stored.
func (s *Service) checkPDUCXNetlink(p *pdRequest, group netplan.Group) ([]messages.Warning, error) {
	if s.d.UCXNetlinkHang == nil || !p.mode.UCXNetlinkAffects() {
		return nil, nil
	}
	var affected []pdUCXNetlinkNode
	var names []string
	for _, node := range p.nodes {
		hang := s.d.UCXNetlinkHang(node.UUID)
		if hang == nil {
			continue
		}
		affected = append(affected, pdUCXNetlinkNode{NodeUUID: node.UUID, NodeName: nodeDisplayName(node), Kernel: hang.Kernel})
		names = append(names, nodeDisplayName(node))
	}
	if len(affected) == 0 {
		return nil, nil
	}
	fixes := pdmode.UCXNetlinkFixes()
	params := map[string]any{
		"mode": p.mode.Name, "network": group.Network.Name,
		"nodes": affected, "names": strings.Join(names, ", "), "fixes": fixes, "fix": fixes[0].Commands[0],
	}
	if !planning.UCXNetlinkBlocks(p.mode.UCXNetlinkAffects(), group.Network) {
		return []messages.Warning{messages.NewWarning(messages.WarningPDUCXNetlinkHangRDMA, params)}, nil
	}
	return []messages.Warning{messages.NewWarning(messages.WarningPDUCXNetlinkHang, params)}, nil
}
