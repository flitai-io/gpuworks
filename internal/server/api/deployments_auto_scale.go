package api

import (
	"encoding/json"
	"net/http"
	"slices"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// Scaling a multi-node or PD deployment the platform places: the request
// says how many replicas (or units) and may name nodes for the first
// placement of the new ones; the server picks the rest the way a create does
// (deployments_auto_units.go). A named node that cannot take its rank now is
// refused, as a single-node replica's is.

// scaling is the replica scaling the handlers share with the automation
// (planning.Scaling).
func (a *apiHandlers) scaling() planning.Scaling {
	return planning.Scaling{Store: a.d.Store, Resolve: a.networks().Resolve, Online: a.d.Online, UCXNetlinkHang: a.ucxNetlinkHangOf()}
}

// writeScaleErr answers a scaling failure with the HTTP status of its kind
// (planning.ScaleErrorKindOf).
func writeScaleErr(w http.ResponseWriter, err error) {
	kind, inner := planning.ScaleErrorKindOf(err)
	status := http.StatusInternalServerError
	switch kind {
	case planning.ScaleInvalid:
		status = http.StatusBadRequest
	case planning.ScaleConflict:
		status = http.StatusConflict
	case planning.ScaleGone:
		status = http.StatusNotFound
	}
	writeErr(w, status, inner)
}

// unitsPlacementMode is the deployment's placement mode as the API states
// it: "auto" or "manual" for a multi-node or PD deployment, "" for a
// single-node one (its draft says, from its node requirements).
func unitsPlacementMode(dep *store.Deployment) string {
	pd, _ := planning.PDOf(dep.Placement)
	if dep.NNodes <= 1 && pd == nil {
		return ""
	}
	if dep.UnitsAuto {
		return "auto"
	}
	return "manual"
}

// withoutPinnedGPUs is a PD placement with every unit's pinned cards
// dropped: the platform picks cards when it places the units.
func withoutPinnedGPUs(pd *planning.PDPlacement) *planning.PDPlacement {
	out := *pd
	out.Roles = map[string]planning.PDRole{}
	for name, role := range pd.Roles {
		units := slices.Clone(role.Units)
		for i := range units {
			units[i].GPUIndexes = nil
			followers := slices.Clone(units[i].Followers)
			for j := range followers {
				followers[j].GPUIndexes = nil
			}
			units[i].Followers = followers
		}
		role.Units = units
		out.Roles[name] = role
	}
	return &out
}

// setPlacementMode switches a multi-node or PD deployment between automatic
// placement and the operator's own. Nothing restarts: the units keep their
// nodes, and the mode decides what happens when one next starts. Switching
// to automatic drops the PD units' pinned cards, which the platform then
// picks.
func (a *apiHandlers) setPlacementMode(w http.ResponseWriter, r *http.Request) {
	dep, err := a.d.Store.GetDeployment(parseUint(chi.URLParam(r, "id")))
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	var req dto.PlacementModeRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if req.Mode != "auto" && req.Mode != "manual" {
		writeProductError(w, http.StatusBadRequest, ErrorFieldInvalid, "mode is auto or manual", map[string]any{"field": "mode"})
		return
	}
	if unitsPlacementMode(dep) == "" {
		writeProductError(w, http.StatusBadRequest, ErrorPlacementModeShape, "a single-node deployment states its placement in its node requirements", nil)
		return
	}
	placement := ""
	if pd, _ := planning.PDOf(dep.Placement); pd != nil && req.Mode == "auto" {
		encoded, err := planning.WithPD(dep.Placement, withoutPinnedGPUs(pd))
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		placement = encoded
	}
	if err := a.d.Store.SetDeploymentUnitsAuto(dep.ID, req.Mode == "auto", placement); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	updated, err := a.d.Store.GetDeployment(dep.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, a.deploymentDTO(updated, true))
}
