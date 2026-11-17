package api

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"gpuworks.ai/gpuworks/internal/benchrun"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/pkg/bench/loadgen"
	"gpuworks.ai/gpuworks/pkg/bench/metrics"
)

// runServerBenchmark drives a server-origin run. Statuses go straight to the
// store, process samples to the curve and the metrics TSDB, and per-request
// detail to the server's artifact directory.
func (a *apiHandlers) runServerBenchmark(ctx context.Context, run *store.BenchmarkRun) {
	client := a.benchmarkHTTPClient(run)
	defer client.CloseIdleConnections()
	apiKey, _ := store.DecryptAIKey(a.d.AISecretbox, run.APIKey)
	// Only this server's own gateway is told the run, and only its answer
	// headers say which class a request ran at.
	own := a.benchmarkObservesPriorityClass(run)
	var detailMu sync.Mutex
	var detail *benchmarkDetail
	detailSeq := 0
	benchrun.Execute(ctx, benchrun.Run{
		ID: run.ID, Model: run.Model, Mode: run.Mode,
		Dataset: json.RawMessage(run.Dataset), Search: json.RawMessage(run.Search), TokenizerRef: run.TokenizerRef,
		Client: loadgen.Client{HTTP: client, BaseURL: run.BaseURL, APIKey: apiKey, Header: benchmarkRequestHeader(run.ID, run.PriorityClass, own), ObservePriorityClass: own},
	}, benchrun.Hooks{
		Report: func(st proto.BenchmarkStatus) {
			if st.CurrentPoint != nil && !st.CurrentPoint.Warmup && len(st.Sample) > 0 {
				a.writeBenchmarkSample(run.ID, st.PointIndex, st.Sample)
			}
			_ = a.d.Store.ApplyBenchmarkStatus(0, st)
		},
		Warn: func(code string) { _ = a.d.Store.AddBenchmarkWarning(run.ID, code) },
		Result: func(seq int, r loadgen.Result) {
			detailMu.Lock()
			defer detailMu.Unlock()
			if detailSeq != seq {
				detail, _ = newBenchmarkDetail(a.d.BenchmarkArtifactDir, run.ID, seq, run.KeepDetail, run.DetailCapMB)
				detailSeq = seq
			}
			detail.Add(r)
		},
		DetailPath: func(seq int) string {
			detailMu.Lock()
			defer detailMu.Unlock()
			if detailSeq != seq {
				return ""
			}
			path, _ := detail.Close()
			return path
		},
	})
}

// writeBenchmarkSample appends one process sample of point seq to the stored
// curve and, when a metrics sink is wired, to the TSDB.
func (a *apiHandlers) writeBenchmarkSample(runID uint, seq int, raw json.RawMessage) {
	var sample metrics.ProcessSample
	if json.Unmarshal(raw, &sample) != nil {
		return
	}
	now := time.Now().UTC()
	_ = a.d.Store.AppendBenchmarkCurveSample(runID, seq, now, sample)
	if a.d.MetricsIngest == nil {
		return
	}
	labels := map[string]string{"benchmark": fmt.Sprint(runID), "point": fmt.Sprint(seq), "node": "server"}
	ms := now.UnixMilli()
	a.d.MetricsIngest("server", []proto.MetricSample{
		{Metric: "gpuworks_bench_output_tput", Labels: labels, Value: sample.OutputTput, TimeMS: ms},
		{Metric: "gpuworks_bench_total_tput", Labels: labels, Value: sample.TotalTput, TimeMS: ms},
		{Metric: "gpuworks_bench_request_tput", Labels: labels, Value: sample.RequestTput, TimeMS: ms},
		{Metric: "gpuworks_bench_ttft_p50_ms", Labels: labels, Value: sample.TTFTP50, TimeMS: ms},
		{Metric: "gpuworks_bench_tpot_p50_ms", Labels: labels, Value: sample.TPOTP50, TimeMS: ms},
		{Metric: "gpuworks_bench_latency_p99_ms", Labels: labels, Value: sample.LatencyP99, TimeMS: ms},
	})
}
