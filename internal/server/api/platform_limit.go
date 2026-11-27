package api

import (
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/server/featuregate"
	"gpuworks.ai/gpuworks/internal/server/messages"
)

// PlatformLimiter answers how far the whole server is available right now.
// A separately built server registers one (app.Host.LimitPlatform); the
// community server has none and never limits itself.
//
// It runs on every request, so it answers from a snapshot it swaps
// atomically: one atomic load, no I/O, no lock and no allocation.
type PlatformLimiter interface {
	PlatformLimit() PlatformLimit
}

// PlatformLimit is a PlatformLimiter's answer.
type PlatformLimit struct {
	// Access is featuregate.Full (nothing is limited), ReadOnly (reads and
	// the data plane are served, changes are refused) or Locked (reads are
	// served; the data plane and changes are refused). Hidden is not an
	// answer for a whole server and reads as Locked, so the zero value
	// refuses.
	Access featuregate.Access
	// Notice is the refusal to return instead of the core's own, built
	// when the snapshot changed; nil uses the core's.
	Notice *messages.ProductError
	// Link is the console page where an administrator lifts the limit;
	// the console's notice opens it. Empty for none.
	Link string
	// Action names what Link does, for the control that opens it; nil
	// leaves the console's own wording.
	Action *messages.ProductError
}

// platformClass is what a limit does to one route. The zero value is the
// strictest class. A route under /api/v1 that no table names is classified
// by its method, so a new GET there is served under a limit; a route outside
// /api/v1 that no rule names is refused. Either way the pinned list in
// platform_limit_routes_test.go differs until the new route is added to
// it, which is where serving it under a limit is decided.
type platformClass uint8

const (
	// platformChange changes the server or brings something new into it:
	// refused while ReadOnly and while Locked.
	platformChange platformClass = iota
	// platformRead only reads: served under any limit.
	platformRead
	// platformServe is the data plane that running work answers: served
	// while ReadOnly, refused while Locked.
	platformServe
	// platformRecovery is served under any limit although it may change
	// something: signing in, health, what machines that already joined
	// need, and what an administrator needs to lift the limit or to repair
	// the server.
	platformRecovery
)

// platformRecoveryRoutes are served under any limit, by method and route
// pattern.
var platformRecoveryRoutes = map[string]bool{
	"GET /healthz":             true,
	"GET /api/v1/readyz":       true,
	"GET /api/v1/healthz":      true,
	"POST /api/v1/auth/login":  true,
	"POST /api/v1/auth/logout": true,
	"GET /api/v1/auth/me":      true,
	// The session of a signed-in administrator stays alive, and a password
	// can be changed, while the server is limited.
	"POST /api/v1/auth/activity": true,
	"POST /api/v1/auth/password": true,
	// The sign-in page and the console shell: without them nobody reaches
	// the page that lifts the limit.
	"GET /api/v1/branding":          true,
	"POST /api/v1/updates/prefetch": true,
	"GET /api/v1/console/manifest":  true,
	// Machines that already joined keep their connection and their
	// health check; a limit never interrupts running work.
	"GET /api/v1/agent/connect":    true,
	"POST /api/v1/slurm/gpu-nodes": true,
	// A machine of a cluster that already joined asks for its identity
	// again when it starts without it (its pod was replaced); the answer is
	// the identity it had. A machine that is new is a node insert, which
	// the module that limits the server refuses through its insert guard.
	"POST /api/v1/kube/enroll":  true,
	"POST /api/v1/slurm/enroll": true,
	"GET /slurm/check.sh":       true,
	"GET /slurm/token-status":   true,
	// The check of a Kubernetes cluster's agents only reads the cluster.
	"GET /kube/check.sh": true,
}

// platformRecoveryPrefixes are subtrees served under any limit: the
// installed packages' management (the local administrator's floor), the
// server's own upgrade (a release may be what makes the limit liftable),
// and what support asks for.
var platformRecoveryPrefixes = []string{
	"/api/v1/plugins",
	"/api/v1/updates",
	"/api/v1/diagnostics/",
}

// platformRefusedRecovery are routes inside a recovery subtree that bring
// new code into the server: refused like any change. Stopping, restarting,
// rolling back and removing an installed package stay served.
var platformRefusedRecovery = map[string]bool{
	"POST /api/v1/plugins/install":      true,
	"POST /api/v1/plugins/{id}/upgrade": true,
}

// platformChangingReads are GET routes that hand out what brings a new
// machine in, or open an interactive session: refused like a change.
var platformChangingReads = map[string]bool{
	"GET /install.sh":                           true,
	"GET /kube/manifest.yaml":                   true,
	"GET /slurm/setup.sh":                       true,
	"GET /api/v1/kube/clusters/{uuid}/manifest": true,
	"GET /api/v1/slurm/clusters/{uuid}/setup":   true,
	"GET /api/v1/slurm/clusters/{uuid}/script":  true,
	"GET /api/v1/nodes/{uuid}/terminal":         true,
}

// platformReachingReads are GET routes that change nothing here but reach
// outside the server to answer: a joined machine, a storage, a cloud
// account, an image registry. They are reads, served under any limit like
// every other; they are named so that serving them is a decision, and so
// that one which starts to change something is moved, not overlooked.
var platformReachingReads = map[string]bool{
	// A joined machine answers: its files, its images, an instance's log.
	"GET /api/v1/nodes/{uuid}/fs":                    true,
	"GET /api/v1/images/nodes/{uuid}/runtime-images": true,
	"GET /api/v1/instances/{uuid}/logs":              true,
	// A storage's files, listed and downloaded, and a batch's files.
	"GET /api/v1/storages/{id}/files":          true,
	"GET /api/v1/storages/{id}/files/download": true,
	"GET /api/v1/batches/{id}/files/{kind}":    true,
	// A cloud account's own lists, and an image registry's tags.
	"GET /api/v1/providers/{id}/instances":    true,
	"GET /api/v1/providers/{id}/clusters":     true,
	"GET /api/v1/providers/{id}/hpc-clusters": true,
	// How a cloud cluster's agent pods stand, read from the cluster.
	"GET /api/v1/kube/clusters/{uuid}/rollout": true,
	"GET /api/v1/images/community/browse":      true,
	// The server's public SSH key and a node's SSH settings without their
	// secrets.
	"GET /api/v1/ssh-key":          true,
	"GET /api/v1/nodes/{uuid}/ssh": true,
}

// platformReadingPosts are POST routes that read: served under any limit.
// A POST reads when it stores nothing of the business and changes nothing
// on a node. It may compute from what the server holds, call an address
// the request names, or ask a node a question the node answers without
// changing itself. A POST that starts a job, stores or changes a resource,
// or makes a node open a listener or mount something is not here, even
// when it is called a check: the deployment preflight starts measurement
// listeners on nodes and the storage probe mounts on one.
var platformReadingPosts = map[string]bool{
	// MCP serves read-only tools, and the assistant answers from what it
	// reads.
	"POST /api/v1/mcp":    true,
	"POST /api/v1/ai/ask": true,
	// Previews of a form: what a model, a deployment or a benchmark would
	// be, computed and returned.
	"POST /api/v1/models/local/preview": true,
	"POST /api/v1/models/preview-url":   true,
	"POST /api/v1/evaluate":             true,
	"POST /api/v1/benchmarks/models":    true,
	// Checks of something not saved: unsaved cloud credentials, an
	// assistant model, a benchmark's target address (a node asks it), the
	// SSH address of a machine that is not a node yet.
	"POST /api/v1/providers/test":   true,
	"POST /api/v1/ai/configs/test":  true,
	"POST /api/v1/benchmarks/probe": true,
	"POST /api/v1/nodes/precheck":   true,
	// The script that checks a Kubernetes cluster before it is connected:
	// rendered and returned, run by the user.
	"POST /api/v1/kube/precheck": true,
	// Which notices this user has read: the user's own view, no resource.
	"POST /api/v1/notices/read": true,
}

// platformServePatterns are the data plane: the OpenAI-compatible address
// and the console's own entry to it, which reaches the same models.
var platformServePatterns = map[string]bool{
	"/v1/*":                   true,
	"/api/v1/playground/v1/*": true,
}

// platformExtensionRoot is where every extension's routes live; the core
// registers nothing under it but the mount that serves installed packages.
const platformExtensionRoot = "/api/v1" + extensionRoot + "/"

// classifyPlatformRoute is the class of a request for method that matched
// the route pattern at path. owner is the extension whose own namespace
// stays served under any limit (the one that registered the limiter). The
// namespace is read from the path only for a route under the extension
// root, where the first segment is the namespace whichever route answers:
// a compiled route, or the packages' mount answering 404 or 405 for it.
// classified is false for a route outside /api/v1 that no rule names: it is
// refused under any limit.
func classifyPlatformRoute(method, pattern, path, owner string) (class platformClass, classified bool) {
	key := method + " " + pattern
	if platformRecoveryRoutes[key] {
		return platformRecovery, true
	}
	if platformRefusedRecovery[key] {
		return platformChange, true
	}
	for _, prefix := range platformRecoveryPrefixes {
		if pattern == strings.TrimSuffix(prefix, "/") || strings.HasPrefix(pattern, strings.TrimSuffix(prefix, "/")+"/") {
			return platformRecovery, true
		}
	}
	if owner != "" && strings.HasPrefix(pattern, platformExtensionRoot) && strings.HasPrefix(path, platformExtensionRoot+owner+"/") {
		return platformRecovery, true
	}
	if platformChangingReads[key] {
		return platformChange, true
	}
	if platformReadingPosts[key] || platformReachingReads[key] {
		return platformRead, true
	}
	if platformServePatterns[pattern] {
		return platformServe, true
	}
	if pattern == "/*" {
		// The catch-all is the console's files and the agent downloads
		// under /dl/. Without the console nobody reaches the page that
		// lifts a limit, and a machine that already joined fetches its
		// agent from /dl/ to upgrade or repair itself; a new machine gains
		// nothing from the file, since its enrollment is refused. Only
		// reading them is served.
		switch method {
		case http.MethodGet, http.MethodHead:
			return platformRecovery, true
		}
		return platformChange, true
	}
	if !strings.HasPrefix(pattern, "/api/v1/") {
		return platformChange, false
	}
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return platformRead, true
	}
	return platformChange, true
}

// platformRefusals are the core's own refusals, built once.
var (
	platformLockedRefusal = messages.NewProductError(messages.ErrorPlatformLocked,
		"this server is locked; it can be viewed, and changes and inference are refused until it is unlocked", nil)
	platformReadOnlyRefusal = messages.NewProductError(messages.ErrorPlatformReadOnly,
		"this server is read-only; what runs keeps running and can be viewed, changes are refused", nil)
)

// allows reports whether a route of class is served under access.
func (c platformClass) allows(access featuregate.Access) bool {
	switch access {
	case featuregate.Full:
		return true
	case featuregate.ReadOnly:
		return c != platformChange
	}
	return c == platformRecovery || c == platformRead
}

// platformRefusal is what a refused request is answered with under limit.
func platformRefusal(limit PlatformLimit) *messages.ProductError {
	if limit.Notice != nil {
		return limit.Notice
	}
	if limit.Access == featuregate.ReadOnly {
		return platformReadOnlyRefusal
	}
	return platformLockedRefusal
}

// platformGuard refuses, before any handler, what the server's limit does
// not serve. Without a limiter it adds nothing, and under a limiter that
// answers Full it costs that one call.
//
// A management route (under /api/v1) is refused only to a caller who has
// authenticated: an anonymous one gets the 401 an unlimited server gives,
// and so learns neither which routes exist nor that the server is limited.
// The data plane and the routes that hand out enrollment material carry
// credentials the guard cannot check, and are refused to anyone.
//
// A route of an installed package that anyone may call without signing in
// (DynamicPublic) is served under any limit, as signing in is: a package
// that signs users in must keep working.
//
// It resolves the route the request would reach in routes (the router it is
// installed on), so the class comes from the route's pattern, never from a
// guess at the path. A request no route matches goes on to the router's
// 404 or 405.
func (a *apiHandlers) platformGuard(routes chi.Routes) func(http.Handler) http.Handler {
	limiter := a.d.Extensions.Platform
	if limiter == nil {
		return func(next http.Handler) http.Handler { return next }
	}
	owner := a.d.Extensions.PlatformOwner
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			limit := limiter.PlatformLimit()
			if limit.Access == featuregate.Full {
				next.ServeHTTP(w, r)
				return
			}
			// The path the router itself routes on.
			path := r.URL.RawPath
			if path == "" {
				path = r.URL.Path
			}
			if path == "" {
				path = "/"
			}
			pattern := routes.Find(chi.NewRouteContext(), r.Method, path)
			if pattern == "" {
				next.ServeHTTP(w, r)
				return
			}
			class, _ := classifyPlatformRoute(r.Method, pattern, path, owner)
			if class.allows(limit.Access) || a.publicPackageRoute(r.Method, pattern, path) {
				next.ServeHTTP(w, r)
				return
			}
			if strings.HasPrefix(pattern, "/api/v1/") {
				if _, err := a.authenticateIdentity(r); err != nil {
					a.writeManagementAuthError(w, err)
					return
				}
			}
			refusal := platformRefusal(limit)
			if class != platformServe {
				writeErr(w, http.StatusForbidden, refusal)
				return
			}
			// The data plane's clients read the gateway's error shape.
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write(append(refusal.GatewayBody("permission_error"), '\n'))
		})
	}
}

// publicPackageRoute reports whether the request is for an installed
// package's route that needs no sign-in.
func (a *apiHandlers) publicPackageRoute(method, pattern, path string) bool {
	if a.dynamic == nil || pattern != platformExtensionRoot+"*" {
		return false
	}
	route, match := a.dynamic.ext.Match(method, strings.TrimPrefix(path, strings.TrimSuffix(platformExtensionRoot, "/")))
	return match == DynamicFound && route.Access == DynamicPublic
}

// consolePlatform is the limit as the console manifest shows it; nil while
// nothing is limited, so the community manifest is unchanged.
func (a *apiHandlers) consolePlatform() *PlatformLimit {
	limiter := a.d.Extensions.Platform
	if limiter == nil {
		return nil
	}
	limit := limiter.PlatformLimit()
	if limit.Access == featuregate.Full {
		return nil
	}
	if limit.Access != featuregate.ReadOnly {
		limit.Access = featuregate.Locked
	}
	if limit.Notice == nil {
		limit.Notice = platformRefusal(limit)
	}
	return &limit
}
