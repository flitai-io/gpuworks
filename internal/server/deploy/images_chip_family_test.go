package deploy

import (
	"testing"
	"time"
)

// The model segment of a runner tag (910b, a3, 310p) names the chip family an
// image is built for. Every cann9.2 image scored the same before, so which one
// won was down to catalog insertion order: an A3 build was picked on a 910B4
// node. The family only orders the pick; no image is removed.
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
	t.Run("an image for another family is ranked last, not removed", func(t *testing.T) {
		// Alone, it is still the pick.
		if got := ResolveImageCatalog(target("910b"), candidates[1:2]); got.Image != candidates[1].ImageRef || got.Blocking {
			t.Fatalf("only an A3 image left: %+v", got)
		}
		// A generic image comes before it, even an older one.
		if got := ResolveImageCatalog(target("910b"), []ImageCandidate{candidates[1], candidates[4]}); got.Image != candidates[4].ImageRef {
			t.Fatalf("A3 vs generic on a 910b node = %q", got.Image)
		}
		// So does an image of the node's family the driver may not support.
		warned := candidates[0]
		warned.FrameworkVersion = "99.0"
		tgt := target("910b")
		tgt.DriverVersion = "23.0.0"
		if got := ResolveImageCatalog(tgt, []ImageCandidate{candidates[1], warned}); got.Image != warned.ImageRef {
			t.Fatalf("A3 vs a driver-warned 910b image = %q", got.Image)
		}
	})
	t.Run("every image stays selectable", func(t *testing.T) {
		for _, c := range candidates {
			if !ImageCandidateSelectable(target("910b"), c) {
				t.Fatalf("%s not selectable on a 910b node", c.ImageRef)
			}
		}
	})
	t.Run("a manual image for another family stands", func(t *testing.T) {
		tgt := target("910b")
		tgt.Override = "r/runner:cann9.2-a3-vllm0.30.0rc2-r1"
		if got := ResolveImageCatalog(tgt, candidates); got.Image != tgt.Override || got.Blocking {
			t.Fatalf("resolution = %+v, want the override as given", got)
		}
		tgt.TrustedOverride = true
		if got := ResolveImageCatalog(tgt, candidates); got.Blocking {
			t.Fatalf("a trusted spec image for another chip family was blocked: %+v", got)
		}
	})
}
