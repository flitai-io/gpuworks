package app

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"gpuworks.ai/gpuworks/internal/server/api"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/selfinstall"
	"gpuworks.ai/gpuworks/internal/version"
)

// Self-upgrade from the console (ADR 0040). The server swaps its own binary
// and restarts through the service manager, so it only offers this when that
// manager exists and the binary it runs is the installation's, in a tree it
// can write.

// installLayout is the installation this process runs from, or the reason a
// console upgrade is not possible: no_service_manager, not_installed,
// not_installed_binary or not_writable.
func (s *serverSettings) installLayout() (selfinstall.Layout, string) {
	if s.restart == nil {
		return selfinstall.Layout{}, "no_service_manager"
	}
	layout, reason := configuredInstallLayout(s.cfg, s.executable())
	if reason != "" {
		return selfinstall.Layout{}, reason
	}
	probe, err := os.CreateTemp(layout.Dir, ".write-probe-*")
	if err != nil {
		return selfinstall.Layout{}, "not_writable"
	}
	_ = probe.Close()
	_ = os.Remove(probe.Name())
	return layout, ""
}

func (s *serverSettings) executable() string {
	if s.executablePath != "" {
		return s.executablePath
	}
	path, err := os.Executable()
	if err != nil {
		return ""
	}
	return path
}

func sameFile(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	resolvedA, errA := filepath.EvalSymlinks(a)
	resolvedB, errB := filepath.EvalSymlinks(b)
	return errA == nil && errB == nil && resolvedA == resolvedB
}

func (s *serverSettings) InstallDir() string {
	if s.cfg.ConfigPath == "" {
		return ""
	}
	return filepath.Dir(s.cfg.ConfigPath)
}

func (s *serverSettings) SelfUpgradeBlocker() string {
	_, blocker := s.installLayout()
	return blocker
}

func (s *serverSettings) RollbackResult(attempt, backup string) (dto.ServerUpgradeProgress, error) {
	unknown := dto.ServerUpgradeProgress{Phase: "unknown"}
	layout, blocker := configuredInstallLayout(s.cfg, s.executable())
	if blocker != "" || (attempt == "" && backup == "") {
		return unknown, nil
	}
	entries, err := selfinstall.ReadHistory(layout.HistoryPath())
	if errors.Is(err, os.ErrNotExist) {
		return unknown, nil
	}
	if err != nil {
		return unknown, err
	}
	for i := len(entries) - 1; i >= 0; i-- {
		e := entries[i]
		if e.Action != selfinstall.ActionUpgrade || !(attempt != "" && e.Attempt == attempt || backup != "" && e.Backup == backup) {
			continue
		}
		if e.ConfirmedRollback() && e.PreviousReleaseID == version.Version {
			return dto.ServerUpgradeProgress{Attempt: e.Attempt, Phase: "rollback", From: e.PreviousReleaseID, To: e.ReleaseID}, nil
		}
		break
	}
	return unknown, nil
}

func (s *serverSettings) Upgrade(ctx context.Context, archive io.Reader, by string, target dto.ServerUpgradeSelection) (dto.ServerUpgradeStarted, error) {
	layout, blocker := s.installLayout()
	if blocker != "" {
		return dto.ServerUpgradeStarted{}, &api.ServerSettingsError{Code: api.ErrorServerUpgradeUnavailable,
			Fallback: "this server cannot upgrade itself (" + blocker + "); run gpuworks upgrade on its host",
			Params:   map[string]any{"reason": blocker}}
	}
	if s.snapshot == nil {
		return dto.ServerUpgradeStarted{}, errors.New("no database to snapshot")
	}
	installer := selfinstall.New(layout, selfinstall.NewDeps(io.Discard), &selfinstall.IO{Out: io.Discard, Err: io.Discard, Quiet: true})
	up, err := installer.StageSelectedInService(ctx, version.Version, archive, s.snapshot, strings.TrimSpace(by), target.Version, target.Attempt)
	var archiveErr *selfinstall.ArchiveError
	var tooLarge *http.MaxBytesError
	switch {
	case errors.Is(err, selfinstall.ErrAlreadyAtRelease):
		return dto.ServerUpgradeStarted{}, &api.ServerSettingsError{Code: api.ErrorServerAlreadyAtRelease,
			Fallback: "the archive holds " + version.Version + ", which this server already runs", Params: map[string]any{"release": version.Version}}
	case errors.As(err, &archiveErr), errors.As(err, &tooLarge):
		return dto.ServerUpgradeStarted{}, &api.ServerSettingsError{Code: api.ErrorServerUpgradeArchiveInvalid,
			Fallback: "not a gpuworks release archive for this server: " + err.Error(), Params: map[string]any{"detail": err.Error()}}
	case err != nil:
		return dto.ServerUpgradeStarted{}, err
	}
	s.restart()
	return dto.ServerUpgradeStarted{From: up.From, To: up.To, Backup: up.Backup}, nil
}
