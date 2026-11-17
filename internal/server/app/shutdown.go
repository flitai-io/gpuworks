package app

import (
	"context"
	"sync"
	"time"
)

// backgroundGroup tracks the loops Run starts on its shared ctx (other than
// proxyAcct and RunMetrics, which have their own established stop
// ordering - see stopProxyAccounting/stopMetricsIngest), so Run can wait for
// every one of them to actually return, on every exit path, before its
// deferred a.proxy.Close() runs (runPDVerifier uses a.proxy).
type backgroundGroup struct {
	wg sync.WaitGroup
}

// start runs fn in a goroutine tracked by the group.
func (g *backgroundGroup) start(fn func()) {
	g.wg.Add(1)
	go func() {
		defer g.wg.Done()
		fn()
	}()
}

// wait blocks until every goroutine started by start has returned.
func (g *backgroundGroup) wait() {
	g.wg.Wait()
}

// stopProxyAccounting waits (briefly) for the proxy accounting flush loop to
// return after ctx is done, so its final flush is fully enqueued into the
// metrics ingest queue before this App's caller is free to close the metrics
// store out from under it. It does NOT by itself guarantee that flush has
// reached tsdb: RunMetrics runs on its own context precisely so it
// can be stopped afterward (see stopMetricsIngest), once this enqueue is
// known to have already happened. The wait here is bounded: a sink stuck
// writing to tsdb must not hang server shutdown.
func (a *App) stopProxyAccounting(done <-chan struct{}) {
	if awaitWithTimeout(done, 2*time.Second) {
		a.log.Warn("proxy accounting did not stop within the shutdown grace period")
	}
}

// stopMetricsIngest cancels RunMetrics's own context and waits
// (briefly) for its final drain, so a proxy-accounting flush enqueued during
// stopProxyAccounting's wait is not silently dropped by an ingest loop that
// had already exited on the shared shutdown context. Must be called after
// stopProxyAccounting returns, never before or concurrently with it - see
// Run's metricsCtx comment for why the ordering itself is the fix. The wait
// is bounded: an ingest loop stuck writing to tsdb must not hang shutdown.
func (a *App) stopMetricsIngest(cancel context.CancelFunc, done <-chan struct{}) {
	cancel()
	if awaitWithTimeout(done, 2*time.Second) {
		a.log.Warn("metrics ingest did not stop within the shutdown grace period")
	}
}

// awaitWithTimeout waits for done to close, returning false, or gives up
// after timeout and returns true.
func awaitWithTimeout(done <-chan struct{}, timeout time.Duration) (timedOut bool) {
	select {
	case <-done:
		return false
	case <-time.After(timeout):
		return true
	}
}
