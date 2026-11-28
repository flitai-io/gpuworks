package api

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/featuregate"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// A public extension route answers without a session, follows its feature,
// and is limited per client address: a burst of 30, then 429 with
// Retry-After; another address has its own bucket.
func TestPublicExtensionRoutesNeedNoSessionAndAreLimitedPerAddress(t *testing.T) {
	src := &switchSource{}
	src.set(map[featuregate.Key]featuregate.Availability{"fake.things": {Access: featuregate.Full}})
	gate := mustGate(t, src, featuregate.Feature{Key: "fake.things"})
	h := Router(Deps{Extensions: ExtensionDeps{Features: gate, Extensions: []Extension{fakeExtension{name: "fake", routes: []RouteSpec{
		{Method: http.MethodGet, Pattern: "/x/fake/start", Access: AccessPublic, Feature: "fake.things"},
	}}}}})
	call := func(ip string) *http.Request {
		req := httptestRequest(http.MethodGet, "/api/v1/x/fake/start", "")
		req.RemoteAddr = ip + ":1234"
		return req
	}
	rec := serve(h, call("198.51.100.7"))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"claims":false`) {
		t.Fatalf("anonymous call = %d %s, want 200 without claims", rec.Code, rec.Body)
	}
	for i := 1; i < 30; i++ {
		if rec := serve(h, call("198.51.100.7")); rec.Code != http.StatusOK {
			t.Fatalf("call %d within the burst = %d", i+1, rec.Code)
		}
	}
	rec = serve(h, call("198.51.100.7"))
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" || !strings.Contains(rec.Body.String(), "TOO_MANY_REQUESTS") {
		t.Fatalf("call past the burst = %d %q %s", rec.Code, rec.Header().Get("Retry-After"), rec.Body)
	}
	if rec := serve(h, call("203.0.113.9")); rec.Code != http.StatusOK {
		t.Fatalf("another address = %d", rec.Code)
	}
	src.set(map[featuregate.Key]featuregate.Availability{"fake.things": {Access: featuregate.Hidden}})
	if rec := serve(h, call("203.0.113.10")); rec.Code != http.StatusNotFound {
		t.Fatalf("hidden feature = %d, want 404", rec.Code)
	}
}

// fakeRedeemer answers every redemption with user and err (no user and
// ErrSignInTicketInvalid by default) and records the source and handle it was
// asked with.
type fakeRedeemer struct {
	user   *store.User
	err    error
	source *string
	handle *string
}

func (f fakeRedeemer) RedeemSignInTicket(_, source, handle string) (*store.User, error) {
	if f.source != nil {
		*f.source, *f.handle = source, handle
	}
	if f.user == nil && f.err == nil {
		return nil, store.ErrSignInTicketInvalid
	}
	return f.user, f.err
}
func (fakeRedeemer) BindingKey() []byte { return []byte("k") }

// A sign-in entry must start at one of its module's public GET routes, and
// entries need a redeemer.
func TestSignInEntriesMustStartAtAPublicGet(t *testing.T) {
	gate := mustGate(t, nil, featuregate.Feature{Key: "fake.things"})
	routes := []RouteSpec{
		{Method: http.MethodGet, Pattern: "/x/fake/start", Access: AccessPublic, Community: true},
		{Method: http.MethodGet, Pattern: "/x/fake/admin", Access: AccessAdmin, Community: true},
	}
	ext := []Extension{fakeExtension{name: "fake", routes: routes}}
	for _, tc := range []struct {
		name  string
		entry SignInOption
		red   SignInRedeemer
		want  string
	}{
		{"not public", SignInOption{Source: "acme", Start: "/x/fake/admin", Module: "fake"}, fakeRedeemer{}, "not one of its AccessPublic GET routes"},
		{"no route", SignInOption{Source: "acme", Start: "/x/fake/none", Module: "fake"}, fakeRedeemer{}, "not one of its AccessPublic GET routes"},
		{"no redeemer", SignInOption{Source: "acme", Start: "/x/fake/start", Module: "fake"}, nil, "without an account redeemer"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := routerPanic(Deps{Extensions: ExtensionDeps{Extensions: ext, Features: gate, SignIns: []SignInOption{tc.entry}, Accounts: tc.red}})
			if !strings.Contains(got, tc.want) {
				t.Fatalf("panic = %q, want %q", got, tc.want)
			}
		})
	}
}

// Without a binding cookie a completion is refused on the sign-in page, and
// a start for a source nobody offers is 404 with a code, never a redirect.
func TestSignInRefusalsAreCodesAndNeverRedirectElsewhere(t *testing.T) {
	gate := mustGate(t, nil, featuregate.Feature{Key: "fake.things"})
	h := Router(Deps{Extensions: ExtensionDeps{Features: gate, Accounts: fakeRedeemer{},
		Extensions: []Extension{fakeExtension{name: "fake", routes: []RouteSpec{
			{Method: http.MethodGet, Pattern: "/x/fake/start", Access: AccessPublic, Community: true}}}},
		SignIns: []SignInOption{{Source: "acme", Label: "Acme", Start: "/x/fake/start", Module: "fake"}}}})

	rec := serve(h, httptestRequest(http.MethodGet, "/api/v1/auth/sign-in/start?source=//evil.example.com", ""))
	if rec.Code != http.StatusNotFound || rec.Header().Get("Location") != "" ||
		strings.TrimSpace(rec.Body.String()) != `{"code":"IDENTITY_SOURCE_UNAVAILABLE","error":"no sign-in through this identity source is available","params":{"source":"//evil.example.com"}}` {
		t.Fatalf("unknown source = %d %q %s", rec.Code, rec.Header().Get("Location"), rec.Body)
	}
	rec = serve(h, httptestRequest(http.MethodGet, "/api/v1/auth/sign-in/start?source=acme", ""))
	loc := rec.Header().Get("Location")
	if rec.Code != http.StatusFound || !strings.HasPrefix(loc, "/api/v1/x/fake/start?handle=") || len(strings.TrimPrefix(loc, "/api/v1/x/fake/start?handle=")) != 64 {
		t.Fatalf("start = %d %q", rec.Code, loc)
	}
	cookie := rec.Result().Cookies()
	if len(cookie) != 1 || cookie[0].Name != signInBindingCookie || !cookie[0].HttpOnly || cookie[0].SameSite != http.SameSiteLaxMode ||
		cookie[0].Path != "/api/v1/auth" || cookie[0].MaxAge != 600 || strings.Contains(loc, cookie[0].Value) {
		t.Fatalf("binding cookie = %+v", cookie)
	}
	rec = serve(h, httptestRequest(http.MethodGet, "/api/v1/auth/complete?ticket=x", ""))
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/login?code=IDENTITY_TICKET_INVALID" {
		t.Fatalf("completion without a binding = %d %q", rec.Code, rec.Header().Get("Location"))
	}
	rec = serve(h, httptestRequest(http.MethodGet, "/api/v1/auth/sign-in-options", ""))
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) !=
		`{"local_password":"all","entries":[{"source":"acme","label":"Acme","url":"/api/v1/auth/sign-in/start?source=acme"}]}` {
		t.Fatalf("options = %d %s", rec.Code, rec.Body)
	}
	for _, path := range []string{"/api/v1/auth/sign-in-options", "/api/v1/auth/sign-in/start?source=acme", "/api/v1/auth/complete?ticket=x"} {
		if rec := serve(h, httptestRequest(http.MethodPost, path, "")); rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("POST %s = %d, want 405", path, rec.Code)
		}
	}
	// Without extensions there are no entries and the password form is all.
	rec = serve(Router(Deps{}), httptestRequest(http.MethodGet, "/api/v1/auth/sign-in-options", ""))
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"local_password":"all","entries":[]}` {
		t.Fatalf("community options = %d %s", rec.Code, rec.Body)
	}
	rec = serve(Router(Deps{}, auth.SessionPolicy{LocalPassword: auth.LocalPasswordLocalAdmin}),
		httptestRequest(http.MethodGet, "/api/v1/auth/sign-in-options", ""))
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"local_password":"local_admin","entries":[]}` {
		t.Fatalf("options with local_password local_admin = %d %s", rec.Code, rec.Body)
	}
}

// A completion redeems the ticket for the source the browser started with,
// under the handle made for that source and binding, and each refusal lands
// on the sign-in page with the code that names it: a disabled account, a
// source withdrawn since the ticket was issued, and a server fault, which is
// not a ticket problem.
func TestSignInCompletionAsksForTheStartedSourceAndNamesEachRefusal(t *testing.T) {
	src := &switchSource{}
	src.set(map[featuregate.Key]featuregate.Availability{"fake.things": {Access: featuregate.Full}})
	gate := mustGate(t, src, featuregate.Feature{Key: "fake.things"})
	router := func(red fakeRedeemer) http.Handler {
		return Router(Deps{Extensions: ExtensionDeps{Features: gate, Accounts: red,
			Extensions: []Extension{fakeExtension{name: "fake", routes: []RouteSpec{
				{Method: http.MethodGet, Pattern: "/x/fake/start", Access: AccessPublic, Feature: "fake.things"}}}},
			SignIns: []SignInOption{{Source: "acme", Label: "Acme", Start: "/x/fake/start", Module: "fake"}}}})
	}
	complete := func(h http.Handler, cookie string) string {
		req := httptestRequest(http.MethodGet, "/api/v1/auth/complete?ticket=t", "")
		req.AddCookie(&http.Cookie{Name: signInBindingCookie, Value: cookie})
		rec := serve(h, req)
		if rec.Code != http.StatusFound {
			t.Fatalf("completion = %d %s", rec.Code, rec.Body)
		}
		return rec.Header().Get("Location")
	}

	var source, handle string
	h := router(fakeRedeemer{source: &source, handle: &handle})
	rec := serve(h, httptestRequest(http.MethodGet, "/api/v1/auth/sign-in/start?source=acme", ""))
	started := rec.Result().Cookies()[0].Value
	sent := strings.TrimPrefix(rec.Header().Get("Location"), "/api/v1/x/fake/start?handle=")
	if !strings.HasPrefix(started, "acme.") {
		t.Fatalf("binding cookie = %q, want it to name the source", started)
	}
	if got := complete(h, started); got != "/login?code=IDENTITY_TICKET_INVALID" || source != "acme" || handle != sent {
		t.Fatalf("completion asked for %q %q (handle sent %q), landed on %q", source, handle, sent, got)
	}
	// The same binding under another source's name is another handle.
	_, binding, _ := strings.Cut(started, ".")
	complete(h, "beta."+binding)
	if source != "beta" || handle == sent {
		t.Fatalf("a binding renamed to beta was asked as %q with the acme handle", source)
	}
	if got := complete(h, binding); got != "/login?code=IDENTITY_TICKET_INVALID" {
		t.Fatalf("a binding without a source = %q", got)
	}

	if got := complete(router(fakeRedeemer{err: store.ErrUserDisabled}), started); got != "/login?code=USER_DISABLED" {
		t.Fatalf("a disabled account = %q", got)
	}
	if got := complete(router(fakeRedeemer{err: errors.New("database is locked")}), started); got != "/login?code=SERVICE_UNAVAILABLE" {
		t.Fatalf("a store failure = %q", got)
	}
	src.set(map[featuregate.Key]featuregate.Availability{"fake.things": {Access: featuregate.Locked}})
	if got := complete(router(fakeRedeemer{user: &store.User{ID: 7, Username: "alice", Role: "user"}}), started); got != "/login?code=IDENTITY_SOURCE_UNAVAILABLE" {
		t.Fatalf("a source locked since the ticket was issued = %q", got)
	}
}
