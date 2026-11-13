package proxy

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/server/tracecontext"
	"gpuworks.ai/gpuworks/internal/server/uxrecord"
)

const (
	clientTrace = "4bf92f3577b34da6a3ce929d0e0e4736"
	clientSpan  = "00f067aa0ba902b7"
)

// traceUpstream answers like a non-streaming engine and records the trace
// headers of every call it gets.
type traceUpstream struct {
	mu     sync.Mutex
	parent []string
	state  []string
}

func (u *traceUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	_, _ = io.Copy(io.Discard, r.Body)
	u.mu.Lock()
	u.parent = append(u.parent, r.Header.Get("traceparent"))
	u.state = append(u.state, strings.Join(r.Header.Values("tracestate"), "|"))
	u.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"1"},"finish_reason":"stop"}],"usage":{"prompt_tokens":9,"completion_tokens":1}}`)
}

func (u *traceUpstream) calls() ([]string, []string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.parent...), append([]string(nil), u.state...)
}

// send posts one chat request through the public entry with extra headers
// and returns the response and the request's record.
func send(t *testing.T, p *Proxy, apiKey string, records chan uxrecord.Request, headers map[string][]string) (*httptest.ResponseRecorder, uxrecord.Request) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, chatPath, strings.NewReader(`{"model":"omni","messages":[{"role":"user","content":"1"}],"max_tokens":32,"temperature":0.2}`))
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	for name, values := range headers {
		for _, v := range values {
			req.Header.Add(name, v)
		}
	}
	rec := httptest.NewRecorder()
	p.Handler()(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	select {
	case r := <-records:
		return rec, r
	case <-time.After(5 * time.Second):
		t.Fatal("no record")
		return nil, uxrecord.Request{}
	}
}

func newTraceProxy(t *testing.T, sampler func(tracecontext.TraceID) bool) (*Proxy, *traceUpstream, string, chan uxrecord.Request) {
	p, up, apiKey, records, _ := newTraceProxyDep(t, sampler)
	return p, up, apiKey, records
}

func newTraceProxyDep(t *testing.T, sampler func(tracecontext.TraceID) bool) (*Proxy, *traceUpstream, string, chan uxrecord.Request, uint) {
	t.Helper()
	up := &traceUpstream{}
	p, _, apiKey, depID, closeUpstream := newAccountingProxy(t, up)
	t.Cleanup(closeUpstream)
	records := make(chan uxrecord.Request, 8)
	p.SetRequestSink(func(r uxrecord.Request) { records <- r })
	p.SetTraceSampler(sampler)
	return p, up, apiKey, records, depID
}

// A request that continues a caller's trace reaches the engine as a child of
// the gateway's span, in the same trace, with the caller's sampling decision
// and tracestate; the record names the same three spans.
func TestTheEngineIsAChildOfTheGatewaySpanInTheCallersTrace(t *testing.T) {
	p, up, apiKey, records := newTraceProxy(t, func(tracecontext.TraceID) bool { return false })
	rec, record := send(t, p, apiKey, records, map[string][]string{
		"traceparent":  {"00-" + clientTrace + "-" + clientSpan + "-01"},
		"tracestate":   {"vendor=a", "other=b"},
		"X-Request-ID": {"upstream-gw-42"},
	})
	parents, states := up.calls()
	if len(parents) != 1 {
		t.Fatalf("engine calls = %d", len(parents))
	}
	tr := record.Trace
	if tr.Trace.String() != clientTrace || tr.Parent.String() != clientSpan || tr.ID.IsZero() || !tr.Sampled() {
		t.Fatalf("record trace = %+v", tr)
	}
	if want := "00-" + clientTrace + "-" + tr.ID.String() + "-01"; parents[0] != want {
		t.Fatalf("engine traceparent = %q, want %q", parents[0], want)
	}
	if states[0] != "vendor=a,other=b" || tr.State != "vendor=a,other=b" {
		t.Fatalf("tracestate engine=%q record=%q", states[0], tr.State)
	}
	if record.Model != "omni" {
		t.Fatalf("model = %q", record.Model)
	}
	// The sampling parameters the client set travel with the record, for the
	// trace exporter (a vLLM deployment's body is read anyway).
	if s := record.Sampling; !s.HasMaxTokens || s.MaxTokens != 32 || !s.HasTemperature || s.Temperature != 0.2 || s.HasTopP || s.HasN {
		t.Fatalf("sampling = %+v", s)
	}
	// Without the chi middleware in front, the client's request id is kept.
	if got := rec.Header().Get("X-Request-ID"); got != "upstream-gw-42" || record.ID != got {
		t.Fatalf("request id header %q record %q", got, record.ID)
	}
}

// A request without a trace starts one: a time-ordered trace id, the random
// flag, the sampler's decision. An invalid traceparent counts as none and
// its tracestate is dropped with it.
func TestARequestWithoutAValidTraceStartsOne(t *testing.T) {
	var sample atomic.Bool
	p, up, apiKey, records := newTraceProxy(t, func(tracecontext.TraceID) bool { return sample.Load() })

	before := time.Now().Add(-time.Second)
	sample.Store(true)
	_, sampled := send(t, p, apiKey, records, nil)
	sample.Store(false)
	_, unsampled := send(t, p, apiKey, records, map[string][]string{
		"traceparent": {"00-" + strings.Repeat("0", 32) + "-" + clientSpan + "-01"},
		"tracestate":  {"vendor=a"},
	})
	parents, states := up.calls()
	for i, r := range []uxrecord.Request{sampled, unsampled} {
		tr := r.Trace
		if tr.Trace.IsZero() || !tr.Parent.IsZero() || tr.State != "" {
			t.Fatalf("record %d trace = %+v", i, tr)
		}
		if at := tr.Trace.Time(); at.Before(before) || at.After(time.Now().Add(time.Second)) {
			t.Fatalf("record %d trace id time %v", i, at)
		}
		if states[i] != "" {
			t.Fatalf("engine %d tracestate = %q", i, states[i])
		}
	}
	if want := "00-" + sampled.Trace.Trace.String() + "-" + sampled.Trace.ID.String() + "-03"; parents[0] != want {
		t.Fatalf("sampled engine traceparent = %q, want %q", parents[0], want)
	}
	if want := "00-" + unsampled.Trace.Trace.String() + "-" + unsampled.Trace.ID.String() + "-02"; parents[1] != want {
		t.Fatalf("unsampled engine traceparent = %q, want %q", parents[1], want)
	}
	if sampled.Trace.Trace == unsampled.Trace.Trace {
		t.Fatal("two requests share a trace")
	}
}

// The in-flight table shows the trace id of a request still running.
func TestALiveRequestCarriesItsTraceID(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{})
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{}`)
	})
	p, _, apiKey, depID, closeUpstream := newAccountingProxy(t, upstream)
	defer closeUpstream()
	done := make(chan struct{})
	go func() {
		defer close(done)
		req := httptest.NewRequest(http.MethodPost, chatPath, strings.NewReader(`{"model":"omni"}`))
		req.Header.Set("Authorization", "Bearer "+apiKey)
		req.Header.Set("traceparent", "00-"+clientTrace+"-"+clientSpan+"-00")
		p.Handler()(httptest.NewRecorder(), req)
	}()
	<-entered
	live := p.LiveRequests(depID, 10)
	close(release)
	<-done
	if len(live.Requests) != 1 || live.Requests[0].TraceID.String() != clientTrace {
		t.Fatalf("live = %+v", live.Requests)
	}
}

// flakyHub fails the first tunnel it is asked for, then dials the upstream.
type flakyHub struct {
	addr  string
	fails atomic.Int32
}

func (h *flakyHub) OpenTunnel(context.Context, string, string) (net.Conn, error) {
	if h.fails.Add(1) == 1 {
		return nil, errors.New("connection refused")
	}
	return net.Dial("tcp", h.addr)
}
func (h *flakyHub) Online(string) bool { return true }

// A retried request reaches the replica that answers with the same trace
// header the failed attempt carried: the retry is part of the gateway span.
func TestARetriedRequestKeepsItsTrace(t *testing.T) {
	up := &traceUpstream{}
	p, _, apiKey, depID, closeUpstream := newAccountingProxy(t, up)
	defer closeUpstream()
	inst, err := p.store.InstanceByUUID("omni-instance")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.store.CreateInstance(&store.Instance{
		UUID: "omni-instance-2", DeploymentID: depID, NodeID: inst.NodeID,
		State: proto.InstRunning, APIPort: 30001, Ports: "[30001]",
	}); err != nil {
		t.Fatal(err)
	}
	direct := p.hub.(dialTestHub)
	hub := &flakyHub{addr: direct.addr}
	p.hub = hub
	p.balancer = newBalancer(p.load)
	p.rp.Transport.(*retryTransport).hub = hub
	p.rp.Transport.(*retryTransport).balancer = p.balancer
	p.routes.rebuild()
	records := make(chan uxrecord.Request, 2)
	p.SetRequestSink(func(r uxrecord.Request) { records <- r })

	_, record := send(t, p, apiKey, records, map[string][]string{"traceparent": {"00-" + clientTrace + "-" + clientSpan + "-01"}})
	if hub.fails.Load() < 2 {
		t.Fatalf("tunnel opens = %d, want a retry", hub.fails.Load())
	}
	parents, _ := up.calls()
	if want := "00-" + clientTrace + "-" + record.Trace.ID.String() + "-01"; len(parents) != 1 || parents[0] != want {
		t.Fatalf("engine traceparents = %q, want [%q]", parents, want)
	}
}

// Both halves of a PD handoff belong to the one gateway span, in the
// sequential handoff and the concurrent one: the decode request as its
// child, the prefill copy under the PD stage's own span.
func TestBothPDStagesAreChildrenOfTheGatewaySpan(t *testing.T) {
	for _, mode := range []string{"vllm-nixl", "sglang-nixl"} {
		t.Run(mode, func(t *testing.T) {
			h := newPDHarnessMode(t, mode, 1, "")
			records := make(chan uxrecord.Request, 2)
			h.proxy.SetRequestSink(func(r uxrecord.Request) { records <- r })
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, chatPath, strings.NewReader(pdChat))
			req.Header.Set("traceparent", "00-"+clientTrace+"-"+clientSpan+"-01")
			req.Header.Set("Authorization", "Bearer "+h.key)
			h.proxy.Handler()(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("status %d %s", rec.Code, rec.Body.String())
			}
			var record uxrecord.Request
			select {
			case record = <-records:
			case <-time.After(5 * time.Second):
				t.Fatal("no record")
			}
			// The decoder is the gateway span's child; the prefill copy is the
			// child of the PD stage's own span, itself the gateway span's.
			want := "00-" + clientTrace + "-" + record.Trace.ID.String() + "-01"
			// The concurrent handoff has no stage time and so no stage span:
			// its prefill copy is the gateway span's child.
			if record.PD == nil || record.PD.SpanID == record.Trace.ID || (mode == "vllm-nixl") == record.PD.SpanID.IsZero() {
				t.Fatalf("record pd = %+v", record.PD)
			}
			wantPrefill := want
			if !record.PD.SpanID.IsZero() {
				wantPrefill = "00-" + clientTrace + "-" + record.PD.SpanID.String() + "-01"
			}
			deadline := time.Now().Add(2 * time.Second)
			for {
				h.prefills[0].mu.Lock()
				prefill := append([]string(nil), h.prefills[0].traces...)
				h.prefills[0].mu.Unlock()
				if len(prefill) == 1 || time.Now().After(deadline) {
					h.decode.mu.Lock()
					decode := append([]string(nil), h.decode.traces...)
					h.decode.mu.Unlock()
					if len(prefill) != 1 || prefill[0] != wantPrefill || len(decode) != 1 || decode[0] != want {
						t.Fatalf("prefill %q decode %q, want %q and %q", prefill, decode, wantPrefill, want)
					}
					return
				}
				time.Sleep(10 * time.Millisecond)
			}
		})
	}
}

// A probe is never sampled, whatever the sampler says: it leaves no record,
// so an engine recording its part would leave an orphan.
func TestAProbeIsNeverSampled(t *testing.T) {
	p, up, _, _, depID := newTraceProxyDep(t, func(tracecontext.TraceID) bool { return true })
	reply := p.Probe(context.Background(), depID, chatPath, map[string]any{"max_tokens": 1})
	if reply.HTTPStatus != http.StatusOK {
		t.Fatalf("probe = %+v", reply)
	}
	parents, _ := up.calls()
	if len(parents) != 1 || !strings.HasSuffix(parents[0], "-02") {
		t.Fatalf("probe traceparent = %q", parents)
	}
}
