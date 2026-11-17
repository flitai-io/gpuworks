package app

import (
	"context"
	"log/slog"
	"time"

	"gpuworks.ai/gpuworks/internal/server/aihelper"
	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// aiService wires the AI helper to stored config + read-only data access. It is
// also the aihelper.ToolEnv implementation (aitools.go); the context it seeds
// each answer with is assembled in aicontext.go.
type aiService struct {
	outboundAllowPrivateHosts []string
	readPrivateHosts          func() []string
	store                     *store.Store
	obs                       *observability
	box                       *auth.Secretbox
	log                       *slog.Logger
}

func newAIService(st *store.Store, obs *observability, box *auth.Secretbox) *aiService {
	return &aiService{store: st, obs: obs, box: box}
}

// aiAnswerTimeout bounds one answer end to end, every LLM call and tool call
// of the loop included.
const aiAnswerTimeout = 120 * time.Second

// Available reports whether an LLM endpoint is configured.
func (a *aiService) Available() bool { return a.store.HasAIConfig() }

// Ask answers one request: the task's seed, the thread so far, the model's
// tools available throughout (aitasks.go).
func (a *aiService) Ask(ctx context.Context, req aihelper.AskRequest) (aihelper.Result, error) {
	q, err := a.prepare(req)
	if err != nil {
		return aihelper.Result{}, err
	}
	cctx, cancel := context.WithTimeout(ctx, aiAnswerTimeout)
	defer cancel()
	res, err := q.runner.RunMessages(cctx, q.messages, aihelper.DefaultMaxSteps)
	a.logAnswer(req.Task, res, err)
	return res, err
}

// AskStream is Ask streamed. Everything that can fail before the model is
// asked - no endpoint, an unknown subject - fails here, so the caller can
// still answer with a status code; after that every outcome is an event, and
// the channel closes after the done or error event. The run stops when ctx
// does, as when the reader goes away.
func (a *aiService) AskStream(ctx context.Context, req aihelper.AskRequest) (<-chan aihelper.Event, error) {
	q, err := a.prepare(req)
	if err != nil {
		return nil, err
	}
	events := make(chan aihelper.Event, 64)
	send := func(e aihelper.Event) {
		select {
		case events <- e:
		case <-ctx.Done():
		}
	}
	go func() {
		defer close(events)
		cctx, cancel := context.WithTimeout(ctx, aiAnswerTimeout)
		defer cancel()
		res, err := q.runner.RunMessagesStream(cctx, q.messages, aihelper.DefaultMaxSteps, aihelper.Observer{
			Step: func(id int, s aihelper.Step, running bool) {
				send(aihelper.Event{Kind: aihelper.EventStep, StepID: id, Step: s, Running: running})
			},
			Text:      func(d string) { send(aihelper.Event{Kind: aihelper.EventText, Delta: d}) },
			Reasoning: func(d string) { send(aihelper.Event{Kind: aihelper.EventReasoning, Delta: d}) },
		})
		a.logAnswer(req.Task, res, err)
		if err != nil {
			send(aihelper.Event{Kind: aihelper.EventError, Result: res, Err: err})
			return
		}
		send(aihelper.Event{Kind: aihelper.EventDone, Result: res})
	}()
	return events, nil
}

// logAnswer records what one answer cost, in the searchable server log: the
// figures that say whether the assistant is fast enough, without a new store.
func (a *aiService) logAnswer(task string, res aihelper.Result, err error) {
	if a.log == nil {
		return
	}
	attrs := []any{"task", task, "first_text_ms", res.FirstTextMS, "total_ms", res.TotalMS, "llm_calls", res.Calls, "tool_calls", len(res.Steps),
		"prompt_tokens", res.Usage.PromptTokens, "completion_tokens", res.Usage.CompletionTokens}
	if err != nil {
		a.log.Warn("ai answer failed", append(attrs, "err", err)...)
		return
	}
	a.log.Info("ai answer", attrs...)
}

// InstanceFindings is the known failures a failed instance shows: the causes
// its agent reported with the failure, or, for an agent that predates them,
// what the known-failure table finds in its logs. It needs no LLM, so it is
// what the UI shows first - and all it shows when no endpoint is configured.
func (a *aiService) InstanceFindings(instanceUUID string) ([]aihelper.Finding, error) {
	in, err := a.store.InstanceByUUID(instanceUUID)
	if err != nil {
		return nil, err
	}
	if failure := in.Failure(); failure != nil {
		return aihelper.Findings(failure, nil), nil
	}
	logs, err := a.seedLogs(instanceUUID)
	if err != nil {
		return nil, err
	}
	return aihelper.Findings(nil, findingLines(in.Message, logs)), nil
}

// ModelFit is the estimator's fit of a model on the online fleet.
func (a *aiService) ModelFit(modelID uint) (aihelper.Fit, error) {
	m, err := a.store.GetModel(modelID)
	if err != nil {
		return aihelper.Fit{}, err
	}
	groups, err := a.store.OnlineGPUGroups()
	if err != nil {
		return aihelper.Fit{}, err
	}
	layouts, err := a.store.NodeCardLayouts(fitSample(groups))
	if err != nil {
		return aihelper.Fit{}, err
	}
	return fleetFit(m, groups, layouts), nil
}

type staticAIErr string

func (e staticAIErr) Error() string { return string(e) }

const errNoAI = staticAIErr("no AI endpoint configured")
