// node_failover.go decides where a replica goes when its node is lost: a
// node offline for longer than the deployment's replacement wait. A
// single-node replica simply starts again from the candidate pool; a rank of
// a multi-node or PD unit names its node, so a replacement node is chosen the
// way the multi-node suggestion chooses one - same compute network, same GPU
// architecture, free cards, the model preferably already there - and the unit
// is rewritten onto it, as an operator editing the replica list would.

package planning

import (
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/netplan"
	"gpuworks.ai/gpuworks/internal/server/nodelabel"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// DefaultNodeReplaceAfter is how long a node may stay offline before the
// replicas on it are moved, unless the deployment says otherwise.
const DefaultNodeReplaceAfter = 10 * time.Minute

// NodeLost reports whether a node that is not online has been gone for at
// least after: its agent was last seen that long ago. A zero after never
// loses a node, and a node never seen is not lost (it never ran anything).
func NodeLost(node *store.Node, online bool, after time.Duration, now time.Time) bool {
	if online || after <= 0 || node == nil || node.LastSeenAt == nil {
		return false
	}
	return now.Sub(*node.LastSeenAt) >= after
}

// LostSince is when a node's replicas may move: its last sighting plus the
// wait; zero when it is not offline or never moves.
func LostSince(node *store.Node, after time.Duration) time.Time {
	if after <= 0 || node == nil || node.LastSeenAt == nil {
		return time.Time{}
	}
	return node.LastSeenAt.Add(after)
}

// ListedRank is a rank a deployment lists on a node, with the GPUs it takes
// there: a rank no instance holds yet claims them (UnitSuggestFacts.Claimed).
type ListedRank struct {
	NodeUUID string
	GPUs     int
	Leader   bool
}

// LoadUnitSuggestFacts reads what a choice of nodes for a deployment's ranks
// is decided from, in bulk: every node with its GPUs, which GPUs it cannot
// take (HeldGPUs: awake instances' and sleepers' kept against it), where the
// model is, the deployment's node requirements, and
// the nodes' resolved networks. listed are the ranks the deployment lists: a
// leader there cannot lead another replica, and a listed rank no instance of
// the deployment holds yet claims its cards.
//
// online says a node's agent is connected (the hub's Online); nil reads the
// nodes' stored state, from the nodes this already reads. ucxHang is the
// hub's live UCX hang fact: for a PD deployment in a mode whose KV moves over
// UCX it leaves the affected nodes out of every choice (UCXNetlinkBlocks);
// nil leaves none out.
//
// It reads the nodes as they are now: an operator's choice (a scale, a
// suggestion, a create) answers on the report the agent just sent.
func LoadUnitSuggestFacts(st *store.Store, resolve func([]store.Node) (netplan.Resolution, error), dep *store.Deployment, reqs nodelabel.Requirements, listed []ListedRank, online func(string) bool, ucxHang UCXNetlinkHangOf, ranks, perRank int) (UnitSuggestFacts, error) {
	nodes, err := st.ListNodes()
	if err != nil {
		return UnitSuggestFacts{}, err
	}
	return UnitSuggestFactsOn(st, nodes, resolve, dep, reqs, listed, online, ucxHang, ranks, perRank)
}

// LoadUnitSuggestFactsFromSnapshot is LoadUnitSuggestFacts on the server's
// node snapshot (store.FleetNodes), for the controller's reconciles: a
// reconcile a status batch enqueued plans on that batch (the controller
// refreshes the snapshot before enqueueing), and a round of reconciles reads
// the fleet once rather than once each.
func LoadUnitSuggestFactsFromSnapshot(st *store.Store, resolve func([]store.Node) (netplan.Resolution, error), dep *store.Deployment, reqs nodelabel.Requirements, listed []ListedRank, online func(string) bool, ucxHang UCXNetlinkHangOf, ranks, perRank int) (UnitSuggestFacts, error) {
	nodes, err := st.FleetNodes()
	if err != nil {
		return UnitSuggestFacts{}, err
	}
	return UnitSuggestFactsOn(st, nodes, resolve, dep, reqs, listed, online, ucxHang, ranks, perRank)
}

// UnitSuggestFactsOn is LoadUnitSuggestFacts over nodes already read.
func UnitSuggestFactsOn(st *store.Store, nodes []store.Node, resolve func([]store.Node) (netplan.Resolution, error), dep *store.Deployment, reqs nodelabel.Requirements, listed []ListedRank, online func(string) bool, ucxHang UCXNetlinkHangOf, ranks, perRank int) (UnitSuggestFacts, error) {
	if online == nil {
		online = StoredOnline(nodes)
	}
	held, err := HeldGPUs(st, dep.YieldPolicy)
	if err != nil {
		return UnitSuggestFacts{}, err
	}
	files, err := st.ModelFilesForModel(dep.ModelID)
	if err != nil {
		return UnitSuggestFacts{}, err
	}
	resolved, err := resolve(nodes)
	if err != nil {
		return UnitSuggestFacts{}, err
	}
	// Groups follow compute networks, so the networks are always resolved;
	// the storages only when a requirement names one.
	facts := LoadNodeFacts(st, nil, nodes, reqs)
	facts.Networks = resolved
	// A rank this deployment already runs holds its cards in held; one it
	// does not - a replica only listed so far, or any replica of a stopped
	// deployment - takes them when it starts. Counting per node, not per
	// replica, keeps a new replica that shares a node with a running one
	// honest.
	insts, err := st.InstancesForDeployment(dep.ID)
	if err != nil {
		return UnitSuggestFacts{}, err
	}
	uuidByID := make(map[uint]string, len(nodes))
	for i := range nodes {
		uuidByID[nodes[i].ID] = nodes[i].UUID
	}
	holding := map[string]int{}
	for _, inst := range insts {
		if inst.State != proto.InstStopped && inst.State != proto.InstFailed && inst.SleepLevel == 0 {
			holding[uuidByID[inst.NodeID]]++
		}
	}
	leaders, claimed := map[string]bool{}, map[string]int{}
	for _, rank := range listed {
		if rank.NodeUUID == "" {
			continue
		}
		if rank.Leader {
			leaders[rank.NodeUUID] = true
		}
		if holding[rank.NodeUUID] > 0 {
			holding[rank.NodeUUID]--
			continue
		}
		claimed[rank.NodeUUID] += rank.GPUs
	}
	return UnitSuggestFacts{
		Nodes: nodes, Held: held, Claimed: claimed, Leaders: leaders, Files: files, Facts: facts,
		Online: online, Requirements: reqs, Ranks: ranks, PerRank: perRank, PeerMemory: needsPeerMemory(dep),
		UCXNetlinkAffects: ucxHang != nil && ucxNetlinkAffects(dep), UCXNetlinkHang: ucxHang,
	}, nil
}

// ReplacementCandidates are the nodes that could take one lost rank, best
// first: on the compute network the rank's group uses and of the GPU
// architecture of the node it replaces, with the rank's cards free, not one
// of exclude (the unit's other ranks, nodes already tried), and - for a
// leader - leading no other replica. facts.PerRank is the rank's cards.
func ReplacementCandidates(facts UnitSuggestFacts, network uint, arch string, exclude map[string]bool, leading bool) []*store.Node {
	facts.Ranks = 1
	groups, _ := SuggestUnitGroups(facts)
	var out []*store.Node
	for _, group := range groups {
		if group.Network.ID != network || group.Arch != arch {
			continue
		}
		for _, candidate := range group.Nodes {
			if exclude[candidate.Node.UUID] || (leading && facts.Leaders[candidate.Node.UUID]) {
				continue
			}
			out = append(out, candidate.Node)
		}
	}
	return out
}

// NodeArch is the GPU architecture key a replacement must match: vendor and
// GPU family of the node's healthy cards (nodeArch).
func NodeArch(node *store.Node) string { return nodeArch(node) }

// WithRankOn is the unit with one rank moved to node. The rank's pinned GPUs
// are dropped: they name cards of the node it leaves, and the scheduler picks
// the new node's.
func WithRankOn(unit PDUnit, rank int, node string) PDUnit {
	out := PDUnit{NodeUUID: unit.NodeUUID, GPUIndexes: unit.GPUIndexes, Followers: append([]PDRank(nil), unit.Followers...)}
	if rank == 0 {
		out.NodeUUID, out.GPUIndexes = node, nil
		return out
	}
	if rank-1 < len(out.Followers) {
		out.Followers[rank-1] = PDRank{NodeUUID: node}
	}
	return out
}

// needsPeerMemory reports whether dep is a PD deployment in a mode that moves
// the KV over RDMA, so its instances go only where the NICs can register the
// cards' memory.
func needsPeerMemory(dep *store.Deployment) bool {
	mode, ok := pdModeOf(dep)
	return ok && mode.PeerMemory
}

// StoredOnline is an online check from the nodes' stored state, for a server
// running without a hub.
func StoredOnline(nodes []store.Node) func(string) bool {
	online := make(map[string]bool, len(nodes))
	for i := range nodes {
		online[nodes[i].UUID] = nodes[i].State == store.NodeStateOnline
	}
	return func(uuid string) bool { return online[uuid] }
}
