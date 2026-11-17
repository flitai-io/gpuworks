package app

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/agent/gpu"
	"gpuworks.ai/gpuworks/internal/proto"
)

// macFixture answers the doctor's macOS commands with output recorded on a
// real Mac (testdata/macos/<mac>-*.txt). failing names commands that exit
// non-zero; edit rewrites a recorded output.
func macFixture(t *testing.T, mac string, failing map[string]bool, edit map[string]func(string) string) {
	t.Helper()
	files := map[string]string{
		"sysctl":        "sysctl",
		"pmset -g":      "pmset",
		"pmset -g batt": "pmset-batt",
		macFirewallTool: "firewall",
	}
	prev := doctorMacRun
	t.Cleanup(func() { doctorMacRun = prev })
	doctorMacRun = func(_ context.Context, name string, args ...string) (string, error) {
		key := name
		if name == "pmset" {
			key = strings.Join(append([]string{name}, args...), " ")
		}
		file, ok := files[key]
		if !ok {
			t.Fatalf("unexpected command %s %v", name, args)
		}
		if failing[key] {
			return "", errors.New("exit status 1")
		}
		raw, err := os.ReadFile("testdata/macos/" + mac + "-" + file + ".txt")
		if err != nil {
			t.Fatal(err)
		}
		out := string(raw)
		if f := edit[key]; f != nil {
			out = f(out)
		}
		return out, nil
	}
}

const gibBytes = int64(1) << 30

func appleGPU(workingSet int64) []proto.GPUInfo {
	return []proto.GPUInfo{{Index: 0, Name: "Apple M4 Max", Vendor: "apple", MemoryTotalBytes: workingSet}}
}

func factValue(r proto.DoctorReport, code string) (string, bool) {
	for _, f := range r.Facts {
		if f.Code == code {
			return f.Value, true
		}
	}
	return "", false
}

func TestDoctorMacDesktopOnACReportsFactsOnly(t *testing.T) {
	macFixture(t, "m4max", nil, nil)
	var r proto.DoctorReport
	doctorMacChecks(context.Background(), &r, appleGPU(96*gibBytes))
	if len(r.Findings) != 0 {
		t.Fatalf("a Mac Studio on AC with sleep 0 needs no action: %+v", r.Findings)
	}
	for code, want := range map[string]string{
		proto.DoctorFactMetalWorkingSet: "GPU may keep 96.0 GiB of 128.0 GiB memory resident (iogpu.wired_limit_mb=0, the macOS default)\nto raise it until the next reboot: sudo sysctl iogpu.wired_limit_mb=<MiB>",
		proto.DoctorFactPower:           "AC Power; idle sleep off",
		proto.DoctorFactMacOSFirewall:   "Firewall is enabled. (State = 1); Firewall stealth mode is on",
	} {
		if got, ok := factValue(r, code); !ok || got != want {
			t.Errorf("%s = %q (present %v), want %q", code, got, ok, want)
		}
	}
}

func TestDoctorMacLaptopThatSleepsWarns(t *testing.T) {
	macFixture(t, "m1max", nil, nil)
	var r proto.DoctorReport
	doctorMacChecks(context.Background(), &r, appleGPU(48*gibBytes))
	if len(r.Findings) != 1 {
		t.Fatalf("findings = %+v", r.Findings)
	}
	f := r.Findings[0]
	if f.Code != proto.DoctorMacOSIdleSleep || f.Status != proto.CheckWarn || f.Recheck != "pmset -g" {
		t.Fatalf("finding = %+v", f)
	}
	wantEvidence := []proto.DoctorField{{Name: "sleep", Value: "1 (sleep prevented by powerd, UURemote)"}, {Name: "power source", Value: "Battery Power"}}
	if len(f.Evidence) != 2 || f.Evidence[0] != wantEvidence[0] || f.Evidence[1] != wantEvidence[1] {
		t.Fatalf("evidence = %+v, want %+v", f.Evidence, wantEvidence)
	}
	if got, _ := factValue(r, proto.DoctorFactPower); got != "Battery Power; battery 63%; discharging; 3:55 remaining; idle sleep after 1 min (sleep prevented by powerd, UURemote)" {
		t.Fatalf("power = %q", got)
	}
	if got, _ := factValue(r, proto.DoctorFactMacOSFirewall); got != "Firewall is disabled. (State = 0); Firewall stealth mode is off" {
		t.Fatalf("firewall = %q", got)
	}
	if got, _ := factValue(r, proto.DoctorFactMetalWorkingSet); !strings.HasPrefix(got, "GPU may keep 48.0 GiB of 64.0 GiB memory resident") {
		t.Fatalf("working set = %q", got)
	}
}

func TestDoctorMacSleepDisabledAndLowPower(t *testing.T) {
	macFixture(t, "m1max", nil, map[string]func(string) string{
		"pmset -g": func(s string) string {
			s = strings.Replace(s, "SleepDisabled\t\t0", "SleepDisabled\t\t1", 1)
			return strings.Replace(s, "powermode            0", "powermode            1", 1)
		},
	})
	var r proto.DoctorReport
	doctorMacChecks(context.Background(), &r, appleGPU(48*gibBytes))
	if len(r.Findings) != 0 {
		t.Fatalf("sleep is disabled system-wide, nothing to act on: %+v", r.Findings)
	}
	if got, _ := factValue(r, proto.DoctorFactPower); !strings.HasSuffix(got, "; sleep disabled; Low Power Mode on") {
		t.Fatalf("power = %q", got)
	}
}

func TestDoctorMacOlderMacOSIsAnError(t *testing.T) {
	macFixture(t, "m1max", nil, map[string]func(string) string{
		"sysctl": func(s string) string { return strings.Replace(s, "15.4.1", "14.7.2", 1) },
	})
	var r proto.DoctorReport
	doctorMacChecks(context.Background(), &r, appleGPU(48*gibBytes))
	var found *proto.DoctorFinding
	for i := range r.Findings {
		if r.Findings[i].Code == proto.DoctorMacOSUnsupported {
			found = &r.Findings[i]
		}
	}
	if found == nil || found.Status != proto.CheckFail || found.Evidence[0].Value != "14.7.2" || found.Evidence[1].Value != "15" {
		t.Fatalf("findings = %+v", r.Findings)
	}
}

func TestDoctorMacFailingCommandsReportNothingInvented(t *testing.T) {
	macFixture(t, "m1max", map[string]bool{"sysctl": true, "pmset -g batt": true, macFirewallTool: true}, nil)
	var r proto.DoctorReport
	doctorMacChecks(context.Background(), &r, appleGPU(48*gibBytes))
	// Nothing is invented, and the two checks that could not read their
	// command say so rather than read as passed.
	if len(r.Facts) != 0 || len(r.Findings) != 2 {
		t.Fatalf("without the commands' answers there is nothing to state: findings %+v facts %+v", r.Findings, r.Facts)
	}
	for i, id := range []string{proto.DoctorCheckMacOSVersion, proto.DoctorCheckMacOSSleep} {
		if f := r.Findings[i]; f.ID != id || f.Status != proto.CheckSkip || f.Code != proto.DoctorSkipToolFailed || len(f.Evidence) != 1 {
			t.Fatalf("finding %d = %+v, want %s skipped", i, f, id)
		}
	}
	// No accelerator reported (the probe failed): the working set cannot be
	// stated even though sysctl answers.
	macFixture(t, "m4max", nil, nil)
	r = proto.DoctorReport{}
	doctorMacChecks(context.Background(), &r, nil)
	if _, ok := factValue(r, proto.DoctorFactMetalWorkingSet); ok {
		t.Fatalf("working set stated without a GPU: %+v", r.Facts)
	}
}

func TestDoctorReportRunsMacChecksOnlyOnMac(t *testing.T) {
	withDoctorRootDisk(t, 1000*gibBytes, 500*gibBytes)
	prober := &gpu.FixedProber{VendorName: "apple", GPUs: appleGPU(48 * gibBytes)}
	macFixture(t, "m1max", nil, nil)

	prev := doctorOnMac
	t.Cleanup(func() { doctorOnMac = prev })
	doctorOnMac = false
	if r := buildDoctorReport(context.Background(), prober, nil); hasCode(r, proto.DoctorMacOSIdleSleep) {
		t.Fatalf("macOS checks ran off a Mac: %+v", r.Findings)
	}
	doctorOnMac = true
	r := buildDoctorReport(context.Background(), prober, nil)
	if !hasCode(r, proto.DoctorMacOSIdleSleep) {
		t.Fatalf("findings = %+v", r.Findings)
	}
	for _, f := range r.Facts {
		if f.Code == proto.DoctorFactDeviceInjection || f.Code == proto.DoctorFactRuntimePlugin {
			t.Errorf("container device fact on a Mac: %+v", f)
		}
	}
	for _, rt := range r.Runtimes {
		if rt.Name == "dcgmi" || rt.Name == "docker" {
			t.Errorf("tool probe on a Mac: %+v", rt)
		}
	}
	for _, want := range []string{
		"[warning] MACOS_IDLE_SLEEP",
		"sudo pmset -c sleep 0",
		"metal working set GPU may keep 48.0 GiB of 64.0 GiB memory resident",
		"macos firewall    Firewall is disabled.",
	} {
		if !strings.Contains(r.Report, want) {
			t.Errorf("report lacks %q:\n%s", want, r.Report)
		}
	}
}

func hasCode(r proto.DoctorReport, code string) bool {
	for _, f := range r.Findings {
		if f.Code == code {
			return true
		}
	}
	return false
}
