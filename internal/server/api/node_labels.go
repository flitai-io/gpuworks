package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/server/store"
)

func (a *apiHandlers) updateNodeLabels(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Labels map[string]string `json:"labels"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if req.Labels == nil {
		writeProductError(w, http.StatusBadRequest, ErrorNodeLabelsInvalid, "labels must be an object; use {} to clear labels", map[string]any{"detail": "labels must be an object; use {} to clear labels"})
		return
	}
	node, err := a.d.Store.SetNodeLabels(chi.URLParam(r, "uuid"), req.Labels)
	if err != nil {
		if errors.Is(err, store.ErrCacheDeletionPending) {
			writeStoreErr(w, err)
			return
		}
		status := http.StatusBadRequest
		if errors.Is(err, store.ErrNotFound) {
			status = http.StatusNotFound
		}
		if status == http.StatusNotFound {
			writeErr(w, status, err)
		} else {
			writeProductError(w, status, ErrorNodeLabelsInvalid, err.Error(), map[string]any{"detail": err.Error()})
		}
		return
	}
	// Labels decide which label-attached networks a node loads.
	a.syncNodesIfOnline(node.UUID)
	writeJSON(w, http.StatusOK, toNodeDTO(node))
}
