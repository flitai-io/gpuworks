package api

import (
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// Request priority classes (docs/explanation/request-priority.md). Anyone
// signed in may read them; only an administrator defines classes, picks the
// default and binds keys, so no key can raise its own priority.

// priorityClassDTO is a class as the API answers it; bound is whether a live
// key names it.
func priorityClassDTO(c store.PriorityClass, bound bool) dto.PriorityClass {
	return dto.PriorityClass{Name: c.Name, DisplayName: c.DisplayName, Description: c.Description, Value: c.Value,
		Builtin: c.Builtin, Default: c.IsDefault, Deletable: !c.Builtin && !c.IsDefault && !bound, CreatedAt: utcTime(c.CreatedAt)}
}

func (a *apiHandlers) listPriorityClasses(w http.ResponseWriter, r *http.Request) {
	rows, err := a.d.Store.PriorityClasses()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	bound, err := a.d.Store.PriorityClassesBound()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	out := dto.PriorityClasses{Items: make([]dto.PriorityClass, 0, len(rows)), MinValue: store.PriorityClassMinValue, MaxValue: store.PriorityClassMaxValue,
		DefaultPolicy: store.DefaultPriorityPolicy, Policies: store.PriorityPolicies(),
		CeilingPolicies: store.PriorityCeilingPolicies()}
	for _, row := range rows {
		out.Items = append(out.Items, priorityClassDTO(row, bound[row.ID]))
	}
	writeJSON(w, http.StatusOK, out)
}

// writePriorityClassError maps a store refusal of a class change.
func writePriorityClassError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeProductError(w, http.StatusNotFound, messages.ErrorPriorityClassNotFound, "no priority class has this name", nil)
	case errors.Is(err, store.ErrPriorityClassExists):
		writeProductError(w, http.StatusConflict, messages.ErrorPriorityClassExists, err.Error(), nil)
	case errors.Is(err, store.ErrPriorityClassBuiltin):
		writeProductError(w, http.StatusConflict, messages.ErrorPriorityClassBuiltin, err.Error(), nil)
	case errors.Is(err, store.ErrPriorityClassInUse):
		writeProductError(w, http.StatusConflict, messages.ErrorPriorityClassInUse, err.Error(), nil)
	case errors.Is(err, store.ErrPriorityClassInvalid):
		writeProductError(w, http.StatusBadRequest, messages.ErrorPriorityClassInvalid, err.Error(), map[string]any{"detail": err.Error()})
	default:
		writeErr(w, http.StatusInternalServerError, err)
	}
}

// validatePriorityClassFields refuses a name or value out of bounds with the
// field it is about, before the store sees it.
func validatePriorityClassFields(w http.ResponseWriter, name *string, value *int) bool {
	if name != nil && !store.ValidPriorityClassName(strings.TrimSpace(*name)) {
		writeProductError(w, http.StatusBadRequest, messages.ErrorPriorityClassInvalid,
			"name takes lowercase letters, digits and hyphens, starting with a letter, at most 32 characters",
			map[string]any{"field": "name"})
		return false
	}
	if value != nil && !store.ValidPriorityClassValue(*value) {
		writeProductError(w, http.StatusBadRequest, messages.ErrorPriorityClassInvalid, "value is out of range",
			map[string]any{"field": "value", "minimum": store.PriorityClassMinValue, "maximum": store.PriorityClassMaxValue})
		return false
	}
	return true
}

func (a *apiHandlers) createPriorityClass(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name        string `json:"name"`
		DisplayName string `json:"display_name"`
		Description string `json:"description"`
		Value       *int   `json:"value"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if req.Value == nil {
		writeProductError(w, http.StatusBadRequest, messages.ErrorPriorityClassInvalid, "value is required", map[string]any{"field": "value"})
		return
	}
	if !validatePriorityClassFields(w, &req.Name, req.Value) {
		return
	}
	row := store.PriorityClass{Name: req.Name, DisplayName: req.DisplayName, Description: req.Description, Value: *req.Value}
	if err := a.d.Store.CreatePriorityClass(&row); err != nil {
		writePriorityClassError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, priorityClassDTO(row, false))
}

func (a *apiHandlers) updatePriorityClass(w http.ResponseWriter, r *http.Request) {
	var req struct {
		DisplayName *string `json:"display_name"`
		Description *string `json:"description"`
		Value       *int    `json:"value"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if !validatePriorityClassFields(w, nil, req.Value) {
		return
	}
	row, err := a.d.Store.UpdatePriorityClass(chi.URLParam(r, "name"), store.PriorityClassUpdate{DisplayName: req.DisplayName, Description: req.Description, Value: req.Value})
	if err != nil {
		writePriorityClassError(w, err)
		return
	}
	bound, err := a.d.Store.PriorityClassesBound()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, priorityClassDTO(row, bound[row.ID]))
}

func (a *apiHandlers) deletePriorityClass(w http.ResponseWriter, r *http.Request) {
	if err := a.d.Store.DeletePriorityClass(chi.URLParam(r, "name")); err != nil {
		writePriorityClassError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// setDefaultPriorityClass makes the class the default; the answer is the
// whole list, since another class stopped being the default.
func (a *apiHandlers) setDefaultPriorityClass(w http.ResponseWriter, r *http.Request) {
	if err := a.d.Store.SetDefaultPriorityClass(chi.URLParam(r, "name")); err != nil {
		writePriorityClassError(w, err)
		return
	}
	a.listPriorityClasses(w, r)
}

// apiKeyPriority reads a binding request: the classes must exist (the store
// checks, in the write's transaction) and the policy be a known one.
func apiKeyPriority(w http.ResponseWriter, req dto.APIKeyPriority) (store.APIKeyPriority, bool) {
	policy, ok := store.ParsePriorityPolicy(req.Policy)
	if !ok {
		writeProductError(w, http.StatusBadRequest, messages.ErrorPriorityPolicyInvalid, "unknown priority policy", map[string]any{"field": "policy", "policy": req.Policy})
		return store.APIKeyPriority{}, false
	}
	return store.APIKeyPriority{Class: strings.TrimSpace(req.Class), Ceiling: strings.TrimSpace(req.Ceiling), Policy: policy}, true
}

// writeAPIKeyPriorityError maps a store refusal of a binding.
func writeAPIKeyPriorityError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrPriorityClassInvalid):
		writeProductError(w, http.StatusBadRequest, messages.ErrorPriorityClassInvalid, err.Error(), map[string]any{"detail": err.Error()})
	case errors.Is(err, store.ErrNotFound):
		writeProductError(w, http.StatusNotFound, ErrorAPIKeyNotFound, "no live API key has this prefix", nil)
	default:
		writeErr(w, http.StatusInternalServerError, err)
	}
}

// setAPIKeyPriority binds a live key's requests to a class; the gateway
// applies it from the key's next request.
func (a *apiHandlers) setAPIKeyPriority(w http.ResponseWriter, r *http.Request) {
	var req dto.APIKeyPriority
	if !decodeJSONBody(w, r, &req) {
		return
	}
	binding, ok := apiKeyPriority(w, req)
	if !ok {
		return
	}
	prefix := chi.URLParam(r, "prefix")
	if err := a.d.Store.SetAPIKeyPriority(prefix, binding); err != nil {
		writeAPIKeyPriorityError(w, err)
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
	writeAPIKeyPriorityError(w, store.ErrNotFound)
}

// apiKeyPriorityDTO is a key's binding by class name; a class id no longer
// in names (deleted with the key revoked) reads as none.
func apiKeyPriorityDTO(k store.APIKey, names map[uint]string) dto.APIKeyPriority {
	out := dto.APIKeyPriority{Policy: store.PriorityBindingPolicy(k.PriorityPolicy)}
	if k.PriorityClassID != nil {
		out.Class = names[*k.PriorityClassID]
	}
	if k.PriorityCeilingClassID != nil {
		out.Ceiling = names[*k.PriorityCeilingClassID]
	}
	return out
}
