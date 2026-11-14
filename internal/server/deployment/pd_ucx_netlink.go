package deployment

import (
	"fmt"
	"strings"

	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/netplan"
	"gpuworks.ai/gpuworks/internal/server/pdmode"
)

// pdUCXNetlinkNode is one node of a PD placement whose agent reports that its
// kernel hangs UCX, as the refusal and the warning name it.
type pdUCXNetlinkNode struct {
	NodeUUID string `json:"node_uuid"`
	NodeName string `json:"node_name"`
	Kernel   string `json:"kernel,omitempty"`
}

// checkPDUCXNetlink guards a PD placement against the kernel behaviour that
// hangs UCX 1.20-1.22, which NIXL bundles (ADR 0028): the engine of a mode
// whose KV moves over UCX never finishes starting on such a node. Over an
// Ethernet network UCX walks the IPv6 route dump on start, so the placement is
// refused with the commands that end it. On a host RDMA network UCX is held to
// RDMA transports and may never run that check, so the same fact is a warning.
// Creating and previewing a deployment both ask it. The fact is live: each
// node's agent reports it with its status, and nothing is stored.
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
	if group.Network.Type == netplan.TypeHostRDMA {
		return []messages.Warning{messages.NewWarning(messages.WarningPDUCXNetlinkHangRDMA, params)}, nil
	}
	return nil, productError(messages.ErrorPDUCXNetlinkHang,
		fmt.Sprintf("PD mode %s moves the KV with NIXL over Ethernet, and the kernel of %s hangs UCX (its IPv6 route dump ends in a way UCX cannot read), so the engine would never finish starting. Turn IPv6 off on the host (this disables IPv6 for the whole host) with: %s",
			p.mode.Name, strings.Join(names, ", "), fixes[0].Commands[0]), params)
}
