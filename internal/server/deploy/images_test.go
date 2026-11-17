package deploy

import "testing"

func TestResolveImageDefaultAndOverride(t *testing.T) {
	if got := ResolveImage(ImageTarget{Engine: "vllm", Vendor: "nvidia"}); got != "vllm/vllm-openai:latest" {
		t.Fatalf("default vllm image = %q", got)
	}
	if got := ResolveImage(ImageTarget{Engine: "vllm", Vendor: "nvidia", Override: "custom/vllm:dev"}); got != "custom/vllm:dev" {
		t.Fatalf("override image = %q", got)
	}
	if got := ResolveImage(ImageTarget{Engine: "sglang", Vendor: "nvidia"}); got != "lmsysorg/sglang:latest" {
		t.Fatalf("sglang default image = %q", got)
	}
}

func TestResolveImageVendorRules(t *testing.T) {
	tests := []struct {
		name   string
		target ImageTarget
		want   string
	}{
		{name: "ascend requires catalog", target: ImageTarget{Engine: "vllm", Vendor: "ascend"}, want: ""},
		{name: "huawei alias requires catalog", target: ImageTarget{Engine: "vllm", Vendor: "Huawei"}, want: ""},
		{name: "amd requires catalog", target: ImageTarget{Engine: "vllm", Vendor: "amd"}, want: ""},
		{name: "rocm alias requires catalog", target: ImageTarget{Engine: "vllm", Vendor: "ROCm"}, want: ""},
		{name: "hygon requires catalog", target: ImageTarget{Engine: "vllm", Vendor: "dcu"}, want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ResolveImage(tt.target); got != tt.want {
				t.Fatalf("ResolveImage(%+v) = %q, want %q", tt.target, got, tt.want)
			}
		})
	}
}

func TestPPUDoesNotFallBackToCUDAImage(t *testing.T) {
	if got := ResolveImage(ImageTarget{Engine: "vllm", Vendor: "ppu"}); got != "" {
		t.Fatalf("PPU automatic fallback = %q, want blocked empty image", got)
	}
	if got := ResolveImage(ImageTarget{Engine: "sglang", Vendor: "ppu"}); got != "" {
		t.Fatalf("PPU SGLang automatic fallback = %q, want blocked empty image", got)
	}
}

func TestResolveImageDefaultOverride(t *testing.T) {
	// The override is process-wide: restore it so later tests (the profile
	// goldens among them) see the engine's own default image.
	enginesMu.RLock()
	previous, had := defaultImages["sglang"]
	enginesMu.RUnlock()
	t.Cleanup(func() {
		enginesMu.Lock()
		defer enginesMu.Unlock()
		if had {
			defaultImages["sglang"] = previous
		} else {
			delete(defaultImages, "sglang")
		}
	})
	SetDefaultImage("sglang", "example/sglang:test")
	if got := ResolveImage(ImageTarget{Engine: "sglang", Vendor: "nvidia"}); got != "example/sglang:test" {
		t.Fatalf("default image override = %q", got)
	}
}
