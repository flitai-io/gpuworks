package scheduler

import (
	serverestimate "gpuworks.ai/gpuworks/internal/server/estimate"
	"gpuworks.ai/gpuworks/pkg/topoparse"
)

// placement is one admissible candidate: the GPU group, how well it is
// connected, and the per-GPU memory the plan reports for it.
type placement struct {
	group     []int
	conn      string
	topoScore int
	need      int64
	claim     int64 // per-GPU claim on GPUs shared by memory (sharedClaim)
	prefer    int64 // placement contract ordering key (CandidateEvaluation.Preference)
}

// plan enumerates tensor-parallel options and, for each, selects the best group
// of free GPUs by interconnect quality. It returns one candidate plan per viable
// tp plus the rejected options, in the order they were considered, so the
// explanation can name the option that was actually binding.
func plan(req Request, node NodeSnapshot) ([]Plan, []Rejected) {
	var plans []Plan
	var rejected []Rejected

	// A dGPU memory-limited deployment shares a GPU; its per-GPU need is the
	// limit + context reserve. tp>1 for shared deployments is out of scope, so
	// limited requests only consider tp=1.
	tps := allowedTPs(req, countSchedulable(node.GPUs))
	if req.PerGPULimitBytes > 0 {
		tps = []int{1}
	}

	// GPUs whose live allocation mode is usable at all. Model memory is applied
	// per candidate: an opt-in placement contract may replace the generic
	// weight-derived estimate once it sees the concrete group.
	eligible := allowedGPUs(usableGPUs(node.GPUs, req, 0), req.AllowedGPUIndexes)

	for _, tp := range tps {
		// tp must divide the attention head count (when known) for valid sharding.
		if heads := headTP(req, tp); req.HeadCount > 0 && req.HeadCount%heads != 0 {
			rejected = append(rejected, Rejected{TP: tp, Reason: reasonTPNotDivisible(heads, req.HeadCount)})
			continue
		}
		if len(eligible) < tp {
			rejected = append(rejected, Rejected{TP: tp, Reason: reasonInsufficientGPUs(req, tp, reportNeed(req, tp, 0), len(eligible))})
			continue
		}
		placed, reason, ok := admit(req, node, eligible, tp)
		if !ok {
			rejected = append(rejected, Rejected{GPUIndexes: placed.group, TP: tp, Reason: reason})
			continue
		}
		reasons := []Reason{{Code: topologyReasonCode(placed.conn), Params: groupParams(req, tp), Score: placed.topoScore}}
		if placed.prefer != 0 {
			reasons = append(reasons, Reason{Code: ReasonPlacementPreference,
				Params: map[string]any{"preference": placed.prefer, "count": len(placed.group)}})
		}
		plans = append(plans, Plan{
			GPUIndexes:  placed.group,
			TP:          tp,
			PerGPUBytes: placed.need,
			ClaimBytes:  placed.claim,
			Preference:  placed.prefer,
			Reasons:     reasons,
		})
	}
	return plans, rejected
}

// admit decides whether one tp can be placed on this node. The chosen group is
// the best-connected one that qualifies; when its memory is short the planner
// retries the same tp across the whole pool before rejecting it, because another
// combination of the same size may fit.
func admit(req Request, node NodeSnapshot, pool []GPU, tp int) (placement, Reason, bool) {
	placed, evaluation, reason, ok := pick(req, node, pool, tp)
	if !ok {
		return placement{}, reason, false
	}
	need := admitNeed(req, tp, evaluation.GPUMemoryBytesPerDevice)
	if req.PerGPULimitBytes > 0 && evaluation.GPUMemoryBytesPerDevice > req.PerGPULimitBytes {
		// The catalog contract says each device needs more than this deployment's
		// memory limit allows.
		return placement{}, reasonInsufficientGPUs(req, tp, evaluation.GPUMemoryBytesPerDevice,
			len(usableGPUs(pool, req, evaluation.GPUMemoryBytesPerDevice))), false
	}
	if fits(req, pool, placed.group, need) {
		return placement{group: placed.group, conn: placed.conn, topoScore: placed.topoScore, prefer: evaluation.Preference,
			need: reportNeed(req, tp, evaluation.GPUMemoryBytesPerDevice), claim: sharedClaim(req, pool, placed.group, need)}, Reason{}, true
	}

	memoryPool := usableGPUs(pool, req, need)
	if len(memoryPool) < tp {
		if reason, blocked := sleepResidueBlock(req, pool, tp, need); blocked {
			return placement{group: placed.group}, reason, false
		}
		// The rejection names the group that came closest, the one its
		// figures describe, not the best-connected group tried first.
		return placement{group: roomiestGroup(req, pool, tp)}, reasonMemoryShortfall(req, pool, tp, need, evaluation.GPUMemoryBytesPerDevice == 0), false
	}
	retry, retryEvaluation, reason, ok := pick(req, node, memoryPool, tp)
	if !ok {
		return placement{group: retry.group}, reason, false
	}
	retryNeed := admitNeed(req, tp, retryEvaluation.GPUMemoryBytesPerDevice)
	if !fits(req, memoryPool, retry.group, retryNeed) {
		return placement{group: retry.group}, reasonMemoryShortfall(req, memoryPool, tp, retryNeed, retryEvaluation.GPUMemoryBytesPerDevice == 0), false
	}
	return placement{group: retry.group, conn: retry.conn, topoScore: retry.topoScore, prefer: retryEvaluation.Preference,
		need: reportNeed(req, tp, retryEvaluation.GPUMemoryBytesPerDevice), claim: sharedClaim(req, memoryPool, retry.group, retryNeed)}, Reason{}, true
}

// sharedClaim is the per-GPU claim of a placed group whose GPUs are shared by
// memory, zero otherwise.
func sharedClaim(req Request, pool []GPU, group []int, need int64) int64 {
	var claim int64
	for _, gpu := range selectGPUsByIndex(pool, group) {
		if gpu.SharedByMemory {
			claim = max(claim, gpu.Claim(req, need))
		}
	}
	return claim
}

// pick selects the best-connected group of tp GPUs from a pool and applies the
// optional placement contract to it.
func pick(req Request, node NodeSnapshot, pool []GPU, tp int) (placement, CandidateEvaluation, Reason, bool) {
	group, conn, topoScore := selectGroup(pool, tp, node.Topology)
	evaluation := evaluateCandidate(req, selectGPUsByIndex(pool, group))
	if evaluation.RejectionReason != "" {
		return placement{group: group}, evaluation, reasonPlacementRejected(evaluation.RejectionReason), false
	}
	return placement{group: group, conn: conn, topoScore: topoScore}, evaluation, Reason{}, true
}

// roomiestGroup is the tp qualifying GPUs with the most capacity for this
// request: the group a memory shortfall describes (bindingUsableCapacity).
func roomiestGroup(req Request, pool []GPU, tp int) []int {
	qualifying := qualifyingByCapacity(req, pool)
	if len(qualifying) > tp {
		qualifying = qualifying[:tp]
	}
	return indexesOf(qualifying)
}

func fits(req Request, pool []GPU, group []int, need int64) bool {
	for _, gpu := range selectGPUsByIndex(pool, group) {
		if !gpu.UsableFor(req, need) {
			return false
		}
	}
	return true
}

// reportNeed is the per-GPU requirement a plan states for the deploy UI. A
// placement contract may have measured a different layout than the weight-derived
// estimate.
//
// A memory-limited deployment is charged its limit plus what its partition
// backend lets the engine hold outside it (estimate.PartitionClaimBytes). That
// is what planning's ledger records against the GPU for a running instance, so
// admission and the inventory cannot disagree about what a shared instance
// costs.
func reportNeed(req Request, tp int, contract int64) int64 {
	if req.PerGPULimitBytes > 0 {
		return serverestimate.PartitionClaimBytes(req.PerGPULimitBytes, req.PartitionBackend, func() int64 { return reserveFor(req) })
	}
	if contract > 0 {
		return contract
	}
	return perGPUNeed(req, tp)
}

// admitNeed is what admission is measured against. A shareable deployment is
// admitted against its limit, never against a contract that asks for more memory
// than the deployment may claim.
func admitNeed(req Request, tp int, contract int64) int64 {
	if req.PerGPULimitBytes > 0 {
		return reportNeed(req, tp, 0)
	}
	return reportNeed(req, tp, contract)
}

func evaluateCandidate(req Request, candidate []GPU) CandidateEvaluation {
	if req.EvaluateCandidate == nil {
		return CandidateEvaluation{}
	}
	return req.EvaluateCandidate(candidate)
}

// allowedGPUs narrows a pool to the GPUs a request names. An empty list means
// the request named none, which is a request for every GPU rather than for no
// GPU at all.
func allowedGPUs(values []GPU, allowed []int) []GPU {
	if len(allowed) == 0 {
		return values
	}
	filtered := make([]GPU, 0, len(values))
	for _, gpu := range values {
		if containsInt(allowed, gpu.Index) {
			filtered = append(filtered, gpu)
		}
	}
	return filtered
}

func selectGPUsByIndex(values []GPU, indexes []int) []GPU {
	result := make([]GPU, 0, len(indexes))
	for _, index := range indexes {
		for _, gpu := range values {
			if gpu.Index == index {
				result = append(result, gpu)
				break
			}
		}
	}
	return result
}

func containsInt(values []int, want int) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// selectGroup chooses tp GPUs from eligible, preferring (1) a subset of a
// fast-interconnect clique, (2) same-NUMA GPUs, (3) any. Returns the indexes, a
// connectivity tag, and the topology score contribution.
func selectGroup(eligible []GPU, tp int, topo *topoparse.Topology) ([]int, string, int) {
	idx := indexesOf(eligible)
	if tp == 1 {
		return idx[:1], "single", 0
	}

	// (1) Fast-interconnect clique subset.
	if topo != nil {
		for _, clique := range fastCliques(topo) {
			inClique := intersectPreserve(idx, clique)
			if len(inClique) >= tp {
				return inClique[:tp], "nvlink", 30
			}
		}
	}
	// (2) same NUMA node.
	byNUMA := map[int][]int{}
	numaOf := map[int]int{}
	for _, g := range eligible {
		byNUMA[g.NUMANode] = append(byNUMA[g.NUMANode], g.Index)
		numaOf[g.Index] = g.NUMANode
	}
	for numa, gs := range byNUMA {
		if numa >= 0 && len(gs) >= tp {
			return gs[:tp], "numa", 10
		}
	}
	// (3) any — cross-NUMA/PCIe.
	return idx[:tp], "cross", -10
}

func fastCliques(topo *topoparse.Topology) [][]int {
	if topo == nil {
		return nil
	}
	if len(topo.FastCliques) > 0 {
		return topo.FastCliques
	}
	return topo.NVLinkCliques
}

func topologyReasonCode(conn string) string {
	switch conn {
	case "nvlink":
		return ReasonTopologyNVLink
	case "numa":
		return ReasonTopologyNUMA
	case "cross":
		return ReasonTopologyCross
	default:
		return ReasonTopologySingle
	}
}

func indexesOf(gpus []GPU) []int {
	out := make([]int, len(gpus))
	for i, g := range gpus {
		out[i] = g.Index
	}
	return out
}

// intersectPreserve returns the elements of a that are in b, preserving a's order.
func intersectPreserve(a, b []int) []int {
	set := make(map[int]bool, len(b))
	for _, x := range b {
		set[x] = true
	}
	var out []int
	for _, x := range a {
		if set[x] {
			out = append(out, x)
		}
	}
	return out
}
