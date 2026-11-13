package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"gpuworks.ai/gpuworks/internal/agent/runtime/hostproc"
	"gpuworks.ai/gpuworks/internal/agentconf"
	"gpuworks.ai/gpuworks/internal/uninstallcheck"
)

func safeRuntimePreference(s string) string {
	switch s {
	case "":
		return "automatic (actual fallback may differ)"
	case "docker", "nerdctl", "udocker", "runveil", hostproc.DriverName:
		return s
	default:
		return "unrecognized; actual runtime unknown"
	}
}

func agentListenerDetails(cfg agentconf.Config) []string {
	port, err := agentconf.ListenPort(cfg.Normalized().Listen)
	if err != nil {
		return []string{"configured listener port unknown"}
	}
	return []string{"configured agent listener port: " + strconv.Itoa(port)}
}

func safeWorkloadName(s string) string {
	if len(s) == 0 || len(s) > 128 {
		return "[unrecognized workload name]"
	}
	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && !strings.ContainsRune("-_.", r) {
			return "[unrecognized workload name]"
		}
	}
	return s
}

func safeDeviceAddress(s string) string {
	if s == "" || len(s) > 16 {
		return "unknown"
	}
	for _, r := range s {
		if (r < 'a' || r > 'f') && (r < 'A' || r > 'F') && (r < '0' || r > '9') && !strings.ContainsRune(":.", r) {
			return "unknown"
		}
	}
	return s
}

// Runtime drivers intentionally ignore incomplete creates and invalid PID files.
// Those records still prevent a checklist from declaring workload inventory empty.
func agentLocalStateChecklist(state string) []uninstallcheck.Item {
	var items []uninstallcheck.Item
	for _, dir := range []string{"udocker-state", "host-state", filepath.Join("udocker", "containers")} {
		entries, err := os.ReadDir(filepath.Join(state, dir))
		if os.IsNotExist(err) {
			continue
		}
		item := uninstallcheck.Item{ID: "runtime.local-records", Status: uninstallcheck.Unknown, Summary: "Persisted local workload records require verification", Action: "Verify the workers/process groups and unfinished creates locally; retained PID records do not prove GPU or worker inactivity."}
		if err != nil {
			item.Summary = "Persisted local workload state could not be read"
			items = append(items, item)
			continue
		}
		count := 0
		for _, entry := range entries {
			if dir == filepath.Join("udocker", "containers") || strings.HasSuffix(entry.Name(), ".pid") || strings.HasSuffix(entry.Name(), ".spec.json") {
				count++
			}
		}
		if count > 0 {
			item.Details = []string{filepath.Join(state, dir), fmt.Sprintf("persisted workload entries: %d", count)}
			items = append(items, item)
		}
	}
	return items
}
