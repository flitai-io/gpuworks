package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/diagnostics"
	"gpuworks.ai/gpuworks/internal/server/provision"
	"gpuworks.ai/gpuworks/internal/server/rpccontext"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/version"
)

func (a *apiHandlers) health(w http.ResponseWriter, r *http.Request) {
	nodes, _ := a.d.Store.ListNodes()
	sessions := 0
	if a.d.SessionCount != nil {
		sessions = a.d.SessionCount()
	}
	writeJSON(w, http.StatusOK, dto.Health{
		Status:   "ok",
		Version:  version.Version,
		Nodes:    len(nodes),
		Sessions: sessions,
	})
}

func (a *apiHandlers) createNode(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name             string `json:"name"`
		ConnectMode      string `json:"connect_mode"`
		ServerDialListen string `json:"server_dial_listen"`
		// HostAddress is where the server reaches the host for server_dial.
		HostAddress string `json:"host_address"`
		ServerURL   string `json:"server_url"`
	}
	if r.Body != nil {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
	}
	if !validateNodeName(w, req.Name) {
		return
	}
	serverDialListen := strings.TrimSpace(req.ServerDialListen)
	if serverDialListen == "" {
		serverDialListen = provision.DefaultServerDialListen
	}
	var serverDialAddress string
	if req.ConnectMode == "server_dial" {
		if err := validServerDialListen(serverDialListen); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		if strings.TrimSpace(req.HostAddress) == "" {
			writeProductError(w, http.StatusBadRequest, ErrorHostAddressRequired, "host_address is required for server_dial", nil)
			return
		}
		serverDialAddress = provision.ServerDialAddress(req.HostAddress, serverDialListen)
		if serverDialAddress == "" {
			writeProductError(w, http.StatusBadRequest, ErrorHostAddressInvalid, fmt.Sprintf("invalid host_address %q", req.HostAddress), map[string]any{"address": req.HostAddress})
			return
		}
	}
	// The address the agent will dial is the caller's to give (the console
	// sends the one its browser uses); the server does not guess its own.
	// An agent the server dials has none.
	serverURL, serverURLErr := installServerURL(req.ServerURL, req.ConnectMode)
	if serverURLErr != nil {
		writeErr(w, http.StatusBadRequest, serverURLErr)
		return
	}
	n, token, err := a.d.Store.CreateNodePending(req.Name, req.ConnectMode)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if err := a.d.Store.SetNodeServerURL(n.UUID, serverURL); err != nil {
		_ = a.d.Store.DeleteNode(n.UUID)
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if err := a.d.Store.SetNodeServerDialListen(n.UUID, serverDialListen); err != nil {
		_ = a.d.Store.DeleteNode(n.UUID)
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	n.ServerDialListen = serverDialListen
	n.ServerURL = strings.TrimRight(serverURL, "/")
	cmd := provision.InstallCommand(serverURL, token, req.ConnectMode, serverDialListen)
	if req.ConnectMode == "server_dial" {
		if err := a.d.Store.SetNodeServerDialAddress(n.UUID, serverDialAddress); err != nil {
			_ = a.d.Store.DeleteNode(n.UUID)
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		n.ServerDialAddress = serverDialAddress
	}
	writeJSON(w, http.StatusCreated, map[string]any{"node": toNodeDTO(n), "token": token, "install_command": cmd, "server_url": serverURL, "server_dial_address": n.ServerDialAddress})
}

// installServerURL is the server address an install records for the agent
// to dial: none for an agent the server dials (server_dial), which never
// contacts the server, whatever the caller sent; otherwise the caller's,
// required and validated (normalizeInstallServerURL).
func installServerURL(raw, connectMode string) (string, error) {
	if connectMode == "server_dial" {
		return "", nil
	}
	return normalizeInstallServerURL(raw)
}

// validServerDialListen rejects a listen address a server_dial agent cannot
// listen on before anything is created for it.
func validServerDialListen(listen string) error {
	if provision.ValidServerDialListen(listen) {
		return nil
	}
	return productError(ErrorServerDialListenInvalid, fmt.Sprintf("invalid server_dial_listen %q: use host:port or :port", listen), map[string]any{"address": listen})
}

// validCloudInstallDir accepts the install directory of a cloud install: an
// absolute path other than /, or "" for the default. Some providers hand it
// to install.sh on the instance, where nobody expands "~" for the server to
// record where the agent went.
func validCloudInstallDir(dir string) error {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return nil
	}
	if strings.HasPrefix(dir, "/") && strings.TrimRight(dir, "/") != "" && !strings.ContainsAny(dir, "\x00\n\r") {
		return nil
	}
	return productError(ErrorInstallDirInvalid, fmt.Sprintf("invalid install_dir %q: use an absolute path other than /", dir), map[string]any{"dir": dir})
}

// normalizeInstallServerURL validates and canonicalizes the control-plane base
// URL before it is persisted or embedded in a shell command. Query strings,
// fragments, and userinfo are not meaningful for the bootstrap endpoint and
// make accidental command/URL composition harder to reason about.
func normalizeInstallServerURL(raw string) (string, error) {
	raw = strings.TrimRight(strings.TrimSpace(raw), "/")
	if raw == "" {
		return "", productError(ErrorServerURLRequired, "server_url is required: the address the agent dials", nil)
	}
	invalid := func(reason string) error {
		return productError(ErrorServerURLInvalid, "server_url "+reason, map[string]any{"url": raw})
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", invalid("must be an absolute http(s) URL")
	}
	if !strings.EqualFold(u.Scheme, "http") && !strings.EqualFold(u.Scheme, "https") {
		return "", invalid("must use http:// or https://")
	}
	if u.User != nil {
		return "", invalid("must not contain userinfo")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return "", invalid("must not contain a query or fragment")
	}
	return raw, nil
}

func (a *apiHandlers) listRejects(w http.ResponseWriter, r *http.Request) {
	if a.d.RecentRejects == nil {
		writeJSON(w, http.StatusOK, dto.NewList[dto.AgentReject](nil))
		return
	}
	writeJSON(w, http.StatusOK, dto.NewList(a.d.RecentRejects()))
}

func (a *apiHandlers) listUpgradeErrors(w http.ResponseWriter, r *http.Request) {
	if a.d.AgentUpgrades.Errors == nil {
		writeJSON(w, http.StatusOK, map[string]string{})
		return
	}
	writeJSON(w, http.StatusOK, a.d.AgentUpgrades.Errors())
}

func (a *apiHandlers) getNode(w http.ResponseWriter, r *http.Request) {
	uuid := chi.URLParam(r, "uuid")
	n, err := a.d.Store.NodeWithGPUs(uuid)
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	view := toNodeDTO(n)
	if claims, ok := a.claimsFromRequest(r); ok && claims.Role == "admin" {
		view = toNodeDTOWithToken(n)
	}
	views := []dto.Node{view}
	a.fillResolvedNodeFields([]store.Node{*n}, views)
	a.fillNodeGPUIsolation(views)
	writeJSON(w, http.StatusOK, views[0])
}

// nodeDoctorTimeout is how long the doctor RPC is waited for. The ordinary
// 30-second RPC budget is too short for it: doctor runs CUDA and multi-GPU
// checks, and on a host whose driver answers slowly (a card that failed to
// initialize holds every client on the driver lock) its GPU queries alone can
// take tens of seconds. The agent bounds its own run below this (see
// doctorBudget in internal/agent/app), so a slow host returns a report rather
// than a timeout.
const nodeDoctorTimeout = 2 * time.Minute

func (a *apiHandlers) nodeDoctor(w http.ResponseWriter, r *http.Request) {
	if a.d.NodeDoctor != nil {
		claims, ok := a.claimsFromRequest(r)
		out, err := a.d.NodeDoctor.Diagnose(r.Context(), chi.URLParam(r, "uuid"), ok && claims.Role == "admin")
		if err != nil {
			status := http.StatusConflict
			if errors.Is(err, store.ErrNotFound) {
				status = http.StatusNotFound
			}
			if errors.Is(err, diagnostics.ErrDoctorResponse) {
				status = http.StatusBadGateway
			}
			writeErr(w, status, err)
			return
		}
		writeJSON(w, http.StatusOK, out)
		return
	}

	if a.d.CallNode == nil {
		writeProductError(w, http.StatusServiceUnavailable, ErrorNodeRPCUnavailable, "node RPC is not configured", nil)
		return
	}
	uuid := chi.URLParam(r, "uuid")
	ctx, cancel := rpccontext.WithTimeout(r.Context(), nodeDoctorTimeout)
	defer cancel()
	raw, err := a.d.CallNode(ctx, uuid, proto.RPCNodeDoctor, map[string]any{})
	if err != nil {
		writeErr(w, http.StatusConflict, err)
		return
	}
	// Normalize Agent results without losing hardware details or legacy text.
	var out proto.DoctorReport
	if err := json.Unmarshal(raw, &out); err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, diagnostics.AgentDoctorReport(out))
}

func (a *apiHandlers) updateNodeRuntime(w http.ResponseWriter, r *http.Request) {
	if a.d.CallNode == nil {
		writeProductError(w, http.StatusServiceUnavailable, ErrorNodeRPCUnavailable, "node RPC is not configured", nil)
		return
	}
	uuid := chi.URLParam(r, "uuid")
	var req struct {
		Runtime string `json:"runtime"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	preference := strings.ToLower(strings.TrimSpace(req.Runtime))
	if strings.TrimSpace(req.Runtime) == "" || !proto.ValidRuntimePreference(preference) {
		writeProductError(w, http.StatusBadRequest, ErrorNodeRuntimeInvalid, "runtime must be auto, docker, nerdctl, or udocker (runveil)", nil)
		return
	}
	// A Kubernetes node runs its instances as pods of its cluster: there is
	// no runtime to pick (the node DTO offers no runtime_choices).
	if node, err := a.d.Store.NodeByUUID(uuid); err == nil && node.KubeClusterID != 0 {
		writeProductError(w, http.StatusConflict, ErrorNodeRuntimeKube, "a Kubernetes node's runtime is its cluster's", nil)
		return
	}
	raw, err := a.d.CallNode(r.Context(), uuid, proto.RPCNodeSetRuntime, map[string]string{"runtime": preference})
	if err != nil {
		writeErr(w, http.StatusConflict, err)
		return
	}
	var result map[string]string
	if err := json.Unmarshal(raw, &result); err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	n, err := a.d.Store.NodeWithGPUs(uuid)
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	if err := a.d.Store.SetNodeRuntimePreference(uuid, preference); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	// The agent status heartbeat is authoritative and will update the stored
	// runtime shortly; return an immediate view for the selector.
	n.RuntimeDriver = result["runtime"]
	n.RuntimePreference = preference
	writeJSON(w, http.StatusOK, toNodeDTO(n))
}

func (a *apiHandlers) upgradeNode(w http.ResponseWriter, r *http.Request) {
	if a.d.AgentUpgrades.Service != nil {
		run, err := a.d.AgentUpgrades.Service.UpgradeNode(r.Context(), chi.URLParam(r, "uuid"), a.agentUpgradeActor(r))
		if err != nil {
			writeAgentUpgradeError(w, err)
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]any{"status": "upgrading", "run_id": run.ID})
		return
	}
	if a.d.CallNode == nil {
		writeProductError(w, http.StatusServiceUnavailable, ErrorNodeRPCUnavailable, "node RPC is not configured", nil)
		return
	}
	uuid := chi.URLParam(r, "uuid")
	if a.d.Online != nil && !a.d.Online(uuid) {
		writeProductError(w, http.StatusConflict, ErrorNodeOffline, "node is offline", nil)
		return
	}
	if a.d.NodeStreams != nil && a.d.Store != nil {
		node, err := a.d.Store.NodeByUUID(uuid)
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, err)
			return
		}
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		pushed, err := a.d.NodeStreams.UpgradeAgent(uuid, node.OS, node.Arch)
		if err != nil {
			writeErr(w, http.StatusConflict, err)
			return
		}
		if pushed {
			writeJSON(w, http.StatusAccepted, map[string]string{"status": "upgrading"})
			return
		}
	}
	raw, err := a.d.CallNode(r.Context(), uuid, proto.RPCAgentUpgrade, map[string]any{})
	if err != nil {
		writeErr(w, http.StatusConflict, err)
		return
	}
	var result any
	if len(raw) > 0 && json.Unmarshal(raw, &result) == nil {
		writeJSON(w, http.StatusAccepted, map[string]any{"status": "upgrading", "result": result})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "upgrading"})
}

func (a *apiHandlers) renameNode(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if !decodeJSONBody(w, r, &req) || !validateNodeName(w, req.Name) {
		return
	}
	n, err := a.d.Store.RenameNode(chi.URLParam(r, "uuid"), req.Name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, err)
			return
		}
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	// Renaming is available to every management user.  A node token is a
	// credential, so do not include it in this ordinary mutation response;
	// token material is only returned by the admin-only regenerate endpoint (or
	// an admin node detail request).
	writeJSON(w, http.StatusOK, toNodeDTO(n))
}

func (a *apiHandlers) regenerateNodeToken(w http.ResponseWriter, r *http.Request) {
	n, token, err := a.d.Store.RegenerateNodeToken(chi.URLParam(r, "uuid"))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, err)
			return
		}
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if a.d.KickNode != nil {
		a.d.KickNode(chi.URLParam(r, "uuid"))
	}
	writeJSON(w, http.StatusOK, map[string]any{"node": toNodeDTOWithToken(n), "token": token})
}
