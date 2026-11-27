package seed

import (
	"slices"
	"testing"
)

// The 27B gated linear-attention hybrids need one Mamba cache block per decode
// sequence under vLLM, and vLLM's default cap of 256 outnumbers the blocks a
// single small card holds after the weights: an RTX 4090 had 229 after the
// FP8 weights (GWKS-528). Each spec that fits one such card states a cap below
// what its smallest admitted card holds; a later edit that drops it brings the
// start failure back on those cards.
//
//	go test ./internal/server/inferencecatalog/seed/ -run TestSmallCardHybridSpecsCapTheirSequences
func TestSmallCardHybridSpecsCapTheirSequences(t *testing.T) {
	for _, tc := range []struct{ model, spec, want string }{
		// cc >= 8.9 admits 48 GiB cards: 229 blocks measured on a 4090.
		{"qwen/qwen3.5-27b", "fp8-vllm-nvidia-general", "128"},
		{"qwen/qwen3.6-27b", "fp8-vllm-nvidia-general", "128"},
		{"qwen/qwen3.8-27b", "fp8-vllm-nvidia-general", "128"},
		// cc >= 8.0 admits a 40 GiB A100, about 100 to 128 blocks.
		{"qwen/qwen3.5-27b", "int4-vllm-nvidia-general", "96"},
		// cc >= 10.0 admits a 32 GiB RTX 5090 for these 22 GB of weights.
		{"qwen/qwen3.6-27b", "nvfp4-vllm-nvidia-cc100", "96"},
		// 26 GB of weights plus the Blackwell reserve leave a 5090 out.
		{"qwen/qwen3.8-27b", "nvfp4-vllm-nvidia-cc100", "128"},
	} {
		t.Run(tc.model+"/"+tc.spec, func(t *testing.T) {
			_, spec := catalogSpec(t, tc.model, tc.spec)
			i := slices.Index(spec.Args, "--max-num-seqs")
			if i < 0 || i+1 >= len(spec.Args) || spec.Args[i+1] != tc.want {
				t.Fatalf("--max-num-seqs is not %s: %v", tc.want, spec.Args)
			}
		})
	}
}
