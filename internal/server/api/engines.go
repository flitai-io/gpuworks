package api

import (
	"net/http"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/server/api/dto"

	"gpuworks.ai/gpuworks/internal/server/deploy"
)

func (a *apiHandlers) engines(w http.ResponseWriter, r *http.Request) {
	vendor := r.URL.Query().Get("target_vendor")
	if vendor != "" && accelerator.NormalizeVendor(vendor) == "" {
		writeErr(w, http.StatusBadRequest, productError(ErrorFieldInvalid, "unknown target vendor", map[string]any{"field": "target_vendor", "detail": vendor}))
		return
	}
	out := make([]deploy.Profile, 0, len(deploy.EngineNames()))
	for _, name := range deploy.EngineNames() {
		if p, err := deploy.ProfileForTarget(name, r.URL.Query().Get("target_vendor")); err == nil {
			out = append(out, p)
		}
	}
	writeJSON(w, http.StatusOK, dto.NewList(out))
}
