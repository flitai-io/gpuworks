package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/deployment"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/pkg/bench/loadgen"

	benchmeta "gpuworks.ai/gpuworks/pkg/bench"
)

func TestBenchmarkHTTPClientsKeepPerRequestDeadline(t *testing.T) {
	admission := newBenchmarkAdmission(t, "benchmark_http_deadline")
	admission.handlers.d.Server = StaticServerSettings{Loopback: "http://127.0.0.1:8800"}
	dep, _ := admission.managedTarget(t, "deadline")
	for _, run := range []*store.BenchmarkRun{
		{TargetKind: "external", BaseURL: "https://example.com", LoadOrigin: "server"},
		{TargetKind: "instance", DeploymentID: dep.ID, InstanceUUID: "deadline-leader", Model: dep.ServedName, LoadOrigin: "server", BaseURL: "http://127.0.0.1:8800/api/v1/playground"},
	} {
		t.Run(run.TargetKind, func(t *testing.T) {
			client := admission.handlers.benchmarkHTTPClient(run)
			defer client.CloseIdleConnections()
			client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
				deadline, ok := req.Context().Deadline()
				remaining := time.Until(deadline)
				if !ok || remaining < benchmeta.DefaultRequestTimeoutSeconds*time.Second-time.Minute || remaining > benchmeta.DefaultRequestTimeoutSeconds*time.Second {
					t.Fatalf("default per-request deadline missing: present=%v remaining=%s", ok, remaining)
				}
				return &http.Response{StatusCode: 204, Header: make(http.Header), Body: http.NoBody}, nil
			})
			response, err := client.Get(run.BaseURL)
			if err != nil {
				t.Fatal(err)
			}
			response.Body.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer cancel()
			client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
				deadline, ok := req.Context().Deadline()
				if !ok || time.Until(deadline) > 20*time.Millisecond {
					t.Fatal("client ignored shorter caller deadline")
				}
				<-req.Context().Done()
				return nil, req.Context().Err()
			})
			request, _ := http.NewRequestWithContext(ctx, http.MethodGet, run.BaseURL, nil)
			if _, err := client.Do(request); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("caller cancellation was lost: %v", err)
			}
		})
	}
}

func TestExternalBenchmarkHTTPPrivateTargetPolicy(t *testing.T) {
	var hits atomic.Int32
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"fixture-model"}]}`))
	}))
	defer fixture.Close()
	for _, allowed := range []bool{false, true} {
		h := &apiHandlers{}
		if allowed {
			h.d.Outside.AllowPrivateHosts = []string{"127.0.0.1"}
		}
		before := hits.Load()
		err := h.probeExternalBenchmark(context.Background(), "server", fixture.URL, "")
		if allowed && err != nil {
			t.Fatalf("explicit fixture exception: %v", err)
		}
		if !allowed && (err == nil || hits.Load() != before) {
			t.Fatalf("default probe reached private endpoint: error=%v hits=%d", err, hits.Load()-before)
		}
		body, _ := json.Marshal(map[string]string{"url": fixture.URL, "node_uuid": "server"})
		rec := httptest.NewRecorder()
		h.listBenchmarkModels(rec, httptest.NewRequest(http.MethodPost, "/api/v1/benchmarks/models", strings.NewReader(string(body))))
		if allowed && (rec.Code != 200 || !strings.Contains(rec.Body.String(), "fixture-model")) {
			t.Fatalf("explicit exception models=%d %s", rec.Code, rec.Body.String())
		}
		if !allowed && (rec.Code != http.StatusConflict || hits.Load() != before || strings.Contains(rec.Body.String(), "dial tcp")) {
			t.Fatalf("default models=%d %s hits=%d", rec.Code, rec.Body.String(), hits.Load()-before)
		}
	}
}

func TestServerExternalBenchmarkExecutionUsesHTTPPolicy(t *testing.T) {
	var requests atomic.Int32
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: {\"choices\":[],\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":1}}\n\ndata: [DONE]\n\n"))
	}))
	defer fixture.Close()
	for _, allowed := range []bool{false, true} {
		st := urlModelStore(t, fmt.Sprintf("benchmark_http_execution_%v", allowed))
		run, err := st.CreateBenchmarkRun(&store.BenchmarkRun{TokenizerRef: benchmarkTestTokenizer(t), Name: "policy", TargetKind: "external", BaseURL: fixture.URL, Model: "m", LoadOrigin: "server", Dataset: `{"kind":"random","input_tokens":2,"output_tokens":1}`, Mode: "grid", Search: `{"concurrency":[1],"isl":[2],"osl":[1],"requests_per_point_multiplier":1}`})
		if err != nil {
			t.Fatal(err)
		}
		h := &apiHandlers{d: Deps{Store: st}}
		if allowed {
			h.d.Outside.AllowPrivateHosts = []string{"127.0.0.1"}
		}
		before := requests.Load()
		h.runServerBenchmark(context.Background(), run)
		if allowed && requests.Load() <= before {
			t.Fatal("explicit private fixture was not measured")
		}
		if !allowed && requests.Load() != before {
			t.Fatal("server-origin run bypassed private target policy")
		}
	}
}

func TestManagedBenchmarkHTTPTrustRequiresPersistedTargetAndRejectsRedirects(t *testing.T) {
	var gatewayHits, redirectHits atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { redirectHits.Add(1) }))
	defer other.Close()
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gatewayHits.Add(1)
		http.Redirect(w, r, other.URL+"/v1/models", http.StatusFound)
	}))
	defer gateway.Close()
	admission := newBenchmarkAdmission(t, "benchmark_managed_http_policy")
	admission.handlers.d.Outside.AllowPrivateHosts = nil
	admission.handlers.d.Server = StaticServerSettings{Loopback: gateway.URL}
	dep, _ := admission.managedTarget(t, "managed-http")
	run := &store.BenchmarkRun{TargetKind: "instance", DeploymentID: dep.ID, InstanceUUID: "managed-http-leader", Model: dep.ServedName, LoadOrigin: "server", BaseURL: gateway.URL + "/api/v1/playground"}
	client := admission.handlers.benchmarkHTTPClient(run)
	defer client.CloseIdleConnections()
	_, err := (loadgen.Client{HTTP: client, BaseURL: run.BaseURL}).Models(context.Background())
	if err == nil || gatewayHits.Load() != 1 || redirectHits.Load() != 0 {
		t.Fatalf("managed redirect: error=%v gateway=%d redirected=%d", err, gatewayHits.Load(), redirectHits.Load())
	}
	for _, mutate := range []func(*store.BenchmarkRun){
		func(r *store.BenchmarkRun) { r.TargetKind = "external" },
		func(r *store.BenchmarkRun) { r.InstanceUUID = "missing-instance" },
		func(r *store.BenchmarkRun) { r.BaseURL = other.URL },
		func(r *store.BenchmarkRun) { r.DeploymentID++ },
	} {
		changed := *run
		mutate(&changed)
		guarded := admission.handlers.benchmarkHTTPClient(&changed)
		_, err := (loadgen.Client{HTTP: guarded, BaseURL: changed.BaseURL}).Models(context.Background())
		guarded.CloseIdleConnections()
		if err == nil || gatewayHits.Load() != 1 || redirectHits.Load() != 0 {
			t.Fatalf("untrusted managed metadata reached network: error=%v", err)
		}
	}
}

func TestBenchmarkNameRejectedBeforeProbe(t *testing.T) {
	h := &apiHandlers{}
	raw, _ := json.Marshal(map[string]string{"name": strings.Repeat("A", deployment.MaxNameRunes+1)})
	rec := httptest.NewRecorder()
	h.createBenchmark(rec, httptest.NewRequest(http.MethodPost, "/api/v1/benchmarks", strings.NewReader(string(raw))))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"field":"name"`) {
		t.Fatalf("name rejection=%d %s", rec.Code, rec.Body.String())
	}
}
