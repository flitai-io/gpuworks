package deployment

import (
	"errors"

	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/netplan"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// ComputeGroup picks the network a group's ranks share and checks that it
// can carry the group (planning.CheckGroupNetwork).
func (s *Service) ComputeGroup(nodes []*store.Node) (netplan.Group, error) {
	plain := make([]store.Node, len(nodes))
	for i, n := range nodes {
		plain[i] = *n
	}
	group, err := s.networks().ComputeGroup(plain)
	var ge *netplan.GroupError
	if errors.As(err, &ge) {
		switch ge.Reason {
		case netplan.ComputeMixed:
			return group, productError(messages.ErrorComputeNetworkMixed, ge.Error(),
				map[string]any{"node": ge.Node.Name, "other": ge.Other.Name})
		case netplan.ComputeMismatch:
			return group, productError(messages.ErrorComputeNetworkMismatch, ge.Error(),
				map[string]any{"node": ge.Node.Name, "network": ge.Network, "other": ge.Other.Name, "expected": ge.Expected})
		default:
			return group, productError(messages.ErrorComputeNetworkNone, ge.Error(), map[string]any{"node": ge.Node.Name})
		}
	}
	if err != nil {
		return group, err
	}
	return group, planning.CheckGroupNetwork(plain, group)
}

// computeFallbackWarning tells the operator a group falls back to the
// management network although some ranks have a compute network.
func computeFallbackWarning(group netplan.Group) []messages.Warning {
	if !group.FellBack {
		return nil
	}
	return []messages.Warning{messages.NewWarning(messages.WarningComputeNetworkFallback, nil)}
}
