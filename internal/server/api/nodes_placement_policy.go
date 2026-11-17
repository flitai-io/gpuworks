package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// setNodePlacementPolicy changes knobs of a node's placement policy: the body
// names the knobs to change by key, each checked by its own setting, and the
// node comes back as GET /nodes/{uuid} returns it. Any value is written
// whether or not the knob takes effect on the node now
// (placement_policy_availability says): that depends on the node's agent
// and partition backend, which change under the operator.
func (a *apiHandlers) setNodePlacementPolicy(w http.ResponseWriter, r *http.Request) {
	var patch dto.NodePlacementPolicyPatch
	if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	uuid := chi.URLParam(r, "uuid")
	var field string
	old, updated, node, err := a.d.Store.SetNodePlacementPolicy(uuid, func(policy store.NodePlacementPolicy) (store.NodePlacementPolicy, error) {
		next, refused, err := patch.Apply(policy)
		field = refused
		return next, err
	})
	switch {
	case field != "":
		writeProductError(w, http.StatusBadRequest, ErrorFieldInvalid, err.Error(), map[string]any{"field": "placement_policy." + field, "detail": err.Error()})
		return
	case errors.Is(err, store.ErrNotFound):
		writeErr(w, http.StatusNotFound, err)
		return
	case err != nil:
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if old != updated && a.d.Log != nil {
		before, _ := dto.NodePlacementPolicyOf(old, false, false)
		after, _ := dto.NodePlacementPolicyOf(updated, false, false)
		a.d.Log.InfoContext(r.Context(), "node placement policy changed", "node", uuid, "from", before, "to", after, "by", a.agentUpgradeActor(r))
	}
	writeJSON(w, http.StatusOK, toNodeDTO(node))
}

// nodePlacementPolicyDictionary is GET /scheduling/node-placement-policies:
// the knobs a node placement policy has, for the UI to render by kind.
func (a *apiHandlers) nodePlacementPolicyDictionary(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, dto.NewList(dto.NodePlacementPolicyDictionaryOf()))
}
