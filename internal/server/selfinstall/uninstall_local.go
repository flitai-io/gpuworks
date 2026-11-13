package selfinstall

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/launchd"
	"gpuworks.ai/gpuworks/internal/uninstallcheck"
)

// Local observations explain this component's stop/removal scope. Unknown
// pre-stop runtime observations are disclosed independently of the resource
// gate: ownership validation and post-stop process/listener checks remain hard
// requirements, including when resources are explicitly retained.
func (i *Installer) uninstallLocalChecklist(ctx context.Context, inst installation) []uninstallcheck.Item {
	configPath := i.layout.ConfigPath()
	if inst.FromInstallJSON {
		configPath = i.layout.installJSONPath()
	}
	_, binaryErr := os.Lstat(i.layout.Binary())
	binaryPresence, installationStatus := "present", uninstallcheck.Pass
	if errors.Is(binaryErr, os.ErrNotExist) {
		binaryPresence = "absent"
	} else if binaryErr != nil {
		binaryPresence = "unverified"
		installationStatus = uninstallcheck.Unknown
	}
	items := []uninstallcheck.Item{
		{ID: "server.installation", Status: installationStatus, Summary: "Observed installation configuration and validated local removal scope", Details: []string{fmt.Sprintf("observed: configuration=%q mode=%q", configPath, inst.Mode), fmt.Sprintf("observed: binary=%q presence=%q", i.layout.Binary(), binaryPresence), fmt.Sprintf("configured: data_directory=%q log_directory=%q service_account=%q", i.layout.DataDir, i.layout.LogsDir, inst.ServiceUser)}},
		i.uninstallServiceObservation(ctx, inst),
		{ID: "server.listeners", Status: uninstallcheck.Unknown, Summary: "Configured listener addresses; current socket ownership and release are unverified", Action: "The stop-time listener checks must complete before removal. A configured address is not evidence of a live listener or an idle host."},
		{ID: "server.shutdown_checks", Status: uninstallcheck.Warn, Summary: "Planned shutdown checks, not observations at precheck time", Details: []string{"planned: close this installation's public connector, then stop its configured local supervisor", "planned: inspect the process table for remaining matching server processes and refuse removal if shutdown cannot be confirmed", "planned: check configured listener addresses after stopping; refuse removal while an address remains held", "planned: remove the owned autostart definition and selected local files only after these checks succeed"}},
	}
	for _, address := range installedAddrs(inst.Config) {
		items[2].Details = append(items[2].Details, fmt.Sprintf("configured: address=%q; not probed during read-only precheck", address))
	}
	return items
}

func (i *Installer) uninstallServiceObservation(ctx context.Context, inst installation) uninstallcheck.Item {
	item := uninstallcheck.Item{ID: "server.service", Status: uninstallcheck.Unknown, Summary: "Local service runtime and autostart state are unverified", Action: "Uninstall will stop the configured supervisor and independently verify matching processes and listeners before deleting files."}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if i.deps.Run == nil {
		item.Details = []string{"No command runner is available to observe the local supervisor."}
		return item
	}
	switch inst.Mode {
	case ModeSystemd:
		item.Details = []string{fmt.Sprintf("observed: matching validated on-disk service definition=%q", i.layout.UnitPath()), "scope: on-disk ownership was validated; this observation does not independently establish the manager's loaded executable identity"}
		output, err := i.deps.Run.Output(ctx, Command{Path: "systemctl", Args: []string{"show", "--no-pager", "--property=ActiveState", "--property=UnitFileState", ServiceName}})
		if err != nil {
			return item
		}
		active, autostart := uninstallSystemdState(output)
		if active == "" || autostart == "" {
			return item
		}
		item.Details = append(item.Details, fmt.Sprintf("observed manager state: active_state=%q autostart_state=%q", active, autostart))
		item.Status = uninstallcheck.Warn
		if (active == "inactive" || active == "failed") && (autostart == "disabled" || autostart == "masked" || autostart == "masked-runtime") {
			item.Status = uninstallcheck.Pass
		}
		item.Summary = "Observed systemd runtime and autostart state for the configured service"
	case ModeLaunchd:
		item.Details = []string{fmt.Sprintf("observed: matching validated on-disk launch daemon definition=%q", i.layout.LaunchdPath()), "configured: plist presence does not verify launchd enablement or prove that a process is running"}
		service := i.service(inst.Mode, inst.ServiceUser, i.unitConfig(inst.ServiceUser, inst.Mode))
		output, err := service.launchdPrint(ctx)
		if err != nil {
			return item
		}
		if pid := launchd.RunningPID(output); pid > 0 {
			item.Status = uninstallcheck.Unknown
			item.Summary = "Observed launchd daemon PID; boot enablement remains unverified"
			item.Details = append(item.Details, fmt.Sprintf("observed: launchd reports pid=%d (process identity is checked separately before signaling)", pid))
		} else {
			item.Details = append(item.Details, "observed: launchd returned a loaded definition without a running PID; this does not prove all server processes exited")
		}
	default:
		item.Details = []string{"observed: no matching owned systemd/launchd definition was selected; detached supervision is configured", fmt.Sprintf("configured: pid_file=%q", i.layout.PIDFile())}
		pid := readPID(i.layout.PIDFile())
		if pid == 0 {
			item.Details = append(item.Details, "observed: no usable managed PID file; manually started or orphaned servers have not been ruled out")
			return item
		}
		output, err := i.deps.Run.Output(ctx, Command{Path: "ps", Args: []string{"-p", fmt.Sprint(pid), "-o", "command="}})
		if err != nil {
			return item
		}
		command := strings.TrimSpace(output)
		if command == i.layout.Binary()+" serve" || strings.HasPrefix(command, i.layout.Binary()+" serve ") {
			item.Status = uninstallcheck.Warn
			item.Summary = "Observed recorded PID running the configured server command"
			item.Details = append(item.Details, fmt.Sprintf("observed: pid=%d; full arguments are omitted", pid))
		} else {
			item.Details = append(item.Details, "observed: recorded PID command did not establish this server's identity; no process will be signaled from this observation")
		}
	}
	return item
}

// Whitelist manager fields instead of exposing arbitrary command output, which
// may contain environment values or other unrelated service diagnostics.
func uninstallSystemdState(output string) (active, autostart string) {
	var activeCount, autostartCount int
	for _, line := range strings.Split(output, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		switch key {
		case "ActiveState":
			activeCount++
			switch value {
			case "active", "reloading", "inactive", "failed", "activating", "deactivating", "maintenance", "refreshing":
				active = value
			}
		case "UnitFileState":
			autostartCount++
			switch value {
			case "enabled", "enabled-runtime", "linked", "linked-runtime", "alias", "masked", "masked-runtime", "static", "disabled", "indirect", "generated", "transient":
				autostart = value
			}
		}
	}
	if activeCount != 1 || autostartCount != 1 {
		return "", ""
	}
	return active, autostart
}
