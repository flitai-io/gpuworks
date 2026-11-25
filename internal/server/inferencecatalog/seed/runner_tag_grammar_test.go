package seed

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const runnerRepositoryPath = "gpuworks/gpuworks-runner"

// runnerTag returns the tag of a reference to the runner repository on any
// registry, and whether the reference names that repository at all.
func runnerTag(ref string) (tag string, runner bool) {
	at := strings.Index(ref, runnerRepositoryPath)
	if at < 0 || (at > 0 && ref[at-1] != '/') {
		return "", false
	}
	rest := ref[at+len(runnerRepositoryPath):]
	if rest != "" && rest[0] != ':' && rest[0] != '@' {
		return "", false // a longer repository name
	}
	tag, _, _ = strings.Cut(strings.TrimPrefix(rest, ":"), "@")
	if strings.HasPrefix(rest, "@") {
		tag = ""
	}
	return tag, true
}

// releaseTagGrammar is the expression the image manifest generator exports
// for the publishing scripts. It is read, never written again here.
func releaseTagGrammar(t *testing.T) *regexp.Regexp {
	t.Helper()
	raw, err := os.ReadFile("../../../../build/runner-images/tag-grammar.txt")
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, line := range strings.Split(string(raw), "\n") {
		if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, "#") {
			lines = append(lines, line)
		}
	}
	if len(lines) != 1 {
		t.Fatalf("tag-grammar.txt holds %d expressions, want 1", len(lines))
	}
	return regexp.MustCompile(lines[0])
}

// declaredLegacyRunnerTags are the existing tags the matrix declares as
// outside the grammar. Their number is the published_tags_outside_grammar
// budget, so a template cannot bring in a new name through this list.
func declaredLegacyRunnerTags(t *testing.T) map[string]bool {
	t.Helper()
	raw, err := os.ReadFile("../../../../build/runner-images/matrix.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var matrix struct {
		Published []struct {
			Tag          string `yaml:"tag"`
			LegacyReason string `yaml:"legacy_reason"`
		} `yaml:"published"`
	}
	if err := yaml.Unmarshal(raw, &matrix); err != nil {
		t.Fatal(err)
	}
	tags := map[string]bool{}
	for _, item := range matrix.Published {
		if strings.TrimSpace(item.LegacyReason) != "" {
			tags[item.Tag] = true
		}
	}
	return tags
}

// A built-in template names a runner image by a tag of the release grammar,
// in image and in every image_sources copy, whichever registry the reference
// goes through. The only other tags accepted are the ones the matrix already
// declares as legacy.
func TestBuiltinTemplateRunnerImagesFollowTheTagGrammar(t *testing.T) {
	catalog, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}
	grammar, legacy := releaseTagGrammar(t), declaredLegacyRunnerTags(t)
	checked := 0
	for _, model := range catalog.Models {
		for _, spec := range model.Specs {
			for _, ref := range append([]string{spec.Image}, spec.ImageSources...) {
				tag, runner := runnerTag(ref)
				if !runner {
					continue
				}
				checked++
				if !grammar.MatchString(tag) && !legacy[tag] {
					t.Errorf("%s/%s references %s: tag %q is outside <backend><version>[-<model>]-<engine><version>-r<number> "+
						"and is not a declared legacy tag in build/runner-images/matrix.yaml", model.ModelID, spec.Name, ref, tag)
				}
			}
		}
	}
	if checked == 0 {
		t.Fatal("no built-in template references a runner image; the check would pass vacuously")
	}
}

func TestRunnerTagReadsReferencesOnAnyRegistry(t *testing.T) {
	for _, test := range []struct {
		ref, tag string
		runner   bool
	}{
		{"gpuworks/gpuworks-runner:cuda13.0-vllm0.30.0-r4", "cuda13.0-vllm0.30.0-r4", true},
		{"quay.io/gpuworks/gpuworks-runner:cuda13.0-vllm0.30.0-r4", "cuda13.0-vllm0.30.0-r4", true},
		{"registry.example:5000/gpuworks/gpuworks-runner:cuda13.0-vllm0.30.0-r4@sha256:ab", "cuda13.0-vllm0.30.0-r4", true},
		{"quay.io/gpuworks/gpuworks-runner@sha256:ab", "", true},
		{"gpuworks/gpuworks-runner", "", true},
		{"gpuworks/gpuworks-runner-extra:r1", "", false},
		{"notgpuworks/gpuworks-runner:r1", "", false},
		{"gpuworks/strata:0.1-gw6", "", false},
		{"vllm/vllm-openai:v0.30.0", "", false},
	} {
		if tag, runner := runnerTag(test.ref); tag != test.tag || runner != test.runner {
			t.Errorf("runnerTag(%q) = %q, %v; want %q, %v", test.ref, tag, runner, test.tag, test.runner)
		}
	}
}
