package app

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestRunFlushesFinalProxyAccountingBeforeReturning is app.Run's shutdown
// sequencing test for the RunMetrics-vs-accountant race described on
// Run's metricsCtx comment: proxy.Accountant.Run's ctx.Done handler only
// enqueues its final flush into the metrics ingest queue partway through its
// own shutdown sequence (flush, close(stop), wait for its writer to drain
// and call the sink). If RunMetrics reacted to the same shutdown
// context, it could already have drained-and-exited before that enqueue,
// silently dropping it — Run must not return until this final flush has
// actually reached tsdb.
func TestRunFlushesFinalProxyAccountingBeforeReturning(t *testing.T) {
	dir := t.TempDir()
	a, err := New(Config{
		Addr: "127.0.0.1:0", DataDir: dir,
		DBPath:    filepath.Join(dir, "gpuworks.db"),
		AdminUser: "admin", AdminPassword: "gpuworks-test-password-2026",
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()

	// Wait for the listener so Run's background goroutines (the accountant
	// and the metrics ingest loop among them) are definitely started.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if addr := a.Addr(); strings.HasPrefix(addr, "127.0.0.1:") && !strings.HasSuffix(addr, ":0") {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Mark a deployment dirty well within the accountant's own 10s
	// auto-flush interval, so the only thing that can flush it during this
	// test is the shutdown path being exercised.
	const deploymentID = uint(4242)
	a.proxyAcct.RecordImmediate(deploymentID, 200, false)

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server did not stop")
	}

	series, err := a.obs.TSDB().QueryRange("gpuworks_proxy_requests_total",
		map[string]string{"deployment": strconv.FormatUint(uint64(deploymentID), 10)},
		0, time.Now().UnixMilli()+1000, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if len(series) == 0 {
		t.Fatal("the accountant's final flush (enqueued during shutdown) did not reach tsdb before Run returned")
	}
}
