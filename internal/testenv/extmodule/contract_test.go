package extmodule

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/server/app"
	"gpuworks.ai/gpuworks/internal/server/featuregate"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/testenv"
)

func section(t *testing.T, text string) yaml.Node {
	t.Helper()
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(text), &doc); err != nil {
		t.Fatal(err)
	}
	return *doc.Content[0]
}

func send(t *testing.T, c *testenv.APIClient, method, path, body string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, c.BaseURL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out
}

// A module reaches the server only through its Host: every method answers
// from the running server, registrations take effect, modules start in
// order, and a registration after Init is refused.
func TestAModuleReachesEveryHostMethod(t *testing.T) {
	var order []string
	first := &fakeModule{name: "fake", source: newFakeSource(featuregate.Full), order: &order}
	second := &fakeModule{name: "other", order: &order}
	e := testenv.NewWithExtensions(t, app.Extensions{Modules: []app.Module{first, second}},
		map[string]yaml.Node{"fake": section(t, "greeting: hello\n")})

	if got := strings.Join(order, ","); got != "fake,other" {
		t.Fatalf("Init order = %s, want fake,other", got)
	}
	if got := strings.Join(first.calls, ","); got != "log,bus,section,tables,derive,registered" {
		t.Fatalf("host calls = %s", got)
	}
	if first.cfg.Greeting != "hello" || second.cfg.Greeting != "" {
		t.Fatalf("sections: fake=%+v other=%+v; each module reads only its own", first.cfg, second.cfg)
	}
	if first.dataDir != e.DataDir() || first.installID == "" || first.installID != second.installID {
		t.Fatalf("data dir %q (want %q), install ids %q/%q", first.dataDir, e.DataDir(), first.installID, second.installID)
	}
	if len(first.keys["signing"]) != 32 || bytes.Equal(first.keys["signing"], first.keys["sealing"]) ||
		bytes.Equal(first.keys["signing"], second.keys["signing"]) {
		t.Fatal("derived keys must be 32 bytes and differ by purpose and by module")
	}
	again, err := first.host.DeriveKey("signing")
	if err != nil || !bytes.Equal(again, first.keys["signing"]) {
		t.Fatalf("a derived key is stable: %v", err)
	}
	if _, err := first.host.DeriveKey("Bad Purpose"); err == nil {
		t.Fatal("a purpose outside the grammar is refused")
	}
	for _, m := range []*fakeModule{first, second} {
		var notes []string
		if err := m.tables.Read(func(db *gorm.DB) error {
			return db.Raw("SELECT note FROM ee_" + m.name + "_records").Scan(&notes).Error
		}); err != nil || len(notes) != 1 || notes[0] != "from init" {
			t.Fatalf("%s tables: notes=%v err=%v", m.name, notes, err)
		}
		if m.tables.Namespace() != m.name || m.tables.TablePrefix() != "ee_"+m.name+"_" {
			t.Fatalf("%s tables are %s/%s", m.name, m.tables.Namespace(), m.tables.TablePrefix())
		}
		if v, err := e.Store().NamespaceVersion(m.name); err != nil || v != 1 {
			t.Fatalf("%s namespace version = %d, %v", m.name, v, err)
		}
	}

	api := e.API(t)
	if status, body := send(t, api, http.MethodGet, "/api/v1/x/fake/ping", ""); status != http.StatusOK || string(body) != `{"pong":true}` {
		t.Fatalf("mounted community route = %d %s", status, body)
	}
	status, body := send(t, api, http.MethodGet, "/api/v1/console/manifest", "")
	want := `{"edition":"community","features":[{"key":"fake.reports","access":"full","presentation":"promote","limits":[]}],` +
		`"nav":[{"id":"fake-reports","group":"admin","label_key":"fake:nav.reports","route":"/fake","feature":"fake.reports"}],` +
		`"banners":[{"id":"fake-notice","severity":"warning","message":{"code":"FAKE_NOTICE","message":"fake notice"}}]}`
	if status != http.StatusOK || strings.TrimSpace(string(body)) != want {
		t.Fatalf("manifest = %d\n%s\nwant\n%s", status, body, want)
	}

	if !panics(func() { first.host.MountAPI(fakeRoutes{}) }) || !panics(func() { first.host.GuardNodeInsert(refuseFromSecond) }) ||
		!panics(func() { first.host.DeclareFeatures(featuregate.Feature{Key: "late.key"}) }) || !panics(func() { first.host.AddBannerSource(fakeBanners{}) }) {
		t.Fatal("a registration after Init must panic")
	}
	if err := first.host.ProvideFeatures(newFakeSource(featuregate.Full)); !errors.Is(err, app.ErrHostClosed) {
		t.Fatalf("ProvideFeatures after Init = %v", err)
	}
	if err := first.host.HoldScheduling(&fakeHolder{}); !errors.Is(err, app.ErrHostClosed) {
		t.Fatalf("HoldScheduling after Init = %v", err)
	}
	if err := first.host.HoldCardScheduling(&fakeCardHolder{}); !errors.Is(err, app.ErrHostClosed) {
		t.Fatalf("HoldCardScheduling after Init = %v", err)
	}

	// A module's Service runs while the server runs, stops with it, and runs
	// again when the same server is run again.
	e.Eventually(t, 5*time.Second, func() bool { return first.running.Load() == 1 && second.running.Load() == 1 })
	e.RestartServer(t)
	e.Eventually(t, 5*time.Second, func() bool { return first.runs.Load() == 2 && first.running.Load() == 1 })
	e.StopServer(t)
	if first.running.Load() != 0 || second.running.Load() != 0 {
		t.Fatal("services must stop with Run")
	}
}

func panics(f func()) (did bool) {
	defer func() { did = recover() != nil }()
	f()
	return false
}

// A second module may not provide features too, and two holders may not
// share a source: both stop the server from starting.
func TestHostRefusesConflictingRegistrations(t *testing.T) {
	for name, mods := range map[string][]app.Module{
		"two feature sources": {&providerModule{name: "fake"}, &providerModule{name: "other"}},
		"two holders":         {&holderModule{name: "fake"}, &holderModule{name: "other"}},
		"two platform limits": {&limiterModule{name: "fake"}, &limiterModule{name: "other"}},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			_, err := app.New(app.Config{Addr: "127.0.0.1:0", DataDir: dir, DBPath: filepath.Join(dir, "gpuworks.db"), AdminUser: "admin", AdminPassword: "gpuworks-test-password-2026",
				Extensions: app.Extensions{Modules: mods}}, slog.New(slog.NewTextHandler(io.Discard, nil)))
			if err == nil || !strings.Contains(err.Error(), "extension other") {
				t.Fatalf("New = %v, want the second module refused", err)
			}
		})
	}
}

type providerModule struct{ name string }

func (m *providerModule) Name() string { return m.name }
func (m *providerModule) Init(_ context.Context, h *app.Host) error {
	return h.ProvideFeatures(newFakeSource(featuregate.Full))
}

type holderModule struct{ name string }

func (m *holderModule) Name() string { return m.name }
func (m *holderModule) Init(_ context.Context, h *app.Host) error {
	return h.HoldScheduling(&fakeHolder{})
}

// The feature source decides an extension route's answer, request by
// request, and the console manifest follows it.
func TestAFeatureSourceDecidesAnExtensionRoute(t *testing.T) {
	src := newFakeSource(featuregate.Full)
	e := testenv.NewWithExtensions(t, app.Extensions{Modules: []app.Module{&fakeModule{name: "fake", source: src}}}, nil)
	api := e.API(t)
	cases := []struct {
		access           featuregate.Access
		get, post        int
		getCode, postCod string
		manifest         string
		navShown         bool
	}{
		{featuregate.Full, 200, 200, "", "", `"access":"full"`, true},
		{featuregate.ReadOnly, 200, 403, "", messages.ErrorFeatureReadOnly, `"access":"read_only"`, true},
		{featuregate.Locked, 403, 403, messages.ErrorFeatureUnavailable, messages.ErrorFeatureUnavailable, `"access":"locked"`, true},
		{featuregate.Hidden, 404, 404, messages.ErrorFeatureUnavailable, messages.ErrorFeatureUnavailable, `"access":"hidden"`, false},
	}
	for _, c := range cases {
		src.set(c.access)
		status, body := send(t, api, http.MethodGet, "/api/v1/x/fake/reports", "")
		if code, _ := productError(body); status != c.get || (c.getCode != "" && code != c.getCode) {
			t.Fatalf("%s GET = %d %s", c.access, status, body)
		}
		status, body = send(t, api, http.MethodPost, "/api/v1/x/fake/reports", "{}")
		if code, _ := productError(body); status != c.post || (c.postCod != "" && code != c.postCod) {
			t.Fatalf("%s POST = %d %s", c.access, status, body)
		}
		// The community route ignores the feature.
		if status, _ := send(t, api, http.MethodGet, "/api/v1/x/fake/ping", ""); status != http.StatusOK {
			t.Fatalf("%s community route = %d", c.access, status)
		}
		_, manifest := send(t, api, http.MethodGet, "/api/v1/console/manifest", "")
		if !strings.Contains(string(manifest), c.manifest) || strings.Contains(string(manifest), `"id":"fake-reports"`) != c.navShown {
			t.Fatalf("%s manifest = %s", c.access, manifest)
		}
	}
}

// Without a module that provides features, a declared feature is locked:
// nothing it guards works, though its community routes do.
func TestADeclaredFeatureWithoutASourceIsLocked(t *testing.T) {
	e := testenv.NewWithExtensions(t, app.Extensions{Modules: []app.Module{&fakeModule{name: "fake"}}}, nil)
	api := e.API(t)
	status, body := send(t, api, http.MethodGet, "/api/v1/x/fake/reports", "")
	if code, _ := productError(body); status != http.StatusForbidden || code != messages.ErrorFeatureProviderAbsent {
		t.Fatalf("GET without a source = %d %s", status, body)
	}
	if status, _ := send(t, api, http.MethodGet, "/api/v1/x/fake/ping", ""); status != http.StatusOK {
		t.Fatalf("community route = %d", status)
	}
}

// A node insert guard refuses new nodes on every path that inserts one; the
// API answers with the guard's code.
func TestANodeInsertGuardRefusesNewNodes(t *testing.T) {
	e := testenv.NewWithExtensions(t, app.Extensions{Modules: []app.Module{&fakeModule{name: "fake", guard: refuseFromSecond}}}, nil)
	api := e.API(t)
	if status, body := send(t, api, http.MethodPost, "/api/v1/nodes", `{"name":"first","server_url":"http://127.0.0.1:1"}`); status/100 != 2 {
		t.Fatalf("first node = %d %s", status, body)
	}
	status, body := send(t, api, http.MethodPost, "/api/v1/nodes", `{"name":"second","server_url":"http://127.0.0.1:1"}`)
	if code, params := productError(body); status != http.StatusForbidden || code != "FAKE_NODE_CAP" || params["nodes"] != float64(1) {
		t.Fatalf("second node = %d %s", status, body)
	}
	if _, _, err := e.Store().CreateNodePending("third"); !errors.Is(err, store.ErrNodeEnrollmentRefused) {
		t.Fatalf("store insert = %v", err)
	}
	if nodes, err := e.Store().ListNodes(); err != nil || len(nodes) != 1 {
		t.Fatalf("nodes = %d, %v", len(nodes), err)
	}
}

// A scheduling holder cordons the nodes it holds under its source; an
// operator cannot make a held node schedulable, and the node is released
// when the hold ends.
func TestASchedulingHolderHoldsANode(t *testing.T) {
	holder := &fakeHolder{}
	holder.on.Store(true)
	e := testenv.NewWithExtensions(t, app.Extensions{Modules: []app.Module{&fakeModule{name: "fake", holder: holder}}}, nil)
	n := e.AddNode("held", testenv.WithGPUs(2, testenv.GPUSpec{}))
	e.WaitNodeGPUs(t, "held", 2)
	e.Eventually(t, 10*time.Second, func() bool {
		node, err := e.Store().NodeByUUID(n.UUID)
		return err == nil && node.Scheduling == store.NodeCordoned && node.SchedulingBy == "fakehold"
	})
	api := e.API(t)
	status, body := send(t, api, http.MethodPut, "/api/v1/nodes/"+n.UUID+"/scheduling", `{"scheduling":"schedulable"}`)
	if code, params := productError(body); status != http.StatusConflict || code != "FAKE_HOLD" || params["cards"] != float64(2) {
		t.Fatalf("uncordon a held node = %d %s", status, body)
	}
	holder.on.Store(false)
	// The server applies holds in its own pass too; whichever pass finds the
	// hold gone first releases the node, so this call may find nothing left.
	if _, released, err := e.Store().ApplySchedulingHolds(); err != nil || len(released) > 1 || (len(released) == 1 && released[0] != n.UUID) {
		t.Fatalf("release = %v %v", released, err)
	}
	if node, _ := e.Store().NodeByUUID(n.UUID); node.Scheduling != store.NodeSchedulable || node.SchedulingBy != store.SchedulingBySelf {
		t.Fatalf("released node = %s by %q", node.Scheduling, node.SchedulingBy)
	}
}

// Guards run inside the writer's transaction on its single connection, so a
// guard that calls the Store (here the module's own tables) cannot proceed
// until the insert that asked it has finished: it would deadlock. The guard
// below detects that with a timeout and refuses instead, which is what
// proves the rule; a real guard answers from a snapshot and never waits.
func TestAGuardThatCallsTheStoreWaitsOnTheWriter(t *testing.T) {
	mod := &fakeModule{name: "fake"}
	finished := make(chan error, 1)
	mod.guard = func(store.FleetCapacity) *messages.ProductError {
		go func() {
			finished <- mod.tables.Write(func(tx *gorm.DB) error {
				return tx.Exec("INSERT INTO ee_fake_records (note) VALUES (?)", "from guard").Error
			})
		}()
		select {
		case err := <-finished:
			finished <- err
			return nil
		case <-time.After(500 * time.Millisecond):
			return messages.NewProductError("GUARD_WOULD_DEADLOCK", "the guard waited on the writer", nil)
		}
	}
	e := testenv.NewWithExtensions(t, app.Extensions{Modules: []app.Module{mod}}, nil)
	_, _, err := e.Store().CreateNodePending("blocked")
	var pe *messages.ProductError
	if !errors.As(err, &pe) || pe.Code() != "GUARD_WOULD_DEADLOCK" {
		t.Fatalf("insert with an impure guard = %v, want the guard to find the writer held", err)
	}
	select {
	case err := <-finished:
		if err != nil {
			t.Fatalf("the guard's write after the insert released the writer: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the guard's write never ran after the insert released the writer")
	}
}

// With no modules the server is the community server: the manifest names
// no feature, menu entry or banner.
func TestNoModulesAnswerTheCommunityManifest(t *testing.T) {
	e := testenv.NewWithExtensions(t, app.Extensions{}, map[string]yaml.Node{"leftover": section(t, "a: 1\n")})
	status, body := send(t, e.API(t), http.MethodGet, "/api/v1/console/manifest", "")
	if status != http.StatusOK || strings.TrimSpace(string(body)) != `{"edition":"community","features":[],"nav":[],"banners":[]}` {
		t.Fatalf("manifest = %d %s", status, body)
	}
	if v, err := e.Store().NamespaceVersion("fake"); err != nil || v != 0 {
		t.Fatalf("a community open has no extension schema: version %d, %v", v, err)
	}
}

// An Init error stops New and leaves the database free: the same data
// directory starts again at once.
func TestAnInitErrorFreesTheDatabase(t *testing.T) {
	dir := t.TempDir()
	cfg := app.Config{Addr: "127.0.0.1:0", DataDir: dir, DBPath: filepath.Join(dir, "gpuworks.db"), AdminUser: "admin", AdminPassword: "gpuworks-test-password-2026"}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	failing := cfg
	failing.Extensions = app.Extensions{Modules: []app.Module{&fakeModule{name: "fake", initErr: errFake("boom")}}}
	if _, err := app.New(failing, log); err == nil || !strings.Contains(err.Error(), "extension fake: init: boom") {
		t.Fatalf("New with a failing Init = %v", err)
	}
	a, err := app.New(cfg, log)
	if err != nil {
		t.Fatalf("New after a failed Init: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	// The failed open migrated the module's schema before Init failed; the
	// community open leaves it alone.
	if v, err := a.Store().NamespaceVersion("fake"); err != nil || v != 1 {
		t.Fatalf("namespace after the failed start = %d, %v", v, err)
	}
	_ = a.Store().Close()
}
