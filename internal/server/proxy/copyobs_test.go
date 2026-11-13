package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// syncBuffer is a concurrency-safe sink for captured slog output; the copy
// observation is logged from the reverse-proxy goroutine while the test reads.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// newObservedProxy wires a proxy that serves the "omni" model through upstream,
// capturing structured logs at Debug level into buf.
func newObservedProxy(t *testing.T, upstream http.Handler, buf *syncBuffer) (*Proxy, string, func()) {
	t.Helper()
	upstreamServer := httptest.NewServer(upstream)
	t.Cleanup(upstreamServer.Close)
	st, err := store.Open("file:proxy_obs_"+strings.ReplaceAll(t.Name(), "/", "_")+"?mode=memory&cache=shared", bus.New())
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
	apiKey, err := st.CreateAPIKey("proxy-obs", 0)
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	p := New(st, storeNetworks(st), dialTestHub{addr: strings.TrimPrefix(upstreamServer.URL, "http://")}, log, 0)
	t.Cleanup(p.Close)
	return p, apiKey, upstreamServer.Close
}

// waitForLog polls buf until a JSON log line with the given msg appears (or the
// deadline passes), returning the decoded record.
func waitForLog(t *testing.T, buf *syncBuffer, msg string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
			if line == "" {
				continue
			}
			var record map[string]any
			if err := json.Unmarshal([]byte(line), &record); err != nil {
				continue
			}
			if record["msg"] == msg {
				return record
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("log line %q not observed; got:\n%s", msg, buf.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func hasLogMsg(buf *syncBuffer, msg string) bool {
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			continue
		}
		if record["msg"] == msg {
			return true
		}
	}
	return false
}

func TestProxyLogsTruncationOnShortContentLength(t *testing.T) {
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, brw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		// Declare 100 bytes but deliver only 50, then drop the connection.
		fmt.Fprintf(brw, "HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nContent-Length: 100\r\nConnection: close\r\n\r\n%s", strings.Repeat("A", 50))
		_ = brw.Flush()
		_ = conn.Close()
	})
	buf := &syncBuffer{}
	p, apiKey, closeUpstream := newObservedProxy(t, upstream, buf)
	defer closeUpstream()
	front := httptest.NewServer(p.Handler())
	defer front.Close()

	req, _ := http.NewRequest(http.MethodPost, front.URL+"/v1/chat/completions", strings.NewReader(`{"model":"omni"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("client request: %v", err)
	}
	requestID := resp.Header.Get("X-Request-ID")
	body, readErr := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if readErr == nil {
		t.Fatalf("client expected a truncated read, got clean %d bytes", len(body))
	}

	record := waitForLog(t, buf, "proxy response body truncated")
	if requestID == "" || record["request_id"] != requestID {
		t.Fatalf("log request_id=%v, want header %q", record["request_id"], requestID)
	}
	if record["declared_length"].(float64) != 100 {
		t.Fatalf("declared_length=%v, want 100", record["declared_length"])
	}
	if record["copied_bytes"].(float64) != 50 {
		t.Fatalf("copied_bytes=%v, want 50", record["copied_bytes"])
	}
	if s, _ := record["copy_error"].(string); s == "" {
		t.Fatalf("copy_error missing/empty: %v", record["copy_error"])
	}
	if record["level"] != "WARN" {
		t.Fatalf("level=%v, want WARN", record["level"])
	}
}

func TestProxyLogsTruncationOnChunkedMidStreamFailure(t *testing.T) {
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, brw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		// Chunked (no declared length): send one chunk, then drop before the
		// terminating zero-length chunk.
		_, _ = io.WriteString(brw, "HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nTransfer-Encoding: chunked\r\n\r\n")
		_, _ = io.WriteString(brw, "5\r\nhello\r\n")
		_ = brw.Flush()
		_ = conn.Close()
	})
	buf := &syncBuffer{}
	p, apiKey, closeUpstream := newObservedProxy(t, upstream, buf)
	defer closeUpstream()
	front := httptest.NewServer(p.Handler())
	defer front.Close()

	req, _ := http.NewRequest(http.MethodPost, front.URL+"/v1/chat/completions", strings.NewReader(`{"model":"omni"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("client request: %v", err)
	}
	requestID := resp.Header.Get("X-Request-ID")
	_, readErr := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if readErr == nil {
		t.Fatal("client expected a truncated chunked read, got clean EOF")
	}

	record := waitForLog(t, buf, "proxy response body truncated")
	if requestID == "" || record["request_id"] != requestID {
		t.Fatalf("log request_id=%v, want header %q", record["request_id"], requestID)
	}
	if record["declared_length"].(float64) != -1 {
		t.Fatalf("declared_length=%v, want -1 (chunked)", record["declared_length"])
	}
	if record["copied_bytes"].(float64) != 5 {
		t.Fatalf("copied_bytes=%v, want 5", record["copied_bytes"])
	}
	if s, _ := record["copy_error"].(string); s == "" {
		t.Fatalf("copy_error missing/empty: %v", record["copy_error"])
	}
}

func TestProxyHealthyResponseLogsNoTruncationWarning(t *testing.T) {
	const payload = `{"choices":[{"message":{"content":"ok"}}]}`
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", fmt.Sprint(len(payload)))
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, payload)
	})
	buf := &syncBuffer{}
	p, apiKey, closeUpstream := newObservedProxy(t, upstream, buf)
	defer closeUpstream()
	front := httptest.NewServer(p.Handler())
	defer front.Close()

	req, _ := http.NewRequest(http.MethodPost, front.URL+"/v1/chat/completions", strings.NewReader(`{"model":"omni"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("client request: %v", err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil || string(body) != payload {
		t.Fatalf("healthy body=%q err=%v", body, err)
	}

	record := waitForLog(t, buf, "proxy response body copied")
	if record["copied_bytes"].(float64) != float64(len(payload)) {
		t.Fatalf("copied_bytes=%v, want %d", record["copied_bytes"], len(payload))
	}
	if record["declared_length"].(float64) != float64(len(payload)) {
		t.Fatalf("declared_length=%v, want %d", record["declared_length"], len(payload))
	}
	if record["level"] != "DEBUG" {
		t.Fatalf("healthy copy level=%v, want DEBUG", record["level"])
	}
	if hasLogMsg(buf, "proxy response body truncated") {
		t.Fatal("healthy response emitted a truncation warning")
	}
}

func TestProxyWebSocketUpgradeSurvivesObservabilityWrapping(t *testing.T) {
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		typ, data, err := conn.Read(r.Context())
		if err != nil {
			return
		}
		_ = conn.Write(r.Context(), typ, data)
	})
	buf := &syncBuffer{}
	p, apiKey, closeUpstream := newObservedProxy(t, upstream, buf)
	defer closeUpstream()
	front := httptest.NewServer(p.Handler())
	defer front.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	url := "ws" + strings.TrimPrefix(front.URL, "http") + "/v1/realtime?model=omni"
	conn, response, err := websocket.Dial(ctx, url, &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": []string{"Bearer " + apiKey}},
	})
	if err != nil {
		status := 0
		if response != nil {
			status = response.StatusCode
		}
		t.Fatalf("WebSocket handshake status=%d: %v", status, err)
	}
	defer conn.CloseNow()
	if err := conn.Write(ctx, websocket.MessageText, []byte("ping")); err != nil {
		t.Fatalf("ws write: %v", err)
	}
	typ, echoed, err := conn.Read(ctx)
	if err != nil || typ != websocket.MessageText || string(echoed) != "ping" {
		t.Fatalf("ws echo typ=%v msg=%q err=%v", typ, echoed, err)
	}
	if hasLogMsg(buf, "proxy response body truncated") {
		t.Fatal("WebSocket upgrade must not be treated as a truncated body copy")
	}
}
