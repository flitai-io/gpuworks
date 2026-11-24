package scheduler

import (
	"testing"

	"gpuworks.ai/gpuworks/internal/accelerator"
)

// The explanation must describe the check that actually failed. These cases pin
// the failures that used to be reported as a memory shortfall regardless of their
// cause, which produced messages that contradicted themselves (a candidate whose
// requirement was below the available memory, reported as "not enough memory").

func reasonNode(gpus ...GPU) NodeSnapshot {
	return NodeSnapshot{UUID: "n1", DtypeSupported: true, DiskAvailBytes: 1 << 50, GPUs: gpus}
}

// capacityOf is the whole-GPU capacity the admission predicate offers, so a
// message assertion is stated in the same terms the verdict was decided in.
func capacityOf(totalBytes int64) int64 {
	return int64(float64(totalBytes) * 9 / 10)
}

func TestNoPlanReportsTheRejectionThatBoundIt(t *testing.T) {
	// A tensor parallel size that cannot shard the model's heads. Both the memory
	// and the card count are fine, so claiming a memory shortfall here was simply
	// wrong — and the numbers it printed proved it (need < available).
	res := Schedule(
		Request{WeightBytes: 60 * giB, HeadCount: 64, AllowedTPs: []int{5}, GPUMemUtil: 0.9},
		reasonNode(gpu(0, 80, true, 0), gpu(1, 80, true, 0), gpu(2, 80, true, 0), gpu(3, 80, true, 0), gpu(4, 80, true, 0)),
	)
	if res.OK {
		t.Fatalf("expected no plan, got %+v", res.Plan)
	}
	if res.Message.Code != ReasonTPHeadCountNotDivisible {
		t.Fatalf("message = %+v, want the divisibility rejection", res.Message)
	}
	if got := res.Message.Params["head_count"]; got != 64 {
		t.Fatalf("head_count = %#v, want 64", got)
	}
	for _, key := range []string{"required_gib", "available_gib", "available_bytes"} {
		if _, ok := res.Message.Params[key]; ok {
			t.Fatalf("a divisibility verdict must not carry a memory figure, got %q in %+v", key, res.Message.Params)
		}
	}
	// The rejected option is still explained for clients that render the whole
	// explain block.
	if len(res.Explain.Rejected) != 1 || res.Explain.Rejected[0].TP != 5 {
		t.Fatalf("explain = %+v", res.Explain.Rejected)
	}
}

func TestNoPlanPrefersTheLeastDemandingCandidate(t *testing.T) {
	// tp=1 needs the most memory per card and fails on memory; tp=2 cannot get two
	// cards at all. The verdict names the option that is easiest to satisfy,
	// because that is the one that actually has to be fixed.
	busy := gpu(1, 80, false, 0)
	res := Schedule(
		Request{WeightBytes: 200 * giB, HeadCount: 32, AllowedTPs: []int{1, 2}, GPUMemUtil: 0.9},
		reasonNode(gpu(0, 24, true, 0), busy),
	)
	if res.OK {
		t.Fatalf("expected no plan, got %+v", res.Plan)
	}
	if res.Message.Code != ReasonNoFit {
		t.Fatalf("message = %+v, want the least demanding candidate's memory verdict", res.Message)
	}
	if got := res.Message.Params["tp"]; got != 1 {
		t.Fatalf("tp = %#v, want 1", got)
	}
	if got := res.Message.Params["available_bytes"]; got != capacityOf(24*giB) {
		t.Fatalf("available_bytes = %#v, want the util-adjusted capacity of the smallest free card", got)
	}
	if got := res.Message.Params["gpu_count"]; got != 1 {
		t.Fatalf("gpu_count = %#v, want 1", got)
	}
}

func TestMemoryShortfallNamesTheCardThatLimitsIt(t *testing.T) {
	// A heterogeneous node: the group of two only needs 30 GiB per card, which the
	// 80 GiB card offers and the 24 GiB card does not. The shortfall must name the
	// smaller card — it is the reason this tp fails.
	res := Schedule(
		Request{WeightBytes: 56 * giB, HeadCount: 32, AllowedTPs: []int{2}, GPUMemUtil: 0.9},
		reasonNode(gpu(0, 80, true, 0), gpu(1, 24, true, 0)),
	)
	if res.OK {
		t.Fatalf("expected no plan, got %+v", res.Plan)
	}
	if res.Message.Code != ReasonNoFit {
		t.Fatalf("message = %+v, want no_fit", res.Message)
	}
	if got := res.Message.Params["available_bytes"]; got != capacityOf(24*giB) {
		t.Fatalf("available_bytes = %#v, want the smaller card's capacity", got)
	}
	if got, ok := res.Message.Params["required_bytes"].(int64); !ok || got <= capacityOf(24*giB) {
		t.Fatalf("required_bytes = %#v, want the requirement this candidate failed", res.Message.Params["required_bytes"])
	}
}

func TestTooFewCardsIsACountProblem(t *testing.T) {
	// Requesting four cards on a node with two free ones is a count problem: the
	// message must say how many cards qualified, not claim a memory shortfall.
	res := Schedule(
		Request{WeightBytes: 8 * giB, HeadCount: 32, AllowedTPs: []int{4}, GPUMemUtil: 0.9},
		reasonNode(gpu(0, 48, true, 0), gpu(1, 48, true, 0)),
	)
	if res.OK {
		t.Fatalf("expected no plan, got %+v", res.Plan)
	}
	if res.Message.Code != ReasonInsufficientEmptyGPUs {
		t.Fatalf("message = %+v, want insufficient_empty_gpus", res.Message)
	}
	if got := res.Message.Params["available"]; got != 2 {
		t.Fatalf("available = %#v, want the qualifying card count 2", got)
	}
	if _, ok := res.Message.Params["available_gib"]; ok {
		t.Fatalf("a count verdict must not carry a per-card figure: %+v", res.Message.Params)
	}
}

func TestBlockingReasonWithoutRejectionsKeepsTheCode(t *testing.T) {
	// Unreachable while allowedTPs yields an option, but a bare code is still
	// better than numbers nobody computed.
	reason := blockingReason(nil)
	if reason.Code != ReasonNoFit || len(reason.Params) != 0 {
		t.Fatalf("reason = %+v, want a bare no_fit", reason)
	}
}

// On a GPU shared by memory a vLLM at 0.8 is refused for its share, not its
// startup need; the message names the share, or it would read "needs 10 GiB,
// 48 GiB available".
func TestMemoryShortfallOnASharedGPUNamesTheClaim(t *testing.T) {
	mac := GPU{Index: 0, MemoryTotal: 96 * giB, Schedulable: true, SharedByMemory: true, AllocatedBytes: 48 * giB}
	req := Request{GPUMemUtil: 0.8, UtilizationIsShare: true}
	need := int64(10 * giB)
	if mac.UsableFor(req, need) {
		t.Fatal("a 0.8 engine was admitted beside half the card in use")
	}
	reason := reasonMemoryShortfall(req, []GPU{mac}, 1, need, true)
	required, _ := reason.Params["required_bytes"].(int64)
	available, _ := reason.Params["available_bytes"].(int64)
	if reason.Code != ReasonNoFit || required != mac.Claim(req, need) || required <= available {
		t.Fatalf("reason = %+v, want the claim %d above the %d available", reason, mac.Claim(req, need), available)
	}
}

// A feature without a refusal code would refuse with an empty message.
func TestEveryPlatformFeatureHasARefusal(t *testing.T) {
	for _, f := range accelerator.Features {
		if featureReason[f] == "" {
			t.Errorf("feature %q has no refusal reason", f)
		}
	}
}

func memoryHint(res Result) (bool, bool) {
	v, ok := res.Message.Params[ParamMemoryContractHint]
	hint, _ := v.(bool)
	return hint, ok
}

// A refusal that is the weight-derived estimate alone, on a card that could
// not hold it even empty, carries the hint that a template's placement
// contract may declare the real figure.
func TestMemoryShortfallOfTheEstimateCarriesTheContractHint(t *testing.T) {
	res := Schedule(
		Request{WeightBytes: 37 * giB, HeadCount: 32, AllowedTPs: []int{1}, GPUMemUtil: 0.9},
		reasonNode(gpu(0, 24, true, 0)),
	)
	if res.OK || res.Message.Code != ReasonNoFit {
		t.Fatalf("message = %+v, want a no_fit refusal", res.Message)
	}
	if hint, ok := memoryHint(res); !ok || !hint {
		t.Fatalf("params = %+v, want %s=true", res.Message.Params, ParamMemoryContractHint)
	}
}

// Nothing else about a refusal earns the hint: the estimate that would fit an
// empty card, a card count, a dtype, and a figure a contract already declared.
func TestOnlyAnEstimateTooLargeForAnEmptyCardCarriesTheContractHint(t *testing.T) {
	t.Run("fits an empty card but the card is partly used", func(t *testing.T) {
		// A shared-by-memory card with room for the estimate when empty.
		shared := GPU{Index: 0, MemoryTotal: 96 * giB, Schedulable: true, SharedByMemory: true, AllocatedBytes: 80 * giB}
		res := Schedule(
			Request{WeightBytes: 30 * giB, HeadCount: 32, AllowedTPs: []int{1}, GPUMemUtil: 0.5},
			reasonNode(shared),
		)
		if res.OK || res.Message.Code != ReasonNoFit {
			t.Fatalf("message = %+v, want a no_fit refusal", res.Message)
		}
		if _, ok := memoryHint(res); ok {
			t.Fatalf("params = %+v, a busy card is not an estimate problem", res.Message.Params)
		}
	})
	t.Run("too few cards", func(t *testing.T) {
		res := Schedule(
			Request{WeightBytes: 37 * giB, HeadCount: 32, AllowedTPs: []int{4}, GPUMemUtil: 0.9},
			reasonNode(gpu(0, 24, true, 0), gpu(1, 24, true, 0)),
		)
		if res.OK || res.Message.Code != ReasonInsufficientEmptyGPUs {
			t.Fatalf("message = %+v, want a count refusal", res.Message)
		}
		if _, ok := memoryHint(res); ok {
			t.Fatalf("params = %+v, a card count is not an estimate problem", res.Message.Params)
		}
	})
	t.Run("a memory-limited request", func(t *testing.T) {
		res := Schedule(
			Request{WeightBytes: 37 * giB, HeadCount: 32, AllowedTPs: []int{1}, GPUMemUtil: 0.9, PerGPULimitBytes: 8 * giB, PartitionBackend: "hami"},
			reasonNode(GPU{Index: 0, MemoryTotal: 24 * giB, Schedulable: true, PartitionBackends: []string{"hami"}}),
		)
		if _, ok := memoryHint(res); ok {
			t.Fatalf("params = %+v, a limit the operator set is not the estimate's", res.Message.Params)
		}
	})
	t.Run("a placement contract declared the figure", func(t *testing.T) {
		res := Schedule(
			Request{WeightBytes: 37 * giB, HeadCount: 32, AllowedTPs: []int{1}, GPUMemUtil: 0.9,
				EvaluateCandidate: func([]GPU) CandidateEvaluation { return CandidateEvaluation{GPUMemoryBytesPerDevice: 30 * giB} }},
			reasonNode(gpu(0, 24, true, 0)),
		)
		if res.OK || res.Message.Code != ReasonNoFit {
			t.Fatalf("message = %+v, want a no_fit refusal", res.Message)
		}
		if _, ok := memoryHint(res); ok {
			t.Fatalf("params = %+v, the template already declares its memory", res.Message.Params)
		}
	})
}
