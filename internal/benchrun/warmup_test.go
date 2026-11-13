package benchrun

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/pkg/bench/dataset"
	"gpuworks.ai/gpuworks/pkg/bench/search"

	"gpuworks.ai/gpuworks/pkg/bench/loadgen"
)

func TestWarmupRecoversBeforeStartingMeasuredPoint(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(503)
			_, _ = w.Write([]byte(`{"error":{"message":"engine restarting"}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: {\"choices\":[],\"usage\":{\"prompt_tokens\":8,\"completion_tokens\":1}}\n\ndata: [DONE]\n\n"))
	}))
	defer srv.Close()
	rec := &recorder{}
	Execute(context.Background(), Run{ID: 98, TokenizerRef: warmupTokenizerRef(t), Model: "m", Client: loadgen.Client{BaseURL: srv.URL}, Dataset: json.RawMessage(`{"kind":"random"}`), Search: json.RawMessage(`{"concurrency":1,"isl":[8],"osl":[3],"requests_per_point_multiplier":1,"warmup_requests":1}`)}, rec.hooks())
	if rec.last().State != "done" {
		t.Fatalf("transient warmup terminated run: %+v", rec.last())
	}
	var attempts []loadgen.DetailRecord
	if json.Unmarshal(rec.last().Warmup, &attempts) != nil || len(attempts) != 2 || attempts[0].Attempt != 1 || attempts[1].Attempt != 2 || attempts[0].SessionID != attempts[1].SessionID || attempts[1].ErrorText != "" {
		t.Fatalf("lost retry identity or result: %+v", attempts)
	}
	if calls.Load() != 3 || len(rec.points(t)) != 1 || rec.results[1] != 1 {
		t.Fatalf("warmup counted as measurement or retry missing: calls=%d points=%d measured=%v", calls.Load(), len(rec.points(t)), rec.results)
	}
}

func TestWarmupRecoveryPermanentExhaustionAndCancel(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		cancel bool
	}{
		{"permanent", http.StatusUnauthorized, false}, {"exhaustion", http.StatusServiceUnavailable, false}, {"cancel", http.StatusServiceUnavailable, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(`{"error":{"message":"engine unavailable"}}`))
			}))
			defer srv.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			rec := &recorder{}
			hooks := rec.hooks()
			report := hooks.Report
			hooks.Report = func(st proto.BenchmarkStatus) {
				report(st)
				if tc.cancel && strings.Contains(st.Message, "retrying") {
					cancel()
				}
			}
			e := &execution{run: Run{ID: 98, Model: "m", Client: loadgen.Client{BaseURL: srv.URL}}, hooks: hooks, dc: dataset.Config{Kind: "random"}, doc: search.Document{WarmupRequests: 1}}
			window, interval := time.Minute, 5*time.Millisecond
			if tc.name == "exhaustion" {
				// Exercise exhaustion while waiting to retry, with exactly one
				// completed HTTP diagnostic. In-flight expiry has its own case.
				window, interval = time.Second, time.Minute
			}
			err := e.warmupRecovery(ctx, search.Point{Concurrency: 1, ISL: 8, OSL: 1}, window, interval)
			if err == nil {
				t.Fatal("expected warmup failure")
			}
			if len(rec.results) != 0 || len(rec.points(t)) != 0 {
				t.Fatal("warmup created measured evidence")
			}
			switch tc.name {
			case "permanent":
				if calls.Load() != 1 || !strings.Contains(err.Error(), "HTTP 401, stage http: engine unavailable") {
					t.Fatalf("calls=%d err=%v", calls.Load(), err)
				}
			case "exhaustion":
				if calls.Load() != 1 || !strings.Contains(err.Error(), "exhausted") || !strings.Contains(err.Error(), "HTTP 503, stage http: engine unavailable") {
					t.Fatalf("calls=%d err=%v", calls.Load(), err)
				}
			case "cancel":
				if !errors.Is(err, context.Canceled) || calls.Load() != 1 {
					t.Fatalf("calls=%d err=%v", calls.Load(), err)
				}
			}
			var attempts []loadgen.DetailRecord
			if json.Unmarshal(rec.last().Warmup, &attempts) != nil || len(attempts) == 0 || attempts[0].HTTPStatus != tc.status || attempts[0].ErrorText == "" {
				t.Fatalf("lost non-measured warmup diagnostic: %+v", rec.last())
			}
			if tc.name == "exhaustion" && len(attempts) != 1 {
				t.Fatalf("timer exhaustion started another attempt: %+v", attempts)
			}
			if attempts[0].PointSeq != 0 || attempts[0].Text != "" || attempts[0].Usage != nil {
				t.Fatalf("warmup leaked body or fake point: %+v", attempts[0])
			}
		})
	}
}

func TestWarmupDiagnosticsReadOpenAIAndGatewayErrorMessages(t *testing.T) {
	for _, body := range []string{`{"error":{"message":"engine restarting"}}`, `{"error":"engine restarting","code":"unavailable"}`} {
		got := warmupDiagnostic(loadgen.DetailRecord{HTTPStatus: 503, ErrorStage: "http", ErrorText: "endpoint returned 503 Service Unavailable: " + body})
		if got != "HTTP 503, stage http: engine restarting" {
			t.Fatalf("diagnostic=%q", got)
		}
	}
}

func TestWarmupEscapedEndpointErrorIsSafeBeforeStatusPersistence(t *testing.T) {
	for _, wire := range []string{`{"error":{"message":"engine failed https:\/\/url-user:url-password@host.test\/?access_token=url-query"}}`, `{"error":"engine failed \u0068ttps\u003a\u002f\u002furl-user:url-password@host.test/?access_token=url-query"}`} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(400); _, _ = w.Write([]byte(wire)) }))
		rec := &recorder{}
		Execute(context.Background(), Run{ID: 98, TokenizerRef: warmupTokenizerRef(t), Model: "m", Client: loadgen.Client{BaseURL: srv.URL}, Dataset: json.RawMessage(`{"kind":"random"}`), Search: json.RawMessage(`{"concurrency":1,"warmup_requests":1,"isl":[8],"osl":[1]}`)}, rec.hooks())
		srv.Close()
		if rec.last().State != "failed" {
			t.Fatal("permanent endpoint error not fatal")
		}
		for _, st := range rec.statuses {
			raw, _ := json.Marshal(st)
			for _, secret := range []string{"url-user", "url-password", "url-query"} {
				if strings.Contains(string(raw), secret) {
					t.Fatalf("status/persisted attempt retained credential: %s", raw)
				}
			}
		}
		if !strings.Contains(rec.last().Message, "HTTP 400, stage http: engine failed") {
			t.Fatalf("lost useful status/cause: %s", rec.last().Message)
		}
	}
}

func warmupTokenizerRef(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tokenizer.json")
	if err := os.WriteFile(path, []byte(`{"model":{"vocab":{"a":0,"b":1}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

type warmupRoundTrip func(*http.Request) (*http.Response, error)

func (f warmupRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestWarmupTransportPermanentErrorsStopAndConnectionRefusedRecovers(t *testing.T) {
	tlsServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("untrusted TLS request reached handler") }))
	defer tlsServer.Close()
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":8,"completion_tokens":1}}`))
	}))
	defer good.Close()
	closed := httptest.NewServer(nil)
	refused := closed.URL
	closed.Close()
	for _, kind := range []string{"certificate", "protocol", "outbound policy", "connection refused"} {
		t.Run(kind, func(t *testing.T) {
			var calls atomic.Int32
			transport := http.DefaultTransport.(*http.Transport).Clone()
			defer transport.CloseIdleConnections()
			client := &http.Client{Transport: warmupRoundTrip(func(r *http.Request) (*http.Response, error) {
				count := calls.Add(1)
				switch kind {
				case "outbound policy":
					return nil, errors.New("outbound policy denied")
				case "protocol":
					copy := r.Clone(r.Context())
					copy.URL.Scheme = "ftp"
					return transport.RoundTrip(copy)
				case "certificate":
					copy := r.Clone(r.Context())
					target, _ := url.Parse(tlsServer.URL)
					copy.URL.Scheme, copy.URL.Host = target.Scheme, target.Host
					return transport.RoundTrip(copy)
				case "connection refused":
					if count == 1 {
						copy := r.Clone(r.Context())
						target, _ := url.Parse(refused)
						copy.URL.Host = target.Host
						return transport.RoundTrip(copy)
					}
				}
				return transport.RoundTrip(r)
			})}
			rec := &recorder{}
			e := &execution{run: Run{ID: 98, Model: "m", NoStream: true, Client: loadgen.Client{BaseURL: good.URL, HTTP: client}}, hooks: rec.hooks(), dc: dataset.Config{Kind: "random"}, doc: search.Document{WarmupRequests: 1}}
			err := e.warmupRecovery(context.Background(), search.Point{Concurrency: 1, ISL: 8, OSL: 1}, time.Minute, time.Millisecond)
			if kind == "connection refused" {
				if err != nil || calls.Load() != 2 {
					t.Fatalf("transient connection did not recover: calls=%d err=%v", calls.Load(), err)
				}
			} else if err == nil || calls.Load() != 1 {
				t.Fatalf("permanent transport retried: calls=%d err=%v", calls.Load(), err)
			}
		})
	}
}

func TestWarmupCancellationRetainsCanceledAttemptOutcome(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		cancel()
		<-r.Context().Done()
	}))
	defer server.Close()
	rec := &recorder{}
	e := &execution{run: Run{ID: 98, Model: "m", Client: loadgen.Client{BaseURL: server.URL}}, hooks: rec.hooks(), dc: dataset.Config{Kind: "random"}, doc: search.Document{WarmupRequests: 1}}
	if err := e.warmupRecovery(ctx, search.Point{Concurrency: 1, ISL: 8, OSL: 1}, time.Minute, time.Second); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel=%v", err)
	}
	var attempts []loadgen.DetailRecord
	if !strings.Contains(string(rec.last().Warmup), `"canceled":true`) || json.Unmarshal(rec.last().Warmup, &attempts) != nil || len(attempts) != 1 || !attempts[0].Canceled || attempts[0].ErrorStage != "transport" {
		t.Fatalf("lost canceled attempt/source: %s", rec.last().Warmup)
	}
}

func TestWarmupExhaustionDuringRetryRetainsAttemptDiagnostics(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":{"message":"engine unavailable"}}`))
			return
		}
		// Hold the retry until its own recovery context expires: no sleep and no
		// assumed ordering between the last network response and the deadline.
		<-r.Context().Done()
	}))
	defer server.Close()
	rec := &recorder{}
	e := &execution{run: Run{ID: 98, Model: "m", Client: loadgen.Client{BaseURL: server.URL}}, hooks: rec.hooks(), dc: dataset.Config{Kind: "random"}, doc: search.Document{WarmupRequests: 1}}
	err := e.warmupRecovery(context.Background(), search.Point{Concurrency: 1, ISL: 8, OSL: 1}, time.Second, time.Millisecond)
	if err == nil || calls.Load() != 2 || !strings.Contains(err.Error(), "exhausted") || !strings.Contains(err.Error(), "HTTP unknown, stage transport:") || !strings.Contains(err.Error(), "context deadline exceeded") {
		t.Fatalf("calls=%d err=%v", calls.Load(), err)
	}
	var attempts []loadgen.DetailRecord
	if json.Unmarshal(rec.last().Warmup, &attempts) != nil || len(attempts) != 2 {
		t.Fatalf("lost attempt history: %s", rec.last().Warmup)
	}
	if attempts[0].HTTPStatus != http.StatusServiceUnavailable || attempts[0].ErrorStage != "http" || !strings.Contains(attempts[0].ErrorText, "engine unavailable") {
		t.Fatalf("lost original endpoint diagnostic: %+v", attempts[0])
	}
	if attempts[1].HTTPStatus != 0 || attempts[1].ErrorStage != "transport" || !strings.Contains(attempts[1].ErrorText, "context deadline exceeded") || attempts[0].SessionID == "" || attempts[0].SessionID != attempts[1].SessionID || attempts[0].Attempt != 1 || attempts[1].Attempt != 2 {
		t.Fatalf("lost in-flight deadline or retry identity: %+v", attempts)
	}
	if len(rec.results) != 0 || len(rec.points(t)) != 0 {
		t.Fatal("warmup created measured evidence")
	}
	for _, attempt := range attempts {
		if attempt.PointSeq != 0 || attempt.Text != "" || attempt.Usage != nil {
			t.Fatalf("warmup leaked body or fake point: %+v", attempt)
		}
	}
}
