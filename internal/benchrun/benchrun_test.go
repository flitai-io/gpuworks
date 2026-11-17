package benchrun

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/pkg/bench/dataset"
	"gpuworks.ai/gpuworks/pkg/bench/loadgen"
	"gpuworks.ai/gpuworks/pkg/bench/metrics"
)

// fakeEndpoint is an OpenAI-compatible streaming endpoint: every completion
// streams three content chunks and reports usage, the shape vLLM and SGLang
// produce with stream_options.include_usage. fail makes it answer 503.
type fakeEndpoint struct {
	mu            sync.Mutex
	bodies        []map[string]any
	fail          bool
	failureStatus int
	// textField names the delta field the chunks carry; empty is "content".
	textField string
}

func (f *fakeEndpoint) serve(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.bodies = append(f.bodies, body)
		fail, field := f.fail, f.textField
		f.mu.Unlock()
		if field == "" {
			field = "content"
		}
		if fail {
			status := f.failureStatus
			if status == 0 {
				status = http.StatusServiceUnavailable
			}
			http.Error(w, `{"error":{"message":"overloaded"}}`, status)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, piece := range []string{"a", "b", "c"} {
			_, _ = w.Write([]byte(`data: {"choices":[{"delta":{"` + field + `":"` + piece + `"}}]}` + "\n\n"))
			w.(http.Flusher).Flush()
			time.Sleep(2 * time.Millisecond)
		}
		_, _ = w.Write([]byte(`data: {"choices":[],"usage":{"prompt_tokens":8,"completion_tokens":3}}` + "\n\ndata: [DONE]\n\n"))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (f *fakeEndpoint) requests() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]map[string]any(nil), f.bodies...)
}

type recorder struct {
	mu       sync.Mutex
	statuses []proto.BenchmarkStatus
	warnings []string
	results  map[int]int
	events   []string
}

func (r *recorder) hooks() Hooks {
	r.results = map[int]int{}
	return Hooks{
		Report: func(st proto.BenchmarkStatus) {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.statuses = append(r.statuses, st)
			if len(st.Point) > 0 {
				r.events = append(r.events, "point")
			}
		},
		Warn: func(code string) { r.mu.Lock(); r.warnings = append(r.warnings, code); r.mu.Unlock() },
		Result: func(seq int, _ loadgen.Result) {
			r.mu.Lock()
			r.results[seq]++
			r.mu.Unlock()
		},
		DetailPath: func(seq int) string {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.events = append(r.events, "detail")
			return "/artifacts/point-" + string(rune('0'+seq))
		},
		PointReported: func(int, []loadgen.Result) { r.mu.Lock(); r.events = append(r.events, "reported"); r.mu.Unlock() },
	}
}

func (r *recorder) points(t *testing.T) []proto.BenchmarkPointResult {
	t.Helper()
	var out []proto.BenchmarkPointResult
	for _, st := range r.statuses {
		if len(st.Point) == 0 {
			continue
		}
		var p proto.BenchmarkPointResult
		if err := json.Unmarshal(st.Point, &p); err != nil {
			t.Fatal(err)
		}
		out = append(out, p)
	}
	return out
}

func (r *recorder) last() proto.BenchmarkStatus { return r.statuses[len(r.statuses)-1] }

func TestClosedLoopGridWithWarmupReportsEveryPoint(t *testing.T) {
	endpoint := &fakeEndpoint{}
	srv := endpoint.serve(t)
	rec := &recorder{}
	Execute(context.Background(), Run{TokenizerRef: testTokenizerRef(t),
		ID: 9, Model: "m", Mode: "grid", Client: loadgen.Client{BaseURL: srv.URL},
		Dataset: json.RawMessage(`{"kind":"random","ignore_eos":true}`),
		Search:  json.RawMessage(`{"concurrency":[1,2],"isl":[8],"osl":[3],"requests_per_point_multiplier":1,"warmup_requests":2}`),
	}, rec.hooks())

	if done := rec.last(); done.State != "done" || done.StopReason != proto.BenchmarkStopGridExhausted || done.PointCount != 2 {
		t.Fatalf("terminal status = %+v", done)
	}
	// The first status announces warmup: two discarded requests at
	// min(warmup, first concurrency) = 1 in flight.
	first := rec.statuses[0]
	if first.CurrentPoint == nil || !first.CurrentPoint.Warmup || first.CurrentPoint.RequestTotal != 2 || first.CurrentPoint.Concurrency != 1 || first.PointIndex != 0 {
		t.Fatalf("first status = %+v current=%+v", first, first.CurrentPoint)
	}
	points := rec.points(t)
	if len(points) != 2 {
		t.Fatalf("points = %+v", points)
	}
	// Warmup requests are not part of any point: 1 + 2 measured, 5 sent.
	if points[0].Concurrency != 1 || points[0].Success != 1 || points[1].Concurrency != 2 || points[1].Success != 2 || len(endpoint.requests()) != 5 {
		t.Fatalf("points = %+v, endpoint saw %d requests", points, len(endpoint.requests()))
	}
	if rec.results[1] != 1 || rec.results[2] != 2 {
		t.Fatalf("measured results per point = %v", rec.results)
	}
	p := points[0]
	if p.RequestRate != 0 || p.TargetISL != 8 || p.TargetOSL != 3 || p.ActualOSL != 3 || p.P50TPOT <= 0 || p.AvgTPOT <= 0 || p.P50Latency <= 0 || p.DetailPath != "/artifacts/point-1" || p.StartedAt == nil || p.EndedAt == nil {
		t.Fatalf("point result = %+v", p)
	}
	var tail map[string]any
	if err := json.Unmarshal(p.LongTail, &tail); err != nil || tail["TPOT"] == nil || tail["PeakInputTokens"] != float64(8) {
		t.Fatalf("long tail = %s err=%v", p.LongTail, err)
	}
	// Detail is sealed before the point result goes out, and the origin is
	// told after it, so an agent's detail never precedes its point row.
	if strings.Join(rec.events, ",") != "detail,point,reported,detail,point,reported" {
		t.Fatalf("event order = %v", rec.events)
	}
	for _, body := range endpoint.requests() {
		if body["ignore_eos"] != true || body["stream"] != true || body["max_tokens"] != float64(3) {
			t.Fatalf("request body = %v", body)
		}
	}
}

func TestRandomDatasetWithoutIgnoreEOSChoiceKeepsLegacyBehaviour(t *testing.T) {
	endpoint := &fakeEndpoint{}
	srv := endpoint.serve(t)
	rec := &recorder{}
	Execute(context.Background(), Run{TokenizerRef: testTokenizerRef(t),
		ID: 1, Model: "m", Client: loadgen.Client{BaseURL: srv.URL},
		Dataset: json.RawMessage(`{"kind":"random"}`), Search: json.RawMessage(`{"concurrency":1,"isl":[8],"osl":[3],"requests_per_point_multiplier":1}`),
	}, rec.hooks())
	if rec.last().State != "done" {
		t.Fatalf("terminal = %+v", rec.last())
	}
	for _, body := range endpoint.requests() {
		if _, sent := body["ignore_eos"]; sent {
			t.Fatalf("a run stored before the switch must not send ignore_eos: %v", body)
		}
	}
}

func TestOpenLoopGridReportsRatePoints(t *testing.T) {
	endpoint := &fakeEndpoint{}
	srv := endpoint.serve(t)
	rec := &recorder{}
	Execute(context.Background(), Run{TokenizerRef: testTokenizerRef(t),
		ID: 2, Model: "m", Mode: "grid", Client: loadgen.Client{BaseURL: srv.URL},
		Dataset: json.RawMessage(`{"kind":"random","ignore_eos":true}`),
		Search:  json.RawMessage(`{"request_rates":[400,800],"max_in_flight":4,"point_duration_s":0.05,"isl":[8],"osl":[3]}`),
	}, rec.hooks())
	points := rec.points(t)
	if len(points) != 2 || rec.last().State != "done" {
		t.Fatalf("points = %+v last = %+v", points, rec.last())
	}
	// 0.05 s of arrivals: 400 req/s hits the floor of 20 requests, 800 req/s
	// yields 40.
	for i, tc := range []struct {
		rate     float64
		requests int
	}{{400, 20}, {800, 40}} {
		if points[i].RequestRate != tc.rate || points[i].Concurrency != 4 || points[i].Success != tc.requests {
			t.Fatalf("point %d = %+v", i, points[i])
		}
	}
	var sawRate bool
	for _, st := range rec.statuses {
		if st.CurrentPoint != nil && st.CurrentPoint.RequestRate == 800 && st.CurrentPoint.RequestTotal == 40 {
			sawRate = true
		}
	}
	if !sawRate {
		t.Fatal("progress statuses do not carry the point's arrival rate")
	}
}

func TestWarmupFailureFailsTheRunBeforeAnyPoint(t *testing.T) {
	endpoint := &fakeEndpoint{fail: true, failureStatus: http.StatusBadRequest}
	srv := endpoint.serve(t)
	rec := &recorder{}
	Execute(context.Background(), Run{TokenizerRef: testTokenizerRef(t),
		ID: 3, Model: "m", Client: loadgen.Client{BaseURL: srv.URL},
		Dataset: json.RawMessage(`{"kind":"random"}`), Search: json.RawMessage(`{"concurrency":4,"isl":[8],"osl":[3],"warmup_requests":3}`),
	}, rec.hooks())
	last := rec.last()
	if last.State != "failed" || last.StopReason != proto.BenchmarkStopFatal || !strings.Contains(last.Message, "all 3 warmup requests failed") || !strings.Contains(last.Message, "400") {
		t.Fatalf("terminal = %+v", last)
	}
	if len(rec.points(t)) != 0 || len(endpoint.requests()) != 3 {
		t.Fatalf("points = %d, endpoint requests = %d", len(rec.points(t)), len(endpoint.requests()))
	}
}

// A coded failure reaches the terminal status with its code and params; a
// failure without one (the warmup above) carries neither.
func TestTerminalStatusCarriesTheCodeOfACodedFailure(t *testing.T) {
	loadErr := fmt.Errorf("point 1: %w", &dataset.LoadError{Dataset: "multiturn", Err: errors.New("unexpected EOF")})
	code, params := failureCode(loadErr)
	if code != dataset.LoadErrorCode || params["dataset"] != "multiturn" {
		t.Fatalf("code = %q params = %v", code, params)
	}
	if code, params := failureCode(errors.New("BENCHMARK_DATASET_LOAD_FAILED: multiturn: x")); code != "" || params != nil {
		t.Fatalf("text classified: %q %v", code, params)
	}
	raw, err := json.Marshal(proto.BenchmarkStatus{ID: 9, State: "failed", Message: loadErr.Error(), Code: code, Params: params, StopReason: proto.BenchmarkStopFatal})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"id":9,"state":"failed","message":"point 1: load the built-in multiturn dataset: unexpected EOF","code":"BENCHMARK_DATASET_LOAD_FAILED","params":{"dataset":"multiturn"},"stop_reason":"fatal"}`
	if string(raw) != want {
		t.Fatalf("wire = %s\nwant %s", raw, want)
	}
	var back proto.BenchmarkStatus
	if err := json.Unmarshal(raw, &back); err != nil || back.Code != code || back.Params["dataset"] != "multiturn" {
		t.Fatalf("round trip = %+v %v", back, err)
	}
	endpoint := &fakeEndpoint{fail: true, failureStatus: http.StatusBadRequest}
	rec := &recorder{}
	Execute(context.Background(), Run{TokenizerRef: testTokenizerRef(t), ID: 3, Model: "m", Client: loadgen.Client{BaseURL: endpoint.serve(t).URL},
		Dataset: json.RawMessage(`{"kind":"random"}`), Search: json.RawMessage(`{"concurrency":4,"isl":[8],"osl":[3],"warmup_requests":3}`),
	}, rec.hooks())
	if last := rec.last(); last.State != "failed" || last.Code != "" || last.Params != nil {
		t.Fatalf("uncoded failure = %+v", last)
	}
}

func TestFailedRequestsInAPointAreCountedNotFatal(t *testing.T) {
	endpoint := &fakeEndpoint{fail: true}
	srv := endpoint.serve(t)
	rec := &recorder{}
	Execute(context.Background(), Run{TokenizerRef: testTokenizerRef(t),
		ID: 4, Model: "m", Client: loadgen.Client{BaseURL: srv.URL},
		Dataset: json.RawMessage(`{"kind":"random"}`), Search: json.RawMessage(`{"concurrency":2,"isl":[8],"osl":[3],"requests_per_point_multiplier":2}`),
	}, rec.hooks())
	points := rec.points(t)
	if len(points) != 1 || points[0].Errors != 4 || points[0].Success != 0 || points[0].P50TPOT != 0 || rec.last().State != "done" {
		t.Fatalf("points = %+v last = %+v", points, rec.last())
	}
}

func TestAgenticWithoutTokenizerFails(t *testing.T) {
	rec := &recorder{}
	Execute(context.Background(), Run{ID: 5, Dataset: json.RawMessage(`{"kind":"agentic"}`), Search: json.RawMessage(`{}`)}, rec.hooks())
	if last := rec.last(); len(rec.statuses) != 1 || last.State != "failed" || !strings.Contains(last.Message, "tokenizer") {
		t.Fatalf("statuses = %+v", rec.statuses)
	}
}

func TestInvalidDocumentsFail(t *testing.T) {
	for _, run := range []Run{
		{ID: 6, Dataset: json.RawMessage(`{"kind":`), Search: json.RawMessage(`{}`)},
		{ID: 7, Dataset: json.RawMessage(`{}`), Search: json.RawMessage(`{"concurrency":"x"}`)},
	} {
		rec := &recorder{}
		Execute(context.Background(), run, rec.hooks())
		if last := rec.last(); last.State != "failed" || last.ID != run.ID {
			t.Fatalf("run %d statuses = %+v", run.ID, rec.statuses)
		}
	}
}

func TestCanceledRunPreservesPartialPointWithoutTakingRunTerminalOwnership(t *testing.T) {
	endpoint := &fakeEndpoint{}
	srv := endpoint.serve(t)
	ctx, cancel := context.WithCancel(context.Background())
	rec := &recorder{}
	hooks := rec.hooks()
	result := hooks.Result
	hooks.Result = func(seq int, r loadgen.Result) {
		result(seq, r)
		cancel()
	}
	Execute(ctx, Run{TokenizerRef: testTokenizerRef(t),
		ID: 8, Model: "m", Client: loadgen.Client{BaseURL: srv.URL},
		Dataset: json.RawMessage(`{"kind":"random"}`), Search: json.RawMessage(`{"concurrency":[1,2,4],"isl":[8],"osl":[3],"requests_per_point_multiplier":3}`),
	}, hooks)
	var completed int
	for _, st := range rec.statuses {
		if st.State != "running" {
			t.Fatalf("canceled run took terminal ownership: %s", st.State)
		}
		if len(st.Point) > 0 {
			completed = st.Completed
		}
	}
	points := rec.points(t)
	if len(points) != 1 || points[0].State != "canceled" {
		t.Fatalf("partial points = %+v", points)
	}
	var tail LongTailDoc
	if err := json.Unmarshal(points[0].LongTail, &tail); err != nil {
		t.Fatal(err)
	}
	e := tail.Evidence
	if e == nil || e.Plan.PlannedRequests != 3 || e.All.Attempted != completed || completed >= 3 || e.Outcomes.CanceledBeforeStart == 0 {
		t.Fatalf("partial evidence=%+v completed=%d", e, completed)
	}
	if e.All.Attempted+e.Outcomes.CanceledBeforeStart+e.Outcomes.Skipped != e.Plan.PlannedRequests {
		t.Fatal("canceled request ledger does not close")
	}
}

func TestAdaptiveRunFeedsObservationsWithMedianTPOT(t *testing.T) {
	endpoint := &fakeEndpoint{}
	srv := endpoint.serve(t)
	rec := &recorder{}
	// An unreachable interactivity floor fails the first SLA point, so the
	// search stops after it: the observation carried P50 TPOT.
	Execute(context.Background(), Run{TokenizerRef: testTokenizerRef(t),
		ID: 10, Model: "m", Mode: "sla", Client: loadgen.Client{BaseURL: srv.URL},
		Dataset: json.RawMessage(`{"kind":"random"}`), Search: json.RawMessage(`{"concurrency":1,"max_concurrency":8,"isl":[8],"osl":[3],"requests_per_point_multiplier":1,"min_interactivity":1000000}`),
	}, rec.hooks())
	if last := rec.last(); last.State != "done" || last.StopReason != proto.BenchmarkStopSLAViolated || len(rec.points(t)) != 1 {
		t.Fatalf("last = %+v points = %d", last, len(rec.points(t)))
	}
}

func TestLongTailCarriesOptionalFacts(t *testing.T) {
	hit, coverage := 0.5, 1.0
	tail := LongTail(metrics.Snapshot{
		TTFT: map[int]time.Duration{50: 10 * time.Millisecond}, KVHitRate: &hit, CacheReportCoverage: &coverage, PeakInputTokens: 9,
		Speculative: &metrics.Speculative{TokensPerIteration: 2, AcceptanceRate: 0.5},
	})
	// The stored document keeps the keys rows written before LongTailDoc
	// existed carry, so old and new rows read alike.
	raw, err := json.Marshal(tail)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"TTFT":{"50":10},"TPOT":{},"Latency":{},"ITL":{},"ApproximateTokens":false,"KVHitRate":0.5,"CacheReportCoverage":1,"PeakInputTokens":9,"Speculative":{"TokensPerIteration":2,"AcceptanceRate":0.5}}`
	if string(raw) != want {
		t.Fatalf("long tail = %s\nwant %s", raw, want)
	}
	var back LongTailDoc
	if err := json.Unmarshal([]byte(want), &back); err != nil || back.Speculative == nil || *back.KVHitRate != 0.5 {
		t.Fatalf("round trip = %+v err=%v", back, err)
	}
}

func TestUnrecognizedStreamOutputWarnsOnce(t *testing.T) {
	for _, tc := range []struct {
		field string
		warn  bool
	}{{"content", false}, {"reasoning", false}, {"text", true}} {
		t.Run(tc.field, func(t *testing.T) {
			endpoint := &fakeEndpoint{textField: tc.field}
			srv := endpoint.serve(t)
			rec := &recorder{}
			Execute(context.Background(), Run{TokenizerRef: testTokenizerRef(t),
				ID: 9, Model: "m", Mode: "grid", Client: loadgen.Client{BaseURL: srv.URL},
				Dataset: json.RawMessage(`{"kind":"random","ignore_eos":true}`),
				Search:  json.RawMessage(`{"concurrency":[1,2],"isl":[8],"osl":[3],"requests_per_point_multiplier":1}`),
			}, rec.hooks())
			if done := rec.last(); done.State != "done" {
				t.Fatalf("terminal status = %+v", done)
			}
			warned := 0
			for _, code := range rec.warnings {
				if code == proto.BenchmarkWarningStreamOutputUnrecognized {
					warned++
				}
			}
			// One warning per point; the store keeps a code once per run.
			if (warned > 0) != tc.warn {
				t.Fatalf("field %q warnings = %v", tc.field, rec.warnings)
			}
		})
	}
}
