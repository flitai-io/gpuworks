package seed

import (
	"reflect"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/deploy"
)

func isStrataRunnerSpec(name string) bool {
	switch name {
	case strataRunnerSpecName, "q2-0-strata-nvidia", "iq3-xxs-strata-nvidia", "iq3-s-strata-nvidia":
		return true
	}
	return false
}

// The mount and the setup quantization share an option name in the image.
// A separate --model QUANT would replace the mount; omitting the override
// would make setup look for IQ2_XS in a different quantization's directory.
func TestStrataQuantizationsResolveMatchingWeightsAndLaunch(t *testing.T) {
	_, original := catalogSpec(t, strataModelID, strataRunnerSpecName)
	for _, tc := range []struct {
		name, quant string
		bytes       int64
	}{
		{"q2-0-strata-nvidia", "Q2_0", 66423878624},
		{"iq3-xxs-strata-nvidia", "IQ3_XXS", 75839998528},
		{"iq3-s-strata-nvidia", "IQ3_S", 83617662656},
	} {
		t.Run(tc.quant, func(t *testing.T) {
			_, spec := catalogSpec(t, strataModelID, tc.name)
			if spec.Weights.Quantization != strings.ToLower("GSQ-RCO "+tc.quant) || spec.WeightBytes != tc.bytes {
				t.Fatalf("artifact = %q / %d bytes, want %s / %d bytes", spec.Weights.Quantization, spec.WeightBytes, tc.quant, tc.bytes)
			}
			if len(spec.Weights.Sources) != 2 {
				t.Fatal("quantization needs both verified weight providers")
			}
			for i, provider := range []struct{ name, revision string }{
				{"huggingface", "ed59f92082b1e93c0e96d60a8b11aab089b52f09"},
				{"modelscope", "1868010e27505d4b8cc15970cdcc4dd76005d7de"},
			} {
				source := spec.Weights.Sources[i]
				if source.Provider != provider.name || source.RepoID != "ISTA-DASLab/Qwen3.8-Flash-Next-GSQ-RCO-GGUF" ||
					source.Revision != provider.revision || source.Subfolder != tc.quant {
					t.Fatal("each provider must download the selected quantization's pinned subfolder")
				}
			}
			if spec.DisplayNameZHCN != "Turing 及以上 · "+tc.quant+" · Strata r5" ||
				spec.DisplayNameEN != "Turing+ · "+tc.quant+" · Strata r5" {
				t.Fatal("display names must identify the quantization and published image")
			}
			engine, err := deploy.SelectEngine(spec.Engine)
			if err != nil {
				t.Fatal(err)
			}
			launch, err := deploy.ResolveLaunch(deploy.ResolveLaunchInput{
				Engine: engine, Entrypoint: spec.Entrypoint, APIContract: spec.API,
				Params: deploy.Params{ModelPath: "/model", APIPort: 30000, TemplateArgs: spec.Args, TemplateEnv: spec.Env},
			})
			if err != nil {
				t.Fatal(err)
			}
			want := []string{"--model", "/model", "--host", "0.0.0.0", "--port", "30000", "--model=" + tc.quant}
			if !reflect.DeepEqual(launch.Argv, want) {
				t.Fatalf("launch arguments = %v, want %v", launch.Argv, want)
			}
			// Quantization changes neither device assignment nor admission, the
			// queue asset, tokenizer, API, image or its published mirrors.
			spec.Name = original.Name
			spec.DisplayNameZHCN, spec.DisplayNameEN = original.DisplayNameZHCN, original.DisplayNameEN
			spec.Weights, spec.WeightBytes, spec.Args = original.Weights, original.WeightBytes, original.Args
			if !reflect.DeepEqual(spec, original) {
				t.Fatal("quantization changed the shared runner launch contract")
			}
		})
	}
}
