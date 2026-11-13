package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/deployment"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// ---- deployments ----

func (a *apiHandlers) cloneDeployment(w http.ResponseWriter, r *http.Request) {
	id := parseUint(chi.URLParam(r, "id"))
	original, err := a.d.Store.GetDeployment(id)
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	clone := *original
	clone.ID = 0
	clone.Name = original.Name + "-clone"
	clone.ServedName = withServedNameSuffix(original.ServedName)
	clone.State = store.DeployActive
	clone.DesiredSleep = 0
	clone.CreatedAt = time.Time{}
	clone.UpdatedAt = time.Time{}
	var override struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&override); err != nil && !errors.Is(err, io.EOF) {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if strings.TrimSpace(override.Name) != "" {
		clone.Name = strings.TrimSpace(override.Name)
		clone.ServedName = clone.Name
	}
	if !validateName(w, clone.Name) || !validateTextField(w, "served_name", clone.ServedName, deployment.MaxNameRunes) {
		return
	}
	var created *store.Deployment
	// The copy runs where the original may: its requirements are rows of
	// their own, which copying the row does not carry.
	reqs, err := a.d.Store.DeploymentRequirements(original.ID)
	if err == nil {
		created, err = a.d.Store.CreateDeploymentWithRequirements(&clone, reqs)
	}
	if err != nil {
		writeUseCaseErr(w, deployment.CreateRowError(err))
		return
	}
	writeJSON(w, http.StatusCreated, a.deploymentDTO(created, true))
}

func (a *apiHandlers) getDeployment(w http.ResponseWriter, r *http.Request) {
	id := parseUint(chi.URLParam(r, "id"))
	dep, err := a.d.Store.GetDeployment(id)
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	writeJSON(w, http.StatusOK, a.deploymentDTO(dep, true))
}

func (a *apiHandlers) setRoutingPolicy(w http.ResponseWriter, r *http.Request) {
	id := parseUint(chi.URLParam(r, "id"))
	dep, err := a.d.Store.GetDeployment(id)
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	var body struct {
		RoutingPolicy string `json:"routing_policy"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	policy := deploy.NormalizeRoutingPolicy(body.RoutingPolicy)
	if !deploy.ValidRoutingPolicy(policy) {
		writeProductError(w, http.StatusBadRequest, ErrorRoutingPolicyInvalid, "routing_policy is invalid", map[string]any{"routing_policy": policy})
		return
	}
	if err := a.d.Store.SetDeploymentRoutingPolicy(dep.ID, policy); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	dep.RoutingPolicy = policy
	writeJSON(w, http.StatusOK, a.deploymentDTO(dep, true))
}

func (a *apiHandlers) stopDeployment(w http.ResponseWriter, r *http.Request) {
	id := parseUint(chi.URLParam(r, "id"))
	if err := a.d.Store.SetDeploymentState(id, store.DeployStopped); err != nil {
		switch {
		case errors.Is(err, store.ErrStateChanged):
			writeProductError(w, http.StatusConflict, ErrorDeploymentDeleting, "the deployment is being deleted", nil)
		case errors.Is(err, store.ErrNotFound):
			writeErr(w, http.StatusNotFound, err)
		default:
			writeErr(w, http.StatusInternalServerError, err)
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "stopping"})
}

func (a *apiHandlers) deleteDeployment(w http.ResponseWriter, r *http.Request) {
	id := parseUint(chi.URLParam(r, "id"))
	dep, err := a.d.Store.GetDeployment(id)
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	if r.URL.Query().Get("force") == "true" {
		// A force delete guarantees that the CONTROL-PLANE record goes away,
		// even when the node is unreachable. It must still give the node a
		// chance to tear the container down on the way out: sending nothing
		// left the container running with its GPU allocated and no deployment
		// left to explain it (measured at 37.4 GiB), and nothing would ever
		// reap it.
		//
		// The node is told by re-syncing its desired state, which is the
		// existing mechanism for this: agent-side SetDesiredFull walks the
		// instances it had and tears down every one that is no longer in the
		// desired set. That has to happen AFTER the row is gone, or the
		// re-sync would still be describing the deployment as live. It is
		// best-effort and not waited on - that is what force means - so an
		// unreachable node is logged and the delete proceeds regardless.
		nodeUUIDs := map[string]bool{}
		if insts, err := a.d.Store.InstancesForDeployment(id); err == nil {
			for _, inst := range insts {
				if node, err := a.d.Store.NodeByID(inst.NodeID); err == nil && node.UUID != "" {
					nodeUUIDs[node.UUID] = true
				}
			}
		}
		if err := a.d.Store.DeleteDeployment(id); err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		for uuid := range nodeUUIDs {
			if err := a.d.NodeSync.SyncNode(uuid); err != nil && a.d.Log != nil {
				a.d.Log.Warn("force delete: could not re-sync node to reap the container",
					"deployment", id, "node", uuid, "err", err)
			}
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
		return
	}
	if dep.State != store.DeployDeleting {
		if err := a.d.Store.SetDeploymentState(id, store.DeployDeleting); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				writeErr(w, http.StatusNotFound, err)
			} else {
				writeErr(w, http.StatusInternalServerError, err)
			}
			return
		}
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "deleting"})
}

// evaluate previews the assembled vllm command line for the deploy form.
