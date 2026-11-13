package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/server/aihelper"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// AIService is the AI helper as the API sees it (implemented by the app).
type AIService interface {
	Available() bool
	Ask(ctx context.Context, req aihelper.AskRequest) (aihelper.Result, error)
	AskStream(ctx context.Context, req aihelper.AskRequest) (<-chan aihelper.Event, error)
	InstanceFindings(instanceUUID string) ([]aihelper.Finding, error)
	ModelFit(modelID uint) (aihelper.Fit, error)
	// ToolEnv is what the assistant's tools read for a caller other than an
	// ask - the MCP endpoint - reading the platform API through api.
	ToolEnv(api aihelper.APIReader) aihelper.ToolEnv
}

func (a *apiHandlers) aiAvailable(w http.ResponseWriter, r *http.Request) {
	out := dto.AIAvailability{Available: a.d.AI != nil && a.d.AI.Available()}
	if out.Available {
		if cfg, err := a.d.Store.DefaultAIConfig(); err == nil && cfg.ImageInput {
			out.ImageInput = true
			out.Images = &dto.AIImageLimit{PerMessage: aihelper.ImagesPerMessage, PerRequest: aihelper.ImagesPerRequest,
				MaxBytes: aihelper.ImageMaxBytes, MaxSide: aihelper.ImageMaxSide}
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// aiAskMaxBytes bounds an ask's body: every image the request may carry,
// base64-encoded, and the text around them.
const aiAskMaxBytes = aihelper.ImagesPerRequest*aihelper.ImageMaxBytes*4/3 + 1<<20

func (a *apiHandlers) listAIConfigs(w http.ResponseWriter, r *http.Request) {
	cs, err := a.d.Store.ListAIConfigs()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	// Never return the API key: dto.AIConfigView has no field for it.
	out := make([]dto.AIConfigView, 0, len(cs))
	for i := range cs {
		out = append(out, a.aiConfigView(&cs[i]))
	}
	writeJSON(w, http.StatusOK, dto.NewList(out))
}

func (a *apiHandlers) createAIConfig(w http.ResponseWriter, r *http.Request) {
	var req dto.AIConfigRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		writeFieldInvalid(w, "name", "is required")
		return
	}
	if !validateAIConfigName(w, name, "") {
		return
	}
	// Fill base_url/model from a preset if given and not overridden, then
	// store the base address the way an edit does. An address may be left
	// empty here: a preset fills it, and a service stored without one is a
	// configuration the assistant reports as unusable, not a lost write.
	imageInput := false
	if p, ok := aihelper.Presets[req.Preset]; ok {
		if req.BaseURL == "" {
			req.BaseURL = p.BaseURL
		}
		if req.Model == "" {
			req.Model = p.Model
			imageInput = p.ImageInput
		}
	}
	if req.ImageInput != nil {
		imageInput = *req.ImageInput
	}
	proxy, ok := validateAIProxy(w, req.ProxyMode, req.ProxyURL, false)
	if !ok {
		return
	}
	base, ok := validateAIBaseURL(w, req.BaseURL, false)
	if !ok {
		return
	}
	req.BaseURL = base
	if !validateAIColumn(w, "preset", req.Preset, 32) || !validateAIColumn(w, "model", req.Model, 64) {
		return
	}
	encryptedKey, err := store.EncryptAIKey(a.d.AISecretbox, req.APIKey)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	sealedProxy := ""
	if proxy.url != "" {
		if sealedProxy, ok = a.sealAIProxyURL(w, proxy.url); !ok {
			return
		}
	}
	c, err := a.d.Store.CreateAIConfig(&store.AIConfig{
		Name: name, Preset: req.Preset, BaseURL: req.BaseURL, APIKey: encryptedKey,
		Model: req.Model, IsDefault: req.IsDefault,
		ProxyMode: proxy.mode, ProxyURL: sealedProxy, ImageInput: imageInput,
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": c.ID})
}

// deleteAIConfig removes one AI service (DELETE /api/v1/ai/configs/{id}).
// Removing the service the assistant uses hands the mark to another one in the
// same transaction, and the answer names it: the console says which service the
// assistant moved to, instead of the egress notice silently changing host.
func (a *apiHandlers) deleteAIConfig(w http.ResponseWriter, r *http.Request) {
	promoted, err := a.d.Store.DeleteAIConfig(parseUint(chi.URLParam(r, "id")))
	if err != nil {
		writeAIStoreErr(w, err)
		return
	}
	answer := map[string]any{"status": "deleted"}
	if promoted != 0 {
		answer["promoted_id"] = promoted
	}
	writeJSON(w, http.StatusOK, answer)
}

// aiAsk runs a task (dto.AIAskRequest). Each task names its subject by
// reference and the server seeds the answer with that subject's context.
func (a *apiHandlers) aiAsk(w http.ResponseWriter, r *http.Request) {
	if a.d.AI == nil || !a.d.AI.Available() {
		writeProductError(w, http.StatusServiceUnavailable, ErrorAIEndpointNotConfigured, "no AI endpoint configured", nil)
		return
	}
	var body dto.AIAskRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, aiAskMaxBytes)).Decode(&body); err != nil {
		writeJSONBodyError(w, err)
		return
	}
	req := aihelper.AskRequest{
		Task: body.Task, Question: strings.TrimSpace(body.Question), Locale: body.Locale,
		Target: aihelper.Target{InstanceUUID: strings.TrimSpace(body.InstanceUUID), ModelID: body.ModelID, DeploymentID: body.DeploymentID, NodeUUID: strings.TrimSpace(body.NodeUUID)},
		// The tools read the platform API as this user, with their permissions.
		API: a.apiReaderFor(r),
	}
	for _, m := range body.History {
		req.History = append(req.History, aihelper.Message{Role: m.Role, Content: m.Content, Images: m.Images})
	}
	req.Images = body.Images
	if !validateAskImages(w, body) {
		return
	}
	if !aihelper.KnownTask(req.Task) {
		writeProductError(w, http.StatusBadRequest, ErrorAITaskInvalid, "unknown assistant task", map[string]any{"task": req.Task})
		return
	}
	if !req.TaskSubjectSet() {
		writeProductError(w, http.StatusBadRequest, ErrorAITargetRequired, "task "+req.Task+" needs its subject", map[string]any{"task": req.Task})
		return
	}
	if subjects(req.Target) > 1 {
		writeProductError(w, http.StatusBadRequest, ErrorAITargetRequired, "name at most one subject", map[string]any{"task": req.Task})
		return
	}
	asker := ""
	if claims, ok := a.claimsFromRequest(r); ok {
		asker = claims.Username
	}
	if !a.aiAsks.acquire(asker) {
		writeProductError(w, http.StatusTooManyRequests, ErrorAIAskBusy, "too many questions in flight", map[string]any{"limit": aiAsksPerUser})
		return
	}
	defer a.aiAsks.release(asker)
	if strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
		a.aiAskStream(w, r, req)
		return
	}
	res, err := a.d.AI.Ask(r.Context(), req)
	if !writeAskErr(w, err, req) {
		return
	}
	out := dto.AIAnswer{Answer: res.Answer, Steps: make([]dto.AIStep, 0, len(res.Steps)), Calls: res.Calls,
		Usage: dto.AIUsage{PromptTokens: res.Usage.PromptTokens, CompletionTokens: res.Usage.CompletionTokens}}
	for _, s := range res.Steps {
		out.Steps = append(out.Steps, dto.AIStep{Tool: s.Tool, Args: s.Args, MS: s.MS, Error: s.Error})
	}
	writeJSON(w, http.StatusOK, out)
}

// aiStreamPing is how often an idle answer stream says it is alive: under the
// common 60s proxy idle timeout, while a model thinks or a tool runs.
var aiStreamPing = 10 * time.Second

// aiAskStream serves /ai/ask as SSE. What fails before the model is asked
// still answers with a status code; after that every outcome is an event.
func (a *apiHandlers) aiAskStream(w http.ResponseWriter, r *http.Request, req aihelper.AskRequest) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	events, err := a.d.AI.AskStream(r.Context(), req)
	if !writeAskErr(w, err, req) {
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	fl.Flush()
	ping := time.NewTicker(aiStreamPing)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ping.C:
			_, _ = w.Write([]byte(": ping\n\n"))
			fl.Flush()
		case ev, open := <-events:
			if !open {
				return
			}
			writeAIEvent(w, ev, len(req.Images) > 0 || historyImages(req.History) > 0)
			fl.Flush()
		}
	}
}

// subjects counts the subjects a request names.
func subjects(t aihelper.Target) int {
	n := 0
	for _, set := range []bool{t.InstanceUUID != "", t.ModelID != 0, t.DeploymentID != 0, t.NodeUUID != ""} {
		if set {
			n++
		}
	}
	return n
}

func writeAIEvent(w http.ResponseWriter, ev aihelper.Event, withImages bool) {
	switch ev.Kind {
	case aihelper.EventStep:
		status := "running"
		if !ev.Running {
			status = "done"
			if ev.Step.Error != "" {
				status = "error"
			}
		}
		writeSSE(w, "step", dto.AIStreamStep{ID: ev.StepID, Tool: ev.Step.Tool, Args: ev.Step.Args, Status: status, MS: ev.Step.MS, Error: ev.Step.Error})
	case aihelper.EventText, aihelper.EventReasoning:
		writeSSE(w, string(ev.Kind), dto.AIStreamDelta{Delta: ev.Delta})
	case aihelper.EventDone:
		res := ev.Result
		writeSSE(w, "done", dto.AIStreamDone{Calls: res.Calls, Usage: dto.AIUsage{PromptTokens: res.Usage.PromptTokens, CompletionTokens: res.Usage.CompletionTokens},
			FirstTextMS: res.FirstTextMS, TotalMS: res.TotalMS})
	case aihelper.EventError:
		out := dto.AIStreamError{Error: ev.Err.Error()}
		if withImages && aihelper.ImageRefused(ev.Err) {
			out.Code = ErrorAIImageRejected
		}
		writeSSE(w, "error", out)
	}
}

// aiInstanceFindings runs the known-failure table over an instance's logs.
// It needs no LLM: the UI shows it first, and alone when none is configured.
func (a *apiHandlers) aiInstanceFindings(w http.ResponseWriter, r *http.Request) {
	if a.d.AI == nil {
		writeJSON(w, http.StatusOK, dto.NewList[dto.AIFinding](nil))
		return
	}
	fs, err := a.d.AI.InstanceFindings(chi.URLParam(r, "uuid"))
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	out := make([]dto.AIFinding, 0, len(fs))
	for _, f := range fs {
		item := dto.AIFinding{Code: f.Code, Evidence: make([]dto.AIEvidence, 0, len(f.Evidence))}
		for _, e := range f.Evidence {
			item.Evidence = append(item.Evidence, dto.AIEvidence{Line: e.Line})
		}
		out = append(out, item)
	}
	writeJSON(w, http.StatusOK, dto.NewList(out))
}

// aiModelFit is the estimator's fit of a model on the online fleet; it needs
// no LLM either.
func (a *apiHandlers) aiModelFit(w http.ResponseWriter, r *http.Request) {
	id := parseUint(chi.URLParam(r, "id"))
	if a.d.AI == nil || id == 0 {
		writeErr(w, http.StatusNotFound, store.ErrNotFound)
		return
	}
	fit, err := a.d.AI.ModelFit(id)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	out := dto.AIModelFit{ModelID: fit.ModelID, WeightBytes: fit.WeightBytes, Dtype: fit.Dtype, GPUMemUtil: fit.GPUMemUtil, Rows: make([]dto.AIFitRow, 0, len(fit.Rows))}
	for _, row := range fit.Rows {
		out.Rows = append(out.Rows, dto.AIFitRow(row))
	}
	writeJSON(w, http.StatusOK, out)
}

// aiAsksPerUser is how many questions one user may have in flight: each
// runs up to a dozen model turns and tens of API reads, and a stuck tab or
// a script must not multiply that.
const aiAsksPerUser = 2
