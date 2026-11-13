package proxy

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// The gateway asks for a key unless an administrator says otherwise; the
// number of keys that exist is not a setting.
//
//	go test ./internal/server/proxy -run 'TestGateway' -race -timeout 5m
func TestGatewayRequiresAKeyByDefault(t *testing.T) {
	rig := newKeyScopeRig(t)
	// No key exists yet, and no switch was turned on: the request is refused
	// before it can reach an engine.
	if code, body := rig.chat(t, "", "alpha"); code != http.StatusUnauthorized {
		t.Fatalf("keyless request with no keys in the database = %d %s, want 401", code, body)
	}
	if hits := rig.upstreamHits.Load(); hits != 0 {
		t.Fatalf("a refused request reached an engine (%d hits)", hits)
	}
	// The same holds with a live key present.
	live, err := rig.st.CreateAPIKey("live", 0)
	if err != nil {
		t.Fatal(err)
	}
	if code, body := rig.chat(t, "", "alpha"); code != http.StatusUnauthorized {
		t.Fatalf("keyless request beside a live key = %d %s, want 401", code, body)
	}
	if code, body := rig.chat(t, live, "alpha"); code != http.StatusOK {
		t.Fatalf("valid key = %d %s, want 200", code, body)
	}
	if code, body := rig.chat(t, "gpw_bogus_secret", "alpha"); code != http.StatusUnauthorized {
		t.Fatalf("invalid key = %d %s, want 401", code, body)
	}
}

// Revoking the last key used to open the gateway: the auth gate was derived
// from whether any unrevoked key existed. It must not change the posture now.
func TestRevokingTheLastKeyDoesNotOpenTheGateway(t *testing.T) {
	rig := newKeyScopeRig(t)
	key, err := rig.st.CreateAPIKey("only-key", 0)
	if err != nil {
		t.Fatal(err)
	}
	if code, body := rig.chat(t, key, "alpha"); code != http.StatusOK {
		t.Fatalf("baseline with the key = %d %s", code, body)
	}
	if err := rig.st.RevokeAPIKey(auth.PrefixOf(key)); err != nil {
		t.Fatal(err)
	}
	before := rig.upstreamHits.Load()
	if code, body := rig.chat(t, "", "alpha"); code != http.StatusUnauthorized {
		t.Fatalf("keyless request after revoking the last key = %d %s, want 401", code, body)
	}
	if code, body := rig.chat(t, key, "alpha"); code != http.StatusUnauthorized {
		t.Fatalf("revoked key = %d %s, want 401", code, body)
	}
	if hits := rig.upstreamHits.Load(); hits != before {
		t.Fatalf("a refused request reached an engine (%d -> %d)", before, hits)
	}
}

// A failed read of the key count used to answer "no keys", which skipped key
// validation entirely and accepted even a forged key. The gateway now decides
// on the setting, so a count failure cannot open it.
func TestFailedKeyCountDoesNotOpenTheGateway(t *testing.T) {
	rig := newKeyScopeRig(t)
	if _, err := rig.st.CreateAPIKey("live-key", 0); err != nil {
		t.Fatal(err)
	}
	injectKeyCountFailure(t, rig.st)
	if code, body := rig.chat(t, "", "alpha"); code != http.StatusUnauthorized {
		t.Fatalf("keyless request while the key count fails = %d %s, want 401", code, body)
	}
	if code, body := rig.chat(t, "gpw_bogus_secret", "alpha"); code != http.StatusUnauthorized {
		t.Fatalf("forged key while the key count fails = %d %s, want 401", code, body)
	}
	if hits := rig.upstreamHits.Load(); hits != 0 {
		t.Fatalf("a refused request reached an engine (%d hits)", hits)
	}
}

// An unreadable open-access setting is closed, not open. The failure is
// injected on the settings read so nothing else about the request changes.
func TestUnreadableOpenAccessSettingRefusesKeylessRequests(t *testing.T) {
	rig := newKeyScopeRig(t)
	live, err := rig.st.CreateAPIKey("live", 0)
	if err != nil {
		t.Fatal(err)
	}
	injectSettingReadFailure(t, rig.st)
	if code, body := rig.chat(t, "", "alpha"); code != http.StatusUnauthorized {
		t.Fatalf("keyless request with an unreadable setting = %d %s, want 401", code, body)
	}
	// A request that presents a key does not need the setting at all, so it
	// keeps working: a database hiccup must not lock out every client.
	if code, body := rig.chat(t, live, "alpha"); code != http.StatusOK {
		t.Fatalf("valid key with an unreadable setting = %d %s, want 200", code, body)
	}
}

// Open access is an explicit switch. When an administrator turns it on, a
// request without a key is accepted anonymously; a request that does present
// one is still verified, so a forged key is not a way in.
func TestExplicitOpenAccessAcceptsOnlyKeylessRequests(t *testing.T) {
	rig := newKeyScopeRig(t)
	live, err := rig.st.CreateAPIKey("live", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := rig.st.SetSecurityPolicy(store.SecurityPolicy{InferenceOpenAccess: true}); err != nil {
		t.Fatal(err)
	}
	code, body := rig.chat(t, "", "alpha")
	if code != http.StatusOK {
		t.Fatalf("keyless request with open access on = %d %s, want 200", code, body)
	}
	if code, body := rig.chat(t, "gpw_bogus_secret", "alpha"); code != http.StatusUnauthorized {
		t.Fatalf("forged key with open access on = %d %s, want 401", code, body)
	}
	if code, body := rig.chat(t, live, "alpha"); code != http.StatusOK {
		t.Fatalf("valid key with open access on = %d %s, want 200", code, body)
	}
}

// Turning the switch back off closes the gateway again. The reading is cached
// for a second, so the assertion allows that window to pass.
func TestOpenAccessCanBeTurnedBackOff(t *testing.T) {
	rig := newKeyScopeRig(t)
	if err := rig.st.SetSecurityPolicy(store.SecurityPolicy{InferenceOpenAccess: true}); err != nil {
		t.Fatal(err)
	}
	if code, body := rig.chat(t, "", "alpha"); code != http.StatusOK {
		t.Fatalf("keyless request with open access on = %d %s, want 200", code, body)
	}
	if err := rig.st.SetSecurityPolicy(store.SecurityPolicy{}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * inferenceOpenAccessTTL)
	for {
		code, body := rig.chat(t, "", "alpha")
		if code == http.StatusUnauthorized {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("keyless request still accepted long after the switch was turned off: %d %s", code, body)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// injectKeyCountFailure makes every count of unrevoked keys fail.
func injectKeyCountFailure(t *testing.T, st *store.Store) {
	t.Helper()
	const name = "auth:fail_key_count"
	err := st.DB().Callback().Query().Before("gorm:query").Register(name, func(tx *gorm.DB) {
		if tx.Statement.Table == "api_keys" {
			if _, ok := tx.Statement.Dest.(*int64); ok {
				tx.AddError(errors.New("injected: database is briefly unavailable"))
			}
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.DB().Callback().Query().Remove(name) })
}

// injectSettingReadFailure makes reads of the settings table fail, which is
// how the open-access switch becomes unreadable.
func injectSettingReadFailure(t *testing.T, st *store.Store) {
	t.Helper()
	const name = "security:fail_setting_read"
	err := st.DB().Callback().Query().Before("gorm:query").Register(name, func(tx *gorm.DB) {
		if tx.Statement.Table == "settings" {
			tx.AddError(errors.New("injected: settings table is briefly unavailable"))
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.DB().Callback().Query().Remove(name) })
}

// The batch entry goes through the same gate: an in-process request without a
// key is refused while the gateway requires one.
func TestBatchEntryRequiresAKeyByDefault(t *testing.T) {
	rig := newKeyScopeRig(t)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"alpha","messages":[]}`))
	req.Header.Set("Content-Type", "application/json")
	if _, ok := rig.p.Identify(req); ok {
		t.Fatal("Identify accepted a request with no key")
	}
	live, err := rig.st.CreateAPIKey("live", 0)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+live)
	if _, ok := rig.p.Identify(req); !ok {
		t.Fatal("Identify refused a valid key")
	}
}
