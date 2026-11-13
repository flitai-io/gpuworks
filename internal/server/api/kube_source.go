package api

import (
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/kubemanifest"
	"gpuworks.ai/gpuworks/internal/server/provider"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// Clusters of a cloud account become cluster entries, and the server can
// apply an entry's manifest to its cluster with a kubeconfig the cloud
// issues for that one operation, on the operator's confirmation (ADR 0063).

// providerClusters lists a cloud account's managed Kubernetes clusters, live,
// with the entry already made from each.
func (a *apiHandlers) providerClusters(w http.ResponseWriter, r *http.Request) {
	if a.d.Providers == nil {
		writeErr(w, http.StatusServiceUnavailable, errNoProviders)
		return
	}
	id := parseUint(chi.URLParam(r, "id"))
	clusters, err := a.d.Providers.Clusters(r.Context(), id)
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeErr(w, http.StatusNotFound, err)
		return
	case errors.Is(err, provider.ErrUnsupported):
		writeErr(w, http.StatusNotImplemented, err)
		return
	case err != nil:
		writeErr(w, statusProviderFailed, err)
		return
	}
	entries, err := a.d.Store.KubeClustersFromProvider(id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	entryIDs := make([]uint, 0, len(entries))
	for _, e := range entries {
		entryIDs = append(entryIDs, e.ID)
	}
	heldBy, err := a.d.Store.KubeClusterPools(entryIDs...)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	held := map[string]map[string]bool{} // cloud cluster -> pool ids its entry holds
	for cluster, e := range entries {
		held[cluster] = map[string]bool{}
		for _, p := range heldBy[e.ID] {
			held[cluster][p.PoolID] = true
		}
	}
	var kind string
	if p, err := a.d.Store.GetProvider(id); err == nil {
		kind = p.Kind
	}
	out := make([]dto.ProviderCluster, 0, len(clusters))
	for _, c := range clusters {
		pc := dto.ProviderCluster{ExternalID: c.ExternalID, Name: c.Name, Region: c.Region, Version: c.Version, State: c.State,
			Running: c.Running, PublicAPI: c.PublicAPI, GPUPools: make([]dto.ProviderClusterPool, 0, len(c.GPUPools)),
			AgentImageSource: kubeAgentImageSourceFor(kind, c.Region)}
		for _, p := range c.GPUPools {
			pc.GPUPools = append(pc.GPUPools, dto.ProviderClusterPool{ID: p.ID, Name: p.Name, GPUModel: p.GPUModel, GPUCount: p.GPUCount,
				Nodes: p.Nodes, Vendor: p.Vendor, InEntry: held[c.ExternalID][p.ID]})
		}
		if e, ok := entries[c.ExternalID]; ok {
			pc.Entry = &dto.KubeClusterRef{UUID: e.UUID, Name: e.Name}
		}
		out = append(out, pc)
	}
	writeJSON(w, http.StatusOK, dto.NewList(out))
}

// kubeClusterSource checks a create request's source and fills it in.
// It answers the request itself and returns false when the source is wrong.
func (a *apiHandlers) kubeClusterSource(w http.ResponseWriter, req dto.KubeClusterRequest, c *store.KubeCluster) bool {
	clusterID := strings.TrimSpace(req.ProviderClusterID)
	if req.ProviderID == nil && clusterID == "" {
		return true
	}
	if req.ProviderID == nil || clusterID == "" {
		kubeInvalid(w, "provider_cluster_id", "provider_id and provider_cluster_id go together")
		return false
	}
	if _, err := a.d.Store.GetProvider(*req.ProviderID); err != nil {
		kubeInvalid(w, "provider_id", "no such cloud account")
		return false
	}
	id := *req.ProviderID
	c.ProviderID, c.ProviderClusterID = &id, clusterID
	return true
}

// kubeClusterSources describes each entry's source for the list.
func (a *apiHandlers) kubeClusterSources(rows []store.KubeCluster) map[uint]*dto.KubeClusterSource {
	out := map[uint]*dto.KubeClusterSource{}
	var providers map[uint]store.Provider
	for _, c := range rows {
		if c.ProviderID == nil {
			continue
		}
		if providers == nil {
			providers = map[uint]store.Provider{}
			if list, err := a.d.Store.ListProviders(); err == nil {
				for _, p := range list {
					providers[p.ID] = p
				}
			}
		}
		p := providers[*c.ProviderID]
		out[c.ID] = &dto.KubeClusterSource{ProviderID: *c.ProviderID, ProviderName: p.Name, ProviderKind: p.Kind, ClusterID: c.ProviderClusterID}
	}
	return out
}

// applyKubeCluster applies the entry's manifest to its cluster.
func (a *apiHandlers) applyKubeCluster(w http.ResponseWriter, r *http.Request) {
	a.kubeClusterOperation(w, r, false)
}

// removeKubeCluster removes the entry's manifest objects from its cluster.
func (a *apiHandlers) removeKubeCluster(w http.ResponseWriter, r *http.Request) {
	a.kubeClusterOperation(w, r, true)
}

func (a *apiHandlers) kubeClusterOperation(w http.ResponseWriter, r *http.Request, remove bool) {
	var req dto.KubeClusterOperationRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if req.DryRun == nil {
		kubeInvalid(w, "dry_run", "say true to check or false to apply")
		return
	}
	dryRun := *req.DryRun
	if a.d.Providers == nil {
		writeErr(w, http.StatusServiceUnavailable, errNoProviders)
		return
	}
	c, err := a.d.Store.KubeClusterByUUID(chi.URLParam(r, "uuid"))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, err)
			return
		}
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if c.ProviderID == nil {
		writeProductError(w, http.StatusConflict, ErrorKubeClusterNoSource, "the cluster was not connected from a cloud account; apply the manifest with kubectl", nil)
		return
	}
	if c.RevokedAt != nil && !remove {
		kubeInvalid(w, "uuid", "the cluster's enrollment token is revoked")
		return
	}
	params, err := a.kubeClusterParams(*c)
	var manifest string
	if err == nil {
		manifest, err = kubemanifest.Render(params)
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	op := a.d.Providers.ApplyToCluster
	if remove {
		op = a.d.Providers.RemoveFromCluster
	}
	results, err := op(r.Context(), *c.ProviderID, c.ProviderClusterID, manifest, dryRun)
	out := dto.KubeClusterOperation{DryRun: dryRun, Results: make([]dto.KubeObjectResult, 0, len(results))}
	for _, res := range results {
		out.Results = append(out.Results, dto.KubeObjectResult{Kind: res.Kind, Name: res.Name, Namespace: res.Namespace,
			Action: res.Action, Reason: res.Reason, Error: res.Error})
	}
	var opErr *provider.ClusterOperationError
	switch {
	case err == nil:
		if a.d.Log != nil && !dryRun {
			a.d.Log.Info("kubernetes cluster manifest", "cluster", c.Name, "removed", remove, "objects", len(results))
		}
		writeJSON(w, http.StatusOK, out)
	case errors.As(err, &opErr):
		out.Code, out.Error = ErrorKubeClusterOperationFailed, err.Error()
		out.Params = &dto.KubeOperationErrorParams{Step: opErr.Step, Reason: opErr.Err.Error()}
		writeJSON(w, statusProviderFailed, out)
	case errors.Is(err, store.ErrNotFound):
		// The account the entry came from is gone.
		writeProductError(w, http.StatusConflict, ErrorKubeClusterNoSource, "the cloud account the cluster was connected from no longer exists", nil)
	case errors.Is(err, provider.ErrUnsupported):
		writeErr(w, http.StatusNotImplemented, err)
	default:
		writeErr(w, statusProviderFailed, err)
	}
}
