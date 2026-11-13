// Package scheduler implements "user selects the node, the system selects the
// GPUs". Given a node snapshot (GPUs + topology + derived allocation ledger) and
// a request (weight/KV bytes, head count), it runs filters → a GPU planner that
// enumerates tensor-parallel options and prunes by interconnect quality →
// scorers, and produces a Plan plus a schedule_explain structure that both the
// instance timeline and the topology visualization consume.
package scheduler

import (
	"fmt"

	"gpuworks.ai/gpuworks/internal/accelerator"
	serverestimate "gpuworks.ai/gpuworks/internal/server/estimate"
	"gpuworks.ai/gpuworks/internal/server/parallelism"
	"gpuworks.ai/gpuworks/pkg/topoparse"
)

// GPU is one accelerator in a node snapshot. AllocatedBytes is the sum of claims
// from running instances (a whole-GPU instance claims the full memory; a dGPU
// memory-limited instance claims limit+engine-reserve), so the ledger supports
// both whole-GPU placement and GPU sharing.
type GPU struct {
	// WholeGPUAllocated distinguishes an exclusive claim from shared claims
	// that happen to fill the card; it only explains resource refusals.
	WholeGPUAllocated bool

	Index          int
	MemoryTotal    int64
	AllocatedBytes int64
	SleepingBytes  int64
	NUMANode       int
	// Schedulable is whether work may go on this card at all
	// (store.GPU.CanTakeWork): this node's own, at a level that serves,
	// and not held outside the platform.
	Schedulable bool
	// HeldOutsidePlatform marks a card a process outside the platform is
	// using — one reason it is not Schedulable that is not a fault, and
	// that the refusal names so an operator looks at the right thing.
	HeldOutsidePlatform bool
	// Degraded marks a healthy GPU the agent flagged at warning level (a memory
	// repair waiting for a reset, a vendor-tool warning): it may be placed on,
	// but only when the fully healthy GPUs cannot take the request.
	Degraded          bool
	PartitionBackends []string // backends verified for this physical GPU
	// SharedByMemory marks a GPU several engines use at once, each taking
	// memory from the same pool (accelerator.SharedByMemory): there is no
	// whole card to hand out, so a request is admitted against what is
	// unclaimed, by the share it will take (Claim).
	SharedByMemory bool
}

// Available returns the unclaimed memory on the GPU.
func (g GPU) Available() int64 { return g.MemoryTotal - g.AllocatedBytes - g.SleepingBytes }

// Empty reports whether the GPU has no claims (needed for whole-GPU placement).
func (g GPU) Empty() bool { return g.AllocatedBytes <= 0 }

// CapacityFor is the memory this GPU offers a request: its unclaimed memory when
// the request shares the GPU (a dGPU memory limit), or the engine's
// utilization-adjusted total when the request takes the GPU whole. A whole-GPU
// request must still pass the residue check in UsableFor: vLLM needs that
// utilization amount free before it can start.
func (g GPU) CapacityFor(req Request) int64 {
	if req.PerGPULimitBytes > 0 || g.SharedByMemory {
		return g.Available()
	}
	return int64(float64(g.MemoryTotal) * req.GPUMemUtil)
}

// QualifiesFor reports whether the GPU can host the request at all, before any
// per-candidate memory requirement is applied: a shared request needs a healthy
// GPU, a whole-GPU request needs one that is also free.
func (g GPU) QualifiesFor(req Request) bool {
	if !g.Schedulable {
		return false
	}
	if req.PerGPULimitBytes > 0 {
		return req.PartitionBackend != "" && contains(g.PartitionBackends, req.PartitionBackend)
	}
	return g.SharedByMemory || g.Empty()
}

// Claim is the memory a request admitted with need takes from this GPU: need
// itself, except on a GPU shared by memory, where an engine that sizes itself
// by utilization (UtilizationIsShare) takes that share of the card whenever it
// is the larger - two such engines at 0.8 must not both be let in because
// each starts with less. A zero need asks only for structural eligibility.
func (g GPU) Claim(req Request, need int64) int64 {
	if !g.SharedByMemory || req.PerGPULimitBytes > 0 || need <= 0 || !req.UtilizationIsShare {
		return need
	}
	if share := int64(float64(g.MemoryTotal) * req.GPUMemUtil); share > need {
		return share
	}
	return need
}

// UsableFor is the single admission predicate: the planner accepts a candidate
// with it and the explanation reports failures with it, so a verdict and its
// message cannot disagree.
func (g GPU) UsableFor(req Request, need int64) bool {
	if !g.QualifiesFor(req) || g.CapacityFor(req) < g.Claim(req, need) {
		return false
	}
	// A sleeping residue can coexist with a whole-GPU placement only when the
	// engine's startup reservation still fits in the currently free memory. The
	// zero-need pass is structural eligibility; defer this check until the real
	// candidate requirement is known so the explanation can name the residue.
	if req.PerGPULimitBytes == 0 && !g.SharedByMemory && need > 0 && g.SleepingBytes > 0 {
		startupReservation := int64(float64(g.MemoryTotal) * req.GPUMemUtil)
		if startupReservation > g.MemoryTotal-g.SleepingBytes {
			return false
		}
	}
	return true
}

// NodeSnapshot is the scheduling input for one (user-selected) node.
type NodeSnapshot struct {
	UUID string
	// Vendor is the node's accelerator vendor, which decides the platform
	// features it provides (accelerator.Supports).
	Vendor                string
	GPUs                  []GPU
	Topology              *topoparse.Topology // may be nil (no topo probed)
	DtypeSupported        bool
	DtypeReason           string
	DiskAvailBytes        int64
	RuntimeDiskAvailBytes int64
	// ServingUnavailable marks a node with no network that may carry serving
	// traffic: no serving network, and the management network may not.
	ServingUnavailable bool
}

// Request describes what to place. Weight and multimodal bytes are the startup
// requirements; KV cache is deliberately absent because engines allocate those
// blocks dynamically and admission does not depend on them.
type Request struct {
	// ReserveBase is the engine's own declared holdback (deploy.Profile.
	// ReserveBase). Like ReserveBytes it is carried rather than looked up,
	// because the scheduler knows no engine facts; it only matters to a caller
	// that did not compute ReserveBytes.
	ReserveBase    int64
	WeightBytes    int64
	MMEncoderBytes int64
	// ReserveBytes is the engine's non-weight, non-KV memory for this deployment
	// (estimate.ReserveBytes). It is carried on the request because it depends on
	// the target architecture and on whether the deployment captures CUDA graphs,
	// neither of which the scheduler sees.
	ReserveBytes         int64
	DiskNeedBytes        int64
	RuntimeDiskNeedBytes int64
	// StorageRefusal is set when no storage the deployment may use can serve
	// the node; planning works it out because the scheduler knows no storages.
	StorageRefusal *Reason
	HeadCount      int   // attention heads (tp must divide this)
	AllowedTPs     []int // default powers of two up to the node's healthy GPU count
	// Parallel is the deployment's parallelism. When its TP is stated, the
	// group on this node is its per-node GPU count, the weights are spread
	// over all TP×PP GPUs of the deployment, and the head count must divide
	// its TP; AllowedTPs is then ignored. With only PP stated the scheduler
	// sizes the group in multiples of PP, and TP is the group over PP.
	Parallel          parallelism.Spec
	AllowedGPUIndexes []int // optional manual GPU selection
	// PerGPULimitBytes > 0 means a dGPU memory-limited (shareable) deployment;
	// 0 means whole-GPU (must land on an empty GPU).
	PerGPULimitBytes int64
	PartitionBackend string
	// UtilizationIsShare says the engine takes GPUMemUtil of the card for
	// itself (vLLM, SGLang); on a GPU shared by memory that share, not only
	// its startup need, is what it claims (GPU.Claim).
	UtilizationIsShare bool
	// Features are the platform features the deployment needs beyond running
	// its engine; a node whose vendor lacks one is filtered out.
	Features []accelerator.Feature
	// GPUMemUtil is the effective whole-GPU capacity fraction this deployment
	// runs at. It is materialized from the launch configuration before the
	// request is built, so there is no such thing as "not set": Validate
	// rejects anything outside (0,1] rather than substituting a number.
	GPUMemUtil float64
	// EvaluateCandidate applies the optional catalog placement contract to one
	// concrete GPU group. A rejection reason excludes the group; a non-zero
	// memory requirement replaces the generic estimate. The scheduler still owns
	// admission against live capacity, allocations and partition limits.
	EvaluateCandidate func([]GPU) CandidateEvaluation
}

// CandidateEvaluation is the single scheduler-facing result of an optional
// catalog placement contract. The zero value accepts with the generic estimate.
type CandidateEvaluation struct {
	GPUMemoryBytesPerDevice int64
	RejectionReason         string
	// Preference orders accepted candidates: the plan with the highest value
	// wins before any other scorer is consulted. Zero (the default) states no
	// preference, so candidates tie and the ordinary scoring decides.
	Preference int64
}

// Validate reports a request the scheduler cannot reason about. Reaching it
// means a caller bypassed launch-configuration materialization, which is a
// defect to surface rather than a value to guess at.
func (r Request) Validate() error {
	if r.GPUMemUtil <= 0 || r.GPUMemUtil > 1 {
		return fmt.Errorf("gpu_memory_utilization must be within (0,1], got %v", r.GPUMemUtil)
	}
	return nil
}

// Reason is the shared explanation unit (feeds instance_events and the topo graph).
type Reason struct {
	Code    string         `json:"code,omitempty"`
	Params  map[string]any `json:"params,omitempty"`
	Message string         `json:"message,omitempty"` // legacy persisted fallback
	Score   int            `json:"score,omitempty"`
}

// Stable reason codes are part of the server/frontend wire contract. Product
// text for these codes lives in the UI locale resources.
const (
	// ReasonRuntimeUnavailable is no longer produced — a node whose runtime
	// cannot use its cards is cordoned instead (store.Node.RuntimeGPUBlocked)
	// — but it stays defined because recorded refusals carry it.
	ReasonRuntimeUnavailable                 = "runtime_unavailable"
	ReasonRuntimeUnsupported                 = "runtime_unsupported"
	ReasonLaunchFeaturesUnknown              = "launch_features_unknown"
	ReasonRuntimeUnreported                  = "runtime_unreported"
	ReasonAcceleratorFeatureUnsupported      = "accelerator_feature_unsupported"
	ReasonAcceleratorDeviceSelectionRequired = "accelerator_device_selection_required"
	ReasonServingUnavailable                 = "serving_unavailable"
	ReasonDtypeUnsupported                   = "dtype_unsupported"
	ReasonDtypeUnsupportedDetail             = "dtype_unsupported_detail"
	ReasonPartitionUnavailable               = "partition_unavailable"
	ReasonInsufficientDisk                   = "insufficient_disk"
	// The node's vendor does not provide a feature the deployment asks for
	// ({vendor}): engine sleep, ranks on several nodes, a KV disk tier.
	ReasonSleepUnsupportedOnNode     = "sleep_unsupported_on_node"
	ReasonMultiNodeUnsupportedOnNode = "multi_node_unsupported_on_node"
	ReasonPDUnsupportedOnNode        = "pd_unsupported_on_node"
	ReasonKVDiskUnsupportedOnNode    = "kv_disk_unsupported_on_node"
	ReasonInsufficientRuntimeDisk    = "insufficient_runtime_disk"
	// No card takes work, and why, from the operator's side: the node has
	// no card of its own (none at all, or all lent to virtual machines or
	// held by another driver), its cards are held by processes outside the
	// platform, or none is at a health level that serves.
	ReasonNoOwnedGPU                = "no_owned_gpu"
	ReasonGPUsHeldOutsidePlatform   = "gpus_held_outside_platform"
	ReasonNoHealthyGPU              = "no_healthy_gpu"
	ReasonNoFreeGPU                 = "no_free_gpu"
	ReasonFiltersPassed             = "filters_passed"
	ReasonNoFit                     = "no_fit"
	ReasonSleepResidueBlocksStartup = "sleep_residue_blocks_startup"
	ReasonTPHeadCountNotDivisible   = "tp_head_count_not_divisible"
	ReasonInsufficientSharedGPUs    = "insufficient_shared_gpus"
	ReasonInsufficientEmptyGPUs     = "insufficient_empty_gpus"
	// ReasonPinnedGPUsUnavailable: other deployments ({holders}) hold the
	// GPUs an operator pinned a rank to on the node ({node}, {gpus}); the
	// deployment waits for them.
	ReasonPinnedGPUsUnavailable = "pinned_gpus_unavailable"
	ReasonTopologyNVLink        = "topology_nvlink"
	ReasonTopologyNUMA          = "topology_numa"
	ReasonTopologyCross         = "topology_cross"
	ReasonTopologySingle        = "topology_single"
	ReasonPacking               = "packing"
	ReasonTightFit              = "tight_fit"
	// ReasonPlacementPreference: the catalog's placement contract ranked this
	// candidate above the others it accepted ({preference}, {count}). Emitted
	// only when the contract stated a non-zero preference.
	ReasonPlacementPreference = "placement_preference"
	// ReasonDegradedGPUsUsed: the chosen group includes GPUs at warning level
	// ({gpus}, {count}) because the fully healthy GPUs could not take the
	// request.
	ReasonDegradedGPUsUsed     = "degraded_gpus_used"
	ReasonPlacementRejected    = "placement_rejected"
	ReasonEnginePortsExhausted = "engine_ports_exhausted"
	ReasonImageDeliveryFailed  = "image_delivery_failed"
	ReasonImageDeliveryWaiting = "image_delivery_waiting"
	// ReasonImageSourceForbidden: the pull source rules that apply to the
	// node ({node}) leave it nothing to pull the image ({image}) from.
	ReasonImageSourceForbidden = "image_source_forbidden"
	// ReasonWeightsFetchForbidden: the pull source rules that apply to the
	// node ({node}) leave nowhere to download the model ({model}) from, and
	// no peer or storage has it for the node.
	ReasonWeightsFetchForbidden = "weights_fetch_forbidden"
	ReasonInvalidRequest        = "invalid_request"
	// ReasonPoolEmpty: no node of the deployment's candidate pool can take a
	// replica now - the counts say how many nodes there are ({nodes}), how
	// many its node requirements exclude ({unmet}), how many are cordoned or
	// draining ({cordoned}) and how many are offline ({offline}).
	ReasonPoolEmpty = "pool_empty"
	// ReasonNodeCordoned: a replica that names its node ({node}) waits while
	// an operator has stopped scheduling on it.
	ReasonNodeCordoned = "node_cordoned"
	// ReasonPoolNoFit: the pool's best candidates ({tried} of {candidates})
	// were each planned and none had room; {node} and {first} are the best
	// one and why it refused.
	ReasonPoolNoFit = "pool_no_fit"
	// ReasonWaitingForYield: the replica fits on {node} once deployments
	// sharing its cards by yield policy give them up ({holders}: each one's
	// {deployment} and the terms it gives way on: {at_once}, or once idle
	// {idle_sec} or, taking turns, at the end of {max_serve_sec}); the
	// residency arbiter puts them to sleep by those terms.
	ReasonWaitingForYield = "waiting_for_yield"
	// ReasonReservedForSleepers: the replica would fit on {node}'s cards
	// {gpus} but deployments asleep there ({sleepers}: each one's {deployment}
	// and yield {policy}) keep them for a request's wake, and the replica's
	// deployment does not share cards with them (its policy is never).
	ReasonReservedForSleepers = "reserved_for_sleepers"
	// ReasonDoesNotWaitForYield: the replica would fit on {node}'s cards
	// {gpus} were the awake deployments there asleep, but its deployment
	// never yields, so it waits for none of them: {holders} would give way
	// to one that shares cards, {blockers} never yield (each one's
	// {deployment} and yield {policy}). Only the deploy form's evaluation
	// states it.
	ReasonDoesNotWaitForYield = "does_not_wait_for_yield"
	// ReasonHeldByNonYielding: the replica would fit on {node}'s cards {gpus}
	// were the awake deployments there asleep, but {blockers} do not yield to
	// its deployment (each one's {deployment} and yield {policy}); {holders}
	// would, each with its {policy} and the terms of waiting_for_yield. Only
	// the deploy form's evaluation states it.
	ReasonHeldByNonYielding = "held_by_non_yielding"
	// ReasonModelUnavailable: a model the replica reads ({model}) is stuck on
	// its node ({node}) - its acquisition failed or waits for an operator to
	// choose a source ({state}, {detail}).
	ReasonModelUnavailable = "model_unavailable"
	// ReasonStorageUnavailable: the storage the model would come from does
	// not apply to the node or is not ready there ({storage}, {reason}).
	ReasonStorageUnavailable = "storage_unavailable"
	// ReasonModelExceedsCacheLimit: the model alone is larger than the node's
	// cache cap for an nfs_cache storage.
	ReasonModelExceedsCacheLimit = "model_exceeds_cache_limit"
	// ReasonCacheFullInUse: the node's cache cap is taken by models that
	// cannot be evicted, typically ones running deployments read.
	ReasonCacheFullInUse = "cache_full_in_use"
	// ReasonModelMissingInStorage: a shared storage does not hold the model.
	ReasonModelMissingInStorage = "model_missing_in_storage"
	// ReasonStorageDisabled: the node lacks the model and the storage is
	// read-only, so nothing can be put there.
	ReasonStorageDisabled = "storage_disabled"
)

// Plan is a concrete placement.
type Plan struct {
	GPUIndexes  []int `json:"gpus"`
	TP          int   `json:"tp"`
	PerGPUBytes int64 `json:"per_gpu_bytes"`
	// ClaimBytes is what the instance takes from each of its GPUs when they
	// are shared by memory (GPU.Claim); zero when it holds them whole or under
	// a dGPU limit. The ledger charges it for as long as the instance lives.
	ClaimBytes int64 `json:"claim_bytes,omitempty"`
	Score      int   `json:"score"`
	// Preference is the catalog placement contract's ordering key for this
	// candidate (CandidateEvaluation.Preference). It is compared before Score
	// and is deliberately not part of it.
	Preference int64    `json:"preference,omitempty"`
	Reasons    []Reason `json:"reasons"`
}

// Rejected records a considered-but-discarded option.
type Rejected struct {
	GPUIndexes []int  `json:"gpus,omitempty"`
	TP         int    `json:"tp"`
	Reason     Reason `json:"reason"`
}

// TopoHighlight tells the topology graph what to emphasize for the chosen plan.
type TopoHighlight struct {
	GPUs  []int    `json:"gpus"`
	Links [][2]int `json:"links"`
	NUMA  []int    `json:"numa"`
}

// Explain is the schedule_explain structure.
type Explain struct {
	Chosen        *Plan         `json:"chosen"`
	Rejected      []Rejected    `json:"rejected"`
	TopoHighlight TopoHighlight `json:"topo_highlight"`
	Filters       []Reason      `json:"filters"`
}

// Result is the scheduling outcome.
type Result struct {
	Plan    *Plan   `json:"plan"`
	Explain Explain `json:"explain"`
	OK      bool    `json:"ok"`
	Message Reason  `json:"message"`
}

// Schedule runs the full pipeline for one node.
func Schedule(req Request, node NodeSnapshot) Result {
	var ex Explain

	if err := req.Validate(); err != nil {
		reason := Reason{Code: ReasonInvalidRequest, Message: err.Error(),
			Params: map[string]any{"gpu_memory_utilization": req.GPUMemUtil}}
		ex.Filters = append(ex.Filters, reason)
		return Result{OK: false, Message: reason, Explain: ex}
	}

	// --- Filters ---
	if ok, reason := runFilters(req, node); !ok {
		ex.Filters = append(ex.Filters, reason)
		return Result{OK: false, Message: reason, Explain: ex}
	}
	ex.Filters = append(ex.Filters, Reason{Code: ReasonFiltersPassed})

	// --- GPU planner ---
	plans, rejected := planPreferringHealthy(req, node)
	ex.Rejected = rejected
	if len(plans) == 0 {
		return Result{OK: false, Message: blockingReason(rejected), Explain: ex}
	}

	// --- Scorers ---
	best := scorePlans(plans, node)
	ex.Chosen = best
	ex.TopoHighlight = highlight(best, node)

	return Result{Plan: best, Explain: ex, OK: true}
}

// planPreferringHealthy places on fully healthy GPUs whenever they can take
// the request, and only otherwise lets degraded GPUs take part, so a degraded
// GPU is used last. A plan that ends up on degraded GPUs says so.
func planPreferringHealthy(req Request, node NodeSnapshot) ([]Plan, []Rejected) {
	preferred := node
	preferred.GPUs = nil
	for _, g := range node.GPUs {
		if !g.Degraded {
			preferred.GPUs = append(preferred.GPUs, g)
		}
	}
	if len(preferred.GPUs) == len(node.GPUs) {
		return plan(req, node)
	}
	if plans, rejected := plan(req, preferred); len(plans) > 0 {
		return plans, rejected
	}
	plans, rejected := plan(req, node)
	degraded := map[int]bool{}
	for _, g := range node.GPUs {
		degraded[g.Index] = g.Degraded
	}
	for i := range plans {
		var used []int
		for _, index := range plans[i].GPUIndexes {
			if degraded[index] {
				used = append(used, index)
			}
		}
		if len(used) > 0 {
			plans[i].Reasons = append(plans[i].Reasons, Reason{Code: ReasonDegradedGPUsUsed,
				Params: map[string]any{"gpus": used, "count": len(used)}})
		}
	}
	return plans, rejected
}

// reserveFor is the reserve a request is charged. A caller that did not compute
// one still gets the engine's conservative default rather than none: charging no
// reserve is the direction that admits a deployment the engine then cannot start.
//
// It lives here rather than at each call site so that admission and the plan's
// reported requirement cannot end up charging different amounts.
func reserveFor(req Request) int64 {
	if req.ReserveBytes > 0 {
		return req.ReserveBytes
	}
	return serverestimate.ReserveBytes(req.ReserveBase, "", true, req.GPUMemUtil)
}

// perGPUNeed computes the per-GPU memory requirement for a group of tp GPUs
// on this node. Weights are sharded within a DP replica, possibly across nodes;
// independent DP replicas each retain a complete copy.
func perGPUNeed(req Request, tp int) int64 {
	// DP duplicates a whole model; only TP × PP shards its weights.
	shards := req.Parallel.Assigned(tp).World()
	return serverestimate.RequiredPerGPUBytes(req.WeightBytes, req.MMEncoderBytes, reserveFor(req), shards)
}

// headTP is the tensor-parallel degree the attention heads are split over for
// a group of tp GPUs on this node.
func headTP(req Request, tp int) int {
	return req.Parallel.Assigned(tp).TP
}

func allowedTPs(req Request, maxGPUs int) []int {
	if req.Parallel.Stated() {
		// The caller validated the spec; one that cannot be shared by its
		// nodes leaves nothing to place.
		perNode, err := req.Parallel.GPUsPerNode()
		if err != nil || perNode < 1 {
			return nil
		}
		return []int{perNode}
	}
	if len(req.AllowedTPs) > 0 {
		if spec := req.Parallel.Normalized(); spec.DP > 1 {
			// An exact card selection must still contain whole DP copies.
			// Divide before multiplying so malformed large degrees cannot
			// overflow the group size check.
			var groups []int
			for _, count := range req.AllowedTPs {
				if count > 0 && count%spec.DP == 0 && (count/spec.DP)%spec.PP == 0 {
					groups = append(groups, count)
				}
			}
			return groups
		}
		return req.AllowedTPs
	}
	if maxGPUs <= 0 {
		maxGPUs = 8
	}
	// Every DP copy has PP stages with the same TP. No candidate can need
	// more than the node's cards.
	minimum := req.Parallel.MinimumGPUs()
	if minimum > maxGPUs {
		return nil
	}
	tps := make([]int, 0, 5)
	for tp := minimum; tp <= maxGPUs; tp *= 2 {
		tps = append(tps, tp)
	}
	return tps
}
