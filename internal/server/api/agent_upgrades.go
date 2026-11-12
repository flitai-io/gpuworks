package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// AgentUpgradeService is the HTTP boundary for durable agent upgrades.
// The app adapter owns conversion from domain records to these wire DTOs.
type AgentUpgradeService interface {
	Fleet(context.Context, []store.Node) (AgentFleetStatus, error)
	Preview(context.Context, []string) (dto.AgentUpgradePreview, error)
	Create(context.Context, dto.AgentUpgradeCreateRequest, string) (dto.AgentUpgradeRun, error)
	List(context.Context, int, int) (dto.AgentUpgradeRunPage, error)
	Get(context.Context, uint) (dto.AgentUpgradeRun, error)
	Items(context.Context, uint, int, int, string) (dto.AgentUpgradeItemPage, error)
	Stop(context.Context, uint, string) (dto.AgentUpgradeRun, error)
	ContinuePreview(context.Context, uint) (dto.AgentUpgradePreview, error)
	Release(context.Context, uint, string, string, string) error
	UpgradeNode(context.Context, string, string) (dto.AgentUpgradeRun, error)
	BeginServerUpgrade() (func(bool), error)
}

type agentUpgradeRequestError string

func (e agentUpgradeRequestError) Error() string          { return string(e) }
func (agentUpgradeRequestError) UpgradeErrorCode() string { return "AGENT_UPGRADE_INVALID_REQUEST" }

type AgentFleetStatus struct {
	Target      string
	RunID       uint
	Eligibility map[string]string
}

// AgentUpgradeHooks groups durable execution and the legacy error view of the same domain.
type AgentUpgradeHooks struct {
	Service AgentUpgradeService
	Errors  func() map[string]string
}

func (a *apiHandlers) upgradeService(w http.ResponseWriter) AgentUpgradeService {
	if a.d.AgentUpgrades.Service == nil {
		writeProductError(w, http.StatusServiceUnavailable, ErrorAgentUpgradeRefused, "agent upgrade service is unavailable", map[string]any{"reason": "AGENT_UPGRADE_UNAVAILABLE", "detail": "agent upgrade service is unavailable"})
	}
	return a.d.AgentUpgrades.Service
}

func writeAgentUpgradeError(w http.ResponseWriter, err error) {
	status, reason := http.StatusInternalServerError, "AGENT_UPGRADE_INTERNAL"
	var domain interface{ UpgradeErrorCode() string }
	switch {
	case errors.Is(err, store.ErrNotFound):
		status, reason = http.StatusNotFound, "AGENT_UPGRADE_NOT_FOUND"
	case errors.As(err, &domain):
		reason = domain.UpgradeErrorCode()
		status = http.StatusConflict
		switch reason {
		case "AGENT_UPGRADE_INVALID_REQUEST", "AGENT_UPGRADE_RELEASE_NOTE_REQUIRED":
			status = http.StatusBadRequest
		case "AGENT_UPGRADE_NOT_FOUND":
			status = http.StatusNotFound
		case "AGENT_UPGRADE_UNAVAILABLE":
			status = http.StatusServiceUnavailable
		}
	}
	writeProductError(w, status, ErrorAgentUpgradeRefused, err.Error(), map[string]any{"reason": reason, "detail": err.Error()})
}

func (a *apiHandlers) agentUpgradeActor(r *http.Request) string {
	if claims, ok := a.claimsFromRequest(r); ok {
		return claims.Username
	}
	return ""
}

func agentUpgradeRunID(w http.ResponseWriter, r *http.Request) (uint, bool) {
	id, err := strconv.ParseUint(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id == 0 {
		writeAgentUpgradeError(w, agentUpgradeRequestError("invalid agent upgrade run id"))
		return 0, false
	}
	return uint(id), true
}

func (a *apiHandlers) previewAgentUpgrade(w http.ResponseWriter, r *http.Request) {
	svc := a.upgradeService(w)
	if svc == nil {
		return
	}
	var req dto.AgentUpgradePreviewRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if (req.Scope != "" && req.Scope != "all") || (req.Scope == "all" && len(req.NodeUUIDs) > 0) || (req.Scope == "" && len(req.NodeUUIDs) == 0) {
		writeAgentUpgradeError(w, agentUpgradeRequestError("choose scope all or explicit node_uuids"))
		return
	}
	preview, err := svc.Preview(r.Context(), req.NodeUUIDs)
	if err != nil {
		writeAgentUpgradeError(w, err)
		return
	}
	preview.TargetBuild = a.agentUpgradeBuildInfo(preview.TargetVersion)
	writeJSON(w, http.StatusOK, preview)
}

func (a *apiHandlers) createAgentUpgrade(w http.ResponseWriter, r *http.Request) {
	svc := a.upgradeService(w)
	if svc == nil {
		return
	}
	var req dto.AgentUpgradeCreateRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	run, err := svc.Create(r.Context(), req, a.agentUpgradeActor(r))
	if err != nil {
		writeAgentUpgradeError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, run)
}

func (a *apiHandlers) listAgentUpgrades(w http.ResponseWriter, r *http.Request) {
	svc := a.upgradeService(w)
	if svc == nil {
		return
	}
	q, err := parseListQuery(r, nil)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	page, err := svc.List(r.Context(), q.page, q.pageSize)
	if err != nil {
		writeAgentUpgradeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, page)
}

func (a *apiHandlers) getAgentUpgrade(w http.ResponseWriter, r *http.Request) {
	svc := a.upgradeService(w)
	if svc == nil {
		return
	}
	id, ok := agentUpgradeRunID(w, r)
	if !ok {
		return
	}
	run, err := svc.Get(r.Context(), id)
	if err != nil {
		writeAgentUpgradeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, run)
}

func (a *apiHandlers) agentUpgradeItems(w http.ResponseWriter, r *http.Request) {
	svc := a.upgradeService(w)
	if svc == nil {
		return
	}
	id, ok := agentUpgradeRunID(w, r)
	if !ok {
		return
	}
	phases := []string{"queued", "transferring", "waiting_registration", "observing_recovery", "succeeded", "failed", "skipped", "cancelled", "interrupted", "unconfirmed"}
	q, err := parseListQuery(r, phases)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if len(q.states) > 1 {
		writeErr(w, http.StatusBadRequest, listFilterError("state", r.URL.Query().Get("state")))
		return
	}
	page, err := svc.Items(r.Context(), id, q.page, q.pageSize, strings.TrimSpace(r.URL.Query().Get("state")))
	if err != nil {
		writeAgentUpgradeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, page)
}

func (a *apiHandlers) stopAgentUpgrade(w http.ResponseWriter, r *http.Request) {
	svc := a.upgradeService(w)
	if svc == nil {
		return
	}
	id, ok := agentUpgradeRunID(w, r)
	if !ok {
		return
	}
	run, err := svc.Stop(r.Context(), id, a.agentUpgradeActor(r))
	if err != nil {
		writeAgentUpgradeError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, run)
}

func (a *apiHandlers) continueAgentUpgradePreview(w http.ResponseWriter, r *http.Request) {
	svc := a.upgradeService(w)
	if svc == nil {
		return
	}
	id, ok := agentUpgradeRunID(w, r)
	if !ok {
		return
	}
	preview, err := svc.ContinuePreview(r.Context(), id)
	if err != nil {
		writeAgentUpgradeError(w, err)
		return
	}
	preview.TargetBuild = a.agentUpgradeBuildInfo(preview.TargetVersion)
	writeJSON(w, http.StatusOK, preview)
}

func (a *apiHandlers) releaseAgentUpgrade(w http.ResponseWriter, r *http.Request) {
	svc := a.upgradeService(w)
	if svc == nil {
		return
	}
	id, ok := agentUpgradeRunID(w, r)
	if !ok {
		return
	}
	var req struct {
		Note string `json:"note"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if err := svc.Release(r.Context(), id, chi.URLParam(r, "uuid"), a.agentUpgradeActor(r), req.Note); err != nil {
		writeAgentUpgradeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
