package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/provision"
	"gpuworks.ai/gpuworks/internal/server/store"
)

type nodeDeleteResponse struct {
	Status           string `json:"status"`
	AgentUninstalled bool   `json:"agent_uninstalled"`
}

func (a *apiHandlers) deleteNode(w http.ResponseWriter, r *http.Request) {
	force := false
	if values, present := r.URL.Query()["force"]; present {
		var err error
		if len(values) != 1 {
			writeProductError(w, http.StatusBadRequest, ErrorNodeDeleteForceInvalid, "force must be a single boolean", nil)
			return
		}
		force, err = strconv.ParseBool(values[0])
		if err != nil {
			writeProductError(w, http.StatusBadRequest, ErrorNodeDeleteForceInvalid, "force must be a single boolean", nil)
			return
		}
	}
	uuid := chi.URLParam(r, "uuid")
	node, err := a.d.Store.NodeByUUID(uuid)
	if err != nil {
		a.writeNodeDeleteError(w, err)
		return
	}
	instances, err := a.d.Store.InstancesForNode(node.ID)
	if err != nil {
		a.writeNodeDeleteError(w, err)
		return
	}
	// A row only waiting to be removed (a replica moved away while the node
	// was offline) does not keep the node: it goes with it.
	running := 0
	for _, inst := range instances {
		if inst.State != proto.InstRemoving && inst.State != proto.InstPendingRemoval {
			running++
		}
	}
	if running != 0 {
		writeProductError(w, http.StatusConflict, ErrorNodeDeleteHasInstances, "stop deployments before deleting the node", nil)
		return
	}
	// Keep the agent available to finish an unresolved deletion. Hold the
	// reservation across SSH uninstall so no new eviction or launch can start
	// after the preflight check but before the node record is removed.
	release, reserved := a.d.Store.TryEvictNodeCache(node.ID)
	if !reserved {
		writeProductError(w, http.StatusConflict, ErrorNodeDeleteCacheBusy, "node cache is busy; retry node deletion later", nil)
		return
	}
	defer release()
	files, err := a.d.Store.ModelFilesForNode(node.ID)
	if err != nil {
		a.writeNodeDeleteError(w, err)
		return
	}
	for _, file := range files {
		if file.State == proto.MFDeleting {
			a.writeNodeDeleteError(w, store.ErrCacheDeletionPending)
			return
		}
	}
	// A Kubernetes node's agent belongs to its cluster's DaemonSet, which
	// nothing here can uninstall; the node comes back while the DaemonSet
	// still selects it (the cluster's manifest is where it is removed).
	if !force && node.KubeClusterID == 0 {
		if !node.HasSSHCredential {
			writeProductError(w, http.StatusConflict, ErrorNodeDeleteSSHRequired,
				"normal deletion requires saved SSH credentials to uninstall the agent; node retained. Configure SSH and retry, or uninstall the agent manually and use force=true to remove only its management record", nil)
			return
		}
		if err := a.uninstallNode(r.Context(), node); err != nil {
			writeProductError(w, http.StatusConflict, ErrorNodeUninstallUnconfirmed,
				"agent uninstall over SSH was not confirmed; node and SSH credentials retained. Fix SSH and retry, or uninstall the agent manually and use force=true to remove only its management record: "+err.Error(), map[string]any{"detail": err.Error()})
			return
		}
	}
	if err := a.d.Store.DeleteNode(uuid); err != nil {
		if !force {
			err = wrapProductError(ErrorNodeDeleteCleanupFailed,
				"agent uninstalled, but node record cleanup failed: "+err.Error(), map[string]any{"detail": err.Error()}, err)
		}
		a.writeNodeDeleteError(w, err)
		return
	}
	if a.d.CardHealth != nil {
		a.d.CardHealth.ForgetNode(uuid, node.ID)
	}
	if a.d.KickNode != nil {
		a.d.KickNode(uuid)
	}
	writeJSON(w, http.StatusOK, nodeDeleteResponse{Status: "deleted", AgentUninstalled: !force})
}

func (a *apiHandlers) uninstallNode(ctx context.Context, node *store.Node) error {
	uninstaller, ok := a.d.NodeInstaller.(interface {
		UninstallAgent(context.Context, provision.SSHProbeRequest) error
	})
	if !ok {
		return fmt.Errorf("SSH agent uninstaller is not configured")
	}
	credential, secrets, err := a.d.Store.GetNodeSSHCredential(node.UUID)
	if err != nil {
		return fmt.Errorf("read node SSH credentials: %w", err)
	}
	return uninstaller.UninstallAgent(ctx, provision.SSHProbeRequest{
		Host: credential.Host, Port: credential.Port, User: credential.User,
		// The token picks this node's agent on a host that runs several.
		ServerURL: node.ServerURL, Token: node.Token,
		SSHAuth: provision.SSHAuth{
			Password: secrets.Password, PrivateKeyPEM: secrets.PrivateKeyPEM, PlatformKey: a.platformKey(),
		},
	})
}

func (a *apiHandlers) writeNodeDeleteError(w http.ResponseWriter, err error) {
	status := http.StatusConflict
	if errors.Is(err, store.ErrNotFound) {
		status = http.StatusNotFound
	}
	writeErr(w, status, err)
}
