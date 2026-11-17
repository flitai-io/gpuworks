// Package benchrun executes one benchmark run for both load origins: the
// server drives it for server-origin runs and the agent for agent-origin
// runs. Planning points, warmup, generating and sending requests, and turning
// each finished point into the status.benchmark wire contract live here once;
// each origin supplies only where statuses, warnings and per-request detail go.
package benchrun

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync/atomic"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/pkg/bench/dataset"
	"gpuworks.ai/gpuworks/pkg/bench/loadgen"
	"gpuworks.ai/gpuworks/pkg/bench/metrics"
	"gpuworks.ai/gpuworks/pkg/bench/search"
	"gpuworks.ai/gpuworks/pkg/bench/tokenizer"
)

// Run is one benchmark run as stored: model, search mode and documents.
type Run struct {
	ID           uint
	Model        string
	Mode         string
	Dataset      json.RawMessage
	Search       json.RawMessage
	TokenizerRef string
	Client       loadgen.Client
	// NoStream sends non-streaming requests, which have no TTFT or TPOT. Only
	// the command-line bench offers it.
	NoStream bool
}

// Hooks are the origin-specific sinks. Report and Warn are required.
type Hooks struct {
	// Report receives every status of the run, terminal ones included.
	Report func(proto.BenchmarkStatus)
	// Warn records a run warning code once it applies.
	Warn func(code string)
	// Result receives every measured (non-warmup) request of point seq.
	Result func(seq int, r loadgen.Result)
	// DetailPath is called when point seq has finished, before its result is
	// reported; it returns the stored per-request detail, if any.
	DetailPath func(seq int) string
	// PointReported is called after point seq's result has been reported,
	// with that point's requests. Without it the point keeps no results: each
	// one only passes through Result.
	PointReported func(seq int, results []loadgen.Result)
}

// Execute runs every point the search plan yields and reports the terminal
// status. A canceled context returns without a terminal status: each origin
// records cancellation its own way.
func Execute(ctx context.Context, run Run, hooks Hooks) {
	e := &execution{run: run, hooks: hooks}
	if err := e.execute(ctx); err != nil && ctx.Err() == nil {
		status := proto.BenchmarkStatus{ID: run.ID, State: "failed", Message: err.Error(), StopReason: proto.BenchmarkStopFatal}
		status.Code, status.Params = failureCode(err)
		e.report(status)
	}
}

// codedFailure is a failure that knows its stable code, such as
// *dataset.LoadError.
type codedFailure interface {
	error
	Code() string
	Params() map[string]any
}

// failureCode is the code and params of the coded failure in err's chain;
// an uncoded failure reports none and is shown by its message.
func failureCode(err error) (string, map[string]any) {
	var coded codedFailure
	if errors.As(err, &coded) {
		return coded.Code(), coded.Params()
	}
	return "", nil
}

type execution struct {
	run             Run
	hooks           Hooks
	doc             search.Document
	dc              dataset.Config
	tok             *tokenizer.Tokenizer
	warmupSucceeded int
	warmupRecords   []loadgen.DetailRecord
}

func (e *execution) execute(ctx context.Context) error {
	if err := json.Unmarshal(e.run.Dataset, &e.dc); err != nil && len(e.run.Dataset) > 0 {
		return fmt.Errorf("invalid dataset config: %w", err)
	}
	if e.dc.Kind == "" {
		e.dc.Kind = "random"
	}
	doc, err := search.ParseDocument(e.run.Search)
	if err != nil {
		return fmt.Errorf("invalid search config: %w", err)
	}
	e.doc = doc
	if err := e.loadTokenizer(ctx); err != nil {
		return err
	}
	e.run.Client.HTTP = withRequestTimeout(e.run.Client.HTTP, doc.RequestTimeout())

	var points []search.Point
	var adaptive *search.Strategy
	if e.run.Mode != "" && e.run.Mode != string(search.Grid) {
		if adaptive, err = search.NewDocument(e.run.Mode, doc); err != nil {
			return err
		}
		if p, ok := adaptive.Next(); ok {
			points = append(points, p)
		}
	} else if points, err = doc.Expand(); err != nil {
		return err
	}
	if len(points) == 0 {
		return errors.New("search plan has no points")
	}
	if err := e.warmup(ctx, points[0]); err != nil {
		return err
	}
	for index := 0; index < len(points); index++ {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		observation, err := e.point(ctx, index, &points[index], len(points))
		if err != nil {
			return err
		}
		if adaptive != nil {
			adaptive.Observe(observation)
			if next, ok := adaptive.Next(); ok {
				points = append(points, next)
			}
		}
	}
	stopReason := proto.BenchmarkStopGridExhausted
	if adaptive != nil {
		stopReason = adaptive.StopReason()
	}
	e.report(proto.BenchmarkStatus{ID: e.run.ID, State: "done", PointIndex: len(points), PointTotal: len(points), PointCount: len(points), StopReason: stopReason})
	return nil
}

// pointDataset returns the dataset configuration of the point at index and
// fixes the point's lengths for agentic runs, whose input length is the
// workload's mean rather than a grid dimension.
func (e *execution) pointDataset(index int, point *search.Point) dataset.Config {
	if e.dc.Kind != "agentic" {
		e.dc.InputTokens = point.ISL
		e.dc.OutputTokens = point.OSL
	} else {
		// Every point draws fresh sessions; the stride keeps seeds of
		// neighbouring points far apart.
		e.dc.Seed += int64(index) * 10_000_019
		point.ISL = e.dc.MeanInputTokens
		point.OSL = e.dc.OutputTokens
	}
	return e.dc
}

func (e *execution) requests(dc dataset.Config, n int) ([]loadgen.Request, error) {
	if err := ValidateAllocation(dc, n); err != nil {
		return nil, err
	}
	samples, err := dataset.GenerateWithTokenizer(dc, n, e.tok)
	if err != nil {
		return nil, err
	}
	ignoreEOS := dc.EffectiveIgnoreEOS()
	out := make([]loadgen.Request, len(samples))
	for i, sample := range samples {
		messages := make([]loadgen.Message, len(sample.Messages))
		for j, m := range sample.Messages {
			messages[j] = loadgen.Message{Role: m.Role, Content: m.Content}
		}
		followUps := make([]loadgen.Message, len(sample.FollowUps))
		for j, m := range sample.FollowUps {
			followUps[j] = loadgen.Message{Role: m.Role, Content: m.Content}
		}
		maxTokens := dc.OutputTokens
		if sample.OutputTokens > 0 {
			maxTokens = sample.OutputTokens
		}
		out[i] = loadgen.Request{Model: e.run.Model, Messages: messages, FollowUps: followUps, MaxTokens: maxTokens, Stream: !e.run.NoStream, IgnoreEOS: ignoreEOS, DelayBefore: time.Duration(sample.WaitMillis) * time.Millisecond, StreamOptions: &loadgen.StreamOptions{IncludeUsage: true}}
	}
	return out, nil
}

// runner returns the load generator of a point: open loop by arrival rate, or
// closed loop at the point's concurrency. The resolved point seed must match
// the dataset and evidence, including when the run requested a random seed.
func (e *execution) runner(point search.Point, seed int64) loadgen.Runner {
	r := loadgen.Runner{Client: e.run.Client, Concurrency: max(1, point.Concurrency), DiscardResults: e.hooks.PointReported == nil}
	if e.tok != nil {
		r.CountTokens = e.tok.Count
	}
	if point.RequestRate > 0 {
		r.Arrival = &loadgen.Arrival{Rate: point.RequestRate, Burstiness: e.doc.EffectiveBurstiness(), Seed: seed}
		r.MaxInFlight = point.Concurrency
	}
	return r
}

func (e *execution) point(ctx context.Context, index int, point *search.Point, planned int) (search.Observation, error) {
	seq := index + 1
	dc := e.pointDataset(index, point)
	if dc.Seed == 0 {
		dc.Seed = newPointSeed()
	}
	n, err := e.doc.RequestsForPoint(*point)
	if err != nil {
		return search.Observation{}, err
	}
	reqs, err := e.requests(dc, n)
	if err != nil {
		return search.Observation{}, err
	}
	acc := metrics.New(10 * time.Second)
	requestTotal := loadgen.TotalRequests(reqs)
	current := &proto.BenchmarkCurrentPoint{Seq: seq, Concurrency: point.Concurrency, TargetISL: point.ISL, TargetOSL: point.OSL, RequestTotal: requestTotal, RequestRate: point.RequestRate}
	sample := func() json.RawMessage {
		snapshot := acc.ProgressSnapshot(time.Now().UTC())
		if snapshot.AggregationError != "" {
			return nil
		}
		b, _ := json.Marshal(snapshot.ProcessSample())
		return b
	}
	status := func(completed int, sample json.RawMessage) proto.BenchmarkStatus {
		return proto.BenchmarkStatus{ID: e.run.ID, State: "running", PointIndex: seq, PointTotal: planned, PointCount: planned, Completed: completed, CurrentPoint: current, Sample: sample}
	}
	e.report(status(0, sample()))
	runner := e.runner(*point, dc.Seed)
	var unrecognized atomic.Bool
	runner.OnResult = func(r loadgen.Result) {
		acc.Add(r.Metrics)
		if streamOutputUnrecognized(r) {
			unrecognized.Store(true)
		}
		if e.hooks.Result != nil {
			e.hooks.Result(seq, r)
		}
	}
	var completed atomic.Int64
	runner.OnProgress = func(done, _ int) { completed.Store(int64(done)) }
	stopProgress := e.reportProgress(func() {
		e.report(status(int(completed.Load()), sample()))
	}, func() int64 { return completed.Load() })
	started := time.Now().UTC()
	results := runner.Run(ctx, reqs)
	ended := time.Now().UTC()
	stopProgress()
	snapshot := acc.Snapshot(ended)
	requests := acc.Requests()
	peak := metrics.RollingPeakThroughput(requests, ended, 10*time.Second)
	averages := metrics.AverageRequestMetrics(requests)
	actualISL, actualOSL := snapshot.AverageTokenLengths()
	if unrecognized.Load() {
		e.hooks.Warn(proto.BenchmarkWarningStreamOutputUnrecognized)
	}
	if dc.Kind == "agentic" && dataset.AgenticLengthDriftExceeded(actualISL, dc.MeanInputTokens) {
		e.hooks.Warn(proto.BenchmarkWarningAgenticDrift)
	}
	detailPath := ""
	if e.hooks.DetailPath != nil {
		detailPath = e.hooks.DetailPath(seq)
	}
	ms := metrics.DurationMillis
	longTailDoc := LongTail(snapshot)
	longTailDoc.Evidence = e.pointEvidence(dc, reqs, requests, ctx.Err() != nil)
	longTail, _ := json.Marshal(longTailDoc)
	var result, final json.RawMessage
	if snapshot.AggregationError == "" {
		result, _ = json.Marshal(snapshot)
		final, _ = json.Marshal(snapshot.ProcessSample())
	}
	pointState := "done"
	if ctx.Err() != nil {
		pointState = "canceled"
	} else if snapshot.AggregationError != "" {
		pointState = "failed"
	}
	pointJSON, _ := json.Marshal(proto.BenchmarkPointResult{
		Seq: seq, Concurrency: point.Concurrency, TargetISL: point.ISL, TargetOSL: point.OSL, RequestRate: point.RequestRate,
		State: pointState, ActualISL: actualISL, ActualOSL: actualOSL, InputTokens: snapshot.InputTokens, OutputTokens: snapshot.OutputTokens,
		Success: snapshot.Success, Errors: snapshot.Errors,
		OutputTput: snapshot.OutputTput, TotalTput: snapshot.TotalTput, MaxOutputTput: peak.Output, MaxTotalTput: peak.Total, RequestTput: snapshot.RequestTput,
		AvgTTFT: averages.TTFT, P50TTFT: ms(snapshot.TTFT[50]), P99TTFT: ms(snapshot.TTFT[99]),
		AvgTPOT: averages.TPOT, P50TPOT: ms(snapshot.TPOT[50]), P99TPOT: ms(snapshot.TPOT[99]),
		AvgLatency: averages.Latency, P50Latency: ms(snapshot.Latency[50]), P99Latency: ms(snapshot.Latency[99]),
		LongTail: longTail, DetailPath: detailPath, StartedAt: &started, EndedAt: &ended,
	})
	done := status(snapshot.Count, final)
	done.Result, done.Point = result, pointJSON
	e.report(done)
	if e.hooks.PointReported != nil {
		e.hooks.PointReported(seq, results)
	}
	if ctx.Err() != nil {
		return search.Observation{}, ctx.Err()
	}
	if snapshot.AggregationError != "" {
		return search.Observation{}, fmt.Errorf("point %d: %s", seq, snapshot.AggregationError)
	}
	return search.WithEvidence(search.Observation{
		Point: *point, Throughput: snapshot.OutputTput, TotalThroughput: snapshot.TotalTput, RequestTput: snapshot.RequestTput,
		SuccessRate: float64(snapshot.Success) / float64(max(1, snapshot.Success+snapshot.Errors)),
		AvgTTFT:     averages.TTFT, P99TTFT: ms(snapshot.TTFT[99]),
		AvgTPOT: averages.TPOT, P50TPOT: ms(snapshot.TPOT[50]), P99TPOT: ms(snapshot.TPOT[99]),
		AvgLatency: averages.Latency, P99Latency: ms(snapshot.Latency[99]),
	}, longTail), nil
}

// progressInterval paces a point's running statuses. A request's completion
// only records the count; one reporter publishes the count and a live sample
// at most this often. Sampling on every completion summarizes every request
// so far each time: at hundreds of completions a second that outgrows the
// host's cores, and the samples piling up in the request goroutines exhausted
// the server's memory.
var progressInterval = time.Second

// reportProgress runs report every progressInterval while done has moved
// since the last report, until the returned stop is called; stop waits for a
// report in flight so none lands after the point's own result.
func (e *execution) reportProgress(report func(), done func() int64) (stop func()) {
	quit, exited := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(exited)
		ticker := time.NewTicker(progressInterval)
		defer ticker.Stop()
		var last int64
		for {
			select {
			case <-quit:
				return
			case <-ticker.C:
				if n := done(); n != last {
					last = n
					report()
				}
			}
		}
	}()
	return func() { close(quit); <-exited }
}

// LongTailDoc is a benchmark point's long_tail document as stored in
// benchmark_points.long_tail: every percentile of TTFT, TPOT, latency and ITL
// in milliseconds, keyed by percentile, plus cache and speculative-decoding
// facts. Its keys are the stored ones, which predate the API's snake_case
// convention; the API answers it as dto.BenchmarkLongTail.
type LongTailDoc struct {
	Evidence            *metrics.PointEvidence `json:"Evidence,omitempty"`
	TTFT                map[string]float64     `json:"TTFT"`
	TPOT                map[string]float64     `json:"TPOT"`
	Latency             map[string]float64     `json:"Latency"`
	ITL                 map[string]float64     `json:"ITL"`
	ApproximateTokens   bool                   `json:"ApproximateTokens"`
	KVHitRate           *float64               `json:"KVHitRate,omitempty"`
	CacheReportCoverage *float64               `json:"CacheReportCoverage,omitempty"`
	PeakInputTokens     int                    `json:"PeakInputTokens"`
	Speculative         *SpeculativeDoc        `json:"Speculative,omitempty"`
}

// SpeculativeDoc is LongTailDoc's speculative-decoding part.
type SpeculativeDoc struct {
	TokensPerIteration float64 `json:"TokensPerIteration"`
	AcceptanceRate     float64 `json:"AcceptanceRate"`
}

// LongTail is the point's long_tail document for a run's snapshot.
func LongTail(s metrics.Snapshot) LongTailDoc {
	convert := func(in map[int]time.Duration) map[string]float64 {
		out := map[string]float64{}
		for p, d := range in {
			out[fmt.Sprint(p)] = metrics.DurationMillis(d)
		}
		return out
	}
	out := LongTailDoc{
		TTFT: convert(s.TTFT), TPOT: convert(s.TPOT), Latency: convert(s.Latency), ITL: convert(s.ITL),
		ApproximateTokens: s.ApproximateTokens, KVHitRate: s.KVHitRate, CacheReportCoverage: s.CacheReportCoverage,
		PeakInputTokens: s.PeakInputTokens,
	}
	if s.Speculative != nil {
		out.Speculative = &SpeculativeDoc{TokensPerIteration: s.Speculative.TokensPerIteration, AcceptanceRate: s.Speculative.AcceptanceRate}
	}
	return out
}

// streamOutputUnrecognized reports a successful streamed request whose usage
// counts generated tokens although no chunk carried text the load generator
// recognizes: the stream names its text fields in a way the parser does not
// know, and the request has no first or last output time to sample.
func streamOutputUnrecognized(r loadgen.Result) bool {
	m := r.Metrics
	return r.Stream && m.Error == nil && !m.Canceled && m.FirstOutputAt.IsZero() && m.ServerOutputTokens != nil && *m.ServerOutputTokens > 0
}

// withRequestTimeout gives the inference requests of a run its configured
// timeout in place of the origin client's default, keeping the origin's
// transport and redirect policy. The origin's client is not modified.
func withRequestTimeout(client *http.Client, timeout time.Duration) *http.Client {
	if client == nil {
		return &http.Client{Timeout: timeout}
	}
	bounded := *client
	bounded.Timeout = timeout
	return &bounded
}
