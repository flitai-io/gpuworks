package inferencecatalog

import (
	"strings"
	"testing"
)

func TestPlacementMaxNodesRejectsInvalidValuesOnImport(t *testing.T) {
	const valid = `models:
- model_id: org/model
  specs:
  - name: bf16-balanced
    engine: vllm
    image: vllm/vllm-openai:v0.25.1
    weights: {quantization: bf16, sources: [{huggingface: Org/Model}]}
    gpus: {vendor: nvidia}
    placement:
      language: starlark
      resolver: |
        def resolve(gpu):
            return {}
      max_nodes: 0
`
	if _, err := ParseYAML([]byte(valid)); err != nil {
		t.Fatalf("valid fixture: %v", err)
	}
	for _, tc := range []struct{ value, detail string }{
		{"-1", "must be nonnegative"},
		{"-0.5", "must be an integer"},
		{"0.5", "must be an integer"},
		{"1.9", "must be an integer"},
		{`"1"`, "must be an integer"},
	} {
		t.Run(tc.value, func(t *testing.T) {
			invalid := strings.Replace(valid, "max_nodes: 0", "max_nodes: "+tc.value, 1)
			if _, err := ParseYAML([]byte(invalid)); err == nil || !strings.Contains(err.Error(), "placement.max_nodes "+tc.detail) {
				t.Fatalf("import with max_nodes: %s = %v, want %s", tc.value, err, tc.detail)
			}
		})
	}
}

func TestPlacementMaxNodesEditorSchema(t *testing.T) {
	model := items(schemaProperties(JSONSchema())["models"].(map[string]any))
	spec := items(schemaProperties(model)["specs"].(map[string]any))
	placement := schemaProperties(spec)["placement"].(map[string]any)
	field, ok := schemaProperties(placement)["max_nodes"].(map[string]any)
	if !ok || field["type"] != "integer" || field["minimum"] != 0 {
		t.Fatalf("max_nodes schema = %#v, want integer with minimum 0", field)
	}
	for _, key := range placement["required"].([]string) {
		if key == "max_nodes" {
			t.Fatal("max_nodes must be optional")
		}
	}
}
