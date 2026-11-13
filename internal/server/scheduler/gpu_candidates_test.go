package scheduler

import (
	"fmt"
	"reflect"
	"testing"

	"gpuworks.ai/gpuworks/internal/accelerator"
)

func TestGPUCandidatesResourceCombinations(t *testing.T) {
	const gib int64 = 1 << 30
	for _, backend := range []string{accelerator.BackendNVIDIALogical, accelerator.BackendAscendLogical} {
		for _, shared := range []bool{false, true} {
			for _, occupied := range []int64{0, 32 * gib, 48 * gib, 64 * gib} {
				for _, available := range []bool{false, true} {
					name := backend + "/whole"
					if shared {
						name = backend + "/shared"
					}
					t.Run(fmt.Sprintf("%s/allocated-%d/schedulable-%v", name, occupied/gib, available), func(t *testing.T) {
						card := GPU{Index: 7, MemoryTotal: 64 * gib, AllocatedBytes: occupied, Schedulable: available, PartitionBackends: []string{backend}}
						req := Request{GPUMemUtil: 0.9, ReserveBytes: 2 * gib}
						if shared {
							req.PerGPULimitBytes, req.PartitionBackend = 16*gib, backend
						}
						want := available && occupied == 0
						if shared {
							want = available && occupied <= 32*gib
						}
						choices := GPUCandidates(req, []GPU{card})
						if len(choices) != 1 || choices[0].Index != 7 || choices[0].Available != want || (choices[0].Reason == nil) != want {
							t.Fatalf("shared=%v occupied=%d available=%v: %+v, want selectable=%v", shared, occupied, available, choices, want)
						}
					})
				}
			}
		}
	}
}

func TestGPUCandidatesBoundariesAndCapabilities(t *testing.T) {
	const gib int64 = 1 << 30
	base := GPU{Index: 0, MemoryTotal: 64 * gib, Schedulable: true, PartitionBackends: []string{accelerator.BackendNVIDIALogical}}
	whole := Request{GPUMemUtil: 0.9}
	shared := Request{GPUMemUtil: 0.9, PerGPULimitBytes: 16 * gib, PartitionBackend: accelerator.BackendNVIDIALogical}
	nativeShare := Request{GPUMemUtil: 0.9, ReserveBytes: 2 * gib, UtilizationIsShare: true}
	for _, tc := range []struct {
		name   string
		change func(*GPU)
		req    Request
		want   bool
		code   string
	}{
		{"exact claim", func(g *GPU) { g.AllocatedBytes = 47*gib + gib/2 }, shared, true, ""},
		{"whole allocation", func(g *GPU) { g.AllocatedBytes = g.MemoryTotal; g.WholeGPUAllocated = true }, whole, false, ReasonGPUWholeAllocated},
		{"whole allocation for dGPU", func(g *GPU) { g.AllocatedBytes = g.MemoryTotal; g.WholeGPUAllocated = true }, shared, false, ReasonNoFit},
		{"shared full capacity for whole request", func(g *GPU) { g.AllocatedBytes = g.MemoryTotal }, whole, false, ReasonNoFreeGPU},
		{"stopped whole allocation", func(g *GPU) { g.AllocatedBytes = g.MemoryTotal; g.WholeGPUAllocated = true; g.Schedulable = false }, whole, false, ReasonGPUNotSchedulable},
		{"externally held whole allocation", func(g *GPU) {
			g.AllocatedBytes = g.MemoryTotal
			g.WholeGPUAllocated = true
			g.Schedulable = false
			g.HeldOutsidePlatform = true
		}, whole, false, ReasonGPUsHeldOutsidePlatform},
		{"one byte short", func(g *GPU) { g.AllocatedBytes = 47*gib + gib/2 + 1 }, shared, false, ReasonNoFit},
		{"unsupported backend", func(g *GPU) { g.PartitionBackends = nil }, shared, false, ReasonPartitionUnavailable},
		{"external use", func(g *GPU) { g.Schedulable = false; g.HeldOutsidePlatform = true }, shared, false, ReasonGPUsHeldOutsidePlatform},
		{"operator override", func(g *GPU) { g.HeldOutsidePlatform = true }, shared, true, ""},
		{"unhealthy or stopped", func(g *GPU) { g.Schedulable = false }, whole, false, ReasonGPUNotSchedulable},
		{"no device index", func(g *GPU) { g.Index = -1 }, whole, false, ReasonGPUIndexUnavailable},
		{"warning remains usable", func(g *GPU) { g.Degraded = true }, whole, true, ""},
		{"sleep residue fits startup", func(g *GPU) { g.MemoryTotal = 24 * gib; g.SleepingBytes = 3 * gib / 2 }, whole, true, ""},
		{"sleep residue blocks startup", func(g *GPU) { g.MemoryTotal = 24 * gib; g.SleepingBytes = 5 * gib / 2 }, whole, false, ReasonSleepResidueBlocksStartup},
		{"shared residue consumes room", func(g *GPU) { g.AllocatedBytes = 46 * gib; g.SleepingBytes = 2 * gib }, shared, false, ReasonNoFit},
		{"native memory sharing", func(g *GPU) { g.SharedByMemory = true; g.AllocatedBytes = 32 * gib }, whole, true, ""},
		{"native utilization share fits", func(g *GPU) { g.SharedByMemory = true; g.AllocatedBytes = 4 * gib }, nativeShare, true, ""},
		{"native utilization share exceeds remaining capacity", func(g *GPU) { g.SharedByMemory = true; g.AllocatedBytes = 32 * gib }, nativeShare, false, ReasonNoFit},
		{"invalid request", func(g *GPU) {}, Request{}, false, ReasonInvalidRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			card := base
			tc.change(&card)
			indexes := GPUCandidates(tc.req, []GPU{card})
			code := ""
			if len(indexes) == 1 && indexes[0].Reason != nil {
				code = indexes[0].Reason.Code
			}
			if len(indexes) != 1 || indexes[0].Available != tc.want || (indexes[0].Reason == nil) != tc.want || code != tc.code {
				t.Fatalf("indexes=%v want selectable=%v", indexes, tc.want)
			}
		})
	}
}

func TestGPUCandidatesIgnoreSelectionAndDoNotMutateRequest(t *testing.T) {
	req := Request{GPUMemUtil: 0.9, AllowedGPUIndexes: []int{1}}
	node := NodeSnapshot{GPUs: []GPU{{Index: 1, MemoryTotal: 64 << 30, Schedulable: true}, {Index: 5, MemoryTotal: 64 << 30, Schedulable: true}}}
	choices := GPUCandidates(req, node.GPUs)
	if len(choices) != 2 || choices[0].Index != 1 || choices[1].Index != 5 || !choices[0].Available || !choices[1].Available || !reflect.DeepEqual(req.AllowedGPUIndexes, []int{1}) {
		t.Fatalf("choices=%+v request=%+v", choices, req)
	}
	if empty := GPUCandidates(req, nil); empty == nil || len(empty) != 0 {
		t.Fatalf("empty=%+v", empty)
	}
}

func TestGPUCandidatesDoNotRequireTheModelToFitOnEachCard(t *testing.T) {
	req := Request{WeightBytes: 96 * giB, ReserveBytes: 2 * giB, HeadCount: 32, GPUMemUtil: 0.9}
	node := NodeSnapshot{UUID: "large-model", DtypeSupported: true, GPUs: []GPU{gpu(0, 64, true, 0), gpu(1, 64, true, 0)}}
	for _, tp := range []int{1, 2} {
		req.AllowedTPs = []int{tp}
		candidates := GPUCandidates(req, node.GPUs)
		if len(candidates) != 2 || !candidates[0].Available || !candidates[1].Available {
			t.Fatalf("model size removed resource candidates: %+v", candidates)
		}
		result := Schedule(req, node)
		if result.OK != (tp == 2) {
			t.Fatalf("tp=%d result=%+v", tp, result)
		}
		if result.OK && !reflect.DeepEqual(result.Plan.GPUIndexes, []int{0, 1}) {
			t.Fatalf("plan=%+v", result.Plan)
		}
	}
}
