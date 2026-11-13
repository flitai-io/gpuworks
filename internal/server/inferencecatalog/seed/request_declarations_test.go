package seed

import (
	"os"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"gpuworks.ai/gpuworks/internal/server/inferenceapi"
)

func TestQwen3TTSSpecsDeclareTaskInputs(t *testing.T) {
	raw, err := os.ReadFile("../../../../test/e2e/testdata/playground-acceptance/qwen3-tts-api-declarations.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Declarations map[string]inferenceapi.Contract `yaml:"declarations"`
	}
	if err := yaml.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	catalog, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]int{}
	for _, model := range catalog.Models {
		want, ok := fixture.Declarations[model.ModelID]
		if !ok {
			continue
		}
		want, err = inferenceapi.NormalizeContract(want)
		if err != nil {
			t.Fatal(err)
		}
		for _, spec := range model.Specs {
			if spec.API == nil || !reflect.DeepEqual(*spec.API, want) {
				t.Fatalf("%s/%s differs from approved request declaration", model.ModelID, spec.Name)
			}
			seen[model.ModelID]++
		}
	}
	if len(seen) != 5 {
		t.Fatalf("models=%v", seen)
	}
	for name := range fixture.Declarations {
		if seen[name] == 0 {
			t.Fatalf("missing %s", name)
		}
	}
}

func TestEveryEmbeddedSpecContractNormalizes(t *testing.T) {
	catalog, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}
	for _, model := range catalog.Models {
		for _, spec := range model.Specs {
			contract, err := inferenceapi.EffectiveContract(spec.API, inferenceapi.DefaultsInput{Engine: spec.Engine})
			if err != nil {
				t.Fatalf("%s/%s: %v", model.ModelID, spec.Name, err)
			}
			for _, operation := range contract.Operations {
				if operation.Inputs == nil {
					t.Fatalf("%s/%s/%s has nil inputs", model.ModelID, spec.Name, operation.Type)
				}
				if strings.HasPrefix(model.ModelID, "qwen/qwen3-tts-") && len(operation.Inputs) == 0 {
					t.Fatalf("%s/%s lost task inputs", model.ModelID, spec.Name)
				}
			}
		}
	}
}
