package app

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/tsdb"
)

// deploymentLookupTTL bounds how long a resolved (or confirmed-absent)
// instance -> deployment mapping is trusted before ingest asks the resolver
// again. It only needs to be short enough that a re-deployed/reused instance
// UUID does not carry a stale deployment for long; it does not need to be
// anywhere near real-time, since ingest runs every batch and a store hit per
// sample would dominate cost otherwise.
const deploymentLookupTTL = 30 * time.Second

// deploymentCacheCap bounds deployCache's size: agent-supplied instance UUIDs
// are not a trusted, bounded key space, so an unbounded map would let a
// misbehaving or churning fleet grow it without limit. 4096 comfortably
// covers every instance a single node pool would have in flight at once.
const deploymentCacheCap = 4096

// deploymentLookup caches one instance UUID's resolver result.
type deploymentLookup struct {
	id      uint
	ok      bool
	expires time.Time
}

const (
	// metricsIngestQueueBatches bounds the backlog: each entry is one agent
	// TMetrics message's (or the proxy accountant's) already-converted sample
	// batch. Overflow drops whole batches, counted — the same tradeoff
	// logIngestQueueChunks makes: better a lost sample window than a blocked
	// control stream, since a stalled hub ctrl read loop delays every other
	// message type (heartbeat, status) for that node too.
	metricsIngestQueueBatches = 1024
	// metricsIngestFlushRows / metricsIngestFlushEvery shape the batcher's
	// commits: a fleet-scale flush amortizes one WriteBatch transaction across
	// many nodes' samples, while 200ms keeps ingest-visibility latency low
	// (plan-9-18-monitor-center.md §8 capacity requirements).
	metricsIngestFlushRows  = 2000
	metricsIngestFlushEvery = 200 * time.Millisecond
	// metricsDropWarnInterval rate-limits the overflow warning: a sustained
	// overflow would otherwise log once per dropped batch.
	metricsDropWarnInterval = 5 * time.Second
)

// proxyMetricsSink adapts an Accountant flush batch to observability's tsdb
// ingest path, the same one every agent-reported metric batch already goes
// through. It always runs on the Accountant's own single writer goroutine
// (see proxy.Accountant), never on a request-handling goroutine.
func proxyMetricsSink(obs *observability) func([]tsdb.Sample) {
	return func(samples []tsdb.Sample) {
		batch := make([]proto.MetricSample, 0, len(samples))
		for _, s := range samples {
			batch = append(batch, proto.MetricSample{Metric: s.Metric, Labels: s.Labels, Value: s.Value, TimeMS: s.TimeMS})
		}
		obs.ingestMetrics("server", batch)
	}
}

// ingestMetrics converts an agent (or the proxy accountant's) metric batch to
// the tsdb wire shape and hands it to the ingest queue; it never touches the
// store and can never block the caller. Deployment-label backfill and
// quantile-at-ingest both happen later, in metricsIngestLoop, so this stays
// cheap enough to call from the hub's ctrl-stream read loop.
func (o *observability) ingestMetrics(_ string, samples []proto.MetricSample) {
	if len(samples) == 0 {
		return
	}
	batch := make([]tsdb.Sample, 0, len(samples))
	for _, s := range samples {
		batch = append(batch, tsdb.Sample{Metric: s.Metric, Labels: s.Labels, Value: s.Value, TimeMS: s.TimeMS})
	}
	if o.metricsQueue == nil {
		// Constructions without the batcher (tests) keep the old synchronous
		// behavior rather than silently dropping everything.
		o.writeMetricsBatch(batch)
		return
	}
	select {
	case o.metricsQueue <- batch:
	default:
		o.metricsDropped.Add(int64(len(batch)))
		o.warnMetricsDrop()
	}
}

// warnMetricsDrop rate-limits the overflow warning so a sustained overflow
// logs at most once per metricsDropWarnInterval instead of once per dropped
// batch.
func (o *observability) warnMetricsDrop() {
	nowMS := o.clock().UnixMilli()
	last := o.lastMetricsDropWarnMS.Load()
	if nowMS-last < metricsDropWarnInterval.Milliseconds() {
		return
	}
	if !o.lastMetricsDropWarnMS.CompareAndSwap(last, nowMS) {
		return // another goroutine just logged it
	}
	slog.Warn("metrics ingest queue full; dropping batch",
		"dropped_total", o.metricsDropped.Load(), "queued", len(o.metricsQueue))
}

// writeMetricsBatch is the sole tsdb writer's per-batch work: resolve/backfill
// the "deployment" label, fold histogram buckets into quantile gauges, and
// write the result. Called either synchronously (queue nil, tests) or from
// metricsIngestLoop.
//
// deployCache eviction runs at most once here (per batch), not once per
// resolved instance inside withDeploymentLabel: see ttl_evict.go's doc
// comment for why a per-insert eviction check does not scale.
func (o *observability) writeMetricsBatch(batch []tsdb.Sample) {
	if len(batch) == 0 {
		return
	}
	for i := range batch {
		batch[i].Labels = o.withDeploymentLabel(batch[i].Labels)
	}
	if o.metricsQueue == nil {
		// The synchronous fallback (queue nil, tests) never runs
		// metricsIngestLoop, so it never gets the periodic 5s sweep either;
		// without this, a test driving ingestMetrics directly in a loop
		// would see over-cap eviction (below) but never TTL expiry, which is
		// not the semantics a real ingest tick has. The sweep is one bounded
		// map pass (cheap - see ttl_evict.go), so doing it every call here is
		// fine: this path is never the fleet-scale hot path metricsIngestLoop
		// is.
		now := o.clock()
		o.sweepExpiredDeployCache(now)
		o.sweepExpiredBucketState(now)
	}
	o.evictDeployCacheIfOverCapacity()
	if o.onSamples != nil {
		o.onSamples(batch)
	}
	_ = o.tsdb.WriteBatch(o.applyQuantiles(batch))
}

// clock returns o.now, defaulting to time.Now when unset (test constructions
// that skip it).
func (o *observability) clock() time.Time {
	if o.now != nil {
		return o.now()
	}
	return time.Now()
}

// metricsIngestLoop is the single tsdb writer for agent- and proxy-reported
// metrics. It batches queued sample batches into commits of up to
// metricsIngestFlushRows samples or one flush interval, whichever comes
// first, and flushes what remains when ctx ends.
func (o *observability) metricsIngestLoop(ctx context.Context) {
	if o.metricsQueue == nil {
		return
	}
	observe := time.NewTicker(5 * time.Second)
	defer observe.Stop()
	batch := make([]tsdb.Sample, 0, metricsIngestFlushRows)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		o.writeMetricsBatch(batch)
		batch = batch[:0]
	}
	ticker := time.NewTicker(metricsIngestFlushEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			for {
				select {
				case bs := <-o.metricsQueue:
					o.metricsTaken.Add(int64(len(bs)))
					batch = append(batch, bs...)
				default:
					flush()
					return
				}
			}
		case bs := <-o.metricsQueue:
			o.metricsTaken.Add(int64(len(bs)))
			batch = append(batch, bs...)
			if len(batch) >= metricsIngestFlushRows {
				flush()
			}
		case <-ticker.C:
			flush()
		case <-observe.C:
			slog.Info("metrics ingest pipeline",
				"taken", o.metricsTaken.Load(), "dropped", o.metricsDropped.Load(),
				"queued", len(o.metricsQueue),
				"deploy_cache_evicted", o.deployEvicted.Load(), "bucket_state_evicted", o.bucketEvicted.Load())
			// The one place either TTL map gets a full sweep: cheap in
			// isolation (O(n) once every 5s, against TTLs of 30s and 10m),
			// but only safe to run this rarely, never per-insert - see
			// ttl_evict.go's doc comment.
			now := o.clock()
			o.sweepExpiredDeployCache(now)
			o.sweepExpiredBucketState(now)
		}
	}
}

// withDeploymentLabel returns labels unchanged unless they carry "instance"
// but not "deployment" and the resolver finds a deployment for that instance,
// in which case it returns a copy with "deployment" added. labels is never
// mutated in place: it may be shared with the caller's own bookkeeping.
func (o *observability) withDeploymentLabel(labels map[string]string) map[string]string {
	instance, hasInstance := labels["instance"]
	if !hasInstance || instance == "" {
		return labels
	}
	if v, hasDeployment := labels["deployment"]; hasDeployment && v != "" {
		return labels
	}
	id, ok := o.deploymentFor(instance)
	if !ok || id == 0 {
		return labels
	}
	out := make(map[string]string, len(labels)+1)
	for k, v := range labels {
		out[k] = v
	}
	out["deployment"] = strconv.FormatUint(uint64(id), 10)
	return out
}

// wrapDeploymentLookup adapts a raw instance -> deployment lookup (id, err)
// into the (id, found, cacheable) shape observability.resolveDeployment
// expects: notFound (the store's ErrNotFound sentinel) becomes a cacheable
// negative, since it is a stable fact about the instance, while any other
// error becomes an uncacheable negative, since it says nothing about whether
// the instance actually has a deployment - caching it would freeze the
// instance as "unknown" for deploymentLookupTTL and split its samples across
// two series until the entry expired.
//
// Kept generic (only `error`, no store types) so this package stays free of
// them, matching resolveDeployment's own doc comment; app.go supplies
// store.ErrNotFound as notFound.
func wrapDeploymentLookup(lookup func(instanceUUID string) (uint, error), notFound error) func(string) (uint, bool, bool) {
	return func(instanceUUID string) (uint, bool, bool) {
		id, err := lookup(instanceUUID)
		switch {
		case err == nil:
			return id, true, true
		case errors.Is(err, notFound):
			return 0, false, true
		default:
			return 0, false, false
		}
	}
}

// deploymentFor resolves instanceUUID to a deployment id, through a short-TTL
// cache so a batch of samples from the same instance costs at most one
// resolver call (a store lookup) per TTL window rather than one per sample.
// Both a positive and a confirmed-negative (unknown instance) result are
// cached; a resolver error that is not a confirmed "no such instance" is
// answered but never cached, so it affects only this one call.
//
// It does NOT evict on insert: a batch can call this once per distinct
// instance it carries, and evicting per call was measured as the dominant
// cost of an ingest tick at fleet scale (ttl_evict.go's doc comment). The
// caller (writeMetricsBatch) evicts once per batch instead.
func (o *observability) deploymentFor(instanceUUID string) (uint, bool) {
	if o.resolveDeployment == nil {
		return 0, false
	}
	at := o.clock()

	o.deployMu.Lock()
	if entry, ok := o.deployCache[instanceUUID]; ok && at.Before(entry.expires) {
		o.deployMu.Unlock()
		return entry.id, entry.ok
	}
	o.deployMu.Unlock()

	id, found, cacheable := o.resolveDeployment(instanceUUID)
	if !cacheable {
		return id, found
	}

	o.deployMu.Lock()
	if o.deployCache == nil {
		o.deployCache = map[string]deploymentLookup{}
	}
	o.deployCache[instanceUUID] = deploymentLookup{id: id, ok: found, expires: at.Add(deploymentLookupTTL)}
	o.deployMu.Unlock()
	return id, found
}

// deployCacheLimit returns the configured cap, or the default when unset.
func (o *observability) deployCacheLimit() int {
	if o.deployCacheCap > 0 {
		return o.deployCacheCap
	}
	return deploymentCacheCap
}

// evictDeployCacheIfOverCapacity is the bounded, once-per-batch fallback:
// see ttl_evict.go's evictBoundedLocked doc comment. It does not sweep
// expired entries itself; sweepExpiredDeployCache (run periodically from
// metricsIngestLoop) does that. Takes no clock reading: evictBoundedLocked
// only needs the map and the limit, not the current time.
func (o *observability) evictDeployCacheIfOverCapacity() {
	o.deployMu.Lock()
	defer o.deployMu.Unlock()
	n := evictBoundedLocked(o.deployCache, o.deployCacheLimit(), func(v deploymentLookup) time.Time { return v.expires })
	if n > 0 {
		o.deployEvicted.Add(int64(n))
	}
}

// sweepExpiredDeployCache removes every deployCache entry whose TTL has
// already elapsed. Only called periodically (metricsIngestLoop's observe
// tick) — see ttl_evict.go's sweepExpiredLocked doc comment.
func (o *observability) sweepExpiredDeployCache(now time.Time) {
	o.deployMu.Lock()
	defer o.deployMu.Unlock()
	sweepExpiredLocked(o.deployCache, now, func(v deploymentLookup) time.Time { return v.expires })
}
