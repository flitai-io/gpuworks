package planning

import (
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/nodelabel"
	"gpuworks.ai/gpuworks/internal/server/scheduler"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// PoolTrialBudget is how many of a pool's candidates one placement plans in
// full - the controller per pass, the deploy form's automatic placement per
// evaluation. Ranking the whole pool reads every node from a handful of bulk
// queries; a full plan reads one node's ledger, so the plans are what is
// bounded: a pool of a thousand nodes costs the same few plans as a pool of
// three.
const PoolTrialBudget = 8

// ModelLocality is how close a candidate is to having the deployment's model:
// a node that holds it starts a replica at once, one already fetching it is
// on its way, any other has the whole download ahead of it.
type ModelLocality int

const (
	ModelAbsent ModelLocality = iota
	ModelFetching
	ModelPresent
)

// ModelLocalityOf reads a node's model file row (nil when it has none).
func ModelLocalityOf(file *store.ModelFile) ModelLocality {
	if file == nil {
		return ModelAbsent
	}
	switch file.State {
	case proto.MFPresent, proto.MFRegistered:
		return ModelPresent
	case proto.MFPending, proto.MFDownloading, proto.MFSyncing:
		return ModelFetching
	}
	return ModelAbsent
}

// bestFileOn is each node's closest copy of the model: a node may hold it in
// several storages, and it reads whichever it has (store.ModelSourceOn). It
// is a ranking hint over rows read once for the whole pool, not the decision:
// the trial plan and the launch ask ModelSourceOn for the node they pick.
func bestFileOn(files []store.ModelFile) map[uint]*store.ModelFile {
	out := make(map[uint]*store.ModelFile, len(files))
	for i := range files {
		file := &files[i]
		if file.NodeID == 0 {
			continue
		}
		if have := out[file.NodeID]; have == nil || ModelLocalityOf(file) > ModelLocalityOf(have) {
			out[file.NodeID] = file
		}
	}
	return out
}

// PoolCandidate is one node of a deployment's candidate pool, with the facts
// the ranking reads. Every fact is derived in bulk - one read of the node,
// instance and model-file tables for the whole pool - so ranking a thousand
// candidates costs no per-node query.
type PoolCandidate struct {
	Node *store.Node
	// Replicas is how many of the deployment's replicas already run on it.
	Replicas int
	// FreeGPUs is how many selectable healthy GPUs no awake instance holds:
	// an approximation for ordering, never a placement decision.
	FreeGPUs int
	Model    ModelLocality
	// Sticky marks a node the deployment has run on before (an instance of
	// it is still there, on its way out or kept as a spent attempt): its
	// caches are warm.
	Sticky bool
}

// RankPool orders a pool for a replica needing need whole GPUs (0 for a
// memory-limited replica that can share cards): the order the controller
// tries candidates in, best first.
//
// Candidates that look like they have room come first; the free-GPU count is
// an approximation, so the others are not dropped, only tried last. Then the
// strategy: spread prefers the node running the fewest of the deployment's
// replicas, binpack the node with the least room left. Then the model's
// locality, then a node the deployment ran on before, then - under spread -
// the most room, so replicas land where they are least likely to crowd later
// ones. The node id breaks the remaining ties, so the order is stable.
func RankPool(candidates []PoolCandidate, strategy store.PlacementStrategy, need int) []PoolCandidate {
	out := append([]PoolCandidate(nil), candidates...)
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if fa, fb := a.FreeGPUs >= need, b.FreeGPUs >= need; fa != fb {
			return fa
		}
		if strategy == store.PlacementBinpack {
			if a.FreeGPUs != b.FreeGPUs {
				return a.FreeGPUs < b.FreeGPUs
			}
		} else if a.Replicas != b.Replicas {
			return a.Replicas < b.Replicas
		}
		if a.Model != b.Model {
			return a.Model > b.Model
		}
		if a.Sticky != b.Sticky {
			return a.Sticky
		}
		if strategy != store.PlacementBinpack && a.FreeGPUs != b.FreeGPUs {
			return a.FreeGPUs > b.FreeGPUs
		}
		return a.Node.ID < b.Node.ID
	})
	return out
}

// FreeHealthyGPUs counts a node's healthy GPUs that holders does not list.
func FreeHealthyGPUs(node *store.Node, held map[int]bool) int {
	return freeHealthyGPUs(node, held, nil)
}

func freeHealthyGPUs(node *store.Node, held map[int]bool, allowed []int) int {
	free := 0
	for _, gpu := range node.OwnedGPUs() {
		if gpu.CanTakeWork() && !held[gpu.Index] && (len(allowed) == 0 || slices.Contains(allowed, gpu.Index)) {
			free++
		}
	}
	return free
}

// HardwareShape names what a node permanently is, as far as placing a
// replica goes: its vendor and CPU architecture, each card's model, memory
// and compute capability, and whether it may serve. Two nodes of one shape
// answer an unloaded plan the same way, so a pool is judged once per shape.
func HardwareShape(node *store.Node, canServe bool) string {

	cards := make([]string, 0, len(node.GPUs))
	for _, gpu := range node.OwnedGPUs() {
		cards = append(cards, fmt.Sprintf("%s/%s/%d/%s/%s", accelerator.NormalizeVendor(node.VendorOf(gpu)), accelerator.NormalizeGPUName(gpu.Name), gpu.MemoryTotal, gpu.ComputeCapability, gpu.ArchID))
	}
	sort.Strings(cards)
	return fmt.Sprintf("%s|%s|%s|%s|%t|%s", accelerator.NormalizeVendor(node.GPUVendor), node.OS, node.Arch, node.RuntimeDriver, canServe, strings.Join(cards, ","))
}

// PoolStatus is the first check a node fails for one of a deployment's
// replicas, or that it passes them all as far as a bulk read can tell. Each
// node has exactly one, so the counts of a pool add up to its node count.
type PoolStatus string

const (
	// PoolRoom: a candidate that looks like it has room; only a full plan
	// on the node can say for sure.
	PoolRoom PoolStatus = "room"
	// PoolBusy: a candidate whose cards look taken by awake instances.
	PoolBusy PoolStatus = "busy"
	// PoolUnmet: the node does not satisfy the deployment's requirements.
	PoolUnmet PoolStatus = "unmet"
	// PoolCordoned: the node satisfies them but an operator stopped
	// scheduling on it, cordoned or draining (store.Node.Scheduling).
	PoolCordoned PoolStatus = "cordoned"
	// PoolOffline: the node satisfies them but is offline.
	PoolOffline PoolStatus = "offline"
)

// PoolStatuses is every status a pool entry can have, candidates first.
func PoolStatuses() []PoolStatus {
	return []PoolStatus{PoolRoom, PoolBusy, PoolUnmet, PoolCordoned, PoolOffline}
}

// PoolEntry is one node of the cluster as the deployment's pool sees it.
type PoolEntry struct {
	PoolCandidate
	Status PoolStatus
	// Rank is the candidate's position in the order replicas try nodes in
	// (1-based); zero for a node that is not a candidate.
	Rank int
	// UnmetKey and Detail say which requirement an unmet node fails.
	UnmetKey string
	Detail   string
	// Reason is the structured refusal of a node whose accelerators cannot
	// run the deployment (UnmetKey PoolUnmetAccelerator); the client renders
	// it by code, so Detail stays empty.
	Reason *scheduler.Reason
}

// PoolUnmetAccelerator is the UnmetKey of nodes refused because no card can
// run the deployment's launch (host runtime or accelerator feature support).
const PoolUnmetAccelerator = "accelerator"

// PoolFacts is everything a pool is built from, each read in bulk.
type PoolFacts struct {
	Nodes []store.Node
	// Held is which GPUs the deployment cannot take, by node: awake
	// instances hold them, or sleepers keep them against it (HeldGPUs).
	Held map[uint]map[int]bool
	// Files are the model's file rows on every node.
	Files []store.ModelFile
	// Facts are what matching the requirements reads beyond the node rows
	// (LoadNodeFacts).
	Facts NodeFacts
	// Instances are the deployment's own instances.
	Instances    []store.Instance
	Online       func(uuid string) bool
	Requirements nodelabel.Requirements
	Strategy     store.PlacementStrategy
	// Need is the whole GPUs one replica takes (0 when it shares cards).
	Need int
	// Placement restricts the free-card approximation to the GPUs a replica
	// may actually use, just as the full scheduler trial does.
	Placement Placement
	// TargetRefusal returns the node's refusal, or nil for a compatible target.
	TargetRefusal func(*store.Node) *scheduler.Reason
}

// Pool is a deployment's candidate pool: every node classified, and the
// candidates ranked in the order replicas try them.
type Pool struct {
	Ranked  []PoolCandidate
	Entries []PoolEntry
	Counts  map[PoolStatus]int
	// UnmetByKey counts unmet nodes by the requirement they fail first.
	UnmetByKey map[string]int
	// UnmetDetail says why the most telling unmet node is excluded: one the
	// deployment is pinned to when it is pinned, else the first.
	UnmetDetail string
}

// BuildPool classifies every node for a deployment's next replica and ranks
// the candidates (RankPool). It reads nothing itself: the facts come from a
// handful of bulk reads, so it costs the same whether the caller is the
// controller placing a replica or the API explaining why none was placed.
func BuildPool(f PoolFacts) *Pool {
	fileOn := bestFileOn(f.Files)
	replicas, sticky := map[uint]int{}, map[uint]bool{}
	for _, inst := range f.Instances {
		if inst.Headless {
			continue
		}
		sticky[inst.NodeID] = true
		if InstanceActive(inst.State) {
			replicas[inst.NodeID]++
		}
	}
	pool := &Pool{Counts: map[PoolStatus]int{}, UnmetByKey: map[string]int{}}
	var candidates []PoolCandidate
	for i := range f.Nodes {
		node := &f.Nodes[i]
		entry := PoolEntry{PoolCandidate: PoolCandidate{
			Node: node, Replicas: replicas[node.ID], FreeGPUs: freeHealthyGPUs(node, f.Held[node.ID], f.Placement.GPUIndexes),
			Model: ModelLocalityOf(fileOn[node.ID]), Sticky: sticky[node.ID],
		}}
		var targetRefusal *scheduler.Reason
		switch err := MatchRequirements(node, f.Facts, f.Requirements); {
		case err != nil:
			entry.Status, entry.Detail = PoolUnmet, err.Error()
			var unmet *UnmetRequirement
			if errors.As(err, &unmet) {
				entry.UnmetKey = unmet.Key
			}
			if pool.UnmetDetail == "" && entry.UnmetKey != nodelabel.KeyNode {
				pool.UnmetDetail = entry.Detail
			}
		case f.TargetRefusal != nil && func() bool { targetRefusal = f.TargetRefusal(node); return targetRefusal != nil }():
			entry.Status, entry.UnmetKey, entry.Reason = PoolUnmet, PoolUnmetAccelerator, targetRefusal
		case !node.Scheduling.TakesReplicas():
			entry.Status = PoolCordoned
		case f.Online != nil && !f.Online(node.UUID):
			entry.Status = PoolOffline
		default:
			candidates = append(candidates, entry.PoolCandidate)
			continue
		}
		pool.Counts[entry.Status]++
		if entry.Status == PoolUnmet {
			pool.UnmetByKey[entry.UnmetKey]++
		}
		pool.Entries = append(pool.Entries, entry)
	}
	pool.Ranked = RankPool(candidates, f.Strategy, f.Need)
	ranked := make([]PoolEntry, 0, len(pool.Ranked))
	for index, candidate := range pool.Ranked {
		status := PoolRoom
		if candidate.FreeGPUs < f.Need {
			status = PoolBusy
		}
		pool.Counts[status]++
		ranked = append(ranked, PoolEntry{PoolCandidate: candidate, Status: status, Rank: index + 1})
	}
	pool.Entries = append(ranked, pool.Entries...)
	return pool
}

// ReplicaGPUNeed is how many whole GPUs one replica of a single-node
// deployment takes, for ranking. A memory-limited replica shares cards, so a
// busy card does not rule a node out and it needs none.
func ReplicaGPUNeed(dep *store.Deployment) int {
	if dep.VRAMLimitBytes > 0 {
		return 0
	}
	placement, err := ParsePlacement(dep.Placement)
	if err != nil {
		return 0
	}
	spec, err := DeploymentParallelism(dep, placement)
	if err != nil {
		return 0
	}
	spec.NNodes = 1
	need, err := spec.GPUsPerNode()
	if err != nil {
		return 0
	}
	if !spec.Stated() {
		return spec.MinimumGPUs()
	}
	return need
}
