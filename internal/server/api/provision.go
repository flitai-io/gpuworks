package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/provision"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func (a *apiHandlers) precheckNode(w http.ResponseWriter, r *http.Request) {
	if a.d.NodePrecheck == nil {
		writeProductError(w, http.StatusServiceUnavailable, ErrorNodePrecheckUnavailable, "node precheck is not configured", nil)
		return
	}
	var req provision.SSHPrecheckRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	req.PlatformKey = a.platformKey()
	report, err := a.d.NodePrecheck.PrecheckSSH(r.Context(), req)
	if err != nil {
		// Precheck is advisory. Keep transport/authentication failures visible as
		// a structured check so the user can still continue with installation.
		failed := provision.SSHPrecheckReport{
			Target: req.Target,
			Checks: []provision.CheckResult{{Name: "SSH", Status: "error", Detail: err.Error()}},
		}
		// A dial error used to leave these zero, and the form printed @:0.
		// Keep the identity when the target parses; otherwise leave it blank.
		if user, host, port, parseErr := provision.ParseTarget(req.Target, req.User, req.Port); parseErr == nil {
			failed.User, failed.Host, failed.Port = user, host, port
		}
		writeJSON(w, http.StatusOK, failed)
		return
	}
	if a.d.Store != nil {
		if nodes, err := a.d.Store.NodesWithSSHHost(report.Host); err != nil {
			report.Checks = append(report.Checks, provision.CheckResult{Name: checkExistingNode, Status: "warn", Detail: "look up nodes with this SSH host: " + err.Error()})
		} else if len(nodes) > 0 {
			report.Checks = append(report.Checks, provision.CheckResult{Name: checkExistingNode, Status: "warn",
				Detail: "this host is already added here as " + nodeNames(nodes) + "; installing again replaces its agent and that node goes offline"})
		} else {
			report.Checks = append(report.Checks, provision.CheckResult{Name: checkExistingNode, Status: "ok", Detail: "no node here uses this SSH host"})
		}
	}
	writeJSON(w, http.StatusOK, report)
}

const checkExistingNode = "Existing node"

func nodeNames(nodes []store.Node) string {
	names := make([]string, 0, len(nodes))
	for _, n := range nodes {
		name := strings.TrimSpace(n.Name)
		if name == "" {
			name = strings.TrimSpace(n.Hostname)
		}
		if name == "" {
			name = n.UUID
		}
		names = append(names, name)
	}
	return strings.Join(names, ", ")
}

func (a *apiHandlers) installNode(w http.ResponseWriter, r *http.Request) {
	if a.d.NodeInstaller == nil {
		writeProductError(w, http.StatusServiceUnavailable, ErrorNodeInstallerUnavailable, "node installer is not configured", nil)
		return
	}
	var req provision.SSHInstallRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if !validateNodeName(w, req.Name) {
		return
	}
	if strings.TrimSpace(req.ServerDialListen) == "" {
		req.ServerDialListen = provision.DefaultServerDialListen
	}
	if req.ConnectMode == "server_dial" {
		if err := validServerDialListen(req.ServerDialListen); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
	}
	var serverURLErr error
	req.ServerURL, serverURLErr = installServerURL(req.ServerURL, req.ConnectMode)
	if serverURLErr != nil {
		writeErr(w, http.StatusBadRequest, serverURLErr)
		return
	}
	if a.d.Store == nil {
		writeProductError(w, http.StatusServiceUnavailable, ErrorNodeStoreUnavailable, "node store is not configured", nil)
		return
	}
	// Installing onto a host that is already a node, or that runs another
	// agent, replaces that agent: ask the caller to confirm first.
	// A nohup install runs beside the agents a host already has, so a host
	// that is already a node may take another; InstallSSH still asks before
	// replacing an agent running from the same directory.
	if !req.ReplaceExistingAgent && !req.NoSystemd {
		if _, host, _, parseErr := provision.ParseTarget(req.Target, req.User, req.Port); parseErr == nil {
			nodes, err := a.d.Store.NodesWithSSHHost(host)
			if err != nil {
				writeErr(w, http.StatusInternalServerError, err)
				return
			}
			if len(nodes) > 0 {
				names := nodeNames(nodes)
				writeProductError(w, http.StatusConflict, ErrorNodeInstallHostAlreadyNode,
					"this host is already added as "+names+"; installing again replaces its agent", map[string]any{"nodes": names})
				return
			}
		}
	}
	req.GuardExistingAgent = true
	pending, token, err := a.d.Store.CreateNodePending(req.Name, req.ConnectMode)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	req.Token = token
	req.PlatformKey = a.platformKey()
	if preference := strings.ToLower(strings.TrimSpace(req.RuntimeDriver)); preference != "" {
		if !proto.ValidRuntimePreference(preference) || preference == proto.RuntimePreferenceAuto {
			if preference == proto.RuntimePreferenceAuto {
				req.RuntimeDriver = ""
			}
			if err := a.d.Store.SetNodeRuntimePreference(pending.UUID, proto.RuntimePreferenceAuto); err != nil {
				_ = a.d.Store.DeleteNode(pending.UUID)
				writeErr(w, http.StatusBadRequest, err)
				return
			}
		} else if err := a.d.Store.SetNodeRuntimePreference(pending.UUID, preference); err != nil {
			_ = a.d.Store.DeleteNode(pending.UUID)
			writeErr(w, http.StatusBadRequest, err)
			return
		}
	}
	if err := a.d.Store.SetNodeServerURL(pending.UUID, req.ServerURL); err != nil {
		_ = a.d.Store.DeleteNode(pending.UUID)
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if err := a.d.Store.SetNodeServerDialListen(pending.UUID, req.ServerDialListen); err != nil {
		_ = a.d.Store.DeleteNode(pending.UUID)
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	report, err := a.d.NodeInstaller.InstallSSH(r.Context(), req)
	if err != nil {
		_ = a.d.Store.DeleteNode(pending.UUID)
		var existing *provision.ExistingAgentError
		if errors.As(err, &existing) {
			found := existing.Found
			switch {
			case existing.InstallDir != "":
				writeErr(w, http.StatusConflict, wrapProductError(ErrorNodeInstallExistingAgentSameDir, err.Error(), map[string]any{"dir": existing.InstallDir}, err))
			case found.Unit && found.UnitServer == "":
				writeErr(w, http.StatusConflict, wrapProductError(ErrorNodeInstallExistingAgentUnknown, err.Error(), nil, err))
			case found.Unit && found.UnitServer != strings.TrimRight(req.ServerURL, "/"):
				writeErr(w, http.StatusConflict, wrapProductError(ErrorNodeInstallExistingAgent, err.Error(), map[string]any{"server": found.UnitServer}, err))
			default:
				writeErr(w, http.StatusConflict, wrapProductError(ErrorNodeInstallExistingAgentSelf, err.Error(), nil, err))
			}
			return
		}
		if blocked, ok := untraversableInstallDirError(err); ok {
			writeErr(w, http.StatusBadRequest, blocked)
			return
		}
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if strings.TrimSpace(report.ServerURL) == "" {
		report.ServerURL = strings.TrimRight(strings.TrimSpace(req.ServerURL), "/")
	}
	if _, err := a.d.Store.SetNodeSSHCredential(pending.UUID, report.Host, report.Port, report.User, installedSSHSecrets(req.SSHAuth, report)); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if err := a.d.Store.SetNodeInstallDir(pending.UUID, report.InstallDir); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if req.ConnectMode == "server_dial" {
		if strings.TrimSpace(report.ServerDialAddress) == "" {
			hostAddress := strings.TrimSpace(req.HostAddress)
			if hostAddress == "" {
				_, host, _, parseErr := provision.ParseTarget(req.Target, req.User, req.Port)
				if parseErr == nil {
					hostAddress = host
				}
			}
			report.ServerDialAddress = provision.ServerDialAddress(hostAddress, req.ServerDialListen)
		}
		if strings.TrimSpace(report.ServerDialAddress) != "" {
			if err := a.d.Store.SetNodeServerDialAddress(pending.UUID, report.ServerDialAddress); err != nil {
				writeErr(w, http.StatusInternalServerError, err)
				return
			}
		}
		report.Checks = append(report.Checks, serverDialInstallCheck(r.Context(), report.ServerDialAddress, req.Token, a.d.ServerDialAgent))
	}
	writeJSON(w, http.StatusOK, struct {
		provision.SSHInstallReport
		NodeUUID string `json:"node_uuid"`
	}{SSHInstallReport: report, NodeUUID: pending.UUID})
}

// untraversableInstallDirError is the stable error for an install the
// installer stopped because a directory above the install directory is closed
// to the dedicated runtime user ({dir}: that directory, {mode}, {install_dir},
// {fix}: the command that opens it).
// Its text is the check's own, with the repair.
func untraversableInstallDirError(err error) (error, bool) {
	var blocked *provision.InstallDirUntraversableError
	if !errors.As(err, &blocked) {
		return nil, false
	}
	return wrapProductError(ErrorNodeInstallDirUntraversable, blocked.Error(),
		map[string]any{"dir": blocked.Ancestor, "mode": blocked.Mode, "install_dir": blocked.InstallDir, "fix": blocked.FixCommand()}, err), true
}

// installedSSHSecrets is what out-of-band recovery keeps after an install:
// nothing when the target now accepts the platform key (empty secrets mean
// "log in with the platform key"), otherwise the credential that worked.
func installedSSHSecrets(auth provision.SSHAuth, report provision.SSHInstallReport) store.NodeSSHSecrets {
	if report.PlatformKeyAuthorized {
		return store.NodeSSHSecrets{}
	}
	switch report.AuthUsed {
	case provision.AuthPassword, provision.AuthKeyboardInteractive:
		return store.NodeSSHSecrets{Password: auth.Password}
	case provision.AuthPrivateKey:
		return store.NodeSSHSecrets{PrivateKeyPEM: auth.PrivateKeyPEM}
	case provision.AuthInstancePassword:
		return store.NodeSSHSecrets{Password: auth.InstancePassword}
	}
	return store.NodeSSHSecrets{}
}

func serverDialInstallCheck(ctx context.Context, addr, secret string, dial func(context.Context, string, string) error) provision.CheckResult {
	check := provision.CheckResult{Name: "server_dial", Status: "warn"}
	addr = strings.TrimSpace(addr)
	if addr == "" {
		check.Detail = "server_dial address is empty"
		return check
	}
	if dial == nil {
		check.Detail = "server_dial is not configured"
		return check
	}
	const attempts = 2
	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		attemptCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		err := dial(attemptCtx, addr, secret)
		cancel()
		if err == nil {
			return provision.CheckResult{Name: "server_dial", Status: "ok", Detail: "connected to " + addr}
		}
		lastErr = err
		if attempt+1 >= attempts {
			break
		}
		select {
		case <-ctx.Done():
			check.Detail = ctx.Err().Error()
			return check
		case <-time.After(time.Second):
		}
	}
	check.Detail = "queued for reconnection: " + lastErr.Error()
	return check
}
