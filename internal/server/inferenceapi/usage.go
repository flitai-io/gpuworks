package inferenceapi

// Usage is where an operation's response reports what it cost, so the gateway
// can count tokens and time the first one without knowing any protocol of its
// own (ADR 0039). Only engines report usage; nothing here is estimated.
type Usage struct {
	// Prompt and Completion are keys of the response's "usage" object.
	// Completion is empty for an operation that generates no tokens.
	Prompt     string
	Completion string
	// Cached is the key path, inside "usage", of the prompt tokens the engine
	// served from its cache. Empty when the protocol has no such field.
	Cached []string
	// Reasoning are key paths, inside "usage", where engines put the
	// completion tokens spent reasoning; the first present wins (vLLM and
	// TensorFold nest it, SGLang does not).
	Reasoning [][]string
	// StreamText are the keys of a streamed choice's delta whose first
	// non-empty value is the first generated token. Empty for an operation
	// that does not stream text.
	StreamText []string
	// StreamTextInChoice says the StreamText keys are members of the choice
	// itself (text completion: choices[0].text) rather than of its delta.
	StreamTextInChoice bool
	// Format is the reply protocol beyond the usage keys: how a stream's
	// events are told apart and how the reply says it ended.
	Format Format
	// UsageOnRequest says a stream carries usage only when the request asks
	// for it (OpenAI stream_options.include_usage), so the gateway may ask
	// on the client's behalf (ADR 0042).
	UsageOnRequest bool
}

// Format is a reply protocol the gateway reads usage and stream text from.
type Format uint8

const (
	// FormatOpenAI is a chat or text completion: choices with deltas, a
	// finish_reason, usage in the last event.
	FormatOpenAI Format = iota
	// FormatResponses is the OpenAI Responses API: typed events
	// (response.output_text.delta), usage inside the final response object.
	FormatResponses
	// FormatMessages is the Anthropic Messages API: content_block_delta
	// events, the input tokens in message_start and the output tokens in the
	// last message_delta.
	FormatMessages
)

var usages = map[string]Usage{
	OperationChat: {
		Prompt: "prompt_tokens", Completion: "completion_tokens",
		Cached:    []string{"prompt_tokens_details", "cached_tokens"},
		Reasoning: [][]string{{"completion_tokens_details", "reasoning_tokens"}, {"reasoning_tokens"}},
		// reasoning_content (vLLM, SGLang, TensorFold) and reasoning (newer
		// vLLM, OpenRouter) carry a reasoning model's first tokens; a reply
		// that opens with a tool call has no content at all.
		StreamText:     []string{"content", "reasoning_content", "reasoning", "tool_calls"},
		UsageOnRequest: true,
	},
	// Text completion reports usage as chat does; a streamed choice carries
	// its text directly, with no delta.
	OperationCompletions: {
		Prompt: "prompt_tokens", Completion: "completion_tokens",
		Cached:     []string{"prompt_tokens_details", "cached_tokens"},
		StreamText: []string{"text"}, StreamTextInChoice: true,
		UsageOnRequest: true,
	},
	OperationEmbeddings: {Prompt: "prompt_tokens"},
	// A System One decision generates no tokens. Its usage.output_tokens is
	// whatever the engine chooses to count (Intern-Decision counts answered
	// questions), not generated tokens, so only the input is taken.
	OperationDecide: {Prompt: "input_tokens"},
}

// protocolUsages are replies the gateway forwards on paths no operation
// owns: agent clients (Claude Code, Codex) call vLLM's and SGLang's
// Anthropic- and Responses-compatible endpoints directly. Both always report
// usage, streamed or not, so nothing is asked on the client's behalf.
var protocolUsages = map[string]Usage{
	// input_tokens counts the whole prompt here; the cached share is under
	// input_tokens_details (vLLM 0.30, SGLang).
	"/v1/responses": {
		Prompt: "input_tokens", Completion: "output_tokens",
		Cached:    []string{"input_tokens_details", "cached_tokens"},
		Reasoning: [][]string{{"output_tokens_details", "reasoning_tokens"}},
		Format:    FormatResponses,
		// Responses events are typed, not deltas: the proxy reads them by
		// their type (response.reasoning_text.delta), these keys only make
		// the reply a stream.
		StreamText: []string{"delta"},
	},
	// Anthropic usage splits the prompt: input_tokens is the uncached part
	// when cache_read_input_tokens and cache_creation_input_tokens are
	// present (vLLM), and the whole prompt when they are not (SGLang). The
	// proxy adds them up (messagesUsage); Prompt and Cached name the parts.
	"/v1/messages": {
		Prompt: "input_tokens", Completion: "output_tokens",
		Cached:     []string{"cache_read_input_tokens"},
		Format:     FormatMessages,
		StreamText: []string{"text", "thinking", "partial_json"},
	},
}

// usageByEndpoint maps every endpoint an operation with usage may be served
// on to that usage. A deployment's endpoints are confined to its operations'
// allowed endpoints (NormalizeContract), so the request path alone says which
// protocol the response speaks.
var usageByEndpoint = func() map[string]Usage {
	index := map[string]Usage{}
	for operation, usage := range usages {
		for _, endpoint := range definitions[operation].AllowedEndpoints {
			index[endpoint] = usage
		}
	}
	return index
}()

// UsageForPath returns the usage of the operation served at a gateway request
// path (/v1/chat/completions), or false when the operation reports none.
func UsageForPath(path string) (Usage, bool) {
	if usage, ok := usageByEndpoint[path]; ok {
		return usage, true
	}
	usage, ok := protocolUsages[path]
	return usage, ok
}
