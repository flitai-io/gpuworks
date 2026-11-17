package app

import (
	"context"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/obsingest"
	"gpuworks.ai/gpuworks/internal/server/proxy"
	"gpuworks.ai/gpuworks/internal/server/tsdb"
)

// TestProxyMetricsSinkAdaptsSamplesToIngestMetrics verifies the app.go
// wiring between proxy.Accountant's flush batches and observability's tsdb
// write path: labels and values must survive the tsdb.Sample ->
// proto.MetricSample -> tsdb.Sample round trip unchanged.
func TestProxyMetricsSinkAdaptsSamplesToIngestMetrics(t *testing.T) {
	fake := &fakeTSDBStore{}
	obs := &observability{obsingest.New(obsingest.Stores{TSDB: fake})}
	sink := proxyMetricsSink(obs)

	sink([]tsdb.Sample{
		{Metric: "gpuworks_proxy_requests_total", Labels: map[string]string{"deployment": "3", "class": "2xx", "node": "server"}, Value: 5, TimeMS: 1000},
	})

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.written) != 1 {
		t.Fatalf("written = %d samples, want 1", len(fake.written))
	}
	got := fake.written[0]
	if got.Metric != "gpuworks_proxy_requests_total" || got.Value != 5 || got.TimeMS != 1000 {
		t.Fatalf("sample = %+v, want metric/value/time preserved", got)
	}
	if got.Labels["deployment"] != "3" || got.Labels["class"] != "2xx" || got.Labels["node"] != "server" {
		t.Fatalf("labels = %v, want deployment/class/node preserved", got.Labels)
	}
}

// TestAwaitWithTimeoutReturnsPromptlyWhenDoneCloses is the fast-path half of
// item 12: an ordinary shutdown must not wait the full grace period once the
// accounting loop has actually stopped.
func TestAwaitWithTimeoutReturnsPromptlyWhenDoneCloses(t *testing.T) {
	done := make(chan struct{})
	go func() {
		time.Sleep(20 * time.Millisecond)
		close(done)
	}()
	start := time.Now()
	if timedOut := awaitWithTimeout(done, 2*time.Second); timedOut {
		t.Fatal("should not have timed out: done closed well within the timeout")
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("awaitWithTimeout took %v, want close to when done closed (20ms), not the 2s timeout", elapsed)
	}
}

// TestAwaitWithTimeoutTimesOutWhenDoneNeverCloses is the bounded-wait unit
// test's other half: awaitWithTimeout must give up and report true once the
// timeout elapses when done is never closed.
func TestAwaitWithTimeoutTimesOutWhenDoneNeverCloses(t *testing.T) {
	done := make(chan struct{}) // deliberately never closed
	start := time.Now()
	if timedOut := awaitWithTimeout(done, 150*time.Millisecond); !timedOut {
		t.Fatal("expected awaitWithTimeout to time out when done never closes")
	}
	if elapsed := time.Since(start); elapsed < 150*time.Millisecond || elapsed > 600*time.Millisecond {
		t.Fatalf("awaitWithTimeout took %v, want close to the 150ms timeout", elapsed)
	}
}

// TestProxyAccountingRunReturnsPromptlyEvenWithAStuckSink is the app-level
// side of the proxy package's N3 fix: Accountant.Run must return as soon as
// its own stop signal is closed, without waiting for its writer goroutine —
// a sink stuck writing to tsdb must not keep Run (and, transitively,
// App.Run's stopProxyAccounting wait, which is bounded by the same
// 2s window) blocked. Previously Run waited for its writer before
// returning, so a stuck sink kept Run alive past that 2s bound, and a
// RestartServer could then start a second, concurrent Run on the same
// Accountant.
func TestProxyAccountingRunReturnsPromptlyEvenWithAStuckSink(t *testing.T) {
	blockSink := make(chan struct{})
	acct := proxy.NewAccountant(func([]tsdb.Sample) {
		<-blockSink // simulate a sink stuck writing to tsdb
	}, nil)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		acct.Run(ctx)
	}()
	acct.Dispatch(1)
	acct.Finish(1, 200, time.Millisecond)
	cancel() // triggers the final flush; the writer picks it up and blocks in the sink

	if timedOut := awaitWithTimeout(done, 2*time.Second); timedOut {
		t.Fatal("Accountant.Run should return promptly even though its writer is stuck in a slow sink")
	}

	close(blockSink) // release the stuck sink so its goroutine doesn't leak
}
