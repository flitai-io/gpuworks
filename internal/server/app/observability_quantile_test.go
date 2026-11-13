package app

import (
	"fmt"
	"math"
	"sort"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/tsdb"
)

// bucketBatch builds one tick's `_bucket` samples for one (instance, family)
// plus the family's `_sum`/`_count` counters, mirroring what the agent
// collector ships per phase2-metrics-contract.md.
func bucketBatch(family, instance string, les []string, counts []float64, tsMS int64) []tsdb.Sample {
	out := make([]tsdb.Sample, 0, len(les)+2)
	for i, le := range les {
		out = append(out, tsdb.Sample{
			Metric: family + "_bucket",
			Labels: map[string]string{"instance": instance, "node": "n1", "le": le},
			Value:  counts[i], TimeMS: tsMS,
		})
	}
	out = append(out, tsdb.Sample{Metric: family + "_sum", Labels: map[string]string{"instance": instance, "node": "n1"}, Value: counts[len(counts)-1] * 0.7, TimeMS: tsMS})
	out = append(out, tsdb.Sample{Metric: family + "_count", Labels: map[string]string{"instance": instance, "node": "n1"}, Value: counts[len(counts)-1], TimeMS: tsMS})
	return out
}

func findQuantile(t *testing.T, out []tsdb.Sample, family, q string) (tsdb.Sample, bool) {
	t.Helper()
	for _, s := range out {
		if s.Metric == family && s.Labels["quantile"] == q {
			return s, true
		}
	}
	return tsdb.Sample{}, false
}

const ttftFamily = "gpuworks_infer_ttft_seconds"

// TestApplyQuantilesFirstBatchEmitsNothing pins the seed case: the very first
// batch for an (instance, family) has no previous vector to diff against, so
// no quantile gauges are written, the _bucket samples are still dropped
// (never written raw), and _sum/_count pass through unchanged.
func TestApplyQuantilesFirstBatchEmitsNothing(t *testing.T) {
	o := &observability{now: time.Now, bucketState: map[string]*bucketVector{}}
	les := []string{"0.1", "0.5", "1", "2", "5", "+Inf"}
	batch := bucketBatch(ttftFamily, "inst-1", les, []float64{2, 5, 8, 9, 10, 10}, 1000)

	out := o.applyQuantiles(batch)

	for _, s := range out {
		if s.Metric == ttftFamily+"_bucket" {
			t.Fatalf("a _bucket sample reached the output: %+v", s)
		}
	}
	if _, ok := findQuantile(t, out, ttftFamily, "0.5"); ok {
		t.Fatalf("first batch must not emit a quantile gauge, got: %+v", out)
	}
	var sawSum, sawCount bool
	for _, s := range out {
		switch s.Metric {
		case ttftFamily + "_sum":
			sawSum = true
		case ttftFamily + "_count":
			sawCount = true
			if s.Value != 10 {
				t.Fatalf("_count value = %v, want 10", s.Value)
			}
		}
	}
	if !sawSum || !sawCount {
		t.Fatalf("_sum/_count did not pass through: %+v", out)
	}
	if len(o.bucketState) != 1 {
		t.Fatalf("bucket state = %+v, want exactly the seeded entry", o.bucketState)
	}
}

// TestApplyQuantilesComputesExactQuantilesFromHandComputedDelta hand-computes
// histogram_quantile over the delta between two ticks and checks the server's
// output matches to the last digit (Prometheus' own bucketQuantile
// interpolation, see histogramQuantile's doc comment).
//
// les = [0.1, 0.5, 1, 2, 5, +Inf]; cumulative counts tick1=[2,5,8,9,10,10],
// tick2=[3,9,15,18,20,20]; delta=[1,4,7,9,10,10] (total 10 new observations).
// p50: rank=5 -> bucket (0.5,1], count 7-4=3, offset 5-4=1 -> 0.5+0.5*(1/3).
// p95: rank=9.5 -> bucket (2,5], count 10-9=1, offset 9.5-9=0.5 -> 2+3*0.5.
// p99: rank=9.9 -> same bucket, offset 9.9-9=0.9 -> 2+3*0.9.
func TestApplyQuantilesComputesExactQuantilesFromHandComputedDelta(t *testing.T) {
	o := &observability{now: time.Now, bucketState: map[string]*bucketVector{}}
	les := []string{"0.1", "0.5", "1", "2", "5", "+Inf"}
	o.applyQuantiles(bucketBatch(ttftFamily, "inst-1", les, []float64{2, 5, 8, 9, 10, 10}, 1000))

	out := o.applyQuantiles(bucketBatch(ttftFamily, "inst-1", les, []float64{3, 9, 15, 18, 20, 20}, 2000))

	cases := map[string]float64{
		"0.5":  0.5 + 0.5*(1.0/3.0),
		"0.95": 2 + 3*0.5,
		"0.99": 2 + 3*0.9,
	}
	for q, want := range cases {
		s, ok := findQuantile(t, out, ttftFamily, q)
		if !ok {
			t.Fatalf("quantile=%s missing from %+v", q, out)
		}
		if math.Abs(s.Value-want) > 1e-9 {
			t.Errorf("quantile=%s = %v, want %v", q, s.Value, want)
		}
		if s.TimeMS != 2000 {
			t.Errorf("quantile=%s TimeMS = %d, want 2000", q, s.TimeMS)
		}
		if s.Labels["instance"] != "inst-1" || s.Labels["node"] != "n1" {
			t.Errorf("quantile=%s labels = %+v, want instance/node preserved", q, s.Labels)
		}
		if _, hasLe := s.Labels["le"]; hasLe {
			t.Errorf("quantile=%s labels still carry le: %+v", q, s.Labels)
		}
	}
	for _, s := range out {
		if s.Metric == ttftFamily+"_bucket" {
			t.Fatalf("a _bucket sample reached the output: %+v", s)
		}
	}
}

// TestApplyQuantilesInfBucketRankFallsBackToLastFiniteBound covers the
// contract's documented +Inf handling: when the requested rank falls inside
// the +Inf bucket itself (no observations above the last finite bound in this
// window), histogram_quantile has no upper bound to interpolate against and
// must return the last finite bucket's own upper bound instead.
func TestApplyQuantilesInfBucketRankFallsBackToLastFiniteBound(t *testing.T) {
	o := &observability{now: time.Now, bucketState: map[string]*bucketVector{}}
	les := []string{"1", "+Inf"}
	o.applyQuantiles(bucketBatch(ttftFamily, "inst-1", les, []float64{0, 0}, 1000))

	out := o.applyQuantiles(bucketBatch(ttftFamily, "inst-1", les, []float64{8, 10}, 2000))
	// delta = [8, 10]; total 10. p99 rank=9.9 falls in the +Inf bucket
	// (finite-bucket count 8 < 9.9), so the answer is the last finite bound: 1.
	s, ok := findQuantile(t, out, ttftFamily, "0.99")
	if !ok {
		t.Fatalf("quantile=0.99 missing from %+v", out)
	}
	if s.Value != 1 {
		t.Fatalf("quantile=0.99 = %v, want 1 (last finite bound)", s.Value)
	}
}

// TestApplyQuantilesCounterResetReseedsWithoutEmitting covers a bucket vector
// that goes backwards (the engine process restarted, resetting its
// Prometheus counters): the batch must be treated as a fresh first
// observation - no quantile emitted - and the *next* batch must diff against
// this reset baseline, not the pre-reset one.
func TestApplyQuantilesCounterResetReseedsWithoutEmitting(t *testing.T) {
	o := &observability{now: time.Now, bucketState: map[string]*bucketVector{}}
	les := []string{"1", "+Inf"}
	o.applyQuantiles(bucketBatch(ttftFamily, "inst-1", les, []float64{50, 100}, 1000))

	// Reset: counts drop below the previous tick's.
	resetOut := o.applyQuantiles(bucketBatch(ttftFamily, "inst-1", les, []float64{1, 2}, 2000))
	if _, ok := findQuantile(t, resetOut, ttftFamily, "0.5"); ok {
		t.Fatalf("a counter reset must not emit a quantile, got: %+v", resetOut)
	}

	// The next batch must diff against the reset baseline (1,2), not (50,100).
	out := o.applyQuantiles(bucketBatch(ttftFamily, "inst-1", les, []float64{3, 5}, 3000))
	s, ok := findQuantile(t, out, ttftFamily, "0.5")
	if !ok {
		t.Fatalf("post-reset batch should emit a quantile, got: %+v", out)
	}
	// delta = [2,3] (from the reset baseline 1,2), total 3, p50 rank=1.5:
	// bucket (0,1], count=2, offset=1.5 -> 0 + 1*(1.5/2) = 0.75.
	if math.Abs(s.Value-0.75) > 1e-9 {
		t.Fatalf("post-reset quantile=0.5 = %v, want 0.75 (diffed against the reset baseline)", s.Value)
	}
}

// TestApplyQuantilesZeroDeltaEmitsNothingButAdvancesBaseline covers "delta
// count == 0" (no new observations this tick, e.g. an idle instance still
// scraped): nothing is emitted, but the stored vector still advances so a
// later batch diffs against this tick rather than the older one.
func TestApplyQuantilesZeroDeltaEmitsNothingButAdvancesBaseline(t *testing.T) {
	o := &observability{now: time.Now, bucketState: map[string]*bucketVector{}}
	les := []string{"1", "+Inf"}
	o.applyQuantiles(bucketBatch(ttftFamily, "inst-1", les, []float64{5, 10}, 1000))

	unchanged := o.applyQuantiles(bucketBatch(ttftFamily, "inst-1", les, []float64{5, 10}, 2000))
	if _, ok := findQuantile(t, unchanged, ttftFamily, "0.5"); ok {
		t.Fatalf("zero delta must not emit a quantile, got: %+v", unchanged)
	}

	out := o.applyQuantiles(bucketBatch(ttftFamily, "inst-1", les, []float64{6, 11}, 3000))
	if _, ok := findQuantile(t, out, ttftFamily, "0.5"); !ok {
		t.Fatalf("the next batch with new observations should emit a quantile, got: %+v", out)
	}
}

// TestApplyQuantilesTTLEvictionTreatsStaleEntryAsFirstObservation checks the
// lazy TTL check: once bucketStateTTL has elapsed since a tick, the next
// batch is treated as a fresh first observation (no quantile emitted) instead
// of diffing against the now-stale vector.
func TestApplyQuantilesTTLEvictionTreatsStaleEntryAsFirstObservation(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	o := &observability{now: func() time.Time { return now }, bucketState: map[string]*bucketVector{}}
	les := []string{"1", "+Inf"}
	o.applyQuantiles(bucketBatch(ttftFamily, "inst-1", les, []float64{5, 10}, 1000))

	now = now.Add(bucketStateTTL + time.Second)
	out := o.applyQuantiles(bucketBatch(ttftFamily, "inst-1", les, []float64{20, 40}, 2000))
	if _, ok := findQuantile(t, out, ttftFamily, "0.5"); ok {
		t.Fatalf("a batch after TTL expiry must reseed rather than diff, got: %+v", out)
	}

	// A following batch (within TTL of the reseed) diffs normally.
	now = now.Add(time.Second)
	out = o.applyQuantiles(bucketBatch(ttftFamily, "inst-1", les, []float64{22, 42}, 3000))
	if _, ok := findQuantile(t, out, ttftFamily, "0.5"); !ok {
		t.Fatalf("post-reseed batch should emit a quantile, got: %+v", out)
	}
}

// TestApplyQuantilesCapEvictionDropsEntriesNearestExpiry mirrors
// TestDeployCacheDropsOldestWhenStillOverCapacityAfterSweepingExpired for the
// bucket-state map: once the map exceeds its cap and nothing has expired yet,
// the entries nearest to expiry (earliest inserted, since every entry shares
// the same TTL) are dropped to fit.
func TestApplyQuantilesCapEvictionDropsEntriesNearestExpiry(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	o := &observability{
		now: func() time.Time { return now }, bucketState: map[string]*bucketVector{},
		bucketStateCap: 3,
	}
	les := []string{"1", "+Inf"}
	for i := 0; i < 5; i++ {
		instance := "inst-" + string(rune('0'+i))
		o.applyQuantiles(bucketBatch(ttftFamily, instance, les, []float64{1, 2}, 1000))
		now = now.Add(time.Millisecond)
	}
	if len(o.bucketState) != 3 {
		t.Fatalf("bucket state size = %d, want capped at 3: %+v", len(o.bucketState), keysOf(o.bucketState))
	}
	for _, instance := range []string{"inst-0", "inst-1"} {
		if _, ok := o.bucketState[instance+"\x1f"+ttftFamily]; ok {
			t.Fatalf("oldest entry %s survived eviction", instance)
		}
	}
	for _, instance := range []string{"inst-2", "inst-3", "inst-4"} {
		if _, ok := o.bucketState[instance+"\x1f"+ttftFamily]; !ok {
			t.Fatalf("newest entry %s was evicted", instance)
		}
	}
	// Over-cap eviction (real data loss under sustained load, unlike a
	// routine TTL sweep) must be visible on the running counter reported on
	// the "metrics ingest pipeline" log line, not only inferrable from
	// silence: 2 of the 5 inserts above were evicted.
	if got := o.bucketEvicted.Load(); got != 2 {
		t.Fatalf("bucketEvicted = %d, want 2", got)
	}
}

func keysOf(m map[string]*bucketVector) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// TestApplyQuantilesFamilyWithOnlySumCountIsUntouched covers a histogram
// family that never ships _bucket samples (an old agent, or a family the
// contract only defines _sum/_count for): those samples pass straight
// through and no bucket state or quantile gauge is created.
func TestApplyQuantilesFamilyWithOnlySumCountIsUntouched(t *testing.T) {
	o := &observability{now: time.Now, bucketState: map[string]*bucketVector{}}
	batch := []tsdb.Sample{
		{Metric: ttftFamily + "_sum", Labels: map[string]string{"instance": "inst-1"}, Value: 1.5, TimeMS: 1000},
		{Metric: ttftFamily + "_count", Labels: map[string]string{"instance": "inst-1"}, Value: 3, TimeMS: 1000},
	}
	out := o.applyQuantiles(batch)
	if len(out) != 2 {
		t.Fatalf("got %d samples, want the 2 inputs passed through unchanged: %+v", len(out), out)
	}
	if len(o.bucketState) != 0 {
		t.Fatalf("bucket state should stay empty, got %+v", o.bucketState)
	}
}

// TestApplyQuantilesDropsBucketSampleMissingInstanceOrLe covers the two
// labels a bucket sample must carry to be tracked at all: without "le" or
// without "instance" it cannot be folded, and per the contract raw _bucket
// series must never reach tsdb regardless, so it is simply dropped.
func TestApplyQuantilesDropsBucketSampleMissingInstanceOrLe(t *testing.T) {
	o := &observability{now: time.Now, bucketState: map[string]*bucketVector{}}
	batch := []tsdb.Sample{
		{Metric: ttftFamily + "_bucket", Labels: map[string]string{"instance": "inst-1"}, Value: 1, TimeMS: 1000},    // no le
		{Metric: ttftFamily + "_bucket", Labels: map[string]string{"le": "1", "node": "n1"}, Value: 1, TimeMS: 1000}, // no instance, no deployment
		{Metric: ttftFamily + "_bucket", Labels: map[string]string{"le": "1", "deployment": ""}, Value: 1, TimeMS: 1000},
	}
	out := o.applyQuantiles(batch)
	if len(out) != 0 {
		t.Fatalf("untrackable _bucket samples must be dropped, not passed through: %+v", out)
	}
}

// The gateway's histograms carry a deployment and no instance: they fold into
// quantile gauges per deployment like an engine's per instance, and two
// deployments' histograms never mix.
func TestApplyQuantilesFoldsGatewayHistogramsPerDeployment(t *testing.T) {
	o := &observability{now: time.Now, bucketState: map[string]*bucketVector{}}
	const family = "gpuworks_proxy_ttft_ms"
	les := []string{"250", "500", "1000", "+Inf"}
	batch := func(deployment string, counts []float64, ts int64) []tsdb.Sample {
		var out []tsdb.Sample
		for i, le := range les {
			out = append(out, tsdb.Sample{Metric: family + "_bucket", Labels: map[string]string{"deployment": deployment, "node": "server", "le": le}, Value: counts[i], TimeMS: ts})
		}
		return out
	}
	o.applyQuantiles(append(batch("1", []float64{0, 0, 0, 0}, 1000), batch("2", []float64{0, 0, 0, 0}, 1000)...))
	out := o.applyQuantiles(append(batch("1", []float64{10, 10, 10, 10}, 2000), batch("2", []float64{0, 0, 10, 10}, 2000)...))
	got := map[string]float64{}
	for _, sample := range out {
		if sample.Metric != family || sample.Labels["node"] != "server" || sample.Labels["le"] != "" {
			t.Fatalf("unexpected sample %+v", sample)
		}
		if sample.Labels["quantile"] == "0.5" {
			got[sample.Labels["deployment"]] = sample.Value
		}
	}
	// Deployment 1 answered within 250ms, deployment 2 between 500 and 1000ms.
	if got["1"] > 250 || got["2"] <= 500 || got["2"] > 1000 {
		t.Fatalf("p50 per deployment = %v", got)
	}
}

// TestApplyQuantilesCoalescesDuplicateLeFromTwoMergedTicks is the regression
// test for the merged-batch bug: metricsIngestLoop batches by row
// count/time, not by tick boundary, so a slow consumer or a burst can merge
// two queued ingestMetrics calls (two ticks of the same instance+family)
// into one applyQuantiles call. Without coalescing, the raw per-le sample
// list would be twice the expected length, misaligning every index against
// the stored previous vector. The fix (coalesceBucketSamples) keeps, per le,
// only the sample with the largest tsMS — Prometheus' own coalesceBuckets
// rule — so a stale duplicate must not affect the result at all.
func TestApplyQuantilesCoalescesDuplicateLeFromTwoMergedTicks(t *testing.T) {
	o := &observability{now: time.Now, bucketState: map[string]*bucketVector{}}
	les := []string{"0.1", "0.5", "1", "2", "5", "+Inf"}
	o.applyQuantiles(bucketBatch(ttftFamily, "inst-1", les, []float64{2, 5, 8, 9, 10, 10}, 1000))

	// A stale duplicate tick queued alongside the real one; if it were not
	// coalesced away, its interleaved le entries would corrupt this group's
	// (les, counts) alignment against the stored baseline.
	stale := bucketBatch(ttftFamily, "inst-1", les, []float64{2, 6, 9, 10, 11, 11}, 1500)
	real := bucketBatch(ttftFamily, "inst-1", les, []float64{3, 9, 15, 18, 20, 20}, 2000)
	merged := append(append([]tsdb.Sample{}, stale...), real...)

	out := o.applyQuantiles(merged)

	cases := map[string]float64{
		"0.5":  0.5 + 0.5*(1.0/3.0),
		"0.95": 2 + 3*0.5,
		"0.99": 2 + 3*0.9,
	}
	for q, want := range cases {
		s, ok := findQuantile(t, out, ttftFamily, q)
		if !ok {
			t.Fatalf("quantile=%s missing from coalesced output: %+v", q, out)
		}
		if math.Abs(s.Value-want) > 1e-9 {
			t.Errorf("quantile=%s = %v, want %v (the stale duplicate must not have corrupted the result)", q, s.Value, want)
		}
		if s.TimeMS != 2000 {
			t.Errorf("quantile=%s TimeMS = %d, want 2000 (the later tick wins the coalesce)", q, s.TimeMS)
		}
	}
}

// TestApplyQuantilesBucketArrivalOrderIsIrrelevant checks that
// coalesceBucketSamples' sort makes the result independent of the order
// _bucket samples happen to arrive in within a batch (a real scrape does
// not guarantee any particular label/order across samples).
func TestApplyQuantilesBucketArrivalOrderIsIrrelevant(t *testing.T) {
	les := []string{"0.1", "0.5", "1", "2", "5", "+Inf"}
	first := bucketBatch(ttftFamily, "inst-1", les, []float64{2, 5, 8, 9, 10, 10}, 1000)
	second := bucketBatch(ttftFamily, "inst-1", les, []float64{3, 9, 15, 18, 20, 20}, 2000)

	inOrder := &observability{now: time.Now, bucketState: map[string]*bucketVector{}}
	inOrder.applyQuantiles(first)
	wantOut := inOrder.applyQuantiles(second)

	shuffled := &observability{now: time.Now, bucketState: map[string]*bucketVector{}}
	shuffled.applyQuantiles(reversed(first))
	gotOut := shuffled.applyQuantiles(reversed(second))

	for _, q := range []string{"0.5", "0.95", "0.99"} {
		want, ok := findQuantile(t, wantOut, ttftFamily, q)
		if !ok {
			t.Fatalf("in-order run missing quantile=%s", q)
		}
		got, ok := findQuantile(t, gotOut, ttftFamily, q)
		if !ok {
			t.Fatalf("reversed-order run missing quantile=%s", q)
		}
		if math.Abs(got.Value-want.Value) > 1e-9 {
			t.Errorf("quantile=%s = %v with reversed arrival order, want %v (same as in-order)", q, got.Value, want.Value)
		}
	}
}

func reversed(s []tsdb.Sample) []tsdb.Sample {
	out := make([]tsdb.Sample, len(s))
	for i, v := range s {
		out[len(s)-1-i] = v
	}
	return out
}

// TestApplyQuantilesRejectsAFamilyMissingTheInfBucket covers item 7: a
// histogram's cumulative total only lives in the +Inf bucket. A tick that is
// missing it (a truncated scrape, a partial batch) has no valid total to
// diff or interpolate against and must be rejected outright for that tick -
// not folded against whatever the highest present finite bucket happens to
// be, which would understate the true total and skew every quantile.
func TestApplyQuantilesRejectsAFamilyMissingTheInfBucket(t *testing.T) {
	o := &observability{now: time.Now, bucketState: map[string]*bucketVector{}}
	les := []string{"0.1", "0.5", "1"} // no +Inf
	o.applyQuantiles(bucketBatch(ttftFamily, "inst-1", les, []float64{2, 5, 8}, 1000))

	out := o.applyQuantiles(bucketBatch(ttftFamily, "inst-1", les, []float64{3, 9, 15}, 2000))
	if _, ok := findQuantile(t, out, ttftFamily, "0.5"); ok {
		t.Fatalf("a family missing its +Inf bucket must not emit a quantile, got: %+v", out)
	}
	// Nothing should have been recorded as a baseline either, since neither
	// tick was a valid, completable observation.
	if len(o.bucketState) != 0 {
		t.Fatalf("bucket state should stay empty for a family that never had a valid tick, got %+v", o.bucketState)
	}
}

// TestApplyQuantilesBucketSetChangeReseedsThenRecoversOnThirdTick covers the
// full lifecycle around a shape change (an agent/engine upgrade that adds or
// removes a histogram bucket boundary): tick 2 changes the le set from tick
// 1's baseline, which cannot be diffed and must reseed without emitting;
// tick 3 (same shape as tick 2) must then compute normally against tick 2's
// baseline.
func TestApplyQuantilesBucketSetChangeReseedsThenRecoversOnThirdTick(t *testing.T) {
	o := &observability{now: time.Now, bucketState: map[string]*bucketVector{}}
	lesOld := []string{"1", "+Inf"}
	lesNew := []string{"1", "2", "+Inf"}

	// Tick 1: original shape, establishes a baseline.
	o.applyQuantiles(bucketBatch(ttftFamily, "inst-1", lesOld, []float64{5, 10}, 1000))

	// Tick 2: shape changed (a bucket boundary was added) - must reseed
	// without emitting, and without erroring on the length mismatch.
	out2 := o.applyQuantiles(bucketBatch(ttftFamily, "inst-1", lesNew, []float64{4, 9, 12}, 2000))
	if _, ok := findQuantile(t, out2, ttftFamily, "0.5"); ok {
		t.Fatalf("a bucket-set change must reseed without emitting, got: %+v", out2)
	}

	// Tick 3: same (new) shape as tick 2 - must diff normally against it.
	out3 := o.applyQuantiles(bucketBatch(ttftFamily, "inst-1", lesNew, []float64{6, 13, 18}, 3000))
	s, ok := findQuantile(t, out3, ttftFamily, "0.5")
	if !ok {
		t.Fatalf("tick 3 (matching tick 2's shape) should emit a quantile, got: %+v", out3)
	}
	// delta = [6-4, 13-9, 18-12] = [2,4,6]; total=6; p50 rank=3: bucket
	// (1,2], count=4-2=2, offset=3-2=1 -> 1 + (2-1)*(1/2) = 1.5.
	if math.Abs(s.Value-1.5) > 1e-9 {
		t.Fatalf("tick 3 quantile=0.5 = %v, want 1.5", s.Value)
	}
}

// TestEnsureMonotonicClampsDecreasesUpToTheRunningMax is the direct unit
// test for the Prometheus ensureMonotonic correction.
func TestEnsureMonotonicClampsDecreasesUpToTheRunningMax(t *testing.T) {
	counts := []float64{80, 20, 85, 90, 95, 100}
	ensureMonotonic(counts)
	want := []float64{80, 80, 85, 90, 95, 100}
	for i := range want {
		if counts[i] != want[i] {
			t.Fatalf("ensureMonotonic(...) = %v, want %v", counts, want)
		}
	}
}

// TestApplyQuantilesCorrectsANonMonotoneDeltaVector reproduces the measured
// bug: a real engine's histogram is not read atomically across its buckets,
// so a scrape race can make a later (larger le) bucket's delta smaller than
// an earlier one purely from that race. Without ensureMonotonic,
// sort.Search's binary search assumes a monotonic predicate and can return
// an arbitrary index on such a dip - measured as identical (wrong) values
// for every quantile. With the fix, the dip is clamped up to the running
// max (matching Prometheus' own correction) and the quantiles come out
// correct for the corrected vector.
func TestApplyQuantilesCorrectsANonMonotoneDeltaVector(t *testing.T) {
	o := &observability{now: time.Now, bucketState: map[string]*bucketVector{}}
	les := []string{"0.1", "0.5", "1", "2", "5", "+Inf"}
	o.applyQuantiles(bucketBatch(ttftFamily, "inst-1", les, []float64{0, 0, 0, 0, 0, 0}, 1000))

	// Raw delta = [80, 20, 85, 90, 95, 100]: a dip at le=0.5 (20 < 80), the
	// scrape-race artifact. Corrected (ensureMonotonic): [80,80,85,90,95,100].
	out := o.applyQuantiles(bucketBatch(ttftFamily, "inst-1", les, []float64{80, 20, 85, 90, 95, 100}, 2000))

	cases := map[string]float64{
		"0.5":  0 + (0.1-0)*(50.0/80.0), // rank=50, bucket (0,0.1], count=80
		"0.95": 5,                       // rank=95, bucket (2,5], count=95-90=5, offset=5
		"0.99": 5,                       // rank=99 falls in the +Inf bucket -> last finite bound
	}
	for q, want := range cases {
		s, ok := findQuantile(t, out, ttftFamily, q)
		if !ok {
			t.Fatalf("quantile=%s missing: %+v", q, out)
		}
		if math.Abs(s.Value-want) > 1e-9 {
			t.Errorf("quantile=%s = %v, want %v (non-monotone delta must be corrected before searching)", q, s.Value, want)
		}
	}
}

// TestParseLEAcceptsScientificNotationAndInf covers le label forms a real
// Prometheus exposition can use beyond plain decimals.
func TestParseLEAcceptsScientificNotationAndInf(t *testing.T) {
	cases := map[string]float64{
		"1e-03": 0.001,
		"1E-3":  0.001,
		"+Inf":  math.Inf(1),
		"Inf":   math.Inf(1),
		"inf":   math.Inf(1),
	}
	for s, want := range cases {
		got, ok := parseLE(s)
		if !ok {
			t.Fatalf("parseLE(%q) reported not-ok", s)
		}
		if got != want {
			t.Errorf("parseLE(%q) = %v, want %v", s, got, want)
		}
	}
	if _, ok := parseLE("not-a-number"); ok {
		t.Fatal("parseLE(\"not-a-number\") should report not-ok")
	}
}

// fleetScaleBudget is TestApplyQuantilesAtFleetScaleStaysFastAndLossless's
// wall-clock bound. The original bug (a full sweep-then-sort.Slice on every
// group rather than once per batch) cost 14.8s for this same 6000-group
// scenario - multiple orders of magnitude above either bound below - so both
// still catch a regression back to that behavior. Neither bound is the
// review's literal 100ms: measured in-process cost is a few ms, but on a
// shared/loaded host scheduling jitter alone pushed a plain (non-race)
// run to 108ms, so 100ms has no margin at all against ordinary contention;
// 300ms keeps two orders of magnitude of margin below the 14.8s regression
// while tolerating that jitter. The race-detector build gets a much looser
// bound still, since instrumentation overhead (and the same contention) can
// otherwise dominate the measurement far more than the code path under test.
const fleetScaleBudget = 300 * time.Millisecond

var fleetScaleBudgetRace = 3 * time.Second

// TestApplyQuantilesAtFleetScaleStaysFastAndLossless is the regression test
// for item 1's measured bug: 1000 instances x 6 histogram families (6000
// groups, the review's own reproduction) used to cost 14.8s per tick and
// drop 68% of gauges once the bucket-state map was at its (then 4096) cap,
// because eviction ran a full sweep-then-sort.Slice on every single group
// rather than once per batch. At the new default cap (16384, comfortable
// headroom over this fleet size) and with eviction moved to run once per
// batch via a bounded heap, this tick must complete well within budget and
// account for every single group - none evicted mid-batch.
func TestApplyQuantilesAtFleetScaleStaysFastAndLossless(t *testing.T) {
	o := &observability{now: time.Now, bucketState: map[string]*bucketVector{}}
	const instances = 1000
	const families = 6
	les := []string{"0.1", "0.5", "1", "2", "5", "+Inf"}

	familyNames := make([]string, families)
	for i := range familyNames {
		familyNames[i] = fmt.Sprintf("gpuworks_infer_family_%d_seconds", i)
	}
	instanceNames := make([]string, instances)
	for i := range instanceNames {
		instanceNames[i] = fmt.Sprintf("inst-%d", i)
	}

	tick := func(counts []float64, tsMS int64) []tsdb.Sample {
		out := make([]tsdb.Sample, 0, instances*families*len(les))
		for _, instance := range instanceNames {
			for _, family := range familyNames {
				out = append(out, bucketBatch(family, instance, les, counts, tsMS)...)
			}
		}
		return out
	}

	o.applyQuantiles(tick([]float64{2, 5, 8, 9, 10, 10}, 1000)) // seed tick: emits nothing
	if len(o.bucketState) != instances*families {
		t.Fatalf("bucket state after seeding = %d entries, want %d (nothing evicted)", len(o.bucketState), instances*families)
	}

	// Measure the quantile batch, not construction of the synthetic samples.
	batch := tick([]float64{3, 9, 15, 18, 20, 20}, 2000)
	start := time.Now()
	out := o.applyQuantiles(batch)
	elapsed := time.Since(start)

	budget := fleetScaleBudget
	if raceEnabled {
		budget = fleetScaleBudgetRace
	}
	if elapsed > budget {
		t.Fatalf("applyQuantiles took %v for %d groups, want < %v", elapsed, instances*families, budget)
	}
	wantSamples := instances * families * len(quantiles)
	got := 0
	for _, s := range out {
		if s.Labels["quantile"] != "" {
			got++
		}
	}
	if got != wantSamples {
		t.Fatalf("emitted %d quantile samples, want %d (100%% of groups; some were evicted mid-batch)", got, wantSamples)
	}
}
