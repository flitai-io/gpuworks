package obsingest

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/tsdb"
)

// fakeTSDBStore records every batch WriteBatch receives, so a test can assert
// on the labels IngestMetrics actually persisted without a real SQLite file.
type fakeTSDBStore struct {
	mu      sync.Mutex
	written []tsdb.Sample
}

func (f *fakeTSDBStore) WriteBatch(samples []tsdb.Sample) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.written = append(f.written, samples...)
	return nil
}
func (f *fakeTSDBStore) QueryRange(string, map[string]string, int64, int64, int64) ([]tsdb.Series, error) {
	return nil, nil
}
func (f *fakeTSDBStore) QueryLatest(string, map[string]string, int64, ...int64) ([]tsdb.LatestPoint, error) {
	return nil, nil
}
func (f *fakeTSDBStore) Retain(context.Context, int64) error { return nil }
func (f *fakeTSDBStore) Close() error                        { return nil }

func (f *fakeTSDBStore) samples() []tsdb.Sample {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]tsdb.Sample, len(f.written))
	copy(out, f.written)
	return out
}

// newTestObservability builds an observability wired to a fakeTSDBStore and a
// resolver backed by a plain map, plus a call counter so tests can assert on
// cache behavior. Every resolution is cacheable, matching a resolver that
// never sees a transient error (see TestDeploymentForCachesConfirmedNotFoundButNotATransientError
// for that path).
func newTestObservability(known map[string]uint) (*Pipeline, *fakeTSDBStore, *int) {
	store := &fakeTSDBStore{}
	calls := 0
	o := &Pipeline{
		tsdb:        store,
		now:         time.Now,
		deployCache: map[string]deploymentLookup{},
		resolveDeployment: func(instanceUUID string) (uint, bool, bool) {
			calls++
			id, ok := known[instanceUUID]
			return id, ok, true
		},
	}
	return o, store, &calls
}

func TestIngestMetricsPassesThroughSampleAlreadyLabeled(t *testing.T) {
	o, store, calls := newTestObservability(map[string]uint{"inst-1": 7})
	labels := map[string]string{"node": "n1", "instance": "inst-1", "deployment": "9"}
	o.IngestMetrics("node-1", []proto.MetricSample{{Metric: "m", Labels: labels, Value: 1, TimeMS: 1000}})

	got := store.samples()
	if len(got) != 1 || got[0].Labels["deployment"] != "9" {
		t.Fatalf("written = %+v, want deployment=9 preserved (resolver must not override it)", got)
	}
	if *calls != 0 {
		t.Fatalf("resolver calls = %d, want 0 (already labeled)", *calls)
	}
}

func TestIngestMetricsBackfillsMissingDeploymentLabel(t *testing.T) {
	o, store, calls := newTestObservability(map[string]uint{"inst-1": 7})
	labels := map[string]string{"node": "n1", "instance": "inst-1"}
	o.IngestMetrics("node-1", []proto.MetricSample{{Metric: "m", Labels: labels, Value: 1, TimeMS: 1000}})

	got := store.samples()
	if len(got) != 1 || got[0].Labels["deployment"] != "7" {
		t.Fatalf("written = %+v, want deployment=7 backfilled", got)
	}
	if got[0].Labels["instance"] != "inst-1" || got[0].Labels["node"] != "n1" {
		t.Fatalf("written labels lost original keys: %+v", got[0].Labels)
	}
	if *calls != 1 {
		t.Fatalf("resolver calls = %d, want 1", *calls)
	}
	// The caller's own map must not have been mutated in place.
	if _, ok := labels["deployment"]; ok {
		t.Fatalf("original labels map was mutated: %+v", labels)
	}
}

func TestIngestMetricsLeavesUnknownInstanceUnlabeled(t *testing.T) {
	o, store, calls := newTestObservability(map[string]uint{})
	labels := map[string]string{"node": "n1", "instance": "inst-ghost"}
	o.IngestMetrics("node-1", []proto.MetricSample{{Metric: "m", Labels: labels, Value: 1, TimeMS: 1000}})

	got := store.samples()
	if len(got) != 1 {
		t.Fatalf("written = %+v, want 1 sample", got)
	}
	if _, ok := got[0].Labels["deployment"]; ok {
		t.Fatalf("unknown instance got a deployment label: %+v", got[0].Labels)
	}
	if *calls != 1 {
		t.Fatalf("resolver calls = %d, want 1", *calls)
	}
}

func TestIngestMetricsSkipsSamplesWithoutAnInstanceLabel(t *testing.T) {
	o, store, calls := newTestObservability(map[string]uint{"inst-1": 7})
	labels := map[string]string{"node": "n1", "gpu": "0"}
	o.IngestMetrics("node-1", []proto.MetricSample{{Metric: "gpuworks_gpu_utilization", Labels: labels, Value: 1, TimeMS: 1000}})

	got := store.samples()
	if len(got) != 1 || got[0].Labels["gpu"] != "0" {
		t.Fatalf("written = %+v", got)
	}
	if _, ok := got[0].Labels["deployment"]; ok {
		t.Fatalf("gpu sample got a deployment label: %+v", got[0].Labels)
	}
	if *calls != 0 {
		t.Fatalf("resolver calls = %d, want 0 (no instance label to resolve)", *calls)
	}
}

func TestIngestMetricsCachesResolverWithinTTLThenRefreshesAfterExpiry(t *testing.T) {
	o, _, calls := newTestObservability(map[string]uint{"inst-1": 7})
	now := time.Unix(1_700_000_000, 0)
	o.now = func() time.Time { return now }

	labels := func() map[string]string { return map[string]string{"node": "n1", "instance": "inst-1"} }
	o.IngestMetrics("node-1", []proto.MetricSample{{Metric: "m", Labels: labels(), Value: 1, TimeMS: 1000}})
	if *calls != 1 {
		t.Fatalf("resolver calls after first ingest = %d, want 1", *calls)
	}

	// Still within the TTL: a second batch for the same instance must not hit
	// the resolver again.
	now = now.Add(deploymentLookupTTL / 2)
	o.IngestMetrics("node-1", []proto.MetricSample{{Metric: "m", Labels: labels(), Value: 2, TimeMS: 2000}})
	if *calls != 1 {
		t.Fatalf("resolver calls within TTL = %d, want still 1 (cache hit)", *calls)
	}

	// Past the TTL: the cache entry has expired and must be refreshed.
	now = now.Add(deploymentLookupTTL)
	o.IngestMetrics("node-1", []proto.MetricSample{{Metric: "m", Labels: labels(), Value: 3, TimeMS: 3000}})
	if *calls != 2 {
		t.Fatalf("resolver calls after TTL expiry = %d, want 2", *calls)
	}
}

func TestIngestMetricsWithNilResolverLeavesSamplesUnlabeled(t *testing.T) {
	store := &fakeTSDBStore{}
	o := &Pipeline{tsdb: store, now: time.Now, deployCache: map[string]deploymentLookup{}}
	labels := map[string]string{"node": "n1", "instance": "inst-1"}
	o.IngestMetrics("node-1", []proto.MetricSample{{Metric: "m", Labels: labels, Value: 1, TimeMS: 1000}})

	got := store.samples()
	if len(got) != 1 {
		t.Fatalf("written = %+v", got)
	}
	if _, ok := got[0].Labels["deployment"]; ok {
		t.Fatalf("sample got a deployment label with no resolver wired: %+v", got[0].Labels)
	}
}

// TestDeploymentForCachesConfirmedNotFoundButNotATransientError exercises
// deploymentFor's contract for the two ways a resolver answers "not found":
// a resolver-reported transient error (cacheable=false) must be retried on
// every call, while a resolver-confirmed "no such instance" (cacheable=true)
// must be cached like a positive result.
func TestDeploymentForCachesConfirmedNotFoundButNotATransientError(t *testing.T) {
	calls := 0
	cacheable := false
	o := &Pipeline{
		now:         time.Now,
		deployCache: map[string]deploymentLookup{},
		resolveDeployment: func(string) (uint, bool, bool) {
			calls++
			return 0, false, cacheable
		},
	}

	if id, ok := o.deploymentFor("inst-x"); ok || id != 0 {
		t.Fatalf("deploymentFor = %d,%v, want 0,false", id, ok)
	}
	if id, ok := o.deploymentFor("inst-x"); ok || id != 0 {
		t.Fatalf("deploymentFor = %d,%v, want 0,false", id, ok)
	}
	if calls != 2 {
		t.Fatalf("resolver calls for a transient error = %d, want 2 (must not be cached)", calls)
	}

	cacheable = true
	calls = 0
	if id, ok := o.deploymentFor("inst-y"); ok || id != 0 {
		t.Fatalf("deploymentFor = %d,%v, want 0,false", id, ok)
	}
	if id, ok := o.deploymentFor("inst-y"); ok || id != 0 {
		t.Fatalf("deploymentFor = %d,%v, want 0,false", id, ok)
	}
	if calls != 1 {
		t.Fatalf("resolver calls for a confirmed not-found = %d, want 1 (must be cached)", calls)
	}
}

// TestWrapDeploymentLookupDistinguishesNotFoundFromOtherErrors pins the
// app.go wiring's own logic in isolation: store.ErrNotFound (represented here
// by a sentinel so this package stays free of store types) becomes a
// cacheable negative, any other error becomes an uncacheable negative, and a
// successful lookup is cacheable.
func TestWrapDeploymentLookupDistinguishesNotFoundFromOtherErrors(t *testing.T) {
	notFound := errors.New("not found")
	transient := errors.New("database is locked")
	lookup := WrapDeploymentLookup(func(instanceUUID string) (uint, error) {
		switch instanceUUID {
		case "inst-known":
			return 7, nil
		case "inst-missing":
			return 0, notFound
		default:
			return 0, transient
		}
	}, notFound)

	if id, found, cacheable := lookup("inst-known"); id != 7 || !found || !cacheable {
		t.Fatalf("known instance = %d,%v,%v, want 7,true,true", id, found, cacheable)
	}
	if id, found, cacheable := lookup("inst-missing"); id != 0 || found || !cacheable {
		t.Fatalf("confirmed not-found = %d,%v,%v, want 0,false,true", id, found, cacheable)
	}
	if id, found, cacheable := lookup("inst-error"); id != 0 || found || cacheable {
		t.Fatalf("transient error = %d,%v,%v, want 0,false,false", id, found, cacheable)
	}
}

// TestDeployCacheEvictsExpiredEntriesFirstWhenOverCapacity checks the cheap
// path: once the cache exceeds its cap, expired entries alone are enough to
// bring it back under, so nothing else needs to be dropped.
func TestDeployCacheEvictsExpiredEntriesFirstWhenOverCapacity(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	o := &Pipeline{
		now:            func() time.Time { return now },
		deployCache:    map[string]deploymentLookup{},
		deployCacheCap: 3,
		resolveDeployment: func(instanceUUID string) (uint, bool, bool) {
			return 1, true, true
		},
	}

	// Two entries written now, then time passes their TTL...
	o.deploymentFor("inst-old-1")
	o.deploymentFor("inst-old-2")
	now = now.Add(deploymentLookupTTL + time.Second)
	// ...then a third and fourth push the cache past its cap of 3.
	o.deploymentFor("inst-new-1")
	o.deploymentFor("inst-new-2")

	// Eviction is no longer inline per insert (see ttl_evict.go): a real
	// ingest tick calls the periodic sweep (RunMetrics' ticker) and
	// the once-per-batch bounded fallback (writeMetricsBatch); simulate that
	// exact sequence directly.
	o.sweepExpiredDeployCache(now)
	o.evictDeployCacheIfOverCapacity()

	if len(o.deployCache) != 2 {
		t.Fatalf("cache = %+v, want only the 2 unexpired entries left", o.deployCache)
	}
	for _, uuid := range []string{"inst-old-1", "inst-old-2"} {
		if _, ok := o.deployCache[uuid]; ok {
			t.Fatalf("expired entry %s survived eviction: %+v", uuid, o.deployCache)
		}
	}
	for _, uuid := range []string{"inst-new-1", "inst-new-2"} {
		if _, ok := o.deployCache[uuid]; !ok {
			t.Fatalf("unexpired entry %s was evicted: %+v", uuid, o.deployCache)
		}
	}
}

// TestDeployCacheDropsOldestWhenStillOverCapacityAfterSweepingExpired covers
// the harder path: nothing has expired yet, so the sweep alone cannot bring
// the cache back under its cap, and the entries nearest to expiry (the
// earliest inserted, since every entry shares the same TTL) must be dropped
// instead.
func TestDeployCacheDropsOldestWhenStillOverCapacityAfterSweepingExpired(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	o := &Pipeline{
		now:            func() time.Time { return now },
		deployCache:    map[string]deploymentLookup{},
		deployCacheCap: 3,
		resolveDeployment: func(instanceUUID string) (uint, bool, bool) {
			return 1, true, true
		},
	}

	// Insert 5 entries, a tick apart, none of which will have expired by the
	// time the last one triggers eviction.
	for i := 0; i < 5; i++ {
		o.deploymentFor(fmt.Sprintf("inst-%d", i))
		now = now.Add(time.Millisecond)
	}
	o.sweepExpiredDeployCache(now)
	o.evictDeployCacheIfOverCapacity()

	if len(o.deployCache) != 3 {
		t.Fatalf("cache size = %d, want capped at 3: %+v", len(o.deployCache), o.deployCache)
	}
	// The two oldest (earliest inserted, so earliest to expire) must be gone;
	// the three most recently inserted must remain.
	for _, uuid := range []string{"inst-0", "inst-1"} {
		if _, ok := o.deployCache[uuid]; ok {
			t.Fatalf("oldest entry %s survived eviction: %+v", uuid, o.deployCache)
		}
	}
	for _, uuid := range []string{"inst-2", "inst-3", "inst-4"} {
		if _, ok := o.deployCache[uuid]; !ok {
			t.Fatalf("newest entry %s was evicted: %+v", uuid, o.deployCache)
		}
	}
}

// TestEvictDeployCacheIfOverCapacityIncrementsDeployEvictedCounter covers the
// observability counter reported on the "metrics ingest pipeline" log line:
// over-cap eviction (real data loss under sustained load, unlike a routine
// TTL sweep) must be visible as a running total, not only inferrable from
// silence.
func TestEvictDeployCacheIfOverCapacityIncrementsDeployEvictedCounter(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	o := &Pipeline{
		now:            func() time.Time { return now },
		deployCache:    map[string]deploymentLookup{},
		deployCacheCap: 3,
		resolveDeployment: func(instanceUUID string) (uint, bool, bool) {
			return 1, true, true
		},
	}
	for i := 0; i < 5; i++ {
		o.deploymentFor(fmt.Sprintf("inst-%d", i))
		now = now.Add(time.Millisecond)
	}
	o.evictDeployCacheIfOverCapacity()

	if got := o.deployEvicted.Load(); got != 2 {
		t.Fatalf("deployEvicted = %d, want 2 (5 inserts over a cap of 3)", got)
	}
	if len(o.deployCache) != 3 {
		t.Fatalf("cache size = %d, want 3", len(o.deployCache))
	}
}

// New's pipeline has no queues: ingest writes on the caller's goroutine and
// the writers have nothing to run, so they return at once.
func TestNewWritesOnTheCallersGoroutine(t *testing.T) {
	metrics := &fakeTSDBStore{}
	p := New(Stores{TSDB: metrics})
	p.IngestMetrics("node", []proto.MetricSample{{Metric: "util", Labels: map[string]string{"node": "n"}, Value: 1, TimeMS: 1000}})
	if got := metrics.samples(); len(got) != 1 || got[0].Metric != "util" {
		t.Fatalf("written = %+v, want the one sample at once", got)
	}
	done := make(chan struct{})
	go func() {
		p.RunLogs(context.Background())
		p.RunMetrics(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the writers of a pipeline without queues did not return")
	}
}
