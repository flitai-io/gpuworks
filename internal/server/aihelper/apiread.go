package aihelper

import (
	"context"
	"net/url"
	"strings"
)

// APIReader reads the platform's own HTTP API as the user asking: the same
// routes, permissions and wire shapes the UI gets. The api package provides
// it per request; the assistant's call_api, search_api and cluster_overview
// tools go through it.
type APIReader interface {
	// Get issues GET path?query and answers the status and body. It refuses
	// a path whose route Readable does not allow.
	Get(ctx context.Context, path string, query url.Values) (status int, body []byte, err error)
	// Routes lists the GET route patterns under /api/v1
	// ("/api/v1/nodes/{uuid}").
	Routes() []string
	// Route answers the pattern a concrete GET path matches, or "".
	Route(path string) string
}

// deniedRoutes are the GET routes the assistant never reads, and why. Every
// other GET route is readable once the catalog describes it
// (TestEveryGETRouteIsClassified keeps the two lists complete).
var deniedRoutes = map[string]string{
	"/api/v1/providers/{id}":                      "cloud account edit configuration",
	"/api/v1/agent/connect":                       "the agents' WebSocket, not a read",
	"/api/v1/ai/configs":                          "AI endpoints and their keys",
	"/api/v1/apikeys":                             "API keys",
	"/api/v1/experience":                          "the experience environment's console password",
	"/api/v1/deployments/{id}/draft":              "the deploy form's edit draft, environment values included; GET /api/v1/deployments/{id} describes the deployment",
	"/api/v1/branding":                            "console branding, mostly an inline logo image",
	"/api/v1/platform-tokens":                     "platform API tokens",
	"/api/v1/events":                              "a server-sent event stream",
	"/api/v1/instances/{uuid}/logs":               "a log stream; /api/v1/logs/search reads the same lines",
	"/api/v1/inference-templates/export/{digest}": "an archive download",
	"/api/v1/diagnostics/bundles/{id}/download":   "a diagnostics archive, recorded request contents included",
	"/api/v1/models/{id}/logo":                    "an image",
	"/api/v1/nodes/{uuid}/ssh":                    "SSH access settings",
	"/api/v1/nodes/{uuid}/terminal":               "an interactive terminal",
	"/api/v1/nodes/{uuid}/fs":                     "browses files on a node's host",
	"/api/v1/storages/{id}/files":                 "browses the files kept on a storage",
	"/api/v1/storages/{id}/files/download":        "a file or archive download",
	"/api/v1/images/community/browse":             "queries public registries outside the platform",
	"/api/v1/playground/openapi.json":             "the playground's proxy to model endpoints",
	"/api/v1/pull/registry-credentials":           "registry logins",
	"/api/v1/ssh-key":                             "the platform's SSH key",
	"/api/v1/kube/clusters/{uuid}/manifest":       "carries the cluster's enrollment token",
	"/api/v1/terminal/sessions":                   "terminal audit records, with client addresses",
}

// Readable reports whether the assistant may read a GET route pattern.
func Readable(pattern string) bool {
	if !strings.HasPrefix(pattern, "/api/v1/") {
		return false
	}
	_, denied := deniedRoutes[pattern]
	return !denied
}

// DeniedRoutes returns the denied patterns and why, for tests and docs.
func DeniedRoutes() map[string]string {
	out := make(map[string]string, len(deniedRoutes))
	for k, v := range deniedRoutes {
		out[k] = v
	}
	return out
}
