package api

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// The proxies the platform's requests for each node go through, by node
// requirements like the source rules: the first entry that selects a node is
// its proxy. A change is sent to every online node as a network-only update.

func pullProxyDTO(spec store.PullProxySpec, matched int) dto.PullProxy {
	return dto.PullProxy{
		ID: spec.ID, Position: spec.Position, HTTPProxy: spec.HTTPProxy, HTTPSProxy: spec.HTTPSProxy,
		NoProxy: spec.NoProxy, Username: spec.Username, HasPassword: spec.HasSecret,
		ControlPlane: spec.ControlPlane, Note: spec.Note,
		NodeRequirements: requirementsDTO(spec.Requirements), MatchedNodes: matched,
	}
}

func (a *apiHandlers) listPullProxies(w http.ResponseWriter, r *http.Request) {
	proxies, err := a.d.Store.PullProxies()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	selectors := make([]map[string][]string, len(proxies))
	for i, proxy := range proxies {
		selectors[i] = proxy.Requirements
	}
	counts, err := a.matchedNodeCounts(selectors)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	out := make([]dto.PullProxy, 0, len(proxies))
	for i, proxy := range proxies {
		out = append(out, pullProxyDTO(proxy, counts[i]))
	}
	writeJSON(w, http.StatusOK, dto.NewList(out))
}

func decodePullProxy(w http.ResponseWriter, r *http.Request) (store.PullProxySpec, *string, bool) {
	var req dto.PullProxyRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return store.PullProxySpec{}, nil, false
	}
	return store.PullProxySpec{
		HTTPProxy: req.HTTPProxy, HTTPSProxy: req.HTTPSProxy, NoProxy: req.NoProxy, Username: req.Username,
		ControlPlane: req.ControlPlane, Note: req.Note, Requirements: req.NodeRequirements,
	}, req.Password, true
}

func (a *apiHandlers) createPullProxy(w http.ResponseWriter, r *http.Request) {
	spec, password, ok := decodePullProxy(w, r)
	if !ok {
		return
	}
	plain := ""
	if password != nil {
		plain = *password
	}
	created, err := a.d.Store.CreatePullProxy(spec, plain)
	if err != nil {
		writePullSettingError(w, err)
		return
	}
	a.writePullProxy(w, http.StatusCreated, created)
}

func (a *apiHandlers) updatePullProxy(w http.ResponseWriter, r *http.Request) {
	id := parseUint(chi.URLParam(r, "id"))
	spec, password, ok := decodePullProxy(w, r)
	if !ok {
		return
	}
	updated, err := a.d.Store.UpdatePullProxy(id, spec, password)
	if err != nil {
		writePullSettingError(w, err)
		return
	}
	a.writePullProxy(w, http.StatusOK, updated)
}

func (a *apiHandlers) writePullProxy(w http.ResponseWriter, status int, spec store.PullProxySpec) {
	counts, err := a.matchedNodeCounts([]map[string][]string{spec.Requirements})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	a.requestOnlineNetworkSync()
	writeJSON(w, status, pullProxyDTO(spec, counts[0]))
}

func (a *apiHandlers) deletePullProxy(w http.ResponseWriter, r *http.Request) {
	if err := a.d.Store.DeletePullProxy(parseUint(chi.URLParam(r, "id"))); err != nil {
		writePullSettingError(w, err)
		return
	}
	a.requestOnlineNetworkSync()
	w.WriteHeader(http.StatusNoContent)
}

func (a *apiHandlers) orderPullProxies(w http.ResponseWriter, r *http.Request) {
	var req dto.PullProxyOrder
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := a.d.Store.ReorderPullProxies(req.IDs); err != nil {
		writePullSettingError(w, err)
		return
	}
	a.requestOnlineNetworkSync()
	a.listPullProxies(w, r)
}

// requestOnlineNetworkSync sends every online node its proxy again, as a
// network-only update queued per node (hub.RequestNetworkSync): an operator's
// edit does not wait on any agent.
func (a *apiHandlers) requestOnlineNetworkSync() {
	if a.d.NodeSync == nil {
		return
	}
	nodes, err := a.d.Store.NodeUUIDsByID()
	if err != nil {
		return
	}
	for _, uuid := range nodes {
		a.requestNetworkSync(uuid)
	}
}

// getNodePullProxy is the proxy one node uses and its runtime's view of it:
// what the node detail shows, and what the proxies page previews for a node.
func (a *apiHandlers) getNodePullProxy(w http.ResponseWriter, r *http.Request) {
	node, err := a.d.Store.NodeByUUID(chi.URLParam(r, "uuid"))
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	selection, err := planning.PullSettingsFor(a.d.Store, a.networks().Resolve, node)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	out := dto.NodePullProxy{Runtime: a.nodeRuntime(node, selection.Network())}
	if selection.Proxy != nil {
		counts, err := a.matchedNodeCounts([]map[string][]string{selection.Proxy.Requirements})
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		proxy := pullProxyDTO(*selection.Proxy, counts[0])
		out.Proxy = &proxy
	}
	writeJSON(w, http.StatusOK, out)
}

// nodeNetworkRemoved answers the per-node network override routes, which the
// proxy entries replaced.
func (a *apiHandlers) nodeNetworkRemoved(w http.ResponseWriter, _ *http.Request) {
	writeProductError(w, http.StatusGone, ErrorSettingRemoved,
		"a node's network override has been removed; configure proxies under /api/v1/pull/proxies and site rules under /api/v1/pull/source-rules",
		map[string]any{"setting": "network", "route": "/api/v1/pull/proxies"})
}
