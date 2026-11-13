package api

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// Platform API tokens authenticate the management API and MCP as the user
// who made them (auth.go). Each user manages their own, from a login
// session only.

// maxPlatformTokenDays bounds a token's stated lifetime.
const maxPlatformTokenDays = 3650

func platformTokenDTO(t store.PlatformToken) dto.PlatformToken {
	return dto.PlatformToken{ID: t.ID, Name: t.Name, Prefix: t.Prefix, Scope: t.Scope, CreatedAt: t.CreatedAt.UTC(), ExpiresAt: utcTimePtr(t.ExpiresAt), LastUsedAt: utcTimePtr(t.LastUsedAt)}
}

func (a *apiHandlers) listPlatformTokens(w http.ResponseWriter, r *http.Request) {
	claims, _ := a.claimsFromRequest(r)
	rows, err := a.d.Store.ListPlatformTokens(claims.UserID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	items := make([]dto.PlatformToken, 0, len(rows))
	for _, row := range rows {
		items = append(items, platformTokenDTO(row))
	}
	writeJSON(w, http.StatusOK, dto.NewList(items))
}

func (a *apiHandlers) createPlatformToken(w http.ResponseWriter, r *http.Request) {
	claims, _ := a.claimsFromRequest(r)
	var req dto.PlatformTokenRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if !validateName(w, req.Name) {
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" || len(req.Name) > 128 {
		writeProductError(w, http.StatusBadRequest, ErrorPlatformTokenNameInvalid, "name must be 1 to 128 UTF-8 bytes after trimming", map[string]any{"max": 128})
		return
	}
	if req.Scope == "" {
		req.Scope = store.PlatformTokenRead
	}
	if req.Scope != store.PlatformTokenRead && req.Scope != store.PlatformTokenWrite {
		writeProductError(w, http.StatusBadRequest, ErrorPlatformTokenScopeInvalid, "scope must be read or write", map[string]any{"scope": req.Scope})
		return
	}
	if req.ExpiresInDays < 0 || req.ExpiresInDays > maxPlatformTokenDays {
		writeProductError(w, http.StatusBadRequest, ErrorPlatformTokenExpiryInvalid, "expires_in_days must be 0 (never) to "+strconv.Itoa(maxPlatformTokenDays), map[string]any{"max": maxPlatformTokenDays})
		return
	}
	var expires *time.Time
	if req.ExpiresInDays > 0 {
		at := time.Now().UTC().AddDate(0, 0, req.ExpiresInDays)
		expires = &at
	}
	full, row, err := a.d.Store.CreatePlatformToken(claims.UserID, req.Name, req.Scope, expires)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if a.d.Log != nil {
		a.d.Log.Info("platform token created", "user", claims.Username, "prefix", row.Prefix, "scope", row.Scope, "expires_at", expires)
	}
	writeJSON(w, http.StatusCreated, dto.PlatformTokenCreated{Token: full, Item: platformTokenDTO(*row)})
}

func (a *apiHandlers) deletePlatformToken(w http.ResponseWriter, r *http.Request) {
	claims, _ := a.claimsFromRequest(r)
	id, err := strconv.ParseUint(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		writeProductError(w, http.StatusBadRequest, ErrorPlatformTokenIDInvalid, "invalid token id", nil)
		return
	}
	row, err := a.d.Store.DeletePlatformToken(claims.UserID, uint(id))
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if a.d.Log != nil {
		a.d.Log.Info("platform token revoked", "user", claims.Username, "prefix", row.Prefix, "scope", row.Scope)
	}
	w.WriteHeader(http.StatusNoContent)
}
