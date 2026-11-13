package inferenceapi

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
)

// ComposeRequest applies the four request layers in their documented order:
// primary input, catalog defaults, declared user inputs, then expert JSON.
// The returned value is a JSON object with model forcibly restored to the
// routed deployment after the expert layer.
func ComposeRequest(operation Operation, model string, primaryInput map[string]any, userInputs map[string]any, rawOverride json.RawMessage) (json.RawMessage, error) {
	contract, err := NormalizeContract(Contract{Operations: []Operation{operation}})
	if err != nil {
		return nil, err
	}
	op := contract.Operations[0]
	body := map[string]any{}
	for key, value := range primaryInput {
		if key == "model" {
			continue
		}
		body[key] = value
	}
	for key, value := range op.RequestDefaults {
		body[key] = value
	}
	declared := map[string]InputField{}
	for _, field := range op.Inputs {
		declared[field.Name] = field
	}
	for key, value := range userInputs {
		field, ok := declared[key]
		if !ok {
			return nil, fmt.Errorf("input %q is not declared by operation %s", key, op.Type)
		}
		if err := validateInputValue(field, value); err != nil {
			return nil, fmt.Errorf("input %s: %w", key, err)
		}
		body[key] = value
	}
	if len(rawOverride) > 0 && string(rawOverride) != "null" {
		var raw map[string]any
		if err := json.Unmarshal(rawOverride, &raw); err != nil {
			return nil, fmt.Errorf("expert request must be a JSON object: %w", err)
		}
		for key, value := range raw {
			body[key] = value
		}
	}
	body["model"] = model
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("compose %s request: %w", op.Type, err)
	}
	return encoded, nil
}

// EncodeMediaInput converts a browser/file byte sequence to the data URL form
// required by a JSON media input declaration.
func EncodeMediaInput(field InputField, mime string, data []byte) (string, error) {
	if field.Encoding != "data_url" || (field.Kind != "audio" && field.Kind != "image" && field.Kind != "file") {
		return "", fmt.Errorf("input %s is not a data_url media field", field.Name)
	}
	if strings.TrimSpace(mime) == "" {
		return "", fmt.Errorf("input %s requires a MIME type", field.Name)
	}
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data), nil
}
