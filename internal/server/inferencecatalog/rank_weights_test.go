package inferencecatalog

import (
	"strings"
	"testing"
)

// A spec's own weights may name the rank in their subfolder (ADR 0076); the
// YAML form round-trips, and the shapes that would hand a rank anything but
// its own part are refused at import.
func TestRankScopedWeightSources(t *testing.T) {
	raw := []byte(`models:
  - model_id: Org/Ranked
    specs:
      - name: tp2
        engine: vllm
        image: vllm/vllm-openai:latest
        weight_bytes: 1000
        weights:
          quantization: bf16
          sources:
            - modelscope:
                repo_id: Org/Ranked-tp2
                subfolder: rank${NODE_RANK}
            - huggingface:
                repo_id: Org/Ranked-tp2
                subfolder: rank${GPUWORKS_NODE_RANK}
        gpus:
          vendor: nvidia
`)
	catalog, err := ParseYAML(raw)
	if err != nil {
		t.Fatal(err)
	}
	if got := catalog.Models[0].Specs[0].Weights.Sources[0].Subfolder; got != "rank${NODE_RANK}" {
		t.Fatalf("subfolder = %q", got)
	}
	encoded, err := MarshalYAML(catalog)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), "subfolder: rank${NODE_RANK}") {
		t.Fatalf("marshaled YAML lost the rank subfolder:\n%s", encoded)
	}

	for _, tc := range []struct {
		name string
		edit func(*InferenceSpec)
		want string
	}{
		{name: "another variable", edit: func(spec *InferenceSpec) {
			spec.Weights.Sources[0].Subfolder = "tp${TP_SIZE}"
		}, want: "may name only ${GPUWORKS_NODE_RANK}, not ${TP_SIZE}"},
		{name: "a mirror without the rank", edit: func(spec *InferenceSpec) {
			spec.Weights.Sources[1].Subfolder = ""
		}, want: "either every weight source names ${GPUWORKS_NODE_RANK}"},
		{name: "a primary component without the rank", edit: func(spec *InferenceSpec) {
			whole := spec.Weights
			whole.Sources = []WeightSource{{Provider: "modelscope", RepoID: "Org/Ranked-tp2"}}
			spec.Components = []Component{{Name: "main", Role: "primary", Weights: whole, WeightBytes: 1000, MountPath: "/model"}}
		}, want: "the primary component main and the spec's own weights must both name ${GPUWORKS_NODE_RANK}"},
		{name: "with pd_mode", edit: func(spec *InferenceSpec) {
			spec.PDMode = "nixl"
		}, want: ""},
		{name: "unsafe expansion", edit: func(spec *InferenceSpec) {
			spec.Weights.Sources[0].Subfolder = "../rank${NODE_RANK}"
		}, want: "unsafe model repository subfolder"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			catalog, err := DecodeYAML(raw)
			if err != nil {
				t.Fatal(err)
			}
			tc.edit(&catalog.Models[0].Specs[0])
			err = ValidateFlatCatalog(&catalog)
			if err == nil {
				t.Fatal("accepted")
			}
			if tc.want != "" && !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}

// The rule applied to a spec value alone: plain subfolders are untouched,
// a PD spec with a rank subfolder is refused by the rank rule itself.
func TestValidateRankScopedWeightsRefusesPD(t *testing.T) {
	spec := &InferenceSpec{Name: "pd", PDMode: "nixl", Weights: Weights{Sources: []WeightSource{{Provider: "modelscope", RepoID: "o/r", Subfolder: "rank${NODE_RANK}"}}}}
	if err := validateRankScopedWeights("o/m", spec); err == nil || !strings.Contains(err.Error(), "pd_mode") {
		t.Fatalf("pd spec = %v", err)
	}
	spec.PDMode, spec.Weights.Sources[0].Subfolder = "nixl", "rank0"
	if err := validateRankScopedWeights("o/m", spec); err != nil {
		t.Fatalf("plain subfolder refused: %v", err)
	}
}

// A component's weights may name the rank too, each weights block deciding on
// its own: the GB10 TensorFold shape is a primary pack the same on every rank
// beside an auxiliary component split per rank. The primary component is the
// spec's own weights, so the two decide alike, and the PD layouts refuse a
// rank-scoped component as they refuse rank-scoped spec weights.
func TestRankScopedComponentWeights(t *testing.T) {
	raw := []byte(`models:
  - model_id: Org/Split-Aux
    specs:
      - name: tp2
        engine: vllm
        image: vllm/vllm-openai:latest
        weight_bytes: 1000
        weights:
          quantization: exl3
          sources:
            - huggingface: Org/Pack
        gpus:
          vendor: nvidia
        args:
          - --engram
          - /engram/rank${GPUWORKS_NODE_RANK}
        components:
          - name: main
            role: primary
            weights:
              quantization: exl3
              sources:
                - huggingface: Org/Pack
            weight_bytes: 1000
            mount_path: /model
          - name: engram
            role: auxiliary
            weights:
              quantization: bf16
              sources:
                - modelscope:
                    repo_id: Org/Engram-TP2
                    subfolder: rank${GPUWORKS_NODE_RANK}
                - huggingface:
                    repo_id: Org/Engram-TP2
                    subfolder: rank${NODE_RANK}
            weight_bytes: 500
            mount_path: /engram
`)
	catalog, err := ParseYAML(raw)
	if err != nil {
		t.Fatal(err)
	}
	if got := catalog.Models[0].Specs[0].Components[1].Weights.Sources[0].Subfolder; got != "rank${GPUWORKS_NODE_RANK}" {
		t.Fatalf("component subfolder = %q", got)
	}
	encoded, err := MarshalYAML(catalog)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), "subfolder: rank${GPUWORKS_NODE_RANK}") {
		t.Fatalf("marshaled YAML lost the component's rank subfolder:\n%s", encoded)
	}

	for _, tc := range []struct {
		name string
		edit func(*InferenceSpec)
		want string
	}{
		{name: "a component mirror without the rank", edit: func(spec *InferenceSpec) {
			spec.Components[1].Weights.Sources[1].Subfolder = ""
		}, want: "component engram: either every weight source names ${GPUWORKS_NODE_RANK}"},
		{name: "another variable in a component", edit: func(spec *InferenceSpec) {
			spec.Components[1].Weights.Sources[0].Subfolder = "tp${TP_SIZE}"
			spec.Components[1].Weights.Sources[1].Subfolder = "tp${TP_SIZE}"
		}, want: "may name only ${GPUWORKS_NODE_RANK}, not ${TP_SIZE}"},
		{name: "a rank-scoped primary beside whole spec weights", edit: func(spec *InferenceSpec) {
			spec.Components[0].Weights.Sources[0].Subfolder = "rank${NODE_RANK}"
		}, want: "the primary component main and the spec's own weights must both name"},
		{name: "a rank-scoped component with pd_mode", edit: func(spec *InferenceSpec) {
			spec.PDMode = "nixl"
		}, want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			catalog, err := DecodeYAML(raw)
			if err != nil {
				t.Fatal(err)
			}
			tc.edit(&catalog.Models[0].Specs[0])
			err = ValidateFlatCatalog(&catalog)
			if err == nil {
				t.Fatal("accepted")
			}
			if tc.want != "" && !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}

	// Both the primary and the spec's own weights rank-scoped is the spec's
	// rank-scoped weights written as a component.
	catalog, err = DecodeYAML(raw)
	if err != nil {
		t.Fatal(err)
	}
	spec := &catalog.Models[0].Specs[0]
	spec.Weights.Sources[0].Subfolder = "rank${GPUWORKS_NODE_RANK}"
	spec.Components[0].Weights.Sources[0].Subfolder = "rank${GPUWORKS_NODE_RANK}"
	if err := ValidateFlatCatalog(&catalog); err != nil {
		t.Fatalf("a rank-scoped primary component was refused: %v", err)
	}
	pd := &InferenceSpec{Name: "pd", PDMode: "nixl", Components: []Component{{Name: "aux", Role: "auxiliary",
		Weights: Weights{Sources: []WeightSource{{Provider: "modelscope", RepoID: "o/r", Subfolder: "rank${NODE_RANK}"}}}}}}
	if err := validateRankScopedWeights("o/m", pd); err == nil || !strings.Contains(err.Error(), "pd_mode") {
		t.Fatalf("pd spec with a rank-scoped component = %v", err)
	}
}
