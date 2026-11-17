package planning

import (
	"gpuworks.ai/gpuworks/internal/server/estimate"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// QuotaShortfall is a card that cannot keep a deployment's slices at a new
// dGPU memory limit: it has FreeBytes for them once their current claims are
// handed back, and they would claim NeedBytes.
type QuotaShortfall struct {
	Node      *store.Node
	GPUIndex  int
	FreeBytes int64
	NeedBytes int64
}

// QuotaRoom reports the first card that could not keep the named instances
// of a deployment (live, the ones a quota change holds to limitBytes in
// place) at the new limit, or nil when every card they hold can. A card is
// asked whether it holds their new claims in place of their old ones: its
// capacity, less what everyone else claims, what sleepers keep and what
// processes outside the platform hold with its headroom; on a held card
// whose holders' memory is not known, no more than they claim already (see
// ForeignHold). Instances left out keep their claims; they take the new
// limit at a restart, and are placed by the scheduler then.
//
// The claims are the GPU ledger's (instanceClaim and
// estimate.PartitionClaimBytes), so the answer agrees with the admission
// that follows.
func QuotaRoom(st *store.Store, dep *store.Deployment, limitBytes int64, live []string) (*QuotaShortfall, error) {
	if len(live) == 0 {
		return nil, nil
	}
	changing := make(map[string]bool, len(live))
	for _, uuid := range live {
		changing[uuid] = true
	}
	insts, err := st.InstancesForDeployment(dep.ID)
	if err != nil {
		return nil, err
	}
	type card struct {
		node  uint
		index int
	}
	nodes := map[uint]*store.Node{}
	ledgers := map[uint]map[int]GPUAllocation{}
	held, next := map[card]int64{}, map[card]int64{}
	var order []card
	for i := range insts {
		inst := &insts[i]
		if !changing[inst.UUID] || !holdsClaim(inst) {
			continue // keeps its claim, or holds none (gpuAllocations skips it too)
		}
		node, ok := nodes[inst.NodeID]
		if !ok {
			if node, err = st.NodeByID(inst.NodeID); err != nil {
				return nil, err
			}
			nodes[inst.NodeID] = node
			ledgers[inst.NodeID] = GPUAllocations(st, node)
		}
		_, _, archFamily, _ := nodeGPUProfile(node)
		facts := ledgerFactsOf(inst)
		claim := instanceClaim(st, inst, archFamily)
		limited := estimate.PartitionClaimBytes(limitBytes, facts.PartitionBackend, func() int64 {
			return estimate.ReserveBytes(engineReserveBase(facts.Engine), archFamily, true, instanceGPUMemUtil(st, inst, facts.Engine))
		})
		for _, index := range parseInts(inst.GPUIndexes) {
			key := card{inst.NodeID, index}
			if _, seen := held[key]; !seen {
				order = append(order, key)
			}
			held[key] += claim
			next[key] += limited
		}
	}
	for _, key := range order {
		node := nodes[key.node]
		var capacity, foreign int64
		foreignKnown := true
		for _, gpu := range node.OwnedGPUs() {
			if gpu.Index == key.index {
				capacity = gpu.MemoryTotal
				// A card shared with processes outside the platform keeps
				// what they hold, and the headroom beside it, out of reach,
				// as admission does (scheduler.GPU.Available).
				foreign, foreignKnown = ForeignHold(node, &gpu)
			}
		}
		allocation := ledgers[key.node][key.index]
		room := capacity - allocation.AllocatedBytes - allocation.SleepingBytes - foreign + held[key]
		if !foreignKnown {
			// What the outside processes hold is not known: the instances
			// keep what they claim and grow into nothing more.
			room = min(room, held[key])
		}
		if next[key] > room {
			return &QuotaShortfall{Node: node, GPUIndex: key.index, FreeBytes: room, NeedBytes: next[key]}, nil
		}
	}
	return nil, nil
}
