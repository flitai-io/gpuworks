package api

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strings"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/kubemanifest"
	"gpuworks.ai/gpuworks/internal/server/provider"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// An entry made from a cloud cluster holds a set of that cluster's GPU node
// pools, of any vendors: the DaemonSet runs on the nodes of those pools and
// nowhere else (docs/adr/0063-apply-to-cloud-clusters.md).

// kubeEntryView is what the DTO of one entry needs beyond its row.
type kubeEntryView struct {
	nodes      int
	vendors    []dto.KubeVendorNodes
	admin      bool
	source     *dto.KubeClusterSource
	privateAPI bool
	poolIDs    []string
	// sharedNS names the other entries that may be of the same cluster and
	// use the same namespace (kubeNamespaceShared).
	sharedNS []string
}

// kubeEntryViews gathers the views of the given entries in one pass: node
// counts, sources, the pools they hold and the cloud's word on private API
// servers.
func (a *apiHandlers) kubeEntryViews(ctx context.Context, rows []store.KubeCluster, admin bool) (map[uint]kubeEntryView, error) {
	byVendor, err := a.d.Store.KubeClusterNodeVendors()
	if err != nil {
		return nil, err
	}
	ids := make([]uint, 0, len(rows))
	for _, c := range rows {
		ids = append(ids, c.ID)
	}
	pools, err := a.d.Store.KubeClusterPools(ids...)
	if err != nil {
		return nil, err
	}
	sources := a.kubeClusterSources(rows)
	private := a.kubePrivateAPIClusters(ctx, rows)
	all, err := a.d.Store.KubeClusters()
	if err != nil {
		return nil, err
	}
	out := make(map[uint]kubeEntryView, len(rows))
	for _, c := range rows {
		nodes, vendors := kubeVendorNodes(byVendor[c.ID])
		out[c.ID] = kubeEntryView{nodes: nodes, vendors: vendors, admin: admin, source: sources[c.ID], privateAPI: private[c.ID],
			poolIDs: kubePoolIDs(pools[c.ID]), sharedNS: kubeNamespaceShared(c, all)}
	}
	return out, nil
}

// kubeNamespaceShared names the other entries with c's namespace that may
// be of the same Kubernetes cluster: the manifest names its objects by
// namespace, so in one cluster the entry applied later takes the other's
// agents over. Entries whose clusters are known to differ (different
// kube-system UIDs, or different cloud clusters) are left out; an entry no
// node has joined yet may be of any cluster.
func kubeNamespaceShared(c store.KubeCluster, all []store.KubeCluster) []string {
	out := []string{}
	for _, o := range all {
		switch {
		case o.ID == c.ID || o.Namespace != c.Namespace:
		case o.ClusterUID != "" && c.ClusterUID != "" && o.ClusterUID != c.ClusterUID:
		case o.ProviderID != nil && c.ProviderID != nil && (*o.ProviderID != *c.ProviderID || o.ProviderClusterID != c.ProviderClusterID):
		default:
			out = append(out, o.Name)
		}
	}
	sort.Strings(out)
	return out
}

// kubeVendorNodes is an entry's node total and its nodes by vendor, most
// first and then by name, so the list reads the same on every poll.
func kubeVendorNodes(counts map[string]int) (int, []dto.KubeVendorNodes) {
	total := 0
	out := make([]dto.KubeVendorNodes, 0, len(counts))
	for v, n := range counts {
		total += n
		out = append(out, dto.KubeVendorNodes{Vendor: v, Nodes: n})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Nodes != out[j].Nodes {
			return out[i].Nodes > out[j].Nodes
		}
		return out[i].Vendor < out[j].Vendor
	})
	return total, out
}

func kubePoolIDs(pools []store.KubeClusterPool) []string {
	out := make([]string, 0, len(pools))
	for _, p := range pools {
		out = append(out, p.PoolID)
	}
	return out
}

// kubeClusterParams are the manifest parameters of a stored entry, with the
// node pools it holds.
func (a *apiHandlers) kubeClusterParams(c store.KubeCluster) (kubemanifest.Params, error) {
	pools, err := a.d.Store.KubeClusterPools(c.ID)
	if err != nil {
		return kubemanifest.Params{}, err
	}
	return kubeManifestParams(c, pools[c.ID])
}

// kubePoolSelection resolves the node pools a request names against the cloud
// account's live listing, answering the request itself and returning false
// when they cannot be held: none, unknown to the cluster, or of other vendors
// than the entry's. entryVendor is the vendor of an entry that exists (its
// pools may not change it); empty for a new entry, whose vendor is then the
// pools'.
func (a *apiHandlers) kubePoolSelection(w http.ResponseWriter, r *http.Request, providerID uint, clusterID string, ids []string) (pools []store.KubeClusterPool, cluster provider.Cluster, ok bool) {
	wanted := make([]string, 0, len(ids))
	seen := map[string]bool{}
	for _, id := range ids {
		if id = strings.TrimSpace(id); id != "" && !seen[id] {
			seen[id] = true
			wanted = append(wanted, id)
		}
	}
	if len(wanted) == 0 {
		kubeInvalid(w, "provider_pool_ids", "select at least one node pool of the cluster")
		return nil, provider.Cluster{}, false
	}
	if a.d.Providers == nil {
		writeErr(w, http.StatusServiceUnavailable, errNoProviders)
		return nil, provider.Cluster{}, false
	}
	clusters, err := a.d.Providers.Clusters(r.Context(), providerID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		kubeInvalid(w, "provider_id", "no such cloud account")
		return nil, provider.Cluster{}, false
	case errors.Is(err, provider.ErrUnsupported):
		writeErr(w, http.StatusNotImplemented, err)
		return nil, provider.Cluster{}, false
	case err != nil:
		writeErr(w, statusProviderFailed, err)
		return nil, provider.Cluster{}, false
	}
	var found *provider.Cluster
	for i := range clusters {
		if clusters[i].ExternalID == clusterID {
			found = &clusters[i]
		}
	}
	if found == nil {
		kubeInvalid(w, "provider_cluster_id", "the cloud account has no such cluster")
		return nil, provider.Cluster{}, false
	}
	byID := map[string]provider.ClusterPool{}
	for _, p := range found.GPUPools {
		byID[p.ID] = p
	}
	for _, id := range wanted {
		p, known := byID[id]
		if !known {
			kubeInvalid(w, "provider_pool_ids", "node pool "+id+" is not a GPU node pool of the cluster")
			return nil, provider.Cluster{}, false
		}
		if p.LabelKey == "" {
			kubeInvalid(w, "provider_pool_ids", "the cloud gave no label for node pool "+id)
			return nil, provider.Cluster{}, false
		}
		pools = append(pools, store.KubeClusterPool{PoolID: p.ID, LabelKey: p.LabelKey})
	}
	return pools, *found, true
}

// setKubeClusterPools replaces the node pools an entry made from a cloud
// cluster holds. The cluster itself changes only when the manifest is applied
// again (POST …/apply, or kubectl).
func (a *apiHandlers) setKubeClusterPools(w http.ResponseWriter, r *http.Request) {
	var req dto.KubeClusterPoolsRequest
	if !decodeJSONBody(w, r, &req) {
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
		writeProductError(w, http.StatusConflict, ErrorKubeClusterNoSource, "the cluster was not connected from a cloud account; its nodes are chosen by its node selector", nil)
		return
	}
	pools, _, ok := a.kubePoolSelection(w, r, *c.ProviderID, c.ProviderClusterID, req.ProviderPoolIDs)
	if !ok {
		return
	}
	held, err := a.d.Store.KubeClusterPools(c.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	// The pools replace a node selector the entry may still carry.
	next := *c
	next.NodeSelector = ""
	if !a.confirmEntrySelection(w, r, *c, next, held[c.ID], pools, req.ConfirmDropInstances) {
		return
	}
	updated, err := a.d.Store.SetKubeClusterPools(c.UUID, pools)
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeErr(w, http.StatusNotFound, err)
		return
	case err != nil:
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if a.d.Log != nil {
		a.d.Log.Info("kubernetes cluster pools changed", "cluster", updated.Name, "pools", len(pools))
	}
	views, err := a.kubeEntryViews(r.Context(), []store.KubeCluster{*updated}, true)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, kubeClusterDTO(*updated, views[updated.ID]))
}
