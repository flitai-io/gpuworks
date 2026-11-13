package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// tokenRig is a router over a real store with an admin, a login session for
// them, and a helper that sends a request with any bearer value.
type tokenRig struct {
	t       *testing.T
	h       http.Handler
	st      *store.Store
	session string
}

func newTokenRig(t *testing.T) *tokenRig {
	t.Helper()
	st, err := store.Open("file:"+t.Name()+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := st.EnsureAdmin("admin", "gpuworks-test-password-2026"); err != nil {
		t.Fatal(err)
	}
	admin, ok := st.VerifyUser("admin", "gpuworks-test-password-2026")
	if !ok {
		t.Fatal("no admin")
	}
	signer, err := auth.NewTokenSigner("platform-token-test-secret", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	session, err := managementSessionTestToken(t, st, signer, auth.Claims{UserID: admin.ID, Username: admin.Username, Role: admin.Role})
	if err != nil {
		t.Fatal(err)
	}
	return &tokenRig{t: t, h: Router(Deps{AuthSigner: signer, Store: st, AI: &fakeAI{available: true}, SchedulingActivity: replayActivity{st}}), st: st, session: session}
}

func (r *tokenRig) do(bearer, method, path, body string) *httptest.ResponseRecorder {
	r.t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	r.h.ServeHTTP(rec, req)
	return rec
}

func (r *tokenRig) mint(scope string) string {
	r.t.Helper()
	rec := r.do(r.session, "POST", "/api/v1/platform-tokens", `{"name":"t-`+scope+`","scope":"`+scope+`"}`)
	if rec.Code != http.StatusCreated {
		r.t.Fatalf("mint %s: %d %s", scope, rec.Code, rec.Body)
	}
	var out struct {
		Token string `json:"token"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return out.Token
}

func TestPlatformTokenCreateWireAndValidation(t *testing.T) {
	rig := newTokenRig(t)
	rec := rig.do(rig.session, "POST", "/api/v1/platform-tokens", `{"name":" ci "}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	var created map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	token, _ := created["token"].(string)
	item, _ := created["item"].(map[string]any)
	if len(created) != 2 || !strings.HasPrefix(token, "gwp_") || item == nil {
		t.Fatalf("created = %s", rec.Body)
	}
	if item["name"] != "ci" || item["scope"] != "read" || item["prefix"] != token[:12] || item["expires_at"] != nil || item["last_used_at"] != nil || item["created_at"] == nil || item["id"] == nil || len(item) != 7 {
		t.Fatalf("item = %v", item)
	}
	rec = rig.do(rig.session, "POST", "/api/v1/platform-tokens", `{"name":"week","scope":"write","expires_in_days":7}`)
	var week struct {
		Item struct {
			ExpiresAt time.Time `json:"expires_at"`
		} `json:"item"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &week)
	// The handler counts the days in UTC, and so must this: a local AddDate
	// over a daylight-saving change lands an hour away from the UTC one, which
	// made the test fail on a host in such a zone (America/Chicago, a week
	// before 11-01) while the token was correct.
	if rec.Code != http.StatusCreated || week.Item.ExpiresAt.Sub(time.Now().UTC().AddDate(0, 0, 7)).Abs() > time.Minute {
		t.Fatalf("expiring token: %d %s", rec.Code, rec.Body)
	}

	list := rig.do(rig.session, "GET", "/api/v1/platform-tokens", "")
	var page struct {
		Items []map[string]any `json:"items"`
		Total int              `json:"total"`
	}
	if err := json.Unmarshal(list.Body.Bytes(), &page); err != nil || page.Total != 2 || page.Items[0]["name"] != "week" || strings.Contains(list.Body.String(), token) {
		t.Fatalf("list = %s", list.Body)
	}

	for body, want := range map[string]string{
		`{"name":"x","scope":"admin"}`:          `{"code":"PLATFORM_TOKEN_SCOPE_INVALID","error":"scope must be read or write","params":{"scope":"admin"}}`,
		`{"name":"  "}`:                         `"code":"PLATFORM_TOKEN_NAME_INVALID"`,
		`{"name":"x","expires_in_days":-1}`:     `"code":"PLATFORM_TOKEN_EXPIRY_INVALID"`,
		`{"name":"x","expires_in_days":100000}`: `"code":"PLATFORM_TOKEN_EXPIRY_INVALID"`,
		`not json`:                              `"code":"FIELD_INVALID"`,
	} {
		rec := rig.do(rig.session, "POST", "/api/v1/platform-tokens", body)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), want) {
			t.Errorf("%s: %d %s", body, rec.Code, rec.Body)
		}
	}
	if rec := rig.do("", "GET", "/api/v1/platform-tokens", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous list: %d", rec.Code)
	}
}

func TestPlatformTokenNameUTF8ByteBoundary(t *testing.T) {
	rig := newTokenRig(t)
	for _, tc := range []struct {
		name   string
		status int
	}{
		{strings.Repeat("a", 128), http.StatusCreated},
		{strings.Repeat("a", 129), http.StatusBadRequest},
		{strings.Repeat("中", 42) + "ab", http.StatusCreated},
		{strings.Repeat("中", 43), http.StatusBadRequest},
	} {
		body, err := json.Marshal(map[string]string{"name": " " + tc.name + " "})
		if err != nil {
			t.Fatal(err)
		}
		rec := rig.do(rig.session, "POST", "/api/v1/platform-tokens", string(body))
		if rec.Code != tc.status {
			t.Fatalf("%d bytes: status %d, want %d", len(tc.name), rec.Code, tc.status)
		}
		if tc.status == http.StatusBadRequest {
			if !strings.Contains(rec.Body.String(), `"code":"PLATFORM_TOKEN_NAME_INVALID"`) || !strings.Contains(rec.Body.String(), "UTF-8 bytes") {
				t.Fatalf("byte-limit error must identify its unit: %s", rec.Body)
			}
		} else {
			var result struct {
				Item struct {
					Name string `json:"name"`
				} `json:"item"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if result.Item.Name != tc.name {
				t.Fatal("name trim changed the boundary value")
			}
		}
	}
}

// A read token reads; a write token does what its user may; neither manages
// tokens; a revoked or made-up token is no one.
func TestPlatformTokenScopes(t *testing.T) {
	rig := newTokenRig(t)
	read, write := rig.mint("read"), rig.mint("write")

	if rec := rig.do(read, "GET", "/api/v1/nodes", ""); rec.Code != http.StatusOK {
		t.Fatalf("read token GET: %d %s", rec.Code, rec.Body)
	}
	if rec := rig.do(read, "GET", "/api/v1/auth/me", ""); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"username":"admin"`) {
		t.Fatalf("read token /auth/me: %d %s", rec.Code, rec.Body)
	}
	rec := rig.do(read, "POST", "/api/v1/apikeys", `{"name":"x"}`)
	if rec.Code != http.StatusForbidden || strings.TrimSpace(rec.Body.String()) != `{"code":"PLATFORM_TOKEN_READ_ONLY","error":"a read token can only read","params":{"scope":"read"}}` {
		t.Fatalf("read token POST: %d %s", rec.Code, rec.Body)
	}
	if rec := rig.do(read, "PUT", "/api/v1/mcp/settings", `{"enabled":true}`); rec.Code != http.StatusForbidden {
		t.Fatalf("read token PUT: %d", rec.Code)
	}
	if rec := rig.do(write, "POST", "/api/v1/apikeys", `{"name":"x"}`); rec.Code != http.StatusCreated {
		t.Fatalf("write token POST: %d %s", rec.Code, rec.Body)
	}
	// The admin route takes the token's user's role.
	if rec := rig.do(write, "PUT", "/api/v1/mcp/settings", `{"enabled":true}`); rec.Code != http.StatusOK {
		t.Fatalf("write token admin route: %d %s", rec.Code, rec.Body)
	}
	for _, tok := range []string{read, write} {
		if rec := rig.do(tok, "POST", "/api/v1/auth/activity", ""); rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), `"code":"PLATFORM_TOKEN_NOT_ALLOWED"`) {
			t.Fatalf("platform token cannot renew a login session: %d %s", rec.Code, rec.Body)
		}
		if rec := rig.do(tok, "POST", "/api/v1/auth/password", `{"current_password":"x","new_password":"Tiger#Lake9"}`); rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), `"code":"PLATFORM_TOKEN_NOT_ALLOWED"`) {
			t.Fatalf("platform token cannot change the password: %d %s", rec.Code, rec.Body)
		}
		for _, m := range []string{"GET", "POST"} {
			rec := rig.do(tok, m, "/api/v1/platform-tokens", `{"name":"escalate","scope":"write"}`)
			if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), `"code":"PLATFORM_TOKEN_NOT_ALLOWED"`) {
				t.Fatalf("token %s /platform-tokens: %d %s", m, rec.Code, rec.Body)
			}
		}
		if rec := rig.do(tok, "DELETE", "/api/v1/platform-tokens/1", ""); rec.Code != http.StatusForbidden {
			t.Fatalf("token revoke: %d", rec.Code)
		}
	}
	if rec := rig.do("gwp_"+strings.Repeat("A", 43), "GET", "/api/v1/nodes", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("made-up token: %d", rec.Code)
	}

	var page struct {
		Items []struct {
			ID         uint       `json:"id"`
			Scope      string     `json:"scope"`
			LastUsedAt *time.Time `json:"last_used_at"`
		} `json:"items"`
	}
	_ = json.Unmarshal(rig.do(rig.session, "GET", "/api/v1/platform-tokens", "").Body.Bytes(), &page)
	var readID uint
	for _, it := range page.Items {
		if it.LastUsedAt == nil {
			t.Fatalf("a used token has no last_used_at: %+v", it)
		}
		if it.Scope == "read" {
			readID = it.ID
		}
	}
	if rec := rig.do(rig.session, "DELETE", "/api/v1/platform-tokens/"+jsonUint(readID), ""); rec.Code != http.StatusNoContent {
		t.Fatalf("revoke: %d %s", rec.Code, rec.Body)
	}
	if rec := rig.do(read, "GET", "/api/v1/nodes", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("revoked token: %d", rec.Code)
	}
	if rec := rig.do(rig.session, "DELETE", "/api/v1/platform-tokens/"+jsonUint(readID), ""); rec.Code != http.StatusNotFound {
		t.Fatalf("revoke twice: %d", rec.Code)
	}
	if rec := rig.do(rig.session, "DELETE", "/api/v1/platform-tokens/abc", ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("revoke bad id: %d", rec.Code)
	}
}
