package app

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"

	"gpuworks.ai/gpuworks/internal/debuglog"
	"gpuworks.ai/gpuworks/internal/securefile"
	"gpuworks.ai/gpuworks/internal/server/bus"

	"gpuworks.ai/gpuworks/internal/server/diagnostics"
	"gpuworks.ai/gpuworks/internal/server/hub"
	"gpuworks.ai/gpuworks/internal/server/proxy"
	"gpuworks.ai/gpuworks/internal/server/selfinstall"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// newDiagnostics is the installation's debug sessions and diagnostics
// collections, kept under <data>/diagnostics: the gateway records the
// sessions' requests, and collections read the stores and the log file. A
// server built without a debug tee (tests, embedding) runs without them.
func newDiagnostics(cfg Config, st *store.Store, h *hub.Hub, obs *observability, px *proxy.Proxy, log *slog.Logger) *diagnostics.Manager {
	if cfg.DebugLog == nil {
		return nil
	}
	m := diagnostics.NewManager(filepath.Join(cfg.DataDir, "diagnostics"), cfg.DebugLog, h, diagnosticsScope{st}, log)
	px.SetRequestRecorder(m)
	m.SetSources(diagnosticsSources{st: st, hub: h, logs: obs.Logs(), ux: obs.UX(), logFile: cfg.LogFile, dataDir: cfg.DataDir, backupsDir: installBackupsDir(cfg)})
	return m
}

// installBackupsDir is the upgrade backups directory of the installation
// whose server.yaml this is; empty without one, or before any upgrade made it.
func installBackupsDir(cfg Config) string {
	if cfg.ConfigPath == "" {
		return ""
	}
	dir := selfinstall.NewLayout(filepath.Dir(cfg.ConfigPath), cfg.DataDir, "").BackupsDir
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return ""
	}
	return dir
}

// diagnosticsScope resolves a debug session's scope against the store.
type diagnosticsScope struct{ st *store.Store }

// DeploymentNodes are the nodes the deployments' instances are on: one read
// per deployment, bounded by what an administrator picks.
func (s diagnosticsScope) DeploymentNodes(ids []uint) ([]string, error) {
	seen := map[uint]bool{}
	var out []string
	for _, id := range ids {
		instances, err := s.st.InstancesForDeployment(id)
		if err != nil {
			return nil, err
		}
		for _, inst := range instances {
			if inst.NodeID == 0 || seen[inst.NodeID] {
				continue
			}
			seen[inst.NodeID] = true
			if node, err := s.st.NodeByID(inst.NodeID); err == nil {
				out = append(out, node.UUID)
			}
		}
	}
	return out, nil
}

func (s diagnosticsScope) UnknownDeployment(ids []uint) (uint, bool) {
	for _, id := range ids {
		if _, err := s.st.GetDeployment(id); err != nil {
			return id, true
		}
	}
	return 0, false
}

func (s diagnosticsScope) UnknownNode(uuids []string) (string, bool) {
	for _, uuid := range uuids {
		if _, err := s.st.NodeByUUID(uuid); err != nil {
			return uuid, true
		}
	}
	return "", false
}

// OfflineDiagnosticsRequest is a collection made without the server running
// (gpuworks support-bundle): the data directory's databases and the log file
// are read directly; no node can be asked, so each named node is recorded as
// offline.
type OfflineDiagnosticsRequest struct {
	DataDir string
	DBPath  string
	LogFile string
	// WorkDir is where the collection is staged and packed; empty means the
	// data directory's diagnostics/bundles, which a full disk refuses.
	WorkDir string
	Request diagnostics.BundleRequest
}

// CollectDiagnosticsOffline builds the collection and answers its archive.
func CollectDiagnosticsOffline(ctx context.Context, in OfflineDiagnosticsRequest, log *slog.Logger, policies ...securefile.Policy) (diagnostics.Bundle, string, error) {
	st, err := store.Open(in.DBPath, bus.New(), policies...)
	if err != nil {
		return diagnostics.Bundle{}, "", err
	}
	defer st.Close()
	obs, err := openObservability(in.DataDir)
	if err != nil {
		return diagnostics.Bundle{}, "", err
	}
	defer obs.Close()
	m := diagnostics.NewManager(filepath.Join(in.DataDir, "diagnostics"), debuglog.New(slog.DiscardHandler), offlineNodes{}, diagnosticsScope{st}, log)
	m.SetSources(diagnosticsSources{st: st, logs: obs.Logs(), ux: obs.UX(), logFile: in.LogFile, dataDir: in.DataDir})
	if in.WorkDir != "" {
		m.SetBundleDir(in.WorkDir)
	}
	return m.CollectNow(ctx, in.Request)
}

// offlineNodes answers for a server that is not running: no node is reached.
type offlineNodes struct{}

func (offlineNodes) Online(string) bool { return false }

func (offlineNodes) CallNode(context.Context, string, string, any) (json.RawMessage, error) {
	return nil, errors.New("the server is not running")
}
