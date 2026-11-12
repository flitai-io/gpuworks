package api

import (
	"encoding/json"
	"net/http"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// getSecuritySettings returns the platform security switches; any signed-in
// user may read them (the playground needs the preview policy and every
// client benefits from knowing whether /v1 requires a key).
func (a *apiHandlers) getSecuritySettings(w http.ResponseWriter, r *http.Request) {
	policy, err := a.d.Store.ReadSecurityPolicy()
	if err != nil {
		a.securitySettingsReadError(w, err)
		return
	}
	writeSecuritySettings(w, policy)
}

// patchSecuritySettings changes the fields the body names and leaves the rest;
// the router admits administrators only.
func (a *apiHandlers) patchSecuritySettings(w http.ResponseWriter, r *http.Request) {
	var req struct {
		PlaygroundPreviewNetwork *bool `json:"playground_preview_network"`
		InferenceOpenAccess      *bool `json:"inference_open_access"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	policy, err := a.d.Store.ReadSecurityPolicy()
	if err != nil {
		a.securitySettingsReadError(w, err)
		return
	}
	if req.PlaygroundPreviewNetwork != nil {
		policy.PlaygroundPreviewNetwork = *req.PlaygroundPreviewNetwork
	}
	if req.InferenceOpenAccess != nil {
		policy.InferenceOpenAccess = *req.InferenceOpenAccess
	}
	if err := a.d.Store.SetSecurityPolicy(policy); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	// Return the policy just committed. A second read could fail after the
	// successful write and incorrectly tell the caller the update failed.
	writeSecuritySettings(w, policy)
}

func writeSecuritySettings(w http.ResponseWriter, policy store.SecurityPolicy) {
	writeJSON(w, http.StatusOK, dto.SecuritySettings{
		PlaygroundPreviewNetwork: policy.PlaygroundPreviewNetwork,
		InferenceOpenAccess:      policy.InferenceOpenAccess,
	})
}

func (a *apiHandlers) securitySettingsReadError(w http.ResponseWriter, err error) {
	if a.d.Log != nil {
		a.d.Log.Warn("security settings read failed", "err", err)
	}
	writeProductError(w, http.StatusInternalServerError, ErrorSecuritySettingsUnavailable, "security settings could not be read; retry or inspect the server log", nil)
}
