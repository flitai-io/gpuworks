// The package keeps only the facts that are properties of the model rather than
// of a serving engine. The KV layout used to be derived here too; it is measured
// and carried by the catalog now, because deriving it meant reimplementing the
// engines. A regression that reintroduces a derivation would show up as this
// file growing an adapter test again.
package estimate

import (
	"testing"

	"gpuworks.ai/gpuworks/pkg/modelconfig"
)

func TestDtypeSizeBytes(t *testing.T) {
	cases := map[string]float64{
		"float16":     2,
		"bfloat16":    2,
		"fp16":        2,
		"bf16":        2,
		"BF16":        2,
		"fp8":         1,
		"fp8_e4m3":    1,
		"fp8_e5m2":    1,
		"FP8_E4M3":    1,
		"float8_e4m3": 1,
		"e4m3":        1,
		"e5m2":        1,
		"float32":     4,
		"fp32":        4,
		"auto":        2,
		"":            2,
		"weird":       2,
	}
	for in, want := range cases {
		if got := DtypeSizeBytes(in); got != want {
			t.Errorf("DtypeSizeBytes(%q) = %v, want %v", in, got, want)
		}
	}
}

// qwen3_8b is a Qwen3-8B-like GQA config.
func qwen3_8b() *modelconfig.Config {
	return &modelconfig.Config{
		ModelType:             "qwen3",
		NumHiddenLayers:       36,
		HiddenSize:            4096,
		NumAttentionHeads:     32,
		NumKeyValueHeads:      8,
		HeadDim:               128,
		MaxPositionEmbeddings: 40960,
		TorchDtype:            "bfloat16",
	}
}

func TestMMEncoderRuntimeBytes(t *testing.T) {
	cfg := qwen3_8b()
	cfg.HiddenSize = 4096
	cfg.TorchDtype = "bfloat16"
	cfg.VisionConfig = &modelconfig.TowerConfig{ImageSize: 448, PatchSize: 14}
	cfg.AudioConfig = &modelconfig.TowerConfig{MaxSourcePositions: 1500}

	// vision: (ceil(448/14)^2 + cls) = 1025; audio: 1500. The
	// reserve is encoder outputs plus an equal-sized working area.
	wantMM := int64(1025+1500) * 4096 * 2 * 2
	if got := MMEncoderRuntimeBytes(cfg, "bfloat16"); got != wantMM {
		t.Fatalf("MMEncoderRuntimeBytes = %d, want %d", got, wantMM)
	}

	text := qwen3_8b()
	if got := MMEncoderRuntimeBytes(text, "bf16"); got != 0 {
		t.Fatalf("plain text MM reserve = %d, want 0", got)
	}
	// A nil config is a supported partial-estimate input and must not panic.
	if got := MMEncoderRuntimeBytes(nil, "bf16"); got != 0 {
		t.Fatalf("nil config MM reserve = %d, want 0", got)
	}
}
