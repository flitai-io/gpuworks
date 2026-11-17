package loadgen

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"gpuworks.ai/gpuworks/pkg/bench/metrics"
)

type Request struct {
	Model         string         `json:"model"`
	Messages      []Message      `json:"messages"`
	MaxTokens     int            `json:"max_tokens,omitempty"`
	Stream        bool           `json:"stream"`
	StreamOptions *StreamOptions `json:"stream_options,omitempty"`
	IgnoreEOS     bool           `json:"ignore_eos,omitempty"`
	DelayBefore   time.Duration  `json:"-"`
	FollowUps     []Message      `json:"-"`
	SessionID     string         `json:"-"`
}
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}
type StreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

// ErrModelsEndpointNotFound is a GET /v1/models answered 404: the base URL is
// not the root of an OpenAI-compatible API.
var ErrModelsEndpointNotFound = errors.New("models endpoint not found")

type Client struct {
	HTTP            *http.Client
	BaseURL, APIKey string
	// Header is sent with every request besides the content type and the
	// key, such as the priority class a gpuworks gateway is asked for.
	Header http.Header
	// ObservePriorityClass keeps the class the endpoint says each request ran
	// at; set only for a gpuworks gateway, whose answer the header is.
	ObservePriorityClass bool
}

// PriorityClassHeader names the request priority class a gpuworks gateway
// is asked for; its answer names the class the request ran at.
const PriorityClassHeader = "X-GPUWorks-Priority-Class"

// BenchmarkRunHeader marks a server-origin evaluation's request with its
// run, so a gpuworks gateway records it as an evaluation's.
const BenchmarkRunHeader = "X-GPUWorks-Benchmark-Run"

func (c Client) setHeader(req *http.Request) {
	for name, values := range c.Header {
		for _, v := range values {
			req.Header.Add(name, v)
		}
	}
	if c.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
	}
}

type Model struct {
	ID string `json:"id"`
}

// NormalizeBaseURL accepts the OpenAI-ecosystem habit of pasting an endpoint
// ending in /v1. Callers append concrete API paths themselves, so retaining
// that suffix would otherwise produce /v1/v1/models.
func NormalizeBaseURL(raw string) (string, bool) {
	normalized := strings.TrimRight(strings.TrimSpace(raw), "/")
	strippedV1 := strings.HasSuffix(normalized, "/v1")
	if strippedV1 {
		normalized = strings.TrimSuffix(normalized, "/v1")
	}
	return strings.TrimRight(normalized, "/"), strippedV1
}

func (c Client) Models(ctx context.Context) (models []Model, err error) {
	defer func() { err = c.redactError(err) }()
	if c.HTTP == nil {
		c.HTTP = &http.Client{Timeout: 30 * time.Second}
	}
	baseURL, _ := NormalizeBaseURL(c.BaseURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/v1/models", nil)
	if err != nil {
		return nil, err
	}
	c.setHeader(req)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		if resp.StatusCode == http.StatusNotFound {
			return nil, fmt.Errorf("%w: models endpoint returned %s: %s", ErrModelsEndpointNotFound, resp.Status, string(b))
		}
		return nil, fmt.Errorf("models endpoint returned %s: %s", resp.Status, string(b))
	}
	var body struct {
		Data []Model `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}
	if len(body.Data) == 0 {
		return nil, fmt.Errorf("models endpoint returned no models")
	}
	return body.Data, nil
}

type Result struct {
	generatedText string
	UsageJSON     json.RawMessage
	OutputBudget  int
	Stream        bool
	Metrics       metrics.Request
	Text          string
	RawUsage      Usage
}
type Usage struct {
	PromptTokens        int `json:"prompt_tokens"`
	CompletionTokens    int `json:"completion_tokens"`
	TotalTokens         int `json:"total_tokens"`
	PromptTokensDetails *struct {
		CachedTokens *int `json:"cached_tokens"`
	} `json:"prompt_tokens_details,omitempty"`
	CompletionTokensDetails *struct {
		ReasoningTokens *int `json:"reasoning_tokens"`
	} `json:"completion_tokens_details,omitempty"`
}
type Runner struct {
	Client      Client
	Concurrency int
	// Arrival, when set with a positive rate, dispatches sessions open loop by
	// arrival time; Concurrency is then unused and MaxInFlight (0 = no bound)
	// caps the sessions in flight.
	Arrival     *Arrival
	MaxInFlight int
	OnResult    func(Result)
	OnProgress  func(done, total int)
	// Sleep waits between arrivals and before delayed requests. Nil uses a
	// timer; tests inject a recorder.
	Sleep func(ctx context.Context, d time.Duration) error
	// CountTokens counts local text without a chat template; it is an estimate.
	CountTokens func(string) int
	// DiscardResults makes Run return no results: each one still reaches
	// OnResult, but none is kept. A result carries the whole answer text and
	// per-chunk timings, so keeping every one for a long point grows with the
	// request count when nothing reads them back.
	DiscardResults bool
}

// TotalRequests returns the number of HTTP requests a run will issue. A
// multi-turn sample contributes one initial request plus one request for every
// follow-up turn, so len(requests) is not a correct progress denominator.
func TotalRequests(requests []Request) int {
	total := 0
	for _, req := range requests {
		total += 1 + len(req.FollowUps)
	}
	return total
}

func (c Client) Do(ctx context.Context, req Request) (out Result, err error) {
	out.OutputBudget, out.Stream = req.MaxTokens, req.Stream
	out.Metrics.StartedAt = time.Now().UTC()
	defer func() {
		out.Metrics.FinishedAt = time.Now().UTC()
		applyUsageFallback(&out, req)
		if err == nil {
			err = out.Metrics.TokenCountError()
			if err != nil {
				out.Metrics.ErrorStage = "usage"
			}
		}
		if err != nil {
			err = c.redactError(err)
			out.Metrics.Error = err
			out.Metrics.Canceled = errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) && ctx.Err() != nil
		}
	}()
	if c.HTTP == nil {
		c.HTTP = &http.Client{Timeout: 5 * time.Minute}
	}
	body, err := json.Marshal(req)
	if err != nil {
		out.Metrics.ErrorStage = "encode"
		return out, err
	}
	baseURL, _ := NormalizeBaseURL(c.BaseURL)
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		out.Metrics.ErrorStage = "request"
		return out, err
	}
	hreq.Header.Set("Content-Type", "application/json")
	c.setHeader(hreq)
	out.Metrics.StartedAt = time.Now().UTC()
	resp, err := c.HTTP.Do(hreq)
	if err != nil {
		out.Metrics.ErrorStage = "transport"
		return out, err
	}
	defer resp.Body.Close()
	out.Metrics.HTTPStatus = resp.StatusCode
	if c.ObservePriorityClass {
		out.Metrics.PriorityClass = observedClass(resp.Header.Get(PriorityClassHeader))
	}
	if resp.StatusCode/100 != 2 {
		out.Metrics.ErrorStage = "http"
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return out, fmt.Errorf("endpoint returned %s: %s", resp.Status, string(b))
	}
	if req.Stream {
		parsed, parseErr := parseSSE(resp.Body, out.Metrics.StartedAt)
		parsed.OutputBudget, parsed.Stream, parsed.Metrics.HTTPStatus = req.MaxTokens, req.Stream, resp.StatusCode
		parsed.Metrics.PriorityClass = out.Metrics.PriorityClass
		out = parsed
		if parseErr != nil {
			out.Metrics.ErrorStage = "stream"
		}
		return out, parseErr
	}
	var v struct {
		Choices []struct {
			Message struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
				Reasoning        string `json:"reasoning"`
				Thinking         string `json:"thinking"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage json.RawMessage `json:"usage"`
		Error json.RawMessage `json:"error"`
	}
	if err = json.NewDecoder(resp.Body).Decode(&v); err != nil {
		out.Metrics.ErrorStage = "decode"
		return out, err
	}
	if len(v.Error) > 0 && string(v.Error) != "null" {
		out.Metrics.ErrorStage = "response"
		return out, fmt.Errorf("endpoint error: %s", string(v.Error))
	}
	if len(v.Choices) == 0 {
		out.Metrics.ErrorStage = "response"
		return out, fmt.Errorf("endpoint returned no choices")
	}
	out.Text, out.Metrics.FinishReason = v.Choices[0].Message.Content, v.Choices[0].FinishReason
	message := v.Choices[0].Message
	out.generatedText = reasoningDelta(message.ReasoningContent, message.Reasoning, message.Thinking) + out.Text
	out.Metrics.Latency = time.Since(out.Metrics.StartedAt)
	out.UsageJSON = append(json.RawMessage(nil), v.Usage...)
	applyServerUsage(&out)
	return out, nil
}

func applyUsageFallback(out *Result, req Request) {
	if out.Metrics.InputSource == "" {
		var runes int
		for _, m := range req.Messages {
			runes += len([]rune(m.Content))
		}
		out.Metrics.InputTokens = (runes + 3) / 4
		out.Metrics.InputSource = "character_estimate"
		out.Metrics.ApproximateTokens = true
	}
	if out.Metrics.OutputSource == "" {
		text := out.generatedText
		if text == "" {
			text = out.Text
		}
		out.Metrics.OutputTokens = (len([]rune(text)) + 3) / 4
		out.Metrics.OutputSource = "character_estimate"
		out.Metrics.ApproximateTokens = true
	}
}

// Run issues every request and returns the results in completion order. A
// closed-loop runner keeps Concurrency sessions in flight; with Arrival set it
// dispatches sessions by arrival time instead (open loop), see runOpenLoop.
func (r Runner) Run(ctx context.Context, requests []Request) []Result {
	requests = append([]Request(nil), requests...)
	for i := range requests {
		if requests[i].SessionID == "" {
			requests[i].SessionID = metrics.SessionID(i)
		}
	}
	total := TotalRequests(requests)
	var results []Result
	if !r.DiscardResults {
		results = make([]Result, 0, total)
	}
	var mu sync.Mutex
	done := 0
	record := func(res Result) {
		if r.OnResult != nil {
			r.OnResult(res)
		}
		mu.Lock()
		if !r.DiscardResults {
			results = append(results, res)
		}
		done++
		done := done
		mu.Unlock()
		if r.OnProgress != nil {
			r.OnProgress(done, total)
		}
	}
	if r.Arrival != nil && r.Arrival.Rate > 0 {
		r.runOpenLoop(ctx, requests, record)
	} else {
		r.runClosedLoop(ctx, requests, record)
	}
	return results
}

func (r Runner) runClosedLoop(ctx context.Context, requests []Request, record func(Result)) {
	if r.Concurrency < 1 {
		r.Concurrency = 1
	}
	ch := make(chan Request)
	var wg sync.WaitGroup
	worker := func() {
		defer wg.Done()
		for req := range ch {
			select {
			case <-ctx.Done():
				return
			default:
			}
			if req.DelayBefore > 0 && r.sleep(ctx, req.DelayBefore) != nil {
				return
			}
			r.session(ctx, req, record)
		}
	}
	for i := 0; i < min(r.Concurrency, len(requests)); i++ {
		wg.Add(1)
		go worker()
	}
sendLoop:
	for _, req := range requests {
		select {
		case ch <- req:
		case <-ctx.Done():
			break sendLoop
		}
	}
	close(ch)
	wg.Wait()
}

// runOpenLoop starts one session per request on the arrival schedule: the
// first immediately, each next after a gamma-distributed gap, regardless of
// how many earlier sessions are still running. MaxInFlight > 0 bounds the
// sessions running at once; an arrival over the bound waits for a slot and its
// timings start when it is actually sent, as in vLLM's bench serve.
// DelayBefore is ignored: the arrival process owns session start times.
func (r Runner) runOpenLoop(ctx context.Context, requests []Request, record func(Result)) {
	gaps := r.Arrival.Gaps(len(requests))
	var slots chan struct{}
	if r.MaxInFlight > 0 {
		slots = make(chan struct{}, r.MaxInFlight)
	}
	var wg sync.WaitGroup
	for i, req := range requests {
		if i > 0 && r.sleep(ctx, gaps[i-1]) != nil {
			break
		}
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		go func(req Request) {
			defer wg.Done()
			if slots != nil {
				select {
				case slots <- struct{}{}:
					defer func() { <-slots }()
				case <-ctx.Done():
					return
				}
			}
			r.session(ctx, req, record)
		}(req)
	}
	wg.Wait()
}

// session sends the request and then each follow-up turn, appending the
// previous answer to the conversation. A failed turn ends the session.
func (r Runner) session(ctx context.Context, req Request, record func(Result)) {
	turns := append([]Message(nil), req.FollowUps...)
	req.FollowUps = nil
	for turn := 0; ; turn++ {
		if ctx.Err() != nil {
			return
		}
		res, err := r.Client.Do(ctx, req)
		res.Metrics.SessionID, res.Metrics.Turn, res.Metrics.PlannedTurns = req.SessionID, turn+1, len(turns)+1
		addLocalCounts(&res, req, r.CountTokens)
		if err == nil {
			err = res.Metrics.TokenCountError()
			if err != nil {
				res.Metrics.ErrorStage = "usage"
			}
		}
		if err != nil {
			res.Metrics.Error = err
		}
		record(res)
		if err != nil || turn >= len(turns) {
			return
		}
		req.Messages = append(req.Messages, Message{Role: "assistant", Content: res.Text}, turns[turn])
	}
}

func (r Runner) sleep(ctx context.Context, d time.Duration) error {
	if r.Sleep != nil {
		return r.Sleep(ctx, d)
	}
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// observedClass is a class name as the gateway writes it (lowercase, at most
// 64 bytes); anything longer is cut there.
func observedClass(name string) string {
	if len(name) > 64 {
		return name[:64]
	}
	return name
}
