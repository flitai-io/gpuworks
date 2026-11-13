package app

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"gpuworks.ai/gpuworks/internal/securefile"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/installcred"
	"gpuworks.ai/gpuworks/internal/server/selfinstall"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// openCredentialStore enforces credential permissions and creation policy
// before the rest of the server allocates resources or accepts requests.
func openCredentialStore(cfg Config, events *bus.Bus, log *slog.Logger) (*store.Store, string, error) {
	policy, err := startupCredentialPolicy(cfg)
	if err != nil {
		return nil, "", err
	}
	if err := protectStartupCredentials(cfg, log, policy); err != nil {
		return nil, "", err
	}
	master, err := masterSecret(cfg, policy)
	if err != nil {
		return nil, "", fmt.Errorf("load credential encryption key: %w", err)
	}
	st, err := store.Open(cfg.DBPath, events, policy)
	if err != nil {
		return nil, "", err
	}
	if err := bootstrapAdminAccount(cfg, st, log, policy); err != nil {
		_ = st.Close()
		return nil, "", err
	}
	return st, master, nil
}

// configuredInstallLayout identifies the installation that owns this binary.
// A standalone configuration file does not authorize scanning its neighbours.
func configuredInstallLayout(cfg Config, executable string) (selfinstall.Layout, string) {
	if cfg.ConfigPath == "" {
		return selfinstall.Layout{}, "not_installed"
	}
	layout := selfinstall.NewLayout(filepath.Dir(cfg.ConfigPath), cfg.DataDir, "")
	if !sameFile(executable, layout.Binary()) {
		return selfinstall.Layout{}, "not_installed_binary"
	}
	return layout, ""
}

func startupCredentialPolicy(cfg Config) (securefile.Policy, error) {
	if cfg.ConfigPath == "" {
		return securefile.Policy{}, nil
	}
	executable, err := os.Executable()
	if err != nil {
		return securefile.Policy{}, fmt.Errorf("identify installation for credential protection: %w", err)
	}
	layout, reason := configuredInstallLayout(cfg, executable)
	if reason != "" {
		return securefile.Policy{}, nil
	}
	return selfinstall.SensitiveFilePolicy(layout)
}

func protectStartupCredentials(cfg Config, log *slog.Logger, policies ...securefile.Policy) error {
	if err := installcred.ProtectInitialCredentials(cfg.DataDir, policies...); err != nil {
		return fmt.Errorf("protect initial credentials: %w", err)
	}
	if cfg.ConfigPath == "" {
		return nil
	}
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("identify installation for credential protection: %w", err)
	}
	layout, reason := configuredInstallLayout(cfg, executable)
	if reason != "" {
		return nil
	}
	warn := func(path string, err error) {
		if log != nil {
			log.Warn("cannot protect backup snapshot; have its owner or root restrict it", "path", path, "err", err)
		}
	}
	if err := selfinstall.HardenSensitiveFiles(layout, warn); err != nil {
		return fmt.Errorf("protect installation credentials: %w", err)
	}
	return nil
}

// forgetInitialCredentials removes the install-time admin record once the
// console has replaced the password it holds.
func forgetInitialCredentials(cfg Config) func(username string) error {
	return func(username string) error {
		policy, err := startupCredentialPolicy(cfg)
		if err != nil {
			return err
		}
		return installcred.ForgetInitialCredentials(cfg.DataDir, username, policy)
	}
}
