package seed

import (
	"context"
	"fmt"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/placement"
)

// The 2-GPU Turbo profile serves any Ampere-or-newer pair with at least 24 GB
// per card, keeping as many DiT blocks resident as the card holds: the RTX 4090
// recipe's 12 on 24 GB cards, the RTX 5090 recipe's 20 from 32 GB up.
func TestMiniMaxH3TwoGPUTurboSizesResidentLayersByCardMemory(t *testing.T) {
	_, spec := catalogSpec(t, "minimaxai/minimax-h3", "bf16-vllm-nvidia-2x-turbo4")
	if spec.GPUs.CC != ">=8.0" || spec.GPUs.Name != "" {
		t.Fatalf("2-GPU Turbo is bound to cc %q name %q, want every Ampere-or-newer card", spec.GPUs.CC, spec.GPUs.Name)
	}
	if !containsArgumentPair(spec.Args, "--dlo-resident-layers", "${DLO_RESIDENT_LAYERS}") {
		t.Fatalf("resident layers are not taken from placement: %v", spec.Args)
	}
	for _, tc := range []struct {
		count     int
		memoryGiB float64
		reject    bool
		resident  string
		mib       int64
	}{
		{count: 2, memoryGiB: 23.6, resident: "12", mib: 22528}, // RTX 3090 / 4090
		{count: 2, memoryGiB: 31.4, resident: "20", mib: 30720}, // RTX 5090
		{count: 2, memoryGiB: 79.1, resident: "20", mib: 30720}, // A100 80 GB
		{count: 2, memoryGiB: 15.6, reject: true},               // 16 GB cards
		{count: 1, memoryGiB: 31.4, reject: true},
	} {
		result, err := placement.Resolve(context.Background(), spec.Placement.Resolver, placement.GPU{Count: tc.count, Vendor: "nvidia", MinMemoryGiB: tc.memoryGiB})
		if err != nil {
			t.Fatalf("%d x %.1f GiB: %v", tc.count, tc.memoryGiB, err)
		}
		if tc.reject {
			if result.Reason == "" {
				t.Errorf("%d x %.1f GiB accepted, want a rejection", tc.count, tc.memoryGiB)
			}
			continue
		}
		if result.Reason != "" || fmt.Sprint(result.Variables["DLO_RESIDENT_LAYERS"]) != tc.resident || result.GPUMemoryMiBPerDevice != tc.mib {
			t.Errorf("%d x %.1f GiB = %+v, want %s resident layers at %d MiB", tc.count, tc.memoryGiB, result, tc.resident, tc.mib)
		}
	}
}

// The single-GPU Turbo profile needs a 32 GB card; FastH3 Omni NVFP4 needs
// Blackwell for its FP4 kernels.
func TestMiniMaxH3SingleGPUProfilesGateOnMemoryAndArchitecture(t *testing.T) {
	_, turbo := catalogSpec(t, "minimaxai/minimax-h3", "bf16-vllm-nvidia-1x-turbo4")
	if turbo.GPUs.CC != ">=8.0" || turbo.GPUs.Name != "" {
		t.Fatalf("1-GPU Turbo is bound to cc %q name %q", turbo.GPUs.CC, turbo.GPUs.Name)
	}
	for memory, accept := range map[float64]bool{23.6: false, 31.4: true, 79.1: true} {
		result, err := placement.Resolve(context.Background(), turbo.Placement.Resolver, placement.GPU{Count: 1, Vendor: "nvidia", MinMemoryGiB: memory})
		if err != nil || (result.Reason == "") != accept {
			t.Errorf("1-GPU Turbo on %.1f GiB: accepted=%v (%v), want %v", memory, result.Reason == "", err, accept)
		}
	}
	_, fastH3 := catalogSpec(t, "minimaxai/minimax-h3", "nvfp4-vllm-nvidia-blackwell-fasth3-omni-1x")
	if fastH3.GPUs.CC != ">=10.0" {
		t.Fatalf("FastH3 NVFP4 cc = %q, want Blackwell (>=10.0)", fastH3.GPUs.CC)
	}
}
