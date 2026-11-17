package obsingest

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/tsdb"
)

// TestIngestMetricsEnqueuesInsteadOfWritingSynchronouslyWhenQueued pins the
// decoupling itself: with a queue wired, IngestMetrics must not touch tsdb at
// all - only RunMetrics (the sole writer) may.
func TestIngestMetricsEnqueuesInsteadOfWritingSynchronouslyWhenQueued(t *testing.T) {
	store := &fakeTSDBStore{}
	o := &Pipeline{
		tsdb: store, now: time.Now, deployCache: map[string]deploymentLookup{},
		metricsQueue: make(chan []tsdb.Sample, 4),
	}
	o.IngestMetrics("node-1", []proto.MetricSample{{Metric: "m", Labels: map[string]string{"gpu": "0"}, Value: 1, TimeMS: 1000}})

	if got := store.samples(); len(got) != 0 {
		t.Fatalf("IngestMetrics wrote synchronously with a queue wired: %+v", got)
	}
	select {
	case batch := <-o.metricsQueue:
		if len(batch) != 1 || batch[0].Metric != "m" || batch[0].Labels["gpu"] != "0" {
			t.Fatalf("queued batch = %+v", batch)
		}
	default:
		t.Fatal("nothing was enqueued")
	}
}

// TestIngestMetricsSynchronousFallbackWhenQueueNil re-covers the existing
// (pre-queue) synchronous contract: tests and any other caller that builds an
// observability without a queue must keep working exactly as before.
func TestIngestMetricsSynchronousFallbackWhenQueueNil(t *testing.T) {
	o, store, _ := newTestObservability(map[string]uint{"inst-1": 7})
	o.IngestMetrics("node-1", []proto.MetricSample{{Metric: "m", Labels: map[string]string{"instance": "inst-1"}, Value: 1, TimeMS: 1000}})
	got := store.samples()
	if len(got) != 1 || got[0].Labels["deployment"] != "7" {
		t.Fatalf("synchronous fallback did not write+backfill: %+v", got)
	}
}

// TestIngestMetricsDropsWholeBatchWhenQueueFull covers overflow: a full queue
// drops the whole incoming batch (not a partial write) and counts every
// dropped sample, rather than blocking the caller (the hub's ctrl read loop).
func TestIngestMetricsDropsWholeBatchWhenQueueFull(t *testing.T) {
	store := &fakeTSDBStore{}
	o := &Pipeline{
		tsdb: store, now: time.Now, deployCache: map[string]deploymentLookup{},
		metricsQueue: make(chan []tsdb.Sample, 1),
	}
	o.IngestMetrics("node-1", []proto.MetricSample{{Metric: "m", Value: 1, TimeMS: 1000}})
	o.IngestMetrics("node-1", []proto.MetricSample{
		{Metric: "m", Value: 2, TimeMS: 2000},
		{Metric: "m", Value: 3, TimeMS: 3000},
	})

	if got := o.metricsDropped.Load(); got != 2 {
		t.Fatalf("metricsDropped = %d, want 2", got)
	}
	if len(o.metricsQueue) != 1 {
		t.Fatalf("queue length = %d, want 1 (only the first batch kept)", len(o.metricsQueue))
	}
}

// TestWarnMetricsDropRateLimitsLoggingButCountsEveryDrop covers item 9: every
// dropped sample must still be counted (metricsDropped), but the overflow
// warning itself must be rate-limited to at most once per
// metricsDropWarnInterval, not once per dropped batch.
func TestWarnMetricsDropRateLimitsLoggingButCountsEveryDrop(t *testing.T) {
	store := &fakeTSDBStore{}
	now := time.Unix(1_700_000_000, 0)
	o := &Pipeline{
		tsdb: store, now: func() time.Time { return now }, deployCache: map[string]deploymentLookup{},
		metricsQueue: make(chan []tsdb.Sample, 1),
	}

	var logs bytes.Buffer
	oldLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(oldLogger) })

	// Fill the queue once so every subsequent IngestMetrics call drops.
	o.IngestMetrics("n", []proto.MetricSample{{Metric: "m", Value: 1, TimeMS: 1}})

	// Three drops in quick succession (same `now`), well within
	// metricsDropWarnInterval: only the first may actually log.
	o.IngestMetrics("n", []proto.MetricSample{{Metric: "m", Value: 2, TimeMS: 2}})
	o.IngestMetrics("n", []proto.MetricSample{{Metric: "m", Value: 3, TimeMS: 3}})
	o.IngestMetrics("n", []proto.MetricSample{{Metric: "m", Value: 4, TimeMS: 4}})

	if got := o.metricsDropped.Load(); got != 3 {
		t.Fatalf("metricsDropped = %d, want 3 (every dropped sample counted)", got)
	}
	if n := strings.Count(logs.String(), "metrics ingest queue full"); n != 1 {
		t.Fatalf("logged %d times within the rate-limit window, want 1: %s", n, logs.String())
	}

	// Past the interval: the next drop must log again.
	now = now.Add(metricsDropWarnInterval + time.Second)
	o.IngestMetrics("n", []proto.MetricSample{{Metric: "m", Value: 5, TimeMS: 5}})
	if got := o.metricsDropped.Load(); got != 4 {
		t.Fatalf("metricsDropped = %d, want 4", got)
	}
	if n := strings.Count(logs.String(), "metrics ingest queue full"); n != 2 {
		t.Fatalf("logged %d times after the rate-limit window elapsed, want 2: %s", n, logs.String())
	}
}

// waitForSamples polls store.samples() until it has n entries or the deadline
// passes, returning the last observed slice.
func waitForSamples(t *testing.T, store *fakeTSDBStore, n int, timeout time.Duration) []tsdb.Sample {
	t.Helper()
	deadline := time.After(timeout)
	for {
		got := store.samples()
		if len(got) >= n {
			return got
		}
		select {
		case <-deadline:
			return got
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// TestMetricsIngestLoopFlushesWhenRowThresholdReached covers the "batching up
// to N samples" half of the flush policy: a single queued batch at or above
// metricsIngestFlushRows must be written without waiting for the flush timer.
func TestMetricsIngestLoopFlushesWhenRowThresholdReached(t *testing.T) {
	store := &fakeTSDBStore{}
	o := &Pipeline{
		tsdb: store, now: time.Now, deployCache: map[string]deploymentLookup{},
		metricsQueue: make(chan []tsdb.Sample, 2),
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { o.RunMetrics(ctx); close(done) }()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	big := make([]tsdb.Sample, metricsIngestFlushRows)
	for i := range big {
		big[i] = tsdb.Sample{Metric: "m", Labels: map[string]string{"i": fmt.Sprint(i)}, Value: 1, TimeMS: 1000}
	}
	o.metricsQueue <- big

	got := waitForSamples(t, store, metricsIngestFlushRows, 2*time.Second)
	if len(got) != metricsIngestFlushRows {
		t.Fatalf("flush did not happen on row threshold; got %d samples, want %d", len(got), metricsIngestFlushRows)
	}
}

// TestMetricsIngestLoopFlushesOnTimerWhenBelowRowThreshold covers the "or
// 200ms" half: a small batch is not written immediately, but is written once
// the flush timer fires.
func TestMetricsIngestLoopFlushesOnTimerWhenBelowRowThreshold(t *testing.T) {
	store := &fakeTSDBStore{}
	o := &Pipeline{
		tsdb: store, now: time.Now, deployCache: map[string]deploymentLookup{},
		metricsQueue: make(chan []tsdb.Sample, 2),
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { o.RunMetrics(ctx); close(done) }()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	o.metricsQueue <- []tsdb.Sample{{Metric: "m", Value: 1, TimeMS: 1000}}

	// Well under the flush interval: nothing should be written yet.
	time.Sleep(50 * time.Millisecond)
	if got := store.samples(); len(got) != 0 {
		t.Fatalf("flushed before the timer fired: %+v", got)
	}

	got := waitForSamples(t, store, 1, 2*time.Second)
	if len(got) != 1 {
		t.Fatal("timer flush did not happen")
	}
}

// TestMetricsIngestLoopDrainsQueuedBatchesOnContextDone covers the "final
// drain on ctx done" requirement: cancelling before the flush timer fires
// must still write everything already queued, rather than losing it.
func TestMetricsIngestLoopDrainsQueuedBatchesOnContextDone(t *testing.T) {
	store := &fakeTSDBStore{}
	o := &Pipeline{
		tsdb: store, now: time.Now, deployCache: map[string]deploymentLookup{},
		metricsQueue: make(chan []tsdb.Sample, 2),
	}
	o.metricsQueue <- []tsdb.Sample{{Metric: "m", Value: 1, TimeMS: 1000}}
	o.metricsQueue <- []tsdb.Sample{{Metric: "m", Value: 2, TimeMS: 2000}}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { o.RunMetrics(ctx); close(done) }()
	time.Sleep(10 * time.Millisecond) // well under metricsIngestFlushEvery
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("loop did not exit after ctx cancellation")
	}
	if got := store.samples(); len(got) != 2 {
		t.Fatalf("final drain wrote %d samples, want 2: %+v", len(got), got)
	}
	// The ctx.Done drain path must count what it took, the same as the
	// steady-state path does, so the pipeline-health log (observe tick) does
	// not under-report during shutdown.
	if got := o.metricsTaken.Load(); got != 2 {
		t.Fatalf("metricsTaken = %d, want 2 (drained on the ctx.Done path)", got)
	}
}

// TestMetricsIngestLoopAppliesDeploymentLabelAndQuantiles is the integration
// check that the loop's flush path - not just the synchronous fallback - runs
// both withDeploymentLabel and applyQuantiles before WriteBatch.
func TestMetricsIngestLoopAppliesDeploymentLabelAndQuantiles(t *testing.T) {
	store := &fakeTSDBStore{}
	o := &Pipeline{
		tsdb: store, now: time.Now, deployCache: map[string]deploymentLookup{},
		bucketState:  map[string]*bucketVector{},
		metricsQueue: make(chan []tsdb.Sample, 4),
		resolveDeployment: func(instanceUUID string) (uint, bool, bool) {
			return 42, true, true
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { o.RunMetrics(ctx); close(done) }()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	o.metricsQueue <- []tsdb.Sample{
		{Metric: "m", Labels: map[string]string{"instance": "inst-1"}, Value: 1, TimeMS: 1000},
		{Metric: ttftFamily + "_bucket", Labels: map[string]string{"instance": "inst-1", "le": "1"}, Value: 5, TimeMS: 1000},
	}

	got := waitForSamples(t, store, 1, 2*time.Second)
	var sawPlain bool
	for _, s := range got {
		if s.Metric == "m" {
			sawPlain = true
			if s.Labels["deployment"] != "42" {
				t.Fatalf("deployment label not backfilled on the loop path: %+v", s)
			}
		}
		if s.Metric == ttftFamily+"_bucket" {
			t.Fatalf("a _bucket sample reached tsdb via the loop path: %+v", s)
		}
	}
	if !sawPlain {
		t.Fatalf("expected the plain sample to be written, got %+v", got)
	}
}
