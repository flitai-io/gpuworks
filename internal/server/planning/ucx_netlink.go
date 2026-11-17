package planning

import (
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/netplan"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// UCXNetlinkHangOf is the live fact a node's agent last reported about its
// kernel hanging UCX (NIXL, ADR 0028): nil when it does not, was not
// reported, or the node is offline. It is read from the connected session -
// the way Online is - and never stored.
type UCXNetlinkHangOf func(uuid string) *proto.UCXNetlinkHang

// UCXNetlinkBlocks is the one rule for a node whose kernel hangs UCX: it
// cannot take a rank when the deployment's PD mode moves its KV over UCX
// (affects: pdmode.Mode.UCXNetlinkAffects) on a network that is not host
// RDMA, where UCX walks the IPv6 route dump on start and the engine never
// finishes starting. On host RDMA UCX is held to RDMA transports and may
// never run that check, so the same fact is only a warning. Automatic
// placement leaves such nodes out (UnitSuggestFacts); a placement that names
// one is refused by the same rule.
func UCXNetlinkBlocks(affects bool, network netplan.Network) bool {
	return affects && network.Type != netplan.TypeHostRDMA
}

// ucxNetlinkAffects reports whether dep is a PD deployment in a mode whose KV
// moves over UCX.
func ucxNetlinkAffects(dep *store.Deployment) bool {
	mode, ok := pdModeOf(dep)
	return ok && mode.UCXNetlinkAffects()
}

// ucxNetlinkNamed bounds how many left-out nodes a count names: the count
// stays exact, the names are for the operator to find the first few.
const ucxNetlinkNamed = 5
