package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/server/expose"
	"gpuworks.ai/gpuworks/internal/server/selfinstall"
)

// uninstallExpose closes only the connector belonging to this installation.
// The server's upgrade lock is held by the caller throughout this operation.
func uninstallExpose(ctx context.Context, layout selfinstall.Layout, runner selfinstall.Runner, streams *selfinstall.IO, unitDir string) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	unitPath := filepath.Join(unitDir, expose.SystemdUnitName)
	owned, err := uninstallExposeUnitOwned(unitPath, layout)
	if err != nil {
		return nil, err
	}
	store := expose.NewStore(layout.DataDir)
	info, err := os.Lstat(store.Root())
	if errors.Is(err, os.ErrNotExist) {
		if owned {
			return removeUninstallExposeUnit(ctx, runner, unitPath)
		}
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("inspect public entry point: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("public entry point directory %s is not a real directory", store.Root())
	}
	if err := validateUninstallExposePaths(store); err != nil {
		return nil, err
	}
	held, err := store.SupervisorCheck()
	if err != nil {
		return nil, fmt.Errorf("cannot determine whether the public entry point is running: %w", err)
	}
	_, err = store.ReadConfig()
	if err != nil {
		if held || owned {
			return nil, fmt.Errorf("cannot safely close the public entry point with unreadable configuration: %w", err)
		}
		if !errors.Is(err, expose.ErrNoConfig) {
			streams.Warn("public entry point configuration is unreadable; no connector is running: %v", err)
		}
		return nil, nil
	}
	// The config lock prevents another expose CLI from overwriting the disable
	// request between the read and write. Do not hold it while waiting to exit.
	release, err := store.Lock(time.Second)
	if err != nil {
		return nil, err
	}
	cfg, err := store.ReadConfig()
	if err == nil {
		cfg.Desired = expose.DesiredDisabled
		cfg.Revision = expose.NewRevision()
		err = store.WriteConfig(cfg)
	}
	release()
	if err != nil {
		return nil, fmt.Errorf("disable the public entry point: %w", err)
	}
	var removed []string
	if owned {
		removed, err = removeUninstallExposeUnit(ctx, runner, unitPath)
		if err != nil {
			return removed, err
		}
	}
	if err := waitUninstallExpose(ctx, store, 45*time.Second); err != nil {
		return removed, err
	}
	streams.Say("[cleaned] public entry point stopped; provider tunnel and DNS retained")
	return removed, nil
}

// Match the complete generated command, never a substring of a path. A unit
// referencing only one of our binary/data paths is ambiguous and must block
// deletion of a potentially shared installation.
func uninstallExposeUnitOwned(path string, layout selfinstall.Layout) (bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return false, fmt.Errorf("cannot establish ownership of symbolic link %s", path)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return false, fmt.Errorf("inspect public entry point service: %w", err)
	}
	expected, err := expose.RenderServiceUnit(layout.Binary(), layout.DataDir, "")
	if err != nil {
		return false, err
	}
	var want string
	for _, line := range strings.Split(expected, "\n") {
		if strings.HasPrefix(line, "ExecStart=") {
			want = line
		}
	}
	var commands []string
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "ExecStart=") {
			commands = append(commands, line)
		}
	}
	if len(commands) == 1 && commands[0] == want {
		if err := selfinstall.CheckUninstallServiceOverrides(path); err != nil {
			return false, err
		}
		return true, nil
	}
	if strings.Contains(string(raw), layout.Binary()) || strings.Contains(string(raw), layout.DataDir) {
		return false, fmt.Errorf("public entry point service %s references this installation with an unrecognized command; close it before uninstalling", path)
	}
	return false, nil
}

func removeUninstallExposeUnit(ctx context.Context, runner selfinstall.Runner, unitPath string) ([]string, error) {
	run := func(args ...string) error {
		return runner.Run(ctx, selfinstall.Command{Path: "systemctl", Args: args, Sudo: true})
	}
	if err := run("disable", "--now", expose.SystemdUnitName); err != nil {
		return nil, err
	}
	if err := os.Remove(unitPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		if !errors.Is(err, os.ErrPermission) {
			return nil, err
		}
		if err := runner.Run(ctx, selfinstall.Command{Path: "rm", Args: []string{"-f", "--", unitPath}, Sudo: true}); err != nil {
			return nil, err
		}
	}
	if _, err := os.Lstat(unitPath); !errors.Is(err, os.ErrNotExist) {
		if err == nil {
			err = fmt.Errorf("service unit still exists after removal")
		}
		return nil, fmt.Errorf("remove %s: %w", unitPath, err)
	}
	removed := []string{unitPath}
	if err := run("daemon-reload"); err != nil {
		return removed, err
	}
	return removed, nil
}

func waitUninstallExpose(ctx context.Context, store *expose.Store, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		held, err := store.SupervisorCheck()
		if err != nil {
			return fmt.Errorf("check public entry point shutdown: %w", err)
		}
		if !held {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("public entry point did not exit; check %s: %w", store.DaemonLogPath(), ctx.Err())
		case <-ticker.C:
		}
	}
}

// Check every path Ensure/Lock may touch before any Store call can change modes
// or request shutdown. In particular an ordinary uninstall can retain a custom
// DataDir, so its ancestors need the same scrutiny as recursively removed data.
func validateUninstallExposePaths(store *expose.Store) error {
	paths := map[string]bool{
		store.Root():                                    true,
		filepath.Join(store.Root(), "logs"):             true,
		store.CredentialsDir():                          true,
		store.BinDir():                                  true,
		store.ConfigPath():                              false,
		filepath.Join(store.Root(), ".lock"):            false,
		filepath.Join(store.Root(), ".supervisor.lock"): false,
	}
	for path, directory := range paths {
		for current := path; ; current = filepath.Dir(current) {
			info, err := os.Lstat(current)
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			if err == nil {
				if info.Mode()&os.ModeSymlink != 0 {
					return fmt.Errorf("refusing symbolic link %s", current)
				}
				wantDir := directory || current != path
				if wantDir && !info.IsDir() || !wantDir && !info.Mode().IsRegular() {
					return fmt.Errorf("cannot determine public entry point state: unexpected file type at %s", current)
				}
			}
			if parent := filepath.Dir(current); parent == current {
				break
			}
		}
	}
	return nil
}
