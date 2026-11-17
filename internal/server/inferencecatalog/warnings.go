package inferencecatalog

import (
	"gpuworks.ai/gpuworks/internal/engines"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/messages"
	placementeval "gpuworks.ai/gpuworks/internal/server/placement"
)

// CatalogWarnings lists what a catalog that already passed validation still
// leaves its author to check, in model and spec order. A warning never blocks
// an import; the template evaluate endpoint and the import response report
// the same list, and this is the only place that produces it (see
// docs/reference/inference-catalog-spec.md). Every warning is a ProductError
// carrying the stable code, an English fallback, and the model_id and spec
// name it is about.
func CatalogWarnings(catalog FlatCatalog) []*messages.ProductError {
	var warnings []*messages.ProductError
	for _, model := range catalog.Models {
		for _, spec := range model.Specs {
			warnings = append(warnings, specWarnings(model.ModelID, spec)...)
		}
	}
	return warnings
}

func specWarnings(modelID string, spec InferenceSpec) []*messages.ProductError {
	params := func() map[string]any { return map[string]any{"model_id": modelID, "spec": spec.Name} }
	var warnings []*messages.ProductError
	if spec.RetiredDisplayProfile != "" || spec.GPUs.RetiredDisplayTarget != "" {
		warnings = append(warnings, messages.NewProductError(messages.WarningCatalogDisplayProfileRetired,
			"Spec "+spec.Name+" of "+modelID+" sets display_profile or gpus.display_target, which are no longer used and were ignored; name the spec with display_name_zh_cn and display_name_en.",
			params()))
	}
	// A value the parameter dictionary cannot read never blocks a template:
	// the dictionary trails the engines, and the author's recipe is what the
	// engine gets. The token reaches the engine verbatim (deploy.ReverseArgs
	// keeps it in the residual); the platform only cannot show it in the form
	// or use it in the estimate, which is what this warning says.
	for _, problem := range deploy.LintArgs(spec.Engine, spec.Args) {
		warning := params()
		warning["flag"], warning["value"], warning["key"] = problem.Flag, problem.Value, problem.Key
		warnings = append(warnings, messages.NewProductError(messages.WarningCatalogArgPassedThrough,
			"Spec "+spec.Name+" of "+modelID+": "+problem.String()+" in the parameter dictionary, so it is passed to the engine verbatim and the form and the memory estimate do not see it.",
			warning))
	}
	if spec.Engine != engines.Callable {
		return warnings
	}
	if spec.API == nil || spec.API.Metrics == nil {
		warnings = append(warnings, messages.NewProductError(messages.WarningCatalogCallableMetricsUndeclared,
			"Callable spec "+spec.Name+" of "+modelID+" declares no api.metrics, so the monitoring center shows no engine metrics for it unless its /metrics happens to use vLLM or SGLang metric names.",
			params()))
	}
	if spec.API == nil || spec.API.HealthEndpoint == "" {
		warnings = append(warnings, messages.NewProductError(messages.WarningCatalogHealthEndpointUndeclared,
			"Callable spec "+spec.Name+" of "+modelID+" declares no api.health_endpoint, so its readiness is probed on the default /health.",
			params()))
	}
	if name, fixed := fixedServedModelName(spec); fixed {
		warning := params()
		warning["name"] = name
		warnings = append(warnings, messages.NewProductError(messages.WarningCatalogCallableServedNameFixed,
			"Callable spec "+spec.Name+" of "+modelID+" passes --served-model-name "+name+" instead of ${SERVED_MODEL_NAME}; the gateway and the health check send the deployment's served name, which an engine that checks the model rejects unless the two match.",
			warning))
	}
	return warnings
}

// fixedServedModelName finds a --served-model-name the spec's own command
// passes without ${SERVED_MODEL_NAME}. The gateway rewrites the request's
// model to the engine's name only for vLLM and SGLang; a callable engine is
// sent the deployment's served name as is. It stays a warning: an adapter may
// ignore the model, or the author may mean to pin the name.
func fixedServedModelName(spec InferenceSpec) (string, bool) {
	references := func(value string) bool {
		for _, name := range placementeval.References(value) {
			if placementeval.Canonical(name) == placementeval.VarServedModelName {
				return true
			}
		}
		return false
	}
	for _, values := range [][]string{spec.Args, spec.Entrypoint} {
		if name, ok := deploy.LaunchFlag(values, "--served-model-name"); ok && !references(name) {
			return name, true
		}
	}
	return "", false
}
