package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"

	"time"

	"gpuworks.ai/gpuworks/internal/agentconf"
)

const agentUnitPath = "/etc/systemd/system/gpuworks-agent.service"

type linuxUninstaller struct {
	binary, procDir, unitPath string
	run                       func(context.Context, ...string) error
	signal                    func(int) error
}

func (u linuxUninstaller) uninstall(ctx context.Context, state string) error {
	unit, err := os.ReadFile(u.unitPath)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read agent service: %w", err)
	}
	unitState := unitStateDir(string(unit))
	if state == "" {
		state = unitState
		if state == "" {
			state = defaultStateDir()
		}
	}
	state, err = filepath.Abs(state)
	if err != nil {
		return err
	}
	// The unit is this installation's only when it runs from this state
	// directory. A host can run several agents — the unit for one, nohup
	// agents from other directories — so another directory's unit is left
	// running and only this directory's agent is stopped.
	ownUnit := len(unit) > 0 && unitState != "" && filepath.Clean(unitState) == state
	if len(unit) > 0 && unitState == "" {
		return fmt.Errorf("agent service names no state directory; refusing to guess whether it is %s", state)
	}
	if ownUnit {
		if err := u.validateUnit(unit); err != nil {
			return err
		}
		if err := u.run(ctx, "disable", "--now", "gpuworks-agent.service"); err != nil {
			return err
		}
	}
	pids, err := u.processes(state)
	if err != nil {
		return err
	}
	for _, pid := range pids {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !u.matches(pid, state) {
			continue
		}
		if err := u.signal(pid); err != nil && !errors.Is(err, syscall.ESRCH) {
			return fmt.Errorf("stop agent process %d: %w", pid, err)
		}
	}
	if err := u.waitStopped(ctx, state); err != nil {
		return err
	}
	if ownUnit {
		if err := removeIfPresent(u.unitPath); err != nil {
			return err
		}
		if err := u.run(ctx, "daemon-reload"); err != nil {
			return err
		}
	}
	for _, file := range []string{"identity.json", "gpuworks-agent.pid", agentconf.FileName} {
		if err := removeIfPresent(filepath.Join(state, file)); err != nil {
			return err
		}
	}
	return nil
}

func unitStateDir(unit string) string {
	for _, line := range strings.Split(unit, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "ExecStart=") {
			continue
		}
		match := regexp.MustCompile(`(?:^|\s)--state(?:=|\s+)(?:"([^"]+)"|'([^']+)'|([^\s;]+))`).FindStringSubmatch(line)
		if len(match) == 0 {
			continue
		}
		for _, value := range match[1:] {
			if value != "" {
				return value
			}
		}
	}
	return ""
}

func (u linuxUninstaller) processes(state string) ([]int, error) {
	entries, err := os.ReadDir(u.procDir)
	if err != nil {
		return nil, fmt.Errorf("inspect agent processes: %w", err)
	}
	var pids []int
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid == os.Getpid() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(u.procDir, entry.Name(), "cmdline"))
		if os.IsNotExist(err) {
			if _, gone := os.Stat(filepath.Join(u.procDir, entry.Name())); os.IsNotExist(gone) {
				continue // A process may exit between listing and reading.
			}
		}
		if err != nil {
			return nil, fmt.Errorf("process %d command-line identity could not be read", pid)
		}
		if agentRunArgs(data, u.binary, state) {
			exe, err := os.Readlink(filepath.Join(u.procDir, entry.Name(), "exe"))
			if err != nil {
				if _, gone := os.Stat(filepath.Join(u.procDir, entry.Name())); os.IsNotExist(gone) {
					continue
				}
				return nil, fmt.Errorf("process %d executable identity could not be read", pid)
			}
			if sameAgentBinary(strings.TrimSuffix(exe, " (deleted)"), u.binary) && u.matches(pid, state) {
				pids = append(pids, pid)
			}
		}
	}
	return pids, nil
}

func (u linuxUninstaller) matches(pid int, state string) bool {
	dir := filepath.Join(u.procDir, strconv.Itoa(pid))
	data, err := os.ReadFile(filepath.Join(dir, "cmdline"))
	if err != nil {
		return false
	}
	if !agentRunArgs(data, u.binary, state) {
		return false
	}
	exe, err := os.Readlink(filepath.Join(dir, "exe"))
	if err != nil || !sameAgentBinary(strings.TrimSuffix(exe, " (deleted)"), u.binary) {
		return false
	}
	stat, _ := os.ReadFile(filepath.Join(dir, "stat"))
	if _, rest, found := strings.Cut(string(stat), ") "); found && strings.HasPrefix(rest, "Z ") {
		return false
	}
	return true
}

func agentRunArgs(data []byte, binary, state string) bool {
	args := strings.Split(strings.TrimRight(string(data), "\x00"), "\x00")
	if len(args) < 2 || args[1] != "run" || !sameAgentBinary(args[0], binary) {
		return false
	}
	for i, arg := range args {
		if arg == "--state" && i+1 < len(args) && filepath.Clean(args[i+1]) == state {
			return true
		}
		if strings.HasPrefix(arg, "--state=") && filepath.Clean(strings.TrimPrefix(arg, "--state=")) == state {
			return true
		}
	}
	return false
}

func sameAgentBinary(a, b string) bool {
	resolve := func(path string) string {
		if resolved, err := filepath.EvalSymlinks(path); err == nil {
			return resolved
		}
		return filepath.Clean(path)
	}
	return resolve(a) == resolve(b)
}

func (u linuxUninstaller) waitStopped(ctx context.Context, state string) error {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		pids, err := u.processes(state)
		if err != nil {
			return err
		}
		if len(pids) == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("agent processes %v have not stopped: %w", pids, ctx.Err())
		case <-ticker.C:
		}
	}
}

func removeIfPresent(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove %s: %w", path, err)
	}
	return nil
}
