package app

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/proto"
)

// The checks below run on macOS nodes only. They read power, firewall and
// Metal settings a Linux node does not have, and report them without changing
// any: the Mac is usually someone's own computer (ADR 0044).

// doctorMacRun runs one read-only command through the doctor's runner; tests
// substitute recorded output.
var doctorMacRun = func(ctx context.Context, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := checkDoctorCmdRun(ctx, name, args...)
	return string(out), err
}

const macFirewallTool = "/usr/libexec/ApplicationFirewall/socketfilterfw"

func doctorMacChecks(ctx context.Context, r *proto.DoctorReport, gpus []proto.GPUInfo) {
	sysctl, sysctlErr := doctorMacRun(ctx, "sysctl", "kern.osproductversion", "hw.memsize", "iogpu.wired_limit_mb")
	facts := parseSysctlLines(sysctl)

	version := facts["kern.osproductversion"]
	major, versionErr := strconv.Atoi(strings.SplitN(version, ".", 2)[0])
	switch {
	case version == "" || versionErr != nil:
		r.Findings = append(r.Findings, doctorMacToolSkip(proto.DoctorCheckMacOSVersion, "sysctl kern.osproductversion", "sw_vers", sysctlErr, sysctl))
	case major < accelerator.MinimumMacOSMajor:
		r.Findings = append(r.Findings, proto.DoctorFinding{
			ID: proto.DoctorCheckMacOSVersion, Status: proto.CheckFail, Code: proto.DoctorMacOSUnsupported,
			Probe:    "sysctl kern.osproductversion",
			Evidence: []proto.DoctorField{{Name: "macOS", Value: version}, {Name: "required", Value: strconv.Itoa(accelerator.MinimumMacOSMajor)}},
			Recheck:  "sw_vers",
		})
	}

	if value, ok := metalWorkingSetFact(facts, gpus); ok {
		r.Facts = append(r.Facts, proto.DoctorFact{Code: proto.DoctorFactMetalWorkingSet, Value: value})
	}

	settings, settingsErr := doctorMacRun(ctx, "pmset", "-g")
	battery, batteryErr := doctorMacRun(ctx, "pmset", "-g", "batt")
	if err := errors.Join(settingsErr, batteryErr); err != nil {
		r.Findings = append(r.Findings, doctorMacToolSkip(proto.DoctorCheckMacOSSleep, "pmset -g", "pmset -g", err, ""))
	} else {
		power := parsePmset(settings, battery)
		r.Facts = append(r.Facts, proto.DoctorFact{Code: proto.DoctorFactPower, Value: power.text()})
		if power.idleSleepMinutes > 0 && !power.sleepDisabled {
			r.Findings = append(r.Findings, proto.DoctorFinding{
				ID: proto.DoctorCheckMacOSSleep, Status: proto.CheckWarn, Code: proto.DoctorMacOSIdleSleep,
				Probe: "pmset -g",
				Evidence: []proto.DoctorField{
					{Name: "sleep", Value: power.sleepSetting},
					{Name: "power source", Value: power.source},
				},
				Recheck: "pmset -g",
			})
		}
	}

	if out, err := doctorMacRun(ctx, macFirewallTool, "--getglobalstate", "--getstealthmode"); err == nil {
		r.Facts = append(r.Facts, proto.DoctorFact{Code: proto.DoctorFactMacOSFirewall, Value: strings.Join(strings.Fields(strings.ReplaceAll(strings.TrimSpace(out), "\n", "; ")), " ")})
	}
}

// doctorMacToolSkip is a macOS check that could not read what it judges:
// the command failed, or answered with nothing the check understands.
func doctorMacToolSkip(id, probe, recheck string, err error, out string) proto.DoctorFinding {
	detail := strings.TrimSpace(out)
	if err != nil {
		detail = err.Error()
	}
	if detail == "" {
		detail = "no output"
	}
	return proto.DoctorFinding{
		ID: id, Status: proto.CheckSkip, Code: proto.DoctorSkipToolFailed,
		Probe: probe, Evidence: []proto.DoctorField{{Name: "result", Value: detail}}, Recheck: recheck,
	}
}

// parseSysctlLines reads `sysctl name...` output ("name: value" per line).
func parseSysctlLines(out string) map[string]string {
	facts := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if k, v, ok := strings.Cut(line, ":"); ok {
			facts[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return facts
}

// metalWorkingSetFact states how much of the Mac's memory Metal lets the GPU
// keep resident, where that comes from, and how a person raises it. The agent
// never changes it (docs/adr/0044-mac-engines-as-host-processes.md): a larger
// share squeezes the owner's own applications.
func metalWorkingSetFact(facts map[string]string, gpus []proto.GPUInfo) (string, bool) {
	memBytes, err := strconv.ParseInt(facts["hw.memsize"], 10, 64)
	if err != nil || memBytes <= 0 || len(gpus) == 0 || gpus[0].MemoryTotalBytes <= 0 {
		return "", false
	}
	limit := facts["iogpu.wired_limit_mb"]
	source := "iogpu.wired_limit_mb=" + limit
	if limit == "" || limit == "0" {
		source = "iogpu.wired_limit_mb=0, the macOS default"
	}
	return fmt.Sprintf("GPU may keep %s of %s memory resident (%s)\nto raise it until the next reboot: sudo sysctl iogpu.wired_limit_mb=<MiB>",
		gibText(gpus[0].MemoryTotalBytes), gibText(memBytes), source), true
}

// macPower is what pmset reports about sleep and power.
type macPower struct {
	source           string // "AC Power" or "Battery Power"
	battery          string // "63%; discharging; 3:45 remaining", if there is a battery
	sleepSetting     string // the raw "sleep" value, e.g. "1 (sleep prevented by powerd)"
	idleSleepMinutes int
	sleepDisabled    bool
	lowPower         bool
}

// parsePmset reads `pmset -g` (the settings in use for the current power
// source) and `pmset -g batt`.
func parsePmset(settings, battery string) macPower {
	var p macPower
	inUse := false
	for _, line := range strings.Split(settings, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if strings.HasPrefix(line, "Currently in use") {
			inUse = true
			continue
		}
		switch {
		case fields[0] == "SleepDisabled" && len(fields) > 1:
			p.sleepDisabled = fields[1] == "1"
		case inUse && fields[0] == "sleep" && len(fields) > 1:
			p.sleepSetting = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "sleep"))
			p.idleSleepMinutes, _ = strconv.Atoi(fields[1])
		case inUse && (fields[0] == "lowpowermode" || fields[0] == "powermode") && len(fields) > 1:
			// powermode 1 is Low Power on the Macs that name it that way.
			p.lowPower = p.lowPower || fields[1] == "1"
		}
	}
	for _, line := range strings.Split(battery, "\n") {
		line = strings.TrimSpace(line)
		if rest, ok := strings.CutPrefix(line, "Now drawing from '"); ok {
			p.source = strings.TrimSuffix(rest, "'")
		} else if strings.HasPrefix(line, "-InternalBattery") {
			if _, status, ok := strings.Cut(line, "\t"); ok {
				p.battery = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(status), "present: true"))
			}
		}
	}
	return p
}

func (p macPower) text() string {
	parts := []string{p.source}
	if p.battery != "" {
		parts = append(parts, "battery "+p.battery)
	}
	switch {
	case p.sleepDisabled:
		parts = append(parts, "sleep disabled")
	case p.idleSleepMinutes > 0:
		idle := fmt.Sprintf("idle sleep after %d min", p.idleSleepMinutes)
		if _, note, ok := strings.Cut(p.sleepSetting, " "); ok {
			idle += " " + strings.TrimSpace(note)
		}
		parts = append(parts, idle)
	default:
		parts = append(parts, "idle sleep off")
	}
	if p.lowPower {
		parts = append(parts, "Low Power Mode on")
	}
	return strings.Join(parts, "; ")
}
