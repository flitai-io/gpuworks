package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/provider"
	"gpuworks.ai/gpuworks/internal/server/provision"
)

func (a *apiHandlers) installAgentOnInstance(w http.ResponseWriter, r *http.Request) {
	if a.d.Providers == nil {
		writeErr(w, http.StatusServiceUnavailable, errNoProviders)
		return
	}
	var req struct {
		ExternalID        string `json:"external_id"`
		ServerURL         string `json:"server_url"`
		SSHKeyPEM         string `json:"ssh_key_pem"`
		Password          string `json:"password"`
		SSHConnectionID   string `json:"ssh_connection_id"`
		ConnectMode       string `json:"connect_mode"`
		ServerDialListen  string `json:"server_dial_listen"`
		HostAddress       string `json:"host_address"`
		ServerDialAddress string `json:"server_dial_address"`
		// AllowInstanceRestart: the operator agreed that opening a port for
		// a server-dialed agent restarts the instance (RunPod).
		AllowInstanceRestart bool `json:"allow_instance_restart"`
		// InstallDir is the agent's install directory on the instance, ""
		// for /opt/gpuworks; NoSystemd runs it under nohup instead of the
		// gpuworks-agent unit.
		InstallDir string `json:"install_dir"`
		NoSystemd  bool   `json:"no_systemd"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if req.ConnectMode == "server_dial" && strings.TrimSpace(req.ServerDialListen) != "" {
		if err := validServerDialListen(req.ServerDialListen); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
	}
	if err := validCloudInstallDir(req.InstallDir); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	serverURL, serverURLErr := installServerURL(req.ServerURL, req.ConnectMode)
	if serverURLErr != nil {
		writeErr(w, http.StatusBadRequest, serverURLErr)
		return
	}
	precheck, precheckErr := a.d.Providers.Precheck(r.Context(), parseUint(chi.URLParam(r, "id")), req.ExternalID, serverURL, provider.InstallOptions{
		SSHKeyPEM: req.SSHKeyPEM, Password: req.Password, SSHConnectionID: req.SSHConnectionID, ConnectMode: req.ConnectMode, ServerDialListen: req.ServerDialListen,
		HostAddress: req.HostAddress, ServerDialAddress: req.ServerDialAddress,
		InstallDir: req.InstallDir, NoSystemd: req.NoSystemd,
	})
	if precheckErr != nil {
		// Prechecks remain advisory until their rules are stable enough to gate installs.
		precheck = provision.SSHPrecheckReport{
			Target: req.ExternalID,
			Checks: []provision.CheckResult{provider.SSHConnectFailedCheck(req.ExternalID, precheckErr)},
		}
	}
	result, err := a.d.Providers.Install(r.Context(), parseUint(chi.URLParam(r, "id")), req.ExternalID, serverURL, provider.InstallOptions{
		SSHKeyPEM: req.SSHKeyPEM, Password: req.Password, SSHConnectionID: req.SSHConnectionID, ConnectMode: req.ConnectMode,
		ServerDialListen: req.ServerDialListen, HostAddress: req.HostAddress, ServerDialAddress: req.ServerDialAddress,
		AllowInstanceRestart: req.AllowInstanceRestart,
		InstallDir:           req.InstallDir, NoSystemd: req.NoSystemd,
	})
	if errors.Is(err, provider.ErrInstanceRestartRequired) {
		// Nothing was changed: ask the operator, who may agree and retry.
		// need says what the restart is for, which the question names.
		var restart *provider.InstanceRestartError
		params := map[string]any{"need": "port"}
		if errors.As(err, &restart) && restart.Need != "" {
			params["need"] = restart.Need
		}
		writeErr(w, http.StatusConflict, wrapProductError(ErrorInstanceRestartRequired, err.Error(), params, err))
		return
	}
	if blocked, ok := untraversableInstallDirError(err); ok {
		// Not the vendor's failure: the operator can fix the path.
		writeErr(w, http.StatusBadRequest, blocked)
		return
	}
	var inCluster *provider.InstanceInClusterError
	if errors.As(err, &inCluster) {
		writeErr(w, http.StatusConflict, wrapProductError(ErrorProviderInstanceInCluster, err.Error(), map[string]any{"cluster": inCluster.Cluster}, err))
		return
	}
	if err != nil {
		writeErr(w, statusProviderFailed, err)
		return
	}
	installCheck := provision.SkipCheck(provision.CheckIDInstallAgent, messages.CheckAgentUnverified, "provider driver did not return agent process verification", nil)
	for _, check := range result.InstallChecks {
		if check.ID == provision.CheckIDAgentProcess {
			installCheck = check
			installCheck.ID = provision.CheckIDInstallAgent
			break
		}
	}
	checks := []provision.CheckResult{summarizePrecheck(precheck.Checks), installCheck}
	if result.ConnectMode == "server_dial" {
		checks = append(checks, serverDialInstallCheck(r.Context(), result.ServerDialAddress, result.Token, a.d.ServerDialAgent))
	} else {
		checks = append(checks, agentDialInstallCheck(r.Context(), result.NodeUUID, a.d.Online))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "installing", "node_uuid": result.NodeUUID, "server_url": serverURL,
		"connect_mode":        result.ConnectMode,
		"server_dial_address": result.ServerDialAddress, "ssh_connection": result.SSHConnection,
		"ssh_attempts": result.SSHAttempts, "checks": checks,
		// Every step behind the summary rows: how the platform key reached the
		// instance, which SSH credential worked, and the driver's own checks.
		"install_details": result.InstallChecks,
		// Every item behind the one-line "precheck" row, passing ones included.
		"precheck_details": precheck.Checks,
	})
}

func agentDialInstallCheck(ctx context.Context, nodeUUID string, online func(string) bool) provision.CheckResult {
	waiting := provision.WarnCheck(provision.CheckIDAgentDial, messages.CheckAgentSessionWaiting, "agent started; waiting for its first session", nil)
	established := provision.PassCheck(provision.CheckIDAgentDial, messages.CheckAgentSessionEstablished, "agent session established", nil)
	if strings.TrimSpace(nodeUUID) == "" || online == nil {
		return provision.SkipCheck(provision.CheckIDAgentDial, messages.CheckAgentSessionUnobserved, "the server cannot observe the agent's session here", nil)
	}
	if online(nodeUUID) {
		return established
	}
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return waiting.WithEvidence(ctx.Err().Error())
		case <-timer.C:
			return waiting
		case <-ticker.C:
			if online(nodeUUID) {
				return established
			}
		}
	}
}

// summarizePrecheck is the one "precheck" row of an install: the worst status
// of its items, and the ids of those that need attention, so a warning is
// not buried among a dozen passing ones; every item, passing ones included,
// is in the response's precheck_details.
func summarizePrecheck(checks []provision.CheckResult) provision.CheckResult {
	if len(checks) == 0 {
		return provision.SkipCheck(provision.CheckIDPrecheck, messages.CheckPrecheckEmpty, "the provider precheck reported no items", nil)
	}
	var ids, parts, evidence []string
	for _, check := range checks {
		if check.Status == proto.CheckPass || check.Status == proto.CheckNA {
			continue
		}
		ids = append(ids, check.ID)
		if message := strings.TrimSpace(check.Message); message != "" {
			parts = append(parts, check.ID+": "+message)
		} else {
			parts = append(parts, check.ID)
		}
		if check.Evidence != "" {
			evidence = append(evidence, check.ID+": "+check.Evidence)
		}
	}
	status := provision.WorstStatus(checks)
	if len(ids) == 0 {
		return provision.PassCheck(provision.CheckIDPrecheck, messages.CheckPrecheckPassed, fmt.Sprintf("all %d checks passed", len(checks)), map[string]any{"total": len(checks)})
	}
	summary := provision.WarnCheck(provision.CheckIDPrecheck, messages.CheckPrecheckAttention, strings.Join(parts, "; "),
		map[string]any{"count": len(ids), "total": len(checks), "checks": strings.Join(ids, ", ")})
	summary.Status = status
	return summary.WithEvidence(strings.Join(evidence, "\n"))
}
