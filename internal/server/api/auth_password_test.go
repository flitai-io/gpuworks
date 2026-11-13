package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

const testPassword = "gpuworks-test-password-2026"

func postPassword(router http.Handler, cookie *http.Cookie, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/password", strings.NewReader(body))
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func passwordBody(current, next string) string {
	raw, _ := json.Marshal(map[string]string{"current_password": current, "new_password": next})
	return string(raw)
}

func TestChangePasswordEndsEverySessionAndForgetsInitialCredentials(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "session.db"), bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, _, _, err := st.EnsureAdmin("admin", testPassword); err != nil {
		t.Fatal(err)
	}
	signer, err := auth.NewTokenSigner("session-test-secret", 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	var forgotten []string
	router := Router(Deps{Store: st, Bus: st.Bus, AuthSigner: signer, ForgetInitialCredentials: func(username string) error {
		forgotten = append(forgotten, username)
		return errors.New("read-only data directory") // logged, never fails the change
	}})
	cookie, _ := sessionLogin(t, router, testPassword)
	other, otherToken := sessionLogin(t, router, testPassword)

	rec := postPassword(router, cookie, passwordBody(testPassword, "Tiger#Lake9"))
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"status":"password_changed"}` {
		t.Fatalf("change: %d %s", rec.Code, rec.Body)
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("password responses must not be cached")
	}
	cleared := false
	for _, c := range rec.Result().Cookies() {
		cleared = cleared || (c.Name == managementCookie && c.MaxAge < 0)
	}
	if !cleared || len(forgotten) != 1 || forgotten[0] != "admin" {
		t.Fatalf("cookie cleared=%v, initial credentials forgotten for %v", cleared, forgotten)
	}
	for _, got := range []*httptest.ResponseRecorder{
		sessionRequest(router, http.MethodGet, "/api/v1/auth/me", cookie, ""),
		sessionRequest(router, http.MethodGet, "/api/v1/auth/me", other, ""),
		sessionRequest(router, http.MethodGet, "/api/v1/auth/me", nil, otherToken),
	} {
		if got.Code != http.StatusUnauthorized {
			t.Fatalf("a session outlived the password change: %d", got.Code)
		}
	}
	if _, err := st.AuthenticateUser("admin", testPassword); err == nil {
		t.Fatal("old password still signs in")
	}
	sessionLogin(t, router, "Tiger#Lake9")
}

func TestChangePasswordRejections(t *testing.T) {
	st, _, router := newSessionRouter(t)
	cookie, _ := sessionLogin(t, router, testPassword)
	for _, tc := range []struct {
		name, body, code string
		status           int
	}{
		{"wrong current", passwordBody("not-the-password", "Tiger#Lake9"), ErrorCurrentPasswordInvalid, http.StatusBadRequest},
		{"too short", passwordBody(testPassword, "short"), ErrorPasswordLength, http.StatusBadRequest},
		{"guessable", passwordBody(testPassword, "Password123"), ErrorPasswordGuessable, http.StatusBadRequest},
		{"control", passwordBody(testPassword, "tab\tseparated"), ErrorPasswordCharacters, http.StatusBadRequest},
		{"unchanged", passwordBody(testPassword, testPassword), ErrorPasswordUnchanged, http.StatusBadRequest},
		{"unchanged but the current password is wrong", passwordBody("Tiger#Lake9", "Tiger#Lake9"), ErrorCurrentPasswordInvalid, http.StatusBadRequest},
		{"malformed", `{"current_password":`, "", http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := postPassword(router, cookie, tc.body)
			if rec.Code != tc.status || (tc.code != "" && !strings.Contains(rec.Body.String(), `"code":"`+tc.code+`"`)) {
				t.Fatalf("%d %s, want %d %s", rec.Code, rec.Body, tc.status, tc.code)
			}
		})
	}
	// A wrong current password and a rejected new one are reported together.
	both := postPassword(router, cookie, passwordBody("not-the-password", "Password123"))
	var combined struct {
		Code   string         `json:"code"`
		Params map[string]any `json:"params"`
	}
	if err := json.Unmarshal(both.Body.Bytes(), &combined); err != nil || both.Code != http.StatusBadRequest ||
		combined.Code != ErrorCurrentPasswordInvalid || combined.Params["new_password"] != ErrorPasswordGuessable {
		t.Fatalf("combined rejection: %d %s", both.Code, both.Body)
	}
	if rec := postPassword(router, cookie, passwordBody("not-the-password", "Tiger#Lake9")); strings.Contains(rec.Body.String(), "new_password") {
		t.Fatalf("an acceptable new password was reported: %s", rec.Body)
	}
	// A length rejection tells the client the bounds to show.
	rec := postPassword(router, cookie, passwordBody(testPassword, "short"))
	var payload struct {
		Params map[string]int `json:"params"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil || payload.Params["min"] != auth.MinPasswordRunes || payload.Params["max"] != auth.MaxPasswordRunes {
		t.Fatalf("length params: %s", rec.Body)
	}
	if rec := sessionRequest(router, http.MethodGet, "/api/v1/auth/me", cookie, ""); rec.Code != http.StatusOK {
		t.Fatalf("a rejected change ended the session: %d", rec.Code)
	}
	if _, err := st.AuthenticateUser("admin", testPassword); err != nil {
		t.Fatal("a rejected change replaced the password")
	}
}

func TestChangePasswordRejectsTheUsernameForNamedAccounts(t *testing.T) {
	st, _, router := newSessionRouter(t)
	hash, err := auth.HashPassword(testPassword)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.DB().Create(&store.User{Username: "zhangsan", PasswordHash: hash, Role: "user"}).Error; err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]string{"username": "zhangsan", "password": testPassword})
	login := httptest.NewRecorder()
	router.ServeHTTP(login, httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(string(body))))
	var cookie *http.Cookie
	for _, c := range login.Result().Cookies() {
		if c.Name == managementCookie {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatalf("login: %d %s", login.Code, login.Body)
	}
	if rec := postPassword(router, cookie, passwordBody(testPassword, "ZhangSan2026")); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"code":"`+ErrorPasswordUsername+`"`) {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	// Changing one's own password leaves other accounts signed in.
	admin, _ := sessionLogin(t, router, testPassword)
	if rec := postPassword(router, cookie, passwordBody(testPassword, "Tiger#Lake9")); rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if rec := sessionRequest(router, http.MethodGet, "/api/v1/auth/me", admin, ""); rec.Code != http.StatusOK {
		t.Fatalf("another account's session ended: %d", rec.Code)
	}
}

func TestChangePasswordGuessesShareTheLoginThrottle(t *testing.T) {
	_, _, router := newSessionRouter(t)
	cookie, _ := sessionLogin(t, router, testPassword)
	var last *httptest.ResponseRecorder
	for i := 0; i < 20; i++ {
		last = postPassword(router, cookie, passwordBody("guess-"+strings.Repeat("x", i), "Tiger#Lake9"))
		if last.Code == http.StatusTooManyRequests {
			break
		}
	}
	if last.Code != http.StatusTooManyRequests || last.Header().Get("Retry-After") == "" {
		t.Fatalf("repeated wrong current passwords were not throttled: %d %s", last.Code, last.Body)
	}
	// The account, not just this form, is held back.
	body, _ := json.Marshal(map[string]string{"username": "admin", "password": testPassword})
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(string(body))))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("login after guessing: %d", rec.Code)
	}
}

func TestChangePasswordNeedsASameOriginSession(t *testing.T) {
	_, _, router := newSessionRouter(t)
	if rec := postPassword(router, nil, passwordBody(testPassword, "Tiger#Lake9")); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous: %d", rec.Code)
	}
	cookie, _ := sessionLogin(t, router, testPassword)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/password", strings.NewReader(passwordBody(testPassword, "Tiger#Lake9")))
	req.AddCookie(cookie)
	req.Header.Set("Origin", "https://attacker.example")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-origin: %d", rec.Code)
	}
	if rec := sessionRequest(router, http.MethodGet, "/api/v1/auth/password", cookie, ""); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET: %d", rec.Code)
	}
}

func TestForgetInitialCredentialsIsOptional(t *testing.T) {
	// Servers without an install record (containers, tests) wire no hook.
	_, _, router := newSessionRouter(t)
	cookie, _ := sessionLogin(t, router, testPassword)
	if rec := postPassword(router, cookie, passwordBody(testPassword, "Tiger#Lake9")); rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
}
