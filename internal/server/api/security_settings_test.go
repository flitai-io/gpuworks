package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// The security settings wire: any signed-in user reads them, only an admin
// writes them, a missing row is the restrictive policy, a patch changes only
// the fields it names, and the playground catalog carries the HTML preview
// policy derived from them.
func TestSecuritySettingsAPI(t *testing.T) {
	st, err := store.Open("file:"+t.Name()+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	signer, member := roleTestToken(t, st, "member")
	_, admin := roleTestToken(t, st, "admin")
	router := Router(Deps{Store: st, AuthSigner: signer})
	call := func(token, method, path, body string) (int, string) {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec.Code, strings.TrimSpace(rec.Body.String())
	}
	// Both switches, in the DTO's field order, with open access off.
	const off, on = `{"playground_preview_network":false,"inference_open_access":false}`,
		`{"playground_preview_network":true,"inference_open_access":false}`
	const openOn = `{"playground_preview_network":true,"inference_open_access":true}`
	catalogPolicy := func() string {
		t.Helper()
		code, body := call(member, http.MethodGet, "/api/v1/playground/models", "")
		if code != http.StatusOK {
			t.Fatalf("catalog = %d %s", code, body)
		}
		start := strings.Index(body, `"html_preview":`)
		if start < 0 {
			t.Fatalf("catalog has no html_preview: %s", body)
		}
		return body[start : start+strings.Index(body[start:], "}")+1]
	}

	if code, _ := call("", http.MethodGet, "/api/v1/security/settings", ""); code != http.StatusUnauthorized {
		t.Fatalf("anonymous read = %d, want 401", code)
	}
	if code, body := call(member, http.MethodGet, "/api/v1/security/settings", ""); code != http.StatusOK || body != off {
		t.Fatalf("default read = %d %s, want %s", code, body, off)
	}
	if got := catalogPolicy(); got != `"html_preview":{"allow_network":false}` {
		t.Fatalf("default catalog policy = %s", got)
	}
	if code, _ := call("", http.MethodPatch, "/api/v1/security/settings", on); code != http.StatusUnauthorized {
		t.Fatalf("anonymous write = %d, want 401", code)
	}
	if code, body := call(member, http.MethodPatch, "/api/v1/security/settings", on); code != http.StatusForbidden || !strings.Contains(body, ErrorAdministratorRequired) {
		t.Fatalf("member write = %d %s, want 403", code, body)
	}
	if st.SecurityPolicy().PlaygroundPreviewNetwork {
		t.Fatal("a refused write changed the policy")
	}
	for _, bad := range []string{`{"playground_preview_network":"yes"}`, `not json`} {
		if code, _ := call(admin, http.MethodPatch, "/api/v1/security/settings", bad); code != http.StatusBadRequest {
			t.Fatalf("invalid body %s = %d, want 400", bad, code)
		}
	}
	if code, body := call(admin, http.MethodPatch, "/api/v1/security/settings", on); code != http.StatusOK || body != on {
		t.Fatalf("admin enable = %d %s, want %s", code, body, on)
	}
	if code, body := call(member, http.MethodGet, "/api/v1/security/settings", ""); code != http.StatusOK || body != on {
		t.Fatalf("read after enable = %d %s", code, body)
	}
	// An empty patch names no field and keeps the current value.
	if code, body := call(admin, http.MethodPatch, "/api/v1/security/settings", `{}`); code != http.StatusOK || body != on {
		t.Fatalf("empty patch = %d %s, want %s", code, body, on)
	}
	// A patch naming only open access leaves the preview switch alone, and the
	// reverse: the two switches do not overwrite each other.
	if code, body := call(admin, http.MethodPatch, "/api/v1/security/settings", `{"inference_open_access":true}`); code != http.StatusOK || body != openOn {
		t.Fatalf("open-access-only patch = %d %s, want %s", code, body, openOn)
	}
	if got := catalogPolicy(); got != `"html_preview":{"allow_network":true}` {
		t.Fatalf("an open-access patch changed the preview policy: %s", got)
	}
	if code, body := call(admin, http.MethodPatch, "/api/v1/security/settings", `{"playground_preview_network":false}`); code != http.StatusOK || body != `{"playground_preview_network":false,"inference_open_access":true}` {
		t.Fatalf("preview-only patch = %d %s", code, body)
	}
	if got := catalogPolicy(); got != `"html_preview":{"allow_network":false}` {
		t.Fatalf("preview-only patch did not apply: %s", got)
	}
	// Back to the state the rest of the test expects.
	if code, body := call(admin, http.MethodPatch, "/api/v1/security/settings", `{"inference_open_access":false,"playground_preview_network":true}`); code != http.StatusOK || body != on {
		t.Fatalf("restore = %d %s, want %s", code, body, on)
	}
	if got := catalogPolicy(); got != `"html_preview":{"allow_network":true}` {
		t.Fatalf("catalog policy after enable = %s", got)
	}
	if code, body := call(member, http.MethodGet, "/api/v1/settings", ""); code != http.StatusOK || !strings.Contains(body, `"security":`+on) {
		t.Fatalf("settings listing = %d %s", code, body)
	}
	if code, body := call(admin, http.MethodPatch, "/api/v1/security/settings", off); code != http.StatusOK || body != off {
		t.Fatalf("admin disable = %d %s", code, body)
	}
	// The generic settings route cannot replace the whole policy: that would
	// clear switches the body does not name, open access among them.
	if code, body := call(admin, http.MethodPut, "/api/v1/settings/security", `{"playground_preview_network":true}`); code != http.StatusBadRequest || !strings.Contains(body, ErrorSettingHasOwnRoute) {
		t.Fatalf("generic security write = %d %s, want 400 %s", code, body, ErrorSettingHasOwnRoute)
	}
	if st.SecurityPolicy() != (store.SecurityPolicy{}) {
		t.Fatalf("a refused generic write changed the policy: %+v", st.SecurityPolicy())
	}
}
