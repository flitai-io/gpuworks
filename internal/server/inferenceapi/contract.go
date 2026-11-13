// Package inferenceapi owns the stable inference operation vocabulary shared
// by the executable catalog, deployment snapshots, proxy and Playground.
package inferenceapi

// Contract describes the HTTP service exposed by one executable inference
// spec. It is deliberately small: protocol details live in the operation
// registry rather than being copied into every catalog entry.
type Contract struct {
	HealthEndpoint string      `yaml:"health_endpoint,omitempty" json:"health_endpoint,omitempty"`
	Operations     []Operation `yaml:"operations" json:"operations"`
	// TokenizerRef names a verified, commit-pinned tokenizer repository when
	// the served weights do not include an independent tokenizer vocabulary.
	// Benchmark auto resolution prefers an available node copy to this source.
	TokenizerRef string `yaml:"tokenizer_ref,omitempty" json:"tokenizer_ref,omitempty"`
	// Metrics is the engine's Prometheus endpoint and mapping (metrics.go).
	// It is observation, not serving: launch never reads it, and a deployment
	// takes it from its template's current spec rather than from its snapshot.
	Metrics *Metrics `yaml:"metrics,omitempty" json:"metrics,omitempty"`
	// RequestStats maps fields an engine writes into each reply onto the
	// platform's request stats (requeststats.go). Like Metrics it is
	// observation only and is read from the template's current spec.
	RequestStats MetricMap `yaml:"request_stats,omitempty" json:"request_stats,omitempty"`
}

// Operation selects one registered protocol and, when needed, overrides its
// standard endpoint or execution mode.
type Operation struct {
	RequestDefaults map[string]any `yaml:"request_defaults,omitempty" json:"request_defaults,omitempty"`
	Inputs          []InputField   `yaml:"inputs,omitempty" json:"inputs"`
	RequestEncoding string         `yaml:"request_encoding,omitempty" json:"request_encoding,omitempty"`
	Type            string         `yaml:"type" json:"type"`
	Endpoint        string         `yaml:"endpoint,omitempty" json:"endpoint,omitempty"`
	Mode            string         `yaml:"mode,omitempty" json:"mode,omitempty"`
	// ExampleInput is a working value for a text primary input (prompt,
	// input, query) of a model that needs a particular format, such as
	// PLaMo 2 Translate's tagged prompt. It only shapes the generated usage
	// example and what the Playground starts with; requests are never
	// changed by it.
	ExampleInput map[string]string `yaml:"example_input,omitempty" json:"example_input,omitempty"`
	// MaxReferenceImages is how many reference images an image edit accepts,
	// each sent as one more "image" part of the multipart request. Only
	// openai.image.edit takes it; unset means one (ReferenceImageLimit).
	MaxReferenceImages int `yaml:"max_reference_images,omitempty" json:"max_reference_images,omitempty"`
}

// ReferenceImageLimit is how many reference images a request of this operation
// may carry: the declared count for an image edit, one when it declares none,
// and zero for every other operation.
func (o Operation) ReferenceImageLimit() int {
	if o.Type != OperationImageEdit {
		return 0
	}
	if o.MaxReferenceImages > 0 {
		return o.MaxReferenceImages
	}
	return 1
}

// DefaultsInput contains catalog facts that may safely select standard engine
// operations when a spec does not need a special API declaration.
type DefaultsInput struct {
	Engine string
}

// EffectiveContract returns an explicit normalized contract, or conservative
// standard operations for ordinary vLLM/SGLang catalog models. Generation
// models must declare their concrete image/video/audio operation because their
// product category alone does not identify an endpoint.
func EffectiveContract(explicit *Contract, input DefaultsInput) (Contract, error) {
	_ = input
	if explicit != nil {
		copy := *explicit
		copy.Operations = append([]Operation(nil), explicit.Operations...)
		return NormalizeContract(copy)
	}

	return Contract{HealthEndpoint: "/health", Operations: []Operation{}}, nil
}

// CheckOperation is the operation the server's health check of a deployment
// with this contract asks (internal/server/healthcheck): chat when it serves
// chat, else text completion, else embeddings; false when it serves none.
func (c Contract) CheckOperation() (Operation, bool) {
	for _, want := range []string{OperationChat, OperationCompletions, OperationEmbeddings} {
		for _, op := range c.Operations {
			if op.Type == want {
				return op, true
			}
		}
	}
	return Operation{}, false
}
