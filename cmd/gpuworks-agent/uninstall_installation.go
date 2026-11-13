package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/launchd"
	"gpuworks.ai/gpuworks/internal/uninstallcheck"
)

func agentInstallationChecklist(ctx context.Context, state string) []uninstallcheck.Item {
	if runtime.GOOS == "darwin" {
		return agentLaunchdChecklist(ctx, state)
	}
	binary, err := os.Executable()
	if err != nil {
		return []uninstallcheck.Item{{ID: "agent.safety.processes", Status: uninstallcheck.Unknown, Summary: "Current agent executable identity is unavailable"}}
	}
	u := linuxUninstaller{binary: binary, procDir: "/proc", unitPath: agentUnitPath}
	items := linuxAgentInstallationChecklist(u, state)
	if raw, err := os.ReadFile(agentUnitPath); err == nil && filepath.Clean(unitStateDir(string(raw))) == state {
		probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		out, err := exec.CommandContext(probeCtx, "systemctl", "show", "gpuworks-agent.service", "--property=ActiveState", "--property=UnitFileState").Output()
		item := uninstallcheck.Item{ID: "agent.service.observation", Status: uninstallcheck.Warn, Summary: "Service active/autostart status unavailable", Action: "Check systemd status locally; service stop failures prevent identity cleanup."}
		if err == nil {
			for _, line := range strings.Split(string(out), "\n") {
				key, value, ok := strings.Cut(line, "=")
				if ok && (key == "ActiveState" || key == "UnitFileState") && safeServiceValue(value) {
					item.Details = append(item.Details, key+"="+value)
				}
			}
			if len(item.Details) > 0 {
				item.Summary = "Systemd service active/autostart status observed"
			}
		}
		items = append(items, item)
	}
	return items
}

func linuxAgentInstallationChecklist(u linuxUninstaller, state string) []uninstallcheck.Item {
	service := uninstallcheck.Item{ID: "agent.safety.service", Status: uninstallcheck.Pass, Summary: "No systemd agent unit is installed", Details: []string{u.unitPath}}
	unit, err := os.ReadFile(u.unitPath)
	switch {
	case os.IsNotExist(err):
	case err != nil:
		service.Status, service.Summary = uninstallcheck.Unknown, "Agent service definition could not be read"
	case unitStateDir(string(unit)) == "":
		service.Status, service.Summary = uninstallcheck.Unknown, "Service definition does not identify its state directory"
	case filepath.Clean(unitStateDir(string(unit))) == state && u.validateUnit(unit) != nil:
		service.Status, service.Summary = uninstallcheck.Unknown, "Service executable or override ownership could not be established"
	case filepath.Clean(unitStateDir(string(unit))) == state:
		service.Summary = "Installed systemd unit belongs to the selected state directory"
		service.Action = "Stop/disable this unit and remove its definition after process shutdown."
	default:
		service.Status, service.Summary = uninstallcheck.Warn, "Systemd unit belongs to another state directory and will be retained"
		service.Action = "Only matching processes from the selected state directory will be stopped."
	}
	processes := uninstallcheck.Item{ID: "agent.safety.processes", Status: uninstallcheck.Pass, Summary: "Matching agent run processes observed from executable and state identity", Action: "Only these matching agent processes are eligible for shutdown; workload processes are retained."}
	pids, err := u.processes(state)
	if err != nil {
		processes.Status, processes.Summary = uninstallcheck.Unknown, "Agent process inventory could not be read"
	} else {
		processes.Details = append(processes.Details, fmt.Sprintf("visible matching agent processes: %d", len(pids)))
		for _, pid := range pids {
			processes.Details = append(processes.Details, "PID "+strconv.Itoa(pid))
		}
	}
	items := []uninstallcheck.Item{service, processes}
	if err == nil {
		items = append(items, agentTCPListenerChecklist(u.procDir, pids))
	}
	return items
}

func agentLaunchdChecklist(ctx context.Context, state string) []uninstallcheck.Item {
	h, err := launchd.NewHost(nil)
	if err != nil {
		return []uninstallcheck.Item{{ID: "agent.safety.launchd", Status: uninstallcheck.Unknown, Summary: "Launchd installation account could not be established"}}
	}
	binary, err := os.Executable()
	if err != nil {
		return []uninstallcheck.Item{{ID: "agent.safety.launchd", Status: uninstallcheck.Unknown, Summary: "Current agent executable identity is unavailable"}}
	}
	return agentLaunchdChecklistForHost(ctx, state, h, binary)
}

func agentLaunchdChecklistForHost(ctx context.Context, state string, h *launchd.Host, binary string) []uninstallcheck.Item {
	items := []uninstallcheck.Item{}
	eligiblePIDs := map[int]bool{}
	for _, path := range agentLaunchdPaths(h) {
		item := uninstallcheck.Item{ID: "agent.safety.launchd", Status: uninstallcheck.Pass, Summary: "Launchd autostart definition not installed", Details: []string{path}}
		data, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			items = append(items, item)
			continue
		}
		job, parseErr := launchd.ReadOwnedAgentJob(data, binary)
		if err != nil || parseErr != nil || job.StateDir == "" {
			item.Status, item.Summary = uninstallcheck.Unknown, "Launchd definition ownership could not be established"
		} else if filepath.Clean(job.StateDir) == state {
			item.Status, item.Summary = uninstallcheck.Warn, "Launchd autostart definition belongs to this selected state directory"
			item.Action = "Unload and remove this definition; launchd state and exit errors are checked during removal."
		} else {
			item.Status, item.Summary = uninstallcheck.Warn, "Launchd definition belongs to another selected installation state"
			item.Action = "The complete list of state directories covers both system and user launchd jobs."
		}
		items = append(items, item)
		if err == nil && parseErr == nil && job.StateDir != "" {
			domain := "system"
			if path != filepath.Join(h.Root, launchd.DaemonPlist()) {
				domain = "gui/" + strconv.Itoa(h.Self.UID)
			}
			probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			out, probeErr := h.Launchctl(probeCtx, "print", domain+"/"+launchd.AgentLabel)
			cancel()
			observation := uninstallcheck.Item{ID: "agent.safety.launchd-live", Status: uninstallcheck.Unknown, Summary: "Launchd job live state could not be observed", Action: "Inspect launchctl status locally; uninstall must verify job shutdown before clearing identity."}
			if launchd.IsAbsentJob(out, probeErr) {
				observation.Summary, observation.Status = "Launchd job is not registered in "+domain, uninstallcheck.Pass
			} else if probeErr == nil {
				pid, identityErr := launchd.AgentProgramPID(out, binary)
				if identityErr != nil {
					observation.Summary = "Loaded launchd executable ownership could not be established"
					items = append(items, observation)
					continue
				}
				if pid > 0 && filepath.Clean(job.StateDir) == filepath.Clean(state) {
					eligiblePIDs[pid] = true
				}
				observation.Summary = "Launchd job is registered in " + domain
				observation.Status = uninstallcheck.Warn
				for _, line := range strings.Split(out, "\n") {
					key, value, ok := strings.Cut(strings.TrimSpace(line), " = ")
					if !ok {
						continue
					}
					if key == "pid" {
						if pid, err := strconv.Atoi(value); err == nil && pid > 0 {
							observation.Details = append(observation.Details, "PID "+strconv.Itoa(pid))
						}
					} else if key == "state" && safeServiceValue(value) {
						observation.Details = append(observation.Details, "state="+value)
					}
				}
			}
			items = append(items, observation)

		}
	}
	pids, inventoryErr := h.AgentProcesses(ctx, binary, state)
	processes := uninstallcheck.Item{ID: "agent.safety.launchd-processes", Status: uninstallcheck.Pass, Summary: "Independent process table observed for the selected state", Action: "Agents without a corresponding owned launchd job must be stopped manually; resource retention does not bypass this check."}
	if inventoryErr != nil {
		processes.Status, processes.Summary = uninstallcheck.Unknown, "Independent agent process table could not be established"
	} else if len(pids) > 0 {
		processes.Status = uninstallcheck.Warn
		for _, pid := range pids {
			processes.Details = append(processes.Details, "PID "+strconv.Itoa(pid))
			if !eligiblePIDs[pid] {
				processes.Status = uninstallcheck.Blocked
			}
		}
	}
	return append(items, processes)
}

func safeServiceValue(s string) bool {
	if s == "" || len(s) > 40 {
		return false
	}
	for _, r := range s {
		if (r < 'a' || r > 'z') && r != '-' {
			return false
		}
	}
	return true
}
