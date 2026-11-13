package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"gpuworks.ai/gpuworks/internal/agentconf"
	"gpuworks.ai/gpuworks/internal/launchd"
)

func agentLaunchdPaths(h *launchd.Host) []string {
	return []string{filepath.Join(h.Root, launchd.DaemonPlist()), filepath.Join(h.Root, launchd.AgentPlist(h.Self.Home))}
}

func agentLaunchdStates(h *launchd.Host, binary string) ([]string, error) {
	var states []string
	for _, path := range agentLaunchdPaths(h) {
		data, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, errors.New("launchd installation definition could not be read")
		}
		job, err := launchd.ReadOwnedAgentJob(data, binary)
		if err != nil {
			return nil, err
		}
		states = append(states, job.StateDir)
	}
	return states, nil
}

func uninstallLaunchdStates(ctx context.Context, selected []string, host *launchd.Host, binary string) error {
	var states []string
	for _, state := range selected {
		path, err := filepath.Abs(state)
		if err != nil || path == string(filepath.Separator) || strings.ContainsAny(path, "\r\n\x1b") {
			return errors.New("selected agent state is unsafe")
		}
		states = append(states, path)
		if agentChecklistBlocking(agentLaunchdChecklistForHost(ctx, path, host, binary), true) {
			return errors.New("selected agent state has unsafe or unverified component processes")
		}
	}
	_, err := host.Uninstall(ctx, binary, func(ctx context.Context, jobStates []string) error {
		all := append(append([]string(nil), states...), jobStates...)
		seen := map[string]bool{}
		for _, state := range all {
			if seen[state] {
				continue
			}
			seen[state] = true
			pids, err := host.AgentProcesses(ctx, binary, state)
			if err != nil {
				return err
			}
			if len(pids) > 0 {
				return errors.New("selected agent processes remain; identities retained")
			}
		}
		for state := range seen {
			if err := ctx.Err(); err != nil {
				return err
			}
			for _, name := range []string{"identity.json", agentconf.FileName} {
				if err := removeIfPresent(filepath.Join(state, name)); err != nil {
					return err
				}
			}
		}
		return nil
	})
	return err
}
