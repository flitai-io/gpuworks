package api

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
)

// A deployment's tokenize switch says whether the gateway forwards
// /v1/tokenize and /v1/detokenize to its engine (docs/reference/openai-gateway.md).
// It is open unless turned off; a change restarts nothing, the gateway's route
// table picks it up.

// setTokenize answers PUT /deployments/{id}/tokenize.
func (a *apiHandlers) setTokenize(w http.ResponseWriter, r *http.Request) {
	id := parseUint(chi.URLParam(r, "id"))
	dep, err := a.d.Store.GetDeployment(id)
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	var body dto.TokenizeRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if body.Enabled == nil {
		writeErr(w, http.StatusBadRequest, productError(ErrorFieldInvalid, "enabled is required", map[string]any{
			"field":  "enabled",
			"detail": "enabled must be true or false",
		}))
		return
	}
	if err := a.d.Store.SetDeploymentTokenize(dep.ID, *body.Enabled); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	dep.TokenizeDisabled = !*body.Enabled
	writeJSON(w, http.StatusOK, a.deploymentDTO(dep, true))
}
