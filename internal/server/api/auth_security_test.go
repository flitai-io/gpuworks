package api

import (
	"crypto/tls"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/auth"
)

func TestLoginTwentyFiveInvalidAttemptsAreThrottled(t *testing.T) {
	_, _, router := newSessionRouter(t)
	counts := map[int]int{}
	for i := 0; i < 25; i++ {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(`{"username":"fake-user","password":"wrong"}`))
		// Untrusted forwarded addresses must not rotate the attacker's identity.
		req.Header.Set("X-Forwarded-For", fmt.Sprintf("192.0.2.%d", i+1))
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		counts[rec.Code]++
		if rec.Code == http.StatusTooManyRequests {
			if seconds, err := strconv.Atoi(rec.Header().Get("Retry-After")); err != nil || seconds < 1 {
				t.Fatalf("invalid Retry-After: %q", rec.Header().Get("Retry-After"))
			}
			if strings.Contains(rec.Body.String(), "fake-user") {
				t.Fatalf("throttle leaks account: %s", rec.Body.String())
			}
		}
	}
	if counts[http.StatusUnauthorized] != 5 || counts[http.StatusTooManyRequests] != 20 {
		t.Fatalf("25 login statuses = %v, want 5 unauthorized and 20 throttled", counts)
	}
}

func TestProtectedRoutingErrorsRequireAuthentication(t *testing.T) {
	_, _, router := newSessionRouter(t)
	for _, path := range []string{"/api/v1/models/register-url", "/api/v1/does-not-exist", "/api/v1/auth/activity"} {
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusUnauthorized || rec.Header().Get("Allow") != "" {
			t.Errorf("anonymous GET %s: status=%d Allow=%q", path, rec.Code, rec.Header().Get("Allow"))
		}
	}
	cookie, _ := sessionLogin(t, router, "gpuworks-test-password-2026")
	if rec := sessionRequest(router, http.MethodGet, "/api/v1/auth/activity", cookie, ""); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("authenticated method mismatch=%d", rec.Code)
	}
	if rec := sessionRequest(router, http.MethodGet, "/api/v1/does-not-exist", cookie, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("authenticated unknown route=%d", rec.Code)
	}
}

func TestSecurityHeadersAndHTTPSCookie(t *testing.T) {
	st, signer, _ := newSessionRouter(t)
	router := Router(Deps{Store: st, AuthSigner: signer, UIHandler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ui")) })}, auth.SessionPolicy{Proxies: listedProxies("127.0.0.1")})
	for _, tc := range []struct {
		name, peer, proto string
		tls, secure       bool
	}{
		{name: "plain", peer: "198.51.100.7:4000"},
		{name: "spoofed proxy", peer: "198.51.100.7:4000", proto: "https"},
		{name: "trusted proxy", peer: "127.0.0.1:4000", proto: "https", secure: true},
		{name: "native TLS", peer: "198.51.100.7:4000", tls: true, secure: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, path := range []string{"/", "/api/v1/auth/me", "/api/v1/does-not-exist", "/api/v1/auth/login", "/api/v1/auth/logout"} {
				method := http.MethodGet
				var body *strings.Reader
				if strings.HasSuffix(path, "login") || strings.HasSuffix(path, "logout") {
					method = http.MethodPost
				}
				body = strings.NewReader(`{"username":"admin","password":"gpuworks-test-password-2026"}`)
				req := httptest.NewRequest(method, path, body)
				req.RemoteAddr = tc.peer
				req.Header.Set("X-Forwarded-Proto", tc.proto)
				if tc.tls {
					req.TLS = &tls.ConnectionState{}
				}
				rec := httptest.NewRecorder()
				router.ServeHTTP(rec, req)
				for key, want := range map[string]string{"X-Frame-Options": "DENY", "X-Content-Type-Options": "nosniff", "Referrer-Policy": "strict-origin-when-cross-origin"} {
					if rec.Header().Get(key) != want {
						t.Errorf("%s %s=%q", path, key, rec.Header().Get(key))
					}
				}
				csp := rec.Header().Get("Content-Security-Policy")
				for _, directive := range []string{"frame-ancestors 'none'", "base-uri 'self'", "object-src 'none'"} {
					if !strings.Contains(csp, directive) {
						t.Errorf("%s missing CSP %s: %q", path, directive, csp)
					}
				}
				if rec.Header().Get("Permissions-Policy") == "" {
					t.Errorf("%s lacks permissions policy", path)
				}
				if got := rec.Header().Get("Strict-Transport-Security"); (got != "") != tc.secure {
					t.Errorf("%s HSTS=%q secure=%v", path, got, tc.secure)
				}
				if strings.HasSuffix(path, "login") || strings.HasSuffix(path, "logout") {
					cookies := rec.Result().Cookies()
					if len(cookies) != 1 || cookies[0].Secure != tc.secure {
						t.Errorf("%s cookies=%v", path, cookies)
					}
				}
			}
		})
	}
}

func TestPrivilegedMutationsCannotBypassAdministratorRoutes(t *testing.T) {
	st, signer, router := newSessionRouter(t)
	for _, role := range []string{"user", "admin"} {
		token, err := managementSessionTestToken(t, st, signer, auth.Claims{Username: role, Role: role})
		if err != nil {
			t.Fatal(err)
		}
		for _, tc := range []struct {
			method, path, body string
			adminStatus        int
		}{
			// The security switches have their own route; the generic settings
			// write refuses the key outright (see security_settings_test.go).
			{http.MethodPut, "/api/v1/settings/security", `{"playground_preview_network":true}`, http.StatusBadRequest},
			{http.MethodPut, "/api/v1/settings/storage_policy", `{}`, http.StatusOK},
			{http.MethodPut, "/api/v1/settings/mcp.enabled", `true`, http.StatusOK},
			{http.MethodPost, "/api/v1/nodes/install", `{}`, http.StatusServiceUnavailable},
			{http.MethodPost, "/api/v1/nodes/precheck", `{}`, http.StatusServiceUnavailable},
		} {
			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			req.Header.Set("Authorization", "Bearer "+token)
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			want := http.StatusForbidden
			if role == "admin" {
				want = tc.adminStatus
			}
			if rec.Code != want {
				t.Errorf("role=%s %s %s status=%d want=%d body=%s", role, tc.method, tc.path, rec.Code, want, rec.Body.String())
			}
		}
	}
}
