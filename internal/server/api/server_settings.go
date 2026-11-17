package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/version"
)

// ServerSettings is the machine-level configuration of this server
// (server.yaml): the addresses it answers on.
// How others reach it is theirs to decide; the server keeps no external
// address of its own.
type ServerSettings interface {
	// LoopbackURL is the base URL the server answers on over 127.0.0.1, for
	// requests it makes to itself; "" when it binds no port.
	LoopbackURL() string
	// StorageDeniedPaths are the directories no storage may use: server.yaml's
	// storage_denied_paths, or the built-in list when it is not set.
	StorageDeniedPaths() []string
	Listen() dto.ServerListen
	// UpdateListen saves new addresses; localAddr is the address the request
	// arrived on, which must keep serving the console.
	UpdateListen(update dto.ServerListenUpdate, localAddr string) (dto.ServerListen, error)
	// Restart exits the process for the service manager to start again.
	Restart() error
	// UpdateChannel is the distribution channel this server follows, and
	// whether it can be changed (it is kept in server.yaml).
	UpdateChannel() (string, bool)
	// SetUpdateChannel switches the channel; it applies at once.
	SetUpdateChannel(channel string) error
	// AutoCheckUpdates is whether the server checks the channel by itself and
	// describes the fleet to it while doing so, and whether that can be
	// changed (it is kept in server.yaml).
	AutoCheckUpdates() (bool, bool)
	// SetAutoCheckUpdates switches it; it applies at once.
	SetAutoCheckUpdates(on bool) error
	// InstallID names this installation to the update channel: a one-way
	// derivative of its master key; "" when there is none.
	InstallID() string
	// SelfUpgradeBlocker says why this server cannot upgrade itself from the
	// console; "" when it can (ADR 0040).
	SelfUpgradeBlocker() string
	// InstallDir is the installation directory the server runs from (where
	// its server.yaml is); "" when it was started without one.
	InstallDir() string
	// Upgrade stages the release in archive, snapshots the installation, swaps
	// the binary and restarts; the next start confirms or rolls back. by names
	// who asked, for the install history.
	RollbackResult(attempt, backup string) (dto.ServerUpgradeProgress, error)
	Upgrade(ctx context.Context, archive io.Reader, by string, target dto.ServerUpgradeSelection) (dto.ServerUpgradeStarted, error)
}

// ServerSettingsError is a rejected change, carried to the client as a
// product error.
type ServerSettingsError struct {
	Code     string
	Fallback string
	Params   map[string]any
}

func (e *ServerSettingsError) Error() string { return e.Fallback }

// StaticServerSettings serves fixed values and cannot be edited; it backs
// harnesses that start the API without a configuration file. An empty
// Channel is the build's.
// A nil StorageDenied is the built-in denylist.
type StaticServerSettings struct {
	StorageDenied []string
	Loopback      string
	Channel       string
}

func (s StaticServerSettings) StorageDeniedPaths() []string {
	if s.StorageDenied == nil {
		return store.DefaultStorageDeniedPaths()
	}
	return append([]string{}, s.StorageDenied...)
}
func (s StaticServerSettings) LoopbackURL() string { return s.Loopback }
func (s StaticServerSettings) Listen() dto.ServerListen {
	return dto.ServerListen{Console: []string{}, Agent: []string{}, Gateway: []string{}, Running: []dto.ServerBinding{}, RestartMode: "command"}
}
func (s StaticServerSettings) UpdateListen(dto.ServerListenUpdate, string) (dto.ServerListen, error) {
	return dto.ServerListen{}, &ServerSettingsError{Code: ErrorServerListenReadOnly, Fallback: "the server was started without server.yaml"}
}
func (s StaticServerSettings) Restart() error {
	return &ServerSettingsError{Code: ErrorServerRestartManual, Fallback: "restart the server from its host"}
}
func (s StaticServerSettings) UpdateChannel() (string, bool) {
	if s.Channel != "" {
		return s.Channel, false
	}
	return version.Channel, false
}
func (s StaticServerSettings) SetUpdateChannel(string) error {
	return &ServerSettingsError{Code: ErrorUpdateChannelReadOnly, Fallback: "the server was started without server.yaml"}
}
func (s StaticServerSettings) AutoCheckUpdates() (bool, bool) { return true, false }
func (s StaticServerSettings) SetAutoCheckUpdates(bool) error {
	return &ServerSettingsError{Code: ErrorUpdateAutoCheckReadOnly, Fallback: "the server was started without server.yaml"}
}
func (s StaticServerSettings) InstallID() string          { return "" }
func (s StaticServerSettings) SelfUpgradeBlocker() string { return "no_service_manager" }
func (s StaticServerSettings) InstallDir() string         { return "" }
func (s StaticServerSettings) RollbackResult(string, string) (dto.ServerUpgradeProgress, error) {
	return dto.ServerUpgradeProgress{Phase: "unknown"}, nil
}

func (s StaticServerSettings) Upgrade(context.Context, io.Reader, string, dto.ServerUpgradeSelection) (dto.ServerUpgradeStarted, error) {
	return dto.ServerUpgradeStarted{}, &ServerSettingsError{Code: ErrorServerUpgradeUnavailable, Fallback: "upgrade the server from its host",
		Params: map[string]any{"reason": "no_service_manager"}}
}

func (a *apiHandlers) serverSettings() ServerSettings {
	if a.d.Server == nil {
		return StaticServerSettings{}
	}
	return a.d.Server
}

func (a *apiHandlers) getServerListen(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, a.serverSettings().Listen())
}

func (a *apiHandlers) putServerListen(w http.ResponseWriter, r *http.Request) {
	var update dto.ServerListenUpdate
	if err := json.NewDecoder(r.Body).Decode(&update); err != nil {
		writeProductError(w, http.StatusBadRequest, ErrorServerListenInvalid, "invalid listen configuration", map[string]any{"reason": err.Error()})
		return
	}
	local := ""
	if addr, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr); ok {
		local = addr.String()
	}
	out, err := a.serverSettings().UpdateListen(update, local)
	if err != nil {
		writeServerSettingsError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *apiHandlers) restartServer(w http.ResponseWriter, r *http.Request) {
	if err := a.serverSettings().Restart(); err != nil {
		writeServerSettingsError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "restarting"})
}

func writeServerSettingsError(w http.ResponseWriter, err error) {
	var settingsErr *ServerSettingsError
	if errors.As(err, &settingsErr) {
		status := http.StatusBadRequest
		switch settingsErr.Code {
		case ErrorServerListenReadOnly, ErrorServerRestartManual, ErrorServerUpgradeUnavailable, ErrorServerAlreadyAtRelease,
			ErrorUpdateChannelReadOnly, ErrorUpdateAutoCheckReadOnly, ErrorServerNewerThanRelease, ErrorUsageReportReadOnly:
			status = http.StatusConflict
		}
		writeProductError(w, status, settingsErr.Code, settingsErr.Fallback, settingsErr.Params)
		return
	}
	writeErr(w, http.StatusInternalServerError, err)
}
