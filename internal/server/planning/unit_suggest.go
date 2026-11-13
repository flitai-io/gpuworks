// unit_suggest.go suggests the nodes of one multi-node replica. A multi-node
// replica is not placed automatically - choosing the wrong group (ranks
// across switches, RDMA that does not reach) costs far more than for a
// single-node one - so the platform proposes a group and the operator
// confirms it as a listed replica.

package planning

import (
	"sort"

	"gpuworks.ai/gpuworks/internal/proto"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/server/netplan"
	"gpuworks.ai/gpuworks/internal/server/nodelabel"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// UnitSuggestFacts is what a suggestion is decided from, read in bulk.
type UnitSuggestFacts struct {
	Nodes []store.Node
	// Held is which GPUs the deployment cannot take, by node: awake
	// instances hold them, or sleepers keep them against it (HeldGPUs).
	Held map[uint]map[int]bool
	// Claimed is how many GPUs per node the replicas already listed - and
	// not yet running - will take, by node UUID.
	Claimed map[string]int
	// Leaders are the nodes that already lead a replica of the deployment:
	// a node leads at most one, since every leader listens on the master port.
	Leaders map[string]bool
	// Files is where the model already is.
	Files []store.ModelFile
	// Facts are the nodes' networks (always resolved: groups follow compute
	// networks) and, when a requirement names one, their storages.
	Facts        NodeFacts
	Online       func(uuid string) bool
	Requirements nodelabel.Requirements
	Ranks        int
	PerRank      int
	// Strategy orders each group's nodes after the model: binpack the fewest
	// free cards that still fit, anything else (spread, unset) the most.
	Strategy store.PlacementStrategy
	// PeerMemory leaves out the nodes whose RDMA NICs cannot register their
	// cards' memory: the deployment is a PD mode over RDMA
	// (pdmode.Mode.PeerMemory).
	PeerMemory bool
}

// UnitGroup is a set of nodes that could carry one replica: they share a
// compute network and a GPU architecture, each has PerRank free cards, and
// at least one may lead. Nodes are best first: the model already there,
// then the most free cards.
type UnitGroup struct {
	Network netplan.Network
	Arch    string
	Nodes   []PoolCandidate
}

// UnitSuggestCounts is how many nodes each check left out, each node counted
// once by the first it fails, for saying why nothing was suggested.
type UnitSuggestCounts struct {
	Nodes, Unmet, Cordoned, Offline, Busy, NoNetwork int
	// NoPeerMemory are the nodes left out because the peer-memory module is
	// not loaded (UnitSuggestFacts.PeerMemory).
	NoPeerMemory int
	// Largest is the most nodes any group has, short of Ranks.
	Largest int
}

// SuggestUnitGroups groups the nodes that could carry a rank and returns the
// groups large enough for a replica, most promising first: most nodes with
// the model already there, then most nodes, then most free cards.
func SuggestUnitGroups(f UnitSuggestFacts) ([]UnitGroup, UnitSuggestCounts) {
	counts := UnitSuggestCounts{Nodes: len(f.Nodes)}
	fileOn := bestFileOn(f.Files)
	type key struct {
		network uint
		arch    string
	}
	groups := map[key]*UnitGroup{}
	var order []key
	for i := range f.Nodes {
		node := &f.Nodes[i]
		switch {
		case MatchRequirements(node, f.Facts, f.Requirements) != nil:
			counts.Unmet++
			continue
		case !node.Scheduling.TakesReplicas():
			counts.Cordoned++
			continue
		case f.Online != nil && !f.Online(node.UUID):
			counts.Offline++
			continue
		case f.PeerMemory && node.PeerMemory != proto.PeerMemoryLoaded:
			counts.NoPeerMemory++
			continue
		}
		free := FreeHealthyGPUs(node, f.Held[node.ID]) - f.Claimed[node.UUID]
		if free < f.PerRank {
			counts.Busy++
			continue
		}
		compute, ok := f.Facts.Networks.For(node.ID, netplan.RoleCompute)
		if !ok {
			counts.NoNetwork++
			continue
		}
		k := key{network: compute.Network.ID, arch: nodeArch(node)}
		group := groups[k]
		if group == nil {
			group = &UnitGroup{Network: compute.Network, Arch: k.arch}
			groups[k] = group
			order = append(order, k)
		}
		group.Nodes = append(group.Nodes, PoolCandidate{Node: node, FreeGPUs: free, Model: ModelLocalityOf(fileOn[node.ID])})
	}
	var out []UnitGroup
	for _, k := range order {
		group := groups[k]
		sort.SliceStable(group.Nodes, func(a, b int) bool {
			x, y := group.Nodes[a], group.Nodes[b]
			if x.Model != y.Model {
				return x.Model > y.Model
			}
			if x.FreeGPUs != y.FreeGPUs {
				if f.Strategy == store.PlacementBinpack {
					return x.FreeGPUs < y.FreeGPUs
				}
				return x.FreeGPUs > y.FreeGPUs
			}
			return x.Node.ID < y.Node.ID
		})
		if len(group.Nodes) < max(f.Ranks, 1) || leaderIndex(group.Nodes, f.Leaders) < 0 {
			counts.Largest = max(counts.Largest, len(group.Nodes))
			continue
		}
		out = append(out, *group)
	}
	sort.SliceStable(out, func(a, b int) bool {
		x, y := out[a], out[b]
		if px, py := modelPresent(x.Nodes), modelPresent(y.Nodes); px != py {
			return px > py
		}
		if len(x.Nodes) != len(y.Nodes) {
			return len(x.Nodes) > len(y.Nodes)
		}
		return freeCards(x.Nodes) > freeCards(y.Nodes)
	})
	return out, counts
}

// UnitFromGroup is a group's best replica of ranks nodes skipping those in
// skip: the first node that may lead, then the best others. ok is false when
// the group has too few left.
func UnitFromGroup(group UnitGroup, ranks int, leaders, skip map[string]bool) ([]*store.Node, bool) {
	var left []PoolCandidate
	for _, candidate := range group.Nodes {
		if !skip[candidate.Node.UUID] {
			left = append(left, candidate)
		}
	}
	lead := leaderIndex(left, leaders)
	if lead < 0 || len(left) < ranks {
		return nil, false
	}
	nodes := []*store.Node{left[lead].Node}
	for i, candidate := range left {
		if len(nodes) == ranks {
			break
		}
		if i != lead {
			nodes = append(nodes, candidate.Node)
		}
	}
	return nodes, len(nodes) == ranks
}

func leaderIndex(nodes []PoolCandidate, leaders map[string]bool) int {
	for i, candidate := range nodes {
		if !leaders[candidate.Node.UUID] {
			return i
		}
	}
	return -1
}

func modelPresent(nodes []PoolCandidate) int {
	n := 0
	for _, candidate := range nodes {
		if candidate.Model == ModelPresent {
			n++
		}
	}
	return n
}

func freeCards(nodes []PoolCandidate) int {
	n := 0
	for _, candidate := range nodes {
		n += candidate.FreeGPUs
	}
	return n
}

// nodeArch is the GPU architecture a node's healthy cards share, with the
// vendor, so two vendors' families never meet in one group; a node mixing
// architectures is its own kind ("mixed:" and the families).
func nodeArch(node *store.Node) string {
	vendor := accelerator.NormalizeVendor(node.GPUVendor)
	families := map[string]bool{}
	for _, gpu := range node.OwnedGPUs() {
		if gpu.CanTakeWork() {
			families[node.ArchOf(gpu).Family] = true
		}
	}
	if len(families) == 1 {
		for family := range families {
			return vendor + "/" + family
		}
	}
	names := make([]string, 0, len(families))
	for family := range families {
		names = append(names, family)
	}
	sort.Strings(names)
	key := vendor + "/mixed"
	for _, name := range names {
		key += ":" + name
	}
	return key
}
