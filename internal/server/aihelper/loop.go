package aihelper

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"
)

// Runner drives the assistant: one agent loop seeded with context the server
// assembled, with the read-only tools available at every step. A seed that is
// enough gets answered in one call; when it is not, the model fetches more
// itself (docs/how-to/use-ai-helper.md).
type Runner struct {
	client *Client
	env    ToolEnv
	tools  []Tool
}

// NewRunner builds a runner.
func NewRunner(client *Client, env ToolEnv) *Runner {
	return &Runner{client: client, env: env, tools: BuiltinTools()}
}

// DefaultMaxSteps is the tool-calling budget of one answer. Reading the
// platform through its API takes a search, a read and a narrower read per
// question; in the gpu3 trials (artifacts/archive/2026-10-11-ai-assistant-api-tools)
// open questions settled within eight rounds.
const DefaultMaxSteps = 10

// maxToolCalls bounds the tool calls of one answer across all its rounds,
// and maxParallelTools how many of one round's calls run at once: each call
// is an API read, and a model that asks for dozens at a time would turn one
// question into a burst of list reads at fleet size.
const (
	maxToolCalls     = 30
	maxParallelTools = 4
)

// wrapUpRounds is how many rounds before the budget runs out the model is
// told to answer as soon as it has enough.
const wrapUpRounds = 2

// maxToolResultChars bounds what one tool call feeds back: a result past it is
// cut and the model is told to narrow the query, rather than a large log or
// metric series crowding out everything else in the window.
const maxToolResultChars = 12000

// Step is one tool call the model made while answering.
type Step struct {
	Tool  string `json:"tool"`
	Args  string `json:"args"`
	MS    int64  `json:"ms"`
	Error string `json:"error,omitempty"`
}

// Result is an answer and how it was reached.
type Result struct {
	Answer string
	Steps  []Step
	Usage  Usage
	// Calls counts LLM round trips.
	Calls int
	// FirstTextMS is when the first answer text arrived (streamed runs; zero
	// when no text came), TotalMS when the run ended - both from its start.
	FirstTextMS int64
	TotalMS     int64
}

// EventKind names what an Event carries.
type EventKind string

const (
	EventStep      EventKind = "step"
	EventText      EventKind = "text"
	EventReasoning EventKind = "reasoning"
	EventDone      EventKind = "done"
	EventError     EventKind = "error"
)

// Event is one thing that happened while an answer streamed. A stream ends
// with exactly one done or error event; both carry the Result so far.
type Event struct {
	Kind    EventKind
	StepID  int
	Step    Step
	Running bool
	Delta   string
	Result  Result
	Err     error
}

// Observer receives an answer as it forms. Step is called when a tool call
// starts (Running true, MS zero) and when it ends; one turn's tool calls run
// concurrently, so Step must be safe to call from several goroutines. Any
// field may be nil.
type Observer struct {
	Step      func(id int, s Step, running bool)
	Text      func(delta string)
	Reasoning func(delta string)
}

// Run answers user under system. maxSteps bounds the model's tool calls;
// zero runs a single completion with no tools offered.
func (r *Runner) Run(ctx context.Context, system, user string, maxSteps int) (Result, error) {
	return r.run(ctx, Conversation(system, user, nil, ""), maxSteps, nil)
}

// maxHistory bounds the earlier turns a follow-up carries, and
// maxHistoryChars each turn's text: a follow-up needs the thread, not every
// word of every earlier answer.
const (
	maxHistory      = 10
	maxHistoryChars = 4000
)

// Conversation lays out one ask: the system prompt, the seed the server
// gathered as the first user turn, the earlier turns of the thread (user and
// assistant only, newest kept), then the new question when there is one.
func Conversation(system, seed string, history []Message, question string) []Message {
	msgs := []Message{{Role: "system", Content: system}, {Role: "user", Content: seed}}
	if len(history) > maxHistory {
		history = history[len(history)-maxHistory:]
	}
	for _, h := range history {
		if (h.Role != "user" && h.Role != "assistant") || strings.TrimSpace(h.Content) == "" {
			continue
		}
		msgs = append(msgs, Message{Role: h.Role, Content: clip(h.Content, maxHistoryChars)})
	}
	if q := strings.TrimSpace(question); q != "" {
		msgs = append(msgs, Message{Role: "user", Content: q})
	}
	return msgs
}

// RunMessages is Run over a laid-out conversation (see Conversation).
func (r *Runner) RunMessages(ctx context.Context, messages []Message, maxSteps int) (Result, error) {
	return r.run(ctx, messages, maxSteps, nil)
}

// RunStream is Run with every LLM call streamed: obs sees text and reasoning
// deltas as the endpoint sends them and each tool call as it starts and ends.
func (r *Runner) RunStream(ctx context.Context, system, user string, maxSteps int, obs Observer) (Result, error) {
	return r.RunMessagesStream(ctx, Conversation(system, user, nil, ""), maxSteps, obs)
}

// RunMessagesStream is RunStream over a laid-out conversation.
func (r *Runner) RunMessagesStream(ctx context.Context, messages []Message, maxSteps int, obs Observer) (Result, error) {
	start := time.Now()
	var first time.Duration
	text := obs.Text
	obs.Text = func(delta string) {
		if first == 0 {
			first = max(time.Since(start), time.Millisecond)
		}
		if text != nil {
			text(delta)
		}
	}
	res, err := r.run(ctx, messages, maxSteps, &obs)
	res.FirstTextMS = first.Milliseconds()
	// Both are rounded to milliseconds; a sub-millisecond run must not read
	// as ending before its first text.
	res.TotalMS = max(res.TotalMS, res.FirstTextMS)
	return res, err
}

// chat is one LLM call, streamed when someone is watching.
func (r *Runner) chat(ctx context.Context, messages []Message, tools []ToolDef, obs *Observer) (Message, Usage, error) {
	if obs == nil {
		return r.client.Chat(ctx, messages, tools)
	}
	return r.client.ChatStream(ctx, messages, tools, obs.Text, obs.Reasoning)
}

func (r *Runner) run(ctx context.Context, messages []Message, maxSteps int, obs *Observer) (res Result, err error) {
	start := time.Now()
	defer func() { res.TotalMS = time.Since(start).Milliseconds() }()
	messages = append([]Message(nil), messages...)
	if maxSteps <= 0 {
		msg, usage, err := r.chat(ctx, messages, nil, obs)
		res.Calls, res.Usage = 1, usage
		if err != nil {
			return res, err
		}
		res.Answer = msg.Content
		return res, nil
	}
	defs := make([]ToolDef, len(r.tools))
	byName := map[string]Tool{}
	for i, t := range r.tools {
		defs[i] = t.Def()
		byName[t.Name] = t
	}
	for step := 0; step < maxSteps; step++ {
		msg, usage, err := r.chat(ctx, messages, defs, obs)
		res.Calls++
		res.Usage.add(usage)
		if err != nil {
			return res, err
		}
		messages = append(messages, msg)
		if len(msg.ToolCalls) == 0 {
			res.Answer = msg.Content
			return res, nil
		}
		replies, steps := r.callTools(ctx, byName, msg.ToolCalls, len(res.Steps), obs)
		messages = append(messages, replies...)
		res.Steps = append(res.Steps, steps...)
		if left := maxSteps - step - 1; left == wrapUpRounds {
			messages = append(messages, Message{Role: "user", Content: fmt.Sprintf("You have %d tool rounds left. Answer now if what you have is enough.", left)})
		}
	}
	// Out of steps - ask for a final answer without tools.
	msg, usage, err := r.chat(ctx, append(messages, Message{Role: "user", Content: "Based on the information gathered, give your final answer now."}), nil, obs)
	res.Calls++
	res.Usage.add(usage)
	if err != nil {
		return res, err
	}
	res.Answer = msg.Content
	return res, nil
}

// callTools runs one turn's tool calls concurrently - a model that asks for
// logs and events at once waits for the slower, not the sum - and returns the
// replies in the order the model asked.
func (r *Runner) callTools(ctx context.Context, byName map[string]Tool, calls []ToolCall, firstID int, obs *Observer) ([]Message, []Step) {
	replies := make([]Message, len(calls))
	steps := make([]Step, len(calls))
	var wg sync.WaitGroup
	slots := make(chan struct{}, maxParallelTools)
	for i, tc := range calls {
		wg.Add(1)
		go func(i int, tc ToolCall) {
			defer wg.Done()
			if obs != nil && obs.Step != nil {
				obs.Step(firstID+i, Step{Tool: tc.Function.Name, Args: tc.Function.Arguments}, true)
			}
			slots <- struct{}{}
			defer func() { <-slots }()
			start := time.Now()
			var result, errText string
			if firstID+i < maxToolCalls {
				result, errText = r.callTool(ctx, byName, tc)
			} else {
				errText = fmt.Sprintf("this answer has used its %d tool calls; answer with what you have", maxToolCalls)
				result = "error: " + errText
			}
			steps[i] = Step{Tool: tc.Function.Name, Args: tc.Function.Arguments, MS: time.Since(start).Milliseconds(), Error: errText}
			if obs != nil && obs.Step != nil {
				obs.Step(firstID+i, steps[i], false)
			}
			replies[i] = Message{Role: "tool", ToolCallID: tc.ID, Name: tc.Function.Name, Content: result}
		}(i, tc)
	}
	wg.Wait()
	return replies, steps
}

func (r *Runner) callTool(ctx context.Context, byName map[string]Tool, tc ToolCall) (result, errText string) {
	tool, ok := byName[tc.Function.Name]
	if !ok {
		errText = fmt.Sprintf("unknown tool %q", tc.Function.Name)
		return "error: " + errText, errText
	}
	return tool.Run(ctx, r.env, json.RawMessage(tc.Function.Arguments))
}

// System prompts for the landing points. The estimator provides the numbers;
// the LLM must not do the math, only explain/advise.
const (
	SystemDeployAdvice        = `You are gpuworks' deployment advisor. The platform's deterministic estimator already computed the VRAM breakdown and schedule — do NOT recompute numbers, explain and advise based on them. Be concise, practical, and reference the actual hardware.`
	SystemErrorDiagnosis      = `You are gpuworks' error diagnostician for failed model-serving instances (vLLM, SGLang and similar engines in containers on GPU nodes). The user message carries context the platform already gathered and a manifest of what it left out. Work like an SRE: start from that context. When it already shows the cause, answer directly without calling tools. Call a read-only tool only to settle a specific doubt the context leaves (search the full logs, read events, compare with a healthy replica, check the fit), never to re-read what you were given; prefer one turn with several calls over several turns. Rule-based hypotheses are unverified: confirm or reject each one against the evidence. Answer with the root cause, the evidence lines that show it, and a concrete fix (for example the engine flag to change and its new value). Say plainly when the evidence is not enough to decide. Be specific and concise.`
	SystemModelQA             = `You are gpuworks' model advisor. The user message carries the model's metadata and the platform estimator's fit of that model on each accelerator type in the fleet. Do not recompute or invent numbers; explain the estimator's figures: which card types can serve it, with how many cards and what parallelism, the memory headroom left for KV cache, and quantization or engine caveats. Use the read-only tools only when the given context does not answer the question. Say plainly when information is missing.`
	SystemSchedulingDiagnosis = `You are gpuworks' scheduling diagnostician. The user message carries a deployment whose replicas are not being placed: its GPU need per replica, node requirements (labels), placement strategy, its instances, and the scheduler's latest refusal - a stable code with counts of candidate nodes by status (room, busy, unmet requirements, cordoned, offline) and the closest nodes with what they lack. Explain in plain words why nothing is placed and what would change that (free GPUs on a node by stopping or moving something, relax a requirement, lower the GPU need or the TP, uncordon a node), naming the actual nodes. Use the read-only tools for a node's or a deployment's detail when the refusal does not settle it. Do not invent capacity.`
	SystemNodeHealth          = `You are gpuworks' node health inspector. The user message carries one node: its hardware, each accelerator's health level and the agent's evidence, the node's state and scheduling, the instances on it and its recent warning and error log lines. Say whether the node and each card can serve, what is wrong with any that cannot and how to confirm and fix it (driver, ECC/XID errors, temperature or power, a lost agent connection). Use the read-only tools for metrics (temperature, power, utilization, memory) or logs when the evidence is not enough. Be concrete and short; say plainly when everything looks healthy.`
	// NodeHealthClose follows the shared "read before you answer" paragraph,
	// which would otherwise send this landing back out to re-read the node.
	NodeHealthClose = ` The user message carries hardware health, not a current connection diagnosis. For a connecting/offline node or a connection question, call get_node_connection_diagnostics before attributing the failure. Cite the saved address, relevant stage, vantage and observation time; distinguish current Agent facts, optional SSH checks and historical platform records. Missing SSH, offline or not_checked is unknown, not proof of a broken network. A different public URL alone does not prove the old address is invalid; HTTP/TCP success does not prove WebSocket or token authentication. If diagnostics are unavailable or forbidden, state the evidence gap instead of guessing or repeatedly retrying. Address changes require the operator to confirm a stable address and synchronize platform and Agent configuration; never claim to have repaired it. For hardware, use the seed and only fetch missing temperature, power or throttle readings with its jq.`
	SystemAsk       = `You are gpuworks' assistant for a GPU cluster serving models with vLLM, SGLang and similar engines. Answer the user's question from the platform's live data. When the conversation carries context about a resource, start from it. You cannot change anything: when a change is needed, say exactly what to change and where in the console. Be concise; give the numbers and names you found.`
)

// platformReading is how every landing reads the platform. It names the
// tools and the habits that keep reads small; what each endpoint answers is
// in the tools, not here.
const platformReading = `

You read the platform through read-only tools, as the user who asked: cluster_overview for the whole cluster; search_api to find an endpoint and call_api to read it (lists answer {"items": [...], "total": N} - select the fields you need with jq and keep limit small, then drill in); search_metrics to find a time series, read with call_api GET /api/v1/metrics/query. Read before you answer; if the data does not show something, say so instead of guessing. Times are UTC; now is %s.`

// WithPlatformReading adds how to read the platform, and the current time, to
// a landing's system prompt.
func WithPlatformReading(system string, now time.Time) string {
	return system + fmt.Sprintf(platformReading, now.UTC().Format(time.RFC3339))
}

// WithResponseLanguage adds the shared language contract to every AI landing.
// Button-triggered requests force the UI language. A future free-conversation
// entry can set followUser=true so an explicit language switch in the user's
// message takes precedence over the interface locale.
func WithResponseLanguage(system, locale string, followUser bool) string {
	language := "English"
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(locale)), "zh") {
		language = "Simplified Chinese"
	}
	instruction := "Write everything the user sees in " + language + ", including any note before a tool call."
	if followUser {
		instruction += " If the user writes in a different language, use that language instead."
	}
	return strings.TrimSpace(system) + " " + instruction
}
