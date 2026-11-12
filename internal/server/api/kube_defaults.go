package api

import (
	"context"
	"net/http"
	"time"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/kubemanifest"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// kubeStalledAfter is how long a cluster may wait for its first node before
// the list calls it stalled (dto.KubeCluster.Stalled).
const kubeStalledAfter = 5 * time.Minute

// kubeCloudListAge is how old the cloud's cluster listing may be when the
// entries' list consults it for whether an API server is private. The list is
// polled by every open page, so it must not ask the cloud on each poll.
const kubeCloudListAge = time.Minute

// kubeCloudListWait bounds what one request waits for the cloud: past it the
// entry is shown as it is without the cloud's word.
const kubeCloudListWait = 5 * time.Second

// Why the server cannot operate on a cluster (dto.KubeCluster.OperationBlocked).
const (
	kubeBlockedNoSource   = "no_source"
	kubeBlockedPrivateAPI = "private_api"
	kubeBlockedRevoked    = "revoked"
)

// kubeDefaults answers what a new cluster takes for the fields its request
// leaves empty, so the form can show them as hints without a copy of them.
func (a *apiHandlers) kubeDefaults(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, dto.KubeDefaults{
		Namespace: kubeDefaultNamespace, StateDir: kubeDefaultStateDir,
		EnginePorts: kubeDefaultEnginePorts, IdleReturnSeconds: store.KubeDefaultIdleReturnSeconds,
		IdleReturnKeep: store.KubeKeepCards, AgentImage: kubemanifest.AgentImage(kubemanifest.AgentImageDockerHub),
		StalledAfterSeconds: int(kubeStalledAfter / time.Second),
		AgentImageSource:    kubemanifest.AgentImageDockerHub, AgentImageSources: kubeAgentImageSourceList(),
		Priority: store.KubeDefaultPriority, PriorityMax: kubemanifest.PriorityMax, AllowPreemption: false,
	})
}

// kubeAgentImageSourceList is the registries the published agent image can
// be pulled from, the default first, each with the image it names.
func kubeAgentImageSourceList() []dto.KubeAgentImageSource {
	out := make([]dto.KubeAgentImageSource, 0, len(kubemanifest.AgentImageSources))
	for _, id := range kubemanifest.AgentImageSources {
		out = append(out, dto.KubeAgentImageSource{ID: id, Image: kubemanifest.AgentImage(id)})
	}
	return out
}

// kubeClusterStalled: no node has joined for longer than kubeStalledAfter.
func kubeClusterStalled(c store.KubeCluster, now time.Time) bool {
	return c.ClusterUID == "" && c.RevokedAt == nil && now.Sub(c.CreatedAt) > kubeStalledAfter
}

// kubePrivateAPIClusters names the entries whose cluster the cloud says has no
// public API server. It reads the cloud through a cache and gives up after
// kubeCloudListWait; an entry the cloud could not be asked about is absent, as
// is every entry made by hand.
func (a *apiHandlers) kubePrivateAPIClusters(ctx context.Context, rows []store.KubeCluster) map[uint]bool {
	out := map[uint]bool{}
	if a.d.Providers == nil {
		return out
	}
	ctx, cancel := context.WithTimeout(ctx, kubeCloudListWait)
	defer cancel()
	lists := map[uint]map[string]bool{} // provider -> cluster -> public API
	for _, c := range rows {
		if c.ProviderID == nil {
			continue
		}
		known, asked := lists[*c.ProviderID]
		if !asked {
			clusters, err := a.d.Providers.CachedClusters(ctx, *c.ProviderID, kubeCloudListAge)
			if err == nil {
				known = make(map[string]bool, len(clusters))
				for _, cl := range clusters {
					known[cl.ExternalID] = cl.PublicAPI
				}
			}
			lists[*c.ProviderID] = known
		}
		if public, ok := known[c.ProviderClusterID]; ok && !public {
			out[c.ID] = true
		}
	}
	return out
}

// kubeClusterOperations says whether the server can apply the entry's
// manifest and remove it again, and why not.
func kubeClusterOperations(c store.KubeCluster, privateAPI bool) (canApply, canRemove bool, blocked string) {
	switch {
	case c.ProviderID == nil:
		return false, false, kubeBlockedNoSource
	case privateAPI:
		return false, false, kubeBlockedPrivateAPI
	case c.RevokedAt != nil:
		return false, true, kubeBlockedRevoked
	}
	return true, true, ""
}
