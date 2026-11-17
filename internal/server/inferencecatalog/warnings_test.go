package inferencecatalog

import (
	"reflect"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/inferenceapi"
	"gpuworks.ai/gpuworks/internal/server/messages"
)

func warningCodes(warnings []*messages.ProductError) []string {
	codes := []string{}
	for _, warning := range warnings {
		codes = append(codes, warning.Code())
	}
	return codes
}

func TestCatalogWarningsForCallableSpecs(t *testing.T) {
	serve := []string{"--port", "${API_PORT}"}
	chatOps := []APIOperation{{Type: "openai.chat"}}
	for _, test := range []struct {
		name string
		edit func(*InferenceSpec)
		want []string
	}{
		{name: "vllm declares nothing and needs nothing", edit: func(*InferenceSpec) {}, want: []string{}},
		{name: "callable without an api block", edit: func(s *InferenceSpec) { s.Engine, s.Args = "callable", serve },
			want: []string{messages.WarningCatalogCallableMetricsUndeclared, messages.WarningCatalogHealthEndpointUndeclared}},
		{name: "callable with operations only", edit: func(s *InferenceSpec) {
			s.Engine, s.Args = "callable", serve
			s.API = &APIContract{Operations: chatOps}
		}, want: []string{messages.WarningCatalogCallableMetricsUndeclared, messages.WarningCatalogHealthEndpointUndeclared}},
		{name: "callable with metrics, no health", edit: func(s *InferenceSpec) {
			s.Engine, s.Args = "callable", serve
			s.API = &APIContract{Operations: chatOps, Metrics: &inferenceapi.Metrics{Endpoint: "/metrics"}}
		}, want: []string{messages.WarningCatalogHealthEndpointUndeclared}},
		{name: "callable with health, no metrics", edit: func(s *InferenceSpec) {
			s.Engine, s.Args = "callable", serve
			s.API = &APIContract{Operations: chatOps, HealthEndpoint: "/healthz"}
		}, want: []string{messages.WarningCatalogCallableMetricsUndeclared}},
		{name: "callable with both", edit: func(s *InferenceSpec) {
			s.Engine, s.Args = "callable", serve
			s.API = &APIContract{Operations: chatOps, HealthEndpoint: "/healthz", Metrics: &inferenceapi.Metrics{Endpoint: "/metrics"}}
		}, want: []string{}},
	} {
		t.Run(test.name, func(t *testing.T) {
			catalog := flatFixture()
			test.edit(&catalog.Models[0].Specs[0])
			if err := ValidateFlatCatalog(&catalog); err != nil {
				t.Fatal(err)
			}
			warnings := CatalogWarnings(catalog)
			if got := warningCodes(warnings); !reflect.DeepEqual(got, test.want) {
				t.Fatalf("codes=%v, want %v", got, test.want)
			}
			for _, warning := range warnings {
				want := map[string]any{"model_id": catalog.Models[0].ModelID, "spec": "bf16-balanced"}
				if !reflect.DeepEqual(warning.Params(), want) || warning.Error() == "" {
					t.Fatalf("warning %s params=%v error=%q, want params %v and an English fallback", warning.Code(), warning.Params(), warning.Error(), want)
				}
			}
		})
	}
}

func TestCatalogWarningsCoverEverySpecInOrder(t *testing.T) {
	catalog := flatFixture()
	second := catalog.Models[0].Specs[0]
	second.Name = "second"
	catalog.Models[0].Specs[0].Engine = "callable"
	second.Engine = "callable"
	catalog.Models[0].Specs = append(catalog.Models[0].Specs, second)
	var specs []any
	for _, warning := range CatalogWarnings(catalog) {
		specs = append(specs, warning.Params()["spec"])
	}
	if want := []any{"bf16-balanced", "bf16-balanced", "second", "second"}; !reflect.DeepEqual(specs, want) {
		t.Fatalf("specs=%v, want %v", specs, want)
	}
}

// The gateway rewrites the request's model to the engine's own name only for
// vLLM and SGLang. A callable spec that pins --served-model-name to a literal
// answers under a name no deployment is sent, so it is flagged - in args or in
// the entrypoint, in either flag form - while one that places the platform's
// variable, or names no model at all, is not. vLLM and SGLang specs are never
// flagged: their name is rewritten.
func TestCatalogWarningsFlagCallableFixedServedName(t *testing.T) {
	api := &APIContract{Operations: []APIOperation{{Type: "openai.chat"}}, HealthEndpoint: "/health", Metrics: &inferenceapi.Metrics{Endpoint: "/metrics"}}
	for _, test := range []struct {
		name       string
		engine     string
		args       []string
		entrypoint []string
		want       string
	}{
		{name: "literal in args", engine: "callable", args: []string{"--port", "${API_PORT}", "--served-model-name", "glm53"}, want: "glm53"},
		{name: "literal with equals", engine: "callable", args: []string{"--port", "${API_PORT}", "--served-model-name=glm53"}, want: "glm53"},
		{name: "literal in entrypoint", engine: "callable", args: []string{"--port", "${API_PORT}"},
			entrypoint: []string{"vllm", "serve", "--served-model-name", "glm53"}, want: "glm53"},
		{name: "platform variable", engine: "callable", args: []string{"--port", "${API_PORT}", "--served-model-name", "${SERVED_MODEL_NAME}"}},
		{name: "prefixed platform variable", engine: "callable", args: []string{"--port", "${GPUWORKS_API_PORT}", "--served-model-name", "${GPUWORKS_SERVED_MODEL_NAME}"}},
		{name: "no model name", engine: "callable", args: []string{"--port", "${API_PORT}"}},
		{name: "vllm rewrites", engine: "vllm", args: []string{"--served-model-name", "glm53"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			catalog := flatFixture()
			spec := &catalog.Models[0].Specs[0]
			spec.Engine, spec.Args, spec.Entrypoint, spec.API = test.engine, test.args, test.entrypoint, api
			if err := ValidateFlatCatalog(&catalog); err != nil {
				t.Fatal(err)
			}
			warnings := CatalogWarnings(catalog)
			if test.want == "" {
				if len(warnings) != 0 {
					t.Fatalf("codes=%v, want none", warningCodes(warnings))
				}
				return
			}
			if len(warnings) != 1 || warnings[0].Code() != messages.WarningCatalogCallableServedNameFixed {
				t.Fatalf("codes=%v, want [%s]", warningCodes(warnings), messages.WarningCatalogCallableServedNameFixed)
			}
			want := map[string]any{"model_id": catalog.Models[0].ModelID, "spec": "bf16-balanced", "name": test.want}
			if !reflect.DeepEqual(warnings[0].Params(), want) || !strings.Contains(warnings[0].Error(), "${SERVED_MODEL_NAME}") {
				t.Fatalf("params=%v error=%q, want params %v and an English fallback naming the variable", warnings[0].Params(), warnings[0].Error(), want)
			}
		})
	}
}
