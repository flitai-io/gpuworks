package api

import (
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// apiKeyScopeRequest is the scope part of creating or editing a key.
// AllDeployments left out means "every deployment" exactly when no
// deployment is named, so a client that predates scopes keeps working.
type apiKeyScopeRequest struct {
	AllDeployments *bool  `json:"all_deployments"`
	DeploymentIDs  []uint `json:"deployment_ids"`
}

// scope validates the request's shape; whether the deployments exist is the
// store's to check, in the same transaction as the write.
func (req apiKeyScopeRequest) scope(w http.ResponseWriter) (store.APIKeyScope, bool) {
	all := len(req.DeploymentIDs) == 0
	if req.AllDeployments != nil {
		all = *req.AllDeployments
	}
	if all == (len(req.DeploymentIDs) > 0) {
		writeProductError(w, http.StatusBadRequest, ErrorAPIKeyScopeInvalid,
			"name deployment_ids for a key limited to some deployments, or none with all_deployments", nil)
		return store.APIKeyScope{}, false
	}
	for _, id := range req.DeploymentIDs {
		if id == 0 {
			writeProductError(w, http.StatusBadRequest, ErrorAPIKeyScopeInvalid, "deployment IDs must be positive", nil)
			return store.APIKeyScope{}, false
		}
	}
	return store.APIKeyScope{Restricted: !all, DeploymentIDs: req.DeploymentIDs}, true
}

// writeAPIKeyScopeError maps a store refusal of a scope.
func writeAPIKeyScopeError(w http.ResponseWriter, err error) {
	var unknown *store.UnknownDeploymentsError
	switch {
	case errors.As(err, &unknown):
		writeProductError(w, http.StatusBadRequest, ErrorAPIKeyDeploymentUnknown, err.Error(), map[string]any{"ids": unknown.IDs})
	case errors.Is(err, store.ErrAPIKeyScopeEmpty):
		writeProductError(w, http.StatusBadRequest, ErrorAPIKeyScopeInvalid, err.Error(), nil)
	case errors.Is(err, store.ErrNotFound):
		writeProductError(w, http.StatusNotFound, ErrorAPIKeyNotFound, "no live API key has this prefix", nil)
	default:
		writeErr(w, http.StatusInternalServerError, err)
	}
}

func (a *apiHandlers) apiKeyDTOs() ([]dto.APIKey, error) {
	keys, err := a.d.Store.ListAPIKeys()
	if err != nil {
		return nil, err
	}
	grants, err := a.d.Store.APIKeyGrants()
	if err != nil {
		return nil, err
	}
	classes, err := a.d.Store.PriorityClasses()
	if err != nil {
		return nil, err
	}
	names := make(map[uint]string, len(classes))
	for _, c := range classes {
		names[c.ID] = c.Name
	}
	out := make([]dto.APIKey, 0, len(keys))
	for _, k := range keys {
		deployments := make([]dto.APIKeyDeployment, 0, len(grants[k.ID]))
		for _, g := range grants[k.ID] {
			deployments = append(deployments, dto.APIKeyDeployment{ID: g.DeploymentID, Name: g.Name, ServedName: g.ServedName})
		}
		out = append(out, dto.APIKey{
			Name: k.Name, Prefix: k.Prefix, LastUsed: utcTimePtr(k.LastUsed), Created: utcTime(k.CreatedAt), Revoked: utcTimePtr(k.RevokedAt),
			AllDeployments: !k.Restricted, Deployments: deployments, Priority: apiKeyPriorityDTO(k, names),
		})
	}
	return out, nil
}

func (a *apiHandlers) listAPIKeys(w http.ResponseWriter, r *http.Request) {
	out, err := a.apiKeyDTOs()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, dto.NewList(out))
}

func (a *apiHandlers) createAPIKey(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
		apiKeyScopeRequest
		// Priority binds the new key's requests to a class; only an
		// administrator may state it.
		Priority *dto.APIKeyPriority `json:"priority"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		writeProductError(w, http.StatusBadRequest, ErrorAPIKeyNameRequired, "API key name is required", nil)
		return
	}
	if !validateName(w, req.Name) {
		return
	}
	scope, ok := req.scope(w)
	if !ok {
		return
	}
	var binding *store.APIKeyPriority
	if req.Priority != nil {
		if !a.requireManagementAdmin(w, r) {
			return
		}
		parsed, ok := apiKeyPriority(w, *req.Priority)
		if !ok {
			return
		}
		// Refuse an unknown class before the key exists; the store checks
		// again when it binds.
		table, err := a.d.Store.PriorityClassTable()
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		for _, name := range []string{parsed.Class, parsed.Ceiling} {
			if _, known := table.ByName(name); name != "" && !known {
				writeProductError(w, http.StatusBadRequest, messages.ErrorPriorityClassInvalid, "no priority class "+name, map[string]any{"field": "priority", "class": name})
				return
			}
		}
		// A ceiling below the key's own class (the default's when it binds
		// none) is refused before the key exists, as the store refuses it.
		own := table.Default
		if c, ok := table.ByName(parsed.Class); ok {
			own = c
		}
		if c, ok := table.ByName(parsed.Ceiling); ok && store.PriorityPolicyTakesCeiling(parsed.Policy) && c.Value < own.Value {
			writeProductError(w, http.StatusBadRequest, messages.ErrorPriorityClassInvalid, "the ceiling is below the key's class", map[string]any{"field": "priority", "class": parsed.Ceiling})
			return
		}
		binding = &parsed
	}
	full, err := a.d.Store.CreateScopedAPIKey(req.Name, 0, scope)
	if err != nil {
		writeAPIKeyScopeError(w, err)
		return
	}
	if binding != nil {
		if err := a.d.Store.SetAPIKeyPriority(auth.PrefixOf(full), *binding); err != nil {
			// The key exists but runs unbound: revoke it rather than hand
			// out a key with a priority other than the one asked for.
			_ = a.d.Store.RevokeAPIKey(auth.PrefixOf(full))
			writeAPIKeyPriorityError(w, err)
			return
		}
	}
	// The full key is returned exactly once.
	writeJSON(w, http.StatusCreated, map[string]string{"name": req.Name, "key": full})
}

// setAPIKeyScope replaces which deployments a live key may call; the
// gateway applies it from the key's next request.
func (a *apiHandlers) setAPIKeyScope(w http.ResponseWriter, r *http.Request) {
	var req apiKeyScopeRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if req.AllDeployments == nil {
		writeProductError(w, http.StatusBadRequest, ErrorAPIKeyScopeInvalid, "all_deployments is required", nil)
		return
	}
	scope, ok := req.scope(w)
	if !ok {
		return
	}
	prefix := chi.URLParam(r, "prefix")
	if err := a.d.Store.SetAPIKeyScope(prefix, scope); err != nil {
		writeAPIKeyScopeError(w, err)
		return
	}
	keys, err := a.apiKeyDTOs()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	for _, k := range keys {
		if k.Prefix == prefix {
			writeJSON(w, http.StatusOK, k)
			return
		}
	}
	writeAPIKeyScopeError(w, store.ErrNotFound)
}

func (a *apiHandlers) deleteAPIKey(w http.ResponseWriter, r *http.Request) {
	if err := a.d.Store.RevokeAPIKey(chi.URLParam(r, "prefix")); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeProductError(w, http.StatusNotFound, ErrorAPIKeyNotFound, "no API key has this prefix", nil)
			return
		}
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "revoked"})
}
