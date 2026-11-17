// ledger_foreign.go is the ledger's view of cards that processes outside the
// platform hold (ADR 0068): what they take from a card and whether a partition
// may share it with them.

package planning

import (
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/estimate"
	"gpuworks.ai/gpuworks/internal/server/nodepolicy"
	"gpuworks.ai/gpuworks/internal/server/partitioncap"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// foreignShare is what processes outside the platform hold on one of the
// node's cards and whether a partition may share it with them by memory
// (store.GPU.SharesMemoryWithForeign), which also asks that the node's agent
// keeps the holders reported once an instance of ours joins them
// (proto.PartitionSafetyForeignTracked): an older one drops them, and the
// card would read as free after the first placement. The holders' list is
// parsed only for a held card, once per snapshot.
func foreignShare(node *store.Node, g *store.GPU) (int64, bool) {
	if !g.HeldOutsidePlatform() {
		return 0, false
	}
	bytes, shareable := g.SharesMemoryWithForeign()
	if !shareable && g.CanTakeWork() {
		// An operator allowed work on the card (or it is faulty and allowed):
		// it is not shared by this rule, but what the holders take is still
		// not on offer to a partition.
		bytes, _ = g.ForeignBytes()
	}
	return bytes, shareable && node.PartitionSafetyProtocol >= proto.PartitionSafetyForeignTracked
}

// PartitionMayShareHeldCard is whether the scheduler may place a partition
// on a card held outside the platform on its own: foreignShare's verdict, a
// partition backend ready on the card, as NodeSnapshot hands them to
// admission, and a node whose placement policy allows it without an
// operator choosing the card. Statistics that count such a card as
// schedulable capacity ask it, so they do not count capacity the scheduler
// never uses.
func PartitionMayShareHeldCard(node *store.Node, g *store.GPU) bool {
	if !node.PlacementPolicy.SharesForeign(nodepolicy.Consent{}) {
		return false
	}
	if _, shareable := foreignShare(node, g); !shareable {
		return false
	}
	for _, capability := range partitioncap.ForGPU(node, g) {
		if capability.Status == partitioncap.StatusReady {
			return true
		}
	}
	return false
}

// ForeignHold is the memory on one of the node's cards that processes
// outside the platform hold, with the headroom kept beside it: what neither
// admission (scheduler.GPU.Available), a quota change in place (QuotaRoom)
// nor the GPU list's free figure may offer to the platform. Zero when no
// holder's memory is counted. known is false on a held card that takes no
// work and whose holders' memory is not known (store.GPU.ForeignBytes): the
// card offers nothing beyond what is placed on it already, as admission
// refuses it outright, rather than reading the unknown as nothing held.
func ForeignHold(node *store.Node, g *store.GPU) (bytes int64, known bool) {
	if g.HeldOutsidePlatform() && !g.CanTakeWork() {
		if _, ok := g.ForeignBytes(); !ok {
			return 0, false
		}
	}
	held, _ := foreignShare(node, g)
	if held <= 0 {
		return 0, true
	}
	return held + estimate.ForeignHeadroomBytes(g.MemoryTotal), true
}
