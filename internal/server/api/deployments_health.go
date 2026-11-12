package api

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/deployment"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// A deployment's health check (internal/server/healthcheck) is a setting of
// the deployment: on or off and how long a check waits. Changing it restarts
// nothing; the next round of checks follows it.

// setHealthCheck answers PUT /deployments/{id}/health-check.
func (a *apiHandlers) setHealthCheck(w http.ResponseWriter, r *http.Request) {
	id := parseUint(chi.URLParam(r, "id"))
	dep, err := a.d.Store.GetDeployment(id)
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	var body dto.HealthCheckRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	threshold := dep.HealthCheckThresholdSec
	if body.ThresholdSec != 0 {
		if !store.ValidHealthCheckThresholdSec(body.ThresholdSec) {
			writeErr(w, http.StatusBadRequest, deployment.HealthThresholdError(body.ThresholdSec))
			return
		}
		threshold = body.ThresholdSec
	}
	if body.Enabled {
		model, _ := a.d.Store.GetModel(dep.ModelID)
		if !deployment.HealthCheckable(model) {
			writeErr(w, http.StatusBadRequest, deployment.HealthUncheckableError())
			return
		}
	}
	if err := a.d.Store.SetDeploymentHealthCheck(dep.ID, body.Enabled, threshold); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	dep.HealthCheck, dep.HealthCheckThresholdSec = body.Enabled, threshold
	writeJSON(w, http.StatusOK, a.deploymentDTO(dep, true))
}

// deploymentHealth is a deployment's health check on the wire.
func (a *apiHandlers) deploymentHealth(d *store.Deployment) dto.DeploymentHealth {
	if a.d.Monitor != nil {
		return a.d.Monitor.Health(d.ID, d.HealthCheck, d.HealthCheckThresholdSec)
	}
	status := "off"
	if d.HealthCheck {
		status = "pending"
	}
	return dto.DeploymentHealth{Status: status, ThresholdSec: d.HealthCheckThresholdSec, Recent: []dto.HealthProbe{}}
}
