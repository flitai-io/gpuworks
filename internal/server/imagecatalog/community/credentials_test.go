package community

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestPrivateRegistryLoginRotationAndRevocation(t *testing.T) {
	password := "first-fixture-secret"
	fixture := &fakeRegistries{handler: func(w http.ResponseWriter, r *http.Request, host string) {
		user, secret, ok := r.BasicAuth()
		if !ok || user != "reader" || secret != password {
			w.Header().Set("WWW-Authenticate", `Basic realm="registry"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = io.WriteString(w, `{"tags":["v1.0.0"]}`)
	}}
	browser := fixture.install(t)
	lookupSecret := password
	browser.Credentials = func(host string) (string, string, error) {
		if host != "private.example" {
			t.Fatalf("credential lookup host = %s", host)
		}
		return "reader", lookupSecret, nil
	}
	for _, step := range []string{"first", "cached", "rotate", "wrong", "delete", "restore", "lookup-error"} {
		switch step {
		case "rotate":
			password, lookupSecret = "second-fixture-secret", "second-fixture-secret"
		case "wrong":
			lookupSecret = "rejected-fixture-secret"
		case "delete":
			lookupSecret = ""
		case "restore":
			lookupSecret = password
		case "lookup-error":
			browser.Credentials = func(string) (string, string, error) { return "", "", errors.New("private backend failure") }
		}
		row, err := browser.Browse(context.Background(), "private.example/team/runner", nil)
		wantError := step == "wrong" || step == "delete" || step == "lookup-error"
		if (err != nil) != wantError {
			t.Fatalf("%s: error=%v", step, err)
		}
		if !wantError && len(row.Tags) != 1 {
			t.Fatalf("%s: tags=%d", step, len(row.Tags))
		}
		if step == "cached" && fixture.count() != 1 {
			t.Fatal("successful list was not cached")
		}
		if err != nil && strings.Contains(err.Error(), "fixture-secret") {
			t.Fatal("secret appeared in error")
		}
	}
}

func TestPrivateRegistryBearerChallengeAndPagination(t *testing.T) {
	fixture := &fakeRegistries{handler: func(w http.ResponseWriter, r *http.Request, host string) {
		if host != "private.example" {
			t.Fatalf("unexpected request host %s", host)
		}
		switch r.URL.Path {
		case "/token":
			u, p, ok := r.BasicAuth()
			if !ok || u != "reader" || p != "fixture-password" || r.URL.Query().Get("scope") != "repository:team/runner:pull" {
				w.WriteHeader(401)
				return
			}
			_, _ = io.WriteString(w, `{"token":"fixture-bearer"}`)
		case "/v2/team/runner/tags/list":
			if r.Header.Get("Authorization") != "Bearer fixture-bearer" {
				w.Header().Set("WWW-Authenticate", `Bearer realm="https://private.example/token",service="private.example",scope="repository:team/runner:pull"`)
				w.WriteHeader(401)
				return
			}
			if r.URL.Query().Get("last") == "v1" {
				_, _ = io.WriteString(w, `{"tags":["v2"]}`)
				return
			}
			w.Header().Set("Link", `</v2/team/runner/tags/list?last=v1>; rel="next"`)
			_, _ = io.WriteString(w, `{"tags":["v1"]}`)
		default:
			http.NotFound(w, r)
		}
	}}
	browser := fixture.install(t)
	browser.Credentials = func(string) (string, string, error) { return "reader", "fixture-password", nil }
	row, err := browser.Browse(context.Background(), "private.example/team/runner", nil)
	if err != nil || len(row.Tags) != 2 || fixture.count() != 4 {
		t.Fatalf("browse error=%v tags=%d requests=%d", err, len(row.Tags), fixture.count())
	}
}

func TestPrivateRegistryDoesNotLeakLoginToForeignSources(t *testing.T) {
	for _, mode := range []string{"token", "override", "pagination"} {
		t.Run(mode, func(t *testing.T) {
			foreignCalls := 0
			fixture := &fakeRegistries{handler: func(w http.ResponseWriter, r *http.Request, host string) {
				if host == "foreign.example" {
					foreignCalls++
					if r.Header.Get("Authorization") != "" {
						t.Fatal("credentials escaped registry origin")
					}
					w.WriteHeader(401)
					return
				}
				if mode == "token" {
					w.Header().Set("WWW-Authenticate", `Bearer realm="https://foreign.example/token",scope="repository:team/runner:pull"`)
					w.WriteHeader(401)
					return
				}
				w.Header().Set("Link", `<https://foreign.example/next>; rel="next"`)
				_, _ = io.WriteString(w, `{"tags":["v1"]}`)
			}}
			browser := fixture.install(t)
			browser.Credentials = func(string) (string, string, error) { return "reader", "fixture-password", nil }
			var configured []Repo
			if mode == "override" {
				configured = []Repo{{Repo: "private.example/team/runner", Engine: "vllm", UpstreamAPI: "https://foreign.example/tags"}}
			}
			_, err := browser.Browse(context.Background(), "private.example/team/runner", configured)
			if err == nil {
				t.Fatal("expected private registry refusal")
			}
			if mode == "pagination" && foreignCalls != 0 {
				t.Fatal("foreign pagination was followed")
			}
		})
	}
}

type credentialRoundTripFunc func(*http.Request) (*http.Response, error)

func (f credentialRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestAuthenticatedRegistryRedirectDoesNotReachSubdomain(t *testing.T) {
	calls := 0
	client := &http.Client{Transport: credentialRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if calls > 1 {
			t.Fatal("authenticated redirect reached another origin")
		}
		return &http.Response{StatusCode: 302, Header: http.Header{"Location": []string{"https://sub.private.example/steal"}}, Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
	})}
	browser := NewBrowser(client)
	browser.Credentials = func(string) (string, string, error) { return "reader", "fixture-password", nil }
	_, err := browser.Browse(context.Background(), "private.example/team/runner", nil)
	if err == nil || calls != 1 {
		t.Fatalf("error=%v calls=%d", err, calls)
	}
}
