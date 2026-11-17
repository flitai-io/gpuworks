// Package aihelper is the AI assistant (plan §14): a two-layer design where the
// deterministic estimator produces the numbers and an OpenAI-compatible LLM
// explains/advises over a read-only tool registry. The product is fully usable
// with no LLM configured; AI features simply become unavailable.
package aihelper

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/outbound"
)

// Endpoint is an OpenAI-compatible chat endpoint.
type Endpoint struct {
	BaseURL string
	APIKey  string
	Model   string
}

// Message is a chat message. Images are data: URLs sent with a user message;
// a message with images is written as OpenAI's content parts (MarshalJSON),
// one without as a plain string, which every endpoint accepts.
type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	Images     []string   `json:"-"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	Name       string     `json:"name,omitempty"`
}

// ToolCall is a model-requested tool invocation.
type ToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// ToolDef describes a tool the model may call.
type ToolDef struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
}

// ChatRequest is the OpenAI chat body.
type ChatRequest struct {
	Model    string    `json:"model"`
	Messages []Message `json:"messages"`
	Tools    []ToolDef `json:"tools,omitempty"`
	Stream   bool      `json:"stream"`
}

// ChatResponse is the (non-streaming) OpenAI response subset we use.
type ChatResponse struct {
	Choices []struct {
		Message      Message `json:"message"`
		FinishReason string  `json:"finish_reason"`
	} `json:"choices"`
	Usage Usage `json:"usage"`
}

// Usage is the token accounting an OpenAI-compatible endpoint reports per call.
// An endpoint that reports none leaves it zero.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}

func (u *Usage) add(o Usage) {
	u.PromptTokens += o.PromptTokens
	u.CompletionTokens += o.CompletionTokens
}

// Client calls a chat endpoint.
type Client struct {
	ep   Endpoint
	http *http.Client
	// stream has no client timeout: a streamed answer is bounded by its
	// context, and a fixed timeout would cut a long answer mid-sentence.
	stream *http.Client
}

// NewClient builds a client for an endpoint.
func NewClient(ep Endpoint, allowPrivateHosts ...string) *Client {
	transport := outbound.Transport(func() (proto.NetworkConfig, error) { return proto.NetworkConfig{}, nil }, allowPrivateHosts...)
	return &Client{ep: ep, http: &http.Client{Timeout: 120 * time.Second, Transport: transport}, stream: &http.Client{Transport: transport}}
}

// NewClientWithPrivateHosts checks the live administrator policy on each call,
// including subsequent calls made by an already-running assistant task.
func NewClientWithPrivateHosts(ep Endpoint, readHosts func() []string) *Client {
	return NewClientRouted(ep, readHosts, func() (outbound.Route, error) { return outbound.Route{}, nil })
}

// NewClientRouted is NewClientWithPrivateHosts sending each call the way
// readRoute answers: the service's own proxy choice (ProxyRoute).
func NewClientRouted(ep Endpoint, readHosts func() []string, readRoute func() (outbound.Route, error)) *Client {
	transport := outbound.TransportWithRoute(readRoute, readHosts)
	return &Client{ep: ep, http: &http.Client{Timeout: 120 * time.Second, Transport: transport}, stream: &http.Client{Transport: transport}}
}

// Chat performs one chat completion.
func (c *Client) Chat(ctx context.Context, messages []Message, tools []ToolDef) (Message, Usage, error) {
	body, _ := json.Marshal(ChatRequest{Model: c.ep.Model, Messages: messages, Tools: tools})
	call, err := ChatCompletionsURL(c.ep.BaseURL)
	if err != nil {
		return Message{}, Usage{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, call, bytes.NewReader(body))
	if err != nil {
		return Message{}, Usage{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.ep.APIKey)
	resp, err := c.http.Do(req)
	if err != nil {
		return Message{}, Usage{}, err
	}
	defer c.http.CloseIdleConnections()
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode >= 300 {
		return Message{}, Usage{}, &UpstreamError{Status: resp.StatusCode, Body: string(raw)}
	}
	var cr ChatResponse
	if err := json.Unmarshal(raw, &cr); err != nil {
		return Message{}, Usage{}, err
	}
	if len(cr.Choices) == 0 {
		return Message{}, cr.Usage, fmt.Errorf("llm returned no choices")
	}
	return cr.Choices[0].Message, cr.Usage, nil
}
