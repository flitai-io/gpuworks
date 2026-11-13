package main

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The fixture answers as GitHub does: the installation lookup and the token
// mint both require a valid RS256 App JWT, the mint answers 201 with an
// expiry, and anything else is a 401 with GitHub's error body.
func TestGitHubAppMintsAndReusesInstallationTokens(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 12, 8, 0, 0, 0, time.UTC)
	var mints atomic.Int32
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := verifyJWT(r.Header.Get("Authorization"), &key.PublicKey, "123", now); err != nil {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"message":"A JSON web token could not be decoded","documentation_url":"https://docs.github.com/rest"}`)
			return
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/flitai-io/gpuworks/installation":
			_, _ = io.WriteString(w, `{"id":42,"access_tokens_url":"`+server.URL+`/app/installations/42/access_tokens"}`)
		case r.Method == http.MethodPost && r.URL.Path == "/app/installations/42/access_tokens":
			n := mints.Add(1)
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"token":"ghs_token`+string(rune('0'+n))+`","expires_at":"`+now.Add(time.Hour).Format(time.RFC3339)+`"}`)
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"message":"Not Found"}`)
		}
	}))
	defer server.Close()

	app := &githubApp{api: server.URL, appID: "123", key: key, repo: "flitai-io/gpuworks", client: server.Client(), now: func() time.Time { return now }}
	user, token, err := app.Creds(context.Background())
	if err != nil || user != "x-access-token" || token != "ghs_token1" {
		t.Fatalf("Creds = %q %q %v", user, token, err)
	}
	now = now.Add(50 * time.Minute)
	if _, token, _ := app.Creds(context.Background()); token != "ghs_token1" || mints.Load() != 1 {
		t.Fatalf("token %q after %d mints, want the first reused", token, mints.Load())
	}
	now = now.Add(6 * time.Minute) // within five minutes of the expiry
	if _, token, _ := app.Creds(context.Background()); token != "ghs_token2" {
		t.Fatalf("token %q, want a fresh one near expiry", token)
	}

	wrong, _ := rsa.GenerateKey(rand.Reader, 2048)
	other := &githubApp{api: server.URL, appID: "123", key: wrong, repo: "flitai-io/gpuworks", client: server.Client(), now: func() time.Time { return now }}
	if _, _, err := other.Creds(context.Background()); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("a wrong key = %v, want GitHub's 401", err)
	}
}

func verifyJWT(header string, key *rsa.PublicKey, appID string, now time.Time) error {
	token, ok := strings.CutPrefix(header, "Bearer ")
	parts := strings.Split(token, ".")
	if !ok || len(parts) != 3 {
		return errMalformed
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return err
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(key, crypto.SHA256, digest[:], signature); err != nil {
		return err
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return err
	}
	var claims struct {
		Iat, Exp int64
		Iss      string
	}
	if err := json.Unmarshal(raw, &claims); err != nil {
		return err
	}
	if claims.Iss != appID || claims.Iat > now.Unix() || claims.Exp <= now.Unix() || claims.Exp-claims.Iat > 600 {
		return errMalformed
	}
	return nil
}

var errMalformed = errors.New("malformed JWT")

func TestParseAppKeyAcceptsBothPEMForms(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	pkcs8, _ := x509.MarshalPKCS8PrivateKey(key)
	for name, block := range map[string]*pem.Block{
		"pkcs1": {Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)},
		"pkcs8": {Type: "PRIVATE KEY", Bytes: pkcs8},
	} {
		if parsed, err := parseAppKey(pem.EncodeToMemory(block)); err != nil || !parsed.Equal(key) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if _, err := parseAppKey([]byte("not a key")); err == nil {
		t.Fatal("garbage was accepted as a key")
	}
}

// git runs the binary as GIT_ASKPASS with its prompt as the one argument,
// and reads the answer from stdout.
func TestAskpassAnswersGitsPrompts(t *testing.T) {
	if runAskpass([]string{"gitsync", "Username for 'https://github.com': "}) {
		t.Fatal("answered as askpass without the hook variable")
	}
	t.Setenv(askpassEnv, "1")
	t.Setenv(askpassUserEnv, "x-access-token")
	t.Setenv(askpassPasswordEnv, "secret")
	for prompt, want := range map[string]string{
		"Username for 'https://github.com': ":                "x-access-token",
		"Password for 'https://x-access-token@github.com': ": "secret",
	} {
		if got := askpassAnswer(t, prompt); got != want {
			t.Fatalf("prompt %q answered %q, want %q", prompt, got, want)
		}
	}
}

func askpassAnswer(t *testing.T, prompt string) string {
	t.Helper()
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout := os.Stdout
	os.Stdout = write
	answered := runAskpass([]string{"gitsync", prompt})
	os.Stdout = stdout
	_ = write.Close()
	out, _ := io.ReadAll(read)
	if !answered {
		t.Fatal("runAskpass did not answer")
	}
	return strings.TrimSpace(string(out))
}

// The DingTalk robot answers 200 with errcode 0 on success and 200 with a
// non-zero errcode (an invalid token, a keyword rule) on refusal.
func TestAlertsGoToDingTalkOnceAndAgainAfterTheRepeat(t *testing.T) {
	var bodies []string
	refuse := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(body))
		if refuse {
			_, _ = io.WriteString(w, `{"errcode":310000,"errmsg":"keywords not in content"}`)
			return
		}
		_, _ = io.WriteString(w, `{"errcode":0,"errmsg":"ok"}`)
	}))
	defer server.Close()
	now := time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)
	var logs []string
	a := &alerter{webhook: server.URL, repeat: 30 * time.Minute, client: server.Client(), now: func() time.Time { return now }, logf: func(format string, args ...any) { logs = append(logs, format) }}
	problem := map[string]string{"refs/heads/master x": "refs/heads/master diverged"}

	a.report(context.Background(), problem)
	if len(bodies) != 1 || !strings.Contains(bodies[0], `"msgtype":"text"`) || !strings.Contains(bodies[0], "refs/heads/master diverged") {
		t.Fatalf("bodies = %v, want one text message naming the problem", bodies)
	}
	now = now.Add(10 * time.Minute)
	a.report(context.Background(), problem)
	if len(bodies) != 1 {
		t.Fatal("repeated within the repeat interval")
	}
	now = now.Add(25 * time.Minute)
	a.report(context.Background(), problem)
	if len(bodies) != 2 {
		t.Fatal("not repeated after the interval")
	}
	a.report(context.Background(), map[string]string{})
	a.report(context.Background(), problem)
	if len(bodies) != 3 {
		t.Fatal("a problem that cleared and came back was not reported afresh")
	}

	refuse = true
	now = now.Add(time.Hour)
	a.report(context.Background(), problem)
	a.report(context.Background(), problem)
	if len(bodies) != 5 {
		t.Fatalf("a refused alert was not retried on the next report (%d sends)", len(bodies))
	}
	if !strings.Contains(strings.Join(logs, "\n"), "alert not delivered") {
		t.Fatal("a refused alert was not logged")
	}
}
