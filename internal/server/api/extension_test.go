package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/featuregate"
)

// fakeExtension mounts the routes it is given; each answers who called it.
type fakeExtension struct {
	name   string
	routes []RouteSpec
	nav    []dto.NavContribution
	nilH   bool
}

func (f fakeExtension) Name() string { return f.name }
func (f fakeExtension) Routes(m Mounter) {
	for _, spec := range f.routes {
		if f.nilH {
			m.Handle(spec, nil)
			continue
		}
		m.Handle(spec, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			claims, ok := auth.ClaimsFrom(r.Context())
			writeJSON(w, http.StatusOK, map[string]any{"route": spec.Method + " " + spec.Pattern, "user": claims.Username, "claims": ok})
		}))
	}
}

type fakeNavExtension struct{ fakeExtension }

// keptMounter keeps the Mounter it was handed.
type keptMounter struct{ m Mounter }

func (k *keptMounter) Name() string     { return "keeper" }
func (k *keptMounter) Routes(m Mounter) { k.m = m }

func (f fakeNavExtension) Nav() []dto.NavContribution { return f.nav }

// switchSource is a featuregate.Source whose answers a test swaps.
type switchSource struct {
	snap atomic.Pointer[map[featuregate.Key]featuregate.Availability]
}

func (s *switchSource) set(m map[featuregate.Key]featuregate.Availability) { s.snap.Store(&m) }
func (s *switchSource) Availability(k featuregate.Key) featuregate.Availability {
	if m := s.snap.Load(); m != nil {
		return (*m)[k]
	}
	return featuregate.Availability{}
}

func mustGate(t *testing.T, src featuregate.Source, features ...featuregate.Feature) *featuregate.Gate {
	t.Helper()
	g, err := featuregate.New(features, src)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func routerPanic(deps Deps) (msg string) {
	defer func() {
		if r := recover(); r != nil {
			msg, _ = r.(string)
		}
	}()
	Router(deps)
	return ""
}

func TestExtensionMountRefusesBadRegistrations(t *testing.T) {
	gate := mustGate(t, nil, featuregate.Feature{Key: "fake.things"})
	get := func(pattern string) RouteSpec {
		return RouteSpec{Method: http.MethodGet, Pattern: pattern, Community: true}
	}
	for _, tc := range []struct {
		name string
		ext  []Extension
		want string
	}{
		{"bad name", []Extension{fakeExtension{name: "Fake"}}, "does not match"},
		{"one-letter name", []Extension{fakeExtension{name: "f"}}, "does not match"},
		{"twice", []Extension{fakeExtension{name: "fake"}, fakeExtension{name: "fake"}}, "registered twice"},
		{"outside namespace", []Extension{fakeExtension{name: "fake", routes: []RouteSpec{get("/other")}}}, "outside /api/v1/x/fake/"},
		{"outside the extension root", []Extension{fakeExtension{name: "fake", routes: []RouteSpec{get("/fake/a")}}}, "outside /api/v1/x/fake/"},
		{"another extension's namespace", []Extension{fakeExtension{name: "fake", routes: []RouteSpec{get("/x/other/a")}}}, "outside /api/v1/x/fake/"},
		{"prefix is not a namespace", []Extension{fakeExtension{name: "fake", routes: []RouteSpec{get("/x/fakes/x")}}}, "outside /api/v1/x/fake/"},
		{"method", []Extension{fakeExtension{name: "fake", routes: []RouteSpec{{Method: http.MethodTrace, Pattern: "/x/fake", Community: true}}}}, "not allowed"},
		{"collision", []Extension{fakeExtension{name: "fake", routes: []RouteSpec{get("/x/fake/a"), get("/x/fake/a")}}}, "registered twice"},
		{"collision by parameter name", []Extension{fakeExtension{name: "fake", routes: []RouteSpec{get("/x/fake/{id}"), get("/x/fake/{name}")}}}, "registered twice"},
		{"dot-dot", []Extension{fakeExtension{name: "fake", routes: []RouteSpec{get("/x/fake/../../nodes")}}}, "not a clean path"},
		{"dot", []Extension{fakeExtension{name: "fake", routes: []RouteSpec{get("/x/fake/./a")}}}, "not a clean path"},
		{"double slash", []Extension{fakeExtension{name: "fake", routes: []RouteSpec{get("/x/fake//")}}}, "not a clean path"},
		{"trailing slash", []Extension{fakeExtension{name: "fake", routes: []RouteSpec{get("/x/fake/a/")}}}, "not a clean path"},
		{"no feature", []Extension{fakeExtension{name: "fake", routes: []RouteSpec{{Method: http.MethodGet, Pattern: "/x/fake"}}}}, "names no feature and is not Community"},
		{"both", []Extension{fakeExtension{name: "fake", routes: []RouteSpec{{Method: http.MethodGet, Pattern: "/x/fake", Feature: "fake.things", Community: true}}}}, "choose one"},
		{"undeclared feature", []Extension{fakeExtension{name: "fake", routes: []RouteSpec{{Method: http.MethodGet, Pattern: "/x/fake", Feature: "fake.other"}}}}, "undeclared feature"},
		{"readable write", []Extension{fakeExtension{name: "fake", routes: []RouteSpec{{Method: http.MethodPost, Pattern: "/x/fake", Community: true, AIReadable: true}}}}, "only a GET route"},
		{"nil handler", []Extension{fakeExtension{name: "fake", nilH: true, routes: []RouteSpec{get("/x/fake")}}}, "has no handler"},
		{"menu entry", []Extension{fakeNavExtension{fakeExtension{name: "fake", nav: []dto.NavContribution{{ID: "x", Group: "g", LabelKey: "k", Route: "no-slash"}}}}}, "needs an id"},
		{"menu entry twice", []Extension{fakeNavExtension{fakeExtension{name: "fake", nav: []dto.NavContribution{{ID: "x", Group: "g", LabelKey: "k", Route: "/x"}, {ID: "x", Group: "g", LabelKey: "k", Route: "/y"}}}}}, "added twice"},
		{"menu feature", []Extension{fakeNavExtension{fakeExtension{name: "fake", nav: []dto.NavContribution{{ID: "x", Group: "g", LabelKey: "k", Route: "/x", Feature: "fake.other"}}}}}, "undeclared feature"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := routerPanic(Deps{Extensions: ExtensionDeps{Extensions: tc.ext, Features: gate}})
			if !strings.Contains(got, tc.want) {
				t.Fatalf("panic = %q, want it to say %q", got, tc.want)
			}
		})
	}
	// A Mounter kept past Routes mounts nothing.
	keeper := &keptMounter{}
	Router(Deps{Extensions: ExtensionDeps{Extensions: []Extension{keeper}}})
	func() {
		defer func() {
			if r, _ := recover().(string); !strings.Contains(r, "after Router was built") {
				t.Fatalf("late Handle: panic = %q", r)
			}
		}()
		keeper.m.Handle(RouteSpec{Method: http.MethodGet, Pattern: "/x/keeper/late", Community: true}, http.NotFoundHandler())
	}()
	// Without a gate no feature is declared, so naming one fails at Router.
	if got := routerPanic(Deps{Extensions: ExtensionDeps{Extensions: []Extension{fakeExtension{name: "fake", routes: []RouteSpec{{Method: http.MethodGet, Pattern: "/x/fake", Feature: "fake.things"}}}}}}); !strings.Contains(got, "undeclared feature") {
		t.Fatalf("nil gate: panic = %q", got)
	}
}

func TestExtensionRoutesInheritManagementAuth(t *testing.T) {
	var signer *auth.TokenSigner
	ext := fakeExtension{name: "fake", routes: []RouteSpec{
		{Method: http.MethodGet, Pattern: "/x/fake/items", Community: true},
		{Method: http.MethodPost, Pattern: "/x/fake/items", Community: true},
		{Method: http.MethodPost, Pattern: "/x/fake/admin", Access: AccessAdmin, Community: true},
		{Method: http.MethodGet, Pattern: "/x/fake/admin/{id}", Access: AccessAdmin, Community: true},
		{Method: http.MethodPost, Pattern: "/x/fake/session", Access: AccessAdminSession, Community: true},
	}}
	rig := newTokenRig(t, func(d *Deps) {
		d.Extensions = ExtensionDeps{Extensions: []Extension{ext}}
		signer = d.AuthSigner
	})
	member, err := managementSessionTestToken(t, rig.st, signer, auth.Claims{Username: "member", Role: "user"})
	if err != nil {
		t.Fatal(err)
	}
	read, write := rig.mint("read"), rig.mint("write")
	for _, tc := range []struct {
		name, bearer, method, path string
		status                     int
		code, user                 string
	}{
		{"anonymous read", "", "GET", "/api/v1/x/fake/items", 401, "AUTHENTICATION_REQUIRED", ""},
		{"anonymous admin", "", "POST", "/api/v1/x/fake/admin", 401, "AUTHENTICATION_REQUIRED", ""},
		{"anonymous unknown path", "", "GET", "/api/v1/x/fake/none", 401, "AUTHENTICATION_REQUIRED", ""},
		{"member reads", member, "GET", "/api/v1/x/fake/items", 200, "", "member"},
		{"member writes a read route", member, "POST", "/api/v1/x/fake/items", 200, "", "member"},
		{"member on admin", member, "POST", "/api/v1/x/fake/admin", 403, "ADMINISTRATOR_REQUIRED", ""},
		{"member on admin read", member, "GET", "/api/v1/x/fake/admin/7", 403, "ADMINISTRATOR_REQUIRED", ""},
		{"admin session", rig.session, "POST", "/api/v1/x/fake/admin", 200, "", "admin"},
		{"read token reads", read, "GET", "/api/v1/x/fake/items", 200, "", "admin"},
		{"read token posts", read, "POST", "/api/v1/x/fake/items", 403, "PLATFORM_TOKEN_READ_ONLY", ""},
		{"read token on admin write", read, "POST", "/api/v1/x/fake/admin", 403, "PLATFORM_TOKEN_READ_ONLY", ""},
		{"write token on admin", write, "POST", "/api/v1/x/fake/admin", 200, "", "admin"},
		{"write token on session-only", write, "POST", "/api/v1/x/fake/session", 403, "PLATFORM_TOKEN_NOT_ALLOWED", ""},
		{"read token on session-only", read, "POST", "/api/v1/x/fake/session", 403, "PLATFORM_TOKEN_NOT_ALLOWED", ""},
		{"member on session-only", member, "POST", "/api/v1/x/fake/session", 403, "ADMINISTRATOR_REQUIRED", ""},
		{"admin session on session-only", rig.session, "POST", "/api/v1/x/fake/session", 200, "", "admin"},
		{"unregistered method", rig.session, "DELETE", "/api/v1/x/fake/items", 405, "METHOD_NOT_ALLOWED", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := rig.do(tc.bearer, tc.method, tc.path, "")
			var body map[string]any
			_ = json.Unmarshal(rec.Body.Bytes(), &body)
			if rec.Code != tc.status {
				t.Fatalf("%s %s = %d %s, want %d", tc.method, tc.path, rec.Code, rec.Body, tc.status)
			}
			if tc.code != "" && body["code"] != tc.code {
				t.Fatalf("code = %v, want %s (%s)", body["code"], tc.code, rec.Body)
			}
			if tc.user != "" && (body["user"] != tc.user || body["claims"] != true) {
				t.Fatalf("handler saw %v, want claims for %s", body, tc.user)
			}
		})
	}
	// A browser session's cross-site write is refused before the handler.
	req := httptestRequest(http.MethodPost, "/api/v1/x/fake/items", rig.session)
	req.Header.Set("Origin", "https://elsewhere.example")
	rec := serve(rig.h, req)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), `"code":"MANAGEMENT_ORIGIN_REJECTED"`) {
		t.Fatalf("cross-origin write = %d %s", rec.Code, rec.Body)
	}
}

func TestExtensionRoutesFollowTheFeatureGate(t *testing.T) {
	src := &switchSource{}
	gate := mustGate(t, src, featuregate.Feature{Key: "fake.things"}, featuregate.Feature{Key: "fake.infra", Presentation: featuregate.Conceal})
	ext := fakeExtension{name: "fake", routes: []RouteSpec{
		{Method: http.MethodGet, Pattern: "/x/fake/things", Feature: "fake.things"},
		{Method: http.MethodPost, Pattern: "/x/fake/things", Feature: "fake.things"},
		{Method: http.MethodDelete, Pattern: "/x/fake/things/{id}", Feature: "fake.things", WriteWhenReadOnly: true},
		{Method: http.MethodGet, Pattern: "/x/fake/infra", Feature: "fake.infra"},
		{Method: http.MethodGet, Pattern: "/x/fake/open", Community: true},
	}}
	rig := newTokenRig(t, func(d *Deps) { d.Extensions = ExtensionDeps{Extensions: []Extension{ext}, Features: gate} })
	type call struct {
		method, path string
		status       int
		code         string
	}
	for _, tc := range []struct {
		name  string
		av    map[featuregate.Key]featuregate.Availability
		calls []call
	}{
		{"full", map[featuregate.Key]featuregate.Availability{"fake.things": {Access: featuregate.Full}, "fake.infra": {Access: featuregate.Full}}, []call{
			{"GET", "/api/v1/x/fake/things", 200, ""}, {"POST", "/api/v1/x/fake/things", 200, ""}, {"DELETE", "/api/v1/x/fake/things/1", 200, ""}, {"GET", "/api/v1/x/fake/infra", 200, ""},
		}},
		{"read-only", map[featuregate.Key]featuregate.Availability{"fake.things": {Access: featuregate.ReadOnly}}, []call{
			{"GET", "/api/v1/x/fake/things", 200, ""}, {"POST", "/api/v1/x/fake/things", 403, "FEATURE_READ_ONLY"}, {"DELETE", "/api/v1/x/fake/things/1", 200, ""},
		}},
		{"locked", map[featuregate.Key]featuregate.Availability{"fake.things": {Access: featuregate.Locked}, "fake.infra": {Access: featuregate.Locked}}, []call{
			{"GET", "/api/v1/x/fake/things", 403, "FEATURE_UNAVAILABLE"}, {"POST", "/api/v1/x/fake/things", 403, "FEATURE_UNAVAILABLE"}, {"DELETE", "/api/v1/x/fake/things/1", 403, "FEATURE_UNAVAILABLE"},
			// A concealed feature that is locked does not exist for the caller.
			{"GET", "/api/v1/x/fake/infra", 404, "FEATURE_UNAVAILABLE"},
		}},
		{"hidden", map[featuregate.Key]featuregate.Availability{}, []call{
			{"GET", "/api/v1/x/fake/things", 404, "FEATURE_UNAVAILABLE"}, {"POST", "/api/v1/x/fake/things", 404, "FEATURE_UNAVAILABLE"}, {"GET", "/api/v1/x/fake/open", 200, ""},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src.set(tc.av)
			for _, c := range tc.calls {
				rec := rig.do(rig.session, c.method, c.path, "")
				if rec.Code != c.status || (c.code != "" && !strings.Contains(rec.Body.String(), `"code":"`+c.code+`"`)) {
					t.Fatalf("%s %s = %d %s, want %d %s", c.method, c.path, rec.Code, rec.Body, c.status, c.code)
				}
				if c.code != "" && !strings.Contains(rec.Body.String(), `"params":{"feature":`) {
					t.Fatalf("%s %s refusal names no feature: %s", c.method, c.path, rec.Body)
				}
			}
		})
	}
	// Authentication still comes first: an anonymous caller learns nothing
	// about the feature.
	src.set(map[featuregate.Key]featuregate.Availability{})
	if rec := rig.do("", "GET", "/api/v1/x/fake/things", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous on a hidden feature = %d %s", rec.Code, rec.Body)
	}
}

func TestExtensionFeatureFailsClosedWithoutASource(t *testing.T) {
	gate := mustGate(t, nil, featuregate.Feature{Key: "fake.things"})
	ext := fakeExtension{name: "fake", routes: []RouteSpec{{Method: http.MethodGet, Pattern: "/x/fake/things", Feature: "fake.things"}}}
	rig := newTokenRig(t, func(d *Deps) { d.Extensions = ExtensionDeps{Extensions: []Extension{ext}, Features: gate} })
	rec := rig.do(rig.session, "GET", "/api/v1/x/fake/things", "")
	if rec.Code != http.StatusForbidden || strings.TrimSpace(rec.Body.String()) != `{"code":"FEATURE_PROVIDER_ABSENT","error":"nothing on this server answers for feature fake.things","params":{"feature":"fake.things"}}` {
		t.Fatalf("no source = %d %s", rec.Code, rec.Body)
	}
}

func TestAssistantReadsOnlyMarkedExtensionRoutes(t *testing.T) {
	ext := fakeExtension{name: "fake", routes: []RouteSpec{
		{Method: http.MethodGet, Pattern: "/x/fake/public", Community: true, AIReadable: true},
		{Method: http.MethodGet, Pattern: "/x/fake/secret", Community: true},
	}}
	var deps Deps
	rig := newTokenRig(t, func(d *Deps) {
		d.Extensions = ExtensionDeps{Extensions: []Extension{ext}}
		deps = *d
	})
	a := &apiHandlers{d: deps, mux: rigMux(t, rig)}
	ask := httptestRequest(http.MethodPost, "/api/v1/ai/ask", rig.session)
	reader := a.apiReaderFor(ask)
	if status, body, err := reader.Get(ask.Context(), "/api/v1/x/fake/public", nil); err != nil || status != 200 || !strings.Contains(string(body), `"user":"admin"`) {
		t.Fatalf("readable route: %d %s %v", status, body, err)
	}
	if _, _, err := reader.Get(ask.Context(), "/api/v1/x/fake/secret", nil); err == nil || !strings.Contains(err.Error(), "not a readable API route") {
		t.Fatalf("unmarked route read: %v", err)
	}
	routes := reader.Routes()
	if !slices.Contains(routes, "/api/v1/x/fake/public") || slices.Contains(routes, "/api/v1/x/fake/secret") || !slices.Contains(routes, "/api/v1/nodes") {
		t.Fatalf("listed routes miss the rule: %v", routes)
	}
	// The route refuses an assistant read on its own as well, for a reader
	// that skipped the list.
	bare := &aiReader{mux: a.mux, header: http.Header{"Authorization": {"Bearer " + rig.session}}}
	if status, body, err := bare.Get(ask.Context(), "/api/v1/x/fake/secret", nil); err != nil || status != http.StatusNotFound {
		t.Fatalf("route-level refusal: %d %s %v", status, body, err)
	}
	// A browser still reads it.
	if rec := rig.do(rig.session, "GET", "/api/v1/x/fake/secret", ""); rec.Code != http.StatusOK {
		t.Fatalf("console read = %d %s", rec.Code, rec.Body)
	}

	// MCP reads through the same reader.
	rig.do(rig.session, "PUT", "/api/v1/mcp/settings", `{"enabled":true}`)
	read := rig.mint("read")
	call := func(path string) (string, bool) {
		out := rig.rpc(read, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"call_api","arguments":{"path":"`+path+`"}}}`)
		res, _ := out["result"].(map[string]any)
		if res == nil {
			t.Fatalf("%s: %v", path, out)
		}
		return res["content"].([]any)[0].(map[string]any)["text"].(string), res["isError"].(bool)
	}
	if text, isErr := call("/api/v1/x/fake/secret"); !isErr || !strings.Contains(text, "not a readable API route") {
		t.Fatalf("MCP unmarked read: %v %s", isErr, text)
	}
	if text, isErr := call("/api/v1/x/fake/public"); isErr || !strings.Contains(text, "admin") {
		t.Fatalf("MCP marked read: %v %s", isErr, text)
	}
}

func TestRouteInventoryNamesExtensionRoutes(t *testing.T) {
	gate := mustGate(t, nil, featuregate.Feature{Key: "fake.things"})
	ext := fakeExtension{name: "fake", routes: []RouteSpec{
		{Method: http.MethodGet, Pattern: "/x/fake/things", Feature: "fake.things", Action: "fake.list"},
		{Method: http.MethodPost, Pattern: "/x/fake/things", Access: AccessAdmin, Feature: "fake.things", Action: "fake.create"},
		{Method: http.MethodPost, Pattern: "/x/fake/session", Access: AccessAdminSession, Community: true},
		{Method: http.MethodGet, Pattern: "/x/fake/start", Access: AccessPublic, Feature: "fake.things"},
		{Method: http.MethodPost, Pattern: "/x/fake/acs", Access: AccessPublic, Community: true, SignIn: true},
	}}
	got, err := RouteInventory(Router(Deps{Extensions: ExtensionDeps{Extensions: []Extension{ext}, Features: gate}}))
	if err != nil {
		t.Fatal(err)
	}
	core, err := RouteInventory(Router(Deps{}))
	if err != nil {
		t.Fatal(err)
	}
	var added []string
	for _, line := range got {
		if !slices.Contains(core, line) {
			added = append(added, line)
		}
	}
	want := []string{
		"GET /api/v1/x/fake/start [access=public] [feature=fake.things]",
		"GET /api/v1/x/fake/things [access=read] [action=fake.list] [feature=fake.things]",
		"POST /api/v1/x/fake/acs [access=public] [community] [sign-in]",
		"POST /api/v1/x/fake/session [access=admin-session] [community]",
		"POST /api/v1/x/fake/things [access=admin] [action=fake.create] [feature=fake.things]",
	}
	if !slices.Equal(added, want) || len(got) != len(core)+len(want) {
		t.Fatalf("extension inventory = %v, want %v", added, want)
	}
	// An empty extension set leaves the inventory exactly the core's.
	empty, err := RouteInventory(Router(Deps{Extensions: ExtensionDeps{Features: gate, Banners: []BannerSource{}}}))
	if err != nil || !slices.Equal(empty, core) {
		t.Fatalf("empty extensions changed the inventory: %v", err)
	}
	// A route that names neither a feature nor Community fails the inventory,
	// even when registered past Router's own check.
	mux := chiMuxWith(http.MethodGet, "/api/v1/x/fake/x", &extensionEndpoint{spec: RouteSpec{Method: http.MethodGet, Pattern: "/x/fake/x"}, next: http.NotFoundHandler()})
	if _, err := RouteInventory(mux); err == nil || !strings.Contains(err.Error(), "names neither a feature nor Community") {
		t.Fatalf("unmarked route: %v", err)
	}
	if _, err := RouteInventory(http.NotFoundHandler()); err == nil {
		t.Fatal("a non-chi handler has no inventory")
	}
}

func httptestRequest(method, path, bearer string) *http.Request {
	req := httptest.NewRequest(method, path, nil)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	return req
}

func serve(h http.Handler, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func rigMux(t *testing.T, rig *tokenRig) *chi.Mux {
	t.Helper()
	mux, ok := rig.h.(*chi.Mux)
	if !ok {
		t.Fatalf("router is %T", rig.h)
	}
	return mux
}

func chiMuxWith(method, pattern string, h http.Handler) *chi.Mux {
	mux := chi.NewRouter()
	mux.Method(method, pattern, h)
	return mux
}

// Extension namespaces live under /api/v1/x/, where the core serves nothing,
// so a module may take a core path segment as its name: "nodes" mounts at
// /api/v1/x/nodes/ and the core's /api/v1/nodes keeps answering as before.
func TestExtensionNamedAfterACoreSegmentStaysUnderTheExtensionRoot(t *testing.T) {
	core, err := RouteInventory(Router(Deps{}))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range core {
		if _, path, _ := strings.Cut(line, " "); path == "/api/v1"+extensionRoot || strings.HasPrefix(path, "/api/v1"+extensionRoot+"/") {
			t.Fatalf("the core serves %s under the extension root", line)
		}
	}
	ext := fakeExtension{name: "nodes", routes: []RouteSpec{{Method: http.MethodGet, Pattern: "/x/nodes", Community: true}}}
	rig := newTokenRig(t, func(d *Deps) { d.Extensions = ExtensionDeps{Extensions: []Extension{ext}} })
	if rec := rig.do(rig.session, "GET", "/api/v1/x/nodes", ""); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"route":"GET /x/nodes"`) {
		t.Fatalf("extension route = %d %s", rec.Code, rec.Body)
	}
	if rec := rig.do(rig.session, "GET", "/api/v1/nodes", ""); rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), `"route"`) {
		t.Fatalf("core /api/v1/nodes = %d %s, want the core list", rec.Code, rec.Body)
	}
}
