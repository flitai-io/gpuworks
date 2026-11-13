package inferenceapi

import (
	"fmt"
	"path"
	"sort"
	"strings"
)

const (
	OperationChat            = "openai.chat"
	OperationCompletions     = "openai.completions"
	OperationEmbeddings      = "openai.embeddings"
	OperationRerank          = "openai.rerank"
	OperationAudioTranscribe = "openai.audio.transcribe"
	OperationAudioSpeech     = "openai.audio.speech"
	OperationImageGenerate   = "openai.image.generate"
	OperationImageEdit       = "openai.image.edit"
	OperationVideoGenerate   = "openai.video.generate"
	OperationRealtime        = "openai.realtime"
)

const (
	EncodingJSON      = "json"
	EncodingMultipart = "multipart"
	EncodingWebSocket = "websocket"
	ResponseJSON      = "json"
	ResponseSSE       = "sse"
	ResponseAudio     = "audio"
	ResponseImage     = "image"
	ResponseVideo     = "video"
	ResponseWebSocket = "websocket"
)

var operationOrder = []string{
	OperationChat,
	OperationCompletions,
	OperationEmbeddings,
	OperationRerank,
	OperationAudioTranscribe,
	OperationAudioSpeech,
	OperationImageGenerate,
	OperationImageEdit,
	OperationVideoGenerate,
	OperationRealtime,
}

// AsyncLifecycle describes paths derived from the operation's base endpoint.
// {id} is substituted with the opaque task ID returned by the upstream.
type AsyncLifecycle struct {
	IDField         string   `json:"id_field"`
	StatusField     string   `json:"status_field"`
	SuccessStatuses []string `json:"success_statuses"`
	FailureStatuses []string `json:"failure_statuses"`
	PollMethod      string   `json:"poll_method"`
	PollEndpoint    string   `json:"poll_endpoint"`
	ContentMethod   string   `json:"content_method"`
	ContentEndpoint string   `json:"content_endpoint"`
	CancelMethod    string   `json:"cancel_method"`
	CancelEndpoint  string   `json:"cancel_endpoint"`
}

// Definition is code-owned protocol knowledge. Catalog YAML chooses a
// definition; it cannot inject request conversion code or arbitrary routes.
type DiscoveryRoute struct {
	Name     string `json:"name"`
	Endpoint string `json:"endpoint"`
}

type Definition struct {
	PrimaryInputs    []string         `json:"primary_inputs"`
	Discovery        []DiscoveryRoute `json:"discovery"`
	Type             string           `json:"type"`
	DefaultEndpoint  string           `json:"default_endpoint"`
	AllowedEndpoints []string         `json:"allowed_endpoints"`
	RequestEncodings []string         `json:"request_encodings"`
	Modes            []string         `json:"modes"`
	Renderer         string           `json:"renderer"`
	Response         string           `json:"response"`
	Async            *AsyncLifecycle  `json:"async,omitempty"`
}

var definitions = map[string]Definition{
	OperationChat: {
		PrimaryInputs: []string{"messages"},
		Type:          OperationChat, DefaultEndpoint: "/v1/chat/completions",
		AllowedEndpoints: []string{"/v1/chat/completions"}, RequestEncodings: []string{EncodingJSON},
		Modes: []string{"stream", "sync"}, Renderer: "chat", Response: ResponseSSE,
	},
	// Text completion: a raw prompt continued, for models without a chat
	// template (base models, fixed-format task models such as PLaMo 2
	// Translate) and clients of the legacy OpenAI endpoint.
	OperationCompletions: {
		PrimaryInputs: []string{"prompt"},
		Type:          OperationCompletions, DefaultEndpoint: "/v1/completions",
		AllowedEndpoints: []string{"/v1/completions"}, RequestEncodings: []string{EncodingJSON},
		Modes: []string{"stream", "sync"}, Renderer: "completion", Response: ResponseSSE,
	},
	OperationEmbeddings: {
		PrimaryInputs: []string{"input"},
		Type:          OperationEmbeddings, DefaultEndpoint: "/v1/embeddings",
		AllowedEndpoints: []string{"/v1/embeddings"}, RequestEncodings: []string{EncodingJSON},
		Modes: []string{"sync"}, Renderer: "vector", Response: ResponseJSON,
	},
	OperationRerank: {
		PrimaryInputs: []string{"query", "documents"},
		Type:          OperationRerank, DefaultEndpoint: "/v1/rerank",
		AllowedEndpoints: []string{"/v1/rerank", "/rerank"}, RequestEncodings: []string{EncodingJSON},
		Modes: []string{"sync"}, Renderer: "rerank", Response: ResponseJSON,
	},
	OperationAudioTranscribe: {
		PrimaryInputs: []string{"file"},
		Type:          OperationAudioTranscribe, DefaultEndpoint: "/v1/audio/transcriptions",
		AllowedEndpoints: []string{"/v1/audio/transcriptions"}, RequestEncodings: []string{EncodingMultipart},
		Modes: []string{"sync"}, Renderer: "audio", Response: ResponseJSON,
	},
	OperationAudioSpeech: {
		PrimaryInputs: []string{"input"},
		Type:          OperationAudioSpeech, DefaultEndpoint: "/v1/audio/speech",
		// vllm-omni serves text-to-audio generation models (Stable Audio) on
		// /v1/audio/generate with the same JSON request and audio response;
		// their /v1/audio/speech route ignores the clip length.
		AllowedEndpoints: []string{"/v1/audio/speech", "/v1/audio/generate"}, RequestEncodings: []string{EncodingJSON},
		Modes: []string{"sync", "stream"}, Renderer: "audio", Response: ResponseAudio,
	},
	OperationImageGenerate: {
		PrimaryInputs: []string{"prompt"},
		Type:          OperationImageGenerate, DefaultEndpoint: "/v1/images/generations",
		AllowedEndpoints: []string{"/v1/images/generations"}, RequestEncodings: []string{EncodingJSON, EncodingMultipart},
		Modes: []string{"sync"}, Renderer: "image", Response: ResponseImage,
	},
	OperationImageEdit: {
		PrimaryInputs: []string{"prompt", "image"},
		Type:          OperationImageEdit, DefaultEndpoint: "/v1/images/edits",
		AllowedEndpoints: []string{"/v1/images/edits"}, RequestEncodings: []string{EncodingMultipart},
		Modes: []string{"sync"}, Renderer: "image", Response: ResponseImage,
	},
	OperationVideoGenerate: {
		PrimaryInputs: []string{"prompt"},
		Type:          OperationVideoGenerate, DefaultEndpoint: "/v1/videos",
		AllowedEndpoints: []string{"/v1/videos", "/v1/videos/sync"}, RequestEncodings: []string{EncodingJSON, EncodingMultipart},
		Modes: []string{"async", "sync"}, Renderer: "video", Response: ResponseVideo,
		Async: &AsyncLifecycle{
			IDField: "id", StatusField: "status", SuccessStatuses: []string{"completed"},
			FailureStatuses: []string{"failed", "cancelled", "deleted"},
			PollMethod:      "GET", PollEndpoint: "{endpoint}/{id}",
			ContentMethod: "GET", ContentEndpoint: "{endpoint}/{id}/content",
			CancelMethod: "DELETE", CancelEndpoint: "{endpoint}/{id}",
		},
	},
	OperationRealtime: {
		PrimaryInputs: []string{},
		Type:          OperationRealtime, DefaultEndpoint: "/v1/realtime",
		AllowedEndpoints: []string{"/v1/realtime"}, RequestEncodings: []string{EncodingWebSocket},
		Modes: []string{"realtime"}, Renderer: "realtime", Response: ResponseWebSocket,
	},
}

// Lookup returns an immutable copy of one operation definition.
func Lookup(operationType string) (Definition, bool) {
	definition, ok := definitions[strings.ToLower(strings.TrimSpace(operationType))]
	if !ok {
		return Definition{}, false
	}
	definition.PrimaryInputs = append([]string{}, definition.PrimaryInputs...)
	definition.Discovery = []DiscoveryRoute{{Name: "models", Endpoint: "/v1/models"}, {Name: "openapi", Endpoint: "/openapi.json"}}
	if operationType == OperationAudioSpeech {
		definition.Discovery = append(definition.Discovery, DiscoveryRoute{Name: "voices", Endpoint: "/v1/audio/voices"})
	}
	definition.AllowedEndpoints = append([]string(nil), definition.AllowedEndpoints...)
	definition.RequestEncodings = append([]string(nil), definition.RequestEncodings...)
	definition.Modes = append([]string(nil), definition.Modes...)
	if definition.Async != nil {
		copy := *definition.Async
		copy.SuccessStatuses = append([]string(nil), copy.SuccessStatuses...)
		copy.FailureStatuses = append([]string(nil), copy.FailureStatuses...)
		definition.Async = &copy
	}
	return definition, true
}

// Definitions returns the complete stable registry in deterministic order.
func Definitions() []Definition {
	result := make([]Definition, 0, len(operationOrder))
	for _, operationType := range operationOrder {
		definition, _ := Lookup(operationType)
		result = append(result, definition)
	}
	return result
}

// NormalizeContract validates and fills the registered endpoint/mode defaults.
func NormalizeContract(contract Contract) (Contract, error) {
	contract.Operations = append([]Operation(nil), contract.Operations...)
	contract.TokenizerRef = strings.TrimSpace(contract.TokenizerRef)
	if contract.TokenizerRef != "" && !catalogTokenizerReference.MatchString(contract.TokenizerRef) {
		return Contract{}, fmt.Errorf("api.tokenizer_ref must name a commit-pinned hf://org/repo@<40-hex-commit> or ms://org/repo@<40-hex-commit> repository")
	}
	contract.HealthEndpoint = strings.TrimSpace(contract.HealthEndpoint)
	if contract.HealthEndpoint != "" && !validEndpoint(contract.HealthEndpoint) {
		return Contract{}, fmt.Errorf("invalid API health_endpoint %q", contract.HealthEndpoint)
	}
	if len(contract.Operations) == 0 {
		return Contract{}, fmt.Errorf("API contract requires at least one operation")
	}
	if contract.Metrics != nil {
		metrics := *contract.Metrics
		if err := normalizeMetrics(&metrics); err != nil {
			return Contract{}, err
		}
		contract.Metrics = &metrics
	}
	if len(contract.RequestStats) > 0 {
		stats, err := normalizeRequestStats(contract.RequestStats)
		if err != nil {
			return Contract{}, err
		}
		contract.RequestStats = stats
	}
	seen := map[string]bool{}
	for index := range contract.Operations {
		operation := &contract.Operations[index]
		operation.Type = strings.ToLower(strings.TrimSpace(operation.Type))
		definition, ok := Lookup(operation.Type)
		if !ok {
			return Contract{}, fmt.Errorf("API operation %d has unsupported type %q", index, operation.Type)
		}
		if seen[operation.Type] {
			return Contract{}, fmt.Errorf("duplicate API operation %q", operation.Type)
		}
		seen[operation.Type] = true
		operation.Endpoint = strings.TrimSpace(operation.Endpoint)
		if operation.Endpoint == "" {
			operation.Endpoint = definition.DefaultEndpoint
		}
		if !contains(definition.AllowedEndpoints, operation.Endpoint) {
			return Contract{}, fmt.Errorf("API operation %s endpoint %q is outside its allowed routes", operation.Type, operation.Endpoint)
		}
		operation.Mode = strings.ToLower(strings.TrimSpace(operation.Mode))
		if operation.Mode == "" {
			operation.Mode = definition.Modes[0]
		}
		if !contains(definition.Modes, operation.Mode) {
			return Contract{}, fmt.Errorf("API operation %s does not support mode %q", operation.Type, operation.Mode)
		}
		operation.RequestEncoding = strings.ToLower(strings.TrimSpace(operation.RequestEncoding))
		if operation.RequestEncoding == "" {
			operation.RequestEncoding = definition.RequestEncodings[0]
		}
		if !contains(definition.RequestEncodings, operation.RequestEncoding) {
			return Contract{}, fmt.Errorf("API operation %s does not support request encoding %q", operation.Type, operation.RequestEncoding)
		}
		if operation.MaxReferenceImages != 0 {
			if operation.Type != OperationImageEdit {
				return Contract{}, fmt.Errorf("API operation %s: max_reference_images is only valid for %s", operation.Type, OperationImageEdit)
			}
			if operation.MaxReferenceImages < 1 {
				return Contract{}, fmt.Errorf("API operation %s: max_reference_images must be a positive count", operation.Type)
			}
		}
		if err := normalizeRequestDeclaration(operation, definition); err != nil {
			return Contract{}, fmt.Errorf("API operation %s: %w", operation.Type, err)
		}
		if operation.Type == OperationVideoGenerate {
			expectedMode := "async"
			if operation.Endpoint == "/v1/videos/sync" {
				expectedMode = "sync"
			}
			if operation.Mode != expectedMode {
				return Contract{}, fmt.Errorf("API operation %s endpoint %q does not support mode %q", operation.Type, operation.Endpoint, operation.Mode)
			}
		}
	}
	return contract, nil
}

// AllowsRequest reports whether a concrete upstream method/path is part of an
// operation, including its registered async lifecycle routes.
func AllowsRequest(operation Operation, method, requestPath string) bool {
	contract, err := NormalizeContract(Contract{Operations: []Operation{operation}})
	if err != nil {
		return false
	}
	operation = contract.Operations[0]
	definition, _ := Lookup(operation.Type)
	method = strings.ToUpper(strings.TrimSpace(method))
	requestPath = path.Clean("/" + strings.TrimPrefix(strings.TrimSpace(requestPath), "/"))
	for _, route := range definition.Discovery {
		if requestPath == route.Endpoint {
			return method == "GET"
		}
	}
	if requestPath == operation.Endpoint {
		if operation.Type == OperationRealtime {
			return method == "GET"
		}
		return method == "POST"
	}
	if definition.Async == nil || operation.Mode != "async" {
		return false
	}
	base := strings.TrimSuffix(operation.Endpoint, "/") + "/"
	if !strings.HasPrefix(requestPath, base) {
		return false
	}
	remainder := strings.TrimPrefix(requestPath, base)
	parts := strings.Split(remainder, "/")
	if len(parts) == 1 && parts[0] != "" {
		return method == definition.Async.PollMethod || method == definition.Async.CancelMethod
	}
	return len(parts) == 2 && parts[0] != "" && parts[1] == "content" && method == definition.Async.ContentMethod
}

// AllowsContractRequest applies route authorization to the complete effective
// contract of a deployment.
func AllowsContractRequest(contract Contract, method, requestPath string) bool {
	for _, operation := range contract.Operations {
		if AllowsRequest(operation, method, requestPath) {
			return true
		}
	}
	return false
}

func validEndpoint(value string) bool {
	return strings.HasPrefix(value, "/") && !strings.Contains(value, "//") && !strings.Contains(value, "..") && !strings.ContainsAny(value, "?#")
}

func contains(values []string, value string) bool {
	copy := sortedCopy(values)
	index := sort.SearchStrings(copy, value)
	return index < len(copy) && copy[index] == value
}

func sortedCopy(values []string) []string {
	copy := append([]string(nil), values...)
	sort.Strings(copy)
	return copy
}
