package deploy

import (
	"testing"
	"time"
)

// The model segment of a runner tag (910b, a3, 310p) names the chip family an
// image is built for. Every cann9.2 image scored the same before, so which one
// won was down to catalog insertion order: an A3 build was picked on a 910B4
// node.
func TestResolveImageCatalogChipFamily(t *testing.T) {
	now := time.Now()
	image := func(family, ref, fwVersion string, age time.Duration) ImageCandidate {
		return ImageCandidate{Engine: "vllm", Framework: "cann", FrameworkVersion: fwVersion, Arch: "arm64", ChipFamily: family, ImageRef: ref, Source: "official", Enabled: true, CreatedAt: now.Add(age)}
	}
	candidates := []ImageCandidate{
		image("910b", "r/runner:cann9.2-910b-vllm0.30.0rc2-r1", "9.2", 0),
		image("910c", "r/runner:cann9.2-a3-vllm0.30.0rc2-r1", "9.2", time.Hour), // inserted later, so newer
		image("310p", "r/runner:cann9.2-310p-vllm0.30.0rc2-r1", "9.2", 2*time.Hour),
		image("950", "r/runner:cann9.2-950-vllm0.30.0rc2-r1", "9.2", 3*time.Hour),
		image("", "r/runner:cann8.5-vllm0.18.0-r1", "8.5", -time.Hour), // generic, older framework
	}
	target := func(family string) ImageTarget {
		return ImageTarget{Engine: "vllm", Vendor: "ascend", Framework: "cann", PlatformArch: "arm64", ChipFamily: family}
	}
	tests := []struct {
		name   string
		target ImageTarget
		want   string
	}{
		{"910b node skips the newer A3 build", target("910b"), "r/runner:cann9.2-910b-vllm0.30.0rc2-r1"},
		{"910c node takes the A3 build", target("910c"), "r/runner:cann9.2-a3-vllm0.30.0rc2-r1"},
		{"310p node takes the 310p build", target("310p"), "r/runner:cann9.2-310p-vllm0.30.0rc2-r1"},
		{"unknown node family excludes nothing", target(""), "r/runner:cann9.2-950-vllm0.30.0rc2-r1"},
		{"family case is not significant", target("910B"), "r/runner:cann9.2-910b-vllm0.30.0rc2-r1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ResolveImageCatalog(tt.target, candidates)
			if got.Image != tt.want || got.Blocking || got.Source != "catalog" {
				t.Fatalf("resolution = %+v, want catalog image %q", got, tt.want)
			}
		})
	}

	t.Run("generic image is kept for every family", func(t *testing.T) {
		generic := []ImageCandidate{candidates[4]}
		for _, family := range []string{"910b", "910c", "310p", "950", ""} {
			if got := ResolveImageCatalog(target(family), generic); got.Image != generic[0].ImageRef {
				t.Fatalf("family %q: image = %q, want the generic image", family, got.Image)
			}
		}
	})
	t.Run("only a mismatched image left means no match", func(t *testing.T) {
		got := ResolveImageCatalog(target("910b"), candidates[1:2])
		if got.Image != "" || !got.Blocking {
			t.Fatalf("resolution = %+v, want a blocking no-match", got)
		}
	})
	t.Run("selectable follows the same rule", func(t *testing.T) {
		if ImageCandidateSelectable(target("910b"), candidates[1]) {
			t.Fatal("A3 image was selectable on a 910b node")
		}
		if !ImageCandidateSelectable(target("910b"), candidates[0]) || !ImageCandidateSelectable(target("910b"), candidates[4]) {
			t.Fatal("910b and generic images must stay selectable on a 910b node")
		}
		if !ImageCandidateSelectable(target(""), candidates[1]) {
			t.Fatal("an unknown node family must not remove a choice")
		}
	})
	t.Run("a manual mismatched image is refused", func(t *testing.T) {
		tgt := target("910b")
		tgt.Override = "r/runner:cann9.2-a3-vllm0.30.0rc2-r1"
		got := ResolveImageCatalog(tgt, candidates)
		if got.Image != tgt.Override || !got.Blocking || len(got.Warnings) != 1 {
			t.Fatalf("resolution = %+v, want the override blocked with a warning", got)
		}
		tgt.TrustedOverride = true
		if got := ResolveImageCatalog(tgt, candidates); !got.Blocking {
			t.Fatalf("a trusted spec image for another chip family must be blocked: %+v", got)
		}
		tgt.ChipFamily = "910c"
		if got := ResolveImageCatalog(tgt, candidates); got.Blocking {
			t.Fatalf("the matching family must not be blocked: %+v", got)
		}
	})
}
