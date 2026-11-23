package seed

import (
	"regexp"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/chipmatch"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/imagecatalog"
	"gpuworks.ai/gpuworks/internal/server/imagecatalog/pullref"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// upstreamAscendImages lists the non-runner images reviewed for built-in
// Ascend specs, together with the accelerator each image is built for. An
// unlisted image fails the contract instead of being inferred from tag text.
var upstreamAscendImages = map[string]string{
	"quay.io/ascend/vllm-ascend@sha256:2cbb274af050cb5be7db7318e1c461fe22ec0643c54f1ed32aa7266513a7d421": "910C",
	"quay.io/ascend/vllm-ascend@sha256:4909659565f15e94ec70c3f8523d9f2ea3dd8a2a04c59b238fbe87f5eacdae36": "910C",
	// The kimi-k3.1-pro-a3 tag, which upstream publishes for Atlas 800I A3.
	"quay.io/ascend/vllm-ascend@sha256:e7c67329d7a7390dd472437020531a4ef289daa4f7374d11042364f80ab596b7": "910C",
	"quay.io/ascend/vllm-ascend:v0.30.0rc2-a3":                                                           "910C",
	// v0.32.0rc1-openeuler, the release build for Atlas A2 (910B).
	"quay.io/ascend/vllm-ascend@sha256:53a60329962f843fa4c8965c28e98c194a894e34ce5e473edf79e57cbb723188": "910B",
	// The A2 build the GLM-5.3-Flash 910B source overlay is pinned to.
	"quay.io/ascend/vllm-ascend@sha256:28351d337b7f9367073344240fa4e28d99794d549d577eaf317689950064150a": "910B",
}

func TestBuiltinAscendSpecsRunReviewedImages(t *testing.T) {
	catalog, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, model := range catalog.Models {
		for _, spec := range model.Specs {
			if !strings.EqualFold(spec.GPUs.Vendor, "ascend") {
				continue
			}
			where := model.ModelID + "/" + spec.Name
			if strings.HasPrefix(spec.Image, systemImageRepository) {
				continue
			}
			card, reviewed := upstreamAscendImages[spec.Image]
			if !reviewed {
				t.Errorf("%s runs unreviewed image %q; add it to upstreamAscendImages with the card it is built for", where, spec.Image)
				continue
			}
			checked++
			predicate := chipmatch.Predicate{Vendor: spec.GPUs.Vendor, Name: spec.GPUs.Name, CC: spec.GPUs.CC}
			if result := chipmatch.Evaluate(chipmatch.GPU{Vendor: "ascend", Name: "Ascend " + card}, predicate); !result.Matched {
				t.Errorf("%s runs the %s build of %q but its predicate %q rejects that card", where, card, spec.Image, spec.GPUs.Name)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no built-in Ascend spec runs a reviewed upstream image; the check would pass vacuously")
	}
}

// Mirror pulls can be tagged as the canonical runner and reused by any spec.
func TestBuiltinRunnerImageSourcesMatchCanonicalReference(t *testing.T) {
	catalog, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}
	for _, model := range catalog.Models {
		for _, spec := range model.Specs {
			_, image, _ := strings.Cut(pullref.RefKey(spec.Image), "/")
			repository, _, _ := strings.Cut(image, "@")
			repository, _, _ = strings.Cut(repository, ":")
			if repository != "gpuworks/gpuworks-runner" {
				continue
			}
			for _, source := range spec.ImageSources {
				if !strings.HasSuffix(pullref.RefKey(source), "/"+image) {
					t.Errorf("%s/%s source %q must preserve the runner repository and version of %q", model.ModelID, spec.Name, source, spec.Image)
				}
			}
		}
	}
}

func TestEvery910CClaimingBuiltInTemplateSelectsAnA3Image(t *testing.T) {
	catalog, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open("file:seed_910c_a3_images?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, err := imagecatalog.SyncSeed(st); err != nil {
		t.Fatal(err)
	}
	candidates, err := imagecatalog.Candidates(st)
	if err != nil {
		t.Fatal(err)
	}
	byRef := make(map[string]deploy.ImageCandidate, len(candidates))
	for _, candidate := range candidates {
		byRef[candidate.ImageRef] = candidate
	}

	checked := 0
	for _, model := range catalog.Models {
		for _, spec := range model.Specs {
			if !strings.EqualFold(spec.GPUs.Vendor, "ascend") {
				continue
			}
			predicate, err := regexp.Compile(spec.GPUs.Name)
			if err != nil {
				t.Errorf("%s/%s has invalid GPU predicate %q: %v", model.ModelID, spec.Name, spec.GPUs.Name, err)
				continue
			}
			if !predicate.MatchString("910C") {
				continue
			}
			checked++
			where := model.ModelID + "/" + spec.Name
			frameworkVersion := ""
			if original, audited := byRef[spec.Image]; audited {
				frameworkVersion = original.FrameworkVersion
			}
			target := deploy.ImageTarget{
				Engine: spec.Engine, Vendor: "ascend", Framework: "cann",
				FrameworkVersion: frameworkVersion, PlatformArch: "arm64",
				Override: spec.Image, TrustedOverride: true,
			}
			resolution := deploy.ResolveImageCatalog(target, candidates)
			if resolution.Blocking || strings.TrimSpace(resolution.Image) == "" {
				t.Errorf("%s has no A3 image replacement: %+v", where, resolution)
				continue
			}
			if card, reviewed := upstreamAscendImages[resolution.Image]; reviewed && card == "910C" {
				continue
			}
			if !strings.Contains(strings.ToLower(resolution.Image), "a3") {
				t.Errorf("%s resolved non-A3 image %q", where, resolution.Image)
			}
		}
	}
	// This exact count audits the current catalog's image-routing contract. It
	// is deliberately not presented as a count of real-hardware validations.
	const current910CClaimingTemplates = 111
	if checked != current910CClaimingTemplates {
		t.Fatalf("910C-claiming template count = %d, want the reviewed catalog count %d", checked, current910CClaimingTemplates)
	}
	t.Logf("audited A3 image routing for %d 910C-claiming templates", checked)
}
