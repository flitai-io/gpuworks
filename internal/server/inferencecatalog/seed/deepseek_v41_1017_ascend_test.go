package seed

import (
	"context"
	"slices"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/placement"
)

func TestDeepSeekV411017AscendW8A8Contract(t *testing.T) {
	_, spec := catalogSpec(t, "deepseek-ai/deepseek-v4.1-flash-1017", "int8-vllm-ascend-a3-tp4-dp4")
	const runner = "gpuworks/gpuworks-runner:cann9.2-a3-vllm0.30.0rc2-r1"
	if spec.Image != runner {
		t.Errorf("image = %q", spec.Image)
	}
	wantSources := []string{"quay.nju.edu.cn/" + runner}
	if !slices.Equal(spec.ImageSources, wantSources) {
		t.Errorf("image sources = %v, want %v", spec.ImageSources, wantSources)
	}
	if spec.Weights.Quantization != "int8" || spec.WeightBytes != 781477421602 || len(spec.Weights.Sources) != 1 {
		t.Fatalf("weight contract = %+v, bytes = %d", spec.Weights, spec.WeightBytes)
	}
	source := spec.Weights.Sources[0]
	if source.Provider != "modelscope" || source.RepoID != "Eco-Tech/DeepSeek-V4.1-Flash-1017-w8a8" || source.Revision != "12b85455a58c7df345c1abc43934624a76bac576" || source.Subfolder != "" {
		t.Errorf("weight source = %+v", source)
	}
	if spec.Env["VLLM_USE_V2_MODEL_RUNNER"] != "0" {
		t.Errorf("V2 model runner = %q, want 0", spec.Env["VLLM_USE_V2_MODEL_RUNNER"])
	}
	for _, pair := range [][2]string{{"--tensor-parallel-size", "4"}, {"--data-parallel-size", "4"}, {"--quantization", "ascend"}} {
		if !containsArgumentPair(spec.Args, pair[0], pair[1]) {
			t.Errorf("missing %s %s", pair[0], pair[1])
		}
	}

	if spec.Placement == nil {
		t.Fatal("missing placement resolver")
	}
	for _, tc := range []struct {
		count  int
		memory float64
		accept bool
	}{{16, 64, true}, {15, 64, false}, {17, 64, false}, {16, 63, false}} {
		names, memories := make([]string, tc.count), make([]float64, tc.count)
		for i := range names {
			names[i], memories[i] = "910C", tc.memory
		}
		got, err := placement.Resolve(context.Background(), spec.Placement.Resolver, placement.GPU{
			Count: tc.count, Vendor: "ascend", Names: names, MemoriesGiB: memories, MinMemoryGiB: tc.memory,
		})
		if err != nil || (got.Reason == "") != tc.accept {
			t.Errorf("placement count=%d memory=%g: %+v, err=%v", tc.count, tc.memory, got, err)
		}
	}
}
