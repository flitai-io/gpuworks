package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"time"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/diagnostics"
	"gpuworks.ai/gpuworks/internal/server/messages"
)

// Diagnostics runs debug sessions and diagnostics collections (package
// diagnostics).
type Diagnostics interface {
	StartDebug(ctx context.Context, req diagnostics.DebugRequest) (diagnostics.DebugSession, error)
	StopDebug() (diagnostics.DebugSession, bool)
	DebugStatus() (diagnostics.DebugSession, bool)
	DiagnosticsBundles
}

const (
	ErrorDebugSessionInvalid     = messages.ErrorDebugSessionInvalid
	ErrorDebugSessionOpen        = messages.ErrorDebugSessionOpen
	ErrorDiagnosticsScopeUnknown = messages.ErrorDiagnosticsScopeUnknown
	ErrorDiagnosticsUnavailable  = messages.ErrorDiagnosticsUnavailable
)

func debugSessionLimits() dto.DebugSessionLimits {
	return dto.DebugSessionLimits{
		DefaultDurationSeconds: int(diagnostics.DefaultDebugDuration.Seconds()),
		MinDurationSeconds:     int(time.Minute.Seconds()),
		MaxDurationSeconds:     int(diagnostics.MaxDebugDuration.Seconds()),
		DefaultMaxBytes:        diagnostics.DefaultDebugBytes,
		MaxMaxBytes:            diagnostics.MaxDebugBytes,
		NodeMaxBytes:           diagnostics.NodeDebugBytes,
		RequestMaxBytes:        diagnostics.RequestCaptureBytes,
		KeepDays:               int(diagnostics.SessionKeep.Hours() / 24),
	}
}

func (a *apiHandlers) debugSessionState(s diagnostics.DebugSession, ok bool) dto.DebugSessionState {
	out := dto.DebugSessionState{Limits: debugSessionLimits()}
	if !ok {
		return out
	}
	session := dto.DebugSession{
		ID: s.ID, StartedBy: s.StartedBy, StartedAt: s.StartedAt.UTC().Format(time.RFC3339), Until: s.Until.UTC().Format(time.RFC3339),
		MaxBytes: s.MaxBytes, DeploymentIDs: s.DeploymentIDs, Nodes: []dto.DebugSessionNode{}, Open: s.Open, Ended: s.Ended,
		ServerLogBytes: s.ServerLogBytes, RequestBytes: s.RequestBytes, Requests: s.Requests,
	}
	if session.DeploymentIDs == nil {
		session.DeploymentIDs = []uint{}
	}
	if s.EndedAt != nil {
		session.EndedAt = s.EndedAt.UTC().Format(time.RFC3339)
	}
	for _, n := range s.Nodes {
		name := n.UUID
		if node, err := a.d.Store.NodeByUUID(n.UUID); err == nil {
			name = nodeDisplayName(node)
		}
		session.Nodes = append(session.Nodes, dto.DebugSessionNode{UUID: n.UUID, Name: name, State: n.State, Reason: n.Reason})
	}
	out.Session = &session
	return out
}

func (a *apiHandlers) getDebugSession(w http.ResponseWriter, _ *http.Request) {
	if a.d.Diagnostics == nil {
		writeJSON(w, http.StatusOK, dto.DebugSessionState{Limits: debugSessionLimits()})
		return
	}
	writeJSON(w, http.StatusOK, a.debugSessionState(a.d.Diagnostics.DebugStatus()))
}

func (a *apiHandlers) postDebugSession(w http.ResponseWriter, r *http.Request) {
	if a.d.Diagnostics == nil {
		writeProductError(w, http.StatusNotFound, ErrorDiagnosticsUnavailable, "diagnostics are not available on this server", nil)
		return
	}
	var req dto.DebugSessionStart
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.DurationSeconds < 0 || req.MaxBytes < 0 {
		writeProductError(w, http.StatusBadRequest, ErrorDebugSessionInvalid, "body must be a debug session request", map[string]any{"field": "body"})
		return
	}
	session, err := a.d.Diagnostics.StartDebug(r.Context(), diagnostics.DebugRequest{
		Duration: time.Duration(req.DurationSeconds) * time.Second, MaxBytes: req.MaxBytes,
		DeploymentIDs: req.DeploymentIDs, NodeUUIDs: req.NodeUUIDs, StartedBy: a.agentUpgradeActor(r),
	})
	var pe *messages.ProductError
	switch {
	case errors.As(err, &pe) && pe.Code() == ErrorDebugSessionOpen:
		writeErr(w, http.StatusConflict, err)
	case errors.As(err, &pe):
		writeErr(w, http.StatusBadRequest, err)
	case err != nil:
		writeErr(w, http.StatusInternalServerError, err)
	default:
		writeJSON(w, http.StatusCreated, a.debugSessionState(session, true))
	}
}

// deleteDebugSession stops the open session; with none open it answers the
// last one, so a stop that raced the deadline is not an error.
func (a *apiHandlers) deleteDebugSession(w http.ResponseWriter, _ *http.Request) {
	if a.d.Diagnostics == nil {
		writeProductError(w, http.StatusNotFound, ErrorDiagnosticsUnavailable, "diagnostics are not available on this server", nil)
		return
	}
	if session, ok := a.d.Diagnostics.StopDebug(); ok {
		writeJSON(w, http.StatusOK, a.debugSessionState(session, true))
		return
	}
	writeJSON(w, http.StatusOK, a.debugSessionState(a.d.Diagnostics.DebugStatus()))
}

// DiagnosticsBundles builds and keeps diagnostics collections.
type DiagnosticsBundles interface {
	StartBundle(ctx context.Context, req diagnostics.BundleRequest) (diagnostics.Bundle, error)
	Bundles() []diagnostics.Bundle
	Bundle(id string) (diagnostics.Bundle, bool)
	OpenBundle(id string) (*os.File, diagnostics.Bundle, error)
}

const (
	ErrorDiagnosticsBundleInvalid  = messages.ErrorDiagnosticsBundleInvalid
	ErrorDiagnosticsBundleRunning  = messages.ErrorDiagnosticsBundleRunning
	ErrorDiagnosticsBundleNotFound = messages.ErrorDiagnosticsBundleNotFound
)

func bundleDTO(b diagnostics.Bundle) dto.DiagnosticsBundle {
	out := dto.DiagnosticsBundle{ID: b.ID, RequestedBy: b.RequestedBy, CreatedAt: b.CreatedAt.UTC().Format(time.RFC3339),
		From: b.From.UTC().Format(time.RFC3339), To: b.To.UTC().Format(time.RFC3339), DeploymentIDs: b.Deployments,
		InstanceUUIDs: b.Instances, NodeUUIDs: b.Nodes, Doctor: b.Doctor, State: b.State, Error: b.Error, SizeBytes: b.SizeBytes,
		Items: []dto.DiagnosticsBundleItem{}}
	if b.FinishedAt != nil {
		out.FinishedAt = b.FinishedAt.UTC().Format(time.RFC3339)
	}
	if out.DeploymentIDs == nil {
		out.DeploymentIDs = []uint{}
	}
	if out.InstanceUUIDs == nil {
		out.InstanceUUIDs = []string{}
	}
	if out.NodeUUIDs == nil {
		out.NodeUUIDs = []string{}
	}
	for _, it := range b.Items {
		item := dto.DiagnosticsBundleItem{ID: it.ID(), Path: it.Path, Status: it.Status, Code: it.Code, Evidence: it.Evidence, Bytes: it.Bytes}
		if it.Code != "" {
			item.Params, item.Message = &dto.DiagnosticsBundleItemParams{Path: it.Path, Node: it.Node()}, it.Message()
		}
		out.Items = append(out.Items, item)
	}
	return out
}

func diagnosticsBundlesLimits() dto.DiagnosticsBundlesLimits {
	return dto.DiagnosticsBundlesLimits{
		DefaultWindowSeconds: int(diagnostics.DefaultBundleWindow.Seconds()), MaxWindowSeconds: int(diagnostics.MaxBundleWindow.Seconds()),
		MaxNodes: diagnostics.MaxBundleNodes, MaxInstances: diagnostics.MaxBundleInstances, KeepDays: int(diagnostics.BundleKeep.Hours() / 24),
	}
}

func (a *apiHandlers) getDiagnosticsBundles(w http.ResponseWriter, _ *http.Request) {
	out := dto.DiagnosticsBundles{Bundles: []dto.DiagnosticsBundle{}, Limits: diagnosticsBundlesLimits()}
	if a.d.Diagnostics != nil {
		for _, b := range a.d.Diagnostics.Bundles() {
			out.Bundles = append(out.Bundles, bundleDTO(b))
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *apiHandlers) postDiagnosticsBundle(w http.ResponseWriter, r *http.Request) {
	if a.d.Diagnostics == nil {
		writeProductError(w, http.StatusNotFound, ErrorDiagnosticsUnavailable, "diagnostics are not available on this server", nil)
		return
	}
	var req dto.DiagnosticsBundleStart
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeProductError(w, http.StatusBadRequest, ErrorDiagnosticsBundleInvalid, "body must be a diagnostics collection request", map[string]any{"field": "body"})
		return
	}
	var from, to time.Time
	for _, f := range []struct {
		raw string
		at  *time.Time
	}{{req.From, &from}, {req.To, &to}} {
		if f.raw == "" {
			continue
		}
		at, err := time.Parse(time.RFC3339, f.raw)
		if err != nil {
			writeProductError(w, http.StatusBadRequest, ErrorDiagnosticsBundleInvalid, "from and to must be RFC 3339 times", map[string]any{"field": "window"})
			return
		}
		*f.at = at
	}
	if from.IsZero() != to.IsZero() {
		writeProductError(w, http.StatusBadRequest, ErrorDiagnosticsBundleInvalid, "give both from and to, or neither", map[string]any{"field": "window"})
		return
	}
	doctor := req.Doctor == nil || *req.Doctor
	b, err := a.d.Diagnostics.StartBundle(r.Context(), diagnostics.BundleRequest{From: from, To: to, DeploymentIDs: req.DeploymentIDs,
		InstanceUUIDs: req.InstanceUUIDs, NodeUUIDs: req.NodeUUIDs, RequestedBy: a.agentUpgradeActor(r), Doctor: doctor})
	var pe *messages.ProductError
	switch {
	case errors.As(err, &pe) && pe.Code() == ErrorDiagnosticsBundleRunning:
		writeErr(w, http.StatusConflict, err)
	case errors.As(err, &pe):
		writeErr(w, http.StatusBadRequest, err)
	case err != nil:
		writeErr(w, http.StatusInternalServerError, err)
	default:
		writeJSON(w, http.StatusAccepted, bundleDTO(b))
	}
}

func (a *apiHandlers) getDiagnosticsBundle(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if a.d.Diagnostics != nil {
		if b, ok := a.d.Diagnostics.Bundle(id); ok {
			writeJSON(w, http.StatusOK, bundleDTO(b))
			return
		}
	}
	writeProductError(w, http.StatusNotFound, ErrorDiagnosticsBundleNotFound, "no such diagnostics collection", map[string]any{"bundle": id})
}

func (a *apiHandlers) downloadDiagnosticsBundle(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if a.d.Diagnostics == nil {
		writeProductError(w, http.StatusNotFound, ErrorDiagnosticsBundleNotFound, "no such diagnostics collection", map[string]any{"bundle": id})
		return
	}
	f, b, err := a.d.Diagnostics.OpenBundle(id)
	if err != nil {
		writeProductError(w, http.StatusNotFound, ErrorDiagnosticsBundleNotFound, "no such diagnostics collection ready to download", map[string]any{"bundle": id})
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", `attachment; filename="gpuworks-diagnostics-`+b.ID+`.tar.gz"`)
	w.Header().Set("Cache-Control", "no-store")
	http.ServeContent(w, r, "", b.CreatedAt, f)
}
