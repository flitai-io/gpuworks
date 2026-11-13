package seed

import (
	"regexp"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/chipmatch"
)

// An Ascend runner image carries the kernel package of one chip family. A spec
// pinned to that image must not claim cards whose kernels the image does not
// carry. Generational inheritance therefore needs both a successor predicate
// and the successor's image, not a broader predicate on the original image.
var chipSegment = map[string]struct{ runs, refuses []string }{
	"910b": {
		runs:    []string{"Ascend 910B1", "Ascend 910B2C", "Ascend 910B4"},
		refuses: []string{"Ascend 910C", "Ascend 910C3", "Ascend 950DT", "Ascend 950PR", "Ascend 310P3"},
	},
	"a3": {
		runs:    []string{"Ascend 910C", "Ascend 910C3"},
		refuses: []string{"Ascend 910B1", "Ascend 910B4", "Ascend 950DT", "Ascend 310P3"},
	},
	"310p": {
		runs:    []string{"Ascend 310P3"},
		refuses: []string{"Ascend 910B4", "Ascend 910C", "Ascend 950DT"},
	},
	"950": {
		runs:    []string{"Ascend 950DT", "Ascend 950PR"},
		refuses: []string{"Ascend 910B4", "Ascend 910C", "Ascend 310P3"},
	},
}

// runnerChipSegment reads the accelerator segment from a runner tag and
// reports whether the image is a chip-specific runner.
func runnerChipSegment(image string) (string, bool) {
	if !strings.HasPrefix(image, systemImageRepository) {
		return "", false
	}
	tag := image[strings.LastIndex(image, ":")+1:]
	for segment := range chipSegment {
		if strings.Contains(tag, "-"+segment+"-") {
			return segment, true
		}
	}
	return "", false
}

func TestAscendSpecsClaimTheCardsTheirImageWasBuiltFor(t *testing.T) {
	catalog, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, model := range catalog.Models {
		for _, spec := range model.Specs {
			segment, ok := runnerChipSegment(spec.Image)
			if !ok {
				continue
			}
			checked++
			where := model.ModelID + "/" + spec.Name
			predicate := chipmatch.Predicate{Vendor: spec.GPUs.Vendor, Name: spec.GPUs.Name, CC: spec.GPUs.CC}
			cards := chipSegment[segment]
			matched := false
			for _, name := range cards.runs {
				if chipmatch.Evaluate(chipmatch.GPU{Vendor: "ascend", Name: name}, predicate).Matched {
					matched = true
					break
				}
			}
			if !matched {
				t.Errorf("%s runs the %s build but its predicate %q matches none of %v",
					where, segment, spec.GPUs.Name, cards.runs)
			}
			for _, name := range cards.refuses {
				if chipmatch.Evaluate(chipmatch.GPU{Vendor: "ascend", Name: name}, predicate).Matched {
					t.Errorf("%s claims %s, which the %s build has no kernels for (predicate %q)",
						where, name, segment, spec.GPUs.Name)
				}
			}
		}
	}
	if checked == 0 {
		t.Fatal("no built-in spec runs a chip-specific runner image; the check would pass vacuously")
	}
}

// The inherited A3 profiles are catalog contracts, not individual real-hardware
// certifications. They must remain the same recipe as their 910B origin except
// for the image and predicate.
func TestInheritedA3SpecsMatchTheir910BOriginal(t *testing.T) {
	catalog, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}
	intentionalArgDifferences := map[string]string{
		"qwen/qwen3-vl-8b-instruct/bf16-vllm-ascend-a3-4eac0a8b":            "TP1 needs the measured 196608-token cap",
		"google/gemma-3-4b-it/bf16-sglang-ascend-a3-4cdeb491":               "A3 graph capture hangs; eager mode passed text and image chat",
		"google/gemma-3-27b-it/bf16-sglang-ascend-a3-ca729c08":              "A3 graph capture hangs; eager mode passed text and image chat",
		"google/gemma-4-26b-a4b-it/bf16-vllm-ascend-a3-d8ff96c2":            "A3 graph capture fails; eager mode passed text and image chat",
		"zai-org/glm-4.5v/bf16-sglang-ascend-a3-36013872":                   "A3 graph capture stalls across ranks; eager mode passed image chat",
		"qwen/qwen3-next-80b-a3b-instruct/int8-sglang-ascend-a3-7d129784":   "A3 NCCL graph capture stalls; eager mode passed chat",
		"qwen/qwen3-vl-30b-a3b-instruct/bf16-sglang-ascend-a3-8659b2c8":     "A3 graph capture stalls; eager mode passed text and image chat",
		"qwen/qwen3-235b-a22b/int8-vllm-ascend-a3-abf48ff4":                 "16-rank ACL graph capture exhausts notify resources; eager mode passed chat",
		"qwen/qwen3.6-35b-a3b/bf16-sglang-ascend-a3-67436760":               "A3 graph capture stalls; eager mode passed text and image chat",
		"openbmb/minicpm3-4b/bf16-sglang-ascend-a3-25156446":                "A3 graph capture aborts the child; eager mode passed chat",
		"openbmb/minicpm-v-4_5/int4-sglang-ascend-a3-8fe71b3c":              "AWQ must leave the unquantized vision tower and resampler out; eager mode passed text and image chat",
		"qwen/qwen3-30b-a3b-instruct-2507/bf16-sglang-ascend-a3-04996b03":   "A3 graph capture did not become healthy; eager mode passed chat",
		"qwen/qwen3-omni-30b-a3b-instruct/bf16-sglang-ascend-a3-68c174cb":   "A3 graph capture did not become healthy; eager mode passed chat",
		"qwen/qwen3-14b-gguf/fp16-sglang-ascend-a3-41226ec8":                "the repository contains five GGUF files; Q4_K_M loads eagerly on A3",
		"qwen/qwen3-tts-12hz-1.7b-customvoice/bf16-vllm-ascend-a3-760501dc": "the 0.30 Omni runner requires an explicit deploy config",
		"qwen/qwen3.6-35b-a3b/int8-vllm-ascend-a3-1a118d71":                 "vLLM 0.30 with eager mode avoids the missing linear_attn graph path",
	}
	argsDiffSeen := map[string]bool{}
	pairs := 0
	for _, model := range catalog.Models {
		byName := map[string]int{}
		for i, spec := range model.Specs {
			byName[spec.Name] = i
		}
		for _, spec := range model.Specs {
			if !strings.Contains(spec.Name, "-ascend-a3-") {
				continue
			}
			origin, ok := byName[strings.Replace(spec.Name, "-ascend-a3-", "-ascend-", 1)]
			if !ok {
				continue
			}
			pairs++
			original := model.Specs[origin]
			where := model.ModelID + "/" + spec.Name
			if spec.Engine != original.Engine {
				t.Errorf("%s runs %q but its 910B original runs %q", where, spec.Engine, original.Engine)
			}
			argsDiffer := strings.Join(spec.Args, "\x00") != strings.Join(original.Args, "\x00")
			reason, intentional := intentionalArgDifferences[where]
			if argsDiffer && !intentional {
				t.Errorf("%s arguments drifted from its 910B original", where)
			}
			if argsDiffer && intentional {
				argsDiffSeen[where] = true
				if strings.TrimSpace(reason) == "" {
					t.Errorf("%s has an empty intentional-difference reason", where)
				}
			}
			if spec.WeightBytes != original.WeightBytes {
				t.Errorf("%s weighs %d bytes, its 910B original %d", where, spec.WeightBytes, original.WeightBytes)
			}
			if spec.Image == original.Image {
				t.Errorf("%s runs the same image as its 910B original (%s)", where, spec.Image)
			}
		}
	}
	for where, reason := range intentionalArgDifferences {
		if !argsDiffSeen[where] {
			t.Errorf("%s arguments now match its 910B original; remove the stale exemption (%s)", where, reason)
		}
	}
	const currentInheritedA3Profiles = 88
	if pairs != currentInheritedA3Profiles {
		t.Fatalf("inherited A3 profile count = %d, want the reviewed catalog count %d", pairs, currentInheritedA3Profiles)
	}
	t.Logf("checked %d inherited A3 catalog contracts; this is not a real-hardware pass count", pairs)
}

func TestAscend910CatalogPredicatesAcceptProductSuffixes(t *testing.T) {
	catalog, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}
	tested := 0
	for _, model := range catalog.Models {
		for _, spec := range model.Specs {
			if !strings.EqualFold(spec.GPUs.Vendor, "ascend") || !strings.Contains(strings.ToUpper(spec.GPUs.Name), "910") {
				continue
			}
			tested++
			predicate := chipmatch.Predicate{Vendor: spec.GPUs.Vendor, Name: spec.GPUs.Name, CC: spec.GPUs.CC}
			where := model.ModelID + "/" + spec.Name
			family, suffixed := "Ascend 910B", "Ascend 910B2C"
			if regexp.MustCompile(spec.GPUs.Name).MatchString("910C") {
				family, suffixed = "Ascend 910C", "Ascend 910C3"
			}
			if !chipmatch.Evaluate(chipmatch.GPU{Vendor: "ascend", Name: family}, predicate).Matched &&
				!chipmatch.Evaluate(chipmatch.GPU{Vendor: "ascend", Name: suffixed}, predicate).Matched {
				t.Errorf("%s rejects both %q and %q with predicate %q", where, family, suffixed, spec.GPUs.Name)
			}
			for _, name := range []string{"Ascend 310P", "Ascend 910A"} {
				if result := chipmatch.Evaluate(chipmatch.GPU{Vendor: "ascend", Name: name}, predicate); result.Matched {
					t.Errorf("%s accepts unrelated card %q with predicate %q", where, name, spec.GPUs.Name)
				}
			}
		}
	}
	if tested == 0 {
		t.Fatal("embedded catalog has no Ascend 910 predicates")
	}
}
