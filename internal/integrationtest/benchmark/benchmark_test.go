package benchmark

import (
	"compress/gzip"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/testenv"
	"gpuworks.ai/gpuworks/internal/testenv/apiwire"
	fakehandler "gpuworks.ai/gpuworks/test/fake-engine/handler"
)

func TestAgentBenchmarkMultiPointRunPersistsMetrics(t *testing.T) {
	engine := httptest.NewServer(fakehandler.New(fakehandler.Options{
		Model:            "fake-model",
		ResponseText:     "abcd",
		PromptTokens:     8,
		CompletionTokens: 4,
		CachedTokens:     2,
		TTFTDelay:        5 * time.Millisecond,
		TPOTDelay:        2 * time.Millisecond,
	}))
	defer engine.Close()

	env := testenv.New(t)
	node := env.AddNode("benchmark-node", testenv.WithGPUs(1, testenv.GPUSpec{Model: "A100", VRAMMB: 24576}), testenv.WithDriverName("fake"))
	env.WaitNodeOnline(t, "benchmark-node")
	api := env.API(t)
	body, _ := json.Marshal(map[string]any{
		"name":          "agent multi-point smoke",
		"target_kind":   "external",
		"base_url":      engine.URL,
		"model":         "fake-model",
		"load_origin":   node.UUID,
		"tokenizer_ref": benchmarkTestTokenizer(t), "dataset": map[string]any{"kind": "random", "input_tokens": 8, "output_tokens": 4},
		"mode":          "grid",
		"search":        map[string]any{"concurrency": []int{1, 2}, "isl": []int{8}, "osl": []int{4}, "requests_per_point_multiplier": 2},
		"keep_detail":   true,
		"detail_cap_mb": 1,
	})
	resp, err := api.PostJSON("/api/v1/benchmarks", body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("create status=%d", resp.StatusCode)
	}
	var created struct {
		ID uint `json:"id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()
	if created.ID == 0 {
		t.Fatal("benchmark id is zero")
	}

	env.Eventually(t, 20*time.Second, func() bool {
		run, err := env.Store().GetBenchmarkRun(created.ID)
		return err == nil && run.State == "done"
	})
	run, err := env.Store().GetBenchmarkRun(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	points, err := env.Store().ListBenchmarkPoints(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(points) != 2 {
		t.Fatalf("points=%+v", points)
	}
	for _, p := range points {
		if p.State != "done" || p.Success != p.Concurrency*2 || p.Errors != 0 {
			t.Fatalf("point=%+v", p)
		}
		if p.InputTokens != int64(p.Concurrency*2*8) || p.OutputTokens != int64(p.Concurrency*2*4) {
			t.Fatalf("usage point=%+v", p)
		}
		if p.ActualISL != 8 || p.ActualOSL != 4 {
			t.Fatalf("actual lengths=%+v", p)
		}
		if p.P99TTFT < 5 {
			t.Fatalf("fake engine TTFT delay was not reflected: point=%+v", p)
		}
		if p.DetailPath == "" {
			t.Fatalf("agent detail path missing: %+v", p)
		}
		f, err := os.Open(p.DetailPath)
		if err != nil {
			t.Fatal(err)
		}
		zr, err := gzip.NewReader(f)
		if err != nil {
			_ = f.Close()
			t.Fatal(err)
		}
		var detail map[string]any
		if err := json.NewDecoder(zr).Decode(&detail); err != nil {
			_ = zr.Close()
			_ = f.Close()
			t.Fatal(err)
		}
		_ = zr.Close()
		_ = f.Close()
		if detail["text"] == nil {
			t.Fatalf("agent detail=%v", detail)
		}
	}
	if run.Progress == "" {
		t.Fatal("final progress was not persisted")
	}
	var verdict store.BenchmarkVerdict
	if err := json.Unmarshal([]byte(run.Verdict), &verdict); err != nil || verdict.Kind != "grid_done" || verdict.BestPointSeq == 0 {
		t.Fatalf("grid verdict=%q parsed=%+v err=%v", run.Verdict, verdict, err)
	}

	metricsURL := "/api/v1/metrics/query?metric=gpuworks_bench_output_tput&range=1h&step=1s&benchmark=" + strconv.FormatUint(uint64(created.ID), 10)
	var series []apiwire.MetricSeries
	// Metrics ingest is decoupled from the hub's ctrl-stream read loop (a
	// bounded queue plus a batched writer flushing every 200ms or
	// metricsIngestFlushRows samples, see obsingest.Pipeline.RunMetrics):
	// the last sample from this run's final point can still be sitting in
	// that queue for up to one flush interval at the moment the run itself
	// is observed as "done", so this polls instead of asserting on the
	// first response.
	env.Eventually(t, 5*time.Second, func() bool {
		resp, err := api.Get(metricsURL)
		if err != nil {
			return false
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return false
		}
		var got apiwire.List[apiwire.MetricSeries]
		if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
			t.Fatalf("decode %s: %v", metricsURL, err)
		}
		series = got.Items
		return len(series) > 0
	})
	if len(series) == 0 {
		t.Fatal("benchmark process metrics were not ingested")
	}
}

func TestServerBenchmarkStoresBoundedDetailArtifact(t *testing.T) {
	engine := httptest.NewServer(fakehandler.New(fakehandler.Options{Model: "fake-model", PromptTokens: 4, CompletionTokens: 2, ResponseText: "xy"}))
	defer engine.Close()
	env := testenv.NewWithPrivateHTTPHosts(t, "127.0.0.1")
	body, _ := json.Marshal(map[string]any{
		"name": "server detail smoke", "target_kind": "external", "base_url": engine.URL, "model": "fake-model", "load_origin": "server",
		"tokenizer_ref": benchmarkTestTokenizer(t), "dataset": map[string]any{"kind": "random", "input_tokens": 4, "output_tokens": 2}, "mode": "grid",
		"search":      map[string]any{"concurrency": []int{1, 2}, "isl": []int{4}, "osl": []int{2}, "requests_per_point_multiplier": 2},
		"keep_detail": true, "detail_cap_mb": 1,
	})
	resp, err := env.API(t).PostJSON("/api/v1/benchmarks", body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("create status=%d", resp.StatusCode)
	}
	var created struct {
		ID uint `json:"id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()
	env.Eventually(t, 20*time.Second, func() bool {
		run, _ := env.Store().GetBenchmarkRun(created.ID)
		return run != nil && run.State == "done"
	})
	points, err := env.Store().ListBenchmarkPoints(created.ID)
	if err != nil || len(points) != 2 {
		t.Fatalf("points=%+v err=%v", points, err)
	}
	for _, p := range points {
		if p.DetailPath == "" {
			t.Fatalf("missing detail path: %+v", p)
		}
		st, err := os.Stat(p.DetailPath)
		if err != nil || st.Size() == 0 {
			t.Fatalf("detail path=%q stat=%v", p.DetailPath, err)
		}
	}
	resp, err = env.API(t).Get("/api/v1/benchmarks/" + strconv.FormatUint(uint64(created.ID), 10) + "/points/1/detail")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "application/gzip" {
		t.Fatalf("download status=%d content-type=%q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
}

func TestAgentSaturationSearchStopsBeforeMaximumOnPlateau(t *testing.T) {
	engine := httptest.NewServer(fakehandler.New(fakehandler.Options{Model: "fake-model", PromptTokens: 4, CompletionTokens: 2, ResponseText: "xy"}))
	defer engine.Close()
	env := testenv.New(t)
	node := env.AddNode("saturation-node", testenv.WithGPUs(1, testenv.GPUSpec{Model: "A100"}), testenv.WithDriverName("fake"))
	env.WaitNodeOnline(t, "saturation-node")
	body, _ := json.Marshal(map[string]any{"name": "adaptive saturation", "target_kind": "external", "base_url": engine.URL, "model": "fake-model", "load_origin": node.UUID, "tokenizer_ref": benchmarkTestTokenizer(t), "dataset": map[string]any{"kind": "random", "input_tokens": 4, "output_tokens": 2}, "mode": "saturation", "search": map[string]any{"concurrency": 1, "max_concurrency": 32, "isl": []int{4}, "osl": []int{2}, "min_throughput_gain": 100.0, "requests_per_point_multiplier": 2}})
	resp, err := env.API(t).PostJSON("/api/v1/benchmarks", body)
	if err != nil {
		t.Fatal(err)
	}
	var created struct {
		ID uint `json:"id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()
	env.Eventually(t, 20*time.Second, func() bool {
		run, _ := env.Store().GetBenchmarkRun(created.ID)
		return run != nil && run.State == "done"
	})
	points, err := env.Store().ListBenchmarkPoints(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(points) != 2 || points[0].Concurrency != 1 || points[1].Concurrency != 2 {
		t.Fatalf("adaptive points=%+v", points)
	}
	run, err := env.Store().GetBenchmarkRun(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	var verdict store.BenchmarkVerdict
	if err := json.Unmarshal([]byte(run.Verdict), &verdict); err != nil || verdict.Kind != "saturated" || verdict.SaturationConcurrency < 1 {
		t.Fatalf("saturation verdict=%q parsed=%+v err=%v", run.Verdict, verdict, err)
	}
}

func TestAgentBenchmarkStatusRecoversAfterServerRestart(t *testing.T) {
	engine := httptest.NewServer(fakehandler.New(fakehandler.Options{Model: "fake-model", PromptTokens: 4, CompletionTokens: 2, ResponseText: "xy", TTFTDelay: 250 * time.Millisecond}))
	defer engine.Close()
	env := testenv.New(t)
	node := env.AddNode("benchmark-reconnect-node", testenv.WithGPUs(1, testenv.GPUSpec{Model: "A100"}), testenv.WithDriverName("fake"))
	env.WaitNodeOnline(t, "benchmark-reconnect-node")
	body, _ := json.Marshal(map[string]any{"name": "agent reconnect", "target_kind": "external", "base_url": engine.URL, "model": "fake-model", "load_origin": node.UUID, "tokenizer_ref": benchmarkTestTokenizer(t), "dataset": map[string]any{"kind": "random", "input_tokens": 4, "output_tokens": 2}, "mode": "grid", "search": map[string]any{"concurrency": []int{1}, "isl": []int{4}, "osl": []int{2}, "requests_per_point_multiplier": 4}})
	resp, err := env.API(t).PostJSON("/api/v1/benchmarks", body)
	if err != nil {
		t.Fatal(err)
	}
	var created struct {
		ID uint `json:"id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()
	env.Eventually(t, 10*time.Second, func() bool {
		run, _ := env.Store().GetBenchmarkRun(created.ID)
		return run != nil && run.State == "running"
	})
	env.StopServer(t)
	time.Sleep(1200 * time.Millisecond)
	env.RestartServer(t)
	env.WaitNodeOnline(t, "benchmark-reconnect-node")
	env.Eventually(t, 20*time.Second, func() bool {
		run, _ := env.Store().GetBenchmarkRun(created.ID)
		return run != nil && run.State == "done"
	})
	points, err := env.Store().ListBenchmarkPoints(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(points) != 1 || points[0].Success != 4 {
		t.Fatalf("recovered points=%+v", points)
	}
}

func TestAgentBenchmarkCancelStopsDesiredWork(t *testing.T) {
	var requests atomic.Int32
	started := make(chan struct{})
	canceled := make(chan struct{}, 10)
	models := fakehandler.New(fakehandler.Options{Model: "fake-model"})
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			models.ServeHTTP(w, r)
			return
		}
		var request struct {
			Model         string            `json:"model"`
			Messages      []json.RawMessage `json:"messages"`
			Stream        bool              `json:"stream"`
			MaxTokens     int               `json:"max_tokens"`
			StreamOptions struct {
				IncludeUsage bool `json:"include_usage"`
			} `json:"stream_options"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.Model != "fake-model" || len(request.Messages) == 0 || !request.Stream || request.MaxTokens != 2 || !request.StreamOptions.IncludeUsage {
			t.Errorf("invalid streaming completion request: %+v err=%v", request, err)
			http.Error(w, `{"error":{"message":"invalid streaming completion request"}}`, http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n"))
		w.(http.Flusher).Flush()
		if requests.Add(1) == 1 {
			close(started)
		}
		// Keep a real stream in flight until cancellation reaches the endpoint.
		// A role-only chunk produces no TTFT or output token observation.
		<-r.Context().Done()
		canceled <- struct{}{}
	}))
	t.Cleanup(engine.Close)
	env := testenv.New(t)
	node := env.AddNode("benchmark-cancel-node", testenv.WithGPUs(1, testenv.GPUSpec{Model: "A100"}), testenv.WithDriverName("fake"))
	env.WaitNodeOnline(t, "benchmark-cancel-node")
	body, _ := json.Marshal(map[string]any{"name": "agent cancel", "target_kind": "external", "base_url": engine.URL, "model": "fake-model", "load_origin": node.UUID, "tokenizer_ref": benchmarkTestTokenizer(t), "dataset": map[string]any{"kind": "random", "input_tokens": 4, "output_tokens": 2}, "mode": "grid", "search": map[string]any{"concurrency": []int{1}, "isl": []int{4}, "osl": []int{2}, "requests_per_point_multiplier": 10}})
	api := env.API(t)
	resp, err := api.PostJSON("/api/v1/benchmarks", body)
	if err != nil {
		t.Fatal(err)
	}
	var created struct {
		ID uint `json:"id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()
	env.Eventually(t, 10*time.Second, func() bool {
		run, _ := env.Store().GetBenchmarkRun(created.ID)
		return run != nil && run.State == "running"
	})
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("agent never sent the first streaming request")
	}
	resp, err = api.PostJSON("/api/v1/benchmarks/"+strconv.FormatUint(uint64(created.ID), 10)+"/cancel", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("cancel status=%d, expected confirmed execution stop", resp.StatusCode)
	}
	select {
	case <-canceled:
	case <-time.After(10 * time.Second):
		t.Fatal("endpoint did not observe cancellation of the in-flight request")
	}
	var points []store.BenchmarkPoint
	env.Eventually(t, 10*time.Second, func() bool {
		points, err = env.Store().ListBenchmarkPoints(created.ID)
		return err == nil && len(points) == 1 && points[0].State == "canceled"
	})
	time.Sleep(1200 * time.Millisecond)
	if sent := requests.Load(); sent != 1 {
		t.Fatalf("agent sent queued requests after cancellation: %d", sent)
	}
	run, err := env.Store().GetBenchmarkRun(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.State != "canceled" {
		t.Fatalf("state=%q", run.State)
	}
	points, err = env.Store().ListBenchmarkPoints(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(points) != 1 || points[0].State != "canceled" || points[0].StartedAt == nil || points[0].EndedAt == nil || points[0].Success != 0 || points[0].Errors != 1 {
		t.Fatalf("cancellation must retain one partial result without reporting done/failed: %+v", points)
	}
	var tail struct {
		Evidence *struct {
			Version int `json:"version"`
			Plan    struct {
				PlannedSessions int `json:"planned_sessions"`
				PlannedRequests int `json:"planned_requests"`
			} `json:"plan"`
			All struct {
				Attempted int `json:"attempted"`
				Success   int `json:"success"`
				Failed    int `json:"failed"`
				Canceled  int `json:"canceled"`
			} `json:"all"`
			Outcomes struct {
				CanceledBeforeStart int `json:"canceled_before_start"`
				Skipped             int `json:"skipped"`
				CompletedSessions   int `json:"completed_sessions"`
				CanceledSessions    int `json:"canceled_sessions"`
			} `json:"outcomes"`
		} `json:"Evidence"`
	}
	if err := json.Unmarshal([]byte(points[0].LongTail), &tail); err != nil || tail.Evidence == nil {
		t.Fatalf("missing cancellation evidence: %s err=%v", points[0].LongTail, err)
	}
	evidence := tail.Evidence
	if evidence.Version != 1 || evidence.Plan.PlannedSessions != 10 || evidence.Plan.PlannedRequests != 10 || evidence.All.Attempted != 1 || evidence.All.Success != 0 || evidence.All.Failed != 0 || evidence.All.Canceled != 1 || evidence.Outcomes.CanceledBeforeStart != 9 || evidence.Outcomes.Skipped != 0 || evidence.Outcomes.CompletedSessions != 0 || evidence.Outcomes.CanceledSessions != 10 {
		t.Fatalf("cancellation ledger does not reconcile the queued and in-flight work: %+v", evidence)
	}
	var curve []store.BenchmarkCurveSample
	if err := json.Unmarshal([]byte(points[0].Curve), &curve); err != nil || len(curve) == 0 {
		t.Fatalf("cancellation lost the actual load curve: %q err=%v", points[0].Curve, err)
	}
}

func benchmarkTestTokenizer(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tokenizer.json")
	if err := os.WriteFile(path, []byte(`{"model":{"vocab":{"a":0,"b":1}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}
