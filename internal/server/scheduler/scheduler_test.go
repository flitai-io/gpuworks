package scheduler

import (
	"testing"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/pkg/topoparse"
)

const giB = int64(1) << 30

func gpu(idx int, memGiB int64, free bool, numa int) GPU {
	var alloc int64
	if !free {
		alloc = memGiB * giB
	}
	return GPU{Index: idx, MemoryTotal: memGiB * giB, AllocatedBytes: alloc, NUMANode: numa, Schedulable: true}
}

func TestScheduleSingleGPUFits(t *testing.T) {
	node := NodeSnapshot{
		UUID: "n1", DtypeSupported: true, DiskAvailBytes: 1 << 50,
		GPUs: []GPU{gpu(0, 48, true, 0), gpu(1, 48, true, 0)},
	}
	// 8B model ~16GiB weights, small KV → fits on one 48GiB GPU.
	req := Request{WeightBytes: 16 * giB, HeadCount: 32, GPUMemUtil: 0.9}
	res := Schedule(req, node)
	if !res.OK {
		t.Fatalf("expected OK, got: %+v", res.Message)
	}
	if res.Plan.TP != 1 {
		t.Errorf("expected tp=1 for a model that fits on one GPU, got tp=%d", res.Plan.TP)
	}
	if len(res.Plan.GPUIndexes) != 1 {
		t.Errorf("expected 1 GPU, got %v", res.Plan.GPUIndexes)
	}
}

func TestScheduleNeedsTP2(t *testing.T) {
	// 32B-ish model: ~64GiB weights won't fit on a 48GiB GPU at tp=1 but fits at tp=2.
	node := NodeSnapshot{
		UUID: "n1", DtypeSupported: true, DiskAvailBytes: 1 << 50,
		GPUs:     []GPU{gpu(0, 48, true, 0), gpu(1, 48, true, 0)},
		Topology: &topoparse.Topology{NVLinkCliques: [][]int{{0, 1}}},
	}
	req := Request{WeightBytes: 64 * giB, HeadCount: 40, GPUMemUtil: 0.9}
	res := Schedule(req, node)
	if !res.OK {
		t.Fatalf("expected OK, got: %+v", res.Message)
	}
	if res.Plan.TP != 2 {
		t.Errorf("expected tp=2, got tp=%d (per-gpu need %.1f GiB)", res.Plan.TP, float64(res.Plan.PerGPUBytes)/float64(giB))
	}
	// NVLink clique should be recognized in the explanation.
	foundNVLink := false
	for _, r := range res.Plan.Reasons {
		if r.Code == ReasonTopologyNVLink && r.Score == 30 {
			foundNVLink = true
		}
	}
	if !foundNVLink {
		t.Errorf("expected NVLink topology bonus in reasons: %+v", res.Plan.Reasons)
	}
	// Topo highlight should mark both GPUs and the link.
	if len(res.Explain.TopoHighlight.GPUs) != 2 || len(res.Explain.TopoHighlight.Links) != 1 {
		t.Errorf("unexpected topo highlight: %+v", res.Explain.TopoHighlight)
	}
}

func TestScheduleTPDivisibility(t *testing.T) {
	// HeadCount=6 → tp=4 invalid (6%4!=0); should be rejected, tp=2 used.
	node := NodeSnapshot{
		UUID: "n1", DtypeSupported: true, DiskAvailBytes: 1 << 50,
		GPUs: []GPU{gpu(0, 24, true, 0), gpu(1, 24, true, 0), gpu(2, 24, true, 0), gpu(3, 24, true, 0)},
	}
	req := Request{WeightBytes: 36 * giB, HeadCount: 6, AllowedTPs: []int{1, 2, 4}, GPUMemUtil: 1}
	res := Schedule(req, node)
	if !res.OK {
		t.Fatalf("expected OK, got %+v", res.Message)
	}
	if res.Plan.TP != 2 {
		t.Errorf("expected tp=2 (tp=4 invalid for 6 heads), got %d", res.Plan.TP)
	}
	// A tp=4 rejection should be recorded.
	found := false
	for _, r := range res.Explain.Rejected {
		if r.TP == 4 && r.Reason.Code == ReasonTPHeadCountNotDivisible && r.Reason.Params["tp"] == 4 && r.Reason.Params["head_count"] == 6 {
			found = true
		}
	}
	if !found {
		t.Errorf("expected tp=4 rejection in explain: %+v", res.Explain.Rejected)
	}
}

func TestScheduleHonorsGPUMemoryUtilization(t *testing.T) {
	node := NodeSnapshot{UUID: "util", DtypeSupported: true, DiskAvailBytes: 1 << 50, GPUs: []GPU{gpu(0, 16, true, 0)}}
	res := Schedule(Request{WeightBytes: 13 * giB, HeadCount: 8, AllowedTPs: []int{1}, GPUMemUtil: 0.8}, node)
	if res.OK {
		t.Fatalf("expected 13 GiB request to exceed 80%% of 16 GiB: %+v", res)
	}
}

func TestScheduleNoFit(t *testing.T) {
	node := NodeSnapshot{
		UUID: "n1", DtypeSupported: true, DiskAvailBytes: 1 << 50,
		GPUs: []GPU{gpu(0, 24, true, 0)},
	}
	// 200GiB weights on a single 24GiB GPU → no fit.
	req := Request{WeightBytes: 200 * giB, HeadCount: 32, AllowedTPs: []int{1}, GPUMemUtil: 0.9}
	res := Schedule(req, node)
	if res.OK {
		t.Fatalf("expected no fit, got plan %+v", res.Plan)
	}
	if len(res.Explain.Rejected) == 0 {
		t.Error("expected rejection reasons")
	}
	if res.Message.Code != ReasonNoFit {
		t.Fatalf("message = %+v, want no_fit", res.Message)
	}
	if got := res.Message.Params["tp"]; got != 1 {
		t.Fatalf("no_fit tp = %#v, want concrete tp=1", got)
	}
	if got := res.Message.Params["gpu_count"]; got != 1 {
		t.Fatalf("no_fit gpu_count = %#v, want 1", got)
	}
	if got := res.Message.Params["available_bytes"]; got != capacityOf(24*giB) {
		t.Fatalf("no_fit available_bytes = %#v, want util-adjusted capacity", got)
	}
	if _, ok := res.Message.Params["required_bytes"]; !ok {
		t.Fatal("no_fit omitted required_bytes")
	}
}

func TestScheduleDGPUSharing(t *testing.T) {
	// One 48GiB GPU already hosting an 11GiB-limited instance (11.4 allocated).
	// A second 11GiB-limited deployment must share the SAME GPU (ledger allows).
	g := gpu(0, 48, true, 0)
	frac := 11.4
	g.AllocatedBytes = int64(frac * float64(giB))
	g.PartitionBackends = []string{"nvidia-dgpu"}
	node := NodeSnapshot{
		UUID: "n1", DtypeSupported: true, DiskAvailBytes: 1 << 50,
		GPUs: []GPU{g},
	}
	req := Request{
		WeightBytes: 15 * giB, HeadCount: 28,
		PerGPULimitBytes: 11 * giB, PartitionBackend: "nvidia-dgpu", GPUMemUtil: 0.9,
	}
	res := Schedule(req, node)
	if !res.OK {
		t.Fatalf("expected the limited deployment to share the GPU, got: %+v", res.Message)
	}
	if len(res.Plan.GPUIndexes) != 1 || res.Plan.GPUIndexes[0] != 0 {
		t.Errorf("expected placement on shared GPU 0, got %v", res.Plan.GPUIndexes)
	}

	// A whole-GPU deployment must NOT share the partially-used GPU.
	whole := Request{WeightBytes: 15 * giB, HeadCount: 28, GPUMemUtil: 0.9}
	if r := Schedule(whole, node); r.OK {
		t.Errorf("whole-GPU deployment should not land on a shared GPU, got %v", r.Plan.GPUIndexes)
	}
}

func TestScheduleRejectsDGPURequestWhenPartitionUnavailable(t *testing.T) {
	node := NodeSnapshot{
		UUID: "n1", DtypeSupported: true, DiskAvailBytes: 1 << 50,
		GPUs: []GPU{gpu(0, 48, true, 0)},
	}
	req := Request{WeightBytes: 15 * giB, HeadCount: 28, PerGPULimitBytes: 11 * giB, PartitionBackend: "nvidia-dgpu", GPUMemUtil: 0.9}
	res := Schedule(req, node)
	if res.OK {
		t.Fatalf("expected partition filter failure, got plan %+v", res.Plan)
	}
	if len(res.Explain.Filters) == 0 || res.Explain.Filters[0].Code != ReasonPartitionUnavailable || res.Message.Code != ReasonPartitionUnavailable {
		t.Fatalf("filters = %+v", res.Explain.Filters)
	}
}

func TestScheduleLimitedRequestUsesOnlyVerifiedGPU(t *testing.T) {
	unverified := gpu(0, 48, true, 0)
	verified := gpu(3, 48, true, 0)
	verified.PartitionBackends = []string{"nvidia-dgpu"}
	node := NodeSnapshot{UUID: "mixed-readiness", DtypeSupported: true, GPUs: []GPU{unverified, verified}}
	req := Request{WeightBytes: 4 * giB, PerGPULimitBytes: 8 * giB, PartitionBackend: "nvidia-dgpu", GPUMemUtil: 0.9}
	if got := Schedule(req, node); !got.OK || len(got.Plan.GPUIndexes) != 1 || got.Plan.GPUIndexes[0] != 3 {
		t.Fatalf("auto placement selected an unverified GPU: %+v", got)
	}
	req.AllowedGPUIndexes = []int{0}
	if got := Schedule(req, node); got.OK {
		t.Fatalf("manual placement accepted unverified GPU: %+v", got)
	}
}

func TestScheduleExcludesUnhealthyGPUs(t *testing.T) {
	bad := gpu(4, 32, true, 0)
	bad.Schedulable = false
	node := NodeSnapshot{
		UUID: "ascend", DtypeSupported: true, DiskAvailBytes: 1 << 50,
		GPUs: []GPU{bad, gpu(5, 32, true, 0)},
	}
	res := Schedule(Request{WeightBytes: 8 * giB, HeadCount: 8, AllowedTPs: []int{1}, GPUMemUtil: 0.9}, node)
	if !res.OK || len(res.Plan.GPUIndexes) != 1 || res.Plan.GPUIndexes[0] != 5 {
		t.Fatalf("schedule result = %+v, want healthy GPU 5", res)
	}
}

func TestScheduleRejectsNodeWithOnlyUnhealthyGPUs(t *testing.T) {
	bad := gpu(4, 32, true, 0)
	bad.Schedulable = false
	node := NodeSnapshot{UUID: "ascend", DtypeSupported: true, GPUs: []GPU{bad}}
	res := Schedule(Request{WeightBytes: giB, HeadCount: 8, GPUMemUtil: 0.9}, node)
	if res.OK || len(res.Explain.Filters) == 0 || res.Explain.Filters[0].Code != ReasonNoHealthyGPU || res.Message.Code != ReasonNoHealthyGPU {
		t.Fatalf("schedule result = %+v", res)
	}
}

// A node none of whose cards takes work is refused with what an operator
// would act on: there is no card of its own, its cards are held by a
// process outside the platform, or none is healthy enough. Calling all
// three "no healthy GPU" sent operators to a health alert that did not
// exist.
func TestANodeWithNoSchedulableCardSaysWhy(t *testing.T) {
	held := gpu(0, 32, true, 0)
	held.Schedulable, held.HeldOutsidePlatform = false, true
	faulty := gpu(1, 32, true, 0)
	faulty.Schedulable = false
	for _, tc := range []struct {
		name string
		gpus []GPU
		want string
	}{
		{"every card lent to a VM or without a driver", nil, ReasonNoOwnedGPU},
		{"cards held outside the platform", []GPU{held, faulty}, ReasonGPUsHeldOutsidePlatform},
		{"cards at a level that does not serve", []GPU{faulty}, ReasonNoHealthyGPU},
	} {
		node := NodeSnapshot{UUID: "n", DtypeSupported: true, GPUs: tc.gpus}
		res := Schedule(Request{WeightBytes: giB, HeadCount: 8, GPUMemUtil: 0.9}, node)
		if res.OK || res.Message.Code != tc.want {
			t.Errorf("%s: refusal = %q, want %q", tc.name, res.Message.Code, tc.want)
		}
	}
}

func TestScheduleConsidersTP16OnSixteenCardNode(t *testing.T) {
	gpus := make([]GPU, 16)
	for i := range gpus {
		gpus[i] = gpu(i, 96, true, 0)
	}
	node := NodeSnapshot{UUID: "ppu", DtypeSupported: true, DiskAvailBytes: 1 << 50, GPUs: gpus}
	res := Schedule(Request{WeightBytes: 1200 * giB, HeadCount: 128, GPUMemUtil: 0.9}, node)
	if !res.OK || res.Plan == nil || res.Plan.TP != 16 {
		t.Fatalf("schedule = %+v", res)
	}
}

// A request is charged the reserve it carries; a request that carries none is
// charged a conservative default rather than zero, because admitting a
// deployment against its weights alone is how one the engine cannot start gets
// accepted.
func TestScheduleChargesTheReserveTheRequestCarries(t *testing.T) {
	node := NodeSnapshot{
		UUID: "n1", DtypeSupported: true, DiskAvailBytes: 1 << 50,
		GPUs: []GPU{gpu(0, 24, true, 0)},
	}
	const carried = int64(3) << 30
	req := Request{WeightBytes: 18 * giB, HeadCount: 8, AllowedTPs: []int{1}, ReserveBytes: carried, GPUMemUtil: 0.9}
	if res := Schedule(req, node); !res.OK || res.Plan.PerGPUBytes != 18*giB+carried {
		t.Fatalf("plan = %+v, want the weights plus the carried %d-byte reserve", res.Plan, carried)
	}

	// The same weights with no reserve stated: 18 GiB fits a 24 GiB card on its
	// own (0.9 x 24 = 21.6), so a fit here would mean no reserve was charged.
	bare := Request{WeightBytes: 18 * giB, HeadCount: 8, AllowedTPs: []int{1}, GPUMemUtil: 0.9}
	if res := Schedule(bare, node); res.OK {
		t.Fatalf("a request that stated no reserve was admitted for its weights alone: %+v", res.Plan)
	}
}

// A memory-limited deployment is charged its limit plus what its partition
// backend lets the engine hold outside it. Under NVIDIA's HAMi interception the
// engine's own holdback is spent inside the limit, so the claim is the limit plus
// the CUDA context (512 MiB), not the limit plus a whole-card reserve; a backend
// nobody has measured still carries the reserve. That figure is what planning's
// ledger records against the GPU, so admission and the inventory cannot
// disagree about what a shared instance costs.
func TestScheduleChargesASharedRequestItsBackendOverhead(t *testing.T) {
	const limit, carried = int64(8) << 30, int64(2) << 30
	for _, tc := range []struct {
		backend string
		want    int64
	}{
		{"nvidia-dgpu", limit + 512<<20},
		{"ascend-dgpu", limit + carried},
	} {
		g := gpu(0, 24, true, 0)
		g.PartitionBackends = []string{tc.backend}
		node := NodeSnapshot{
			UUID: "n1", DtypeSupported: true, DiskAvailBytes: 1 << 50,
			GPUs: []GPU{g},
		}
		req := Request{PerGPULimitBytes: limit, PartitionBackend: tc.backend,
			ReserveBytes: carried, HeadCount: 8, AllowedTPs: []int{1}, GPUMemUtil: 0.9}
		res := Schedule(req, node)
		if !res.OK || res.Plan.PerGPUBytes != tc.want {
			t.Fatalf("%s: per-GPU bytes = %+v, want %d", tc.backend, res.Plan, tc.want)
		}
	}
}

// Two 8 GiB NVIDIA slices and the context each keeps outside its limit leave
// a 22 GiB card room for a third 4 GiB slice; charging each slice a whole-card
// reserve on top of its limit refused it with 5 GiB of the card unclaimed.
func TestScheduleAdmitsANVIDIASliceBesideTwoSlices(t *testing.T) {
	const slice = int64(8)<<30 + 512<<20
	g := GPU{Index: 0, MemoryTotal: 22 * giB, Schedulable: true, AllocatedBytes: 2 * slice,
		PartitionBackends: []string{"nvidia-dgpu"}}
	node := NodeSnapshot{UUID: "n1", DtypeSupported: true, DiskAvailBytes: 1 << 50, GPUs: []GPU{g}}
	req := Request{PerGPULimitBytes: 4 * giB, PartitionBackend: "nvidia-dgpu",
		ReserveBytes: 2 * giB, HeadCount: 8, AllowedTPs: []int{1}, GPUMemUtil: 0.9}
	if res := Schedule(req, node); !res.OK {
		t.Fatalf("a 4 GiB slice was refused with %d GiB unclaimed: %+v", (g.MemoryTotal-g.AllocatedBytes)>>30, res.Message)
	}
}

// A slice that does not fit anywhere is explained by the card that came
// closest. The best-connected group is tried first - GPU 0, held whole by
// another deployment - and reporting it read "0.0 GiB available" on a node
// whose other card had 4 GiB free, as if the request had been judged against
// whole cards.
func TestScheduleShortfallNamesTheRoomiestCard(t *testing.T) {
	full := gpu(0, 45, false, 0)
	half := GPU{Index: 1, MemoryTotal: 22 * giB, AllocatedBytes: 18 * giB, Schedulable: true}
	full.PartitionBackends = []string{"nvidia-dgpu"}
	half.PartitionBackends = []string{"nvidia-dgpu"}
	node := NodeSnapshot{UUID: "n1", DtypeSupported: true, DiskAvailBytes: 1 << 50, GPUs: []GPU{full, half}}
	req := Request{PerGPULimitBytes: 4 * giB, PartitionBackend: "nvidia-dgpu",
		HeadCount: 8, AllowedTPs: []int{1}, GPUMemUtil: 0.9}
	res := Schedule(req, node)
	if res.OK {
		t.Fatalf("a 4.5 GiB claim was admitted beside 4 GiB free: %+v", res.Plan)
	}
	if res.Message.Code != ReasonNoFit || res.Message.Params["available_gib"] != "4.0" {
		t.Fatalf("message = %+v, want no_fit naming the 4.0 GiB card", res.Message)
	}
	if len(res.Explain.Rejected) != 1 || len(res.Explain.Rejected[0].GPUIndexes) != 1 || res.Explain.Rejected[0].GPUIndexes[0] != 1 {
		t.Fatalf("rejected = %+v, want the half-free GPU 1", res.Explain.Rejected)
	}
}

func TestScheduleCandidateEvaluationChoosesFirstAcceptedGPUCount(t *testing.T) {
	gpus := make([]GPU, 8)
	for index := range gpus {
		gpus[index] = gpu(index, 80, true, 0)
	}
	node := NodeSnapshot{UUID: "placement", DtypeSupported: true, DiskAvailBytes: 1 << 50, GPUs: gpus}
	request := Request{WeightBytes: giB, HeadCount: 32, GPUMemUtil: 0.9, EvaluateCandidate: func(candidate []GPU) CandidateEvaluation {
		if len(candidate) < 4 || len(candidate)%4 != 0 {
			return CandidateEvaluation{RejectionReason: "GPU count must be a multiple of four"}
		}
		return CandidateEvaluation{}
	}}
	result := Schedule(request, node)
	if !result.OK || result.Plan == nil || result.Plan.TP != 4 || len(result.Plan.GPUIndexes) != 4 {
		t.Fatalf("placement-filtered schedule=%+v", result)
	}
	if len(result.Explain.Rejected) < 2 || result.Explain.Rejected[0].Reason.Code != ReasonPlacementRejected {
		t.Fatalf("placement rejections=%+v", result.Explain.Rejected)
	}
}

func TestScheduleCatalogResourceContractOverridesWeightEstimateButNotLiveCapacity(t *testing.T) {
	node := NodeSnapshot{UUID: "catalog-memory", DtypeSupported: true, DiskAvailBytes: 1 << 50}
	for index := range 4 {
		node.GPUs = append(node.GPUs, gpu(index, 32, true, 0))
	}
	// The contract is the published observed peak for this preset, and it must
	// fit inside 0.9 x the card the preset names.
	const contractBytes = 29424 << 20
	request := Request{
		WeightBytes: 144 << 30, DiskNeedBytes: 144 << 30, HeadCount: 0,
		AllowedTPs: []int{4}, GPUMemUtil: 0.9,
		EvaluateCandidate: func(candidate []GPU) CandidateEvaluation {
			return CandidateEvaluation{GPUMemoryBytesPerDevice: contractBytes}
		},
	}
	result := Schedule(request, node)
	if !result.OK || result.Plan == nil || result.Plan.PerGPUBytes != contractBytes {
		t.Fatalf("resource-contract schedule=%+v", result)
	}
	node.GPUs[0].MemoryTotal = 31 << 30
	result = Schedule(request, node)
	if result.OK {
		t.Fatalf("resource contract bypassed live capacity: %+v", result)
	}
}

func TestScheduleRejectsInsufficientRuntimeFilesystem(t *testing.T) {
	node := NodeSnapshot{
		UUID: "runtime-disk", DtypeSupported: true,
		DiskAvailBytes: 1 << 40, RuntimeDiskAvailBytes: 4 << 30,
		GPUs: []GPU{gpu(0, 80, true, 0)},
	}
	result := Schedule(Request{WeightBytes: 1 << 30, DiskNeedBytes: 1 << 30, RuntimeDiskNeedBytes: 8 << 30, GPUMemUtil: 0.9}, node)
	if result.OK || result.Message.Code != ReasonInsufficientRuntimeDisk {
		t.Fatalf("runtime disk result=%+v", result)
	}
	// Zero is unknown for old agents and must not cause a false rejection.
	node.RuntimeDiskAvailBytes = 0
	result = Schedule(Request{WeightBytes: 1 << 30, RuntimeDiskNeedBytes: 8 << 30, GPUMemUtil: 0.9}, node)
	if !result.OK {
		t.Fatalf("unknown runtime disk rejected: %+v", result)
	}
}

func TestScheduleWithoutCatalogResourceContractKeepsGenericEstimator(t *testing.T) {
	node := NodeSnapshot{UUID: "generic-memory", DtypeSupported: true, DiskAvailBytes: 1 << 50}
	for index := range 4 {
		node.GPUs = append(node.GPUs, gpu(index, 32, true, 0))
	}
	result := Schedule(Request{WeightBytes: 144 << 30, AllowedTPs: []int{4}, GPUMemUtil: 0.9}, node)
	if result.OK {
		t.Fatalf("ordinary template unexpectedly bypassed generic estimate: %+v", result)
	}
}

// A card whose only occupants are asleep is not claimed: a whole-GPU request may
// take it when the engine's utilization reservation fits in the free memory left
// after the sleeper residue.
func TestScheduleWholeGPURequestLandsOnCardWithSleepingResidue(t *testing.T) {
	card := gpu(0, 24, true, 0)
	card.SleepingBytes = 3 * giB / 2
	node := NodeSnapshot{UUID: "sleepers", DtypeSupported: true, DiskAvailBytes: 1 << 50, GPUs: []GPU{card}}

	fits := Schedule(Request{WeightBytes: 14 * giB, HeadCount: 8, AllowedTPs: []int{1}, GPUMemUtil: 0.9}, node)
	if !fits.OK || fits.Plan.GPUIndexes[0] != 0 {
		t.Fatalf("whole-GPU request rejected by a card that only hosts sleepers: %+v", fits)
	}

	// 24 × 0.95 = 22.8 GiB is the engine startup reservation. Although the
	// request's 22.6 GiB requirement fits that capacity, the 1.5 GiB residue
	// leaves only 22.5 GiB free, so this card cannot start the engine.
	req := Request{WeightBytes: 216 * giB / 10, ReserveBytes: giB, HeadCount: 8, AllowedTPs: []int{1}, GPUMemUtil: 0.95}
	res := Schedule(req, node)
	if res.OK {
		t.Fatalf("request exceeding the card less its sleeping residue was admitted: %+v", res.Plan)
	}
	if res.Message.Code != ReasonSleepResidueBlocksStartup {
		t.Fatalf("message = %+v, want sleep_residue_blocks_startup", res.Message)
	}
	util := 0.95
	if got := res.Message.Params["startup_bytes"]; got != int64(float64(24*giB)*util) {
		t.Fatalf("residue reason startup_bytes = %#v, want utilization reservation", got)
	}
	if got := res.Message.Params["available_bytes"]; got != 24*giB-3*giB/2 {
		t.Fatalf("residue reason available_bytes = %#v, want free memory after residue", got)
	}
	if got := res.Message.Params["sleeping_gib"]; got != "1.5" {
		t.Fatalf("residue reason sleeping_gib = %#v, want 1.5", got)
	}
	if _, ok := Schedule(Request{WeightBytes: 200 * giB, HeadCount: 8, AllowedTPs: []int{1}, GPUMemUtil: 0.9}, NodeSnapshot{UUID: "clean", DtypeSupported: true, DiskAvailBytes: 1 << 50, GPUs: []GPU{gpu(0, 24, true, 0)}}).Message.Params["sleeping_gib"]; ok {
		t.Fatal("no_fit on a card without sleepers named a residue")
	}
}

// A shared request is offered the card's unclaimed memory, which sleepers'
// residue reduces like any other occupant.
func TestScheduleSharedRequestChargesSleepingResidue(t *testing.T) {
	card := gpu(0, 16, true, 0)
	card.SleepingBytes = 3 * giB / 2
	card.PartitionBackends = []string{"hami"}
	limit := 10 * giB
	withResidue := NodeSnapshot{UUID: "shared-sleepers", DtypeSupported: true, DiskAvailBytes: 1 << 50, GPUs: []GPU{card}}
	withoutResidue := withResidue
	withoutResidue.GPUs = []GPU{{Index: 0, MemoryTotal: 16 * giB, Schedulable: true, PartitionBackends: []string{"hami"}}}
	req := Request{WeightBytes: 4 * giB, ReserveBytes: 0, HeadCount: 8, PerGPULimitBytes: limit, PartitionBackend: "hami", GPUMemUtil: 0.9}
	if res := Schedule(req, withoutResidue); !res.OK {
		t.Fatalf("small shared request without residue was rejected: %+v", res.Message)
	}
	res := Schedule(req, withResidue)
	if res.OK {
		t.Fatalf("shared request blocked by residue was admitted: %+v", res.Plan)
	}
	if got := card.Available(); got != 16*giB-3*giB/2 {
		t.Fatalf("Available = %d, want total less residue", got)
	}
}

func TestFiltersRejectANodeThatCannotServe(t *testing.T) {
	node := NodeSnapshot{UUID: "n", DtypeSupported: true, GPUs: []GPU{{Index: 0, MemoryTotal: 80 << 30, Schedulable: true}}}
	if ok, reason := runFilters(Request{}, node); !ok {
		t.Fatalf("a serving node was filtered: %+v", reason)
	}
	node.ServingUnavailable = true
	if ok, reason := runFilters(Request{}, node); ok || reason.Code != ReasonServingUnavailable {
		t.Fatalf("ok=%v reason=%+v", ok, reason)
	}
}

// A node is filtered out when its vendor lacks a platform feature the
// deployment asks for; asking for none, or running on a vendor that has them,
// changes nothing.
func TestFiltersRejectANodeWithoutAFeatureTheDeploymentNeeds(t *testing.T) {
	mac := NodeSnapshot{UUID: "mac", Vendor: "apple", DtypeSupported: true, GPUs: []GPU{{Index: 0, MemoryTotal: 48 << 30, Schedulable: true}}}
	if ok, reason := runFilters(Request{}, mac); !ok {
		t.Fatalf("a plain deployment was filtered off a Mac: %+v", reason)
	}
	for feature, code := range map[accelerator.Feature]string{
		accelerator.FeatureEngineSleep: ReasonSleepUnsupportedOnNode,
		accelerator.FeatureMultiNode:   ReasonMultiNodeUnsupportedOnNode,
		accelerator.FeaturePD:          ReasonPDUnsupportedOnNode,
		accelerator.FeatureKVDiskTier:  ReasonKVDiskUnsupportedOnNode,
		// No vendor row denies an external KV cache (the backend's own
		// vendor list does); Apple denies every feature.
		accelerator.FeatureExternalKVCache: ReasonKVBackendUnsupportedOnNode,
	} {
		ok, reason := runFilters(Request{Features: []accelerator.Feature{feature}}, mac)
		if ok || reason.Code != code || reason.Params["vendor"] != "Apple" || reason.Params["feature"] != string(feature) {
			t.Errorf("%s on a Mac: ok=%v reason=%+v", feature, ok, reason)
		}
		for _, vendor := range []string{"nvidia", "ascend"} {
			node := mac
			node.Vendor = vendor
			if ok, reason := runFilters(Request{Features: []accelerator.Feature{feature}}, node); !ok {
				t.Errorf("%s on %s was filtered: %+v", feature, vendor, reason)
			}
		}
	}
	// The reason is the first feature asked for, in order.
	_, reason := runFilters(Request{Features: []accelerator.Feature{accelerator.FeatureKVDiskTier, accelerator.FeatureEngineSleep}}, mac)
	if reason.Code != ReasonKVDiskUnsupportedOnNode {
		t.Fatalf("reason = %+v", reason)
	}
}

// A Mac's GPU is shared by memory: it need not be empty, and a request is
// admitted against what is unclaimed by the share its engine will take.
func TestGPUSharedByMemoryAdmitsByTheShareEachEngineTakes(t *testing.T) {
	mac := GPU{Index: 0, MemoryTotal: 96 * giB, Schedulable: true, SharedByMemory: true}
	total := float64(mac.MemoryTotal)
	vllm := Request{GPUMemUtil: 0.4, UtilizationIsShare: true}
	callable := Request{GPUMemUtil: 0.9}

	// An engine that sizes itself by utilization claims that share, not just
	// its startup need; one that does not claims its need.
	if got := mac.Claim(vllm, 20*giB); got != int64(total*0.4) {
		t.Fatalf("vLLM claim = %d", got)
	}
	if got := mac.Claim(vllm, 50*giB); got != 50*giB {
		t.Fatalf("a need above the share is the claim, got %d", got)
	}
	if got := mac.Claim(callable, 3*giB); got != 3*giB {
		t.Fatalf("callable claim = %d", got)
	}
	if got := mac.Claim(vllm, 0); got != 0 {
		t.Fatalf("structural eligibility claims nothing, got %d", got)
	}

	// Busy, but with room: admitted; not with too little room.
	mac.AllocatedBytes = int64(total * 0.4)
	if !mac.UsableFor(vllm, 20*giB) {
		t.Fatal("a second 0.4 engine does not fit beside the first")
	}
	mac.AllocatedBytes = int64(total * 0.8)
	if mac.UsableFor(vllm, 20*giB) {
		t.Fatal("a third 0.4 engine was let in")
	}
	if !mac.UsableFor(callable, 3*giB) {
		t.Fatal("a small callable server does not fit in what is left")
	}

	// An ordinary card still takes whole-GPU requests only when empty.
	card := GPU{Index: 0, MemoryTotal: 80 * giB, Schedulable: true, AllocatedBytes: 1}
	if card.UsableFor(vllm, 1*giB) || card.Claim(vllm, 1*giB) != 1*giB {
		t.Fatal("an ordinary busy card took a whole-GPU request")
	}
}

func TestSchedulePlansAClaimOnAGPUSharedByMemory(t *testing.T) {
	node := NodeSnapshot{UUID: "mac", Vendor: "apple", DtypeSupported: true,
		GPUs: []GPU{{Index: 0, MemoryTotal: 96 * giB, Schedulable: true, SharedByMemory: true, AllocatedBytes: 30 * giB}}}
	req := Request{WeightBytes: 16 * giB, ReserveBytes: 2 * giB, GPUMemUtil: 0.5, UtilizationIsShare: true}
	result := Schedule(req, node)
	if !result.OK || result.Plan == nil {
		t.Fatalf("Schedule = %+v", result)
	}
	if result.Plan.ClaimBytes != 48*giB {
		t.Fatalf("ClaimBytes = %d, want the 0.5 share %d", result.Plan.ClaimBytes, 48*giB)
	}
	node.GPUs[0].AllocatedBytes = 60 * giB
	if result := Schedule(req, node); result.OK {
		t.Fatalf("a 48 GiB claim was admitted with 36 GiB left: %+v", result.Plan)
	}
	// Off a shared GPU nothing is claimed beyond the whole card.
	plain := NodeSnapshot{UUID: "n", DtypeSupported: true, GPUs: []GPU{{Index: 0, MemoryTotal: 96 * giB, Schedulable: true}}}
	if result := Schedule(req, plain); !result.OK || result.Plan.ClaimBytes != 0 {
		t.Fatalf("plain card plan = %+v", result.Plan)
	}
}
