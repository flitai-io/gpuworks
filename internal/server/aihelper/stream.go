package aihelper

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
)

// streamOptions asks an OpenAI-compatible endpoint to report usage in the
// last chunk of a stream; endpoints that do not know the option ignore it.
type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

// streamChunk is one `data:` frame of a streamed chat completion.
type streamChunk struct {
	Choices []struct {
		Delta struct {
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
			Reasoning        string `json:"reasoning"`
			Thinking         string `json:"thinking"`
			ToolCalls        []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *Usage `json:"usage"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// chunkReasoning is a chunk's reasoning text. Engines name the field
// differently (SGLang and older vLLM reasoning_content, vLLM 0.34 reasoning,
// some gateways thinking), and transitional vLLM releases send the same text
// under two names, so the first non-empty one is taken, not their sum.
func chunkReasoning(names ...string) string {
	for _, text := range names {
		if text != "" {
			return text
		}
	}
	return ""
}

// errStreamCut is a stream that ended without a finish reason or [DONE]:
// the connection dropped mid-answer, and what arrived is not the answer.
var errStreamCut = errors.New("llm stream ended before the answer finished")

// maxStreamLine bounds one SSE line; a tool-call argument chunk is small, so
// anything near this is a broken endpoint, not a long answer.
const maxStreamLine = 4 << 20

// ChatStream performs one streamed chat completion. onText and onReasoning
// receive the deltas as they arrive (either may be nil); the returned message
// is the whole turn, tool calls reassembled from their fragments.
func (c *Client) ChatStream(ctx context.Context, messages []Message, tools []ToolDef, onText, onReasoning func(string)) (Message, Usage, error) {
	body, _ := json.Marshal(struct {
		ChatRequest
		StreamOptions streamOptions `json:"stream_options"`
	}{ChatRequest{Model: c.ep.Model, Messages: messages, Tools: tools, Stream: true}, streamOptions{IncludeUsage: true}})
	call, err := ChatCompletionsURL(c.ep.BaseURL)
	if err != nil {
		return Message{}, Usage{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, call, bytes.NewReader(body))
	if err != nil {
		return Message{}, Usage{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Authorization", "Bearer "+c.ep.APIKey)
	resp, err := c.stream.Do(req)
	if err != nil {
		return Message{}, Usage{}, err
	}
	defer c.stream.CloseIdleConnections()
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		return Message{}, Usage{}, &UpstreamError{Status: resp.StatusCode, Body: string(raw)}
	}

	var (
		text     strings.Builder
		usage    Usage
		finished bool
		calls    = map[int]*ToolCall{}
	)
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), maxStreamLine)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data:") {
			continue // blank separators, `: keep-alive` comments, event names
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			finished = true
			break
		}
		var ch streamChunk
		if err := json.Unmarshal([]byte(data), &ch); err != nil {
			return Message{}, usage, fmt.Errorf("llm stream: %w", err)
		}
		if ch.Error != nil {
			return Message{}, usage, fmt.Errorf("llm stream error: %s", ch.Error.Message)
		}
		if ch.Usage != nil {
			usage = *ch.Usage
		}
		for _, choice := range ch.Choices {
			d := choice.Delta
			if reasoning := chunkReasoning(d.ReasoningContent, d.Reasoning, d.Thinking); reasoning != "" && onReasoning != nil {
				onReasoning(reasoning)
			}
			if d.Content != "" {
				text.WriteString(d.Content)
				if onText != nil {
					onText(d.Content)
				}
			}
			for _, tc := range d.ToolCalls {
				call := calls[tc.Index]
				if call == nil {
					call = &ToolCall{Type: "function"}
					calls[tc.Index] = call
				}
				if tc.ID != "" {
					call.ID = tc.ID
				}
				call.Function.Name += tc.Function.Name
				call.Function.Arguments += tc.Function.Arguments
			}
			if choice.FinishReason != nil && *choice.FinishReason != "" {
				finished = true
			}
		}
	}
	if err := sc.Err(); err != nil {
		return Message{}, usage, fmt.Errorf("llm stream: %w", err)
	}
	if !finished {
		return Message{}, usage, errStreamCut
	}
	msg := Message{Role: "assistant", Content: text.String()}
	indexes := make([]int, 0, len(calls))
	for i := range calls {
		indexes = append(indexes, i)
	}
	sort.Ints(indexes)
	for _, i := range indexes {
		msg.ToolCalls = append(msg.ToolCalls, *calls[i])
	}
	return msg, usage, nil
}
