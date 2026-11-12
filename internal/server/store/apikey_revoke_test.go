package store

import (
	"errors"
	"strings"
	"sync"
	"testing"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/server/auth"
)

func TestRevokeAPIKeyUnknownTargetDoesNotChangeExistingKey(t *testing.T) {
	st := openAPIKeyStore(t)
	key, err := st.CreateAPIKey("keep", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !st.VerifyAPIKey(key) {
		t.Fatal("new key did not authenticate")
	}
	sub := st.Bus.Subscribe(4, "apikey")
	defer sub.Close()
	for _, target := range []string{"gw-unknown000", key, ""} {
		if err := st.RevokeAPIKey(target); !errors.Is(err, ErrNotFound) {
			t.Errorf("unknown target returned %v, want ErrNotFound", err)
		}
	}
	keys, err := st.ListAPIKeys()
	if err != nil || len(keys) != 1 || keys[0].RevokedAt != nil || !st.VerifyAPIKey(key) {
		t.Fatalf("unknown target changed the existing key: rows=%d err=%v", len(keys), err)
	}
	select {
	case event := <-sub.C():
		t.Fatalf("unknown target published a change: %+v", event)
	default:
	}
}

func TestRevokeAPIKeyConcurrentRetriesPublishOnce(t *testing.T) {
	st := openAPIKeyStore(t)
	key, err := st.CreateAPIKey("revoke", 0)
	if err != nil {
		t.Fatal(err)
	}
	sub := st.Bus.Subscribe(16, "apikey")
	defer sub.Close()
	results := make(chan error, 8)
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results <- st.RevokeAPIKey(auth.PrefixOf(key))
		}()
	}
	wg.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Errorf("concurrent revoke: %v", err)
		}
	}
	keys, err := st.ListAPIKeys()
	if err != nil || len(keys) != 1 || keys[0].RevokedAt == nil || st.VerifyAPIKey(key) {
		t.Fatalf("concurrent revoke did not retain one revoked key: rows=%d err=%v", len(keys), err)
	}
	count := 0
	for {
		select {
		case event := <-sub.C():
			if event.Key != auth.PrefixOf(key) || event.Reason != "revoked" {
				t.Errorf("unexpected event: %+v", event)
			}
			count++
		default:
			if count != 1 {
				t.Errorf("revocation events = %d, want one", count)
			}
			return
		}
	}
}

func TestRevokeAPIKeyWriteFailureRollsBackBeforePublishing(t *testing.T) {
	st := openAPIKeyStore(t)
	key, err := st.CreateAPIKey("retain-on-failure", 0)
	if err != nil {
		t.Fatal(err)
	}
	injected := errors.New("injected revoke write failure")
	const callback = "revoke_write_failure"
	if err := st.db.Callback().Update().After("gorm:update").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "api_keys" && strings.Contains(tx.Statement.SQL.String(), "revoked_at") {
			tx.AddError(injected)
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.db.Callback().Update().Remove(callback) })
	sub := st.Bus.Subscribe(4, "apikey")
	defer sub.Close()
	if err := st.RevokeAPIKey(auth.PrefixOf(key)); !errors.Is(err, injected) {
		t.Fatalf("revoke error = %v, want injected failure", err)
	}
	keys, err := st.ListAPIKeys()
	if err != nil || len(keys) != 1 || keys[0].RevokedAt != nil || !st.VerifyAPIKey(key) {
		t.Fatalf("failed revocation committed changes: rows=%d err=%v", len(keys), err)
	}
	select {
	case event := <-sub.C():
		t.Fatalf("failed revocation published %+v", event)
	default:
	}
}

func TestAPIKeyInvalidationSeparatesVerificationFlights(t *testing.T) {
	for _, invalidate := range []string{"revoke-or-scope-change", "create"} {
		t.Run(invalidate, func(t *testing.T) {
			var cache apiKeyCache
			digest := [32]byte{1}
			old, leader := cache.join(digest)
			if !leader {
				t.Fatal("initial request did not lead a verification")
			}
			if invalidate == "create" {
				cache.changed()
			} else {
				cache.forget("gw-revoked")
			}
			fresh, leader := cache.join(digest)
			if !leader || fresh == old {
				t.Fatal("request after invalidation joined the old verification")
			}
			cache.finish(digest, old)
			follower, leader := cache.join(digest)
			if leader || follower != fresh {
				t.Fatal("old verification completion displaced the new verification")
			}
			select {
			case <-old.done:
			default:
				t.Fatal("old waiters were not released")
			}
			select {
			case <-fresh.done:
				t.Fatal("new waiters were released before their verification finished")
			default:
			}
			cache.finish(digest, fresh)
		})
	}
}
