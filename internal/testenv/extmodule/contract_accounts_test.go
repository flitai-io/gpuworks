package extmodule

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/accounts"
	"gpuworks.ai/gpuworks/internal/server/api"
	"gpuworks.ai/gpuworks/internal/server/app"
	"gpuworks.ai/gpuworks/internal/server/featuregate"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/serverconfig"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/testenv"
)

// fakeIdentity is an identity connector without an external identity
// provider: its start route signs in the subject named in the query at once
// and sends the browser to the core's completion, the way a real connector
// does after its callback. hold keeps the ticket URL instead of redirecting,
// so a test can carry it to another browser.
type fakeIdentity struct {
	name           string
	sources        []string
	provide        atomic.Uint32
	host           *app.Host
	accounts       *accounts.Accounts
	sharesFeatures bool
	// declareErr is what DeclareIdentitySources answered.
	declareErr error
	lastURL    atomic.Pointer[string]
	// lastHandle is the handle the core sent its start route.
	lastHandle atomic.Pointer[string]
}

func (m *fakeIdentity) Name() string { return m.name }

func (m *fakeIdentity) Init(_ context.Context, h *app.Host) error {
	m.host, m.accounts = h, h.Accounts()
	if m.declareErr = h.DeclareIdentitySources(m.sources...); m.declareErr != nil {
		// Kept, not returned: the exclusivity test needs a server that starts
		// so it can read the refusal. The module then starts with no routes
		// and no sign-in entry.
		return nil //nolint:nilerr // the refusal is what the test reads
	}
	h.DeclareFeatures(featuregate.Feature{Key: m.feature()})
	// A server has one feature source; a second connector's feature is
	// answered by the first's.
	if !m.sharesFeatures {
		if err := h.ProvideFeatures(fakeAccess{m}); err != nil {
			return err
		}
	}
	h.MountAPI(fakeIdentityRoutes{m})
	return h.OfferSignIn(app.SignInEntry{Source: m.sources[0], Label: "Acme ID", Start: "/x/" + m.name + "/start"})
}

// feature gates the module's start route and so its sign-in entry.
func (m *fakeIdentity) feature() featuregate.Key { return featuregate.Key(m.name + ".sign_in") }

func newFakeIdentity(name string, sources ...string) *fakeIdentity {
	if len(sources) == 0 {
		sources = []string{"acme"}
	}
	m := &fakeIdentity{name: name, sources: sources}
	m.provide.Store(uint32(featuregate.Full))
	return m
}

type fakeAccess struct{ m *fakeIdentity }

func (a fakeAccess) Availability(featuregate.Key) featuregate.Availability {
	return featuregate.Availability{Access: featuregate.Access(a.m.provide.Load())}
}

type fakeIdentityRoutes struct{ m *fakeIdentity }

func (r fakeIdentityRoutes) Name() string { return r.m.name }

func (r fakeIdentityRoutes) Routes(mt api.Mounter) {
	mt.Handle(api.RouteSpec{Method: http.MethodGet, Pattern: "/x/" + r.m.name + "/start", Access: api.AccessPublic,
		Action: r.m.name + ".start", Feature: r.m.feature()}, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		q := req.URL.Query()
		handle := q.Get("handle")
		r.m.lastHandle.Store(&handle)
		ticket, err := r.m.accounts.SignIn(r.m.sources[0], q.Get("subject"),
			accounts.Profile{Username: q.Get("username")}, handle)
		if err != nil {
			var pe *messages.ProductError
			if errors.As(err, &pe) {
				http.Error(w, pe.Code(), http.StatusForbidden)
				return
			}
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		target := "/api/v1/auth/complete?ticket=" + url.QueryEscape(ticket)
		r.m.lastURL.Store(&target)
		if q.Get("hold") != "" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		http.Redirect(w, req, target, http.StatusFound)
	}))
}

// browser is a client with its own cookies that stops at the console or the
// sign-in page.
func browser(t *testing.T) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Client{Jar: jar, CheckRedirect: func(req *http.Request, _ []*http.Request) error {
		if req.URL.Path == "/" || req.URL.Path == "/login" {
			return http.ErrUseLastResponse
		}
		return nil
	}}
}

// get answers the status, the Location and the body of a GET.
func get(t *testing.T, c *http.Client, raw string) (int, string, string) {
	t.Helper()
	resp, err := c.Get(raw)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header.Get("Location"), string(body)
}

// signedIn reports whether c holds a session the server accepts.
func signedIn(t *testing.T, e *testenv.Env, c *http.Client) bool {
	t.Helper()
	status, _, _ := get(t, c, e.URL()+"/api/v1/auth/me")
	return status == http.StatusOK
}

func identityEnv(t *testing.T, m *fakeIdentity, configure func(*app.Config)) *testenv.Env {
	t.Helper()
	return testenv.NewOnDiskConfigured(t, func(cfg *app.Config) {
		cfg.Extensions = app.Extensions{Modules: []app.Module{m}}
		if configure != nil {
			configure(cfg)
		}
	})
}

// A browser that starts at the core signs in through the connector: the
// core binds the sign-in to it, the connector's ticket is bound to that
// binding, and the completion opens a session. The sign-in options list the
// entry while its feature is available and not while it is locked.
func TestAModuleSignsABrowserInThroughTheCore(t *testing.T) {
	m := newFakeIdentity("fakeid")
	e := identityEnv(t, m, nil)

	status, _, body := get(t, http.DefaultClient, e.URL()+"/api/v1/auth/sign-in-options")
	want := `{"local_password":"all","entries":[{"source":"acme","label":"Acme ID","url":"/api/v1/auth/sign-in/start?source=acme"}]}`
	if status != http.StatusOK || strings.TrimSpace(body) != want {
		t.Fatalf("options = %d %s, want %s", status, body, want)
	}

	c := browser(t)
	start := e.URL() + "/api/v1/auth/sign-in/start?source=acme"
	// The fake connector reads subject and username from its query, so add
	// them to the start URL the core redirects to.
	c.CheckRedirect = func(req *http.Request, _ []*http.Request) error {
		if strings.HasSuffix(req.URL.Path, "/x/fakeid/start") {
			q := req.URL.Query()
			q.Set("subject", "corp:42")
			q.Set("username", "alice")
			req.URL.RawQuery = q.Encode()
		}
		if req.URL.Path == "/" || req.URL.Path == "/login" {
			return http.ErrUseLastResponse
		}
		return nil
	}
	status, location, _ := get(t, c, start)
	if status != http.StatusFound || location != "/" {
		t.Fatalf("sign-in ended at %d %q, want the console", status, location)
	}
	if !signedIn(t, e, c) {
		t.Fatal("the browser has no session after signing in")
	}
	u, _, err := e.Store().LinkedAccount("acme", "corp:42")
	if err != nil || u.Username != "alice" || u.Role != "user" {
		t.Fatalf("account = %+v %v", u, err)
	}

	m.provide.Store(uint32(featuregate.Locked))
	status, _, body = get(t, http.DefaultClient, e.URL()+"/api/v1/auth/sign-in-options")
	if status != http.StatusOK || !strings.Contains(body, `"entries":[]`) {
		t.Fatalf("options with the feature locked = %d %s", status, body)
	}
	status, _, body = get(t, http.DefaultClient, start)
	if status != http.StatusNotFound || !strings.Contains(body, messages.ErrorIdentitySourceUnavailable) {
		t.Fatalf("start with the feature locked = %d %s", status, body)
	}
	status, _, body = get(t, http.DefaultClient, e.URL()+"/api/v1/auth/sign-in/start?source=https://example.com")
	if status != http.StatusNotFound || !strings.Contains(body, messages.ErrorIdentitySourceUnavailable) {
		t.Fatalf("start of an unknown source = %d %s", status, body)
	}
}

// A ticket an attacker obtained in their own browser does not sign a victim
// in: the victim's browser has no binding, or another one (login CSRF).
func TestATicketOnlyCompletesInTheBrowserThatStartedIt(t *testing.T) {
	m := newFakeIdentity("fakeid")
	e := identityEnv(t, m, nil)

	attacker := browser(t)
	attacker.CheckRedirect = func(req *http.Request, _ []*http.Request) error {
		if strings.HasSuffix(req.URL.Path, "/x/fakeid/start") {
			q := req.URL.Query()
			q.Set("subject", "attacker")
			q.Set("username", "mallory")
			q.Set("hold", "1")
			req.URL.RawQuery = q.Encode()
		}
		return nil
	}
	if status, _, _ := get(t, attacker, e.URL()+"/api/v1/auth/sign-in/start?source=acme"); status != http.StatusNoContent {
		t.Fatalf("attacker's sign-in = %d", status)
	}
	ticketURL := *m.lastURL.Load()

	victim := browser(t)
	status, location, _ := get(t, victim, e.URL()+ticketURL)
	if status != http.StatusFound || location != "/login?code="+messages.ErrorIdentityTicketInvalid {
		t.Fatalf("victim without a binding = %d %q", status, location)
	}
	if signedIn(t, e, victim) {
		t.Fatal("the victim was signed in to the attacker's account")
	}

	// A victim in the middle of their own sign-in has a binding of their
	// own; the attacker's ticket still does not match it. The first try
	// spent it, so issue a fresh one.
	if status, _, _ := get(t, attacker, e.URL()+"/api/v1/auth/sign-in/start?source=acme"); status != http.StatusNoContent {
		t.Fatalf("attacker's second sign-in = %d", status)
	}
	ticketURL = *m.lastURL.Load()
	victim.CheckRedirect = func(req *http.Request, _ []*http.Request) error {
		if strings.HasSuffix(req.URL.Path, "/x/fakeid/start") {
			q := req.URL.Query()
			q.Set("subject", "victim")
			q.Set("username", "victor")
			q.Set("hold", "1")
			req.URL.RawQuery = q.Encode()
		}
		return http.ErrUseLastResponse
	}
	// Following only the first redirect sets the victim's binding cookie.
	if status, _, _ := get(t, victim, e.URL()+"/api/v1/auth/sign-in/start?source=acme"); status != http.StatusFound {
		t.Fatalf("victim's own start = %d", status)
	}
	status, location, _ = get(t, victim, e.URL()+ticketURL)
	if status != http.StatusFound || location != "/login?code="+messages.ErrorIdentityTicketInvalid {
		t.Fatalf("victim with another binding = %d %q", status, location)
	}
	if signedIn(t, e, victim) {
		t.Fatal("the victim was signed in to the attacker's account")
	}
}

// A module cannot disable, enable, rename or sign out an administrator, even
// one an administrator linked to its source, and cannot act on a source it
// did not declare; it can on an ordinary user of its source.
func TestAModuleCannotActOnAdministratorsOrOtherSources(t *testing.T) {
	m := newFakeIdentity("fakeid")
	e := identityEnv(t, m, nil)
	var admin store.User
	if err := e.Store().DB().Where("username = ?", "admin").First(&admin).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := e.Store().LinkIdentity(admin.ID, "acme", "boss"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.Store().CreateManagementSession(&admin, time.Hour, time.Hour); err != nil {
		t.Fatal(err)
	}
	acc := m.accounts
	isCode := func(err error, code string) bool {
		var pe *messages.ProductError
		return errors.As(err, &pe) && pe.Code() == code
	}
	if err := acc.Disable("acme", "boss"); !isCode(err, messages.ErrorIdentityProtectedUser) {
		t.Fatalf("disable an administrator = %v", err)
	}
	if err := acc.Enable("acme", "boss"); !isCode(err, messages.ErrorIdentityProtectedUser) {
		t.Fatalf("enable an administrator = %v", err)
	}
	if _, err := acc.RevokeSessions("acme", "boss"); !isCode(err, messages.ErrorIdentityProtectedUser) {
		t.Fatalf("sign out an administrator = %v", err)
	}
	if _, err := acc.Upsert("acme", "boss", accounts.Profile{Username: "renamed"}); !isCode(err, messages.ErrorIdentityProtectedUser) {
		t.Fatalf("rename an administrator = %v", err)
	}
	if got, err := e.Store().UserByID(admin.ID); err != nil || got.Username != "admin" || got.DisabledAt != nil {
		t.Fatalf("administrator after the refusals = %+v %v", got, err)
	}
	var sessions int64
	if err := e.Store().DB().Model(&store.ManagementSession{}).Where("user_id = ?", admin.ID).Count(&sessions).Error; err != nil || sessions == 0 {
		t.Fatalf("administrator's sessions = %d %v", sessions, err)
	}
	if _, err := acc.Upsert("acme", "boss", accounts.Profile{Username: "admin"}); err != nil {
		t.Fatalf("an upsert that changes nothing on an administrator = %v", err)
	}

	if _, err := acc.Upsert("other", "x", accounts.Profile{Username: "x"}); !isCode(err, messages.ErrorIdentitySourceConflict) {
		t.Fatalf("upsert in a source the module does not own = %v", err)
	}
	if err := acc.Disable("other", "x"); !isCode(err, messages.ErrorIdentitySourceConflict) {
		t.Fatalf("disable in a source the module does not own = %v", err)
	}
	if _, _, err := acc.List("other", 0, 10); !isCode(err, messages.ErrorIdentitySourceConflict) {
		t.Fatalf("list a source the module does not own = %v", err)
	}

	if _, err := acc.Upsert("acme", "corp:1", accounts.Profile{Username: "erin"}); err != nil {
		t.Fatal(err)
	}
	if err := acc.Disable("acme", "corp:1"); err != nil {
		t.Fatalf("disable an ordinary user = %v", err)
	}
	if _, err := acc.Upsert("acme", "corp:1", accounts.Profile{Username: "erin.e"}); err != nil {
		t.Fatalf("rename an ordinary user = %v", err)
	}
	page, next, err := acc.List("acme", 0, 10)
	if err != nil || next != 0 || len(page) != 2 || !page[0].Protected || page[1].Protected || !page[1].Disabled || page[1].Username != "erin.e" {
		t.Fatalf("list = %+v next=%d err=%v", page, next, err)
	}
	if err := acc.Enable("acme", "corp:1"); err != nil {
		t.Fatal(err)
	}
}

// A source belongs to one module; a module that declares another's source is
// refused, and the sign-in of an unlinked identity is refused when the site
// creates no accounts.
func TestIdentitySourcesAreExclusiveAndProvisioningIsTheSites(t *testing.T) {
	first := newFakeIdentity("fakeid")
	second := newFakeIdentity("otherid")
	testenv.NewOnDiskConfigured(t, func(cfg *app.Config) {
		cfg.Extensions = app.Extensions{Modules: []app.Module{first, second}}
	})
	var pe *messages.ProductError
	if !errors.As(second.declareErr, &pe) || pe.Code() != messages.ErrorIdentitySourceConflict {
		t.Fatalf("a second module declaring acme = %v", second.declareErr)
	}
	if err := first.host.DeclareIdentitySources("late"); !errors.Is(err, app.ErrHostClosed) {
		t.Fatalf("declaring after Init = %v, want ErrHostClosed", err)
	}

	m := newFakeIdentity("fakeid")
	e := identityEnv(t, m, func(cfg *app.Config) { cfg.AuthExternalProvision = serverconfig.ProvisionExisting })
	c := browser(t)
	c.CheckRedirect = func(req *http.Request, _ []*http.Request) error {
		if strings.HasSuffix(req.URL.Path, "/x/fakeid/start") {
			q := req.URL.Query()
			q.Set("subject", "stranger")
			q.Set("username", "stranger")
			req.URL.RawQuery = q.Encode()
		}
		return nil
	}
	status, _, body := get(t, c, e.URL()+"/api/v1/auth/sign-in/start?source=acme")
	if status != http.StatusForbidden || strings.TrimSpace(body) != messages.ErrorIdentityNotProvisioned {
		t.Fatalf("an unlinked sign-in under provision: existing = %d %s", status, body)
	}
	if _, _, err := e.Store().LinkedAccount("acme", "stranger"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("an account was created: %v", err)
	}
}

// heldSignIn starts a sign-in through source in c with the connector holding
// the ticket, so c has the binding and the test decides when to complete.
func heldSignIn(t *testing.T, e *testenv.Env, c *http.Client, module, source, subject, username string) {
	t.Helper()
	c.CheckRedirect = func(req *http.Request, _ []*http.Request) error {
		if strings.HasSuffix(req.URL.Path, "/x/"+module+"/start") {
			q := req.URL.Query()
			q.Set("subject", subject)
			q.Set("username", username)
			q.Set("hold", "1")
			req.URL.RawQuery = q.Encode()
		}
		if req.URL.Path == "/" || req.URL.Path == "/login" {
			return http.ErrUseLastResponse
		}
		return nil
	}
	if status, _, body := get(t, c, e.URL()+"/api/v1/auth/sign-in/start?source="+source); status != http.StatusNoContent {
		t.Fatalf("held sign-in through %s = %d %s", source, status, body)
	}
}

// A ticket another connector obtained with the handle a browser was given for
// one source does not complete that browser's sign-in: the handle is the
// source's, and so is the ticket the browser may redeem.
func TestATicketOfAnotherSourceDoesNotCompleteASignIn(t *testing.T) {
	first := newFakeIdentity("fakeid", "acme")
	second := newFakeIdentity("otherid", "beta")
	second.sharesFeatures = true
	e := testenv.NewOnDiskConfigured(t, func(cfg *app.Config) {
		cfg.Extensions = app.Extensions{Modules: []app.Module{first, second}}
	})
	victim := browser(t)
	heldSignIn(t, e, victim, "fakeid", "acme", "victim", "victor")
	handle := *first.lastHandle.Load()
	ticket, err := second.accounts.SignIn("beta", "attacker", accounts.Profile{Username: "mallory"}, handle)
	if err != nil {
		t.Fatal(err)
	}
	status, location, _ := get(t, victim, e.URL()+"/api/v1/auth/complete?ticket="+url.QueryEscape(ticket))
	if status != http.StatusFound || location != "/login?code="+messages.ErrorIdentityTicketInvalid {
		t.Fatalf("another source's ticket = %d %q", status, location)
	}
	if signedIn(t, e, victim) {
		t.Fatal("the browser was signed in through a source it did not start with")
	}
}

// A ticket issued before the source's feature was locked, or before its
// account was disabled, does not open a session: each refusal lands on the
// sign-in page with its own code.
func TestACompletionRefusesAWithdrawnSourceAndADisabledAccount(t *testing.T) {
	m := newFakeIdentity("fakeid")
	e := identityEnv(t, m, nil)

	c := browser(t)
	heldSignIn(t, e, c, "fakeid", "acme", "corp:5", "frank")
	m.provide.Store(uint32(featuregate.Locked))
	status, location, _ := get(t, c, e.URL()+*m.lastURL.Load())
	if status != http.StatusFound || location != "/login?code="+messages.ErrorIdentitySourceUnavailable {
		t.Fatalf("completion after the feature was locked = %d %q", status, location)
	}
	if signedIn(t, e, c) {
		t.Fatal("a withdrawn source opened a session")
	}

	m.provide.Store(uint32(featuregate.Full))
	c = browser(t)
	heldSignIn(t, e, c, "fakeid", "acme", "corp:5", "frank")
	u, _, err := e.Store().LinkedAccount("acme", "corp:5")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Store().DisableUser(u.ID); err != nil {
		t.Fatal(err)
	}
	status, location, _ = get(t, c, e.URL()+*m.lastURL.Load())
	if status != http.StatusFound || location != "/login?code="+messages.ErrorUserDisabled {
		t.Fatalf("completion for a disabled account = %d %q", status, location)
	}
	if signedIn(t, e, c) {
		t.Fatal("a disabled account opened a session")
	}
}

// A connector that asks for a username another account has gets
// IDENTITY_USERNAME_TAKEN naming it, from a sign-in and from a directory
// sync, and nothing is created.
func TestATakenUsernameIsRefusedWithTheName(t *testing.T) {
	m := newFakeIdentity("fakeid")
	e := identityEnv(t, m, nil)
	handle := strings.Repeat("a", 64)
	for name, call := range map[string]func() error{
		"sign-in": func() error {
			_, err := m.accounts.SignIn("acme", "corp:8", accounts.Profile{Username: "admin"}, handle)
			return err
		},
		"upsert": func() error {
			_, err := m.accounts.Upsert("acme", "corp:8", accounts.Profile{Username: "admin"})
			return err
		},
	} {
		var pe *messages.ProductError
		if err := call(); !errors.As(err, &pe) || pe.Code() != messages.ErrorIdentityUsernameTaken || pe.Params()["username"] != "admin" {
			t.Fatalf("%s with a taken username = %v", name, err)
		}
	}
	if _, _, err := e.Store().LinkedAccount("acme", "corp:8"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a refused account left a link: %v", err)
	}
}
