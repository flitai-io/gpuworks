package api

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// A transient SELECT failure must not look like a real restrictive policy.
// Writes remain available, reproducing the read-fails/write-succeeds path;
// the policy is checked through HTTP again after the one failed read.
func TestSecuritySettingsReadFailureDoesNotReplacePolicy(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodPatch} {
		t.Run(method, func(t *testing.T) {
			st, err := store.Open("file:"+t.Name()+"?mode=memory&cache=shared", bus.New())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = st.Close() })
			signer, admin := roleTestToken(t, st, "admin")
			router := Router(Deps{Store: st, AuthSigner: signer})
			call := func(method, body string) *httptest.ResponseRecorder {
				req := httptest.NewRequest(method, "/api/v1/security/settings", strings.NewReader(body))
				req.Header.Set("Authorization", "Bearer "+admin)
				w := httptest.NewRecorder()
				router.ServeHTTP(w, req)
				return w
			}
			const original = `{"playground_preview_network":true,"inference_open_access":true}`
			if w := call(http.MethodPatch, original); w.Code != http.StatusOK {
				t.Fatalf("initial policy: %d %s", w.Code, w.Body)
			}
			failedReads := 0
			const hook = "test:transient-security-read"
			if err := st.DB().Callback().Query().Before("gorm:query").Register(hook, func(tx *gorm.DB) {
				if tx.Statement.Table == "settings" && failedReads == 0 {
					failedReads++
					tx.AddError(errors.New("private storage read failure details"))
				}
			}); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = st.DB().Callback().Query().Remove(hook) })
			w := call(method, `{"playground_preview_network":false}`)
			if w.Code != http.StatusInternalServerError || strings.TrimSpace(w.Body.String()) != `{"code":"SECURITY_SETTINGS_UNAVAILABLE","error":"security settings could not be read; retry or inspect the server log"}` {
				t.Errorf("failed read must be an unavailable response, not a policy: %d %s", w.Code, w.Body)
			}
			if failedReads != 1 {
				t.Fatalf("fixture failed %d reads, want exactly one", failedReads)
			}
			w = call(http.MethodGet, "")
			if w.Code != http.StatusOK || strings.TrimSpace(w.Body.String()) != original {
				t.Errorf("a failed policy read changed the saved switches: %d %s", w.Code, w.Body)
			}
		})
	}
}
