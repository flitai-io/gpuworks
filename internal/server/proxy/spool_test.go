package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestDefaultMaxBodyBytesIs256MiB(t *testing.T) {
	if DefaultMaxBodyBytes != 256<<20 {
		t.Fatalf("DefaultMaxBodyBytes = %d, want %d (256 MiB)", DefaultMaxBodyBytes, 256<<20)
	}
}

func TestReplayBodyThresholdBudgetAndCleanup(t *testing.T) {
	before := spoolNames(t)
	small, err := newReplayBody(bytes.Repeat([]byte("s"), bodyMemoryThreshold), DefaultMaxBodyBytes)
	if err != nil {
		t.Fatal(err)
	}
	if small.file != nil {
		t.Fatal("body at memory threshold unexpectedly used a temp file")
	}
	if got := replayBudget.Load(); got < bodyMemoryThreshold {
		t.Fatalf("replay budget=%d after small body, want at least %d", got, bodyMemoryThreshold)
	}
	if err := small.Close(); err != nil {
		t.Fatal(err)
	}
	if got := replayBudget.Load(); got != 0 {
		t.Fatalf("replay budget=%d after close, want 0", got)
	}

	large, err := newReplayBody(bytes.Repeat([]byte("l"), bodyMemoryThreshold+1), DefaultMaxBodyBytes)
	if err != nil {
		t.Fatal(err)
	}
	if large.file == nil {
		t.Fatal("body over memory threshold stayed in memory")
	}
	if err := large.Close(); err != nil {
		t.Fatal(err)
	}

	replayBudget.Store(bodyMemoryBudget)
	budgetExhausted, err := newReplayBody([]byte("budget"), DefaultMaxBodyBytes)
	replayBudget.Store(0)
	if err != nil {
		t.Fatal(err)
	}
	if budgetExhausted.file == nil {
		t.Fatal("body used memory after replay budget was exhausted")
	}
	if err := budgetExhausted.Close(); err != nil {
		t.Fatal(err)
	}
	assertNoLeakedSpool(t, before)
}

// spoolNames returns the set of gpuworks-proxy-* temp files currently on disk,
// so a test can assert nothing leaked.
func spoolNames(t *testing.T) map[string]bool {
	t.Helper()
	entries, err := os.ReadDir(os.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "gpuworks-proxy-") {
			names[entry.Name()] = true
		}
	}
	return names
}

func assertNoLeakedSpool(t *testing.T, before map[string]bool) {
	t.Helper()
	for name := range spoolNames(t) {
		if !before[name] {
			t.Fatalf("leaked spool file %s", name)
		}
	}
}

func TestExtractModelBodyExactlyAtCapPasses(t *testing.T) {
	body := `{"model":"qwen"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	model, cleanup, err := extractModel(req, int64(len(body)))
	defer cleanup()
	if err != nil || model != "qwen" {
		t.Fatalf("model=%q err=%v, want qwen and no error at exactly cap", model, err)
	}
}

func TestExtractModelBodyOverCapReturns413AndCleansSpool(t *testing.T) {
	before := spoolNames(t)
	body := `{"model":"qwen"}`    // 16 bytes
	limit := int64(len(body) - 1) // force cap+1 = len(body)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	// Hide the declared length so the streaming cap (not the early check) trips.
	req.ContentLength = 0
	_, cleanup, err := extractModel(req, limit)
	defer cleanup()
	if err == nil {
		t.Fatal("expected 413 for a body over the cap")
	}
	if got := statusForExtractError(err); got != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", got)
	}
	assertNoLeakedSpool(t, before)
}

// trackingReader fails the test if the body is read; used to prove an oversized
// declared Content-Length is rejected before any byte is consumed.
type trackingReader struct{ read bool }

func (r *trackingReader) Read(p []byte) (int, error) {
	r.read = true
	return 0, io.EOF
}

func TestExtractModelDeclaredContentLengthOverCapRejectedWithoutReading(t *testing.T) {
	before := spoolNames(t)
	reader := &trackingReader{}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", reader)
	req.Header.Set("Content-Type", "application/json")
	req.ContentLength = 1024
	_, cleanup, err := extractModel(req, 512)
	defer cleanup()
	if err == nil || statusForExtractError(err) != http.StatusRequestEntityTooLarge {
		t.Fatalf("err=%v status=%d, want 413", err, statusForExtractError(err))
	}
	if reader.read {
		t.Fatal("body was read despite an oversized declared Content-Length")
	}
	assertNoLeakedSpool(t, before)
}

func TestExtractModelMultipartRespectsCap(t *testing.T) {
	before := spoolNames(t)
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if err := mw.WriteField("model", "omni"); err != nil {
		t.Fatal(err)
	}
	part, err := mw.CreateFormFile("file", "audio.wav")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(bytes.Repeat([]byte("a"), 4096)); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.ContentLength = 0 // exercise the streaming cap, not the early check
	_, cleanup, err := extractModel(req, 1024)
	defer cleanup()
	if err == nil || statusForExtractError(err) != http.StatusRequestEntityTooLarge {
		t.Fatalf("multipart over cap err=%v status=%d, want 413", err, statusForExtractError(err))
	}
	assertNoLeakedSpool(t, before)
}

// newCappedProxy wires a full data-plane proxy for the "omni" model with a small
// spool cap; upstreamHit flips true only if the request reaches the upstream.
func newCappedProxy(t *testing.T, maxBody int64, upstreamHit *bool) (*Proxy, string, func()) {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*upstreamHit = true
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)
	st, err := store.Open("file:proxy_cap_"+strings.ReplaceAll(t.Name(), "/", "_")+"?mode=memory&cache=shared", bus.New())
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
	node, err := st.RegisterNode("cap-node", store.HashToken("node"), "cap-node")
	if err != nil {
		t.Fatal(err)
	}
	dep, err := st.CreateDeployment(&store.Deployment{ModelID: model.ID, NodeID: node.ID, ServedName: "omni", Engine: "vllm", State: "active"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateInstance(&store.Instance{UUID: "omni-cap", DeploymentID: dep.ID, NodeID: node.ID, State: proto.InstRunning, APIPort: 30000, Ports: "[30000]"}); err != nil {
		t.Fatal(err)
	}
	apiKey, err := st.CreateAPIKey("cap-test", 0)
	if err != nil {
		t.Fatal(err)
	}
	p := New(st, storeNetworks(st), dialTestHub{addr: strings.TrimPrefix(upstream.URL, "http://")}, slog.New(slog.NewTextHandler(io.Discard, nil)), maxBody)
	t.Cleanup(p.Close)
	return p, apiKey, upstream.Close
}

func TestProxyRejectsOversizedBodyWith413BeforeUpstream(t *testing.T) {
	before := spoolNames(t)
	upstreamHit := false
	p, apiKey, closeUpstream := newCappedProxy(t, 64, &upstreamHit)
	defer closeUpstream()

	body := `{"model":"omni","padding":"` + strings.Repeat("x", 256) + `"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	rec := httptest.NewRecorder()
	p.Handler()(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status=%d, want 413; body=%s", rec.Code, rec.Body.String())
	}
	var result struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil || result.Error.Type != "gpuworks_proxy" || result.Error.Message == "" {
		t.Fatalf("413 error shape = %+v, decode err=%v", result, err)
	}
	if upstreamHit {
		t.Fatal("oversized request reached the upstream")
	}
	assertNoLeakedSpool(t, before)
}

func TestProxyConfigurableCapAllowsBodyWithinLimit(t *testing.T) {
	upstreamHit := false
	p, apiKey, closeUpstream := newCappedProxy(t, 1<<20, &upstreamHit)
	defer closeUpstream()

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"omni"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	rec := httptest.NewRecorder()
	p.Handler()(rec, req)

	if rec.Code == http.StatusRequestEntityTooLarge {
		t.Fatalf("in-limit body wrongly rejected: %s", rec.Body.String())
	}
	if !upstreamHit {
		t.Fatal("in-limit body never reached the upstream")
	}
}
