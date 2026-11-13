package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/server/provision"
	"gpuworks.ai/gpuworks/internal/server/store"
)

type nodeSSHResponse struct {
	Host          string `json:"host"`
	Port          int    `json:"port"`
	User          string `json:"user"`
	HasPassword   bool   `json:"has_password"`
	HasPrivateKey bool   `json:"has_private_key"`
	// AuthMode is derived: no stored secret means the platform key.
	AuthMode string                 `json:"auth_mode"`
	Probe    *provision.ProbeResult `json:"probe,omitempty"`
	// PlatformKey reports authorizing the platform key with the password or
	// key just saved.
	PlatformKey *provision.CheckResult `json:"platform_key,omitempty"`
}

func (a *apiHandlers) getNodeSSH(w http.ResponseWriter, r *http.Request) {
	row, secrets, err := a.d.Store.GetNodeSSHCredential(chi.URLParam(r, "uuid"))
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, sanitizeNodeSSH(row, secrets, nil))
}

func (a *apiHandlers) putNodeSSH(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Host          string `json:"host"`
		Port          int    `json:"port"`
		User          string `json:"user"`
		AuthMode      string `json:"auth_mode"`
		Password      string `json:"password"`
		PrivateKeyPEM string `json:"private_key_pem"`
		Probe         bool   `json:"probe"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if req.Port == 0 {
		req.Port = 22
	}
	auth := provision.SSHAuth{Mode: req.AuthMode, PlatformKey: a.platformKey()}
	switch req.AuthMode {
	case provision.AuthPassword, provision.AuthKeyboardInteractive:
		auth.Password = req.Password
	case provision.AuthPrivateKey:
		auth.PrivateKeyPEM = req.PrivateKeyPEM
	case provision.AuthPlatformKey:
	default: // clients from before auth_mode: whatever they sent
		auth.Password, auth.PrivateKeyPEM = req.Password, req.PrivateKeyPEM
	}
	probeReq := provision.SSHProbeRequest{
		Host: strings.TrimSpace(req.Host), Port: req.Port, User: strings.TrimSpace(req.User), SSHAuth: auth,
	}
	probeReq, err := provision.NormalizeSSHProbeRequest(probeReq)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	node, err := a.d.Store.NodeByUUID(chi.URLParam(r, "uuid"))
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	// Shared hosts can run several nodes for the same server.
	probeReq.ServerURL = node.ServerURL
	probeReq.Token = node.Token
	probeReq.InstallDir = node.InstallDir
	secrets := store.NodeSSHSecrets{Password: auth.Password, PrivateKeyPEM: auth.PrivateKeyPEM}
	// A password or key the operator typed is used once to authorize the
	// platform key; when that works, only the platform key is kept.
	var keyCheck *provision.CheckResult
	if !secrets.Empty() && auth.PlatformKey != nil {
		if authorizer, ok := a.d.NodeInstaller.(interface {
			AuthorizePlatformKey(context.Context, provision.SSHProbeRequest) (provision.CheckResult, bool)
		}); ok {
			check, authorized := authorizer.AuthorizePlatformKey(r.Context(), probeReq)
			keyCheck = &check
			if authorized {
				secrets = store.NodeSSHSecrets{}
				probeReq.SSHAuth = provision.SSHAuth{Mode: provision.AuthPlatformKey, PlatformKey: auth.PlatformKey}
			}
		}
	}
	row, err := a.d.Store.SetNodeSSHCredential(chi.URLParam(r, "uuid"), probeReq.Host, probeReq.Port, probeReq.User, secrets)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	var result *provision.ProbeResult
	if req.Probe {
		if prober, ok := a.d.NodeInstaller.(interface {
			Probe(context.Context, provision.SSHProbeRequest) provision.ProbeResult
		}); ok {
			probe := prober.Probe(r.Context(), probeReq)
			result = &probe
		}
	}
	resp := sanitizeNodeSSH(row, secrets, result)
	resp.PlatformKey = keyCheck
	writeJSON(w, http.StatusOK, resp)
}

func (a *apiHandlers) deleteNodeSSH(w http.ResponseWriter, r *http.Request) {
	err := a.d.Store.DeleteNodeSSHCredential(chi.URLParam(r, "uuid"))
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (a *apiHandlers) probeNodeRecovery(w http.ResponseWriter, r *http.Request) {
	if a.d.Recovery == nil {
		writeProductError(w, http.StatusServiceUnavailable, ErrorNodeRecoveryUnavailable, "node recovery is not configured", nil)
		return
	}
	outcome, err := a.d.Recovery.Trigger(r.Context(), chi.URLParam(r, "uuid"))
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, outcome)
}

func sanitizeNodeSSH(row *store.NodeSSHCredential, secrets store.NodeSSHSecrets, probe *provision.ProbeResult) nodeSSHResponse {
	return nodeSSHResponse{
		Host: row.Host, Port: row.Port, User: row.User,
		HasPassword: secrets.Password != "", HasPrivateKey: strings.TrimSpace(secrets.PrivateKeyPEM) != "",
		AuthMode: storedAuthMode(secrets), Probe: probe,
	}
}

func storedAuthMode(secrets store.NodeSSHSecrets) string {
	switch {
	case strings.TrimSpace(secrets.PrivateKeyPEM) != "":
		return provision.AuthPrivateKey
	case secrets.Password != "":
		return provision.AuthPassword
	}
	return provision.AuthPlatformKey
}
