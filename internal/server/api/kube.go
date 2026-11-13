package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/kubemanifest"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// Kubernetes clusters join as one DaemonSet (docs/adr/0052-kubernetes-nodes.md):
// an administrator creates the
// cluster here, applies the manifest it renders, and every selected node's
// agent trades the cluster's enrollment token for a node token of its own.

// Defaults a new cluster takes for the fields its request leaves empty.
const (
	kubeDefaultNamespace   = "gpuworks-system"
	kubeDefaultStateDir    = kubemanifest.DefaultStateDir
	kubeDefaultEnginePorts = "40000-42767"
)

// kubeAgentImage is the image an entry's DaemonSet runs: its own, or the
// published one from its registry at this server's runtime revision, decided
// when the manifest is rendered so a server with a newer revision renders
// the newer image.
func kubeAgentImage(c store.KubeCluster) string {
	return firstNonEmpty(c.AgentImage, kubemanifest.AgentImage(c.AgentImageSource))
}

// kubeAgentImageSourceFor is the registry a cloud cluster's nodes pull the
// published agent image from: Huawei Cloud SWR in mainland China, where
// Docker Hub is out of reach, Docker Hub elsewhere.
func kubeAgentImageSourceFor(providerKind, region string) string {
	if providerKind == "aliyun" && strings.HasPrefix(region, "cn-") && region != "cn-hongkong" {
		return kubemanifest.AgentImageSWR
	}
	return kubemanifest.AgentImageDockerHub
}

func kubeManifestURL(c store.KubeCluster) string {
	return strings.TrimRight(c.ServerURL, "/") + "/kube/manifest.yaml?token=" + url.QueryEscape(c.Token)
}

func kubeClusterDTO(c store.KubeCluster, v kubeEntryView) dto.KubeCluster {
	out := dto.KubeCluster{
		ID: c.ID, UUID: c.UUID, Name: c.Name, Namespace: c.Namespace, ServerURL: c.ServerURL, AgentImage: c.AgentImage,
		AgentImageSource: firstNonEmpty(c.AgentImageSource, kubemanifest.AgentImageDockerHub), EffectiveAgentImage: kubeAgentImage(c),
		NodeSelector: c.NodeSelector, RuntimeClass: c.RuntimeClass, ImagePullSecret: c.ImagePullSecret,
		ContainerdSocket: c.ContainerdSocket, StateDir: c.StateDir, APIServer: c.APIServer,
		EnginePorts: c.EnginePorts, IdleReturnSeconds: c.IdleReturnSeconds, ClusterUID: c.ClusterUID,
		Priority: c.Priority, AllowPreemption: c.AllowPreemption,
		Revoked: c.RevokedAt != nil, NodeCount: v.nodes, Vendors: v.vendors, Source: v.source, CreatedAt: c.CreatedAt.UTC(),
		Stalled: kubeClusterStalled(c, time.Now().UTC()), PoolIDs: append([]string{}, v.poolIDs...),
		NamespaceSharedWith: append([]string{}, v.sharedNS...),
	}
	out.CanApply, out.CanRemove, out.OperationBlocked = kubeClusterOperations(c, v.privateAPI)
	if v.admin && c.RevokedAt == nil {
		out.ApplyCommand = "kubectl apply -f '" + kubeManifestURL(c) + "'"
	}
	if v.admin {
		out.RemoveCommands = kubemanifest.RemoveCommands(c.Namespace)
	}
	return out
}

// kubeManifestParams are the manifest parameters of an entry holding pools
// (none for an entry made by hand).
func kubeManifestParams(c store.KubeCluster, pools []store.KubeClusterPool) (kubemanifest.Params, error) {
	sel, err := kubemanifest.ParseNodeSelector(c.NodeSelector)
	if err != nil {
		return kubemanifest.Params{}, err
	}
	var poolLabel string
	if len(pools) > 0 {
		poolLabel = pools[0].LabelKey
	}
	return kubemanifest.Params{
		PoolLabel: poolLabel, PoolIDs: kubePoolIDs(pools),
		ClusterName: c.Name, Namespace: c.Namespace, ServerURL: c.ServerURL, Token: c.Token,
		AgentImage: kubeAgentImage(c), NodeSelector: sel,
		ImagePullSecret: c.ImagePullSecret, ContainerdSocket: c.ContainerdSocket, StateDir: c.StateDir, APIServer: c.APIServer,
		Priority: c.Priority, AllowPreemption: c.AllowPreemption,
	}, nil
}

func (a *apiHandlers) listKubeClusters(w http.ResponseWriter, r *http.Request) {
	rows, err := a.d.Store.KubeClusters()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	claims, _ := a.claimsFromRequest(r)
	// The assistant reads this list as the asking administrator but never
	// gets the apply command: it carries the enrollment token.
	admin := claims.Role == "admin" && !isAssistantRead(r)
	views, err := a.kubeEntryViews(r.Context(), rows, admin)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	items := make([]dto.KubeCluster, 0, len(rows))
	for _, c := range rows {
		items = append(items, kubeClusterDTO(c, views[c.ID]))
	}
	writeJSON(w, http.StatusOK, dto.NewList(items))
}

func kubeInvalid(w http.ResponseWriter, field, reason string) {
	writeProductError(w, http.StatusBadRequest, ErrorKubeClusterInvalid, field+": "+reason, map[string]any{"field": field, "reason": reason})
}

func (a *apiHandlers) createKubeCluster(w http.ResponseWriter, r *http.Request) {
	var req dto.KubeClusterRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	c := store.KubeCluster{
		Name:              strings.TrimSpace(req.Name),
		Namespace:         firstNonEmpty(strings.TrimSpace(req.Namespace), kubeDefaultNamespace),
		ServerURL:         strings.TrimRight(strings.TrimSpace(req.ServerURL), "/"),
		AgentImage:        strings.TrimSpace(req.AgentImage),
		AgentImageSource:  strings.TrimSpace(req.AgentImageSource),
		NodeSelector:      strings.TrimSpace(req.NodeSelector),
		RuntimeClass:      strings.TrimSpace(req.RuntimeClass),
		ImagePullSecret:   strings.TrimSpace(req.ImagePullSecret),
		ContainerdSocket:  strings.TrimSpace(req.ContainerdSocket),
		StateDir:          firstNonEmpty(strings.TrimSpace(req.StateDir), kubeDefaultStateDir),
		APIServer:         strings.TrimSpace(req.APIServer),
		EnginePorts:       firstNonEmpty(strings.TrimSpace(req.EnginePorts), kubeDefaultEnginePorts),
		IdleReturnSeconds: store.KubeDefaultIdleReturnSeconds,
		Priority:          store.KubeDefaultPriority,
	}
	if req.Priority != nil {
		c.Priority = *req.Priority
	}
	if req.AllowPreemption != nil {
		c.AllowPreemption = *req.AllowPreemption
	}
	// RuntimeClass is the NVIDIA instance pods' class; empty is decided on
	// each node: managed clusters make NVIDIA's runtime the default and
	// define no class, k3s defines "nvidia" beside a default runc. Other
	// vendors' devices come through the agent's device plugin and ignore it.
	if req.IdleReturnSeconds != nil {
		c.IdleReturnSeconds = *req.IdleReturnSeconds
	}
	if !a.kubeClusterSource(w, req, &c) {
		return
	}
	if c.ProviderID == nil && len(req.ProviderPoolIDs) > 0 {
		kubeInvalid(w, "provider_pool_ids", "node pools belong to a cluster of a cloud account; give provider_id and provider_cluster_id")
		return
	}
	if c.Name == "" || len(c.Name) > 128 {
		kubeInvalid(w, "name", "must be 1 to 128 bytes")
		return
	}
	var pools []store.KubeClusterPool
	if c.ProviderID != nil {
		// The entry selects nodes by the node pools it holds, of any vendors:
		// each agent finds out which accelerator its node has.
		if c.NodeSelector != "" {
			kubeInvalid(w, "node_selector", "an entry made from a cloud cluster selects its nodes by node pool")
			return
		}
		selected, cluster, ok := a.kubePoolSelection(w, r, *c.ProviderID, c.ProviderClusterID, req.ProviderPoolIDs)
		if !ok {
			return
		}
		pools = selected
		if c.AgentImageSource == "" {
			if p, err := a.d.Store.GetProvider(*c.ProviderID); err == nil {
				c.AgentImageSource = kubeAgentImageSourceFor(p.Kind, cluster.Region)
			}
		}
	}
	if !kubeValid(w, c, pools) {
		return
	}
	if rows, err := a.d.Store.KubeClusters(); err == nil {
		for _, other := range rows {
			if other.Name == c.Name {
				writeProductError(w, http.StatusConflict, ErrorKubeClusterNameTaken, "another cluster is named "+c.Name, map[string]any{"name": c.Name})
				return
			}
		}
	}
	created, err := a.d.Store.CreateKubeCluster(c, pools...)
	if errors.Is(err, store.ErrKubeClusterSourceTaken) {
		writeProductError(w, http.StatusConflict, ErrorKubeClusterSourceTaken, err.Error(), map[string]any{"cluster": c.ProviderClusterID})
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if a.d.Log != nil {
		a.d.Log.Info("kubernetes cluster created", "cluster", created.Name, "uuid", created.UUID)
	}
	views, err := a.kubeEntryViews(r.Context(), []store.KubeCluster{*created}, true)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusCreated, kubeClusterDTO(*created, views[created.ID]))
}

func (a *apiHandlers) revokeKubeCluster(w http.ResponseWriter, r *http.Request) {
	uuid := chi.URLParam(r, "uuid")
	if err := a.d.Store.RevokeKubeCluster(uuid); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, err)
			return
		}
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	c, err := a.d.Store.KubeClusterByUUID(uuid)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	views, err := a.kubeEntryViews(r.Context(), []store.KubeCluster{*c}, true)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, kubeClusterDTO(*c, views[c.ID]))
}

func (a *apiHandlers) deleteKubeCluster(w http.ResponseWriter, r *http.Request) {
	switch err := a.d.Store.DeleteKubeCluster(chi.URLParam(r, "uuid")); {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, store.ErrNotFound):
		writeErr(w, http.StatusNotFound, err)
	case errors.Is(err, store.ErrKubeClusterHasNodes):
		writeProductError(w, http.StatusConflict, ErrorKubeClusterHasNodes, "remove the cluster's nodes first", nil)
	default:
		writeErr(w, http.StatusInternalServerError, err)
	}
}

func (a *apiHandlers) writeKubeManifest(w http.ResponseWriter, c store.KubeCluster) {
	params, err := a.kubeClusterParams(c)
	var body string
	if err == nil {
		body, err = kubemanifest.Render(params)
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	w.Header().Set("Content-Type", "application/yaml; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(body))
}

// kubeClusterManifest serves the manifest to an administrator.
func (a *apiHandlers) kubeClusterManifest(w http.ResponseWriter, r *http.Request) {
	c, err := a.d.Store.KubeClusterByUUID(chi.URLParam(r, "uuid"))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, err)
			return
		}
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	a.writeKubeManifest(w, *c)
}

// kubeManifestByToken serves the manifest to whoever holds the enrollment
// token, which is what lets `kubectl apply -f <url>` fetch it without a
// login. The token is the manifest's own secret, so it reveals nothing more.
func (a *apiHandlers) kubeManifestByToken(w http.ResponseWriter, r *http.Request) {
	c, err := a.d.Store.KubeClusterByToken(r.URL.Query().Get("token"))
	if err != nil {
		if errors.Is(err, store.ErrKubeTokenUnknown) {
			writeErr(w, http.StatusNotFound, err)
			return
		}
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	a.writeKubeManifest(w, *c)
}

// kubeEnroll trades an enrollment token for the calling node's node token.
func (a *apiHandlers) kubeEnroll(w http.ResponseWriter, r *http.Request) {
	var req dto.KubeEnrollRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	node, cluster, err := a.d.Store.EnrollKubeNode(req.Token, req.ClusterUID, req.NodeName)
	switch {
	case errors.Is(err, store.ErrKubeTokenUnknown):
		writeProductError(w, http.StatusForbidden, ErrorKubeEnrollRefused, err.Error(), map[string]any{"reason": "unknown_token"})
		return
	case errors.Is(err, store.ErrKubeClusterMismatch):
		writeProductError(w, http.StatusForbidden, ErrorKubeEnrollRefused, err.Error(), map[string]any{"reason": "other_cluster"})
		return
	case errors.Is(err, store.ErrKubeNodeTaken):
		// Two agents on one node would share its state directory; the agent
		// stops rather than take another entry's identity.
		var taken *store.KubeNodeTakenError
		errors.As(err, &taken)
		writeProductError(w, http.StatusForbidden, ErrorKubeEnrollRefused, err.Error(), map[string]any{"reason": "node_taken", "entry": taken.Entry})
		return
	case err != nil:
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if a.d.Log != nil {
		a.d.Log.Info("kubernetes node enrolled", "cluster", cluster.Name, "node", node.KubeNodeName, "uuid", node.UUID)
	}
	writeJSON(w, http.StatusOK, dto.KubeEnrollResponse{NodeUUID: node.UUID, NodeToken: node.Token, IdleReturnSeconds: cluster.IdleReturnSeconds})
}

// kubeValid checks an entry's parameters, creating or editing it, and
// answers the request itself when one is wrong: the fields the manifest does
// not check, then a render with every parameter the manifest needs.
func kubeValid(w http.ResponseWriter, c store.KubeCluster, pools []store.KubeClusterPool) bool {
	if c.Priority < 0 || c.Priority > kubemanifest.PriorityMax {
		kubeInvalid(w, "priority", fmt.Sprintf("must be between 0 and %d", kubemanifest.PriorityMax))
		return false
	}
	if !kubemanifest.ValidAgentImageSource(c.AgentImageSource) {
		kubeInvalid(w, "agent_image_source", "must be one of "+strings.Join(kubemanifest.AgentImageSources, ", "))
		return false
	}
	if c.Name == "" || len(c.Name) > 128 {
		kubeInvalid(w, "name", "must be 1 to 128 bytes")
		return false
	}
	if c.IdleReturnSeconds < store.KubeKeepCards {
		kubeInvalid(w, "idle_return_seconds", "must be -1 (keep) or at least 0")
		return false
	}
	if c.ImagePullSecret != "" && !kubemanifest.ValidSecretName(c.ImagePullSecret) {
		kubeInvalid(w, "image_pull_secret", "must be the name of a Secret in the namespace")
		return false
	}
	if !kubemanifest.ValidRuntimeClass(c.RuntimeClass) {
		kubeInvalid(w, "runtime_class", "must be the name of a RuntimeClass (a DNS label)")
		return false
	}
	if _, err := proto.ParsePortRange(c.EnginePorts); err != nil {
		kubeInvalid(w, "engine_ports", "must be START-END")
		return false
	}
	// Render once so every parameter the manifest needs is checked before
	// the cluster exists.
	probe := c
	probe.Token = "x"
	params, err := kubeManifestParams(probe, pools)
	if err == nil {
		_, err = kubemanifest.Render(params)
	}
	if err != nil {
		kubeInvalid(w, "manifest", err.Error())
		return false
	}
	return true
}
