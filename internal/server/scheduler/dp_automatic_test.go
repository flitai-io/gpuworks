package scheduler

import (
	"testing"

	"gpuworks.ai/gpuworks/internal/server/parallelism"
)

func TestAutomaticTPKeepsDataParallelWeightsPerReplica(t *testing.T) {
	node := NodeSnapshot{UUID: "dp", DtypeSupported: true, DiskAvailBytes: 1 << 50,
		GPUs: []GPU{gpu(0, 48, true, 0), gpu(1, 48, true, 0), gpu(2, 48, true, 0), gpu(3, 48, true, 0)}}
	// Each 64 GiB replica needs two cards. DP2 therefore needs four, even
	// though two cards would have enough memory for a single replica.
	req := Request{WeightBytes: 64 * giB, HeadCount: 32, GPUMemUtil: 0.9, Parallel: parallelism.Spec{DP: 2}}
	result := Schedule(req, node)
	if !result.OK || len(result.Plan.GPUIndexes) != 4 {
		t.Fatalf("automatic TP with DP2: %+v, want four cards", result)
	}
	node.GPUs = node.GPUs[:2]
	if result := Schedule(req, node); result.OK {
		t.Fatal("two DP replicas were admitted using only one replica's weight memory")
	}
}

func TestAutomaticTPHeadDivisibilityExcludesDataParallelCopies(t *testing.T) {
	node := NodeSnapshot{UUID: "dp", DtypeSupported: true, DiskAvailBytes: 1 << 50,
		GPUs: []GPU{gpu(0, 48, true, 0), gpu(1, 48, true, 0)}}
	// Three heads fit TP1 in each independent replica; DP2 does not split
	// those heads across the two cards.
	result := Schedule(Request{WeightBytes: 2 * giB, HeadCount: 3, GPUMemUtil: 0.9, Parallel: parallelism.Spec{DP: 2}}, node)
	if !result.OK || len(result.Plan.GPUIndexes) != 2 {
		t.Fatalf("three heads over TP1 DP2: %+v", result)
	}
	if got := result.Plan.Reasons[0].Params["tp"]; got != 1 {
		t.Fatalf("reported TP = %v, want 1", got)
	}
}

func TestAutomaticDPRejectsIncompleteExplicitGroups(t *testing.T) {
	node := NodeSnapshot{UUID: "dp", DtypeSupported: true, DiskAvailBytes: 1 << 50,
		GPUs: []GPU{gpu(0, 48, true, 0), gpu(1, 48, true, 0)}}
	for _, spec := range []parallelism.Spec{{DP: 4}, {PP: 2, DP: 2}} {
		result := Schedule(Request{WeightBytes: 2 * giB, GPUMemUtil: 0.9, Parallel: spec, AllowedTPs: []int{2}}, node)
		if result.OK {
			t.Fatalf("incomplete DP group accepted: %+v", spec)
		}
	}
}

func TestAutomaticTPStartsAtMinimumParallelismGroup(t *testing.T) {
	got := allowedTPs(Request{Parallel: parallelism.Spec{PP: 2, DP: 2}}, 8)
	if len(got) != 2 || got[0] != 4 || got[1] != 8 {
		t.Fatalf("PP2 DP2 candidates = %v, want [4 8]", got)
	}
	if got := allowedTPs(Request{Parallel: parallelism.Spec{DP: 4}}, 3); len(got) != 0 {
		t.Fatalf("DP4 on three cards produced candidates %v", got)
	}
}
