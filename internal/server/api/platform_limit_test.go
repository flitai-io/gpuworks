package api

import (
	"encoding/json"
	"net/http"
	"slices"
	"sort"
	"strings"
	"sync/atomic"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/featuregate"
	"gpuworks.ai/gpuworks/internal/server/messages"
)

// switchLimiter is a PlatformLimiter a test switches between answers.
type switchLimiter struct{ cur atomic.Pointer[PlatformLimit] }

func newSwitchLimiter(access featuregate.Access) *switchLimiter {
	l := &switchLimiter{}
	l.set(PlatformLimit{Access: access})
	return l
}

func (l *switchLimiter) set(limit PlatformLimit)      { l.cur.Store(&limit) }
func (l *switchLimiter) PlatformLimit() PlatformLimit { return *l.cur.Load() }

// fullRouter is the router with every optional mount present (the data
// plane, the console's entry to it, the console's files, the install script
// and the installed packages' routes), so the inventory holds every route a
// limit can meet.
func fullRouter(extra ...Extension) http.Handler {
	nop := func(http.ResponseWriter, *http.Request) {}
	packages := []DynamicRoute{
		{Plugin: "sample", Method: http.MethodGet, Pattern: "/things", Access: DynamicMember},
		{Plugin: "sample", Method: http.MethodPost, Pattern: "/things", Access: DynamicAdmin},
	}
	dynamic := &DynamicExtension{
		Match:  func(string, string) (*DynamicRoute, DynamicMatch) { return nil, DynamicMiss },
		Routes: func() []DynamicRoute { return packages },
	}
	return Router(Deps{AgentConnect: nop, InstallSh: nop, ProxyV1: http.HandlerFunc(nop), PlaygroundV1: http.HandlerFunc(nop), UIHandler: http.HandlerFunc(nop),
		Extensions: ExtensionDeps{Extensions: extra, Dynamic: dynamic}})
}

// inventoryRoutes are the method and pattern of every route of h.
func inventoryRoutes(t *testing.T, h http.Handler) [][2]string {
	t.Helper()
	lines, err := RouteInventory(h)
	if err != nil {
		t.Fatal(err)
	}
	var out [][2]string
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			t.Fatalf("inventory line %q has no method and pattern", line)
		}
		out = append(out, [2]string{fields[0], fields[1]})
	}
	return out
}

// A route nobody classified is refused under any limit; this fails until a
// new route outside /api/v1 is named in platform_limit.go, so adding one is
// a decision and never an accident.
func TestEveryRouteIsClassifiedForAPlatformLimit(t *testing.T) {
	seen := map[string]bool{}
	for _, route := range inventoryRoutes(t, fullRouter()) {
		method, pattern := route[0], route[1]
		seen[method+" "+pattern] = true
		// A mount that takes every method is listed once per method.
		if _, classified := classifyPlatformRoute(method, pattern, pattern, ""); !classified {
			t.Errorf("%s %s is not classified for a platform limit: name it in platform_limit.go", method, pattern)
		}
	}
	for _, table := range []map[string]bool{platformRecoveryRoutes, platformRefusedRecovery, platformChangingReads, platformReachingReads, platformReadingPosts} {
		for key := range table {
			if !seen[key] {
				t.Errorf("platform_limit.go names %q, which is not a route", key)
			}
		}
	}
	for _, prefix := range platformRecoveryPrefixes {
		found := false
		for key := range seen {
			pattern := key[strings.Index(key, " ")+1:]
			if pattern == strings.TrimSuffix(prefix, "/") || strings.HasPrefix(pattern, strings.TrimSuffix(prefix, "/")+"/") {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("platform_limit.go names the subtree %q, which holds no route", prefix)
		}
	}
}

// A GET that acts is refused like a change under any limit, and a GET that
// reaches outside the server without changing anything is served under any
// limit: each is named, none is decided by its method alone.
func TestReadsThatActAndReadsThatReachOutside(t *testing.T) {
	for _, tc := range []struct {
		route string
		class platformClass
		why   string
	}{
		{"GET /install.sh", platformChange, "hands out what enrolls a new machine"},
		{"GET /kube/manifest.yaml", platformChange, "hands out what enrolls a cluster's machines"},
		{"GET /slurm/setup.sh", platformChange, "hands out what enrolls a cluster's machines"},
		{"GET /api/v1/kube/clusters/{uuid}/manifest", platformChange, "carries a cluster's enrollment"},
		{"GET /api/v1/slurm/clusters/{uuid}/setup", platformChange, "carries a cluster's enrollment"},
		{"GET /api/v1/slurm/clusters/{uuid}/script", platformChange, "carries a cluster's enrollment"},
		{"GET /api/v1/nodes/{uuid}/terminal", platformChange, "opens an interactive session on a machine"},
		{"GET /api/v1/nodes/{uuid}/fs", platformRead, "lists a machine's files"},
		{"GET /api/v1/images/nodes/{uuid}/runtime-images", platformRead, "lists a machine's images"},
		{"GET /api/v1/instances/{uuid}/logs", platformRead, "streams an instance's log"},
		{"GET /api/v1/storages/{id}/files", platformRead, "lists a storage's files"},
		{"GET /api/v1/storages/{id}/files/download", platformRead, "downloads a storage's file"},
		{"GET /api/v1/batches/{id}/files/{kind}", platformRead, "downloads a batch's file"},
		{"GET /api/v1/providers/{id}/instances", platformRead, "lists a cloud account's instances"},
		{"GET /api/v1/providers/{id}/clusters", platformRead, "lists a cloud account's clusters"},
		{"GET /api/v1/providers/{id}/hpc-clusters", platformRead, "lists a cloud account's clusters"},
		{"GET /api/v1/images/community/browse", platformRead, "lists a registry's tags"},
		{"GET /api/v1/ssh-key", platformRead, "shows the server's public key"},
		{"GET /api/v1/nodes/{uuid}/ssh", platformRead, "shows a node's SSH settings without secrets"},
	} {
		method, pattern, _ := strings.Cut(tc.route, " ")
		class, classified := classifyPlatformRoute(method, pattern, pattern, "")
		if !classified || class != tc.class {
			t.Errorf("%s (%s) is class %d, classified %t; want class %d", tc.route, tc.why, class, classified, tc.class)
		}
		for _, access := range []featuregate.Access{featuregate.ReadOnly, featuregate.Locked} {
			if got, want := class.allows(access), tc.class == platformRead; got != want {
				t.Errorf("%s under %s: served %t, want %t", tc.route, access, got, want)
			}
		}
	}
	if n := len(platformChangingReads) + len(platformReachingReads); n != 20 {
		t.Errorf("platform_limit.go names %d reads that act or reach outside; this table has 20: add the new one here", n)
	}
}

// What a limited server serves is this list, route by route: every read,
// the data plane, the recovery surface, and the GETs refused like a change.
// A new route that reads, or a route whose class changes, differs from it;
// the list's own comment says what to check before adding one.
func TestPlatformLimitRoutesAreTheKnownOnes(t *testing.T) {
	var got []string
	for _, route := range inventoryRoutes(t, fullRouter()) {
		method, pattern := route[0], route[1]
		class, _ := classifyPlatformRoute(method, pattern, pattern, "")
		byMethod := platformChange
		switch method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			byMethod = platformRead
		}
		if class == platformChange && byMethod == platformChange {
			continue
		}
		name := map[platformClass]string{platformChange: "change", platformRead: "read", platformServe: "serve", platformRecovery: "recovery"}[class]
		got = append(got, name+" "+method+" "+pattern)
	}
	sort.Strings(got)
	got = slices.Compact(got)
	want := strings.Split(strings.TrimSpace(platformLimitRoutes), "\n")
	if !slices.Equal(got, want) {
		t.Fatalf("what a limited server serves changed; update platform_limit_routes_test.go deliberately\ngot:\n%s", strings.Join(got, "\n"))
	}
}

// A POST is served under a limit only when it stores nothing and changes
// nothing on a node (twelve do); one that starts work, stores something or
// acts on a node is refused. The
// server's own upgrade stays possible under any limit, bringing a new
// package in does not.
func TestWritesThatOnlyReadAndWritesThatBringCodeIn(t *testing.T) {
	if n := len(platformReadingPosts); n != 12 {
		t.Errorf("platform_limit.go names %d reading POSTs; this table has 12: add the new one here", n)
	}
	for _, tc := range []struct {
		route string
		class platformClass
		why   string
	}{
		{"POST /api/v1/mcp", platformRead, "serves read-only tools"},
		{"POST /api/v1/ai/ask", platformRead, "answers from what it reads"},
		{"POST /api/v1/models/local/preview", platformRead, "previews a model that is not registered"},
		{"POST /api/v1/models/preview-url", platformRead, "previews a model that is not registered"},
		{"POST /api/v1/evaluate", platformRead, "previews a deployment that is not created"},
		{"POST /api/v1/benchmarks/models", platformRead, "lists what a benchmark could target"},
		{"POST /api/v1/providers/test", platformRead, "tests credentials that are not saved"},
		{"POST /api/v1/storages/probe", platformChange, "mounts on a node to try a storage"},
		{"POST /api/v1/deployments/preflight", platformChange, "starts measurement listeners on nodes"},
		{"POST /api/v1/notices/read", platformRead, "records which notices this user has read"},
		{"POST /api/v1/deployments", platformChange, "creates a deployment"},
		{"POST /api/v1/benchmarks", platformChange, "starts a benchmark"},
		{"POST /api/v1/ai/configs/test", platformRead, "tests an assistant model that is not saved"},
		{"POST /api/v1/benchmarks/probe", platformRead, "asks a node whether a benchmark's target answers"},
		{"POST /api/v1/nodes/precheck", platformRead, "checks a machine's SSH address before it is added"},
		{"POST /api/v1/kube/precheck", platformRead, "renders a script for a cluster that is not connected; nothing is stored"},
		{"POST /api/v1/nodes", platformChange, "adds a node"},
		{"POST /api/v1/updates/server", platformRecovery, "a release may be what makes the limit liftable"},
		{"PUT /api/v1/updates/channel", platformRecovery, "chooses where that release comes from"},
		{"POST /api/v1/plugins/{id}/disable", platformRecovery, "stops an installed package"},
		{"POST /api/v1/plugins/install", platformChange, "brings a new package in"},
		{"POST /api/v1/plugins/{id}/upgrade", platformChange, "brings a new package version in"},
		{"POST /api/v1/kube/enroll", platformRecovery, "gives a joined cluster's machine its identity again; a new one is refused at the insert"},
		{"POST /api/v1/slurm/enroll", platformRecovery, "gives a joined cluster's machine its identity again; a new one is refused at the insert"},
	} {
		method, pattern, _ := strings.Cut(tc.route, " ")
		if class, _ := classifyPlatformRoute(method, pattern, pattern, ""); class != tc.class {
			t.Errorf("%s (%s) is class %d, want %d", tc.route, tc.why, class, tc.class)
		}
	}
}

func TestPlatformLimitServesByClass(t *testing.T) {
	ext := fakeExtension{name: "owner", routes: []RouteSpec{
		{Method: http.MethodGet, Pattern: "/x/owner/state", Community: true},
		{Method: http.MethodPost, Pattern: "/x/owner/import", Access: AccessAdmin, Community: true},
	}}
	other := fakeExtension{name: "other", routes: []RouteSpec{{Method: http.MethodGet, Pattern: "/x/other/things", Community: true}}}
	limiter := newSwitchLimiter(featuregate.Full)
	served := 0
	rig := newTokenRig(t, func(d *Deps) {
		d.Extensions = ExtensionDeps{Extensions: []Extension{ext, other}, Platform: limiter, PlatformOwner: "owner"}
		d.ProxyV1 = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { served++; w.WriteHeader(http.StatusNoContent) })
		d.PlaygroundV1 = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
		d.AgentConnect = func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }
		d.UIHandler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusAccepted) })
	})
	t.Cleanup(func() { _ = rig.st.Close() })
	read := rig.mint("read")

	const refused = http.StatusForbidden
	for _, tc := range []struct {
		name, bearer, method, path string
		full, readOnly, locked     int
	}{
		{"a read", rig.session, "GET", "/api/v1/nodes", 200, 200, 200},
		{"a read with a read token", read, "GET", "/api/v1/nodes", 200, 200, 200},
		{"a read without a session", "", "GET", "/api/v1/nodes", 401, 401, 401},
		{"a change", rig.session, "PUT", "/api/v1/nodes/n1/labels", 400, refused, refused},
		// A caller who has not signed in learns nothing from a limit: the
		// answer is the one an unlimited server gives.
		{"a change without a session", "", "PUT", "/api/v1/nodes/n1/labels", 401, 401, 401},
		{"a change with a token nobody issued", "gwp_unknown", "PUT", "/api/v1/nodes/n1/labels", 401, 401, 401},
		{"a read that opens a session on a node", rig.session, "GET", "/api/v1/nodes/n1/terminal", 503, refused, refused},
		{"the data plane", "", "POST", "/v1/chat/completions", 204, 204, refused},
		{"the console's entry to the data plane", rig.session, "POST", "/api/v1/playground/v1/chat/completions", 204, 204, refused},
		{"a stream on the console's entry to the data plane", rig.session, "GET", "/api/v1/playground/v1/realtime", 204, 204, refused},
		{"the console's entry to the data plane without a session", "", "POST", "/api/v1/playground/v1/chat/completions", 401, 401, 401},
		{"a joined machine's connection", "", "GET", "/api/v1/agent/connect", 204, 204, 204},
		{"who am I", rig.session, "GET", "/api/v1/auth/me", 200, 200, 200},
		{"the console shell", rig.session, "GET", "/api/v1/console/manifest", 200, 200, 200},
		{"signing in", "", "POST", "/api/v1/auth/login", 400, 400, 400},
		{"the limiting extension's read", rig.session, "GET", "/api/v1/x/owner/state", 200, 200, 200},
		{"the limiting extension's change", rig.session, "POST", "/api/v1/x/owner/import", 200, 200, 200},
		{"the limiting extension's unknown route", rig.session, "GET", "/api/v1/x/owner/nothing", 404, 404, 404},
		{"the limiting extension's unknown method", rig.session, "DELETE", "/api/v1/x/owner/state", 405, 405, 405},
		{"another extension's read", rig.session, "GET", "/api/v1/x/other/things", 200, 200, 200},
		{"a core route whose path names the limiting extension", rig.session, "GET", "/api/v1/nodes/owner", 404, 404, 404},
		{"a path that climbs out of the limiting extension", rig.session, "GET", "/api/v1/x/owner/../other/things", 404, 404, 404},
		{"the console's page", "", "GET", "/", 202, 202, 202},
		{"a console page by its address", "", "GET", "/x/owner", 202, 202, 202},
		{"a console file", "", "HEAD", "/assets/index.js", 202, 202, 202},
		{"an agent download", "", "GET", "/dl/gpuworks-agent-linux-amd64", 202, 202, 202},
		{"a write to the catch-all", "", "POST", "/dl/gpuworks-agent-linux-amd64", 202, refused, refused},
		{"no such route", rig.session, "GET", "/api/v1/does-not-exist", 404, 404, 404},
		{"no such method", rig.session, "TRACE", "/api/v1/nodes", 405, 405, 405},
	} {
		for _, state := range []struct {
			access featuregate.Access
			want   int
		}{{featuregate.Full, tc.full}, {featuregate.ReadOnly, tc.readOnly}, {featuregate.Locked, tc.locked}, {featuregate.Hidden, tc.locked}} {
			limiter.set(PlatformLimit{Access: state.access})
			if rec := rig.do(tc.bearer, tc.method, tc.path, ""); rec.Code != state.want {
				t.Errorf("%s under %s: %s %s = %d %s, want %d", tc.name, state.access, tc.method, tc.path, rec.Code, rec.Body.String(), state.want)
			}
		}
	}
	if served == 0 {
		t.Fatal("the data plane was never reached")
	}
}

// The refusal on the wire: the core's own codes, and the limiter's notice
// in their place when it gives one.
func TestPlatformLimitRefusalWire(t *testing.T) {
	limiter := newSwitchLimiter(featuregate.Locked)
	rig := newTokenRig(t, func(d *Deps) {
		d.Extensions = ExtensionDeps{Platform: limiter}
		d.ProxyV1 = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	})
	t.Cleanup(func() { _ = rig.st.Close() })
	body := func(method, path string) string {
		rec := rig.do(rig.session, method, path, "")
		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s %s = %d %s", method, path, rec.Code, rec.Body.String())
		}
		return strings.TrimSpace(rec.Body.String())
	}
	if got, want := body("POST", "/api/v1/nodes"), `{"code":"PLATFORM_LOCKED","error":"this server is locked; it can be viewed, and changes and inference are refused until it is unlocked"}`; got != want {
		t.Errorf("locked refusal = %s, want %s", got, want)
	}
	// The data plane's clients read the gateway's error shape.
	limiter.set(PlatformLimit{Access: featuregate.Locked, Notice: messages.NewProductError("ACME_REQUIRED", "the acme term has not begun", map[string]any{"days": 3})})
	if got, want := body("POST", "/v1/chat/completions"), `{"error":{"message":"the acme term has not begun","type":"permission_error","code":"ACME_REQUIRED","param":null,"params":{"days":3}}}`; got != want {
		t.Errorf("data plane refusal = %s, want %s", got, want)
	}
	limiter.set(PlatformLimit{Access: featuregate.ReadOnly})
	if got, want := body("POST", "/api/v1/nodes"), `{"code":"PLATFORM_READ_ONLY","error":"this server is read-only; what runs keeps running and can be viewed, changes are refused"}`; got != want {
		t.Errorf("read-only refusal = %s, want %s", got, want)
	}
	limiter.set(PlatformLimit{Access: featuregate.ReadOnly, Notice: messages.NewProductError("ACME_LAPSED", "the acme term ended", map[string]any{"days": 3})})
	if got, want := body("POST", "/api/v1/nodes"), `{"code":"ACME_LAPSED","error":"the acme term ended","params":{"days":3}}`; got != want {
		t.Errorf("notice refusal = %s, want %s", got, want)
	}
}

func TestConsoleManifestShowsThePlatformLimit(t *testing.T) {
	limiter := newSwitchLimiter(featuregate.Full)
	var signer *auth.TokenSigner
	rig := newTokenRig(t, func(d *Deps) { d.Extensions, signer = ExtensionDeps{Platform: limiter}, d.AuthSigner })
	t.Cleanup(func() { _ = rig.st.Close() })
	member, err := managementSessionTestToken(t, rig.st, signer, auth.Claims{Username: "member", Role: "user"})
	if err != nil {
		t.Fatal(err)
	}
	bearer := rig.session
	platform := func() (string, bool) {
		rec := rig.do(bearer, "GET", "/api/v1/console/manifest", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("manifest = %d %s", rec.Code, rec.Body.String())
		}
		var out map[string]json.RawMessage
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		raw, ok := out["platform"]
		return string(raw), ok
	}
	if raw, ok := platform(); ok {
		t.Fatalf("an unlimited server's manifest names a platform limit: %s", raw)
	}
	limiter.set(PlatformLimit{Access: featuregate.Hidden, Link: "/x/acme", Action: messages.NewProductError("ACME_RENEW", "renew", nil)})
	if got, _ := platform(); got != `{"access":"locked","link":"/x/acme","action":{"code":"ACME_RENEW","message":"renew"},"message":{"code":"PLATFORM_LOCKED","message":"this server is locked; it can be viewed, and changes and inference are refused until it is unlocked"}}` {
		t.Errorf("locked platform = %s", got)
	}
	// The page that lifts the limit is an administrator's: anyone else is
	// told of the limit and offered nothing to open.
	bearer = member
	if got, _ := platform(); got != `{"access":"locked","message":{"code":"PLATFORM_LOCKED","message":"this server is locked; it can be viewed, and changes and inference are refused until it is unlocked"}}` {
		t.Errorf("locked platform for a member = %s", got)
	}
	bearer = rig.session
	// An action without a link names nothing and is left out.
	limiter.set(PlatformLimit{Access: featuregate.ReadOnly, Action: messages.NewProductError("ACME_RENEW", "renew", nil),
		Notice: messages.NewProductError("ACME_LAPSED", "the acme term ended", map[string]any{"days": 3})})
	if got, _ := platform(); got != `{"access":"read_only","message":{"code":"ACME_LAPSED","params":{"days":3},"message":"the acme term ended"}}` {
		t.Errorf("read-only platform = %s", got)
	}
}

// Without a limiter, and under one that answers Full, the guard resolves
// no route: a nil router would panic if it did.
func TestPlatformGuardAddsNothingWhenNothingIsLimited(t *testing.T) {
	for name, limiter := range map[string]PlatformLimiter{"no limiter": nil, "full": newSwitchLimiter(featuregate.Full)} {
		a := &apiHandlers{}
		a.d.Extensions.Platform = limiter
		reached := 0
		next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached++ })
		a.platformGuard(nil)(next).ServeHTTP(nil, httptestRequest(http.MethodGet, "/api/v1/nodes", ""))
		if reached != 1 {
			t.Errorf("%s: the handler was reached %d times", name, reached)
		}
	}
}
