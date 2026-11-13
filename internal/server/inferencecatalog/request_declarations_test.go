package inferencecatalog

import (
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"gpuworks.ai/gpuworks/internal/server/inferenceapi"
	"gpuworks.ai/gpuworks/pkg/localization"
)

func TestRequestDeclarationYAMLAndStoreRoundTrip(t *testing.T) {
	catalog := flatFixture()
	catalog.Models[0].Specs[0].API = &APIContract{Operations: []APIOperation{{Type: inferenceapi.OperationChat, RequestDefaults: map[string]any{"stream": true, "nested": map[string]any{"number": 0.0}}, Inputs: []inferenceapi.InputField{
		{Name: "reasoning_effort", Kind: "select", Options: []string{"low", "high"}, Default: "low", Required: true, Label: localization.Text{"en": "Reasoning", "zh-CN": "推理"}},
		{Name: "reference", Kind: "image", Encoding: "data_url"},
	}}}}
	raw, err := MarshalYAML(catalog)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseYAML(raw)
	if err != nil {
		t.Fatal(err)
	}
	want := parsed.Models[0].Specs[0].API
	if want.Operations[0].Inputs[0].Label["zh-CN"] != "推理" {
		t.Fatalf("labels=%v", want)
	}
	rows, err := ToFlatStoreModels(parsed)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := FromFlatStoreModel(rows[0])
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(restored.Specs[0].API, want) {
		t.Fatalf("stored declaration changed: got=%+v want=%+v", restored.Specs[0].API, want)
	}
	second, err := MarshalYAML(parsed)
	if err != nil || string(raw) != string(second) {
		t.Fatalf("unstable YAML: %v\n%s\n%s", err, raw, second)
	}
}

func TestRequestDeclarationErrorsIdentifyModelSpecOperationAndField(t *testing.T) {
	for _, tc := range []struct{ declaration, field string }{
		{"request_defaults: []", "request_defaults"}, {"request_defaults: false", "request_defaults"},
		{"inputs: [{name: extra, kind: unknown}]", "extra"},
		{"inputs: [{name: extra, kind: text, encoding: data_url}]", "extra"},
	} {
		catalog := flatFixture()
		catalog.Models[0].Specs[0].API = &APIContract{Operations: []APIOperation{{Type: inferenceapi.OperationChat}}}
		raw, err := yaml.Marshal(catalog.Models[0])
		if err != nil {
			t.Fatal(err)
		}
		modelYAML := string(raw)
		for _, line := range strings.Split(modelYAML, "\n") {
			if strings.Contains(line, "type: openai.chat") {
				modelYAML = strings.Replace(modelYAML, line, line+"\n"+strings.Repeat(" ", strings.Index(line, "type:"))+tc.declaration, 1)
				break
			}
		}
		for _, packaged := range []bool{false, true} {
			var err error
			if packaged {
				var model InferenceModel
				model, err = parseModelYAML([]byte(modelYAML))
				if err == nil {
					err = ValidateFlatCatalog(&FlatCatalog{Models: []InferenceModel{model}})
				}
			} else {
				_, err = ParseYAML([]byte("models:\n- " + strings.ReplaceAll(strings.TrimSuffix(modelYAML, "\n"), "\n", "\n  ") + "\n"))
			}
			for _, coordinate := range []string{"org/model", "bf16-balanced", "openai.chat", tc.field} {
				if err == nil || !strings.Contains(strings.ToLower(err.Error()), coordinate) {
					t.Fatalf("packaged=%v declaration=%q error=%v missing=%s", packaged, tc.declaration, err, coordinate)
				}
			}
		}
	}
}
