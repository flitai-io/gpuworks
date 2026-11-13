package inferenceapi

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"gpuworks.ai/gpuworks/internal/inferencemetrics"
	"gpuworks.ai/gpuworks/internal/server/messages"
)

// Metrics is a template's declaration of its engine's Prometheus endpoint
// (api.metrics). Engines the platform knows (vllm, sglang) need none; a
// template states one to point the platform at another endpoint, to reuse a
// known engine's mapping (an adapter wrapping SGLang) or to map an engine's
// own families onto the platform's inference metrics. Where a mapped family
// also appears in the inherited mapping, the template's entry wins. The
// declaration is resolved into an instance's observation by
// deploy.ResolveObservation (ADR 0039).
type Metrics struct {
	// Endpoint is the path of the metrics endpoint on the API port; empty
	// means /metrics.
	Endpoint string `yaml:"endpoint,omitempty" json:"endpoint,omitempty"`
	// Format is the endpoint's encoding: empty (Prometheus text) or "json",
	// whose map keys are dotted paths of numbers in the document.
	Format string `yaml:"format,omitempty" json:"format,omitempty"`
	// Inherit names an engine whose built-in mapping this one starts from.
	Inherit string `yaml:"inherit,omitempty" json:"inherit,omitempty"`
	// Map is exposition family (optionally with a label selector) to platform
	// metric short name, or to {to, scale}.
	Map MetricMap `yaml:"map,omitempty" json:"map,omitempty"`
}

// MetricMap maps exposition families to platform metrics.
type MetricMap map[string]MetricTarget

// MetricTarget is one mapping target: a platform metric short name and an
// optional scale. It is written as the bare name, or as {to, scale} when it
// scales.
type MetricTarget struct {
	To    string
	Scale float64
}

type metricTargetObject struct {
	To    string  `yaml:"to" json:"to"`
	Scale float64 `yaml:"scale,omitempty" json:"scale,omitempty"`
}

// UnmarshalYAML accepts the bare name or {to, scale}, refusing other keys the
// way the catalog decoder refuses unknown keys everywhere else.
func (t *MetricTarget) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.ScalarNode:
		t.To, t.Scale = node.Value, 0
		return nil
	case yaml.MappingNode:
		for index := 0; index+1 < len(node.Content); index += 2 {
			if key := node.Content[index].Value; key != "to" && key != "scale" {
				return fmt.Errorf("line %d: field %s not found in metric target (want to, scale)", node.Content[index].Line, key)
			}
		}
		var object metricTargetObject
		if err := node.Decode(&object); err != nil {
			return err
		}
		t.To, t.Scale = object.To, object.Scale
		return nil
	default:
		return fmt.Errorf("line %d: a metric target is a platform metric name or {to, scale}", node.Line)
	}
}

// MarshalYAML writes the bare name unless the target scales.
func (t MetricTarget) MarshalYAML() (any, error) {
	if t.Scale == 0 {
		return t.To, nil
	}
	return metricTargetObject(t), nil
}

// UnmarshalJSON accepts the same two shapes as the YAML form.
func (t *MetricTarget) UnmarshalJSON(data []byte) error {
	var name string
	if err := json.Unmarshal(data, &name); err == nil {
		t.To, t.Scale = name, 0
		return nil
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	var object metricTargetObject
	if err := decoder.Decode(&object); err != nil {
		return fmt.Errorf("a metric target is a platform metric name or {to, scale}: %w", err)
	}
	t.To, t.Scale = object.To, object.Scale
	return nil
}

// MarshalJSON writes the bare name unless the target scales.
func (t MetricTarget) MarshalJSON() ([]byte, error) {
	if t.Scale == 0 {
		return json.Marshal(t.To)
	}
	return json.Marshal(metricTargetObject(t))
}

// Rules is the declaration's own mapping, sorted by source.
func (m Metrics) Rules() []inferencemetrics.Rule {
	rules := make([]inferencemetrics.Rule, 0, len(m.Map))
	for source, target := range m.Map {
		rules = append(rules, inferencemetrics.Rule{Source: source, Metric: target.To, Scale: target.Scale})
	}
	sort.Slice(rules, func(i, j int) bool { return rules[i].Source < rules[j].Source })
	return rules
}

// normalizeMetrics trims a declaration and checks it against the vocabulary,
// together with the mapping it inherits: a template entry can only shadow an
// inherited family correctly if the two are valid as one mapping.
func normalizeMetrics(metrics *Metrics) error {
	metrics.Endpoint = strings.TrimSpace(metrics.Endpoint)
	if metrics.Endpoint != "" && !validEndpoint(metrics.Endpoint) {
		return messages.NewProductError(messages.ErrorCatalogMetricsEndpointInvalid,
			fmt.Sprintf("invalid api.metrics endpoint %q", metrics.Endpoint),
			map[string]any{"endpoint": metrics.Endpoint})
	}
	format, ok := inferencemetrics.ParseFormat(metrics.Format)
	if !ok {
		return messages.NewProductError(messages.ErrorCatalogMetricsFormatInvalid,
			fmt.Sprintf("api.metrics format %q is neither prometheus nor json", metrics.Format),
			map[string]any{"format": metrics.Format})
	}
	// Prometheus is the default and is stored as no format, so a contract
	// that never declared one keeps its stored JSON.
	metrics.Format = ""
	if format == inferencemetrics.FormatJSON {
		metrics.Format = string(inferencemetrics.FormatJSON)
	}
	metrics.Inherit = strings.ToLower(strings.TrimSpace(metrics.Inherit))
	if metrics.Inherit != "" && format == inferencemetrics.FormatJSON {
		return messages.NewProductError(messages.ErrorCatalogMetricsJSONInherit,
			fmt.Sprintf("api.metrics inherit %q needs a Prometheus endpoint; a json endpoint maps its own paths", metrics.Inherit),
			map[string]any{"inherit": metrics.Inherit})
	}
	if metrics.Inherit != "" && !inferencemetrics.HasEngineRules(metrics.Inherit) {
		return messages.NewProductError(messages.ErrorCatalogMetricsInheritUnknown,
			fmt.Sprintf("api.metrics inherit %q is not an engine with a built-in metric mapping", metrics.Inherit),
			map[string]any{"inherit": metrics.Inherit})
	}
	if len(metrics.Map) > 0 {
		trimmed := make(MetricMap, len(metrics.Map))
		for source, target := range metrics.Map {
			source = strings.TrimSpace(source)
			target.To = strings.TrimSpace(target.To)
			if _, duplicate := trimmed[source]; duplicate {
				return metricRuleError(&inferencemetrics.RuleError{Problem: inferencemetrics.ProblemDuplicateSource, Source: source, Metric: target.To})
			}
			trimmed[source] = target
		}
		metrics.Map = trimmed
	}
	own := metrics.Rules()
	if format == inferencemetrics.FormatJSON {
		if err := inferencemetrics.ValidateJSONRules(own); err != nil {
			return metricRuleError(err)
		}
		return nil
	}
	if err := inferencemetrics.ValidateRules(own); err != nil {
		return metricRuleError(err)
	}
	if metrics.Inherit != "" {
		if err := inferencemetrics.ValidateRules(inferencemetrics.Overlay(inferencemetrics.EngineRules(metrics.Inherit), own)); err != nil {
			return metricRuleError(err)
		}
	}
	return nil
}

var metricRuleCodes = map[inferencemetrics.RuleProblem]string{
	inferencemetrics.ProblemEmptySource:        messages.ErrorCatalogMetricsSourceInvalid,
	inferencemetrics.ProblemBadSelector:        messages.ErrorCatalogMetricsSourceInvalid,
	inferencemetrics.ProblemUnknownMetric:      messages.ErrorCatalogMetricsUnknownMetric,
	inferencemetrics.ProblemHistogramSelector:  messages.ErrorCatalogMetricsHistogramSelector,
	inferencemetrics.ProblemHistogramScale:     messages.ErrorCatalogMetricsHistogramScale,
	inferencemetrics.ProblemNegativeScale:      messages.ErrorCatalogMetricsNegativeScale,
	inferencemetrics.ProblemShadowedHistogram:  messages.ErrorCatalogMetricsShadowedHistogram,
	inferencemetrics.ProblemDuplicateSource:    messages.ErrorCatalogMetricsDuplicateSource,
	inferencemetrics.ProblemBadJSONPath:        messages.ErrorCatalogMetricsJSONPathInvalid,
	inferencemetrics.ProblemJSONHistogram:      messages.ErrorCatalogMetricsJSONHistogram,
	inferencemetrics.ProblemJSONCompareCounter: messages.ErrorCatalogMetricsJSONCompareCounter,
}

func metricRuleError(err error) error {
	ruleErr, ok := err.(*inferencemetrics.RuleError)
	if !ok {
		return err
	}
	code, known := metricRuleCodes[ruleErr.Problem]
	if !known {
		code = messages.ErrorCatalogMetricsSourceInvalid
	}
	return messages.NewProductError(code, "api.metrics: "+ruleErr.Error(),
		map[string]any{"source": ruleErr.Source, "metric": ruleErr.Metric})
}
