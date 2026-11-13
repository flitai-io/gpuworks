package api

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/server/provider"
)

func (a *apiHandlers) deleteProvider(w http.ResponseWriter, r *http.Request) {
	if a.d.Providers == nil {
		writeErr(w, http.StatusServiceUnavailable, errNoProviders)
		return
	}
	id := parseUint(chi.URLParam(r, "id"))
	if err := a.d.Providers.PreDeleteCheck(r.Context(), id); err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, provider.ErrProviderInUse) {
			status = http.StatusConflict
		}
		writeErr(w, status, err)
		return
	}
	if err := a.d.Store.DeleteProvider(id); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}
