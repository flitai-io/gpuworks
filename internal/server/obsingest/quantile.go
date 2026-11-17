package obsingest

import (
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/server/tsdb"
)

// Quantile-at-ingest (phase2-metrics-contract.md "Quantile-at-ingest"): the
// agent ships each mapped histogram family Y as three kinds of series -
// `Y_bucket{le=...}` (a cumulative counter per bucket upper bound), `Y_sum`,
// and `Y_count`. Storing every `Y_bucket` series directly would multiply the
// series count by the bucket count (~20x) for no benefit the dashboards use;
// instead applyQuantiles folds each batch's bucket vector, per (instance,
// family), against the last vector seen for that pair and emits three gauge
// samples - Y{quantile="0.5"|"0.95"|"0.99"} - computed by Prometheus'
// histogram_quantile interpolation over the delta between the two vectors.
// `Y_bucket` samples themselves are never written to tsdb; `Y_sum`/`Y_count`
// pass through unchanged.

// bucketStateTTL / bucketStateCapDefault bound the per-(instance,family) last-
// vector map the same way deploymentLookupTTL/deploymentCacheCap bound the
// deployment cache: an instance that stops reporting (redeployed, torn down)
// must not pin its entry forever, and a churning fleet must not grow the map
// without limit. The cap covers the product's documented tsdb capacity target
// (100 nodes / 200 instances, docs/reference "SQLite tsdb capacity") times the
// 8 mapped histogram families, with more than 10x headroom for churn within
// one TTL window (a fleet well past that target - e.g. 1000 instances x 8
// families = 8000 keys - still fits comfortably under cap, so eviction stays
// off the hot path instead of forcing a bounded-evict pass on every tick).
const (
	bucketStateTTL        = 10 * time.Minute
	bucketStateCapDefault = 16384
)

// quantiles is the fixed set of quantiles emitted per histogram family, per
// the contract.
var quantiles = [...]float64{0.5, 0.95, 0.99}

// bucketVector is one (instance, family)'s last-seen cumulative bucket
// vector: les (ascending upper bounds, the last one +Inf) and the matching
// cumulative counts.
type bucketVector struct {
	les     []float64
	counts  []float64
	expires time.Time
}

// bucketSample is one `_bucket` sample, decoded enough to fold.
type bucketSample struct {
	le     float64
	value  float64
	tsMS   int64
	labels map[string]string
}

// bucketGroupKey identifies one histogram to track: the contract keys the
// bucket-vector state by (instance, family). The gateway's histograms belong
// to no instance - it measures a deployment's requests whichever instance
// served them - so a bucket without an instance is keyed by its deployment.
type bucketGroupKey struct{ instance, family string }

// bucketOwner is the identity a bucket sample's histogram is tracked under:
// its instance, or, for a gateway histogram, its deployment.
func bucketOwner(labels map[string]string) (string, bool) {
	if instance, ok := labels["instance"]; ok {
		return instance, true
	}
	if deployment, ok := labels["deployment"]; ok && deployment != "" {
		return "deployment:" + deployment, true
	}
	return "", false
}

// applyQuantiles is RunMetrics's (or the synchronous fallback's) last
// step before WriteBatch: it splits samples into "pass through unchanged"
// (everything that is not a labeled histogram bucket) and "fold into
// quantile gauges" (every `_bucket` sample carrying an `le` label and an
// owner - an `instance`, or a `deployment` for the gateway's histograms). A
// `_bucket` sample missing either cannot be tracked and is dropped rather
// than written raw, since raw bucket series must never reach tsdb.
func (o *Pipeline) applyQuantiles(samples []tsdb.Sample) []tsdb.Sample {
	out := make([]tsdb.Sample, 0, len(samples))
	var groups map[bucketGroupKey][]bucketSample
	for _, s := range samples {
		family, isBucket := bucketFamily(s.Metric)
		if !isBucket {
			out = append(out, s)
			continue
		}
		leStr, hasLe := s.Labels["le"]
		owner, hasOwner := bucketOwner(s.Labels)
		if !hasLe || !hasOwner {
			continue
		}
		le, ok := parseLE(leStr)
		if !ok {
			continue
		}
		if groups == nil {
			groups = map[bucketGroupKey][]bucketSample{}
		}
		key := bucketGroupKey{instance: owner, family: family}
		groups[key] = append(groups[key], bucketSample{le: le, value: s.Value, tsMS: s.TimeMS, labels: s.Labels})
	}
	if len(groups) == 0 {
		return out
	}
	at := o.clock()
	for key, bs := range groups {
		out = append(out, o.foldBucketGroup(key.instance, key.family, bs, at)...)
	}
	// Bounded eviction runs at most once per batch, however many groups it
	// contained - not once per group (ttl_evict.go's doc comment: that was
	// the dominant cost of an ingest tick at fleet scale).
	o.evictBucketStateIfOverCapacity()
	return out
}

// bucketFamily reports the histogram family name for a "..._bucket" metric
// (e.g. "gpuworks_infer_ttft_seconds_bucket" -> "gpuworks_infer_ttft_seconds",
// true); every other metric name (including "_sum"/"_count", which pass
// through applyQuantiles unchanged) reports ok=false.
func bucketFamily(metric string) (family string, ok bool) {
	const suffix = "_bucket"
	if !strings.HasSuffix(metric, suffix) {
		return "", false
	}
	return strings.TrimSuffix(metric, suffix), true
}

// parseLE parses one `le` label value: "+Inf" (and the case variants
// Prometheus exposition allows) becomes +Inf, everything else is a plain
// float.
func parseLE(s string) (float64, bool) {
	switch s {
	case "+Inf", "Inf", "inf", "+inf":
		return math.Inf(1), true
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

// coalesceBucketSamples sorts bs by le ascending and merges duplicate le
// entries, keeping the sample with the largest tsMS for each — Prometheus'
// own coalesceBuckets rule. Duplicates happen whenever two ticks of the same
// (instance, family) end up in one flushed batch (RunMetrics batches
// by row count/time, not by tick boundary, so a slow consumer or a burst can
// merge two queued IngestMetrics calls into one applyQuantiles call): without
// coalescing, the raw per-le sample list would be twice the expected length,
// misaligning every index against the stored previous vector and corrupting
// (or entirely dropping, via the length-mismatch reseed path) both ticks'
// quantiles.
func coalesceBucketSamples(bs []bucketSample) []bucketSample {
	sort.Slice(bs, func(i, j int) bool {
		if bs[i].le != bs[j].le {
			return bs[i].le < bs[j].le
		}
		return bs[i].tsMS < bs[j].tsMS
	})
	out := make([]bucketSample, 0, len(bs))
	for _, b := range bs {
		if n := len(out); n > 0 && out[n-1].le == b.le {
			out[n-1] = b // sorted ascending by tsMS within equal le: b is newer
			continue
		}
		out = append(out, b)
	}
	return out
}

// ensureMonotonic forces a cumulative bucket vector to be non-decreasing,
// exactly as Prometheus does before running histogram_quantile: a real
// engine's histogram is not read atomically across its buckets (each
// bucket is its own counter, incremented independently of the others), so
// two buckets scraped microseconds apart can show a later (larger le)
// bucket with a *smaller* delta than an earlier one purely from that race -
// never because the underlying histogram is actually non-monotonic.
// sort.Search assumes a monotonic predicate; without this, a single such
// dip anywhere in the vector can make it return an arbitrary index instead
// of erroring, which surfaced as identical (wrong) values for every
// quantile in one measured case. Scanning ascending and clamping any
// decrease up to the running max is the correction Prometheus itself
// applies (promql's ensureMonotonic).
func ensureMonotonic(counts []float64) {
	max := math.Inf(-1)
	for i := range counts {
		switch {
		case counts[i] > max:
			max = counts[i]
		case counts[i] < max:
			counts[i] = max
		}
	}
}

// foldBucketGroup folds one batch's bucket vector for (instance, family)
// against the stored previous vector and returns the quantile gauge samples
// to write (nil when there is nothing to emit this tick). The current vector
// always replaces the stored one, regardless of outcome, so the next batch
// deltas against what this tick actually observed. at is the batch's clock
// reading (applyQuantiles reads it once for the whole batch).
func (o *Pipeline) foldBucketGroup(instance, family string, bs []bucketSample, at time.Time) []tsdb.Sample {
	if len(bs) == 0 {
		return nil
	}
	bs = coalesceBucketSamples(bs)
	les := make([]float64, len(bs))
	counts := make([]float64, len(bs))
	var tsMS int64
	var rawLabels map[string]string
	for i, b := range bs {
		les[i] = b.le
		counts[i] = b.value
		if b.tsMS > tsMS {
			tsMS = b.tsMS
		}
		rawLabels = b.labels // any one sample's non-le labels represent the group
	}
	if !math.IsInf(les[len(les)-1], 1) {
		// A histogram's cumulative total only lives in the +Inf bucket; a
		// family missing it this tick (a truncated scrape, a partial batch)
		// has no valid total to diff or interpolate against, so it is
		// rejected for this tick entirely rather than folded against
		// whatever the highest present bucket happens to be.
		return nil
	}
	outLabels := make(map[string]string, len(rawLabels))
	for k, v := range rawLabels {
		if k != "le" {
			outLabels[k] = v
		}
	}

	key := instance + "\x1f" + family
	o.bucketMu.Lock()
	prev, hadPrev := o.bucketState[key]
	fresh := !hadPrev || !at.Before(prev.expires)
	var deltas []float64
	if !fresh && len(prev.les) == len(les) {
		deltas = make([]float64, len(les))
		for i := range les {
			if prev.les[i] != les[i] {
				fresh = true
				break
			}
			d := counts[i] - prev.counts[i]
			if d < 0 {
				// Counter reset (the engine restarted or its process metrics
				// were recreated): treat this batch as the first observation.
				fresh = true
				break
			}
			deltas[i] = d
		}
	} else if !fresh {
		// The bucket boundary set itself changed shape: cannot diff two
		// different histograms meaningfully, so reseed instead of guessing.
		fresh = true
	}
	if o.bucketState == nil {
		o.bucketState = map[string]*bucketVector{}
	}
	o.bucketState[key] = &bucketVector{les: les, counts: counts, expires: at.Add(bucketStateTTL)}
	o.bucketMu.Unlock()

	if fresh {
		return nil
	}
	ensureMonotonic(deltas)
	total := deltas[len(deltas)-1]
	if total <= 0 {
		return nil
	}
	var out []tsdb.Sample
	for _, q := range quantiles {
		v := histogramQuantile(q, les, deltas)
		if math.IsNaN(v) {
			continue
		}
		labels := make(map[string]string, len(outLabels)+1)
		for k, vv := range outLabels {
			labels[k] = vv
		}
		labels["quantile"] = formatQuantile(q)
		out = append(out, tsdb.Sample{Metric: family, Labels: labels, Value: v, TimeMS: tsMS})
	}
	return out
}

// bucketStateLimit returns the configured cap, or the default when unset.
func (o *Pipeline) bucketStateLimit() int {
	if o.bucketStateCap > 0 {
		return o.bucketStateCap
	}
	return bucketStateCapDefault
}

// evictBucketStateIfOverCapacity is the bounded, once-per-batch fallback:
// see ttl_evict.go's evictBoundedLocked doc comment. It does not sweep
// expired entries itself; sweepExpiredBucketState (run periodically from
// RunMetrics) does that. Takes no clock reading: evictBoundedLocked
// only needs the map and the limit, not the current time.
func (o *Pipeline) evictBucketStateIfOverCapacity() {
	o.bucketMu.Lock()
	defer o.bucketMu.Unlock()
	n := evictBoundedLocked(o.bucketState, o.bucketStateLimit(), func(v *bucketVector) time.Time { return v.expires })
	if n > 0 {
		o.bucketEvicted.Add(int64(n))
	}
}

// sweepExpiredBucketState removes every bucketState entry whose TTL has
// already elapsed. Only called periodically (RunMetrics's observe
// tick) — see ttl_evict.go's sweepExpiredLocked doc comment.
func (o *Pipeline) sweepExpiredBucketState(now time.Time) {
	o.bucketMu.Lock()
	defer o.bucketMu.Unlock()
	sweepExpiredLocked(o.bucketState, now, func(v *bucketVector) time.Time { return v.expires })
}

// formatQuantile renders a quantile as the contract's label value ("0.5",
// "0.95", "0.99" - the shortest exact decimal, not "0.50").
func formatQuantile(q float64) string {
	return strconv.FormatFloat(q, 'g', -1, 64)
}

// histogramQuantile computes histogram_quantile(q, ...) for one cumulative
// histogram given as parallel (les, counts) slices sorted by le ascending,
// the last entry being the +Inf bucket. This is Prometheus' own
// interpolation (promql's bucketQuantile): linear interpolation within the
// bucket the rank falls into, and - since the +Inf bucket carries no upper
// bound to interpolate against - a rank that falls into it resolves to the
// last finite bucket's upper bound instead.
func histogramQuantile(q float64, les, counts []float64) float64 {
	n := len(les)
	if n < 2 {
		// No finite bucket to interpolate within.
		return math.NaN()
	}
	total := counts[n-1]
	if total <= 0 {
		return math.NaN()
	}
	rank := q * total
	idx := sort.Search(n-1, func(i int) bool { return counts[i] >= rank })
	if idx == n-1 {
		return les[n-2]
	}
	bucketEnd := les[idx]
	count := counts[idx]
	var bucketStart float64
	if idx > 0 {
		bucketStart = les[idx-1]
		count -= counts[idx-1]
		rank -= counts[idx-1]
	}
	if count <= 0 {
		return bucketEnd
	}
	return bucketStart + (bucketEnd-bucketStart)*(rank/count)
}
