package app

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/api"
	"gpuworks.ai/gpuworks/internal/server/messages"
)

func TestAccessLogHandlerLogsOneCompletionRecord(t *testing.T) {
	var logs strings.Builder
	log := slog.New(slog.NewTextHandler(&logs, nil))
	h := accessLogHandler(log, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte("queued"))
	}))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/deployments", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d", rec.Code)
	}
	out := logs.String()
	if strings.Count(out, "msg=\"http request\"") != 1 || strings.Contains(out, "http request start") || strings.Contains(out, "http request complete") {
		t.Fatalf("logs = %q", out)
	}
	if !strings.Contains(out, "request_id=r") || !strings.Contains(out, "status=202") || !strings.Contains(out, "bytes=6") {
		t.Fatalf("completion log = %q", out)
	}
}

func TestAccessLogReturnsTheGeneratedRequestID(t *testing.T) {
	var logs strings.Builder
	var seen string
	h := accessLogHandler(slog.New(slog.NewJSONHandler(&logs, nil)), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get("X-Request-ID")
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/deployments", nil)
	req.Header.Set("X-Request-ID", "untrusted-client-id")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if seen == "" || seen == "untrusted-client-id" || rec.Header().Get("X-Request-ID") != seen || !strings.Contains(logs.String(), `"request_id":"`+seen+`"`) {
		t.Fatalf("response/log IDs differ: request=%q response=%q logs=%s", seen, rec.Header().Get("X-Request-ID"), logs.String())
	}
}

func TestAccessLogHandlerLogsRedactedJSONParamsAndRestoresBody(t *testing.T) {
	var logs strings.Builder
	log := slog.New(slog.NewTextHandler(&logs, nil))
	const body = `{"model_id":12,"engine":"vllm","password":"password-value-123","nested":{"api_key":"api-value-456","HF_TOKEN":"token-value-789"},"env":{"SAFE":"env-value-012"}}`
	h := accessLogHandler(log, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, err := io.ReadAll(r.Body)
		if err != nil || string(got) != body {
			t.Fatalf("body = %q, err=%v", got, err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/evaluate", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(httptest.NewRecorder(), req)
	out := logs.String()
	if !strings.Contains(out, `model_id`) || !strings.Contains(out, `vllm`) || !strings.Contains(out, `[REDACTED]`) {
		t.Fatalf("safe params missing from logs: %q", out)
	}
	for _, secret := range []string{"password-value-123", "api-value-456", "token-value-789", "env-value-012"} {
		if strings.Contains(out, secret) {
			t.Fatalf("secret %q leaked in logs: %q", secret, out)
		}
	}
}

func TestAccessLogHandlerSeparatesStreamLifecycle(t *testing.T) {
	var logs strings.Builder
	log := slog.New(slog.NewTextHandler(&logs, nil))
	h := accessLogHandler(log, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: ok\n\n"))
	}))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/events", nil)
	h.ServeHTTP(httptest.NewRecorder(), req)
	out := logs.String()
	if strings.Count(out, "http stream opened") != 1 || strings.Count(out, "http stream closed") != 1 {
		t.Fatalf("stream logs = %q", out)
	}
}

// readRecorder records how much of a body was read.
type readRecorder struct {
	r    io.Reader
	read int
}

func (b *readRecorder) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	b.read += n
	return n, err
}

// A storage-files upload is never read ahead of its handler nor logged,
// whatever Content-Type the browser gives it (a .json file arrives as
// application/json), and both transfers are logged as streams.
func TestAccessLogHandlerLeavesStorageUploadsUnread(t *testing.T) {
	for _, contentType := range []string{"application/octet-stream", "application/json", ""} {
		var logs strings.Builder
		log := slog.New(slog.NewTextHandler(&logs, nil))
		body := &readRecorder{r: strings.NewReader(`{"secret_dataset":"row-value-345"}`)}
		readBeforeHandler := -1
		h := accessLogHandler(log, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			readBeforeHandler = body.read
			_, _ = io.Copy(io.Discard, r.Body)
			w.WriteHeader(http.StatusOK)
		}))
		req := httptest.NewRequest(http.MethodPut, "/api/v1/storages/3/files/upload?path=files/a.json", body)
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		h.ServeHTTP(httptest.NewRecorder(), req)
		out := logs.String()
		if readBeforeHandler != 0 || strings.Contains(out, "row-value-345") || strings.Contains(out, "params=") {
			t.Fatalf("content type %q: read ahead %d bytes, logs = %q", contentType, readBeforeHandler, out)
		}
		if strings.Count(out, "http stream opened") != 1 || strings.Count(out, "http stream closed") != 1 {
			t.Fatalf("upload logs = %q", out)
		}
	}
	var logs strings.Builder
	h := accessLogHandler(slog.New(slog.NewTextHandler(&logs, nil)), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/v1/storages/3/files/download?path=files/d", nil))
	if strings.Count(logs.String(), "http stream opened") != 1 {
		t.Fatalf("download logs = %q", logs.String())
	}
	logs.Reset()
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/v1/storages/3/files?path=files", nil))
	if strings.Contains(logs.String(), "http stream opened") {
		t.Fatalf("a listing is not a stream: %q", logs.String())
	}
}

// Model traffic is never read ahead or logged: the body is the conversation,
// which the platform does not keep. The record itself, with the request ID
// that finds the gateway's request record, is still written; a management
// request on the same listener keeps its redacted params.
func TestAccessLogHandlerLeavesInferenceBodiesUnread(t *testing.T) {
	for _, path := range []string{"/v1/chat/completions", "/v1/completions", "/v1/embeddings", "/api/v1/playground/v1/chat/completions"} {
		var logs strings.Builder
		body := &readRecorder{r: strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"prompt-text-918"}]}`)}
		readBeforeHandler := -1
		var received string
		h := accessLogHandler(slog.New(slog.NewTextHandler(&logs, nil)), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			readBeforeHandler = body.read
			data, _ := io.ReadAll(r.Body)
			received = string(data)
			w.WriteHeader(http.StatusOK)
		}))
		req := httptest.NewRequest(http.MethodPost, path, body)
		req.Header.Set("Content-Type", "application/json")
		h.ServeHTTP(httptest.NewRecorder(), req)
		out := logs.String()
		if readBeforeHandler != 0 || strings.Contains(out, "prompt-text-918") || strings.Contains(out, "params=") {
			t.Fatalf("%s: read ahead %d bytes, logs = %q", path, readBeforeHandler, out)
		}
		if !strings.Contains(received, "prompt-text-918") {
			t.Fatalf("%s: handler received %q, want the untouched body", path, received)
		}
		if !strings.Contains(out, `msg="http request"`) || !strings.Contains(out, "request_id=r") || !strings.Contains(out, "path="+path) {
			t.Fatalf("%s: the completion record is still written: %q", path, out)
		}
	}
	var logs strings.Builder
	h := accessLogHandler(slog.New(slog.NewTextHandler(&logs, nil)), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/playground/models-search", strings.NewReader(`{"query":"qwen","api_key":"k-123"}`))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(httptest.NewRecorder(), req)
	if out := logs.String(); !strings.Contains(out, "qwen") || strings.Contains(out, "k-123") {
		t.Fatalf("a management request keeps its redacted params: %q", out)
	}
}

// A product error's code and parameters reach the request's access log line
// through every writer the real router wraps around the response, so a
// refusal's reason is in the server log after the client has dismissed it.
func TestAccessLogCarriesTheProductErrorThroughTheRouter(t *testing.T) {
	var logs strings.Builder
	log := slog.New(slog.NewTextHandler(&logs, nil))
	dir := t.TempDir()
	application, err := New(Config{Addr: "127.0.0.1:0", DataDir: dir, DBPath: dir + "/gpuworks.db", AdminUser: "admin", AdminPassword: "gpuworks-test-password-2026"}, log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { application.proxy.Close() })

	logs.Reset()
	rec := httptest.NewRecorder()
	application.http.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/nodes", nil))
	if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), `"code":"AUTHENTICATION_REQUIRED"`) {
		t.Fatalf("response %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(logs.String(), `msg="http request"`) || !strings.Contains(logs.String(), "path=/api/v1/nodes status=401") || !strings.Contains(logs.String(), "code=AUTHENTICATION_REQUIRED") {
		t.Fatalf("access log:\n%s", logs.String())
	}

}

// Parameters ride along as JSON, through a writer that wraps the access
// log's own the way the router's middleware does.
func TestAccessLogCarriesProductErrorParameters(t *testing.T) {
	var logs strings.Builder
	log := slog.New(slog.NewTextHandler(&logs, nil))
	h := accessLogHandler(log, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		api.WriteError(&unwrappingWriter{w}, http.StatusConflict, messages.NewProductError("DEPLOYMENT_NEVER_FITS", "no node fits",
			map[string]any{"pool": 3, "node": "gpu-01", "reason": "no_fit"}))
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/api/v1/deployments", nil))
	if !strings.Contains(logs.String(), `status=409`) || !strings.Contains(logs.String(), `code=DEPLOYMENT_NEVER_FITS error_params="{\"node\":\"gpu-01\",\"pool\":3,\"reason\":\"no_fit\"}"`) {
		t.Fatalf("access log:\n%s", logs.String())
	}
}

type unwrappingWriter struct{ http.ResponseWriter }

func (w *unwrappingWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
