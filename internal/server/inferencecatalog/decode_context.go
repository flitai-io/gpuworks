package inferencecatalog

import (
	"fmt"

	"gopkg.in/yaml.v3"

	"gpuworks.ai/gpuworks/internal/server/inferenceapi"
)

// Preserve catalog coordinates even when a YAML type error occurs before the
// normal model/spec validator can run. This works for flat imports and packages.
func requestDeclarationDecodeError(data []byte, original error) error {
	var document yaml.Node
	if yaml.Unmarshal(data, &document) != nil || len(document.Content) == 0 {
		return original
	}
	root := document.Content[0]
	models := []*yaml.Node{root}
	if list := yamlMappingValue(root, "models"); list != nil {
		models = list.Content
	}
	for _, model := range models {
		specs := yamlMappingValue(model, "specs")
		if specs == nil {
			continue
		}
		for _, spec := range specs.Content {
			api := yamlMappingValue(spec, "api")
			ops := yamlMappingValue(api, "operations")
			if ops == nil {
				continue
			}
			for _, op := range ops.Content {
				where := fmt.Sprintf("model %s spec %s operation %s", yamlString(model, "model_id"), yamlString(spec, "name"), yamlString(op, "type"))
				if defaults := yamlMappingValue(op, "request_defaults"); defaults != nil && defaults.Kind != yaml.MappingNode && defaults.Tag != "!!null" {
					return fmt.Errorf("%s request_defaults must be a JSON object: %w", where, original)
				}
				var declaration inferenceapi.Operation
				if err := op.Decode(&declaration); err != nil {
					return fmt.Errorf("%s inputs/request_defaults: %w", where, original)
				}
			}
		}
	}
	return original
}

func yamlMappingValue(node *yaml.Node, key string) *yaml.Node {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	for index := 0; index+1 < len(node.Content); index += 2 {
		if node.Content[index].Value == key {
			return node.Content[index+1]
		}
	}
	return nil
}

func yamlString(node *yaml.Node, key string) string {
	if value := yamlMappingValue(node, key); value != nil {
		return value.Value
	}
	return ""
}
