package store

import (
	"errors"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/bus"
)

// countSlowChecks swaps the argon2id verifier for a counting wrapper around
// the real one, so a test can prove which calls paid the slow path.
func countSlowChecks(t *testing.T) *atomic.Int64 {
	t.Helper()
	var calls atomic.Int64
	original := verifyAPIKeyHash
	verifyAPIKeyHash = func(presented, hash string) bool {
		calls.Add(1)
		return auth.VerifyPassword(presented, hash)
	}
	t.Cleanup(func() { verifyAPIKeyHash = original })
	return &calls
}

func openAPIKeyStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open("", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func lastUsed(t *testing.T, st *Store, prefix string) *time.Time {
	t.Helper()
	var k APIKey
	if err := st.db.Where("prefix = ?", prefix).First(&k).Error; err != nil {
		t.Fatal(err)
	}
	return k.LastUsed
}

func TestVerifyAPIKeyPaysTheSlowCheckOncePerKey(t *testing.T) {
	calls := countSlowChecks(t)
	st := openAPIKeyStore(t)
	key, err := st.CreateAPIKey("gateway", 0)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if !st.VerifyAPIKey(key) {
			t.Fatalf("verify #%d rejected a valid key", i)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("argon2id checks = %d for five requests with one key, want 1", got)
	}
}

func TestVerifyAPIKeyNeverCachesARejectedSecret(t *testing.T) {
	calls := countSlowChecks(t)
	st := openAPIKeyStore(t)
	key, err := st.CreateAPIKey("gateway", 0)
	if err != nil {
		t.Fatal(err)
	}
	// Same prefix, wrong secret: it must reach the slow check every time and
	// must never be answered from the cache, even after the real key verified.
	forged := auth.PrefixOf(key) + "-not-the-secret"
	if auth.PrefixOf(forged) != auth.PrefixOf(key) {
		t.Skip("prefix format changed; forged key no longer shares the prefix")
	}
	if !st.VerifyAPIKey(key) {
		t.Fatal("valid key rejected")
	}
	for i := 0; i < 3; i++ {
		if st.VerifyAPIKey(forged) {
			t.Fatal("forged key with a real prefix was accepted")
		}
	}
	if got := calls.Load(); got != 4 {
		t.Fatalf("argon2id checks = %d, want 1 for the real key + 3 for the forged one", got)
	}
	if st.VerifyAPIKey("gpw_unknownprefix_secret") {
		t.Fatal("unknown prefix accepted")
	}
}

func TestRevokeAPIKeyInvalidatesTheCacheImmediately(t *testing.T) {
	st := openAPIKeyStore(t)
	key, err := st.CreateAPIKey("gateway", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !st.VerifyAPIKey(key) {
		t.Fatal("fresh key did not verify")
	}
	sub := st.Bus.Subscribe(4, "apikey")
	defer sub.Close()
	if err := st.RevokeAPIKey(auth.PrefixOf(key)); err != nil {
		t.Fatal(err)
	}
	if st.VerifyAPIKey(key) {
		t.Fatal("revoked key still verified from the cache")
	}
	select {
	case e := <-sub.C():
		if e.Key != auth.PrefixOf(key) || e.Reason != "revoked" {
			t.Fatalf("event = %+v", e)
		}
	case <-time.After(time.Second):
		t.Fatal("revoking published no change")
	}
	// The key stays, marked, so its usage keeps a name; revoking it again
	// changes nothing.
	keys, err := st.ListAPIKeys()
	if err != nil || len(keys) != 1 || keys[0].Name != "gateway" || keys[0].RevokedAt == nil {
		t.Fatalf("keys after revoke = %+v err=%v", keys, err)
	}
	revokedAt := *keys[0].RevokedAt
	if err := st.RevokeAPIKey(auth.PrefixOf(key)); err != nil {
		t.Fatal(err)
	}
	keys, _ = st.ListAPIKeys()
	if !keys[0].RevokedAt.Equal(revokedAt) {
		t.Fatalf("a second revoke moved revoked_at: %v -> %v", revokedAt, keys[0].RevokedAt)
	}
	select {
	case e := <-sub.C():
		t.Fatalf("a no-op revoke published %+v", e)
	default:
	}
	if err := st.RevokeAPIKey("gw-unknown000"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoking an unknown prefix: %v, want ErrNotFound", err)
	}
	// A new key works beside the revoked one.
	other, err := st.CreateAPIKey("gateway", 0)
	if err != nil || !st.VerifyAPIKey(other) || st.VerifyAPIKey(key) {
		t.Fatalf("a new key beside a revoked one: err=%v", err)
	}
}

func TestVerifyAPIKeyCacheExpiresAfterTTL(t *testing.T) {
	calls := countSlowChecks(t)
	st := openAPIKeyStore(t)
	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	st.apiKeys.now = func() time.Time { return now }
	key, err := st.CreateAPIKey("gateway", 0)
	if err != nil {
		t.Fatal(err)
	}
	st.VerifyAPIKey(key)
	now = now.Add(apiKeyCacheTTL - time.Second)
	st.VerifyAPIKey(key)
	if got := calls.Load(); got != 1 {
		t.Fatalf("argon2id checks inside the TTL = %d, want 1", got)
	}
	now = now.Add(2 * time.Second)
	if !st.VerifyAPIKey(key) {
		t.Fatal("valid key rejected after the cache entry expired")
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("argon2id checks after the TTL = %d, want 2", got)
	}
}

func TestVerifyAPIKeyWritesLastUsedAtMostOncePerInterval(t *testing.T) {
	st := openAPIKeyStore(t)
	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	st.apiKeys.now = func() time.Time { return now }
	key, err := st.CreateAPIKey("gateway", 0)
	if err != nil {
		t.Fatal(err)
	}
	prefix := auth.PrefixOf(key)
	st.VerifyAPIKey(key)
	first := lastUsed(t, st, prefix)
	if first == nil || !first.Equal(now) {
		t.Fatalf("last_used after first use = %v, want %v", first, now)
	}
	now = now.Add(apiKeyLastUsedInterval / 2)
	st.VerifyAPIKey(key)
	if got := lastUsed(t, st, prefix); got == nil || !got.Equal(*first) {
		t.Fatalf("last_used rewritten inside the interval: %v", got)
	}
	now = now.Add(apiKeyLastUsedInterval)
	st.VerifyAPIKey(key)
	if got := lastUsed(t, st, prefix); got == nil || !got.Equal(now) {
		t.Fatalf("last_used after the interval = %v, want %v", got, now)
	}
}

func TestAPIKeyCacheBoundsItsSize(t *testing.T) {
	var c apiKeyCache
	for i := 0; i < apiKeyCacheMaxEntries+10; i++ {
		var digest [32]byte
		digest[0], digest[1], digest[2] = byte(i), byte(i>>8), byte(i>>16)
		c.remember(c.snapshot(), digest, APIKeyIdentity{KeyID: uint(i)}, "p")
	}
	if n := len(c.verified); n > apiKeyCacheMaxEntries {
		t.Fatalf("cache holds %d entries, cap is %d", n, apiKeyCacheMaxEntries)
	}
}

func TestAPIKeyCacheDropsReadsThatRacedAChange(t *testing.T) {
	var c apiKeyCache
	// A verification read the key, then the key was deleted before the
	// verification published: the deleted key must not be cached.
	before := c.snapshot()
	c.forget("gw-deleted")
	var digest [32]byte
	c.remember(before, digest, APIKeyIdentity{KeyID: 1}, "gw-deleted")
	if _, ok := c.lookup(digest); ok {
		t.Fatal("a key deleted mid-verification was cached")
	}
	// A verification read the key, then a key was created before the
	// verification published: the result must not be cached either.
	beforeCreate := c.snapshot()
	c.changed()
	c.remember(beforeCreate, digest, APIKeyIdentity{KeyID: 2}, "gw-other")
	if _, ok := c.lookup(digest); ok {
		t.Fatal("a verification that raced a create was cached")
	}
}

func TestVerifyAPIKeyCoalescesConcurrentFirstChecks(t *testing.T) {
	var calls atomic.Int64
	original := verifyAPIKeyHash
	verifyAPIKeyHash = func(presented, hash string) bool {
		calls.Add(1)
		time.Sleep(200 * time.Millisecond) // hold the check open so callers overlap
		return auth.VerifyPassword(presented, hash)
	}
	t.Cleanup(func() { verifyAPIKeyHash = original })
	st := openAPIKeyStore(t)
	key, err := st.CreateAPIKey("gateway", 0)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var rejected atomic.Int64
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if !st.VerifyAPIKey(key) {
				rejected.Add(1)
			}
		}()
	}
	wg.Wait()
	if rejected.Load() != 0 {
		t.Fatalf("%d of 32 concurrent first requests were rejected", rejected.Load())
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("32 concurrent first requests ran %d argon2id checks, want 1", got)
	}
	// A wrong secret is coalesced too, and still rejected for every waiter.
	forged := auth.PrefixOf(key) + "-forged"
	var accepted atomic.Int64
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if st.VerifyAPIKey(forged) {
				accepted.Add(1)
			}
		}()
	}
	wg.Wait()
	if accepted.Load() != 0 {
		t.Fatal("a coalesced wrong secret was accepted")
	}
}

func TestVerifyAPIKeyConcurrentCallersAgree(t *testing.T) {
	st := openAPIKeyStore(t)
	key, err := st.CreateAPIKey("gateway", 0)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var rejected atomic.Int64
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				if !st.VerifyAPIKey(key) {
					rejected.Add(1)
				}
			}
		}()
	}
	wg.Wait()
	if rejected.Load() != 0 {
		t.Fatalf("%d concurrent verifications rejected a valid key", rejected.Load())
	}
}

func TestResolveAPIKeyNamesTheKeyAndOwnerOnBothPaths(t *testing.T) {
	calls := countSlowChecks(t)
	st := openAPIKeyStore(t)
	key, err := st.CreateAPIKey("gateway", 7)
	if err != nil {
		t.Fatal(err)
	}
	other, err := st.CreateAPIKey("other", 9)
	if err != nil {
		t.Fatal(err)
	}
	first, ok := st.ResolveAPIKey(key)
	if !ok || first.UserID != 7 || first.KeyID == 0 {
		t.Fatalf("slow path identity = %+v, %v", first, ok)
	}
	cached, ok := st.ResolveAPIKey(key)
	if !ok || !reflect.DeepEqual(cached, first) {
		t.Fatalf("cached identity = %+v, want %+v", cached, first)
	}
	if calls.Load() != 1 {
		t.Fatalf("the second resolve must come from the cache, slow checks = %d", calls.Load())
	}
	second, ok := st.ResolveAPIKey(other)
	if !ok || second.UserID != 9 || second.KeyID == first.KeyID {
		t.Fatalf("other key identity = %+v", second)
	}
	if bad, ok := st.ResolveAPIKey(key + "x"); ok || !reflect.DeepEqual(bad, APIKeyIdentity{}) {
		t.Fatalf("a wrong secret must resolve to nothing, got %+v %v", bad, ok)
	}
}

func TestResolveAPIKeyConcurrentWaitersShareTheIdentity(t *testing.T) {
	st := openAPIKeyStore(t)
	key, err := st.CreateAPIKey("gateway", 3)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make([]APIKeyIdentity, 8)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], _ = st.ResolveAPIKey(key)
		}(i)
	}
	wg.Wait()
	for i, got := range results {
		if got.UserID != 3 || got.KeyID != results[0].KeyID || got.KeyID == 0 {
			t.Fatalf("waiter %d got %+v", i, got)
		}
	}
}
