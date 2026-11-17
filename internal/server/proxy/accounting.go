package proxy

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"gpuworks.ai/gpuworks/internal/inferencemetrics"
	"gpuworks.ai/gpuworks/internal/server/tsdb"
)

// Metric and label names are fixed by the proxy metrics contract
// (artifacts/archive/2026-09-19-monitor-center/plan.md §4.3): the monitor package and
// its tests key off these exact strings.
// The inference series among them are named by the vocabulary
// (inferencemetrics.GatewayDefinitions); the rest are the gateway's own.
var (
	metricRequestsTotal = inferencemetrics.Gateway("requests_total").Series()
	metricDurationSum   = inferencemetrics.Gateway("request_duration_ms").Series() + "_sum"
	metricDurationCount = inferencemetrics.Gateway("request_duration_ms").Series() + "_count"
	metricInflight      = inferencemetrics.Gateway("requests_inflight").Series()
)

const (
	metricAccountingDroppedTotal = "gpuworks_proxy_accounting_dropped_total"
	metricRoutedTotal            = "gpuworks_proxy_routed_total"
	metricRetriesTotal           = "gpuworks_proxy_retries_total"
	serverNodeLabel              = "server"

	// flushInterval is how often accumulated counters are converted to tsdb
	// samples and handed to the sink.
	flushInterval = 10 * time.Second
	// sinkQueueDepth bounds the number of pending flush batches: the sink
	// (ultimately a SQLite write) can stall or run slow without the flush
	// ticker itself ever blocking. A batch is at most a few dozen samples (one
	// per active deployment x class, plus 3 per active deployment), so a
	// handful of pending batches is already many seconds of buffering.
	sinkQueueDepth = 8
	// dropWarnEvery rate-limits the "sink is dropping batches" log: the first
	// drop always warns (so an operator sees it immediately), then every
	// dropWarnEvery-th drop after that, so a stuck sink logs periodically
	// without flooding at 1 line per 10s flush forever.
	dropWarnEvery = 100
)

// reqClass is one of the fixed accounting classes from the metrics contract.
type reqClass string

const (
	class2xx    reqClass = "2xx"
	class4xx    reqClass = "4xx"
	class429    reqClass = "429"
	class5xx    reqClass = "5xx"
	classAsleep reqClass = "asleep_503"
)

// classifyStatus buckets an upstream/rejection HTTP status into one of the
// fixed accounting classes. asleep must be checked by the caller and passed
// in explicitly (via a sentinel flag from the selection path, never by
// matching the rejection message text) since an asleep rejection is also a
// plain 503 that would otherwise fall into class5xx.
//
// A 101 Switching Protocols response (a successful WebSocket/h2c upgrade
// handshake) is folded into class2xx: it is the closest the fixed class set
// has to "the upgrade succeeded", and without this a WebSocket-only
// deployment would show zero requests despite serving traffic.
//
// ok is false for status codes the contract does not define a class for
// (other 1xx/3xx, or zero e.g. a request that never received any upstream
// status). Such requests still complete for duration/in-flight purposes;
// they just never bump a requests_total series.
func classifyStatus(status int, asleep bool) (reqClass, bool) {
	switch {
	case asleep:
		return classAsleep, true
	case status == http.StatusSwitchingProtocols:
		return class2xx, true
	case status == http.StatusTooManyRequests:
		return class429, true
	case status >= 200 && status < 300:
		return class2xx, true
	case status >= 400 && status < 500:
		return class4xx, true
	case status >= 500:
		return class5xx, true
	default:
		return "", false
	}
}

// deploymentCounters is one deployment's accumulated accounting state.
// Every counter is cumulative for the life of the process (never reset after
// a flush) so flushed tsdb samples are a monotonically increasing counter
// series, matching Prometheus counter semantics.
type deploymentCounters struct {
	classTotals   map[reqClass]uint64
	durationSumMs float64
	durationCount uint64
	inflight      int64
	lastRequestAt time.Time
	// response is the replies' duration buckets, first tokens and usage
	// (accounting_usage.go).
	response responseCounters
	// dirty marks that this deployment saw activity (a Dispatch, Finish or
	// RecordImmediate call) since the last flush. flush() emits a
	// deployment's samples when it is dirty OR it currently has requests in
	// flight (see snapshotDirty), and only clears dirty once that batch has
	// actually been handed to the sink queue — a batch dropped under
	// backpressure leaves dirty set so the same counts are retried on the
	// next flush instead of silently lost (see flush's doc comment).
	dirty bool
}

// Accountant is the in-memory proxy request accounting described in the
// proxy metrics contract (artifacts/archive/2026-09-19-monitor-center/plan.md §4.3).
// It counts completed requests and their duration per deployment (+class for
// the totals), tracks a live per-deployment in-flight gauge, and periodically
// converts that state into tsdb.Samples handed to an injected sink. It never
// touches the store, the request or response bodies, or blocks the request
// path: Dispatch/Finish/RecordImmediate only ever take an in-process mutex,
// and the sink itself is called off a dedicated writer goroutine fed by a
// bounded queue (the same bounded-queue + single-writer + drop-counter shape
// as the log-center ingest loop), so a slow or stuck sink drops flush batches
// instead of stalling accounting or, worse, the proxy's request-handling
// goroutines.
//
// Duration (Finish's duration, and the duration_sum/count series) measures
// only the dispatched leg: from the moment a request is handed to an
// upstream instance (Dispatch) to the moment its response finishes. It
// deliberately excludes authentication, request-body spooling/model
// extraction and deployment/instance selection, which happen before
// Dispatch — those are proxy-side overhead, not upstream latency, and mixing
// them in would make duration_sum/count a worse proxy for "how slow is the
// engine" than for "how slow is gpuworks". RecordImmediate's duration
// argument is accepted for API symmetry but is never added to
// duration_sum/count (see record's doc comment); those requests still bump
// requests_total.
//
// counters is never pruned: an id, once seen, keeps its entry (and its
// cumulative counters) for the life of the process. That is intentional —
// counters must stay monotonic for the tsdb series they feed — and it is
// bounded in practice by the number of distinct deployment ids a store can
// ever hand out, not by request volume.
//
// A nil *Accountant is valid and every method no-ops (or answers "unknown"),
// so a Proxy that never calls SetAccountant behaves exactly as before this
// type existed.
type Accountant struct {
	// traffic is serving bytes per node (serving_traffic.go), reported
	// at each flush.
	traffic  *servingTraffic
	mu       sync.Mutex
	counters map[uint]*deploymentCounters
	now      func() time.Time

	sink  func([]tsdb.Sample)
	log   *slog.Logger
	queue chan []tsdb.Sample
	// dropped is the lifetime count of flush batches discarded because the
	// sink queue was full. It is never reset, so the
	// gpuworks_proxy_accounting_dropped_total sample flush() emits is itself
	// a monotonic counter like everything else here.
	dropped atomic.Uint64
	// lastEmittedDropped is the dropped total already included in a
	// successfully sent batch; flush() only appends a fresh
	// gpuworks_proxy_accounting_dropped_total sample when the live count has
	// moved past it, so a drop is reported once (per change), not on every
	// tick forever. Only ever touched from flush(), which per Run's contract
	// is never called concurrently with itself.
	lastEmittedDropped uint64
	routed             map[string]uint64
	retries            map[string]uint64
	routedSeen         map[string]time.Time
	retriesSeen        map[string]time.Time
	// pd holds the PD gateway counters (accounting_pd.go).
	pd map[uint]*pdCounters

	// runMu guards writerDone below; it is deliberately separate from mu so
	// Run's startup bookkeeping never contends with the request-path
	// Dispatch/Finish/RecordImmediate hot path.
	runMu sync.Mutex
	// writerDone is the completion signal of the most recently started writer
	// goroutine (see Run/writeLoop). A new Run waits briefly on it before
	// starting its own writer, so a previous Run's writer — possibly still
	// blocked inside a stuck sink call — can never run concurrently with a
	// new one; two live writers would both be allowed to call sink, breaking
	// the single-writer guarantee sink's callers (e.g. observability's
	// SQLite write path) rely on.
	writerDone chan struct{}

	// afterSnapshotAndClearForTest, when set, is invoked by flush()
	// immediately after snapshotAndClearDirty returns (and unlocks), before
	// the queue send is attempted. It exists solely so tests can
	// deterministically land a Dispatch/Finish/RecordImmediate exactly in
	// that window instead of relying on goroutine-scheduling luck to catch a
	// regression of the interleaving snapshotAndClearDirty's single lock
	// scope prevents. Always nil outside tests.
	afterSnapshotAndClearForTest func()
}

// previousWriterStopTimeout bounds how long a new Run waits for a previous
// Run's writer goroutine to finish before starting its own (see
// awaitPreviousWriter) — the same bound App.Run itself uses when waiting for
// Run to return, so a restart's total worst-case stall stays on that order
// rather than compounding.
const previousWriterStopTimeout = 2 * time.Second

const (
	accountingSeriesRetention = 24 * time.Hour
	maxAccountingSeries       = 4096
)

// finalFlushWaitTimeout bounds how long Run waits, on its own shutdown, for
// its writer to actually drain the final flush before returning (see Run).
// Short relative to previousWriterStopTimeout: an ordinary sink call is a
// synchronous, near-instant append (a metrics store write, or a test's own
// in-memory slice), so this only ever adds real latency to shutdown when the
// sink is genuinely stuck - and in that case Run must still return promptly
// rather than compound with previousWriterStopTimeout's own wait on a
// following Run.
const finalFlushWaitTimeout = 200 * time.Millisecond

// NewAccountant builds an Accountant that flushes to sink every 10s once Run
// is called. sink must be safe to call from a dedicated goroutine; it is
// never called concurrently with itself. log receives a rate-limited warning
// when the sink queue is under backpressure; a nil log discards it.
func NewAccountant(sink func([]tsdb.Sample), log *slog.Logger) *Accountant {
	return newAccountant(sink, sinkQueueDepth, log)
}

// newAccountant is the unexported constructor tests use to pick a small queue
// depth so backpressure/drop behavior is reachable without pushing thousands
// of flushes through a real-sized queue.
func newAccountant(sink func([]tsdb.Sample), queueDepth int, log *slog.Logger) *Accountant {
	if queueDepth <= 0 {
		queueDepth = 1
	}
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Accountant{
		counters: map[uint]*deploymentCounters{},
		traffic:  newServingTraffic(),
		now:      time.Now,
		sink:     sink,
		log:      log,
		// queue is created once and lives for the Accountant's whole life; it
		// is never closed (see Run's doc comment), so Run can be started and
		// stopped any number of times without ever risking a
		// send-on-closed-channel or double-close panic.
		queue: make(chan []tsdb.Sample, queueDepth), routed: make(map[string]uint64), retries: make(map[string]uint64), routedSeen: make(map[string]time.Time), retriesSeen: make(map[string]time.Time),
	}
}

// clock returns a's time source, defaulting to time.Now.
func (a *Accountant) clock() time.Time {
	if a.now != nil {
		return a.now()
	}
	return time.Now()
}

func (a *Accountant) deploymentLocked(id uint) *deploymentCounters {
	c, ok := a.counters[id]
	if !ok {
		c = &deploymentCounters{classTotals: map[reqClass]uint64{}}
		a.counters[id] = c
	}
	return c
}

// Dispatch marks a request as handed off to an upstream instance for
// deploymentID, incrementing its in-flight gauge. Every Dispatch must be
// matched by exactly one later Finish call for the same deploymentID, even
// when the copy fails or the client aborts. It also advances lastRequestAt,
// so a long-lived streaming request keeps idle detection from firing for as
// long as it is in flight, not only once it finishes.
func (a *Accountant) Dispatch(deploymentID uint) {
	if a == nil {
		return
	}
	a.mu.Lock()
	c := a.deploymentLocked(deploymentID)
	c.inflight++
	c.lastRequestAt = a.clock()
	c.dirty = true
	a.mu.Unlock()
}

// RecordRouted counts the instance selected for a client request. It is a
// separate counter from requests_total, which remains one sample per client
// request even when transport retries occur.
func (a *Accountant) RecordRouted(deploymentID uint, instance string) {
	if a == nil || instance == "" {
		return
	}
	a.mu.Lock()
	if a.routed == nil {
		a.routed = make(map[string]uint64)
	}
	key := strconv.FormatUint(uint64(deploymentID), 10) + "\x00" + instance
	a.routed[key]++
	if a.routedSeen == nil {
		a.routedSeen = make(map[string]time.Time)
	}
	a.routedSeen[key] = a.clock()
	a.deploymentLocked(deploymentID).dirty = true
	a.mu.Unlock()
}

func (a *Accountant) RecordRetry(deploymentID uint, reason string) {
	if a == nil {
		return
	}
	a.mu.Lock()
	if a.retries == nil {
		a.retries = make(map[string]uint64)
	}
	key := strconv.FormatUint(uint64(deploymentID), 10) + "\x00" + reason
	a.retries[key]++
	if a.retriesSeen == nil {
		a.retriesSeen = make(map[string]time.Time)
	}
	a.retriesSeen[key] = a.clock()
	a.deploymentLocked(deploymentID).dirty = true
	a.mu.Unlock()
}

// Finish records the completion of a request previously handed to Dispatch:
// status is the upstream HTTP status (whatever headers actually arrived, even
// if the body copy was later truncated or the client aborted mid-stream —
// classification is by the upstream status the client was told, not by
// whether the copy finished cleanly), duration is measured from Dispatch to
// now. It decrements the in-flight gauge.
func (a *Accountant) Finish(deploymentID uint, status int, duration time.Duration) {
	if a == nil {
		return
	}
	a.record(deploymentID, status, false, duration, true, responseStats{}, nil)
}

// RecordImmediate records a request that never reached Dispatch: either the
// model never resolved to any deployment at all (deploymentID 0), or a
// deployment resolved but the request was rejected before ever reaching an
// upstream instance (asleep, otherwise unready, a body-rewrite failure, or a
// contract mismatch). It never touches the in-flight gauge or
// duration_sum/count — there is no dispatched leg to time — it only bumps
// requests_total.
func (a *Accountant) RecordImmediate(deploymentID uint, status int, asleep bool) {
	if a == nil {
		return
	}
	a.record(deploymentID, status, asleep, 0, false, responseStats{}, nil)
}

func (a *Accountant) record(deploymentID uint, status int, asleep bool, duration time.Duration, wasDispatched bool, stats responseStats, fallbacks []inferencemetrics.GatewayFallback) {
	a.mu.Lock()
	c := a.deploymentLocked(deploymentID)
	if wasDispatched {
		c.inflight--
		c.durationSumMs += float64(duration) / float64(time.Millisecond)
		c.durationCount++
		c.response.record(duration, stats, fallbacks)
	}
	if class, ok := classifyStatus(status, asleep); ok {
		c.classTotals[class]++
	}
	c.lastRequestAt = a.clock()
	c.dirty = true
	a.mu.Unlock()
}

// InFlight is how many of the deployment's requests are being served now, one
// of the residency arbiter's signs of whether it is idle.
func (a *Accountant) InFlight(deploymentID uint) int {
	if a == nil {
		return 0
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if c, ok := a.counters[deploymentID]; ok && c.inflight > 0 {
		return int(c.inflight)
	}
	return 0
}

// LastRequestAt reports the last time any request touched deploymentID —
// dispatched, finished, or immediately rejected: the residency arbiter's
// idle detection measures a deployment's idle time from it.
func (a *Accountant) LastRequestAt(deploymentID uint) (time.Time, bool) {
	if a == nil {
		return time.Time{}, false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	c, ok := a.counters[deploymentID]
	if !ok || c.lastRequestAt.IsZero() {
		return time.Time{}, false
	}
	return c.lastRequestAt, true
}

// Dropped reports how many flush batches were discarded because the sink
// queue was full. It exists for tests and operational visibility; the same
// value is also emitted as gpuworks_proxy_accounting_dropped_total once
// nonzero (see flush).
func (a *Accountant) Dropped() uint64 {
	if a == nil {
		return 0
	}
	return a.dropped.Load()
}

// Run flushes accounting samples to the sink every 10s until ctx is done,
// performing one final flush before returning: on shutdown it enqueues the
// final batch, signals the writer to stop, and waits up to
// finalFlushWaitTimeout - short, since an ordinary sink call is near-instant
// - for the writer to actually drain the queue (that final batch included)
// before returning, so a caller that waits on Run, like App.Run's shutdown
// before it closes the metrics store, does not race an async writer that has
// been signaled but has not run yet. A sink stuck writing forever (e.g. a
// wedged SQLite handle) still cannot hang Run beyond that short bound: Run
// returns anyway, and the writer keeps draining afterward, exactly as
// awaitPreviousWriter expects of a stale writer on the next Run.
//
// The queue itself is never closed — only Run's own per-call stop signal is
// — so Run may be called again after it returns without ever risking a
// send-on-closed-channel or double-close panic. Run must not be called
// concurrently with itself; callers restart it only after a previous call
// has returned. Since a previous Run's writer may still be draining (or
// stuck) when a new Run starts, Run waits briefly for it via
// awaitPreviousWriter first, so at most one writer is ever intentionally
// live — a writer that gave up waiting and is still stuck is a logged,
// accepted residual risk, not silently ignored.
func (a *Accountant) Run(ctx context.Context) {
	if a == nil {
		return
	}
	a.awaitPreviousWriter()

	stop := make(chan struct{})
	writerDone := make(chan struct{})
	a.runMu.Lock()
	a.writerDone = writerDone
	a.runMu.Unlock()
	go a.writeLoop(stop, writerDone)

	t := time.NewTicker(flushInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			a.flush()
			close(stop)
			select {
			case <-writerDone:
			case <-time.After(finalFlushWaitTimeout):
			}
			return
		case <-t.C:
			a.flush()
		}
	}
}

// awaitPreviousWriter waits up to previousWriterStopTimeout for a previous
// Run's writer goroutine to finish, so this Run's writer never runs
// concurrently with it. If it gives up, it logs and proceeds anyway rather
// than hang this Run (and its caller) forever: the stale writer, once its
// stuck sink call eventually returns, simply keeps draining the shared queue
// until it notices its own stop was closed, alongside the new writer.
func (a *Accountant) awaitPreviousWriter() {
	a.runMu.Lock()
	prev := a.writerDone
	a.runMu.Unlock()
	if prev == nil {
		return
	}
	select {
	case <-prev:
	case <-time.After(previousWriterStopTimeout):
		a.log.Warn("proxy accounting: previous flush writer did not stop before this Run started; a stuck sink may still be running")
	}
}

// writeLoop is the single writer: it is the only goroutine that ever calls
// sink, so a sink that mutates shared state (like obsingest.Pipeline.IngestMetrics
// writing to SQLite) needs no locking of its own on that account. On stop it
// drains whatever is already sitting in the queue before returning, so a
// flush enqueued right before shutdown (typically Run's own final flush)
// still reaches the sink instead of being abandoned mid-channel.
func (a *Accountant) writeLoop(stop <-chan struct{}, done chan<- struct{}) {
	defer close(done)
	for {
		select {
		case batch := <-a.queue:
			if a.sink != nil {
				a.sink(batch)
			}
		case <-stop:
			for {
				select {
				case batch := <-a.queue:
					if a.sink != nil {
						a.sink(batch)
					}
				default:
					return
				}
			}
		}
	}
}

// flush converts every deployment with activity since the last flush (or
// currently carrying in-flight requests) into tsdb samples and enqueues them
// for the writer goroutine. It never blocks: a full queue drops the batch and
// counts it (Dropped), the same bounded-queue-drop shape as the log-center
// ingest loop, so a slow sink cannot back up flush (or, transitively, the
// request path). A drop logs a rate-limited warning and re-marks the
// affected deployments dirty, so the same counts are re-emitted on the next
// flush instead of being lost; once a drop has occurred, the next successful
// batch (and only that one — see lastEmittedDropped) also carries the
// cumulative gpuworks_proxy_accounting_dropped_total sample, so the drop is
// visible in tsdb too, not just the log.
func (a *Accountant) flush() {
	ids, samples := a.snapshotAndClearDirty()
	if a.afterSnapshotAndClearForTest != nil {
		a.afterSnapshotAndClearForTest()
	}
	samples = append(samples, a.traffic.samples(a.clock())...)
	dropped := a.dropped.Load()
	includeDropped := dropped > a.lastEmittedDropped
	if includeDropped {
		samples = append(samples, tsdb.Sample{
			Metric: metricAccountingDroppedTotal,
			Labels: map[string]string{"node": serverNodeLabel},
			Value:  float64(dropped),
			TimeMS: a.clock().UnixMilli(),
		})
	}
	if len(samples) == 0 {
		return
	}
	select {
	case a.queue <- samples:
		if includeDropped {
			a.lastEmittedDropped = dropped
		}
	default:
		a.remarkDirty(ids)
		n := a.dropped.Add(1)
		if n == 1 || n%dropWarnEvery == 0 {
			a.log.Warn("proxy accounting flush dropped under sink backpressure",
				"dropped_total", n, "queue_depth", cap(a.queue))
		}
	}
}

// snapshotAndClearDirty computes the samples for every deployment that is
// dirty (saw activity since the last flush) or currently has requests in
// flight — the latter so a long-lived request's in-flight gauge is
// re-reported on every flush it spans, not just the one flush after
// Dispatch — and clears dirty for exactly those deployments, all within a
// single locked pass.
//
// Reading and clearing dirty together under one lock (rather than as two
// separate locked steps with a gap in between) matters: with a gap, a
// Dispatch/Finish/RecordImmediate landing in that gap would set dirty back
// to true correctly, but a subsequent clear step — unaware a fresh update had
// just landed — would stomp it back to false, silently hiding that update
// from every later flush until unrelated traffic happened to re-dirty the
// same deployment. Doing both under one lock makes that interleaving
// impossible: a concurrent call either completes fully before this pass
// starts (its update is included and correctly cleared) or fully after this
// pass ends (it sets dirty itself, correctly, for the next flush to see).
//
// If the caller (flush) fails to enqueue the resulting batch, it must call
// remarkDirty with the returned ids so the same counts are retried instead of
// lost.
// remarkDirty re-sets the dirty flag for exactly the deployments whose
// samples snapshotAndClearDirty just computed (and cleared) but flush failed
// to enqueue, so their counts are retried on the next flush instead of lost.
func (a *Accountant) remarkDirty(ids []uint) {
	if len(ids) == 0 {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, id := range ids {
		if c, ok := a.counters[id]; ok {
			c.dirty = true
		}
	}
}
