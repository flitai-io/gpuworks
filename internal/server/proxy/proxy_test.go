package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/inferenceapi"
	"gpuworks.ai/gpuworks/internal/server/store"
)

type readinessTestHub struct{ online map[string]bool }

func (h readinessTestHub) OpenTunnel(context.Context, string, string) (net.Conn, error) {
	return nil, nil
}
func (h readinessTestHub) Online(uuid string) bool { return h.online[uuid] }

func TestExtractModelJSONRestoresBody(t *testing.T) {
	body := `{"model":"qwen","messages":[{"role":"user","content":"hello"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	model, cleanup, err := extractModel(req, DefaultMaxBodyBytes)
	if err != nil || model != "qwen" {
		t.Fatalf("extract model = %q, err=%v", model, err)
	}
	defer cleanup()
	got, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != body {
		t.Fatalf("restored body = %q", got)
	}
}

func TestExtractModelQuerySupportsBodylessAsyncRequestsAndStripsRoutingField(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/v1/videos/job-1/content?model=video-model&variant=preview", nil)
	model, cleanup, err := extractModel(req, DefaultMaxBodyBytes)
	defer cleanup()
	if err != nil || model != "video-model" {
		t.Fatalf("model=%q err=%v", model, err)
	}
	if req.URL.Query().Get("model") != "" || req.URL.Query().Get("variant") != "preview" {
		t.Fatalf("forwarded query=%q", req.URL.RawQuery)
	}
}

func TestExtractModelWebSocketRetainsModelQueryForRealtimeEngine(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/v1/realtime?model=voxtral&intent=transcribe", nil)
	req.Header.Set("Upgrade", "websocket")
	model, cleanup, err := extractModel(req, DefaultMaxBodyBytes)
	defer cleanup()
	if err != nil || model != "voxtral" {
		t.Fatalf("model=%q err=%v", model, err)
	}
	if req.URL.Query().Get("model") != "voxtral" || req.URL.Query().Get("intent") != "transcribe" {
		t.Fatalf("forwarded WebSocket query=%q", req.URL.RawQuery)
	}
}

func TestExtractModelMultipartLargeBodyRestoresBody(t *testing.T) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if err := mw.WriteField("model", "audio-model"); err != nil {
		t.Fatal(err)
	}
	part, err := mw.CreateFormFile("file", "audio.wav")
	if err != nil {
		t.Fatal(err)
	}
	large := bytes.Repeat([]byte("a"), 40<<20)
	if _, err := part.Write(large); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	wantSize := int64(buf.Len())
	model, cleanup, err := extractModel(req, DefaultMaxBodyBytes)
	if err != nil || model != "audio-model" {
		t.Fatalf("extract multipart model = %q, err=%v", model, err)
	}
	spool, ok := req.Body.(*os.File)
	if !ok {
		t.Fatalf("request body = %T, want disk spool", req.Body)
	}
	spoolName := spool.Name()
	if _, err := os.Stat(spoolName); err != nil {
		t.Fatalf("spool not present before cleanup: %v", err)
	}
	if req.ContentLength != wantSize {
		t.Fatalf("content length = %d, want %d", req.ContentLength, wantSize)
	}
	got, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(got)) != wantSize {
		t.Fatalf("restored multipart size = %d, want %d", len(got), wantSize)
	}
	cleanup()
	if _, err := os.Stat(spoolName); !os.IsNotExist(err) {
		t.Fatalf("spool still present after cleanup: %v", err)
	}
}

func TestExtractModelMultipartValidation(t *testing.T) {
	t.Run("oversized model field", func(t *testing.T) {
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		part, err := mw.CreateFormField("model")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := part.Write(bytes.Repeat([]byte("m"), maxMultipartModelBytes+1)); err != nil {
			t.Fatal(err)
		}
		if err := mw.Close(); err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", &buf)
		req.Header.Set("Content-Type", mw.FormDataContentType())
		_, cleanup, err := extractModel(req, DefaultMaxBodyBytes)
		defer cleanup()
		if err == nil || !strings.Contains(err.Error(), "model field exceeds") {
			t.Fatalf("error = %v, want model field limit", err)
		}
	})

	for name, contentType := range map[string]string{
		"missing boundary": "multipart/form-data",
		"malformed header": `multipart/form-data; boundary="unterminated`,
	} {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", strings.NewReader("body"))
			req.Header.Set("Content-Type", contentType)
			_, cleanup, err := extractModel(req, DefaultMaxBodyBytes)
			defer cleanup()
			if err == nil {
				t.Fatalf("content type %q unexpectedly succeeded", contentType)
			}
		})
	}
}

func TestExtractModelSupportsBothOpenAIAudioEndpoints(t *testing.T) {
	for _, path := range []string{"/v1/audio/transcriptions", "/v1/audio/translations"} {
		t.Run(path, func(t *testing.T) {
			var body bytes.Buffer
			mw := multipart.NewWriter(&body)
			if err := mw.WriteField("model", "omni"); err != nil {
				t.Fatal(err)
			}
			if err := mw.Close(); err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequest(http.MethodPost, path, &body)
			req.Header.Set("Content-Type", mw.FormDataContentType())
			model, cleanup, err := extractModel(req, DefaultMaxBodyBytes)
			defer cleanup()
			if err != nil || model != "omni" {
				t.Fatalf("extractModel(%s) = %q, %v", path, model, err)
			}
		})
	}
}

type dialTestHub struct{ addr string }

func (h dialTestHub) OpenTunnel(context.Context, string, string) (net.Conn, error) {
	return net.Dial("tcp", h.addr)
}
func (h dialTestHub) Online(string) bool { return true }

type flushingRecorder struct {
	*httptest.ResponseRecorder
	flushes int
}

func (r *flushingRecorder) Flush() {
	r.flushes++
	r.ResponseRecorder.Flush()
}

func newProxyE2E(t *testing.T, upstream http.Handler) (*Proxy, string, func()) {
	t.Helper()
	upstreamServer := httptest.NewServer(upstream)
	t.Cleanup(upstreamServer.Close)
	st, err := store.Open("file:proxy_omni_e2e_"+strings.ReplaceAll(t.Name(), "/", "_")+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Error(err)
		}
	})
	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "org/omni", Revision: "main", Name: "omni"})
	if err != nil {
		t.Fatal(err)
	}
	node, err := st.RegisterNode("node-omni", store.HashToken("node"), "node-omni")
	if err != nil {
		t.Fatal(err)
	}
	dep, err := st.CreateDeployment(&store.Deployment{ModelID: model.ID, NodeID: node.ID, ServedName: "omni", Engine: "vllm", State: "active"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateInstance(&store.Instance{
		UUID: "omni-instance", DeploymentID: dep.ID, NodeID: node.ID,
		State: proto.InstRunning, APIPort: 30000, Ports: "[30000]",
	}); err != nil {
		t.Fatal(err)
	}
	apiKey, err := st.CreateAPIKey("proxy-test", 0)
	if err != nil {
		t.Fatal(err)
	}
	addr := strings.TrimPrefix(upstreamServer.URL, "http://")
	p := New(st, storeNetworks(st), dialTestHub{addr: addr}, slog.New(slog.NewTextHandler(io.Discard, nil)), 0)
	t.Cleanup(p.Close)
	return p, apiKey, upstreamServer.Close
}

func TestProxyMultipartRequestIsPreservedEndToEnd(t *testing.T) {
	var gotBody []byte
	var gotContentType, gotAuthorization, gotCustomHeader string
	var gotContentLength int64
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotContentType = r.Header.Get("Content-Type")
		gotContentLength = r.ContentLength
		gotAuthorization = r.Header.Get("Authorization")
		gotCustomHeader = r.Header.Get("X-Omni-Test")
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Upstream", "preserved")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"text":"transcribed"}`)
	})
	p, apiKey, closeUpstream := newProxyE2E(t, upstream)
	defer closeUpstream()

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	filePart, err := mw.CreateFormFile("file", "audio.wav")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.CopyN(filePart, strings.NewReader(strings.Repeat("audio", 1<<20)), 2<<20); err != nil {
		t.Fatal(err)
	}
	if err := mw.WriteField("model", "omni"); err != nil {
		t.Fatal(err)
	}
	if err := mw.WriteField("language", "zh"); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	wantBody := append([]byte(nil), body.Bytes()...)
	contentType := mw.FormDataContentType()
	req := httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", &body)
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("X-Omni-Test", "kept")
	rec := httptest.NewRecorder()
	p.Handler()(rec, req)

	if rec.Code != http.StatusCreated || rec.Header().Get("X-Upstream") != "preserved" || rec.Body.String() != `{"text":"transcribed"}` {
		t.Fatalf("downstream response = %d headers=%v body=%s", rec.Code, rec.Header(), rec.Body.String())
	}
	if gotContentType != contentType || gotAuthorization != "" || gotCustomHeader != "kept" {
		t.Fatalf("upstream headers content-type=%q authorization=%q custom=%q", gotContentType, gotAuthorization, gotCustomHeader)
	}
	if gotContentLength != int64(len(wantBody)) {
		t.Fatalf("upstream content length = %d, want %d", gotContentLength, len(wantBody))
	}
	if !bytes.Equal(gotBody, wantBody) {
		t.Fatalf("upstream multipart body changed: got %d bytes, want %d", len(gotBody), len(wantBody))
	}
}

func TestProxyPreservesSSEBinaryAndUpstreamErrors(t *testing.T) {
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/sse":
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: first\n\n")
			w.(http.Flusher).Flush()
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
			w.(http.Flusher).Flush()
		case "/v1/binary":
			w.Header().Set("Content-Type", "audio/wav")
			w.Header().Set("X-Binary", "yes")
			_, _ = w.Write([]byte{0x52, 0x49, 0x46, 0x46, 0x00, 0xff})
		case "/v1/error":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = io.WriteString(w, `{"error":{"message":"bad audio"}}`)
		case "/v1/unavailable":
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Retry-After", "30")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, `{"error":{"message":"loading"}}`)
		default:
			http.NotFound(w, r)
		}
	})
	p, apiKey, closeUpstream := newProxyE2E(t, upstream)
	defer closeUpstream()

	tests := []struct {
		path        string
		status      int
		contentType string
		body        []byte
	}{
		{path: "/v1/sse", status: http.StatusOK, contentType: "text/event-stream", body: []byte("data: first\n\ndata: [DONE]\n\n")},
		{path: "/v1/binary", status: http.StatusOK, contentType: "audio/wav", body: []byte{0x52, 0x49, 0x46, 0x46, 0x00, 0xff}},
		{path: "/v1/error", status: http.StatusUnprocessableEntity, contentType: "application/json", body: []byte(`{"error":{"message":"bad audio"}}`)},
		{path: "/v1/unavailable", status: http.StatusServiceUnavailable, contentType: "application/json", body: []byte(`{"error":{"message":"loading"}}`)},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, tt.path, strings.NewReader(`{"model":"omni"}`))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+apiKey)
			rec := &flushingRecorder{ResponseRecorder: httptest.NewRecorder()}
			p.Handler()(rec, req)
			if rec.Code != tt.status || !strings.HasPrefix(rec.Header().Get("Content-Type"), tt.contentType) || !bytes.Equal(rec.Body.Bytes(), tt.body) {
				t.Fatalf("response = %d content-type=%q body=%q", rec.Code, rec.Header().Get("Content-Type"), rec.Body.Bytes())
			}
			if tt.path == "/v1/sse" && rec.flushes == 0 {
				t.Fatal("SSE response was not flushed downstream")
			}
			if tt.path == "/v1/binary" && rec.Header().Get("X-Binary") != "yes" {
				t.Fatal("binary response header was not preserved")
			}
			if tt.path == "/v1/unavailable" && rec.Header().Get("Retry-After") != "30" {
				t.Fatal("upstream 503 response header was not preserved")
			}
		})
	}
}

func TestPlaygroundExternalEndpointRewritesModelAndStreams(t *testing.T) {
	var gotAuthorization, gotCookie, gotOrigin, gotReferer, gotModel, gotPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuthorization = r.Header.Get("Authorization")
		gotCookie = r.Header.Get("Cookie")
		gotOrigin = r.Header.Get("Origin")
		gotReferer = r.Header.Get("Referer")
		gotPath = r.URL.Path
		var body struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotModel = body.Model
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n")
		w.(http.Flusher).Flush()
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(upstream.Close)

	st, err := store.Open("file:playground_external_test?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Error(err)
		}
	})
	box, err := auth.NewSecretbox("playground-test-master")
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := store.EncryptAIKey(box, "poe-secret")
	if err != nil {
		t.Fatal(err)
	}
	config, err := st.CreateAIConfig(&store.AIConfig{Name: "Poe", Preset: "poe", BaseURL: upstream.URL + "/v1", APIKey: encrypted, Model: "GPT-5"})
	if err != nil {
		t.Fatal(err)
	}
	p := New(st, storeNetworks(st), readinessTestHub{}, nil, 0)
	t.Cleanup(p.Close)
	body := fmt.Sprintf(`{"model":"external:%d:GPT-5","messages":[{"role":"user","content":"hi"}],"stream":true}`, config.ID)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/playground/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Cookie", "gpuworks_session=must-not-leak")
	req.Header.Set("Origin", "https://console.example:8443")
	req.Header.Set("Referer", "https://console.example:8443/playground/chat")
	rec := &flushingRecorder{ResponseRecorder: httptest.NewRecorder()}
	p.PlaygroundHandler(box)(rec, req)

	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "hello") || rec.flushes == 0 {
		t.Fatalf("external response = %d flushes=%d body=%q", rec.Code, rec.flushes, rec.Body.String())
	}
	if gotPath != "/v1/chat/completions" || gotAuthorization != "Bearer poe-secret" || gotCookie != "" || gotModel != "GPT-5" {
		t.Fatalf("upstream path=%q auth=%q cookie=%q model=%q", gotPath, gotAuthorization, gotCookie, gotModel)
	}
	if gotOrigin != "" || gotReferer != "" {
		t.Fatalf("the console page reached the external endpoint: origin=%q referer=%q", gotOrigin, gotReferer)
	}
}

func TestPlaygroundExternalEndpointRejectsOtherPaths(t *testing.T) {
	st, err := store.Open("file:playground_external_path_test?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Error(err)
		}
	})
	p := New(st, storeNetworks(st), readinessTestHub{}, nil, 0)
	t.Cleanup(p.Close)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/playground/v1/audio/transcriptions", strings.NewReader(`{"model":"external:1:GPT-5"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	p.PlaygroundHandler(nil)(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestPlaygroundProxyAuthorizesVideoLifecycleFromDeploymentContract(t *testing.T) {
	var requests []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Method+" "+r.URL.RequestURI())
		if strings.HasSuffix(r.URL.Path, "/content") {
			w.Header().Set("Content-Type", "video/mp4")
			_, _ = w.Write([]byte{0, 1, 2, 3})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"job-1","status":"completed"}`)
	}))
	t.Cleanup(upstream.Close)

	st, err := store.Open("file:playground_video_contract_test?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Error(err)
		}
	})
	model, _ := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "acme/video", Revision: "main", Name: "video", Operations: `[{"type":"openai.video.generate","endpoint":"/v1/videos","mode":"async"}]`})
	node, _ := st.RegisterNode("video-node", store.HashToken("node"), "video-node")
	dep, _ := st.CreateDeployment(&store.Deployment{ModelID: model.ID, NodeID: node.ID, ServedName: "video-model", Engine: "sglang", State: "active"})
	contract, _ := json.Marshal(inferenceapi.Contract{HealthEndpoint: "/health", Operations: []inferenceapi.Operation{{Type: inferenceapi.OperationVideoGenerate, Endpoint: "/v1/videos", Mode: "async"}}})
	desired, _ := json.Marshal(proto.InstanceDesiredState{ResolvedLaunch: proto.ResolvedLaunch{APIContract: contract}})
	if _, err := st.CreateInstance(&store.Instance{UUID: "video-instance", DeploymentID: dep.ID, NodeID: node.ID, State: proto.InstRunning, APIPort: 30000, DesiredStateJSON: string(desired)}); err != nil {
		t.Fatal(err)
	}
	p := New(st, storeNetworks(st), dialTestHub{addr: strings.TrimPrefix(upstream.URL, "http://")}, nil, 0)
	t.Cleanup(p.Close)
	handler := p.PlaygroundHandler(nil)

	tests := []struct {
		method string
		path   string
		body   string
		status int
	}{
		{http.MethodPost, "/api/v1/playground/v1/videos", `{"model":"video-model","prompt":"hello"}`, http.StatusOK},
		{http.MethodGet, "/api/v1/playground/v1/videos/job-1?model=video-model", "", http.StatusOK},
		{http.MethodGet, "/api/v1/playground/v1/videos/job-1/content?model=video-model&variant=preview", "", http.StatusOK},
		{http.MethodDelete, "/api/v1/playground/v1/videos/job-1?model=video-model", "", http.StatusOK},
		{http.MethodPost, "/api/v1/playground/v1/chat/completions", `{"model":"video-model"}`, http.StatusNotFound},
	}
	for _, test := range tests {
		req := httptest.NewRequest(test.method, test.path, strings.NewReader(test.body))
		if test.body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		rec := httptest.NewRecorder()
		handler(rec, req)
		if rec.Code != test.status {
			t.Fatalf("%s %s status=%d body=%s", test.method, test.path, rec.Code, rec.Body.String())
		}
		if strings.Contains(test.path, "/content") && (rec.Header().Get("Content-Type") != "video/mp4" || !bytes.Equal(rec.Body.Bytes(), []byte{0, 1, 2, 3})) {
			t.Fatalf("video content was not preserved: type=%q body=%v", rec.Header().Get("Content-Type"), rec.Body.Bytes())
		}
	}
	want := []string{"POST /v1/videos", "GET /v1/videos/job-1", "GET /v1/videos/job-1/content?variant=preview", "DELETE /v1/videos/job-1"}
	if fmt.Sprint(requests) != fmt.Sprint(want) {
		t.Fatalf("upstream requests=%v, want %v", requests, want)
	}
}

func TestProxyMultipartErrorsUseOpenAIShape(t *testing.T) {
	p, apiKey, closeUpstream := newProxyE2E(t, http.NotFoundHandler())
	defer closeUpstream()
	for name, contentType := range map[string]string{
		"missing model": func() string {
			var body bytes.Buffer
			mw := multipart.NewWriter(&body)
			_ = mw.WriteField("language", "zh")
			_ = mw.Close()
			return mw.FormDataContentType() + "\n" + body.String()
		}(),
		"missing boundary": "multipart/form-data\nbody",
	} {
		t.Run(name, func(t *testing.T) {
			parts := strings.SplitN(contentType, "\n", 2)
			body := ""
			if len(parts) == 2 {
				body = parts[1]
			}
			req := httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", strings.NewReader(body))
			req.Header.Set("Content-Type", parts[0])
			req.Header.Set("Authorization", "Bearer "+apiKey)
			rec := httptest.NewRecorder()
			p.Handler()(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
			}
			var result struct {
				Error struct {
					Message string `json:"message"`
					Type    string `json:"type"`
				} `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil || result.Error.Message == "" || result.Error.Type != "gpuworks_proxy" {
				t.Fatalf("OpenAI error = %+v, decode err=%v", result, err)
			}
		})
	}
}

func TestProxyCleansSpoolAfterMultipartError(t *testing.T) {
	p, apiKey, closeUpstream := newProxyE2E(t, http.NotFoundHandler())
	defer closeUpstream()
	before, err := os.ReadDir(os.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	beforeNames := map[string]bool{}
	for _, entry := range before {
		if strings.HasPrefix(entry.Name(), "gpuworks-proxy-") {
			beforeNames[entry.Name()] = true
		}
	}

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	part, err := mw.CreateFormField("model")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = part.Write(bytes.Repeat([]byte("x"), maxMultipartModelBytes+1))
	_ = mw.Close()
	req := httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+apiKey)
	rec := httptest.NewRecorder()
	p.Handler()(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	after, err := os.ReadDir(os.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range after {
		if strings.HasPrefix(entry.Name(), "gpuworks-proxy-") && !beforeNames[entry.Name()] {
			t.Fatalf("multipart error leaked spool %s", entry.Name())
		}
	}
}

func TestDistributedSelectionRequiresEveryRankAndRoutesOnlyLeader(t *testing.T) {
	st, err := store.Open("file:proxy_distributed_readiness_test?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Error(err)
		}
	})
	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "Qwen/Qwen3-72B", Revision: "main", Name: "Qwen72B"})
	if err != nil {
		t.Fatal(err)
	}
	nodeA, _ := st.RegisterNode("node-a", store.HashToken("a"), "node-a")
	nodeB, _ := st.RegisterNode("node-b", store.HashToken("b"), "node-b")
	nodeIDs, _ := json.Marshal([]uint{nodeA.ID, nodeB.ID})
	dep, err := st.CreateDeployment(&store.Deployment{
		ModelID: model.ID, NodeID: nodeA.ID, NodeIDs: string(nodeIDs), ServedName: "qwen-72b",
		Engine: "vllm", State: "active", NNodes: 2, DistGroup: "group-a",
	})
	if err != nil {
		t.Fatal(err)
	}
	leader, _ := st.CreateInstance(&store.Instance{
		UUID: "leader", DeploymentID: dep.ID, NodeID: nodeA.ID, State: proto.InstRunning,
		APIPort: 30000, Ports: "[30000]", DistGroup: "group-a", DistRole: "leader",
		NNodes: 2, NodeRank: 0, DistAttempt: 1,
	})
	follower, _ := st.CreateInstance(&store.Instance{
		UUID: "follower", DeploymentID: dep.ID, NodeID: nodeB.ID, State: proto.InstStarting,
		DistGroup: "group-a", DistRole: "follower", NNodes: 2, NodeRank: 1,
		Headless: true, DistAttempt: 1,
	})
	p := New(st, storeNetworks(st), readinessTestHub{online: map[string]bool{"node-a": true, "node-b": true}}, nil, 0)
	t.Cleanup(p.Close)

	_, _, status, message, _ := p.selectDeploymentInstance("qwen-72b")
	if status != http.StatusServiceUnavailable || !strings.Contains(message, "every rank running") {
		t.Fatalf("partial group selection = status %d message %q", status, message)
	}
	if err := st.UpdateInstanceStatus(proto.InstanceStatus{UUID: follower.UUID, State: proto.InstRunning}); err != nil {
		t.Fatal(err)
	}
	_, tgt, status, message, _ := p.selectDeploymentInstance("qwen-72b")
	if status != 0 || message != "" || tgt.nodeUUID != "node-a" || tgt.port != leader.APIPort {
		t.Fatalf("ready group target = %+v status=%d message=%q", tgt, status, message)
	}
	// A follower of another generation does not complete the leader's.
	setAttempt := func(attempt int) {
		if err := st.DB().Model(&store.Instance{}).Where("uuid = ?", follower.UUID).Update("dist_attempt", attempt).Error; err != nil {
			t.Fatal(err)
		}
	}
	setAttempt(2)
	if _, _, status, _, _ = p.selectDeploymentInstance("qwen-72b"); status != http.StatusServiceUnavailable {
		t.Fatalf("stale group generation status = %d, want 503", status)
	}
	setAttempt(1)
	if _, _, status, _, _ = p.selectDeploymentInstance("qwen-72b"); status != 0 {
		t.Fatalf("restored generation status = %d, want routable", status)
	}
	if err := st.UpdateInstanceStatus(proto.InstanceStatus{UUID: follower.UUID, State: proto.InstFailed}); err != nil {
		t.Fatal(err)
	}
	if _, _, status, _, _ = p.selectDeploymentInstance("qwen-72b"); status != http.StatusServiceUnavailable {
		t.Fatalf("failed follower status = %d, want 503", status)
	}
}

func TestSelectInstanceReturns503ForAsleepOrNonRoutableSingleInstance(t *testing.T) {
	for _, tc := range []struct {
		name       string
		sleepLevel int
		headless   bool
		apiPort    int
		wantMsg    string
		wantAsleep bool
	}{
		{name: "asleep", sleepLevel: 1, apiPort: 30001, wantMsg: "model is asleep", wantAsleep: true},
		{name: "headless", apiPort: 30001, headless: true, wantMsg: "no running instance"},
		{name: "no-api-port", apiPort: 0, wantMsg: "no running instance"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, err := store.Open("file:proxy_single_readiness_"+strings.ReplaceAll(t.Name(), "/", "_")+"?mode=memory&cache=shared", bus.New())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := st.Close(); err != nil {
					t.Error(err)
				}
			})
			model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "org/model", Revision: "main", Name: "model"})
			if err != nil {
				t.Fatal(err)
			}
			node, err := st.RegisterNode("single-node", store.HashToken("node"), "single-node")
			if err != nil {
				t.Fatal(err)
			}
			dep, err := st.CreateDeployment(&store.Deployment{ModelID: model.ID, NodeID: node.ID, ServedName: "model", Engine: "vllm", State: "active"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := st.CreateInstance(&store.Instance{
				UUID: "single-instance", DeploymentID: dep.ID, NodeID: node.ID,
				State: proto.InstRunning, SleepLevel: tc.sleepLevel, Headless: tc.headless, APIPort: tc.apiPort,
			}); err != nil {
				t.Fatal(err)
			}
			p := New(st, storeNetworks(st), readinessTestHub{online: map[string]bool{"single-node": true}}, nil, 0)
			t.Cleanup(p.Close)
			_, _, status, msg, asleep := p.selectDeploymentInstance("model")
			if status != http.StatusServiceUnavailable || !strings.Contains(msg, tc.wantMsg) || asleep != tc.wantAsleep {
				t.Fatalf("selection status=%d message=%q asleep=%v, want 503 containing %q asleep=%v", status, msg, asleep, tc.wantMsg, tc.wantAsleep)
			}
		})
	}
}

func TestSelectDeploymentInstanceSkipsNewerPendingRollout(t *testing.T) {
	st, err := store.Open("file:proxy_overlapping_rollout?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Error(err)
		}
	})
	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "org/model", Revision: "main", Name: "model"})
	if err != nil {
		t.Fatal(err)
	}
	node, err := st.RegisterNode("rollout-node", store.HashToken("node"), "rollout-node")
	if err != nil {
		t.Fatal(err)
	}
	serving, err := st.CreateDeployment(&store.Deployment{ModelID: model.ID, NodeID: node.ID, ServedName: "stable-name", Engine: "vllm", State: "active"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateInstance(&store.Instance{UUID: "serving-instance", DeploymentID: serving.ID, NodeID: node.ID, State: proto.InstRunning, APIPort: 30001}); err != nil {
		t.Fatal(err)
	}
	pending := legacySharedNameDeployment(t, st, &store.Deployment{ModelID: model.ID, NodeID: node.ID, ServedName: "stable-name", Engine: "vllm", State: "active"})
	if pending.ID <= serving.ID {
		t.Fatalf("pending deployment id=%d, want newer than serving id=%d", pending.ID, serving.ID)
	}

	p := New(st, storeNetworks(st), readinessTestHub{online: map[string]bool{"rollout-node": true}}, nil, 0)
	t.Cleanup(p.Close)
	selected, target, status, message, asleep := p.selectDeploymentInstance("stable-name")
	if status != 0 || message != "" || asleep || selected == nil || selected.ID != serving.ID || target.nodeUUID != "rollout-node" || target.port != 30001 {
		t.Fatalf("selection deployment=%+v target=%+v status=%d message=%q asleep=%v", selected, target, status, message, asleep)
	}
}

// A deployment ordered to sleep stops being routable on the order itself, not
// on the agent's later report: vLLM queues requests that arrive while it is
// pausing and they hang once it is asleep.
func TestSelectInstanceReturns503OnceSleepIsOrdered(t *testing.T) {
	st, err := store.Open("file:proxy_desired_sleep?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Error(err)
		}
	})
	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "org/model", Revision: "main", Name: "model"})
	if err != nil {
		t.Fatal(err)
	}
	node, err := st.RegisterNode("ordered-node", store.HashToken("node"), "ordered-node")
	if err != nil {
		t.Fatal(err)
	}
	dep, err := st.CreateDeployment(&store.Deployment{ModelID: model.ID, NodeID: node.ID, ServedName: "model", Engine: "vllm", State: "active", Sleepable: true})
	if err != nil {
		t.Fatal(err)
	}
	spec, _ := json.Marshal(proto.InstanceDesiredState{UUID: "ordered-instance", DesiredState: "running", Sleepable: true, APIPort: 30001})
	if _, err := st.CreateInstance(&store.Instance{UUID: "ordered-instance", DeploymentID: dep.ID, NodeID: node.ID, State: proto.InstRunning, APIPort: 30001, DesiredStateJSON: string(spec)}); err != nil {
		t.Fatal(err)
	}
	p := New(st, storeNetworks(st), readinessTestHub{online: map[string]bool{"ordered-node": true}}, nil, 0)
	t.Cleanup(p.Close)
	if _, _, status, msg, asleep := p.selectDeploymentInstance("model"); status != 0 || asleep {
		t.Fatalf("awake instance not routable: %d %q", status, msg)
	}
	if err := st.SetDeploymentSleep(dep.ID, 1); err != nil {
		t.Fatal(err)
	}
	_, _, status, msg, asleep := p.selectDeploymentInstance("model")
	if status != http.StatusServiceUnavailable || !asleep || !strings.Contains(msg, "model is asleep") {
		t.Fatalf("ordered-to-sleep selection = %d %q asleep=%v, want 503 asleep", status, msg, asleep)
	}
}
