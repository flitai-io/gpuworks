package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/agent/gpu"
	agentruntime "gpuworks.ai/gpuworks/internal/agent/runtime"
	dockerdriver "gpuworks.ai/gpuworks/internal/agent/runtime/docker"
	"gpuworks.ai/gpuworks/internal/agent/runtime/hostproc"
	"gpuworks.ai/gpuworks/internal/agent/runtime/nerdctl"
	"gpuworks.ai/gpuworks/internal/agent/runtime/udocker"
	"gpuworks.ai/gpuworks/internal/agentconf"
	"gpuworks.ai/gpuworks/internal/launchd"
	"gpuworks.ai/gpuworks/internal/uninstallcheck"
)

func resolveAgentUninstallStates(explicit string) ([]string, error) {
	var states []string
	if runtime.GOOS == "darwin" {
		h, err := launchd.NewHost(nil)
		if err != nil {
			return nil, err
		}
		binary, err := os.Executable()
		if err != nil {
			return nil, err
		}
		states, err = agentLaunchdStates(h, binary)
		if err != nil {
			return nil, err
		}
	} else {
		raw, err := os.ReadFile(agentUnitPath)
		if err != nil && !os.IsNotExist(err) {
			return nil, err
		}
		unitState := unitStateDir(string(raw))
		if len(raw) > 0 && unitState == "" {
			return nil, fmt.Errorf("agent service names no state directory")
		}
		if explicit == "" && unitState != "" {
			states = append(states, unitState)
		}
	}
	if explicit != "" {
		states = append(states, explicit)
	}
	if len(states) == 0 {
		states = append(states, defaultStateDir())
	}
	var out []string
	seen := map[string]bool{}
	for _, state := range states {
		path, err := filepath.Abs(state)
		if err != nil {
			return nil, err
		}
		if path == string(filepath.Separator) || strings.ContainsAny(path, "\r\n\x1b") {
			return nil, fmt.Errorf("state must name a specific installation directory")
		}
		if !seen[path] {
			out = append(out, path)
			seen[path] = true
		}
	}
	sort.Strings(out)
	return out, nil
}

func agentRemovalChecklist(state string) []uninstallcheck.Item {
	return []uninstallcheck.Item{
		{ID: "agent.installation", Status: uninstallcheck.Warn, Summary: "Selected agent state directory", Details: []string{state}, Action: "Confirm this is the state directory of the agent being removed."},
		{ID: "agent.service", Status: uninstallcheck.Warn, Summary: "Plan: stop only the matching agent service/processes and remove its autostart definition", Action: "Existing process and service ownership checks remain mandatory."},
		{ID: "agent.identity", Status: uninstallcheck.Warn, Summary: "Plan: remove node identity, connection configuration and PID files after confirmed shutdown", Details: []string{filepath.Join(state, "identity.json"), agentconf.Path(state), filepath.Join(state, "gpuworks-agent.pid")}},
		{ID: "agent.retained", Status: uninstallcheck.Warn, Summary: "Agent binary, models, logs, runtime state and workload resources are retained", Details: []string{state}, Action: "Stop or migrate deployments in the control plane before uninstall; remove the node's management record separately."},
		{ID: "agent.ports", Status: uninstallcheck.Warn, Summary: "Workload listeners are not removed by stopping the agent", Action: "Verify the deployment ports and GPU resources after stopping or migrating the workloads; no global free-port claim is made."},
	}
}

func agentResourceChecklist(ctx context.Context, state string) []uninstallcheck.Item {
	return append(agentInstallationChecklist(ctx, state), agentRuntimeChecklist(ctx, state)...)
}

func agentRuntimeChecklist(ctx context.Context, state string) []uninstallcheck.Item {
	cfg, err := agentconf.Load(state)
	items := []uninstallcheck.Item{}
	if err != nil {
		items = append(items, uninstallcheck.Item{ID: "agent.configuration", Status: uninstallcheck.Unknown, Summary: "Launch configuration could not be read or parsed", Action: "Inspect the configuration locally; connection secrets are not printed."})
	} else {
		items = append(items, uninstallcheck.Item{ID: "agent.configuration", Status: uninstallcheck.Pass, Summary: "Launch configuration was read", Details: []string{"configured runtime preference: " + safeRuntimePreference(cfg.RuntimeDriver), "connection mode: " + cfg.Mode()}})
		if strings.HasPrefix(safeRuntimePreference(cfg.RuntimeDriver), "unrecognized") {
			items = append(items, uninstallcheck.Item{ID: "runtime.configuration", Status: uninstallcheck.Unknown, Summary: "Configured runtime preference is not supported by this agent", Action: "Establish the actual runtime and inspect its workloads locally."})
		}
		if cfg.Mode() == agentconf.ServerDial {
			items = append(items, uninstallcheck.Item{ID: "agent.listener", Status: uninstallcheck.Warn, Summary: "Configured inbound agent listener", Details: agentListenerDetails(cfg), Action: "The service shutdown must release this listener; unrelated listeners are retained."})
		}
	}
	type candidate struct {
		driver           agentruntime.Driver
		required, shared bool
	}
	candidates := []candidate{{udocker.New(state), hasAgentState(state, "udocker") || hasAgentState(state, "udocker-state") || cfg.RuntimeDriver == "udocker" || cfg.RuntimeDriver == "runveil", false}, {hostproc.New(state), hasAgentState(state, "host-state") || cfg.RuntimeDriver == hostproc.DriverName, false}}
	for _, entry := range []struct {
		name   string
		driver agentruntime.Driver
	}{{"docker", dockerdriver.New()}, {"nerdctl", nerdctl.New()}} {
		_, e := exec.LookPath(entry.name)
		if e == nil || cfg.RuntimeDriver == entry.name {
			candidates = append(candidates, candidate{entry.driver, true, true})
		}
	}
	localStateMissing := (cfg.RuntimeDriver == "udocker" || cfg.RuntimeDriver == "runveil") && !hasAgentState(state, "udocker") && !hasAgentState(state, "udocker-state")
	localStateMissing = localStateMissing || (cfg.RuntimeDriver == hostproc.DriverName && !hasAgentState(state, "host-state"))
	if localStateMissing {
		items = append(items, uninstallcheck.Item{ID: "runtime.configured-state", Status: uninstallcheck.Unknown, Summary: "Configured local runtime state is unavailable", Action: "Establish the configured backend's state location; absent state cannot prove that its workloads stopped."})
	}
	inspected := 0
	for _, c := range candidates {
		if !c.required {
			continue
		}
		inspected++
		probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		items = append(items, inspectAgentRuntime(probeCtx, c.driver, c.shared))
		cancel()
	}
	if inspected == 0 {
		items = append(items, uninstallcheck.Item{ID: "runtime.inventory", Status: uninstallcheck.Unknown, Summary: "No workload runtime could be inventoried", Action: "Establish the actual backend; missing CLI tools do not prove that daemon workloads have stopped."})
	}
	items = append(items, agentLocalStateChecklist(state)...)
	items = append(items, agentGPUChecklist(ctx)...)
	return items
}

func hasAgentState(state, name string) bool {
	_, err := os.Stat(filepath.Join(state, name))
	return err == nil || !os.IsNotExist(err)
}

// A runtime name prefix identifies a candidate, never a particular agent owner.
func inspectAgentRuntime(ctx context.Context, d agentruntime.Driver, shared bool) uninstallcheck.Item {
	item := uninstallcheck.Item{ID: "runtime." + d.Name(), Status: uninstallcheck.Pass, Summary: "Runtime inventory observed", Action: "Stop or migrate relevant workloads; never remove shared-daemon containers by name prefix."}
	names, err := d.ListContainers(ctx)
	if err != nil {
		item.Status = uninstallcheck.Unknown
		item.Summary = "Runtime inventory could not be read"
		return item
	}
	running, err := d.RunningContainers(ctx)
	if err != nil {
		item.Status = uninstallcheck.Unknown
		item.Summary = "Running workload observation failed"
		return item
	}
	alive := map[string]bool{}
	for _, name := range running {
		alive[name] = true
	}
	for _, name := range names {
		if shared && !strings.HasPrefix(name, "gpuworks-") {
			continue
		}
		phase := "retained/stopped record"
		if alive[name] {
			phase = "reported running"
			item.Status = uninstallcheck.Blocked
		}
		if shared {
			phase += "; state ownership unknown"
			item.Status = uninstallcheck.Unknown
		}
		item.Details = append(item.Details, safeWorkloadName(name)+": "+phase)
	}
	// A list can omit a live entry. Include independently reported running names.
	for _, name := range running {
		if shared && !strings.HasPrefix(name, "gpuworks-") {
			continue
		}
		found := false
		for _, known := range names {
			found = found || known == name
		}
		if found {
			continue
		}
		item.Details = append(item.Details, safeWorkloadName(name)+": reported running; absent from inventory")
		item.Status = uninstallcheck.Unknown
	}
	if len(item.Details) == 0 {
		item.Summary = "No GPUWorks-named workload candidates observed in this runtime"
	}
	if !shared && len(names) > 0 && len(running) == 0 {
		item.Status = uninstallcheck.Unknown
		item.Summary = "Local workload records remain; a stopped group leader does not prove all workers exited"
	}
	return item
}

func agentGPUChecklist(ctx context.Context) []uninstallcheck.Item {
	// Query every supported vendor with a bounded context; missing tools and
	// unsupported providers remain explicit unknown observations.
	var items []uninstallcheck.Item
	for _, vendor := range accelerator.KnownVendors() {
		item := uninstallcheck.Item{ID: "gpu." + vendor, Status: uninstallcheck.Unknown, Summary: "Compute-process observation unavailable", Action: "Verify selected GPUs with the vendor tools; no process is stopped by this checklist."}
		if !gpu.ChoosableVendor(vendor) {
			items = append(items, item)
			continue
		}
		p := gpu.NewFor(vendor)
		lister, ok := p.(gpu.ComputeProcessLister)
		if ok && p.Vendor() == vendor && ctx.Err() == nil {
			probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			procs, err := lister.ComputeProcesses(probeCtx)
			cancel()
			if err == nil {
				item.Summary = "Vendor compute-process observation (ownership and full-host coverage are not established)"
				item.Status = uninstallcheck.Warn
				for _, proc := range procs {
					item.Details = append(item.Details, fmt.Sprintf("PID %d; device %s; GPU memory %s", proc.PID, safeDeviceAddress(proc.PCIeBDF), gpuProcessMemory(proc.MemoryBytes)))
				}
			}
		}
		items = append(items, item)
	}
	if len(items) == 0 {
		return []uninstallcheck.Item{{ID: "gpu.observation", Status: uninstallcheck.Unknown, Summary: "No supported vendor compute-process observation is available", Action: "Check GPU processes manually; unavailable optional vendor observation cannot establish whether GPUs are idle."}}
	}
	items = append(items, uninstallcheck.Item{ID: "gpu.coverage", Status: uninstallcheck.Unknown, Summary: "Vendor observations do not prove complete coverage or that every GPU is idle", Action: "Verify all devices selected for handover, including unsupported or mixed-vendor devices."})
	return items
}

func gpuProcessMemory(bytes int64) string {
	if bytes <= 0 {
		return "unknown"
	}
	return fmt.Sprintf("%d bytes", bytes)
}
