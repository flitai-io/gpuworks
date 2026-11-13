package api

import (
	"net/http"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

// TestRouterInventory guards the registration surface separately from handler
// tests. A handler can have excellent unit coverage while accidentally being
// omitted from Router or mounted under the wrong method/path.
func TestRouterInventoryMatchesCompleteContract(t *testing.T) {
	h := Router(Deps{})
	r, ok := h.(chi.Routes)
	if !ok {
		t.Fatalf("Router returned %T, want chi.Routes for inventory inspection", h)
	}
	var routes []string
	if err := chi.Walk(r, func(method string, path string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		routes = append(routes, method+" "+path)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	sort.Strings(routes)
	want := strings.Split(strings.TrimSpace(fullRouteInventory), "\n")
	if !slices.Equal(routes, want) {
		t.Fatalf("router inventory changed; update the contract deliberately\ngot:\n%s\nwant:\n%s", strings.Join(routes, "\n"), strings.Join(want, "\n"))
	}
}

func TestRouterRejectsUnknownPathAndUnsupportedMethod(t *testing.T) {
	rig := newTokenRig(t)
	t.Cleanup(func() { _ = rig.st.Close() })
	for _, tc := range []struct {
		name   string
		method string
		path   string
		want   int
	}{
		{name: "unknown path", method: http.MethodGet, path: "/api/v1/does-not-exist", want: http.StatusNotFound},
		{name: "unsupported method", method: http.MethodTrace, path: "/api/v1/nodes", want: http.StatusMethodNotAllowed},
		{name: "community browse unsupported method", method: http.MethodTrace, path: "/api/v1/images/community/browse", want: http.StatusMethodNotAllowed},
		{name: "model copy unsupported method", method: http.MethodTrace, path: "/api/v1/models/1/copy", want: http.StatusMethodNotAllowed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, token := range []string{"", rig.session} {
				want := tc.want
				if token == "" {
					want = http.StatusUnauthorized
				}
				recorder := rig.do(token, tc.method, tc.path, "")
				if recorder.Code != want {
					t.Fatalf("authenticated=%t %s %s status=%d body=%s, want %d", token != "", tc.method, tc.path, recorder.Code, recorder.Body.String(), want)
				}
			}
		})
	}
}

func TestAPIKeyCreationRouteRequiresAuthenticationAndPost(t *testing.T) {
	rig := newTokenRig(t)
	t.Cleanup(func() { _ = rig.st.Close() })
	for _, tc := range []struct {
		method, path, token string
		status              int
	}{
		{http.MethodPost, "/api/v1/apikeys", "", http.StatusUnauthorized},
		{http.MethodDelete, "/api/v1/apikeys/gw-000000000", "", http.StatusUnauthorized},
		{http.MethodPost, "/api/v1/apikeys/gw-000000000", rig.session, http.StatusMethodNotAllowed},
		{http.MethodDelete, "/api/v1/apikeys/gw-000000000", rig.session, http.StatusNotFound},
		{http.MethodPatch, "/api/v1/apikeys", rig.session, http.StatusMethodNotAllowed},
		{http.MethodPost, "/api/v1/apikeys/not-a-route/unknown", rig.session, http.StatusNotFound},
		{http.MethodPut, "/api/v1/apikeys/gw-x/scope", "", http.StatusUnauthorized},
		{http.MethodPost, "/api/v1/apikeys/gw-x/scope", rig.session, http.StatusMethodNotAllowed},
	} {
		if rec := rig.do(tc.token, tc.method, tc.path, `{"name":"client"}`); rec.Code != tc.status {
			t.Errorf("%s %s: %d, want %d", tc.method, tc.path, rec.Code, tc.status)
		}
	}
}

const fullRouteInventory = `
DELETE /api/v1/ai/configs/{id}
DELETE /api/v1/apikeys/{prefix}
DELETE /api/v1/benchmarks/{id}
DELETE /api/v1/deployments/{id}
DELETE /api/v1/deployments/{id}/schedule/{sid}
DELETE /api/v1/deployments/{id}/wake-waiting
DELETE /api/v1/diagnostics/debug-session
DELETE /api/v1/image-series/{id}
DELETE /api/v1/images/{id}
DELETE /api/v1/images/{id}/nodes/{uuid}
DELETE /api/v1/inference-templates/spec
DELETE /api/v1/kube/clusters/{uuid}
DELETE /api/v1/models/{id}
DELETE /api/v1/models/{id}/nodes/{uuid}
DELETE /api/v1/models/{id}/storages/{storage_id}
DELETE /api/v1/networks/{id}
DELETE /api/v1/networks/{id}/attachments/{attachmentID}
DELETE /api/v1/nodes/{uuid}
DELETE /api/v1/nodes/{uuid}/ssh
DELETE /api/v1/platform-tokens/{id}
DELETE /api/v1/providers/{id}
DELETE /api/v1/providers/{id}/instances/{externalID}
DELETE /api/v1/pull/credentials/{id}
DELETE /api/v1/pull/proxies/{id}
DELETE /api/v1/pull/registry-credentials/{id}
DELETE /api/v1/pull/source-rules/{id}
DELETE /api/v1/pull/sync-groups/{id}
DELETE /api/v1/storages/{id}
DELETE /api/v1/storages/{id}/cache
DELETE /api/v1/storages/{id}/files
GET /api/v1/agent-upgrades
GET /api/v1/agent-upgrades/summary
GET /api/v1/agent-upgrades/{id}
GET /api/v1/agent-upgrades/{id}/items
GET /api/v1/agent/connect
GET /api/v1/agent/rejects
GET /api/v1/agent/upgrade-errors
GET /api/v1/ai/available
GET /api/v1/ai/configs
GET /api/v1/ai/instances/{uuid}/findings
GET /api/v1/ai/models/{id}/fit
GET /api/v1/ai/presets
GET /api/v1/analytics/gpu-time
GET /api/v1/analytics/gpus
GET /api/v1/analytics/key-usage
GET /api/v1/analytics/quality
GET /api/v1/analytics/usage
GET /api/v1/apikeys
GET /api/v1/auth/me
GET /api/v1/batches
GET /api/v1/batches/dictionary
GET /api/v1/batches/{id}
GET /api/v1/batches/{id}/files/{kind}
GET /api/v1/benchmarks
GET /api/v1/benchmarks/meta
GET /api/v1/benchmarks/{id}
GET /api/v1/benchmarks/{id}/curve
GET /api/v1/benchmarks/{id}/points/{seq}/detail
GET /api/v1/benchmarks/{id}/points/{seq}/timeline
GET /api/v1/benchmarks/{id}/report
GET /api/v1/branding
GET /api/v1/dashboard/summary
GET /api/v1/data-retention
GET /api/v1/data-retention/status
GET /api/v1/deployments
GET /api/v1/deployments/{id}
GET /api/v1/deployments/{id}/candidates
GET /api/v1/deployments/{id}/draft
GET /api/v1/deployments/{id}/events
GET /api/v1/deployments/{id}/schedule
GET /api/v1/dgpus
GET /api/v1/diagnostics/bundles
GET /api/v1/diagnostics/bundles/{id}
GET /api/v1/diagnostics/bundles/{id}/download
GET /api/v1/diagnostics/debug-session
GET /api/v1/engines
GET /api/v1/events
GET /api/v1/experience
GET /api/v1/gpus
GET /api/v1/healthz
GET /api/v1/images
GET /api/v1/images/community/browse
GET /api/v1/images/community/repos
GET /api/v1/images/engines
GET /api/v1/images/versions
GET /api/v1/images/{id}
GET /api/v1/inference-templates
GET /api/v1/inference-templates/chips
GET /api/v1/inference-templates/export/{digest}
GET /api/v1/inference-templates/index
GET /api/v1/inference-templates/model/*
GET /api/v1/inference-templates/my-chips
GET /api/v1/inference-templates/organizations
GET /api/v1/inference-templates/schema
GET /api/v1/inference-templates/summary
GET /api/v1/inference-templates/weight-models
GET /api/v1/inference-templates/yaml
GET /api/v1/instances
GET /api/v1/instances/{uuid}
GET /api/v1/instances/{uuid}/events
GET /api/v1/instances/{uuid}/launch-facts
GET /api/v1/instances/{uuid}/logs
GET /api/v1/kube/clusters
GET /api/v1/kube/clusters/{uuid}/manifest
GET /api/v1/kube/defaults
GET /api/v1/logs/context
GET /api/v1/logs/histogram
GET /api/v1/logs/search
GET /api/v1/mcp/status
GET /api/v1/metrics/query
GET /api/v1/models
GET /api/v1/models/{id}
GET /api/v1/models/{id}/logo
GET /api/v1/monitor/deployments/{id}
GET /api/v1/monitor/deployments/{id}/latency
GET /api/v1/monitor/deployments/{id}/requests
GET /api/v1/monitor/distributions
GET /api/v1/monitor/metrics
GET /api/v1/monitor/overview
GET /api/v1/monitor/requests/lookup
GET /api/v1/monitor/series
GET /api/v1/networks
GET /api/v1/networks/{id}
GET /api/v1/nodes
GET /api/v1/nodes/facets
GET /api/v1/nodes/{uuid}
GET /api/v1/nodes/{uuid}/fs
GET /api/v1/nodes/{uuid}/health-checks
GET /api/v1/nodes/{uuid}/health-checks/detail
GET /api/v1/nodes/{uuid}/health-records
GET /api/v1/nodes/{uuid}/network
GET /api/v1/nodes/{uuid}/networks
GET /api/v1/nodes/{uuid}/pull-proxy
GET /api/v1/nodes/{uuid}/replicas
GET /api/v1/nodes/{uuid}/ssh
GET /api/v1/nodes/{uuid}/terminal
GET /api/v1/nodes/{uuid}/terminal/settings
GET /api/v1/notices
GET /api/v1/peer-transfers
GET /api/v1/platform-tokens
GET /api/v1/playground/models
GET /api/v1/playground/openapi.json
GET /api/v1/providers
GET /api/v1/providers/kinds
GET /api/v1/providers/{id}
GET /api/v1/providers/{id}/clusters
GET /api/v1/providers/{id}/instances
GET /api/v1/pull/credentials
GET /api/v1/pull/metadata-sources
GET /api/v1/pull/proxies
GET /api/v1/pull/registry-credentials
GET /api/v1/pull/source-rules
GET /api/v1/pull/sync-groups
GET /api/v1/routing-policies
GET /api/v1/scheduling/activity
GET /api/v1/scheduling/board
GET /api/v1/scheduling/history
GET /api/v1/scheduling/overview
GET /api/v1/scheduling/timeline
GET /api/v1/scheduling/yield-policies
GET /api/v1/security/settings
GET /api/v1/server/listen
GET /api/v1/server/outbound-policy
GET /api/v1/settings
GET /api/v1/ssh-key
GET /api/v1/storages
GET /api/v1/storages/settings
GET /api/v1/storages/usage
GET /api/v1/storages/{id}
GET /api/v1/storages/{id}/control-planes
GET /api/v1/storages/{id}/files
GET /api/v1/storages/{id}/files/download
GET /api/v1/storages/{id}/model-files
GET /api/v1/telemetry/export
GET /api/v1/terminal/sessions
GET /api/v1/updates
GET /api/v1/usage-report
GET /api/v1/usage-report/preview
GET /healthz
GET /kube/manifest.yaml
PATCH /api/v1/ai/configs/{id}
PATCH /api/v1/branding
PATCH /api/v1/images/versions/{id}
PATCH /api/v1/images/{id}
PATCH /api/v1/models/{id}
PATCH /api/v1/models/{id}/inference-config
PATCH /api/v1/nodes/management-roles
PATCH /api/v1/nodes/{uuid}/accelerator
PATCH /api/v1/nodes/{uuid}/name
PATCH /api/v1/nodes/{uuid}/runtime
PATCH /api/v1/security/settings
PATCH /api/v1/storages/settings
PATCH /api/v1/storages/{id}
PATCH /api/v1/storages/{id}/node-overrides
POST /api/v1/agent-upgrades
POST /api/v1/agent-upgrades/preview
POST /api/v1/agent-upgrades/{id}/continue-preview
POST /api/v1/agent-upgrades/{id}/items/{uuid}/release
POST /api/v1/agent-upgrades/{id}/stop
POST /api/v1/ai/ask
POST /api/v1/ai/configs
POST /api/v1/ai/configs/test
POST /api/v1/apikeys
POST /api/v1/auth/activity
POST /api/v1/auth/login
POST /api/v1/auth/logout
POST /api/v1/auth/password
POST /api/v1/batches
POST /api/v1/batches/{id}/cancel
POST /api/v1/benchmarks
POST /api/v1/benchmarks/datasets/import
POST /api/v1/benchmarks/models
POST /api/v1/benchmarks/probe
POST /api/v1/benchmarks/tokenizers/import
POST /api/v1/benchmarks/{id}/cancel
POST /api/v1/deployments
POST /api/v1/deployments/preflight
POST /api/v1/deployments/{id}/catalog-template
POST /api/v1/deployments/{id}/clone
POST /api/v1/deployments/{id}/restart
POST /api/v1/deployments/{id}/schedule
POST /api/v1/deployments/{id}/sleep
POST /api/v1/deployments/{id}/start
POST /api/v1/deployments/{id}/stop
POST /api/v1/deployments/{id}/units/retry
POST /api/v1/deployments/{id}/units/suggest
POST /api/v1/deployments/{id}/wake
POST /api/v1/deployments/{id}/wake-on-request
POST /api/v1/diagnostics/bundles
POST /api/v1/diagnostics/debug-session
POST /api/v1/evaluate
POST /api/v1/experience/password
POST /api/v1/experience/reset
POST /api/v1/images/import
POST /api/v1/images/preview-archive-url
POST /api/v1/images/sync
POST /api/v1/images/{id}/nodes/{uuid}/cancel
POST /api/v1/images/{id}/nodes/{uuid}/inspect
POST /api/v1/images/{id}/nodes/{uuid}/sync
POST /api/v1/images/{id}/nodes/{uuid}/sync/probe
POST /api/v1/images/{id}/nodes/{uuid}/sync/retry
POST /api/v1/inference-templates/clone
POST /api/v1/inference-templates/evaluate
POST /api/v1/inference-templates/import
POST /api/v1/inference-templates/official
POST /api/v1/inference-templates/sync
POST /api/v1/instances/{uuid}/restart
POST /api/v1/instances/{uuid}/sleep
POST /api/v1/instances/{uuid}/wake
POST /api/v1/kube/clusters
POST /api/v1/kube/clusters/{uuid}/apply
POST /api/v1/kube/clusters/{uuid}/remove
POST /api/v1/kube/clusters/{uuid}/revoke
POST /api/v1/kube/enroll
POST /api/v1/mcp
POST /api/v1/models
POST /api/v1/models/local
POST /api/v1/models/local/preview
POST /api/v1/models/preview-url
POST /api/v1/models/quick-register
POST /api/v1/models/register-url
POST /api/v1/models/{id}/copy
POST /api/v1/models/{id}/download
POST /api/v1/models/{id}/download/cancel
POST /api/v1/models/{id}/metadata/refresh
POST /api/v1/models/{id}/nodes/{uuid}/cache/retry
POST /api/v1/models/{id}/nodes/{uuid}/sync
POST /api/v1/models/{id}/nodes/{uuid}/sync/cancel
POST /api/v1/models/{id}/nodes/{uuid}/sync/probe
POST /api/v1/models/{id}/register-path
POST /api/v1/networks
POST /api/v1/networks/{id}/attachments
POST /api/v1/networks/{id}/attachments/batch
POST /api/v1/networks/{id}/hcas
POST /api/v1/networks/{id}/tests
POST /api/v1/networks/{id}/tests/estimate
POST /api/v1/nodes
POST /api/v1/nodes/install
POST /api/v1/nodes/precheck
POST /api/v1/nodes/{uuid}/connection-diagnostics
POST /api/v1/nodes/{uuid}/doctor
POST /api/v1/nodes/{uuid}/evict
POST /api/v1/nodes/{uuid}/gpus/recheck
POST /api/v1/nodes/{uuid}/recovery/probe
POST /api/v1/nodes/{uuid}/runtime/restart
POST /api/v1/nodes/{uuid}/token/regenerate
POST /api/v1/nodes/{uuid}/upgrade
POST /api/v1/notices/read
POST /api/v1/platform-tokens
POST /api/v1/providers
POST /api/v1/providers/{id}/install
POST /api/v1/providers/{id}/instances
POST /api/v1/providers/{id}/instances/{externalID}/power
POST /api/v1/providers/{id}/instances/{externalID}/resize
POST /api/v1/providers/{id}/sync
POST /api/v1/pull/credentials
POST /api/v1/pull/credentials/{id}/test
POST /api/v1/pull/proxies
POST /api/v1/pull/registry-credentials
POST /api/v1/pull/registry-credentials/{id}/test
POST /api/v1/pull/source-rules
POST /api/v1/pull/source-rules/test
POST /api/v1/pull/sync-groups
POST /api/v1/server/outbound-policy/check
POST /api/v1/server/restart
POST /api/v1/ssh-key/rotate
POST /api/v1/storages
POST /api/v1/storages/probe
POST /api/v1/storages/{id}/cache/reconcile
POST /api/v1/storages/{id}/files/mkdir
POST /api/v1/storages/{id}/files/rename
POST /api/v1/storages/{id}/import
POST /api/v1/storages/{id}/scan
POST /api/v1/telemetry/export/test
POST /api/v1/updates/prefetch
POST /api/v1/updates/server
POST /api/v1/updates/server/upload
PUT /api/v1/apikeys/{prefix}/scope
PUT /api/v1/data-retention
PUT /api/v1/deployments/{id}
PUT /api/v1/deployments/{id}/dgpu
PUT /api/v1/deployments/{id}/health-check
PUT /api/v1/deployments/{id}/node-replacement
PUT /api/v1/deployments/{id}/pd
PUT /api/v1/deployments/{id}/placement-mode
PUT /api/v1/deployments/{id}/replicas
PUT /api/v1/deployments/{id}/requirements
PUT /api/v1/deployments/{id}/routing
PUT /api/v1/deployments/{id}/schedule/{sid}
PUT /api/v1/deployments/{id}/tokenize
PUT /api/v1/deployments/{id}/yield-policy
PUT /api/v1/experience/enabled
PUT /api/v1/kube/clusters/{uuid}
PUT /api/v1/kube/clusters/{uuid}/pools
PUT /api/v1/mcp/settings
PUT /api/v1/networks/{id}
PUT /api/v1/networks/{id}/attachments/{attachmentID}
PUT /api/v1/nodes/{uuid}/gpus/scheduling
PUT /api/v1/nodes/{uuid}/labels
PUT /api/v1/nodes/{uuid}/management-roles
PUT /api/v1/nodes/{uuid}/network
PUT /api/v1/nodes/{uuid}/scheduling
PUT /api/v1/nodes/{uuid}/ssh
PUT /api/v1/providers/{id}
PUT /api/v1/pull/credentials/{id}
PUT /api/v1/pull/proxies/order
PUT /api/v1/pull/proxies/{id}
PUT /api/v1/pull/registry-credentials/{id}
PUT /api/v1/pull/source-rules/order
PUT /api/v1/pull/source-rules/{id}
PUT /api/v1/pull/sync-groups/{id}
PUT /api/v1/server/listen
PUT /api/v1/server/outbound-policy
PUT /api/v1/settings/{key}
PUT /api/v1/ssh-key
PUT /api/v1/storages/{id}/files/upload
PUT /api/v1/telemetry/export
PUT /api/v1/updates/auto-check
PUT /api/v1/updates/channel
PUT /api/v1/usage-report/enabled
`
