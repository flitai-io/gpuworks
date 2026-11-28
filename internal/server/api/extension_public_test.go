package api

import (
	"net/http"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/featuregate"
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
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"claims":false,"route":"GET /x/fake/start","user":""}` {
		t.Fatalf("anonymous call = %d %s, want 200 without claims", rec.Code, rec.Body)
	}
	for i := 1; i < 30; i++ {
		if rec := serve(h, call("198.51.100.7")); rec.Code != http.StatusOK {
			t.Fatalf("call %d within the burst = %d", i+1, rec.Code)
		}
	}
	rec = serve(h, call("198.51.100.7"))
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" ||
		strings.TrimSpace(rec.Body.String()) != `{"code":"TOO_MANY_REQUESTS","error":"too many requests to this route; try again later"}` {
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

// A public route never reads or renews a session: a valid session cookie
// gives the handler no claims and the response no cookie, while the same
// cookie on a signed-in route gives claims and is renewed (a write renews
// a session).
func TestAPublicRouteNeitherReadsNorRenewsTheSession(t *testing.T) {
	rig := newDynamicRig(t, nil, func(d *Deps) {
		d.Extensions.Extensions = []Extension{fakeExtension{name: "fake", routes: []RouteSpec{
			{Method: http.MethodPost, Pattern: "/x/fake/public", Access: AccessPublic, Community: true},
			{Method: http.MethodPost, Pattern: "/x/fake/member", Community: true},
		}}}
	})
	call := func(path string) *http.Request {
		req := httptestRequest(http.MethodPost, path, "")
		req.AddCookie(&http.Cookie{Name: managementCookie, Value: rig.sessionCookie})
		return req
	}
	rec := serve(rig.h, call("/api/v1/x/fake/public"))
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"claims":false,"route":"POST /x/fake/public","user":""}` ||
		len(rec.Result().Cookies()) != 0 {
		t.Fatalf("public route with a session = %d %s cookies %v", rec.Code, rec.Body, rec.Result().Cookies())
	}
	rec = serve(rig.h, call("/api/v1/x/fake/member"))
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"claims":true,"route":"POST /x/fake/member","user":"admin"}` ||
		len(rec.Header().Values("Set-Cookie")) != 1 || !strings.HasPrefix(rec.Header().Get("Set-Cookie"), managementCookie+"="+rig.sessionCookie+";") {
		t.Fatalf("signed-in route with the session = %d %s cookies %v", rec.Code, rec.Body, rec.Result().Cookies())
	}
}

// A route declares a sign-in role only when it is public, and a public
// route is never readable by the assistant.
func TestPublicAndSignInRoutesAreCheckedWhenMounted(t *testing.T) {
	for _, tc := range []struct {
		name string
		spec RouteSpec
		want string
	}{
		{"SignIn on a signed-in route", RouteSpec{Method: http.MethodPost, Pattern: "/x/fake/acs", SignIn: true, Community: true},
			"api: extension fake: POST /api/v1/x/fake/acs: only an AccessPublic route can be a SignIn route"},
		{"SignIn on an admin route", RouteSpec{Method: http.MethodGet, Pattern: "/x/fake/start", Access: AccessAdmin, SignIn: true, Community: true},
			"api: extension fake: GET /api/v1/x/fake/start: only an AccessPublic route can be a SignIn route"},
		{"a public route the assistant reads", RouteSpec{Method: http.MethodGet, Pattern: "/x/fake/start", Access: AccessPublic, AIReadable: true, Community: true},
			"api: extension fake: GET /api/v1/x/fake/start: a public route cannot be AIReadable"},
		{"an unknown access", RouteSpec{Method: http.MethodGet, Pattern: "/x/fake/start", Access: AccessPublic + 1, Community: true},
			"api: extension fake: GET /api/v1/x/fake/start: unknown access 4"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := routerPanic(Deps{Extensions: ExtensionDeps{Extensions: []Extension{fakeExtension{name: "fake", routes: []RouteSpec{tc.spec}}}}})
			if got != tc.want {
				t.Fatalf("panic = %q, want %q", got, tc.want)
			}
		})
	}
}

// Under a platform limit an extension's public route follows its method: a
// read is served, a change is refused at once with the limit's code, before
// any session is asked for. A compiled module's SignIn route is served under
// any limit, POST included. A package's public route has no sign-in role,
// so its change is refused too. A public route of a hidden feature answers
// 404 as on an unlimited server, and a public path asked with a method it
// does not declare is a route that needs a session: the limit shows neither.
// Routes that need a session are unchanged.
func TestExtensionPublicRoutesUnderALimitFollowTheirMethod(t *testing.T) {
	limiter := newSwitchLimiter(featuregate.Full)
	rig := newDynamicRig(t, []DynamicRoute{
		dynRoute("GET", "/hook", DynamicPublic, func(r *DynamicRoute) { r.RatePerMinute, r.RateBurst = 600, 600 }),
		dynRoute("POST", "/hook", DynamicPublic, func(r *DynamicRoute) { r.RatePerMinute, r.RateBurst = 600, 600 }),
		dynRoute("POST", "/concealed", DynamicPublic, func(r *DynamicRoute) { r.Feature, r.RatePerMinute, r.RateBurst = "acme.hidden", 600, 600 }),
		dynRoute("POST", "/items", DynamicMember),
	}, func(d *Deps) {
		d.Extensions.Platform = limiter
		d.Extensions.Extensions = []Extension{fakeExtension{name: "fake", routes: []RouteSpec{
			{Method: http.MethodGet, Pattern: "/x/fake/start", Access: AccessPublic, Community: true},
			{Method: http.MethodPost, Pattern: "/x/fake/hook", Access: AccessPublic, Community: true},
			{Method: http.MethodPost, Pattern: "/x/fake/acs", Access: AccessPublic, SignIn: true, Community: true},
			{Method: http.MethodPost, Pattern: "/x/fake/concealed", Access: AccessPublic, Feature: "acme.hidden"},
			{Method: http.MethodPost, Pattern: "/x/fake/things", Community: true},
		}}}
	})
	const locked = `{"code":"PLATFORM_LOCKED","error":"this server is locked; it can be viewed, and changes and inference are refused until it is unlocked"}`
	const readOnly = `{"code":"PLATFORM_READ_ONLY","error":"this server is read-only; what runs keeps running and can be viewed, changes are refused"}`
	type want struct {
		code int
		body string // compared when set
	}
	served := want{code: http.StatusOK}
	for _, tc := range []struct {
		name, bearer, method, path string
		full, readOnly, locked     want
	}{
		{"a module's public read", "", "GET", "/api/v1/x/fake/start", served, served, served},
		{"a module's public change", "", "POST", "/api/v1/x/fake/hook", served, want{403, readOnly}, want{403, locked}},
		{"a module's sign-in change", "", "POST", "/api/v1/x/fake/acs", served, served, served},
		{"a module's change that needs a session", rig.member, "POST", "/api/v1/x/fake/things", served, want{403, readOnly}, want{403, locked}},
		{"a module's change that needs a session, without one", "", "POST", "/api/v1/x/fake/things", want{code: 401}, want{code: 401}, want{code: 401}},
		{"a package's public read", "", "GET", "/api/v1/x/acme_fake/hook", served, served, served},
		{"a package's public change", "", "POST", "/api/v1/x/acme_fake/hook", served, want{403, readOnly}, want{403, locked}},
		{"a package's change that needs a session", rig.member, "POST", "/api/v1/x/acme_fake/items", served, want{403, readOnly}, want{403, locked}},
		{"a package's change that needs a session, without one", "", "POST", "/api/v1/x/acme_fake/items", want{code: 401}, want{code: 401}, want{code: 401}},
		{"a module's public change of a hidden feature", "", "POST", "/api/v1/x/fake/concealed", want{code: 404}, want{code: 404}, want{code: 404}},
		{"a package's public change of a hidden feature", "", "POST", "/api/v1/x/acme_fake/concealed", want{code: 404}, want{code: 404}, want{code: 404}},
		{"a module's public path by a method it does not declare", "", "DELETE", "/api/v1/x/fake/start", want{code: 401}, want{code: 401}, want{code: 401}},
		{"a package's public path by a method it does not declare", "", "DELETE", "/api/v1/x/acme_fake/hook", want{code: 401}, want{code: 401}, want{code: 401}},
	} {
		for _, state := range []struct {
			access featuregate.Access
			want   want
		}{{featuregate.Full, tc.full}, {featuregate.ReadOnly, tc.readOnly}, {featuregate.Locked, tc.locked}} {
			limiter.set(PlatformLimit{Access: state.access})
			rec := rig.do(tc.bearer, tc.method, tc.path, "")
			if rec.Code != state.want.code || (state.want.body != "" && strings.TrimSpace(rec.Body.String()) != state.want.body) {
				t.Errorf("%s under %s: %s %s = %d %s, want %d %s", tc.name, state.access, tc.method, tc.path, rec.Code, rec.Body.String(), state.want.code, state.want.body)
			}
		}
	}
}
