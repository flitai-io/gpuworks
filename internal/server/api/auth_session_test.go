package api

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func newSessionRouter(t *testing.T) (*store.Store, *auth.TokenSigner, http.Handler) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "session.db"), bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, _, _, err := st.EnsureAdmin("admin", "gpuworks-test-password-2026"); err != nil {
		t.Fatal(err)
	}
	signer, err := auth.NewTokenSigner("session-test-secret", 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return st, signer, Router(Deps{Store: st, Bus: st.Bus, AuthSigner: signer})
}

func sessionLogin(t *testing.T, router http.Handler, password string) (*http.Cookie, string) {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"username": "admin", "password": password})
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(string(body))))
	if rec.Code != http.StatusOK {
		t.Fatalf("login status=%d body=%s", rec.Code, rec.Body.String())
	}
	var payload struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	for _, cookie := range rec.Result().Cookies() {
		if cookie.Name == managementCookie {
			return cookie, payload.Token
		}
	}
	t.Fatal("login did not set session cookie")
	return nil, ""
}

func sessionRequest(router http.Handler, method, path string, cookie *http.Cookie, bearer string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	if cookie != nil {
		req.AddCookie(cookie)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func TestPasswordResetInvalidatesCapturedManagementCredentials(t *testing.T) {
	st, _, router := newSessionRouter(t)
	cookie, token := sessionLogin(t, router, "gpuworks-test-password-2026")
	otherCookie, otherToken := sessionLogin(t, router, "gpuworks-test-password-2026")
	if err := st.SetUserPassword("admin", "gpuworks-new-test-password-2026"); err != nil {
		t.Fatal(err)
	}
	for _, captured := range []struct {
		cookie *http.Cookie
		bearer string
	}{
		{cookie, ""}, {nil, token}, {otherCookie, ""}, {nil, otherToken},
	} {
		rec := sessionRequest(router, http.MethodGet, "/api/v1/auth/me", captured.cookie, captured.bearer)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("old credential survived password reset: status=%d", rec.Code)
		}
	}
	fresh, _ := sessionLogin(t, router, "gpuworks-new-test-password-2026")
	if rec := sessionRequest(router, http.MethodGet, "/api/v1/auth/me", fresh, ""); rec.Code != http.StatusOK {
		t.Fatalf("new login rejected: %d", rec.Code)
	}
}

func TestLogoutRevokesOnlyCurrentManagementSession(t *testing.T) {
	_, _, router := newSessionRouter(t)
	cookie, token := sessionLogin(t, router, "gpuworks-test-password-2026")
	other, _ := sessionLogin(t, router, "gpuworks-test-password-2026")
	if rec := sessionRequest(router, http.MethodPost, "/api/v1/auth/logout", cookie, ""); rec.Code != http.StatusOK {
		t.Fatal(rec.Body.String())
	}
	for _, captured := range []struct {
		cookie *http.Cookie
		bearer string
	}{{cookie, ""}, {nil, token}} {
		if rec := sessionRequest(router, http.MethodGet, "/api/v1/auth/me", captured.cookie, captured.bearer); rec.Code != http.StatusUnauthorized {
			t.Errorf("captured credential survived logout: %d", rec.Code)
		}
	}
	if rec := sessionRequest(router, http.MethodGet, "/api/v1/auth/me", other, ""); rec.Code != http.StatusOK {
		t.Fatalf("other session revoked: %d", rec.Code)
	}
}

func TestBrowserCookieIsOpaqueAndUnboundLegacyJWTIsRejected(t *testing.T) {
	_, signer, router := newSessionRouter(t)
	cookie, _ := sessionLogin(t, router, "gpuworks-test-password-2026")
	if strings.Contains(cookie.Value, ".") || len(cookie.Value) != 64 || !cookie.HttpOnly {
		t.Errorf("browser cookie must contain only an opaque session credential")
	}
	legacy, err := signer.Sign(auth.Claims{UserID: 1, Username: "admin", Role: "admin"})
	if err != nil {
		t.Fatal(err)
	}
	if rec := sessionRequest(router, http.MethodGet, "/api/v1/auth/me", nil, legacy); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unbound JWT accepted: %d", rec.Code)
	}
}

func TestActivityRenewsIdleDeadlineWhilePollingDoesNot(t *testing.T) {
	st, _, router := newSessionRouter(t)
	cookie, _ := sessionLogin(t, router, "gpuworks-test-password-2026")
	hash := store.HashToken(cookie.Value)
	nearExpiry := time.Now().UTC().Add(10 * time.Minute)
	if err := st.DB().Model(&store.ManagementSession{}).Where("token_hash = ?", hash).Update("expires_at", nearExpiry).Error; err != nil {
		t.Fatal(err)
	}
	_, before, err := st.ManagementSessionByHash(hash)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if rec := sessionRequest(router, http.MethodGet, "/api/v1/auth/me", cookie, ""); rec.Code != http.StatusOK {
			t.Fatalf("poll: %d", rec.Code)
		}
	}
	_, polled, err := st.ManagementSessionByHash(hash)
	if err != nil {
		t.Fatal(err)
	}
	if !polled.ExpiresAt.Equal(before.ExpiresAt) {
		t.Fatal("passive polling extended session")
	}
	rec := sessionRequest(router, http.MethodPost, "/api/v1/auth/activity", cookie, "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("activity: %d %s", rec.Code, rec.Body.String())
	}
	_, renewed, err := st.ManagementSessionByHash(hash)
	if err != nil {
		t.Fatal(err)
	}
	if !renewed.ExpiresAt.After(before.ExpiresAt) || !renewed.AbsoluteExpiresAt.Equal(before.AbsoluteExpiresAt) {
		t.Fatal("activity must extend only the idle deadline")
	}
	if len(rec.Result().Cookies()) != 1 || rec.Result().Cookies()[0].Value != cookie.Value {
		t.Fatal("renewal must preserve opaque cookie credential")
	}
	if err := st.DB().Model(&store.ManagementSession{}).Where("token_hash = ?", hash).Update("expires_at", time.Now().UTC().Add(-time.Second)).Error; err != nil {
		t.Fatal(err)
	}
	if rec := sessionRequest(router, http.MethodPost, "/api/v1/auth/activity", cookie, ""); rec.Code != http.StatusUnauthorized {
		t.Fatal("activity revived expired session")
	}
}

func TestManagementSessionUsesCurrentRoleAndReportsStoreFailure(t *testing.T) {
	st, _, router := newSessionRouter(t)
	cookie, token := sessionLogin(t, router, "gpuworks-test-password-2026")
	if err := st.DB().Model(&store.User{}).Where("username = ?", "admin").Update("role", "user").Error; err != nil {
		t.Fatal(err)
	}
	if rec := sessionRequest(router, http.MethodGet, "/api/v1/terminal/sessions", nil, token); rec.Code != http.StatusForbidden {
		t.Fatalf("JWT retained stale admin privilege: %d", rec.Code)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	if rec := sessionRequest(router, http.MethodGet, "/api/v1/auth/me", cookie, ""); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("store failure must not look like expiry: %d", rec.Code)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(`{"username":"admin","password":"gpuworks-test-password-2026"}`))
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("login store failure must not look like bad password: %d", rec.Code)
	}
}

func TestLogoutRevokesCookieEvenWithConflictingBearer(t *testing.T) {
	for _, validBearer := range []bool{false, true} {
		t.Run(map[bool]string{false: "invalid-bearer", true: "another-session-bearer"}[validBearer], func(t *testing.T) {
			_, _, router := newSessionRouter(t)
			cookie, _ := sessionLogin(t, router, "gpuworks-test-password-2026")
			other, token := sessionLogin(t, router, "gpuworks-test-password-2026")
			bearer := "invalid-token"
			if validBearer {
				bearer = token
			}
			if rec := sessionRequest(router, http.MethodPost, "/api/v1/auth/logout", cookie, bearer); rec.Code != http.StatusOK {
				t.Fatal(rec.Body.String())
			}
			if rec := sessionRequest(router, http.MethodGet, "/api/v1/auth/me", cookie, ""); rec.Code != http.StatusUnauthorized {
				t.Fatal("cookie survived logout behind Bearer header")
			}
			want := http.StatusOK
			if validBearer {
				want = http.StatusUnauthorized
			}
			if rec := sessionRequest(router, http.MethodGet, "/api/v1/auth/me", other, ""); rec.Code != want {
				t.Fatalf("other presented credential status=%d want=%d", rec.Code, want)
			}
		})
	}
}

func TestManagementActivityOriginAndSecureCookie(t *testing.T) {
	st, signer, router := newSessionRouter(t)
	cookie, _ := sessionLogin(t, router, "gpuworks-test-password-2026")
	for _, path := range []string{"/api/v1/auth/login", "/api/v1/auth/logout", "/api/v1/auth/activity"} {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"username":"admin","password":"gpuworks-test-password-2026"}`))
		req.AddCookie(cookie)
		req.Header.Set("Origin", "https://attacker.example")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("cross-origin %s: %d", path, rec.Code)
		}
	}
	trusted := Router(Deps{Store: st, AuthSigner: signer}, auth.SessionPolicy{Proxies: listedProxies("127.0.0.1")})
	req := httptest.NewRequest(http.MethodPost, "http://console.example/api/v1/auth/login", strings.NewReader(`{"username":"admin","password":"gpuworks-test-password-2026"}`))
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Header.Set("Origin", "https://console.example")
	rec := httptest.NewRecorder()
	trusted.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || len(rec.Result().Cookies()) != 1 || !rec.Result().Cookies()[0].Secure {
		t.Fatalf("trusted HTTPS proxy must set Secure cookie: %d %v", rec.Code, rec.Result().Cookies())
	}
}

func TestManagementSessionWireContractAndPolicy(t *testing.T) {
	st, signer, _ := newSessionRouter(t)
	router := Router(Deps{Store: st, AuthSigner: signer}, auth.SessionPolicy{IdleTimeout: time.Hour, MaxLifetime: 48 * time.Hour})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(`{"username":"admin","password":"gpuworks-test-password-2026"}`))
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatal(rec.Body.String())
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload) != 2 || payload["token"] == nil || payload["user"] == nil {
		t.Fatalf("unexpected login wire keys: %s", rec.Body.String())
	}
	var expected, actual map[string]any
	if err := json.Unmarshal([]byte(`{"id":1,"username":"admin","role":"admin"}`), &expected); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(payload["user"], &actual); err != nil || !reflect.DeepEqual(actual, expected) {
		t.Fatalf("login user wire=%s", payload["user"])
	}
	var bearer string
	if err := json.Unmarshal(payload["token"], &bearer); err != nil {
		t.Fatal(err)
	}
	cookie := rec.Result().Cookies()[0]
	claims, err := signer.Verify(bearer)
	if err != nil || claims.SessionID != store.HashToken(cookie.Value) || claims.UserID != 1 {
		t.Fatal("Bearer must bind to cookie session and current user")
	}
	_, session, err := st.ManagementSessionByHash(claims.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if session.ExpiresAt.Sub(session.CreatedAt) != time.Hour || session.AbsoluteExpiresAt.Sub(session.CreatedAt) != 48*time.Hour {
		t.Fatal("configured session policy ignored")
	}
	if cookie.Path != "/" || !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode || cookie.MaxAge < int((47*time.Hour).Seconds()) || cookie.Expires.Unix() != session.AbsoluteExpiresAt.Unix() {
		t.Fatalf("cookie flags or lifetime mismatch: %s", cookie.String())
	}
	me := sessionRequest(router, http.MethodGet, "/api/v1/auth/me", cookie, "")
	if me.Code != http.StatusOK {
		t.Fatal(me.Body.String())
	}
	actual, expected = nil, nil
	if err := json.Unmarshal([]byte(`{"id":1,"username":"admin","role":"admin","password_policy":{"min_length":8,"max_length":128}}`), &expected); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(me.Body.Bytes(), &actual); err != nil || !reflect.DeepEqual(actual, expected) {
		t.Fatalf("me wire=%s", me.Body.String())
	}
	if rec.Header().Get("Cache-Control") != "no-store" || me.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("identity responses must not be cached")
	}
	if got := sessionRequest(router, http.MethodPost, "/api/v1/auth/activity", nil, ""); got.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated activity=%d", got.Code)
	}
	if got := sessionRequest(router, http.MethodGet, "/api/v1/auth/activity", cookie, ""); got.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET activity=%d", got.Code)
	}
}

// The headers below are what nginx sends for "proxy_set_header Host $host;
// X-Forwarded-Host $host; X-Forwarded-Port $server_port" behind a non-default
// listen port: $host has no port, so only X-Forwarded-Port can restore it.
func TestManagementOriginRestoresPortDroppedByProxy(t *testing.T) {
	st, signer, _ := newSessionRouter(t)
	router := Router(Deps{Store: st, AuthSigner: signer}, auth.SessionPolicy{Proxies: listedProxies("10.0.0.0/8")})
	for _, tc := range []struct {
		name, remote, host, forwardedHost, forwardedProto, forwardedPort, origin string
		want                                                                     int
	}{
		{"nginx host without port", "10.1.2.3:4000", "203.0.113.5", "203.0.113.5", "http", "8808", "http://203.0.113.5:8808", http.StatusOK},
		{"no forwarded host, bare Host", "10.1.2.3:4000", "203.0.113.5", "", "http", "8808", "http://203.0.113.5:8808", http.StatusOK},
		{"default port stays implicit", "10.1.2.3:4000", "console.example", "console.example", "https", "443", "https://console.example", http.StatusOK},
		{"default port is not added to Origin", "10.1.2.3:4000", "console.example", "console.example", "https", "443", "https://console.example:443", http.StatusForbidden},
		{"forwarded host keeps its own port", "10.1.2.3:4000", "internal:8800", "console.example:9443", "https", "8808", "https://console.example:9443", http.StatusOK},
		{"IPv6 host", "10.1.2.3:4000", "[2001:db8::1]", "[2001:db8::1]", "http", "8808", "http://[2001:db8::1]:8808", http.StatusOK},
		{"invalid port is ignored", "10.1.2.3:4000", "203.0.113.5", "203.0.113.5", "http", "eighty", "http://203.0.113.5:8808", http.StatusForbidden},
		{"untrusted peer cannot assert a port", "198.51.100.7:4000", "203.0.113.5", "203.0.113.5", "http", "8808", "http://203.0.113.5:8808", http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(`{"username":"admin","password":"gpuworks-test-password-2026"}`))
			req.Host = tc.host
			req.RemoteAddr = tc.remote
			if tc.forwardedHost != "" {
				req.Header.Set("X-Forwarded-Host", tc.forwardedHost)
			}
			req.Header.Set("X-Forwarded-Proto", tc.forwardedProto)
			req.Header.Set("X-Forwarded-Port", tc.forwardedPort)
			req.Header.Set("Origin", tc.origin)
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("status = %d want %d body=%s", rec.Code, tc.want, rec.Body.String())
			}
			if tc.want == http.StatusForbidden && !strings.Contains(rec.Body.String(), `"code":"MANAGEMENT_ORIGIN_REJECTED"`) {
				t.Fatalf("body = %s", rec.Body.String())
			}
		})
	}
}

func listedProxies(list string) *auth.ProxyTrust {
	p, _ := auth.NewProxyTrust(list, nil)
	return p
}

// The public entry point's connector runs on the server's host, pointed at
// whichever local address the operator chose, and forwards the browser's https
// origin: while the entry point is enabled the login through it must pass the
// Origin check without anyone listing the connector as a proxy.
func TestManagementOriginTrustsEntryPointConnectorOnThisHost(t *testing.T) {
	st, signer, _ := newSessionRouter(t)
	const site = "dos-seven-making-trials.trycloudflare.com"
	for _, tc := range []struct {
		name, peer, local string
		enabled           bool
		want              int
	}{
		{"loopback origin", "[::1]:51000", "[::1]:8816", true, http.StatusOK},
		{"host address origin", "192.0.2.156:51000", "192.0.2.156:8816", true, http.StatusOK},
		{"IPv4 on a dual-stack socket", "192.0.2.156:51000", "[::ffff:192.0.2.156]:8816", true, http.StatusOK},
		{"entry point disabled", "[::1]:51000", "[::1]:8816", false, http.StatusForbidden},
		{"another host", "192.0.2.200:51000", "192.0.2.156:8816", true, http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			enabled := tc.enabled
			proxies, _ := auth.NewProxyTrust("", func() bool { return enabled })
			router := Router(Deps{Store: st, AuthSigner: signer}, auth.SessionPolicy{Proxies: proxies})
			req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(`{"username":"admin","password":"gpuworks-test-password-2026"}`))
			req.Host = site
			req.RemoteAddr = tc.peer
			local, err := net.ResolveTCPAddr("tcp", tc.local)
			if err != nil {
				t.Fatal(err)
			}
			req = req.WithContext(context.WithValue(req.Context(), http.LocalAddrContextKey, local))
			// What cloudflared sends: the browser's Host, no X-Forwarded-Host.
			req.Header.Set("X-Forwarded-Proto", "https")
			req.Header.Set("X-Forwarded-For", "203.0.113.9")
			req.Header.Set("Origin", "https://"+site)
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("status = %d want %d body=%s", rec.Code, tc.want, rec.Body.String())
			}
			if tc.want == http.StatusOK && (len(rec.Result().Cookies()) != 1 || !rec.Result().Cookies()[0].Secure) {
				t.Fatalf("login through the entry point must set a Secure cookie: %v", rec.Result().Cookies())
			}
			if tc.want == http.StatusForbidden && !strings.Contains(rec.Body.String(), `"code":"MANAGEMENT_ORIGIN_REJECTED"`) {
				t.Fatalf("body = %s", rec.Body.String())
			}
		})
	}
}
