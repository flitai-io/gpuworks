package app

import (
	"log/slog"
	"path/filepath"

	"gpuworks.ai/gpuworks/internal/server/agentbin"
	"gpuworks.ai/gpuworks/internal/server/agentupgrade"
	"gpuworks.ai/gpuworks/internal/server/hub"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/version"
)

func newAgentUpgrades(st *store.Store, h *hub.Hub, dataDir string, log *slog.Logger) (*agentupgrade.Service, error) {
	dir := filepath.Join(dataDir, "dl")
	svc, err := agentupgrade.New(st, agentupgrade.Config{
		TargetVersion:   version.Version,
		ArtifactDir:     filepath.Join(dataDir, "agent-upgrades", "artifacts"),
		BinaryAvailable: func(name string) bool { return agentbin.Available(dir, name, version.Version) },
		ReadBinary:      func(name string) ([]byte, error) { return agentbin.ReadVerified(dir, name, version.Version) },
		Transport:       h, Logger: log,
	})
	if err != nil {
		return nil, err
	}
	h.SetAgentUpgradeObserver(svc.Observe)
	return svc, nil
}
