package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/distribution"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func routerAuthStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open("file:router_auth_test?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func TestManagementRoutesRequireJWT(t *testing.T) {
	signer, err := auth.NewTokenSigner("router-auth-test-secret", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	r := Router(Deps{AuthSigner: signer, Store: routerAuthStore(t), PlaygroundV1: http.NotFoundHandler()})

	for _, path := range []string{"/api/v1/healthz", "/api/v1/engines", "/api/v1/accelerator-vendors", "/api/v1/nodes", "/api/v1/nodes/facets", "/api/v1/dashboard/summary", "/api/v1/inference-templates", "/api/v1/playground/models", "/api/v1/playground/openapi.json", "/api/v1/playground/v1/chat/completions"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("unauthenticated %s status = %d, want %d", path, rec.Code, http.StatusUnauthorized)
		}
	}

	public := httptest.NewRecorder()
	r.ServeHTTP(public, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if public.Code != http.StatusOK {
		t.Fatalf("public /healthz status = %d, want %d", public.Code, http.StatusOK)
	}
}

func TestManagementHealthAndEnginesAcceptJWT(t *testing.T) {
	signer, err := auth.NewTokenSigner("router-auth-success-secret", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	st := routerAuthStore(t)
	token, err := managementSessionTestToken(t, st, signer, auth.Claims{UserID: 1, Username: "admin", Role: "admin"})
	if err != nil {
		t.Fatal(err)
	}
	r := Router(Deps{AuthSigner: signer, Store: st})

	for _, path := range []string{"/api/v1/healthz", "/api/v1/engines"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("authenticated %s status = %d body=%s", path, rec.Code, rec.Body.String())
		}
	}
}

func TestNodeRecoveryMutationRequiresAdministrator(t *testing.T) {
	signer, err := auth.NewTokenSigner("router-recovery-admin-secret", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	st := routerAuthStore(t)
	r := Router(Deps{AuthSigner: signer, Store: st})
	for _, roleStatus := range []struct {
		role string
		want int
	}{{role: "user", want: http.StatusForbidden}, {role: "admin", want: http.StatusServiceUnavailable}} {
		token, err := managementSessionTestToken(t, st, signer, auth.Claims{UserID: 1, Username: roleStatus.role, Role: roleStatus.role})
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodPost, "/api/v1/nodes/node-1/recovery/probe", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if rec.Code != roleStatus.want {
			t.Fatalf("role=%s status=%d want=%d body=%s", roleStatus.role, rec.Code, roleStatus.want, rec.Body.String())
		}
	}
}

func TestImageImportsRequireAdministrator(t *testing.T) {
	signer, err := auth.NewTokenSigner("router-image-admin-secret", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	// The admin reaches the handler; the channel it would read is replaced so
	// the test never touches the network.
	unavailable := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusServiceUnavailable, Status: "503 Service Unavailable", Body: http.NoBody, Request: req}, nil
	})}
	channel := &distribution.Client{Sources: []string{"https://dl.example"}, HTTP: unavailable}
	st := routerAuthStore(t)
	r := Router(Deps{AuthSigner: signer, Store: st, Outside: OutsideSources{Channel: channel}})
	for _, roleStatus := range []struct {
		role string
		want int
	}{{role: "user", want: http.StatusForbidden}, {role: "admin", want: http.StatusBadRequest}} {
		token, err := managementSessionTestToken(t, st, signer, auth.Claims{UserID: 1, Username: roleStatus.role, Role: roleStatus.role})
		if err != nil {
			t.Fatal(err)
		}
		paths := []struct {
			path string
			want int
		}{
			{path: "/api/v1/images/import", want: roleStatus.want},
			{path: "/api/v1/images/sync", want: func() int {
				if roleStatus.role == "user" {
					return http.StatusForbidden
				}
				return http.StatusBadGateway
			}()},
			{path: "/api/v1/images/1/nodes/node-1/inspect", want: func() int {
				if roleStatus.role == "user" {
					return http.StatusForbidden
				}
				return http.StatusServiceUnavailable
			}()},
		}
		for _, item := range paths {
			path := item.path
			req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"images":[]}`))
			req.Header.Set("Authorization", "Bearer "+token)
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req)
			if rec.Code != item.want {
				t.Errorf("role %s path %s status = %d, want %d body=%s", roleStatus.role, path, rec.Code, item.want, rec.Body.String())
			}
		}
	}
}

func TestInferenceTemplateImportRequiresAdministrator(t *testing.T) {
	signer, err := auth.NewTokenSigner("router-template-admin-secret", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	st := routerAuthStore(t)
	r := Router(Deps{AuthSigner: signer, Store: st})
	requests := []struct{ method, path, body string }{{method: http.MethodPost, path: "/api/v1/inference-templates/import", body: `{}`}}
	for _, role := range []string{"user", "admin"} {
		token, err := managementSessionTestToken(t, st, signer, auth.Claims{UserID: 1, Username: role, Role: role})
		if err != nil {
			t.Fatal(err)
		}
		for _, item := range requests {
			req := httptest.NewRequest(item.method, item.path, strings.NewReader(item.body))
			req.Header.Set("Authorization", "Bearer "+token)
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req)
			if role == "user" && rec.Code != http.StatusForbidden {
				t.Errorf("user %s %s status = %d, want 403 body=%s", item.method, item.path, rec.Code, rec.Body.String())
			}
			if role == "admin" && rec.Code == http.StatusForbidden {
				t.Errorf("admin %s %s unexpectedly forbidden", item.method, item.path)
			}
		}
	}
}

func TestRouterAuthenticationCategoryTable(t *testing.T) {
	signer, err := auth.NewTokenSigner("router-category-secret", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	stub := func(status int) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(status) }
	}
	r := Router(Deps{
		AuthSigner: signer, Store: routerAuthStore(t),
		AgentConnect: stub(http.StatusTeapot), InstallSh: stub(http.StatusTeapot),
		ProxyV1: stub(http.StatusTeapot), PlaygroundV1: stub(http.StatusTeapot),
	})
	tests := []struct {
		path string
		want int
	}{
		{path: "/healthz", want: http.StatusOK},
		{path: "/api/v1/agent/connect", want: http.StatusTeapot},
		// Both lanes of the dual-lane tunnel share the one unauthenticated agent
		// route; the lane query parameter is interpreted by the hub handler, not
		// by the router or management auth.
		{path: "/api/v1/agent/connect?lane=ctrl", want: http.StatusTeapot},
		{path: "/api/v1/agent/connect?lane=data", want: http.StatusTeapot},
		{path: "/install.sh", want: http.StatusTeapot},
		{path: "/upgrade.sh", want: http.StatusNotFound},
		{path: "/v1/models", want: http.StatusTeapot},
		{path: "/v1/chat/completions", want: http.StatusTeapot},
		{path: "/api/v1/auth/login", want: http.StatusBadRequest},
		{path: "/api/v1/auth/logout", want: http.StatusOK},
		// The sign-in page starts the update check; it answers nothing.
		{path: "/api/v1/updates/prefetch", want: http.StatusNoContent},
		{path: "/api/v1/updates", want: http.StatusUnauthorized},
		{path: "/api/v1/healthz", want: http.StatusUnauthorized},
		{path: "/api/v1/nodes", want: http.StatusUnauthorized},
		{path: "/api/v1/playground/models", want: http.StatusUnauthorized},
		{path: "/api/v1/playground/openapi.json", want: http.StatusUnauthorized},
		{path: "/api/v1/playground/v1/chat/completions", want: http.StatusUnauthorized},
		{path: "/api/v1/deployments/1/sleep", want: http.StatusUnauthorized},
		{path: "/api/v1/metrics/query", want: http.StatusUnauthorized},
	}
	for _, tc := range tests {
		t.Run(tc.path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			if tc.path == "/api/v1/auth/login" || tc.path == "/api/v1/auth/logout" || tc.path == "/api/v1/updates/prefetch" || strings.Contains(tc.path, "/sleep") {
				req.Method = http.MethodPost
			}
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("unauthenticated %s status=%d body=%s, want %d", tc.path, rec.Code, rec.Body.String(), tc.want)
			}
		})
	}
}
