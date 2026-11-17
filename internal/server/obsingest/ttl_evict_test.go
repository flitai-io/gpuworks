package obsingest

import (
	"testing"
	"time"
)

type ttlEntry struct{ expires time.Time }

// TestEvictBoundedLockedKeepsExactlyTheNewestEntries checks the top-k
// selection itself, independent of any observability wiring: given entries
// with distinct expiries, trimming to a limit must drop exactly the ones
// nearest to expiry (oldest) and keep the rest, regardless of map
// iteration order.
func TestEvictBoundedLockedKeepsExactlyTheNewestEntries(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	m := map[string]ttlEntry{}
	for i := 0; i < 10; i++ {
		m[string(rune('a'+i))] = ttlEntry{expires: base.Add(time.Duration(i) * time.Second)}
	}
	got := evictBoundedLocked(m, 4, func(v ttlEntry) time.Time { return v.expires })
	if got != 6 {
		t.Fatalf("evictBoundedLocked returned %d, want 6 evicted", got)
	}
	if len(m) != 4 {
		t.Fatalf("len = %d, want 4", len(m))
	}
	// The 4 newest (largest expires) are indices 6..9 ('g'..'j').
	for i := 6; i < 10; i++ {
		if _, ok := m[string(rune('a'+i))]; !ok {
			t.Fatalf("newest entry %q was evicted: %+v", string(rune('a'+i)), m)
		}
	}
	for i := 0; i < 6; i++ {
		if _, ok := m[string(rune('a'+i))]; ok {
			t.Fatalf("oldest entry %q survived eviction: %+v", string(rune('a'+i)), m)
		}
	}
}

// TestEvictBoundedLockedIsANoOpUnderLimit ensures the common case (map
// already within limit) costs a single length check.
func TestEvictBoundedLockedIsANoOpUnderLimit(t *testing.T) {
	m := map[string]ttlEntry{"a": {}, "b": {}}
	got := evictBoundedLocked(m, 10, func(v ttlEntry) time.Time { return v.expires })
	if got != 0 {
		t.Fatalf("evictBoundedLocked returned %d, want 0 (no-op)", got)
	}
	if len(m) != 2 {
		t.Fatalf("len = %d, want 2 (no-op)", len(m))
	}
}

// TestSweepExpiredLockedRemovesOnlyExpiredEntries covers the periodic-tick
// half: entries at or past `now` are removed, entries still fresh are kept.
func TestSweepExpiredLockedRemovesOnlyExpiredEntries(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	m := map[string]ttlEntry{
		"expired-exactly": {expires: now},
		"expired-past":    {expires: now.Add(-time.Second)},
		"fresh":           {expires: now.Add(time.Second)},
	}
	sweepExpiredLocked(m, now, func(v ttlEntry) time.Time { return v.expires })
	if len(m) != 1 {
		t.Fatalf("m = %+v, want only \"fresh\" left", m)
	}
	if _, ok := m["fresh"]; !ok {
		t.Fatalf("fresh entry was swept: %+v", m)
	}
}
