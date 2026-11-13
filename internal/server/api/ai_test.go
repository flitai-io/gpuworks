package api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/aihelper"
	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// fakeAI records what the API forwarded and answers with what a test set.
type fakeAI struct {
	// readPath, when set, is read through the request's APIReader with the
	// context Ask receives - inside the /ai/ask request, as the real
	// service's tools read - and the answer kept in readStatus/readBody.
	readPath   string
	readStatus int
	readBody   string
	readErr    error
	available  bool
	req        aihelper.AskRequest
	target     aihelper.Target
	result     aihelper.Result
	err        error
	findings   []aihelper.Finding
	fit        aihelper.Fit
	// events is what AskStream plays; hold, when set, blocks the stream after
	// them until closed.
	events  []aihelper.Event
	hold    chan struct{}
	stopped chan struct{}
}

func waitClosed(f *fakeAI) <-chan struct{} { return f.stopped }

func (f *fakeAI) Available() bool { return f.available }
func (f *fakeAI) Ask(ctx context.Context, req aihelper.AskRequest) (aihelper.Result, error) {
	f.req, f.target = req, req.Target
	if f.readPath != "" && req.API != nil {
		status, body, err := req.API.Get(ctx, f.readPath, nil)
		f.readStatus, f.readBody, f.readErr = status, string(body), err
	}
	return f.result, f.err
}
func (f *fakeAI) AskStream(ctx context.Context, req aihelper.AskRequest) (<-chan aihelper.Event, error) {
	f.req, f.target = req, req.Target
	if f.err != nil {
		return nil, f.err
	}
	ch := make(chan aihelper.Event, len(f.events))
	for _, e := range f.events {
		ch <- e
	}
	if f.hold == nil {
		close(ch)
		return ch, nil
	}
	go func() {
		select {
		case <-f.hold:
		case <-ctx.Done():
		}
		close(ch)
		if f.stopped != nil {
			close(f.stopped)
		}
	}()
	return ch, nil
}
func (f *fakeAI) InstanceFindings(uuid string) ([]aihelper.Finding, error) {
	f.target.InstanceUUID = uuid
	return f.findings, f.err
}
func (f *fakeAI) ToolEnv(api aihelper.APIReader) aihelper.ToolEnv { return fakeToolEnv{api: api} }

// fakeToolEnv reads the API it is given and answers the two findings empty.
type fakeToolEnv struct{ api aihelper.APIReader }

func (e fakeToolEnv) API() aihelper.APIReader { return e.api }
func (e fakeToolEnv) CompareWithHealthyReplica(string) (any, error) {
	return map[string]any{"healthy_replica": nil}, nil
}
func (e fakeToolEnv) GPUTopology(string) (any, error) { return map[string]any{"gpus": []any{}}, nil }

func (f *fakeAI) ModelFit(id uint) (aihelper.Fit, error) {
	f.target.ModelID = id
	return f.fit, f.err
}

func aiRouter(t *testing.T, ai AIService) (http.Handler, string) {
	t.Helper()
	signer, err := auth.NewTokenSigner("ai-router-test-secret", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	st := routerAuthStore(t)
	token, err := managementSessionTestToken(t, st, signer, auth.Claims{UserID: 1, Username: "admin", Role: "admin"})
	if err != nil {
		t.Fatal(err)
	}
	return Router(Deps{AuthSigner: signer, Store: st, AI: ai}), token
}

func aiDo(t *testing.T, h http.Handler, token, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestAIRoutesRequireJWT(t *testing.T) {
	h, _ := aiRouter(t, &fakeAI{available: true})
	for _, rt := range [][2]string{{"POST", "/api/v1/ai/ask"}, {"GET", "/api/v1/ai/instances/i1/findings"}, {"GET", "/api/v1/ai/models/1/fit"}, {"GET", "/api/v1/ai/available"}} {
		if rec := aiDo(t, h, "", rt[0], rt[1], `{}`); rec.Code != http.StatusUnauthorized {
			t.Errorf("unauthenticated %s %s = %d", rt[0], rt[1], rec.Code)
		}
	}
}

func TestAIAskForwardsReferenceAndReturnsAnswerWire(t *testing.T) {
	ai := &fakeAI{available: true, result: aihelper.Result{
		Answer: "root cause", Calls: 2, Usage: aihelper.Usage{PromptTokens: 3412, CompletionTokens: 286},
		Steps: []aihelper.Step{{Tool: "search_logs", Args: `{"instance_uuid":"i1"}`, MS: 12}, {Tool: "estimate_fit", Args: `{}`, MS: 1, Error: "boom"}},
	}}
	h, token := aiRouter(t, ai)
	rec := aiDo(t, h, token, "POST", "/api/v1/ai/ask", `{"task":"instance.diagnose","instance_uuid":" i1 ","question":"  why? ","locale":"zh","history":[{"role":"user","content":"diagnose"},{"role":"assistant","content":"OOM"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d body %s", rec.Code, rec.Body)
	}
	want := `{"answer":"root cause","steps":[{"tool":"search_logs","args":"{\"instance_uuid\":\"i1\"}","ms":12},{"tool":"estimate_fit","args":"{}","ms":1,"error":"boom"}],"calls":2,"usage":{"prompt_tokens":3412,"completion_tokens":286}}`
	if got := strings.TrimSpace(rec.Body.String()); got != want {
		t.Fatalf("wire =\n%s\nwant\n%s", got, want)
	}
	// The request carries the platform API as the asker reads it: a read
	// through it is authorized with the asker's own token.
	reader := ai.req.API
	if reader == nil {
		t.Fatal("ask forwarded without the asker's API reader")
	}
	if status, body, err := reader.Get(context.Background(), "/api/v1/ai/available", nil); err != nil || status != http.StatusOK {
		t.Fatalf("read as the asker = %d %s %v", status, body, err)
	}
	ai.req.API = nil
	wantReq := aihelper.AskRequest{Task: "instance.diagnose", Target: aihelper.Target{InstanceUUID: "i1"}, Question: "why?", Locale: "zh",
		History: []aihelper.Message{{Role: "user", Content: "diagnose"}, {Role: "assistant", Content: "OOM"}}}
	if !reflect.DeepEqual(ai.req, wantReq) {
		t.Fatalf("forwarded %+v", ai.req)
	}
}

func TestAIAskAnswerWithoutStepsIsAnEmptyList(t *testing.T) {
	h, token := aiRouter(t, &fakeAI{available: true, result: aihelper.Result{Answer: "ok", Calls: 1}})
	rec := aiDo(t, h, token, "POST", "/api/v1/ai/ask", `{"task":"model.fit","model_id":7}`)
	if got := strings.TrimSpace(rec.Body.String()); got != `{"answer":"ok","steps":[],"calls":1,"usage":{"prompt_tokens":0,"completion_tokens":0}}` {
		t.Fatalf("wire = %s", got)
	}
}

func TestAIAskValidatesLandingAndSubject(t *testing.T) {
	h, token := aiRouter(t, &fakeAI{available: true})
	for _, c := range []struct{ body, want string }{
		{`{"task":"chat","question":"x"}`, `{"code":"AI_TASK_INVALID","error":"unknown assistant task","params":{"task":"chat"}}`},
		{`{"task":"instance.diagnose"}`, `{"code":"AI_TARGET_REQUIRED","error":"task instance.diagnose needs its subject","params":{"task":"instance.diagnose"}}`},
		{`{"task":"model.fit","model_id":0}`, `{"code":"AI_TARGET_REQUIRED","error":"task model.fit needs its subject","params":{"task":"model.fit"}}`},
		{`{"task":"deployment.unscheduled","node_uuid":"n1"}`, `{"code":"AI_TARGET_REQUIRED","error":"task deployment.unscheduled needs its subject","params":{"task":"deployment.unscheduled"}}`},
		{`{"task":"node.health"}`, `{"code":"AI_TARGET_REQUIRED","error":"task node.health needs its subject","params":{"task":"node.health"}}`},
		{`{"task":"ask","question":"   "}`, `{"code":"AI_TARGET_REQUIRED","error":"task ask needs its subject","params":{"task":"ask"}}`},
		{`{"task":"ask","question":"x","node_uuid":"n1","deployment_id":3}`, `{"code":"AI_TARGET_REQUIRED","error":"name at most one subject","params":{"task":"ask"}}`},
	} {
		rec := aiDo(t, h, token, "POST", "/api/v1/ai/ask", c.body)
		if rec.Code != http.StatusBadRequest || strings.TrimSpace(rec.Body.String()) != c.want {
			t.Errorf("%s -> %d %s", c.body, rec.Code, rec.Body)
		}
	}
	if rec := aiDo(t, h, token, "POST", "/api/v1/ai/ask", `{not json`); rec.Code != http.StatusBadRequest {
		t.Errorf("malformed body -> %d", rec.Code)
	}
}

func TestAIAskErrors(t *testing.T) {
	for _, c := range []struct {
		name   string
		ai     AIService
		status int
		code   string
	}{
		{"no service", nil, http.StatusServiceUnavailable, `"code":"AI_ENDPOINT_NOT_CONFIGURED"`},
		{"not configured", &fakeAI{available: false}, http.StatusServiceUnavailable, `"code":"AI_ENDPOINT_NOT_CONFIGURED"`},
		{"unknown instance", &fakeAI{available: true, err: store.ErrNotFound}, http.StatusNotFound, `"error":"not found"`},
		{"endpoint failed", &fakeAI{available: true, err: errors.New("llm http 429: rate limited")}, http.StatusBadGateway, `"error":"llm http 429: rate limited"`},
	} {
		h, token := aiRouter(t, c.ai)
		rec := aiDo(t, h, token, "POST", "/api/v1/ai/ask", `{"task":"instance.diagnose","instance_uuid":"i1"}`)
		if rec.Code != c.status || !strings.Contains(rec.Body.String(), c.code) {
			t.Errorf("%s: %d %s", c.name, rec.Code, rec.Body)
		}
	}
	h, token := aiRouter(t, &fakeAI{available: true})
	if rec := aiDo(t, h, token, "GET", "/api/v1/ai/ask", ""); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET /ai/ask = %d, want 405", rec.Code)
	}
}

func TestAIInstanceFindingsWire(t *testing.T) {
	ai := &fakeAI{findings: []aihelper.Finding{{Code: "cuda_oom", Evidence: []aihelper.Evidence{{Index: 4, Line: "CUDA out of memory"}}}}}
	h, token := aiRouter(t, ai)
	rec := aiDo(t, h, token, "GET", "/api/v1/ai/instances/i-42/findings", "")
	want := `{"items":[{"code":"cuda_oom","evidence":[{"line":"CUDA out of memory"}]}],"total":1}`
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != want || ai.target.InstanceUUID != "i-42" {
		t.Fatalf("%d %s (uuid %q)", rec.Code, rec.Body, ai.target.InstanceUUID)
	}
	// Findings need no LLM: they are served whether or not one is configured.
	if ai.available {
		t.Fatal("fixture should be unconfigured")
	}
	h, token = aiRouter(t, &fakeAI{})
	if rec := aiDo(t, h, token, "GET", "/api/v1/ai/instances/i-42/findings", ""); strings.TrimSpace(rec.Body.String()) != `{"items":[],"total":0}` {
		t.Fatalf("no findings wire = %s", rec.Body)
	}
	h, token = aiRouter(t, &fakeAI{err: store.ErrNotFound})
	if rec := aiDo(t, h, token, "GET", "/api/v1/ai/instances/nope/findings", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown instance = %d", rec.Code)
	}
	h, token = aiRouter(t, &fakeAI{err: errors.New("disk")})
	if rec := aiDo(t, h, token, "GET", "/api/v1/ai/instances/x/findings", ""); rec.Code != http.StatusInternalServerError {
		t.Fatalf("store failure = %d", rec.Code)
	}
}

func TestAIModelFitWire(t *testing.T) {
	ai := &fakeAI{fit: aihelper.Fit{ModelID: 7, WeightBytes: 100, Dtype: "bfloat16", GPUMemUtil: 0.9, Rows: []aihelper.FitRow{{
		Vendor: "nvidia", Name: "H100", MemoryBytes: 80, Nodes: 1, Cards: 7, Unusable: 1, MaxCardsPerNode: 7, DtypeSupported: true, FastLinkCards: 4, Fits: true, TP: 1, PerGPUBytes: 60, UsableBytes: 72, HeadroomBytes: 12,
	}, {
		Vendor: "nvidia", Name: "Quadro RTX 8000", MemoryBytes: 48, Nodes: 1, Cards: 1, MaxCardsPerNode: 1, DtypeNote: "turing does not support bfloat16 (use float16)", FastLinkCards: 1, Fits: true, TP: 1, PerGPUBytes: 30, UsableBytes: 43, HeadroomBytes: 13,
	}}}}
	h, token := aiRouter(t, ai)
	rec := aiDo(t, h, token, "GET", "/api/v1/ai/models/7/fit", "")
	want := `{"model_id":7,"weight_bytes":100,"dtype":"bfloat16","gpu_memory_utilization":0.9,"rows":[` +
		`{"vendor":"nvidia","name":"H100","memory_bytes":80,"nodes":1,"cards":7,"unusable":1,"max_cards_per_node":7,"dtype_supported":true,"fast_link_cards":4,"fits":true,"tp":1,"per_gpu_bytes":60,"usable_bytes":72,"headroom_bytes":12},` +
		`{"vendor":"nvidia","name":"Quadro RTX 8000","memory_bytes":48,"nodes":1,"cards":1,"unusable":0,"max_cards_per_node":1,"dtype_supported":false,"dtype_note":"turing does not support bfloat16 (use float16)","fast_link_cards":1,"fits":true,"tp":1,"per_gpu_bytes":30,"usable_bytes":43,"headroom_bytes":13}]}`
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != want || ai.target.ModelID != 7 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	h, token = aiRouter(t, &fakeAI{fit: aihelper.Fit{ModelID: 8}})
	if rec := aiDo(t, h, token, "GET", "/api/v1/ai/models/8/fit", ""); strings.TrimSpace(rec.Body.String()) != `{"model_id":8,"weight_bytes":0,"gpu_memory_utilization":0,"rows":[]}` {
		t.Fatalf("empty fleet wire = %s", rec.Body)
	}
	for _, path := range []string{"/api/v1/ai/models/abc/fit", "/api/v1/ai/models/0/fit"} {
		if rec := aiDo(t, h, token, "GET", path, ""); rec.Code != http.StatusNotFound {
			t.Errorf("%s = %d, want 404", path, rec.Code)
		}
	}
	h, token = aiRouter(t, &fakeAI{err: store.ErrNotFound})
	if rec := aiDo(t, h, token, "GET", "/api/v1/ai/models/9/fit", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown model = %d", rec.Code)
	}
}

func aiStream(t *testing.T, h http.Handler, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "/api/v1/ai/ask", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "text/event-stream")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestAIAskStreamWire(t *testing.T) {
	ai := &fakeAI{available: true, events: []aihelper.Event{
		{Kind: aihelper.EventStep, StepID: 0, Step: aihelper.Step{Tool: "search_logs", Args: `{"q":"x"}`}, Running: true},
		{Kind: aihelper.EventStep, StepID: 0, Step: aihelper.Step{Tool: "search_logs", Args: `{"q":"x"}`, MS: 12}},
		{Kind: aihelper.EventStep, StepID: 1, Step: aihelper.Step{Tool: "estimate_fit", Args: `{}`, MS: 1, Error: "boom"}},
		{Kind: aihelper.EventReasoning, Delta: "hmm"},
		{Kind: aihelper.EventText, Delta: "Root cause"},
		{Kind: aihelper.EventDone, Result: aihelper.Result{Answer: "Root cause", Calls: 2, Usage: aihelper.Usage{PromptTokens: 30, CompletionTokens: 4}, FirstTextMS: 1800, TotalMS: 6900}},
	}}
	h, token := aiRouter(t, ai)
	rec := aiStream(t, h, token, `{"task":"instance.diagnose","instance_uuid":"i1","locale":"zh"}`)
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "text/event-stream" || rec.Header().Get("X-Accel-Buffering") != "no" || rec.Header().Get("Cache-Control") != "no-cache" {
		t.Fatalf("status %d headers %v", rec.Code, rec.Header())
	}
	want := "event: step\ndata: {\"id\":0,\"tool\":\"search_logs\",\"args\":\"{\\\"q\\\":\\\"x\\\"}\",\"status\":\"running\",\"ms\":0}\n\n" +
		"event: step\ndata: {\"id\":0,\"tool\":\"search_logs\",\"args\":\"{\\\"q\\\":\\\"x\\\"}\",\"status\":\"done\",\"ms\":12}\n\n" +
		"event: step\ndata: {\"id\":1,\"tool\":\"estimate_fit\",\"args\":\"{}\",\"status\":\"error\",\"ms\":1,\"error\":\"boom\"}\n\n" +
		"event: reasoning\ndata: {\"delta\":\"hmm\"}\n\n" +
		"event: text\ndata: {\"delta\":\"Root cause\"}\n\n" +
		"event: done\ndata: {\"calls\":2,\"usage\":{\"prompt_tokens\":30,\"completion_tokens\":4},\"first_text_ms\":1800,\"total_ms\":6900}\n\n"
	if got := rec.Body.String(); got != want {
		t.Fatalf("wire =\n%s\nwant\n%s", got, want)
	}
	if ai.target.InstanceUUID != "i1" || ai.req.Locale != "zh" || ai.req.Task != "instance.diagnose" {
		t.Fatalf("forwarded %+v", ai.req)
	}
}

func TestAIAskStreamErrors(t *testing.T) {
	// Before the model is asked: a status code, not a stream.
	h, token := aiRouter(t, &fakeAI{available: true, err: store.ErrNotFound})
	if rec := aiStream(t, h, token, `{"task":"instance.diagnose","instance_uuid":"nope"}`); rec.Code != http.StatusNotFound || rec.Header().Get("Content-Type") == "text/event-stream" {
		t.Fatalf("unknown instance = %d %s", rec.Code, rec.Header().Get("Content-Type"))
	}
	h, token = aiRouter(t, &fakeAI{available: false})
	if rec := aiStream(t, h, token, `{"task":"instance.diagnose","instance_uuid":"i1"}`); rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "AI_ENDPOINT_NOT_CONFIGURED") {
		t.Fatalf("unconfigured = %d %s", rec.Code, rec.Body)
	}
	h, token = aiRouter(t, &fakeAI{available: true})
	if rec := aiStream(t, h, token, `{"task":"instance.diagnose"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("missing subject = %d", rec.Code)
	}
	// After: an error event that ends the stream.
	h, token = aiRouter(t, &fakeAI{available: true, events: []aihelper.Event{
		{Kind: aihelper.EventText, Delta: "half"},
		{Kind: aihelper.EventError, Err: errors.New("llm stream ended before the answer finished")},
	}})
	rec := aiStream(t, h, token, `{"task":"instance.diagnose","instance_uuid":"i1"}`)
	if rec.Code != http.StatusOK || !strings.HasSuffix(rec.Body.String(), "event: error\ndata: {\"error\":\"llm stream ended before the answer finished\"}\n\n") {
		t.Fatalf("mid-stream error wire = %s", rec.Body)
	}
}

// An idle stream pings; a client that leaves ends the handler.
func TestAIAskStreamPingsWhileIdleAndStopsWhenClientLeaves(t *testing.T) {
	old := aiStreamPing
	aiStreamPing = 20 * time.Millisecond
	defer func() { aiStreamPing = old }()
	ai := &fakeAI{available: true, hold: make(chan struct{}), stopped: make(chan struct{})}
	h, token := aiRouter(t, ai)
	srv := httptest.NewServer(h)
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "POST", srv.URL+"/api/v1/ai/ask", strings.NewReader(`{"task":"instance.diagnose","instance_uuid":"i1"}`))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	buf := make([]byte, 64)
	n, err := resp.Body.Read(buf)
	if err != nil || !strings.Contains(string(buf[:n]), ": ping") {
		t.Fatalf("first bytes %q err %v, want a ping", buf[:n], err)
	}
	cancel()
	// The fake's producer sees the request context end and closes.
	select {
	case <-time.After(5 * time.Second):
		t.Fatal("handler kept the stream open after the client left")
	case <-waitClosed(ai):
	}
}

// The tools read from inside the /ai/ask request. Its context carries the
// router's state for /ai/ask; a read must route on its own path, not be
// taken for a sub-route of the ask (which answered the SPA page or a 500).
func TestAIReadsRouteOnTheirOwnFromInsideTheAsk(t *testing.T) {
	ai := &fakeAI{available: true, readPath: "/api/v1/ai/available", result: aihelper.Result{Answer: "ok"}}
	h, token := aiRouter(t, ai)
	rec := aiDo(t, h, token, "POST", "/api/v1/ai/ask", `{"task":"ask","question":"how is the cluster?"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("ask = %d %s", rec.Code, rec.Body)
	}
	if ai.readErr != nil || ai.readStatus != http.StatusOK || strings.TrimSpace(ai.readBody) != `{"available":true,"image_input":false}` {
		t.Fatalf("read inside the ask = %d %q %v", ai.readStatus, ai.readBody, ai.readErr)
	}
}

// A user has at most aiAsksPerUser questions in flight; one more is refused
// with a code the UI translates, and a finished question frees its slot.
func TestAIAskLimitsQuestionsInFlightPerUser(t *testing.T) {
	ai := &fakeAI{available: true, hold: make(chan struct{})}
	h, token := aiRouter(t, ai)
	srv := httptest.NewServer(h)
	defer srv.Close()
	open := func() *http.Response {
		req, _ := http.NewRequest("POST", srv.URL+"/api/v1/ai/ask", strings.NewReader(`{"task":"instance.diagnose","instance_uuid":"i1"}`))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Accept", "text/event-stream")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	var streams []*http.Response
	for i := 0; i < aiAsksPerUser; i++ {
		resp := open()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("question %d: status %d", i, resp.StatusCode)
		}
		streams = append(streams, resp)
	}
	rec := aiDo(t, h, token, "POST", "/api/v1/ai/ask", `{"task":"instance.diagnose","instance_uuid":"i1"}`)
	if rec.Code != http.StatusTooManyRequests || strings.TrimSpace(rec.Body.String()) != `{"code":"AI_ASK_BUSY","error":"too many questions in flight","params":{"limit":2}}` {
		t.Fatalf("over the limit: %d %s", rec.Code, rec.Body.String())
	}
	close(ai.hold)
	for _, resp := range streams {
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	ai.hold = nil
	if rec := aiDo(t, h, token, "POST", "/api/v1/ai/ask", `{"task":"instance.diagnose","instance_uuid":"i1"}`); rec.Code != http.StatusOK {
		t.Fatalf("after the questions ended: %d %s", rec.Code, rec.Body.String())
	}
}
