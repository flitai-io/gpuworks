package dto

import (
	"gpuworks.ai/gpuworks/internal/server/inferenceapi"
	"gpuworks.ai/gpuworks/pkg/localization"
)

// PlaygroundCatalog is the complete server-owned model and parameter
// dictionary used to render the playground without hard-coded sampling ranges.
type PlaygroundCatalog struct {
	Models     []PlaygroundModel     `json:"models"`
	Parameters []PlaygroundParameter `json:"parameters"`
	// StreamOptions are the stream_options fields the Playground adds to a
	// streamed chat request. Each can be switched off for an endpoint that
	// rejects it.
	StreamOptions []PlaygroundStreamOption `json:"stream_options"`
	HTMLPreview   PlaygroundHTMLPreview    `json:"html_preview"`
}

// PlaygroundHTMLPreview is the policy the HTML preview of a reply runs under,
// derived from the platform security settings.
type PlaygroundHTMLPreview struct {
	// AllowNetwork drops the preview's network restrictions; the sandbox that
	// keeps it away from the console's session stays either way.
	AllowNetwork bool `json:"allow_network"`
}

type PlaygroundModel struct {
	ID              string                `json:"id"`
	Label           string                `json:"label"`
	Operations      []PlaygroundOperation `json:"operations"`
	InputModalities []string              `json:"input_modalities,omitempty"`
	ContextLength   int64                 `json:"context_length,omitempty"`
	Sleeping        bool                  `json:"sleeping"`
	// WakesOnRequest is set on a sleeping model whose sleep a request ends:
	// the gateway holds the request while it wakes. A sleeping model without
	// it (an operator's or a schedule's sleep) answers 503 until woken.
	WakesOnRequest     bool   `json:"wakes_on_request,omitempty"`
	DeploymentID       uint   `json:"deployment_id,omitempty"`
	TemplateID         uint   `json:"template_id,omitempty"`
	TemplateSpecSource string `json:"template_spec_source,omitempty"`
	TemplateSpecName   string `json:"template_spec_name,omitempty"`
	External           bool   `json:"external,omitempty"`
	PublicDataPlane    bool   `json:"public_data_plane"`
	// ExtraParameterPresets are the extra-parameter buttons for this
	// deployment. Which chat-template field stops thinking depends on the
	// model family and the engine, so the server resolves it here instead of
	// letting the page hard-code a family.
	ExtraParameterPresets []PlaygroundExtraPreset `json:"extra_parameter_presets"`
}

// PlaygroundOperation is the resolved catalog operation plus the code-owned
// protocol metadata needed to render and route it without frontend heuristics.
type PlaygroundOperation struct {
	RequestDefaults  map[string]any                `json:"request_defaults,omitempty"`
	Inputs           []inferenceapi.InputField     `json:"inputs"`
	PrimaryInputs    []string                      `json:"primary_inputs"`
	Discovery        []inferenceapi.DiscoveryRoute `json:"discovery"`
	Type             string                        `json:"type"`
	Endpoint         string                        `json:"endpoint"`
	Mode             string                        `json:"mode"`
	RequestEncoding  string                        `json:"request_encoding"`
	RequestEncodings []string                      `json:"request_encodings"`
	Renderer         string                        `json:"renderer"`
	Response         string                        `json:"response"`
	Async            *inferenceapi.AsyncLifecycle  `json:"async,omitempty"`
	Example          *inferenceapi.Example         `json:"example,omitempty"`
	// ExampleInput is the declared working value of a text primary input
	// (inferenceapi.Operation.ExampleInput), which the page starts with.
	ExampleInput map[string]string `json:"example_input,omitempty"`
	// MaxReferenceImages is how many reference images the page may send, as
	// the server resolves it (inferenceapi.Operation.ReferenceImageLimit):
	// at least one for an image edit, absent for every other operation.
	MaxReferenceImages int `json:"max_reference_images,omitempty"`
}

type PlaygroundParameter struct {
	Name    string            `json:"name"`
	Label   localization.Text `json:"label"`
	Kind    string            `json:"kind"`
	Minimum float64           `json:"minimum,omitempty"`
	Maximum float64           `json:"maximum,omitempty"`
	Step    float64           `json:"step,omitempty"`
	Default any               `json:"default"`
}

// PlaygroundStreamOption is one boolean field of stream_options. Requires
// names another option that must be on for this one to take effect.
type PlaygroundStreamOption struct {
	Name        string            `json:"name"`
	Label       localization.Text `json:"label"`
	Description localization.Text `json:"description"`
	Default     bool              `json:"default"`
	Requires    string            `json:"requires,omitempty"`
}

// PlaygroundExtraPreset is one extra-parameters patch the page can apply to a
// request. A preset that only some deployments can honor is served per model:
// with Value and AppliesWhen when this deployment's declared capability makes
// it effective, and without a Value but with Unavailable (the text naming what
// to add by hand instead) when it is not.
type PlaygroundExtraPreset struct {
	Key   string            `json:"key"`
	Label localization.Text `json:"label"`
	// Value is the patch; empty means this deployment cannot honor the preset.
	Value map[string]any `json:"value,omitempty"`
	// AppliesWhen names the declared capability that makes Value effective
	// (the model family whose chat template reads it). Empty for a preset that
	// is a plain example or cannot be honored here.
	AppliesWhen string `json:"applies_when,omitempty"`
	// Unavailable explains the disabled state and names the parameters to add
	// by hand. It is set only when Value is empty.
	Unavailable localization.Text `json:"unavailable,omitempty"`
}
