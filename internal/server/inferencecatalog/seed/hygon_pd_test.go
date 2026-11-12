package seed

import (
	"regexp"
	"strconv"
	"testing"
)

// vllmMooncakeImageRevision pulls the runner revision out of a DTK vLLM 0.21
// image reference, the tier the vllm-mooncake PD mode runs on.
var vllmMooncakeImageRevision = regexp.MustCompile(`^gpuworks/gpuworks-runner:dtk26\.04-vllm0\.21\.0-r([0-9]+)$`)

// TestEveryVLLMMooncakeSpecRunsAnImageThatCarriesItsModel sweeps the built-in
// vllm-mooncake PD specs. They run the DTK vLLM 0.21 runner, whose r2 makes a
// failed pull follow kv_load_failure_policy; a model with gated delta net or
// Mamba layers needs r4: before r3 its engines do not start (the connector
// cannot register the recurrent state) or crash on the first failed pull, and
// r3 can zero blocks the async KV load is writing, corrupting the answer. The
// count guard keeps the sweep from going green on nothing.
func TestEveryVLLMMooncakeSpecRunsAnImageThatCarriesItsModel(t *testing.T) {
	catalog, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}
	hybrids := 0
	for _, model := range catalog.Models {
		recurrent := model.ModelConfig.HasRecurrentLayers()
		for _, spec := range model.Specs {
			if spec.PDMode != "vllm-mooncake" {
				continue
			}
			match := vllmMooncakeImageRevision.FindStringSubmatch(spec.Image)
			if match == nil {
				t.Errorf("%s/%s: vllm-mooncake runs on the DTK vLLM 0.21 runner, not %s", model.ModelID, spec.Name, spec.Image)
				continue
			}
			revision, _ := strconv.Atoi(match[1])
			want := 2
			if recurrent {
				want = 4
				hybrids++
			}
			if revision < want {
				t.Errorf("%s/%s: %s is r%d, the model needs r%d or later", model.ModelID, spec.Name, spec.Image, revision, want)
			}
		}
	}
	if hybrids < 2 {
		t.Fatalf("judged %d vllm-mooncake specs of hybrid models, want the Qwen3.5-0.8B and Qwen3.8-27B ones", hybrids)
	}
}
