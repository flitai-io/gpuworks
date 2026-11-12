package network

import (
	"gpuworks.ai/gpuworks/internal/server/netplan"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// PeerRelayAllowed permits archive traffic over the management connection only
// when both nodes allow that network to carry storage synchronization.
func PeerRelayAllowed(source, target *store.Node) bool {
	return source != nil && target != nil &&
		!Node(*source).RolesOff[netplan.RoleStorageSync] &&
		!Node(*target).RolesOff[netplan.RoleStorageSync]
}
