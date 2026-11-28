package api

import (
	"context"
	"fmt"
	"net/http"
	"path"
	"regexp"
	"strings"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/featuregate"
	"gpuworks.ai/gpuworks/internal/server/messages"
)

// ExtensionDeps are the hooks a separately built server hands the API. The
// zero value adds nothing: the community server.
type ExtensionDeps struct {
	// Extensions mount routes under /api/v1/x/<name>/ (Extension).
	Extensions []Extension
	// Features answers for the feature keys extension routes and menu
	// entries name; nil declares none, so naming a feature fails at Router.
	Features *featuregate.Gate
	// Banners add notices above every console page.
	Banners []BannerSource
	// Edition names the build in the console manifest, for display only;
	// empty reads community.
	Edition string
	// Plugins manages the installed plugin packages (/api/v1/plugins); nil
	// answers PLUGINS_UNAVAILABLE.
	Plugins PluginAdmin
	// Dynamic serves the installed plugin packages' routes under
	// /api/v1/x/<id>/; nil serves none.
	Dynamic *DynamicExtension
	// Platform limits the whole server (platform_limit.go); nil never
	// limits it. PlatformOwner is the extension that registered it: its
	// own routes are served under any limit, so its page can lift it.
	Platform      PlatformLimiter
	PlatformOwner string
}

// Extension is a set of management routes a separately built server mounts.
type Extension interface {
	// Name is the extension's namespace: its routes live under
	// /api/v1/x/<Name>/. It follows [a-z][a-z0-9_]{1,31}. The core serves
	// nothing under /api/v1/x/, so a name may equal a core path segment
	// ("nodes") without colliding with a core route, now or later.
	Name() string
	// Routes registers the routes, once, while Router builds.
	Routes(m Mounter)
}

// NavContributor is optional on an Extension: the console menu entries it
// adds. Router reads them once.
type NavContributor interface {
	Nav() []dto.NavContribution
}

// BannerSource answers the notices above every console page for one viewer.
// It runs on each manifest request and answers from state it keeps, never
// scanning nodes, deployments or tables.
type BannerSource interface {
	Banners(ctx context.Context, viewer auth.Claims) []dto.ConsoleBanner
}

// Mounter registers an extension's routes.
type Mounter interface {
	// Handle mounts h for spec. Router panics on a pattern outside the
	// extension's namespace, a method and pattern another route has, a
	// route that names no feature without being Community, or an
	// undeclared feature.
	Handle(spec RouteSpec, h http.Handler)
}

// RouteAccess is who may call an extension route.
type RouteAccess uint8

const (
	// AccessRead: any signed-in user and any platform token; a read token
	// only for GET, HEAD and OPTIONS.
	AccessRead RouteAccess = iota
	// AccessAdmin: administrators, by session or platform token (a read
	// token still only reads).
	AccessAdmin
	// AccessAdminSession: administrators signed in with a session; no
	// platform token.
	AccessAdminSession
	// AccessPublic: no authentication, limited per client address like a
	// public plugin route (60 a minute, bursts of 30). The chain never reads
	// or renews a session, so the handler never learns who calls. For what
	// a browser reaches before it has signed in, such as an identity
	// connector's start and callback routes. Under a platform limit it
	// follows its method unless it is a SignIn route.
	AccessPublic
)

func (a RouteAccess) String() string {
	switch a {
	case AccessAdmin:
		return "admin"
	case AccessAdminSession:
		return "admin-session"
	case AccessPublic:
		return "public"
	}
	return "read"
}

// RouteSpec describes one extension route.
type RouteSpec struct {
	// Method is GET, POST, PUT, PATCH or DELETE.
	Method string
	// Pattern is relative to /api/v1 and is "/x/<name>" or starts with
	// "/x/<name>/"; chi parameters ({id}) are allowed.
	Pattern string
	Access  RouteAccess
	// Action names what the route does ("<extension>.<verb>"), for the
	// route inventory.
	Action string
	// Feature is the feature the route belongs to. While it is read-only
	// only reads pass (and writes marked WriteWhenReadOnly); while locked
	// the route answers 403, while hidden 404.
	Feature featuregate.Key
	// WriteWhenReadOnly lets a non-read method through while Feature is
	// read-only: a write that only removes or relaxes what the feature
	// holds.
	WriteWhenReadOnly bool
	// AIReadable lets the assistant and MCP read a GET route; extension
	// routes are unreadable to them unless marked.
	AIReadable bool
	// Community marks a route that belongs to no feature. A route sets
	// exactly one of Feature and Community.
	Community bool
	// SignIn marks an AccessPublic route that is part of signing in (an
	// identity connector's start or callback, a POST for a SAML response):
	// it is served under any platform limit, as signing in with a password
	// is. Only an AccessPublic route may set it.
	SignIn bool
}

var extensionNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{1,31}$`)

// routeParam matches a chi parameter ({id}, {id:[0-9]+}); two patterns that
// differ only in parameter names match the same requests.
var routeParam = regexp.MustCompile(`\{[^}]*\}`)

// extensionRoot is the /api/v1 subtree that holds every extension's
// namespace. The core mounts no route under it (a router inventory test
// keeps it so), so no extension name can collide with a core path.
const extensionRoot = "/x"

// extensionEndpoint is an extension route's registered handler; the route
// inventory and the assistant's reader find a route's spec through it.
type extensionEndpoint struct {
	spec RouteSpec
	next http.Handler
}

func (e *extensionEndpoint) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	e.next.ServeHTTP(w, r)
}

// extensionMounter mounts every extension into the /api/v1 router.
type extensionMounter struct {
	a    *apiHandlers
	r    chi.Router
	name string
	seen map[string]bool
}

// mountExtensions registers the extensions' routes on r, the /api/v1 router,
// after every core route, and keeps their menu entries for the manifest.
func (a *apiHandlers) mountExtensions(r chi.Router) {
	ext := a.d.Extensions
	if len(ext.Extensions) == 0 {
		return
	}
	m := &extensionMounter{a: a, r: r, seen: map[string]bool{}}
	names := map[string]bool{}
	for _, e := range ext.Extensions {
		name := e.Name()
		switch {
		case !extensionNamePattern.MatchString(name):
			panic(fmt.Sprintf("api: extension name %q does not match [a-z][a-z0-9_]{1,31}", name))
		case names[name]:
			panic(fmt.Sprintf("api: extension %q registered twice", name))
		}
		names[name] = true
		m.name = name
		e.Routes(m)
		if nc, ok := e.(NavContributor); ok {
			for _, n := range nc.Nav() {
				a.extensionNav = append(a.extensionNav, m.checkNav(n))
			}
		}
	}
	m.name = "" // a Mounter kept past Routes mounts nothing
}

func (m *extensionMounter) Handle(spec RouteSpec, h http.Handler) {
	if m.name == "" {
		panic("api: an extension mounted a route after Router was built")
	}
	where := spec.Method + " /api/v1" + spec.Pattern
	switch spec.Method {
	case http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
	default:
		panic(fmt.Sprintf("api: extension %s: method %q not allowed", m.name, spec.Method))
	}
	if ns := extensionRoot + "/" + m.name; spec.Pattern != ns && !strings.HasPrefix(spec.Pattern, ns+"/") {
		panic(fmt.Sprintf("api: extension %s: %s is outside /api/v1%s/", m.name, where, ns))
	}
	if path.Clean(spec.Pattern) != spec.Pattern {
		panic(fmt.Sprintf("api: extension %s: %s is not a clean path (no \"..\", \".\", \"//\" or trailing \"/\")", m.name, where))
	}
	if h == nil {
		panic(fmt.Sprintf("api: extension %s: %s has no handler", m.name, where))
	}
	key := spec.Method + " " + routeParam.ReplaceAllString(spec.Pattern, "{}")
	if m.seen[key] {
		panic(fmt.Sprintf("api: extension %s: %s registered twice", m.name, where))
	}
	if spec.AIReadable && spec.Method != http.MethodGet {
		panic(fmt.Sprintf("api: extension %s: %s: only a GET route can be AIReadable", m.name, where))
	}
	if spec.Access > AccessPublic {
		panic(fmt.Sprintf("api: extension %s: %s: unknown access %d", m.name, where, spec.Access))
	}
	if spec.Access == AccessPublic && spec.AIReadable {
		panic(fmt.Sprintf("api: extension %s: %s: a public route cannot be AIReadable", m.name, where))
	}
	if spec.SignIn && spec.Access != AccessPublic {
		panic(fmt.Sprintf("api: extension %s: %s: only an AccessPublic route can be a SignIn route", m.name, where))
	}
	m.checkFeature(where, spec.Feature, spec.Community)
	m.seen[key] = true
	if spec.Access == AccessPublic {
		m.a.notePublic(spec)
	}
	m.r.Method(spec.Method, spec.Pattern, &extensionEndpoint{spec: spec, next: m.a.extensionChain(spec, h)})
}

func (m *extensionMounter) checkFeature(where string, key featuregate.Key, community bool) {
	switch {
	case community && key != "":
		panic(fmt.Sprintf("api: extension %s: %s names feature %q and is Community; choose one", m.name, where, key))
	case !community && key == "":
		panic(fmt.Sprintf("api: extension %s: %s names no feature and is not Community", m.name, where))
	case key != "" && !m.a.d.Extensions.Features.Declared(key):
		panic(fmt.Sprintf("api: extension %s: %s names undeclared feature %q", m.name, where, key))
	}
}

func (m *extensionMounter) checkNav(n dto.NavContribution) dto.NavContribution {
	if n.ID == "" || n.Group == "" || n.LabelKey == "" || !strings.HasPrefix(n.Route, "/") {
		panic(fmt.Sprintf("api: extension %s: menu entry %+v needs an id, a group, a label key and a route", m.name, n))
	}
	for _, prev := range m.a.extensionNav {
		if prev.ID == n.ID {
			panic(fmt.Sprintf("api: extension %s: menu entry %s added twice", m.name, n.ID))
		}
	}
	if n.Feature != "" && !m.a.d.Extensions.Features.Declared(featuregate.Key(n.Feature)) {
		panic(fmt.Sprintf("api: extension %s: menu entry %s names undeclared feature %q", m.name, n.ID, n.Feature))
	}
	return n
}

// extensionChain is an extension route's middleware, in the order core routes
// apply it: authentication (with the session's origin check and renewal),
// the platform token rules, compression and the administrator check; then the
// caller's claims for the handler (auth.ClaimsFrom), the assistant rule and
// the feature gate.
func (a *apiHandlers) extensionChain(spec RouteSpec, h http.Handler) http.Handler {
	h = a.featureGuard(spec, h)
	if spec.Access == AccessPublic {
		// No session is read, so there are no claims and no assistant
		// read; the limiter stands where authentication would.
		return a.publicRouteLimit(spec, h)
	}
	h = assistantGuard(spec, h)
	h = a.withClaims(h)
	if spec.Access != AccessRead {
		h = a.managementAdmin(h)
	}
	h = compressJSON(h)
	if spec.Access == AccessAdminSession {
		h = sessionOnly(h)
	} else {
		h = readTokensRead(h)
	}
	return a.managementAuth(h)
}

// assistantGuard refuses the assistant's in-process reads of a route not
// marked AIReadable; the reader also leaves such routes out of its list.
func assistantGuard(spec RouteSpec, next http.Handler) http.Handler {
	if spec.AIReadable {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isAssistantRead(r) {
			writeProductError(w, http.StatusNotFound, messages.ErrorNotFound, "the assistant does not read this route", nil)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// featureGuard applies spec.Feature: a read passes while the feature is full
// or read-only, a write only while it is full (or read-only with
// WriteWhenReadOnly). A hidden feature answers 404, any other refusal 403.
func (a *apiHandlers) featureGuard(spec RouteSpec, next http.Handler) http.Handler {
	if spec.Feature == "" {
		return next
	}
	gate := a.d.Extensions.Features
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		write := !spec.WriteWhenReadOnly
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			write = false
		}
		if access, refusal := gate.Require(spec.Feature, write); refusal != nil {
			status := http.StatusForbidden
			if access == featuregate.Hidden {
				status = http.StatusNotFound
			}
			writeErr(w, status, refusal)
			return
		}
		next.ServeHTTP(w, r)
	})
}
