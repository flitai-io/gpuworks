package obsingest

import (
	"container/heap"
	"time"
)

// evictBoundedLocked and sweepExpiredLocked implement the eviction policy
// shared by the pipeline's two TTL-keyed maps: the deployment-label
// resolver cache (deployCache) and the quantile-at-ingest bucket-vector
// state (bucketState). Both used to run a full sweep-then-sort.Slice pass
// every time a single entry was inserted (deploymentFor / foldBucketGroup);
// at fleet scale that meant one full-map sort per resolved instance or per
// histogram group within a batch - up to thousands of times per ingest tick
// - and once the map was actually at capacity, every one of those calls paid
// the expensive path. Measured: 1000 instances x 6 histogram families at a
// 4096 cap cost 14.8s per tick and dropped 68% of gauges (entries evicted
// mid-batch before they could be diffed against their previous tick).
//
// The fix has two parts, matching the two functions here:
//   - The full O(n) "delete everything already expired" sweep is cheap in
//     isolation but must not run on every insert; it is instead called once
//     per RunMetrics observe tick (every 5s - the TTLs involved are
//     30s and 10m, so that cadence is still far more frequent than needed).
//   - The "still over cap after sweeping" fallback no longer sorts the whole
//     map: it selects exactly the entries to evict via a bounded max-heap
//     (O(n log k), k = the number evicted) instead of sort.Slice (O(n log
//     n) regardless of how far over cap the map is), and callers invoke it
//     once per batch (writeMetricsBatch / applyQuantiles), not once per
//     entry inserted within it.

// evictBoundedLocked trims m to at most limit entries, preferring to drop
// the entries nearest to expiry (the same ordering as "oldest inserted",
// since every entry in these maps shares one TTL). It does not check
// expiry itself - see sweepExpiredLocked for that - so between periodic
// sweeps a churning fleet can go over cap purely on insert volume, and this
// is the bounded fallback for that case. Callers must hold the map's own
// mutex. Returns the number of entries evicted, so a caller can add it to an
// observable counter: unlike sweepExpiredLocked's routine cleanup of
// already-dead entries, an over-cap eviction here is discarding an entry
// that was still live, i.e. real data loss under sustained over-capacity
// load, and worth surfacing rather than only inferring from silence.
func evictBoundedLocked[K comparable, V any](m map[K]V, limit int, expiresAt func(V) time.Time) int {
	over := len(m) - limit
	if over <= 0 {
		return 0
	}
	h := make(oldestHeap[K], 0, over)
	for k, v := range m {
		exp := expiresAt(v)
		if len(h) < over {
			h = append(h, oldestEntry[K]{key: k, expires: exp})
			if len(h) == over {
				heap.Init(&h)
			}
			continue
		}
		if exp.Before(h[0].expires) {
			h[0] = oldestEntry[K]{key: k, expires: exp}
			heap.Fix(&h, 0)
		}
	}
	for _, e := range h {
		delete(m, e.key)
	}
	return len(h)
}

// sweepExpiredLocked removes every entry whose TTL has already elapsed. It
// is an unavoidable O(n) pass, so it is only ever called from a
// low-frequency periodic tick, never per-insert. Callers must hold the
// map's own mutex.
func sweepExpiredLocked[K comparable, V any](m map[K]V, now time.Time, expiresAt func(V) time.Time) {
	for k, v := range m {
		if !now.Before(expiresAt(v)) {
			delete(m, k)
		}
	}
}

// oldestEntry/oldestHeap is a bounded max-heap (largest `expires` on top) so
// evictBoundedLocked can keep exactly the `over` smallest-expiry (oldest)
// entries seen while scanning the map once: once the heap holds `over`
// entries, a new entry only replaces the current max when it is itself
// older, which is the standard O(n log k) top-k-smallest selection.
type oldestEntry[K comparable] struct {
	key     K
	expires time.Time
}

type oldestHeap[K comparable] []oldestEntry[K]

func (h oldestHeap[K]) Len() int           { return len(h) }
func (h oldestHeap[K]) Less(i, j int) bool { return h[i].expires.After(h[j].expires) }
func (h oldestHeap[K]) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *oldestHeap[K]) Push(x any)        { *h = append(*h, x.(oldestEntry[K])) }
func (h *oldestHeap[K]) Pop() any {
	old := *h
	n := len(old)
	item := old[n-1]
	*h = old[:n-1]
	return item
}
