package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/server/auth"
)

func TestRevokedKeyRejectsNewGatewayRequestWhileEarlierVerificationIsBlocked(t *testing.T) {
	rig := newKeyScopeRig(t)
	key, err := rig.st.CreateAPIKey("revoke-during-verification", 0)
	if err != nil {
		t.Fatal(err)
	}
	other, err := rig.st.CreateAPIKey("keep-authentication-enabled", 0)
	if err != nil {
		t.Fatal(err)
	}
	prefix := auth.PrefixOf(key)
	entered, released := make(chan struct{}), make(chan struct{})
	var held atomic.Bool
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(released) }) }
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	var requests sync.WaitGroup
	const callback = "hold_key_read_before_revocation"
	if err := rig.st.DB().Callback().Query().After("gorm:query").Register(callback, func(tx *gorm.DB) {
		if tx.Error != nil || tx.Statement.Table != "api_keys" || strings.Contains(strings.ToLower(tx.Statement.SQL.String()), "count(") {
			return
		}
		for _, value := range tx.Statement.Vars {
			if text, ok := value.(string); ok && text == prefix && held.CompareAndSwap(false, true) {
				close(entered)
				<-released
				return
			}
		}
	}); err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		release()
		cancel()
		requests.Wait()
		_ = rig.st.DB().Callback().Query().Remove(callback)
	})
	request := func(secret string) int {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"alpha","messages":[]}`)).WithContext(ctx)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+secret)
		rec := httptest.NewRecorder()
		rig.p.Handler()(rec, req)
		return rec.Code
	}
	startRequest := func(secret string) <-chan int {
		status := make(chan int, 1)
		requests.Add(1)
		go func() {
			defer requests.Done()
			status <- request(secret)
		}()
		return status
	}

	beforeRevoke := startRequest(key)
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("earlier request did not read the live key")
	}
	if err := rig.st.RevokeAPIKey(prefix); err != nil {
		t.Fatal(err)
	}
	if status := request(other); status != http.StatusOK {
		t.Fatalf("unrelated live key: HTTP %d, want 200", status)
	}

	// This request starts only after revocation has completed. It must not
	// wait for or inherit the result of the earlier verification.
	afterRevoke := startRequest(key)
	var afterStatus int
	select {
	case afterStatus = <-afterRevoke:
		if hits := rig.upstreamHits.Load(); hits != 1 {
			t.Errorf("upstream hits before releasing earlier request = %d, want only the unrelated key's request", hits)
		}
	case <-time.After(3 * time.Second):
		t.Error("new request waited for a verification that predates revocation")
		release()
		select {
		case afterStatus = <-afterRevoke:
		case <-ctx.Done():
			t.Fatal("new request did not complete after releasing earlier verification")
		}
	}
	if afterStatus != http.StatusUnauthorized {
		t.Errorf("new request after completed revocation: HTTP %d, want 401", afterStatus)
	}
	release()
	var beforeStatus int
	select {
	case beforeStatus = <-beforeRevoke:
	case <-ctx.Done():
		t.Fatal("earlier request did not complete after release")
	}
	if beforeStatus != http.StatusOK && beforeStatus != http.StatusUnauthorized {
		t.Errorf("earlier request: HTTP %d, want 200 or 401", beforeStatus)
	}
	if status := request(key); status != http.StatusUnauthorized {
		t.Errorf("fresh revoked-key request: HTTP %d, want 401", status)
	}
	wantHits := int64(1)
	if beforeStatus == http.StatusOK {
		wantHits++
	}
	if hits := rig.upstreamHits.Load(); hits != wantHits {
		t.Errorf("upstream hits = %d, want %d; revoked requests must not reach the engine", hits, wantHits)
	}
}
