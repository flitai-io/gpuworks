package deploy

import (
	"testing"
	"time"
)

func TestResolveImageCatalogPrecedenceAndCompatibility(t *testing.T) {
	candidates := []ImageCandidate{
		{Engine: "vllm", Framework: "cuda", Arch: "multi", ImageRef: "catalog/generic:latest", Source: "official", Enabled: true},
		{Engine: "vllm", Framework: "cuda", FrameworkVersion: "12.1", Arch: "amd64", ImageRef: "catalog/cuda:12.1", Source: "official", Enabled: true},
		{Engine: "vllm", Framework: "cuda", FrameworkVersion: "12.4", Arch: "amd64", ImageRef: "catalog/cuda:12.4", Source: "official", Enabled: true},
		{Engine: "vllm", Framework: "cuda", FrameworkVersion: "13.0", Arch: "amd64", ImageRef: "catalog/cuda:13", Source: "official", Enabled: true},
		{Engine: "vllm", Framework: "cann", FrameworkVersion: "8.0", Arch: "arm64", ImageRef: "catalog/ascend:8", Source: "official", Enabled: true},
		{Engine: "vllm", Framework: "cann", FrameworkVersion: "8.0", Arch: "arm64", ImageRef: "catalog/ascend-a3:8", Source: "official", Enabled: true},
		{Engine: "vllm", Framework: "cuda", FrameworkVersion: "12.5", Arch: "amd64", ImageRef: "catalog/deprecated", Source: "official", Enabled: true, Deprecated: true},
	}
	tests := []struct {
		name     string
		target   ImageTarget
		want     string
		warning  bool
		blocking bool
	}{
		{name: "latest driver-compatible framework", target: ImageTarget{Engine: "vllm", Vendor: "nvidia", Framework: "cuda", FrameworkVersion: "12.5", DriverVersion: "550.54.15", PlatformArch: "amd64"}, want: "catalog/cuda:12.4"},
		{name: "latest framework when driver is new", target: ImageTarget{Engine: "vllm", Vendor: "nvidia", Framework: "cuda", FrameworkVersion: "13.1", DriverVersion: "580.00", PlatformArch: "amd64"}, want: "catalog/cuda:13"},
		{name: "exact domestic framework match", target: ImageTarget{Engine: "vllm", Vendor: "ascend", Framework: "cann", FrameworkVersion: "8.0", DriverVersion: "npu-smi 26.0.rc1", PlatformArch: "arm64"}, want: "catalog/ascend:8"},
		{name: "deprecated ignored", target: ImageTarget{Engine: "vllm", Vendor: "nvidia", Framework: "cuda", FrameworkVersion: "12.5", DriverVersion: "550.54.15", PlatformArch: "amd64"}, want: "catalog/cuda:12.4"},
		{name: "manual incompatible architecture is blocked", target: ImageTarget{Engine: "vllm", Vendor: "ascend", Framework: "cann", PlatformArch: "arm64", Override: "catalog/cuda:12.4"}, want: "catalog/cuda:12.4", warning: true, blocking: true},
		{name: "manual engine mismatch remains usable", target: ImageTarget{Engine: "sglang", Vendor: "nvidia", Framework: "cuda", PlatformArch: "amd64", Override: "catalog/cuda:12.4"}, want: "catalog/cuda:12.4", warning: true},
		{name: "older CUDA major override is driver compatible", target: ImageTarget{Engine: "vllm", Vendor: "nvidia", Framework: "cuda", FrameworkVersion: "13.1", PlatformArch: "amd64", Override: "catalog/cuda:12.4"}, want: "catalog/cuda:12.4"},
		{name: "unknown custom override remains advisory", target: ImageTarget{Engine: "vllm", Vendor: "nvidia", Framework: "cuda", PlatformArch: "amd64", Override: "private/vllm:latest"}, want: "private/vllm:latest", warning: true},
		{name: "audited catalog spec image is trusted", target: ImageTarget{Engine: "vllm", Vendor: "nvidia", Framework: "cuda", PlatformArch: "amd64", Override: "private/catalog@sha256:abc", TrustedOverride: true}, want: "private/catalog@sha256:abc"},
		{name: "unlisted engine default is trusted on CUDA node", target: ImageTarget{Engine: "vllm", Vendor: "nvidia", Framework: "cuda", PlatformArch: "amd64", Override: "vllm/vllm-openai:latest"}, want: "vllm/vllm-openai:latest"},
		{name: "unlisted engine default remains usable on non CUDA node", target: ImageTarget{Engine: "vllm", Vendor: "ascend", Framework: "cann", PlatformArch: "arm64", Override: "vllm/vllm-openai:latest"}, want: "vllm/vllm-openai:latest", warning: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ResolveImageCatalog(tt.target, candidates)
			if got.Image != tt.want {
				t.Fatalf("image = %q, want %q (resolution=%+v)", got.Image, tt.want, got)
			}
			if (len(got.Warnings) > 0) != tt.warning {
				t.Fatalf("warnings = %v, want warning=%v", got.Warnings, tt.warning)
			}
			if got.Blocking != tt.blocking {
				t.Fatalf("blocking = %v, want %v (resolution=%+v)", got.Blocking, tt.blocking, got)
			}
		})
	}
}

func TestIsEngineDefaultImage(t *testing.T) {
	if !IsEngineDefaultImage("vllm", "vllm/vllm-openai:latest") {
		t.Fatal("vLLM default image was not recognized")
	}
	if IsEngineDefaultImage("vllm", "custom/vllm:latest") {
		t.Fatal("custom image was recognized as engine default")
	}
}

func TestImageCandidateSelectableUsesStableRuntimeConstraints(t *testing.T) {
	target := ImageTarget{Engine: "vllm", Vendor: "ascend", Framework: "cann", PlatformArch: "arm64"}
	base := ImageCandidate{Engine: "vllm", Framework: "cann", Arch: "arm64", ImageRef: "catalog/ascend:v1", Source: "official", Enabled: true}
	if !ImageCandidateSelectable(target, base) {
		t.Fatal("compatible target-chip image was not selectable")
	}
	// The framework is the single fact a vendor identity is derived from;
	// a framework for another vendor is what removes a choice.
	wrongFramework := base
	wrongFramework.Framework = "cuda"
	if ImageCandidateSelectable(target, wrongFramework) {
		t.Fatal("image for another framework family was selectable")
	}
	disabled := base
	disabled.Enabled = false
	if ImageCandidateSelectable(target, disabled) {
		t.Fatal("disabled catalog image was selectable")
	}
}

func TestResolveImageCatalogUsesCreationTimeAsFinalTieBreaker(t *testing.T) {
	target := ImageTarget{Engine: "vllm", Vendor: "nvidia", Framework: "cuda", FrameworkVersion: "12.4", PlatformArch: "amd64"}
	older := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	newer := older.Add(time.Hour)
	got := ResolveImageCatalog(target, []ImageCandidate{
		{Engine: "vllm", EngineVersion: "0.10", Framework: "cuda", FrameworkVersion: "12.4", Arch: "amd64", ImageRef: "catalog:older", Source: "official", Enabled: true, CreatedAt: older},
		{Engine: "vllm", EngineVersion: "0.10", Framework: "cuda", FrameworkVersion: "12.4", Arch: "amd64", ImageRef: "catalog:newer", Source: "official", Enabled: true, CreatedAt: newer},
	})
	if got.Image != "catalog:newer" {
		t.Fatalf("image = %q, want newest catalog entry", got.Image)
	}
}

func TestResolveImageCatalogFallbackAndAliases(t *testing.T) {
	got := ResolveImageCatalog(ImageTarget{Engine: "vllm", Vendor: "Huawei", PlatformArch: "aarch64"}, []ImageCandidate{
		{Engine: "vllm", Framework: "cann", Arch: "amd64", ImageRef: "wrong", Source: "official", Enabled: true},
	})
	if got.Image != "" || !got.Blocking {
		t.Fatalf("domestic fallback = %+v", got)
	}
	if len(got.Warnings) == 0 {
		t.Fatal("expected fallback warning")
	}
}

func TestResolveImageCatalogDoesNotAutoSelectUnclassifiedUserImage(t *testing.T) {
	got := ResolveImageCatalog(ImageTarget{Engine: "vllm", Vendor: "nvidia", Framework: "cuda", PlatformArch: "amd64"}, []ImageCandidate{
		{Engine: "vllm", ImageRef: "custom/vllm:latest", Source: "custom", Enabled: true},
	})
	if got.Image == "custom/vllm:latest" || len(got.Warnings) == 0 {
		t.Fatalf("unclassified image was auto-selected: %+v", got)
	}
	manual := ResolveImageCatalog(ImageTarget{Engine: "vllm", Vendor: "nvidia", Framework: "cuda", PlatformArch: "amd64", Override: "custom/vllm:latest"}, []ImageCandidate{
		{Engine: "vllm", ImageRef: "custom/vllm:latest", Source: "custom", Enabled: true},
	})
	if manual.Image != "custom/vllm:latest" {
		t.Fatalf("explicit unclassified override = %+v", manual)
	}
}

// A rolling tag is never picked automatically, even when it would win on
// version: its content moves, so replicas could end up on different builds.
// It stays selectable by hand.
func TestResolveImageCatalogSkipsRollingTags(t *testing.T) {
	target := ImageTarget{Engine: "vllm", Vendor: "nvidia", Framework: "cuda", FrameworkVersion: "12.9", PlatformArch: "amd64"}
	nightly := ImageCandidate{Engine: "vllm", EngineVersion: "0.99", Framework: "cuda", FrameworkVersion: "12.9", Arch: "amd64", ImageRef: "vllm/vllm-openai:nightly", Source: "community", Enabled: true, RollingTag: true}
	pinned := ImageCandidate{Engine: "vllm", EngineVersion: "0.31.0", Framework: "cuda", FrameworkVersion: "12.9", Arch: "amd64", ImageRef: "gpuworks/gpuworks-runner:cuda12.9-vllm0.31.0-r2", Source: "official", Enabled: true}
	if got := ResolveImageCatalog(target, []ImageCandidate{nightly, pinned}); got.Image != pinned.ImageRef {
		t.Fatalf("image = %q, want the pinned tag", got.Image)
	}
	if got := ResolveImageCatalog(target, []ImageCandidate{nightly}); got.Image == nightly.ImageRef || got.Source == "catalog" {
		t.Fatalf("a rolling tag was chosen automatically: %+v", got)
	}
	if !ImageCandidateSelectable(target, nightly) {
		t.Fatal("a rolling tag must remain selectable by hand")
	}
}

func TestResolveImageCatalogSelectsPreviouslyImportedVendorEngine(t *testing.T) {
	target := ImageTarget{Engine: "vllm", Vendor: "hygon", Framework: "dtk", PlatformArch: "amd64"}
	candidate := ImageCandidate{Engine: "vllm-dcu", Framework: "dtk", Arch: "amd64", Enabled: true, Source: "custom", ImageRef: "test/runner:v1"}
	if got := ResolveImageCatalog(target, []ImageCandidate{candidate}); got.Image != candidate.ImageRef {
		t.Fatalf("compatible vendor build not selected: %+v", got)
	}
	candidate.Engine = "sglang-dcu"
	if ImageCandidateSelectable(target, candidate) {
		t.Fatal("SGLang vendor build offered for a vLLM deployment")
	}
}

func TestManualReleaseIsOnlySelectedByOverride(t *testing.T) {
	target := ImageTarget{Engine: "vllm", Vendor: "nvidia", Framework: "cuda", FrameworkVersion: "13.0", PlatformArch: "amd64"}
	manual := ImageCandidate{Engine: "vllm", EngineVersion: "0.34.0", Framework: "cuda", FrameworkVersion: "13.0", Arch: "amd64", ImageRef: "registry.example/runner:cuda13.0-vllm0.34.0-r1", Source: "official", Enabled: true, AutoSelect: "manual"}
	if got := ResolveImageCatalog(target, []ImageCandidate{manual}); got.Image == manual.ImageRef {
		t.Fatal("auto-selected the manual release")
	}
	target.Override = manual.ImageRef
	if got := ResolveImageCatalog(target, []ImageCandidate{manual}); got.Blocking || got.Image != manual.ImageRef {
		t.Fatalf("override=%+v", got)
	}
}

func TestPartialMultiArchitectureWinner(t *testing.T) {
	multi := ImageCandidate{Engine: "vllm", Framework: "cuda", FrameworkVersion: "13.0", Arch: "multi", Enabled: true, ImageRef: "gpuworks/gpuworks-runner:cuda13.0-vllm0.34-r2", AutoSelect: "arm64"}
	single := multi
	single.AutoSelect = ""
	single.Arch = "amd64"
	single.ImageRef = "gpuworks/gpuworks-runner:cuda13.0-vllm0.34-r10"
	for arch, want := range map[string]string{"amd64": single.ImageRef, "arm64": multi.ImageRef} {
		target := ImageTarget{Engine: "vllm", PlatformArch: arch}
		got := ResolveImageCatalog(target, []ImageCandidate{multi, single})
		if got.Image != want || !ImageCandidateSelectable(target, multi) {
			t.Errorf("%s: %+v", arch, got)
		}
		target.Override = multi.ImageRef
		target.TrustedOverride = true
		if pinned := ResolveImageCatalog(target, []ImageCandidate{multi, single}); pinned.Image != multi.ImageRef || pinned.Blocking {
			t.Errorf("explicit template on %s: %+v", arch, pinned)
		}
	}
}
