package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/provision"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func (a *apiHandlers) nodeConnectionDiagnostics(w http.ResponseWriter, r *http.Request) {
	// The target and SSH identity come exclusively from the stored node.
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024))
	decoder.DisallowUnknownFields()
	var body struct{}
	if err := decoder.Decode(&body); err != nil && err != io.EOF {
		writeProductError(w, http.StatusBadRequest, ErrorNodeConnectionDiagnosticsInput, "connection diagnostics accepts an empty body or an empty JSON object", nil)
		return
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		writeProductError(w, http.StatusBadRequest, ErrorNodeConnectionDiagnosticsInput, "connection diagnostics accepts an empty body or an empty JSON object", nil)
		return
	}
	node, err := a.d.Store.NodeByUUID(chi.URLParam(r, "uuid"))
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	if err != nil {
		writeProductError(w, http.StatusInternalServerError, ErrorNodeConnectionSettingsUnavailable, "node connection settings could not be read; retry or inspect the control-plane log", nil)
		return
	}
	out := dto.NodeConnectionDiagnostics{
		NodeUUID: node.UUID, CheckedAt: time.Now().UTC(), ConnectMode: node.ConnectMode,
		SavedServerURL: safeNodeConnectionURL(node.ServerURL, node.Token, false), LastSeenAt: node.LastSeenAt,
		RecoveryPhase: node.RecoveryPhase, RecoveryAttempts: node.RecoveryAttempts,
		LastProbeAt:       node.LastProbeAt,
		LastRecoveryError: provision.ConnectionDiagnosticDetail(node.LastRecoveryError, node.Token),
		Guidance:          []string{"snapshot_scope", "http_scope", "test_from_node", "manual_address_update"},
	}
	if exposure, ok := a.d.NodeInstaller.(interface {
		ConnectionExposure() *dto.NodeConnectionExposure
	}); ok {
		if snapshot := exposure.ConnectionExposure(); snapshot != nil && safeNodeConnectionURL(snapshot.URL, node.Token, false) != "" {
			copy := *snapshot
			out.Exposure = &copy
			if out.SavedServerURL != "" && strings.TrimRight(copy.URL, "/") != strings.TrimRight(out.SavedServerURL, "/") {
				out.Guidance = append(out.Guidance, "addresses_differ")
			}
		}
	}
	switch node.LastRejectReason {
	case "bad_token", "proto_mismatch", "stale_session":
		out.LastRejectReason, out.LastRejectAt = node.LastRejectReason, utcTimePtr(node.LastRejectAt)
	}
	agentCheck, attempted := a.nodeConnectionAgentReport(r.Context(), node, &out)
	diagnostic := provision.ConnectionDiagnostic{
		SSH:           provision.ConnectionCheck{Stage: "ssh", Outcome: "unsupported"},
		Configuration: provision.ConnectionCheck{Stage: "configuration", Outcome: "not_checked"},
		Health:        provision.ConnectionCheck{Stage: "node_http", Outcome: "not_checked"},
	}
	if !node.HasSSHCredential {
		diagnostic.SSH.Outcome = "credentials_unavailable"
	} else if attempted {
		diagnostic.SSH.Outcome = "not_checked"
	}
	if node.ConnectMode == "server_dial" {
		diagnostic.Health.Outcome = "not_applicable"
		out.Guidance = []string{"server_dial_scope"}
	}
	if prober, ok := a.d.NodeInstaller.(interface {
		DiagnoseConnection(context.Context, provision.SSHProbeRequest) provision.ConnectionDiagnostic
	}); ok && node.HasSSHCredential && !attempted {
		row, secrets, credentialErr := a.d.Store.GetNodeSSHCredential(node.UUID)
		if credentialErr != nil {
			diagnostic.SSH.Outcome = "credential_error"
		}
		if credentialErr == nil {
			ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
			defer cancel()
			diagnostic = prober.DiagnoseConnection(ctx, provision.SSHProbeRequest{
				Host: row.Host, Port: row.Port, User: row.User, ServerURL: node.ServerURL,
				Token: node.Token, InstallDir: node.InstallDir, ConnectMode: node.ConnectMode,
				SSHAuth: provision.SSHAuth{Password: secrets.Password, PrivateKeyPEM: secrets.PrivateKeyPEM, PlatformKey: a.platformKey()},
			})
		}
	}
	out.Checks = []dto.NodeConnectionCheck{
		agentCheck,
		connectionCheckDTO(diagnostic.SSH, "ssh", "control_plane"),
		connectionCheckDTO(diagnostic.Configuration, "configuration", "node"),
		connectionCheckDTO(diagnostic.Health, "node_http", "node"),
	}
	for i := range out.Checks {
		out.Checks[i].Target = safeNodeConnectionURL(out.Checks[i].Target, node.Token, false)
	}
	if diagnostic.Configuration.Target != "" && provision.ConnectionDiagnosticURL(diagnostic.Configuration.Target) != "" && out.SavedServerURL != "" && strings.TrimRight(diagnostic.Configuration.Target, "/") != strings.TrimRight(out.SavedServerURL, "/") {
		out.Guidance = append(out.Guidance, "configuration_differs")
	}
	writeJSON(w, http.StatusOK, out)
}

func connectionCheckDTO(check provision.ConnectionCheck, stage, vantage string) dto.NodeConnectionCheck {
	out := dto.NodeConnectionCheck{Stage: stage, Vantage: vantage, Outcome: "invalid_response"}
	switch check.Outcome {
	case "unsupported", "credentials_unavailable", "credential_error", "unavailable", "not_checked", "not_applicable", "invalid_address", "unverified_identity", "ssh_failed", "cancelled", "timeout", "connected", "command_failed", "invalid_response", "snapshot", "tool_missing", "http_error", "http_ok", "dns", "connect", "tls", "transport":
		out.Outcome = check.Outcome
	}
	out.Target = provision.ConnectionDiagnosticURL(check.Target)
	if check.Source == "state_file" || check.Source == "legacy_arguments" {
		out.Source = check.Source
	}
	if !check.ObservedAt.IsZero() {
		at := check.ObservedAt.UTC()
		out.ObservedAt = &at
	}
	if check.HTTPStatus >= 100 && check.HTTPStatus <= 599 {
		out.HTTPStatus = check.HTTPStatus
	}
	return out
}
