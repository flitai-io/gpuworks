package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// Pull settings are where nodes get images and models from: pull source rules
// (which registries to try for which images, on which nodes), sync groups
// (which nodes copy from one another) and registry logins. Every node reads
// them when it pulls; no deployment carries a choice of its own.

// writePullSettingError answers a write the store refused: a refused input is
// the client's to fix (400), a missing row a 404, anything else a 500.
func writePullSettingError(w http.ResponseWriter, err error) {
	var settingErr *store.PullSettingError
	switch {
	case errors.As(err, &settingErr):
		params := map[string]any{"field": settingErr.Field, "detail": settingErr.Err.Error()}
		if settingErr.Reason != "" {
			params["reason"] = settingErr.Reason
		}
		for key, value := range settingErr.Params {
			params[key] = value
		}
		writeProductError(w, http.StatusBadRequest, ErrorPullSettingInvalid, err.Error(), params)
	case errors.Is(err, store.ErrNotFound):
		writeErr(w, http.StatusNotFound, err)
	default:
		writeErr(w, http.StatusInternalServerError, err)
	}
}

// matchedNodeCounts counts, for each selector, the nodes it selects now. All
// nodes are read once and the facts the selectors need loaded once.
func (a *apiHandlers) matchedNodeCounts(selectors []map[string][]string) ([]int, error) {
	counts := make([]int, len(selectors))
	nodes, err := a.d.Store.ListNodes()
	if err != nil {
		return nil, err
	}
	merged := map[string][]string{}
	for _, reqs := range selectors {
		for key, values := range reqs {
			merged[key] = append(merged[key], values...)
		}
	}
	facts := a.resolveForRequirements(nodePointers(nodes), merged)
	for i, reqs := range selectors {
		for j := range nodes {
			if planning.MatchRequirements(&nodes[j], facts, reqs) == nil {
				counts[i]++
			}
		}
	}
	return counts, nil
}

func requirementsDTO(reqs map[string][]string) map[string][]string {
	if reqs == nil {
		return map[string][]string{}
	}
	return reqs
}

func pullSourceRuleDTO(spec store.PullSourceRuleSpec, matched int) dto.PullSourceRule {
	endpoints := spec.Endpoints
	if endpoints == nil {
		endpoints = []string{}
	}
	return dto.PullSourceRule{
		ID: spec.ID, Position: spec.Position, Kind: spec.Kind(), Match: spec.Match, Endpoints: endpoints,
		AllowUpstream: spec.AllowUpstream, ControlPlane: spec.ControlPlane, Note: spec.Note,
		NodeRequirements: requirementsDTO(spec.Requirements), MatchedNodes: matched,
	}
}

func (a *apiHandlers) listPullSourceRules(w http.ResponseWriter, r *http.Request) {
	rules, err := a.d.Store.PullSourceRules()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	selectors := make([]map[string][]string, len(rules))
	for i, rule := range rules {
		selectors[i] = rule.Requirements
	}
	counts, err := a.matchedNodeCounts(selectors)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	out := make([]dto.PullSourceRule, 0, len(rules))
	for i, rule := range rules {
		out = append(out, pullSourceRuleDTO(rule, counts[i]))
	}
	writeJSON(w, http.StatusOK, dto.NewList(out))
}

func decodePullSourceRule(w http.ResponseWriter, r *http.Request) (store.PullSourceRuleSpec, bool) {
	var req dto.PullSourceRuleRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return store.PullSourceRuleSpec{}, false
	}
	if req.AllowUpstream == nil {
		writeProductError(w, http.StatusBadRequest, ErrorPullSettingInvalid, "allow_upstream is required",
			map[string]any{"field": "allow_upstream", "detail": "allow_upstream is required"})
		return store.PullSourceRuleSpec{}, false
	}
	return store.PullSourceRuleSpec{
		Match: req.Match, Endpoints: req.Endpoints, AllowUpstream: *req.AllowUpstream, ControlPlane: req.ControlPlane, Note: req.Note,
		Requirements: req.NodeRequirements,
	}, true
}

func (a *apiHandlers) createPullSourceRule(w http.ResponseWriter, r *http.Request) {
	spec, ok := decodePullSourceRule(w, r)
	if !ok {
		return
	}
	created, err := a.d.Store.CreatePullSourceRule(spec)
	if err != nil {
		writePullSettingError(w, err)
		return
	}
	a.writePullSourceRule(w, http.StatusCreated, created)
}

func (a *apiHandlers) updatePullSourceRule(w http.ResponseWriter, r *http.Request) {
	spec, ok := decodePullSourceRule(w, r)
	if !ok {
		return
	}
	updated, err := a.d.Store.UpdatePullSourceRule(parseUint(chi.URLParam(r, "id")), spec)
	if err != nil {
		writePullSettingError(w, err)
		return
	}
	a.writePullSourceRule(w, http.StatusOK, updated)
}

func (a *apiHandlers) writePullSourceRule(w http.ResponseWriter, status int, spec store.PullSourceRuleSpec) {
	counts, err := a.matchedNodeCounts([]map[string][]string{spec.Requirements})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, status, pullSourceRuleDTO(spec, counts[0]))
}

func (a *apiHandlers) deletePullSourceRule(w http.ResponseWriter, r *http.Request) {
	if err := a.d.Store.DeletePullSourceRule(parseUint(chi.URLParam(r, "id"))); err != nil {
		writePullSettingError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *apiHandlers) orderPullSourceRules(w http.ResponseWriter, r *http.Request) {
	var req dto.PullSourceRuleOrder
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := a.d.Store.ReorderPullSourceRules(req.IDs); err != nil {
		writePullSettingError(w, err)
		return
	}
	a.listPullSourceRules(w, r)
}

func syncGroupDTO(spec store.SyncGroupSpec, matched int) dto.SyncGroup {
	return dto.SyncGroup{
		ID: spec.ID, Name: spec.Name,
		NodeRequirements: requirementsDTO(spec.Requirements), RateLimitKiBPerSecond: spec.RateLimitKiB, MatchedNodes: matched,
	}
}

func (a *apiHandlers) listSyncGroups(w http.ResponseWriter, r *http.Request) {
	groups, err := a.d.Store.SyncGroups()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	selectors := make([]map[string][]string, len(groups))
	for i, group := range groups {
		selectors[i] = group.Requirements
	}
	counts, err := a.matchedNodeCounts(selectors)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	out := make([]dto.SyncGroup, 0, len(groups))
	for i, group := range groups {
		out = append(out, syncGroupDTO(group, counts[i]))
	}
	writeJSON(w, http.StatusOK, dto.NewList(out))
}

func decodeSyncGroup(w http.ResponseWriter, r *http.Request) (store.SyncGroupSpec, bool) {
	var req dto.SyncGroupRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return store.SyncGroupSpec{}, false
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		writeJSONBodyError(w, err)
		return store.SyncGroupSpec{}, false
	}
	return store.SyncGroupSpec{Name: req.Name, Requirements: req.NodeRequirements, RateLimitKiB: req.RateLimitKiBPerSecond}, true
}

func (a *apiHandlers) createSyncGroup(w http.ResponseWriter, r *http.Request) {
	spec, ok := decodeSyncGroup(w, r)
	if !ok {
		return
	}
	created, err := a.d.Store.CreateSyncGroup(spec)
	if err != nil {
		writePullSettingError(w, err)
		return
	}
	a.writeSyncGroup(w, http.StatusCreated, created)
}

func (a *apiHandlers) updateSyncGroup(w http.ResponseWriter, r *http.Request) {
	spec, ok := decodeSyncGroup(w, r)
	if !ok {
		return
	}
	updated, err := a.d.Store.UpdateSyncGroup(parseUint(chi.URLParam(r, "id")), spec)
	if err != nil {
		writePullSettingError(w, err)
		return
	}
	a.writeSyncGroup(w, http.StatusOK, updated)
}

func (a *apiHandlers) writeSyncGroup(w http.ResponseWriter, status int, spec store.SyncGroupSpec) {
	counts, err := a.matchedNodeCounts([]map[string][]string{spec.Requirements})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, status, syncGroupDTO(spec, counts[0]))
}

func (a *apiHandlers) deleteSyncGroup(w http.ResponseWriter, r *http.Request) {
	if err := a.d.Store.DeleteSyncGroup(parseUint(chi.URLParam(r, "id"))); err != nil {
		writePullSettingError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// syncOnlineNodes asks for every online node's desired state to be re-sent.
func (a *apiHandlers) syncOnlineNodes() { a.requestSyncAllNodes() }

// nodeRuntime is the node's container runtime and, for a daemon runtime, the
// proxy it really uses against the one the node is configured with.
func (a *apiHandlers) nodeRuntime(node *store.Node, effective proto.NetworkConfig) dto.NodeRuntime {
	out := dto.NodeRuntime{Driver: node.RuntimeDriver}
	if a.d.NodeRuntime == nil {
		return out
	}
	if observed := a.d.NodeRuntime.RuntimeProxy(node.UUID); observed != nil {
		out.Proxy = runtimeProxyDTO(observed, effective)
	}
	return out
}

func runtimeProxyDTO(observed *proto.RuntimeProxyStatus, effective proto.NetworkConfig) *dto.RuntimeProxy {
	return &dto.RuntimeProxy{
		HTTPProxy: observed.HTTPProxy, HTTPSProxy: observed.HTTPSProxy, NoProxy: observed.NoProxy,
		ConfiguredHTTPProxy: observed.ConfiguredHTTPProxy, ConfiguredHTTPSProxy: observed.ConfiguredHTTPSProxy,
		// Masked here as well: an agent older than this server reports the
		// drop-in's proxy with its password.
		WrittenHTTPProxy:  proto.MaskProxyCredentials(observed.WrittenHTTPProxy),
		WrittenHTTPSProxy: proto.MaskProxyCredentials(observed.WrittenHTTPSProxy),
		WrittenNoProxy:    observed.WrittenNoProxy,
		LiveRestore:       observed.LiveRestore, DaemonJSONProxy: observed.DaemonJSONProxy, Systemd: observed.Systemd,
		Privilege: observed.Privilege, Error: observed.Error,
		Applied: runtimeProxyApplied(observed, effective),
	}
}

// runtimeProxyApplied reports whether dockerd already runs with what the
// platform set for the node. With no proxy set the platform leaves dockerd's
// own proxy (an operator's file) alone; what is judged then is only whether
// dockerd runs with what it is configured with, which is false right after
// the platform removed its drop-in until dockerd restarts.
func runtimeProxyApplied(observed *proto.RuntimeProxyStatus, effective proto.NetworkConfig) bool {
	desiredHTTP, desiredHTTPS := strings.TrimSpace(effective.HTTPProxy), strings.TrimSpace(effective.HTTPSProxy)
	if desiredHTTP == "" && desiredHTTPS == "" {
		// The platform's drop-in is still there: the agent has not removed it
		// yet (or may not), so dockerd will keep the proxy that was cleared.
		if observed.WrittenHTTPProxy != "" || observed.WrittenHTTPSProxy != "" {
			return false
		}
		if observed.ConfiguredKnown && !observed.DaemonJSONProxy {
			return observed.HTTPProxy == observed.ConfiguredHTTPProxy && observed.HTTPSProxy == observed.ConfiguredHTTPSProxy
		}
		if observed.WrittenHTTPProxy == "" && observed.WrittenHTTPSProxy == "" {
			return true
		}
	}
	return proto.MaskProxyCredentials(desiredHTTP) == observed.HTTPProxy && proto.MaskProxyCredentials(desiredHTTPS) == observed.HTTPSProxy
}

// restartNodeRuntime restarts a node's container runtime daemon so the proxy
// configured for the node takes effect. The console asks first: every
// container on the node stops unless the daemon keeps them (live-restore).
func (a *apiHandlers) restartNodeRuntime(w http.ResponseWriter, r *http.Request) {
	node, err := a.d.Store.NodeByUUID(chi.URLParam(r, "uuid"))
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	if a.d.NodeRuntime == nil || (a.d.Online != nil && !a.d.Online(node.UUID)) {
		writeProductError(w, http.StatusConflict, ErrorRuntimeRestartFailed, "the node is offline", map[string]any{"detail": "the node is offline"})
		return
	}
	observed, err := a.d.NodeRuntime.RestartRuntime(r.Context(), node.UUID)
	if err != nil {
		writeProductError(w, http.StatusConflict, ErrorRuntimeRestartFailed, err.Error(), map[string]any{"detail": err.Error()})
		return
	}
	selection, err := planning.PullSettingsFor(a.d.Store, a.networks().Resolve, node)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	out := dto.NodeRuntime{Driver: node.RuntimeDriver}
	if observed != nil {
		out.Proxy = runtimeProxyDTO(observed, selection.Network())
	}
	writeJSON(w, http.StatusOK, out)
}
