package app

import (
	"fmt"
	"strconv"
	"strings"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/proto"
)

// renderDoctorText renders the report as the English text the CLI prints and
// an older server shows verbatim. The server renders the structured report in
// the reader's locale instead; the sentences here are its English fallback.
func renderDoctorText(r proto.DoctorReport) string {
	var b strings.Builder
	line := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }
	line("== gpuworks agent doctor ==")
	if h := r.Host; h != nil {
		line("host      %s  %s/%s  kernel %s", h.Hostname, h.OS, h.Arch, h.Kernel)
		line("cpu       %s x%d", h.CPUModel, h.CPUCores)
		line("memory    %s total, %s available", gibText(h.MemTotalBytes), gibText(h.MemAvailBytes))
		for i, d := range h.Disks {
			label := "disk"
			if i > 0 {
				label = ""
			}
			line("%-9s %s  %s total, %s available (%d%%)%s", label, d.Path, gibText(d.TotalBytes), gibText(d.AvailBytes), percent(d.AvailBytes, d.TotalBytes), mountText(d.Mount))
		}
	}

	line("")
	line("-- runtimes --")
	for _, rt := range r.Runtimes {
		value := rt.Version
		switch rt.Status {
		case proto.DoctorRuntimeNotFound:
			value = "not found"
		case proto.DoctorRuntimeQueryFailed:
			value = "present (query failed)"
		}
		if rt.Selected {
			value += " (selected)"
		}
		line("%-9s %s", rt.Name, strings.TrimSpace(value))
	}

	line("")
	line("-- accelerators --")
	if a := r.Accelerators; a == nil {
		line("no accelerator tool found (CPU-only node or driver not installed)")
	} else {
		// Health is counted over this node's own cards only. A card lent
		// to a virtual machine carries no finding, and counting it as OK
		// says the node has more working cards than it has.
		counts := map[string]int{}
		owned := 0
		for _, g := range a.GPUs {
			if !accelerator.CardUsable(g.Ownership) {
				continue
			}
			owned++
			counts[g.HealthStatus]++
		}
		usable := fmt.Sprintf("%d this node's", owned)
		if owned == len(a.GPUs) {
			usable = "all this node's"
		}
		line("vendor %s  driver %s  %d total, %s: %d OK, %d Warning, %d Error", a.Vendor, a.Driver, len(a.GPUs), usable,
			counts[accelerator.HealthOK], counts[accelerator.HealthWarning], counts[accelerator.HealthError])
		if len(a.GPUs) > 0 {
			line(" %-3s %-18s %-8s %-14s %-5s %s", "#", "name", "memory", "bdf", "numa", "state")
		}
		for _, g := range a.GPUs {
			// Ownership first: a card this node cannot touch has no health
			// verdict, and "OK" beside it would read as a card going spare.
			state := g.HealthStatus
			if g.HealthCode != "" {
				state += " (" + g.HealthCode + ")"
			}
			switch g.Ownership {
			case accelerator.CardPassedThrough:
				state = "passed through to a VM"
			case accelerator.CardWithoutDriver:
				state = "no driver bound"
			}
			index := strconv.Itoa(g.Index)
			if g.Index < 0 {
				// An index is what the vendor CLI hands out, and it did not.
				index = "-"
			}
			line(" %-3s %-18s %-8s %-14s %-5d %s", index, g.Name, gibText(g.MemoryBytes), g.PCIeBDF, g.NUMANode, state)
		}
	}

	if len(r.Findings) > 0 {
		line("")
		line("-- action required --")
		for _, f := range r.Findings {
			renderFinding(&b, f)
		}
	}

	if len(r.Facts) > 0 {
		line("")
		line("-- facts --")
		for _, fact := range r.Facts {
			value := fact.Value
			if fact.Code == proto.DoctorFactDeviceInjection && value == "explicit" {
				value = "gpuworks mounts the schedulable accelerators into the container itself"
			}
			if fact.Code == proto.DoctorFactRuntimePlugin && value == "none" {
				value = "none (explicit device injection)"
			}
			lines := strings.Split(strings.TrimSpace(value), "\n")
			line("%-17s %s", strings.ReplaceAll(fact.Code, "_", " "), lines[0])
			for _, more := range lines[1:] {
				line("%-17s %s", "", more)
			}
		}
	}
	return b.String()
}

// findingSummary is the English one-line statement of each finding code.
var findingSummary = map[string]string{
	accelerator.HealthMemoryRepairPending: "memory rows or pages are waiting to be replaced; the replacement takes effect at the next reset",
	accelerator.HealthMemoryUncorrectable: "uncorrectable memory errors since the driver loaded or the card was last reset",
	accelerator.HealthMemoryRemapFailed:   "a memory row could not be replaced; contact the hardware vendor",
	accelerator.HealthResetRequired:       "the driver reported an event that requires a card reset",
	accelerator.HealthRebootRequired:      "the driver reported an event that requires a host reboot",
	accelerator.HealthPowerCycleRequired:  "the driver reported an event that requires powering the host off and on",
	accelerator.HealthVendorWarning:       "the vendor tool reports a warning",
	accelerator.HealthVendorFault:         "the vendor tool reports an alarm or critical fault",
	accelerator.HealthRuntimeUnusable:     "the card is listed, but the accelerator runtime could not create a context on it; the platform does not schedule it",
	proto.DoctorDiskLow:                   "less than 10% of the filesystem is free",
	proto.DoctorRuntimeProvisionFailed:    "the selected runtime could not be set up on this node; every deployment fails until it is",
	proto.DoctorRuntimeStateUntraversable: "a directory above the agent's state directory is closed to the user workloads run as, so every launch fails with permission denied; run the fix command or move the state directory",
	proto.DoctorPathNoExec:                "the runtime executes programs from a filesystem mounted noexec",
	proto.DoctorPathReadOnly:              "the agent writes to a filesystem mounted read-only",
	proto.DoctorPathEphemeral:             "the path is on the container's own layer and is lost when the container is recreated; keep it on a persistent volume",
	proto.DoctorRuntimeUnavailable:        "the selected container runtime does not answer",
	proto.DoctorAcceleratorProbeFailed:    "the accelerator tool failed",
	proto.DoctorAcceleratorUnrecognized:   "this host has PCI functions of a supported accelerator vendor that no rule recognizes; send the ids below so the card can be added",
	proto.DoctorCUDAInitFailed:            "the CUDA driver could not be initialised",
	proto.DoctorMultiGPUCheckFailed:       "a multi-GPU check failed",
	proto.DoctorMacOSUnsupported:          "this macOS is older than Mac nodes support",
	proto.DoctorPeerMemoryMissing:         "the RDMA NICs cannot register the cards' memory because the peer-memory module is not loaded; PD over RDMA cannot run here until the fix command is run",
	proto.DoctorUCXNetlinkHang:            "this kernel answers an IPv6 route dump in a way that hangs UCX (bundled with NIXL), so a PD engine using NIXL never finishes starting here; run the fix command (it turns IPv6 off on the host) or use a different kernel",
	proto.DoctorMacOSIdleSleep:            "the Mac sleeps when idle on this power source and then drops off the cluster while no instance runs; to keep it awake on AC power: sudo pmset -c sleep 0",
}

func renderFinding(b *strings.Builder, f proto.DoctorFinding) {
	line := func(format string, args ...any) { fmt.Fprintf(b, format+"\n", args...) }
	head := "[" + f.Level + "] " + f.Code
	if len(f.Targets) > 0 {
		indexes := make([]string, len(f.Targets))
		for i, t := range f.Targets {
			indexes[i] = strconv.Itoa(t.Index)
		}
		head += ": accelerator " + strings.Join(indexes, ", ")
	}
	if f.Disk != nil {
		head += ": " + f.Disk.Path
		if f.Code == proto.DoctorDiskLow {
			head += " " + gibText(f.Disk.AvailBytes) + " free of " + gibText(f.Disk.TotalBytes)
		}
	}
	line("%s", head)
	if summary := findingSummary[f.Code]; summary != "" {
		line("  %s", summary)
	}
	if f.Probe != "" {
		line("  evidence  %s", indentCommand(f.Probe, "            "))
	}
	for _, e := range f.Evidence {
		line("            %s", fieldText(e))
	}
	if f.Recheck != "" {
		line("  recheck   %s", indentCommand(f.Recheck, "            "))
	}
	for _, t := range f.Targets {
		line("  accelerator %d  %s", t.Index, t.PCIeBDF)
		if t.Probe != "" {
			line("    evidence  %s", indentCommand(t.Probe, "              "))
		}
		for _, e := range t.Evidence {
			line("              %s", fieldText(e))
		}
		if t.Recheck != "" {
			line("    recheck   %s", indentCommand(t.Recheck, "              "))
		}
		for _, action := range t.Actions {
			line("    action    %s  (when the card is idle)", action)
		}
		for _, proc := range t.Processes {
			line("    running   pid %d %s: stop it before the action", proc.PID, proc.Name)
		}
	}
}

// indentCommand lines a multi-line command up under its label. A runtime
// check's command carries the script it runs, and left unindented its lines
// read as further findings rather than as one command the reader can copy.
func indentCommand(command, indent string) string {
	return strings.ReplaceAll(command, "\n", "\n"+indent)
}

func fieldText(f proto.DoctorField) string {
	if f.Name == "" {
		return f.Value
	}
	return f.Name + " = " + f.Value
}

func gibText(bytes int64) string {
	return strconv.FormatFloat(float64(bytes)/(1<<30), 'f', 1, 64) + " GiB"
}

func percent(part, total int64) int64 {
	if total <= 0 {
		return 0
	}
	return part * 100 / total
}

// mountText is the filesystem a disk line sits on: its type and the flags that
// matter, in brackets, or nothing for an agent that did not report a mount.
func mountText(m *proto.DoctorMount) string {
	if m == nil {
		return ""
	}
	parts := []string{m.FSType}
	for _, flag := range []struct {
		set  bool
		text string
	}{{m.Network, "network"}, {m.NoExec, "noexec"}, {m.ReadOnly, "read-only"}, {m.Ephemeral, "container layer"}} {
		if flag.set {
			parts = append(parts, flag.text)
		}
	}
	return "  [" + strings.Join(parts, ", ") + "]"
}
