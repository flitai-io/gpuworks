package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	hubpkg "gpuworks.ai/gpuworks/internal/server/hub"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// End-to-end proxy request accounting tests, driven through the HTTP
// handlers rather than calling Accountant methods directly. Unit-level
// Accountant tests live in accounting_test.go.

// newAccountingProxy builds a Proxy serving a single ready "omni" deployment
// through upstream (mirroring newProxyE2E in proxy_test.go), additionally
// wiring an Accountant the test can inspect directly.
func newAccountingProxy(t *testing.T, upstream http.Handler) (p *Proxy, acct *Accountant, apiKey string, depID uint, closeUpstream func()) {
	t.Helper()
	return newAccountingProxyFor(t, upstream, &store.Deployment{Engine: "vllm"})
}

// newAccountingProxyFor is newAccountingProxy for a deployment shaped by the
// caller (its engine, its catalog selection snapshot); the model, node,
// served name and state are filled in here.
func newAccountingProxyFor(t *testing.T, upstream http.Handler, shape *store.Deployment) (p *Proxy, acct *Accountant, apiKey string, depID uint, closeUpstream func()) {
	t.Helper()
	upstreamServer := httptest.NewServer(upstream)
	t.Cleanup(upstreamServer.Close)
	st, err := store.Open("file:proxy_acct_"+strings.ReplaceAll(t.Name(), "/", "_")+"?mode=memory&cache=shared", bus.New())
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
	shape.ModelID, shape.NodeID, shape.ServedName, shape.State = model.ID, node.ID, "omni", "active"
	dep, err := st.CreateDeployment(shape)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateInstance(&store.Instance{
		UUID: "omni-instance", DeploymentID: dep.ID, NodeID: node.ID,
		State: proto.InstRunning, APIPort: 30000, Ports: "[30000]",
	}); err != nil {
		t.Fatal(err)
	}
	key, err := st.CreateAPIKey("proxy-acct-"+strings.ReplaceAll(t.Name(), "/", "_"), 0)
	if err != nil {
		t.Fatal(err)
	}
	addr := strings.TrimPrefix(upstreamServer.URL, "http://")
	pr := New(st, storeNetworks(st), dialTestHub{addr: addr}, slog.New(slog.NewTextHandler(io.Discard, nil)), 0)
	t.Cleanup(pr.Close)
	a := newAccountant(nil, 16, nil)
	pr.SetAccountant(a)
	return pr, a, key, dep.ID, upstreamServer.Close
}

func TestProxyAccountingClassesEndToEndAndInflightReturnsToZero(t *testing.T) {
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/ok":
			w.WriteHeader(http.StatusOK)
		case "/v1/badreq":
			w.WriteHeader(http.StatusBadRequest)
		case "/v1/toomany":
			w.WriteHeader(http.StatusTooManyRequests)
		case "/v1/servererr":
			w.WriteHeader(http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	})
	p, acct, apiKey, depID, closeUpstream := newAccountingProxy(t, upstream)
	defer closeUpstream()

	for _, path := range []string{"/v1/ok", "/v1/badreq", "/v1/toomany", "/v1/servererr"} {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"model":"omni"}`))
		req.Header.Set("Authorization", "Bearer "+apiKey)
		rec := httptest.NewRecorder()
		p.Handler()(rec, req)
	}

	acct.mu.Lock()
	c := acct.counters[depID]
	acct.mu.Unlock()
	if c == nil {
		t.Fatal("no counters recorded for the deployment")
	}
	want := map[reqClass]uint64{class2xx: 1, class4xx: 1, class429: 1, class5xx: 1}
	for class, n := range want {
		if got := c.classTotals[class]; got != n {
			t.Fatalf("class %s = %d, want %d (all: %v)", class, got, n, c.classTotals)
		}
	}
	if c.durationCount != 4 {
		t.Fatalf("durationCount = %d, want 4", c.durationCount)
	}
	if c.inflight != 0 {
		t.Fatalf("inflight after all requests completed = %d, want 0", c.inflight)
	}
}

func TestProxyAccountingUnknownModelCountedUnderDeploymentZero(t *testing.T) {
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	p, acct, apiKey, _, closeUpstream := newAccountingProxy(t, upstream)
	defer closeUpstream()

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"does-not-exist"}`))
	req.Header.Set("Authorization", "Bearer "+apiKey)
	rec := httptest.NewRecorder()
	p.Handler()(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}

	acct.mu.Lock()
	c0 := acct.counters[0]
	acct.mu.Unlock()
	if c0 == nil || c0.classTotals[class4xx] != 1 {
		t.Fatalf("deployment 0 class4xx = %+v, want 1", c0)
	}
}

func TestProxyAccountingAsleepInstanceCountedAsAsleep503(t *testing.T) {
	st, err := store.Open("file:proxy_acct_asleep?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Error(err)
		}
	})
	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "org/sleepy", Revision: "main", Name: "sleepy"})
	if err != nil {
		t.Fatal(err)
	}
	node, err := st.RegisterNode("sleepy-node", store.HashToken("node"), "sleepy-node")
	if err != nil {
		t.Fatal(err)
	}
	dep, err := st.CreateDeployment(&store.Deployment{ModelID: model.ID, NodeID: node.ID, ServedName: "sleepy", Engine: "vllm", State: "active"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateInstance(&store.Instance{
		UUID: "sleepy-instance", DeploymentID: dep.ID, NodeID: node.ID,
		State: proto.InstRunning, SleepLevel: 1, APIPort: 30000,
	}); err != nil {
		t.Fatal(err)
	}
	apiKey, err := st.CreateAPIKey("proxy-acct-asleep", 0)
	if err != nil {
		t.Fatal(err)
	}
	p := New(st, storeNetworks(st), readinessTestHub{online: map[string]bool{"sleepy-node": true}}, nil, 0)
	t.Cleanup(p.Close)
	acct := newAccountant(nil, 4, nil)
	p.SetAccountant(acct)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"sleepy"}`))
	req.Header.Set("Authorization", "Bearer "+apiKey)
	rec := httptest.NewRecorder()
	p.Handler()(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}

	acct.mu.Lock()
	c := acct.counters[dep.ID]
	acct.mu.Unlock()
	if c == nil || c.classTotals[classAsleep] != 1 {
		t.Fatalf("classAsleep = %+v, want 1", c)
	}
	if c.classTotals[class5xx] != 0 {
		t.Fatalf("an asleep rejection must not also be counted as class5xx: %+v", c.classTotals)
	}
	if c.inflight != 0 {
		t.Fatalf("inflight = %d, want 0 (an asleep rejection never dispatches)", c.inflight)
	}
	if c.durationCount != 0 {
		t.Fatalf("durationCount = %d, want 0 (RecordImmediate excludes duration)", c.durationCount)
	}
}

func TestProxyAccountingSSECountedOnceAtEndWithFullStreamDuration(t *testing.T) {
	const upstreamSleep = 150 * time.Millisecond
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		f := w.(http.Flusher)
		_, _ = io.WriteString(w, "data: first\n\n")
		f.Flush()
		time.Sleep(upstreamSleep)
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		f.Flush()
	})
	p, acct, apiKey, depID, closeUpstream := newAccountingProxy(t, upstream)
	defer closeUpstream()
	front := httptest.NewServer(p.Handler())
	defer front.Close()

	req, err := http.NewRequest(http.MethodPost, front.URL+"/v1/chat/completions", strings.NewReader(`{"model":"omni"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if _, err := io.ReadAll(resp.Body); err != nil {
		t.Fatalf("read body: %v", err)
	}
	_ = resp.Body.Close()

	c := waitAccounted(t, acct, depID, 1)
	if c.classTotals[class2xx] != 1 {
		t.Fatalf("class2xx = %d, want 1 (counted once at stream end)", c.classTotals[class2xx])
	}
	if c.durationCount != 1 {
		t.Fatalf("durationCount = %d, want 1", c.durationCount)
	}
	if c.durationSumMs < float64(upstreamSleep.Milliseconds()) {
		t.Fatalf("durationSumMs = %v, want >= %v (the full stream took at least that long)", c.durationSumMs, upstreamSleep.Milliseconds())
	}
	if c.inflight != 0 {
		t.Fatalf("inflight after stream completion = %d, want 0", c.inflight)
	}
}

func TestProxyAccountingClientAbortCountedUnderUpstreamStatusWithAbortDuration(t *testing.T) {
	const upstreamHang = 3 * time.Second
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		f := w.(http.Flusher)
		_, _ = io.WriteString(w, "data: first\n\n")
		f.Flush()
		time.Sleep(upstreamHang)
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		f.Flush()
	})
	p, acct, apiKey, depID, closeUpstream := newAccountingProxy(t, upstream)
	defer closeUpstream()
	front := httptest.NewServer(p.Handler())
	defer front.Close()

	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, front.URL+"/v1/chat/completions", strings.NewReader(`{"model":"omni"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	buf := make([]byte, 64)
	if _, err := resp.Body.Read(buf); err != nil {
		t.Fatalf("first chunk read: %v", err)
	}
	cancel()
	_, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	abortElapsed := time.Since(start)
	if abortElapsed >= upstreamHang {
		t.Fatalf("abort took %v, not exercising a real abort (upstream hang is %v)", abortElapsed, upstreamHang)
	}

	deadline := time.Now().Add(upstreamHang / 2)
	for {
		acct.mu.Lock()
		c := acct.counters[depID]
		recorded := c != nil && c.durationCount == 1
		acct.mu.Unlock()
		if recorded {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("accounting finish was not observed after the client aborted")
		}
		time.Sleep(10 * time.Millisecond)
	}

	acct.mu.Lock()
	c := acct.counters[depID]
	acct.mu.Unlock()
	if c.classTotals[class2xx] != 1 {
		t.Fatalf("class2xx = %d, want 1 (classified by the 200 upstream status despite the truncated copy)", c.classTotals[class2xx])
	}
	if c.durationSumMs <= 0 {
		t.Fatal("duration should still be recorded for an aborted request")
	}
	if c.durationSumMs >= float64(upstreamHang.Milliseconds()) {
		t.Fatalf("durationSumMs = %v, want well under the %v upstream hang (duration must be measured to the abort, not the never-reached stream end)", c.durationSumMs, upstreamHang)
	}
	if c.inflight != 0 {
		t.Fatalf("inflight after abort = %d, want 0", c.inflight)
	}
}

type dialErrorHub struct{}

func (dialErrorHub) OpenTunnel(context.Context, string, string) (net.Conn, error) {
	return nil, errors.New("dial refused")
}
func (dialErrorHub) Online(string) bool { return true }

// dataLaneUnavailableHub simulates the hub's tunnel open failing because the
// node's data lane is detached and did not reattach within its wait budget —
// the same sentinel a real *hub.Hub returns from OpenTunnel, wrapped exactly
// the way Session.awaitDataLane wraps it (a message plus the sentinel via
// %w), so errors.Is sees through it the same way production code does.
type dataLaneUnavailableHub struct{}

func (dataLaneUnavailableHub) OpenTunnel(context.Context, string, string) (net.Conn, error) {
	return nil, fmt.Errorf("%w: node data-lane-unavailable-node did not reattach within 5s", hubpkg.ErrDataLaneUnavailable)
}
func (dataLaneUnavailableHub) Online(string) bool { return true }

func TestProxyAccountingUpstreamConnectionErrorCountedAs5xx(t *testing.T) {
	st, err := store.Open("file:proxy_acct_dialerr?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Error(err)
		}
	})
	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "org/dial", Revision: "main", Name: "dial"})
	if err != nil {
		t.Fatal(err)
	}
	node, err := st.RegisterNode("dial-node", store.HashToken("node"), "dial-node")
	if err != nil {
		t.Fatal(err)
	}
	dep, err := st.CreateDeployment(&store.Deployment{ModelID: model.ID, NodeID: node.ID, ServedName: "dial", Engine: "vllm", State: "active"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateInstance(&store.Instance{UUID: "dial-instance", DeploymentID: dep.ID, NodeID: node.ID, State: proto.InstRunning, APIPort: 30000}); err != nil {
		t.Fatal(err)
	}
	apiKey, err := st.CreateAPIKey("proxy-acct-dial", 0)
	if err != nil {
		t.Fatal(err)
	}
	p := New(st, storeNetworks(st), dialErrorHub{}, slog.New(slog.NewTextHandler(io.Discard, nil)), 0)
	t.Cleanup(p.Close)
	acct := newAccountant(nil, 4, nil)
	p.SetAccountant(acct)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"dial"}`))
	req.Header.Set("Authorization", "Bearer "+apiKey)
	rec := httptest.NewRecorder()
	p.Handler()(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}

	acct.mu.Lock()
	c := acct.counters[dep.ID]
	acct.mu.Unlock()
	if c == nil || c.classTotals[class5xx] != 1 {
		t.Fatalf("class5xx = %+v, want 1", c)
	}
	if c.inflight != 0 {
		t.Fatalf("inflight after a dial error = %d, want 0", c.inflight)
	}
}

// TestProxyDataLaneUnavailableReturns503WithRetryAfter asserts that when the
// hub's tunnel open fails with hub.ErrDataLaneUnavailable (the node is online
// but its data lane is detached and did not reattach within the hub's own
// wait budget), the proxy reports a distinct 503 with a Retry-After header —
// not the generic 502 a dead/refused connection gets — while still counting
// it under the same class5xx bucket and cleaning up in-flight accounting the
// same way. There is only one instance for this deployment, so the retry
// transport has no candidate to switch to; TestRetryTransportRetriesDataLaneUnavailableUsingFailoverPolicy
// in retry_transport_test.go covers the multi-replica case where it does.
func TestProxyDataLaneUnavailableReturns503WithRetryAfter(t *testing.T) {
	st, err := store.Open("file:proxy_acct_datalane_unavailable?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Error(err)
		}
	})
	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "org/lane", Revision: "main", Name: "lane"})
	if err != nil {
		t.Fatal(err)
	}
	node, err := st.RegisterNode("data-lane-unavailable-node", store.HashToken("node"), "data-lane-unavailable-node")
	if err != nil {
		t.Fatal(err)
	}
	dep, err := st.CreateDeployment(&store.Deployment{ModelID: model.ID, NodeID: node.ID, ServedName: "lane", Engine: "vllm", State: "active"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateInstance(&store.Instance{UUID: "lane-instance", DeploymentID: dep.ID, NodeID: node.ID, State: proto.InstRunning, APIPort: 30000}); err != nil {
		t.Fatal(err)
	}
	apiKey, err := st.CreateAPIKey("proxy-acct-lane", 0)
	if err != nil {
		t.Fatal(err)
	}
	p := New(st, storeNetworks(st), dataLaneUnavailableHub{}, slog.New(slog.NewTextHandler(io.Discard, nil)), 0)
	t.Cleanup(p.Close)
	acct := newAccountant(nil, 4, nil)
	p.SetAccountant(acct)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"lane"}`))
	req.Header.Set("Authorization", "Bearer "+apiKey)
	rec := httptest.NewRecorder()
	p.Handler()(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if ra := rec.Header().Get("Retry-After"); ra == "" {
		t.Fatal("503 for a detached data lane must carry a Retry-After header")
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not JSON: %v (%s)", err, rec.Body.String())
	}
	errObj, ok := body["error"].(map[string]any)
	if !ok || errObj["type"] != "gpuworks_proxy" {
		t.Fatalf("body = %s, want the standard gpuworks_proxy error shape", rec.Body.String())
	}

	acct.mu.Lock()
	c := acct.counters[dep.ID]
	acct.mu.Unlock()
	if c == nil || c.classTotals[class5xx] != 1 {
		t.Fatalf("class5xx = %+v, want 1", c)
	}
	if c.inflight != 0 {
		t.Fatalf("inflight after a data-lane-unavailable error = %d, want 0", c.inflight)
	}
}

func TestProxyAccountingConcurrentRequestsAreRaceFree(t *testing.T) {
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	p, acct, apiKey, depID, closeUpstream := newAccountingProxy(t, upstream)
	defer closeUpstream()
	front := httptest.NewServer(p.Handler())
	defer front.Close()

	const n = 40
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			req, err := http.NewRequest(http.MethodPost, front.URL+"/v1/chat/completions", strings.NewReader(`{"model":"omni"}`))
			if err != nil {
				t.Errorf("build request: %v", err)
				return
			}
			req.Header.Set("Authorization", "Bearer "+apiKey)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Errorf("request: %v", err)
				return
			}
			_, _ = io.ReadAll(resp.Body)
			_ = resp.Body.Close()
		}()
	}
	wg.Wait()

	c := waitAccounted(t, acct, depID, n)
	if c.classTotals[class2xx] != n {
		t.Fatalf("class2xx = %d, want %d", c.classTotals[class2xx], n)
	}
	if c.durationCount != n {
		t.Fatalf("durationCount = %d, want %d", c.durationCount, n)
	}
	if c.inflight != 0 {
		t.Fatalf("inflight after concurrent completion = %d, want 0", c.inflight)
	}
}

// TestProxyAccountingRewriteModelFailureCountedForResolvedDeployment covers
// item 5's first bypass: a body-rewrite failure happens after the deployment
// is resolved (so it should be attributed to it) but before Dispatch (so it
// must never contribute to duration_sum/count).
func TestProxyAccountingRewriteModelFailureCountedForResolvedDeployment(t *testing.T) {
	p, acct, apiKey, depID, closeUpstream := newAccountingProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("upstream must not be reached when rewriteModel fails before dispatch")
	}))
	defer closeUpstream()
	// Give the instance an internal model name different from the public
	// served name "omni", so serveSelectedTarget attempts rewriteModel.
	desired, err := json.Marshal(proto.InstanceDesiredState{ResolvedLaunch: proto.ResolvedLaunch{
		Entrypoint: []string{"vllm", "serve", "/runtime/model"}, ModelMount: "/runtime/model",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.store.UpdateInstanceDesiredState("omni-instance", string(desired)); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"omni"}`))
	req.Header.Set("Content-Type", ";;;not-a-media-type") // fails mime.ParseMediaType inside rewriteModel
	req.Header.Set("Authorization", "Bearer "+apiKey)
	rec := httptest.NewRecorder()
	p.Handler()(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}

	acct.mu.Lock()
	c := acct.counters[depID]
	acct.mu.Unlock()
	if c == nil || c.classTotals[class4xx] != 1 {
		t.Fatalf("class4xx = %+v, want 1", c)
	}
	if c.durationCount != 0 {
		t.Fatalf("durationCount = %d, want 0 (a rewriteModel failure never dispatches)", c.durationCount)
	}
	if c.inflight != 0 {
		t.Fatalf("inflight = %d, want 0", c.inflight)
	}
}

// TestProxyAccountingContractMismatchCountedForResolvedDeployment covers item
// 5's second bypass: the Playground handler's contract check runs after
// deployment resolution but before Dispatch.
func TestProxyAccountingContractMismatchCountedForResolvedDeployment(t *testing.T) {
	st, err := store.Open("file:proxy_acct_"+strings.ReplaceAll(t.Name(), "/", "_")+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Error(err)
		}
	})
	node, err := st.RegisterNode("contract-node", store.HashToken("node"), "contract-node")
	if err != nil {
		t.Fatal(err)
	}
	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "test/video", Name: "video", Operations: `[{"type":"openai.video.generate"}]`})
	if err != nil {
		t.Fatal(err)
	}
	dep, err := st.CreateDeployment(&store.Deployment{ModelID: model.ID, NodeID: node.ID, ServedName: "video", Engine: "vllm", State: "active"})
	if err != nil {
		t.Fatal(err)
	}
	desired, _ := json.Marshal(proto.InstanceDesiredState{ResolvedLaunch: proto.ResolvedLaunch{APIContract: json.RawMessage(`{"operations":[{"type":"openai.video.generate"}]}`)}})
	if _, err := st.CreateInstance(&store.Instance{UUID: "video", DeploymentID: dep.ID, NodeID: node.ID, State: proto.InstRunning, APIPort: 30000, DesiredStateJSON: string(desired)}); err != nil {
		t.Fatal(err)
	}
	p := New(st, storeNetworks(st), readinessTestHub{online: map[string]bool{"contract-node": true}}, nil, 0)
	t.Cleanup(p.Close)
	acct := newAccountant(nil, 4, nil)
	p.SetAccountant(acct)

	// The deployment only exposes openai.video.generate; /v1/audio/voices is
	// not part of its contract, so this must 404 without ever dispatching.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/playground/v1/audio/voices?model=video", nil)
	rec := httptest.NewRecorder()
	p.PlaygroundHandler(nil)(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", rec.Code, rec.Body.String())
	}

	acct.mu.Lock()
	c := acct.counters[dep.ID]
	acct.mu.Unlock()
	if c == nil || c.classTotals[class4xx] != 1 {
		t.Fatalf("class4xx = %+v, want 1", c)
	}
	if c.durationCount != 0 {
		t.Fatalf("durationCount = %d, want 0 (a contract mismatch never dispatches)", c.durationCount)
	}
}

// TestProxyAccountingWebSocketUpgradeCountedAsClass2xx covers item 7: a
// successful WebSocket upgrade (101) must still show up in requests_total so
// a WebSocket-only deployment isn't invisible in accounting.
func TestProxyAccountingWebSocketUpgradeCountedAsClass2xx(t *testing.T) {
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
	p, acct, apiKey, depID, closeUpstream := newAccountingProxy(t, upstream)
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
	if _, _, err := conn.Read(ctx); err != nil {
		t.Fatalf("ws read: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		acct.mu.Lock()
		c := acct.counters[depID]
		got := c != nil && c.classTotals[class2xx] >= 1
		acct.mu.Unlock()
		if got {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("WebSocket upgrade was never counted under class2xx")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// waitAccounted waits until n dispatched requests to depID have been
// accounted and returns a snapshot of its counters. Over a real server the
// client can finish reading a response before the proxy handler closes the
// upstream body, which is where accounting happens, so asserting right after
// the client returns undercounts now and then (todo T19).
func waitAccounted(t *testing.T, acct *Accountant, depID uint, n uint64) deploymentCounters {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		acct.mu.Lock()
		c := acct.counters[depID]
		var snapshot deploymentCounters
		if c != nil {
			snapshot = *c
			snapshot.classTotals = make(map[reqClass]uint64, len(c.classTotals))
			for class, total := range c.classTotals {
				snapshot.classTotals[class] = total
			}
		}
		acct.mu.Unlock()
		if snapshot.durationCount >= n {
			return snapshot
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d of %d requests accounted after 10s", snapshot.durationCount, n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
