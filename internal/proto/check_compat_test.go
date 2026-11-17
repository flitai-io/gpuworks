package proto

import (
	"encoding/json"
	"reflect"
	"testing"
)

// Every state and skip reason an older agent sends reads as one of the five
// statuses with an UPPER_SNAKE code; today's values pass through unchanged.
func TestNormalizeCardCheckFoldsOlderAgentWords(t *testing.T) {
	for _, tc := range []struct{ status, code, wantStatus, wantCode string }{
		{"pending", "", CheckSkip, CardCheckNotRunYet},
		{"skip", "busy", CheckSkip, CardCheckSkipBusy},
		{"skip", "foreign", CheckSkip, CardCheckSkipForeign},
		{"skip", "deferred", CheckSkip, CardCheckSkipDeferred},
		{"skip", "not_held", CheckSkip, CardCheckSkipNotHeld},
		{"skip", "unavailable", CheckSkip, CardCheckSkipUnavailable},
		{"skip", "probe_failed", CheckSkip, CardCheckSkipProbeFailed},
		{"warn", "pcie_link_degraded", CheckWarn, "PCIE_LINK_DEGRADED"},
		{"warn", "foreign_processes", CheckWarn, CardCheckForeignProcesses},
		{"warn", "peer_memory_missing", CheckWarn, DoctorPeerMemoryMissing},
		{"fail", "runtime_unusable", CheckFail, "RUNTIME_UNUSABLE"},
		{"pass", "", CheckPass, ""},
		{"na", "", CheckNA, ""},
		// Today's agent.
		{CheckSkip, CardCheckSkipBusy, CheckSkip, CardCheckSkipBusy},
		{CheckSkip, CardCheckNotRunYet, CheckSkip, CardCheckNotRunYet},
		// A status from a newer agent is never read as passed.
		{"degraded", "x", CheckSkip, "X"},
	} {
		status, code := NormalizeCardCheck(tc.status, tc.code)
		if status != tc.wantStatus || code != tc.wantCode {
			t.Errorf("NormalizeCardCheck(%q, %q) = %q, %q; want %q, %q", tc.status, tc.code, status, code, tc.wantStatus, tc.wantCode)
		}
	}
}

// A status report from an agent that predates check statuses, exactly as it
// sent it, reads in today's vocabulary.
func TestNormalizeGPUReportReadsAnOlderAgentsReport(t *testing.T) {
	const old = `{"gpus":[{"index":0,"uuid":"GPU-1","name":"","memory_total_bytes":0,"compute_capability":"","numa_node":0,"healthy":false,` +
		`"health_code":"memory_uncorrectable_errors",` +
		`"checks":[{"id":"vendor_health","s":"fail","r":"memory_uncorrectable_errors"},{"id":"runtime","s":"pending"},{"id":"nvlink","s":"skip","r":"busy","age":60}]}]}`
	var st NodeStatus
	if err := json.Unmarshal([]byte(old), &st); err != nil {
		t.Fatal(err)
	}
	NormalizeGPUReport(st.GPUs)
	g := st.GPUs[0]
	want := []CardCheck{
		{ID: "vendor_health", Status: CheckFail, Code: "MEMORY_UNCORRECTABLE_ERRORS"},
		{ID: "runtime", Status: CheckSkip, Code: CardCheckNotRunYet},
		{ID: "nvlink", Status: CheckSkip, Code: CardCheckSkipBusy, AgeS: 60},
	}
	if g.HealthCode != "MEMORY_UNCORRECTABLE_ERRORS" || !reflect.DeepEqual(g.Checks, want) {
		t.Fatalf("normalized = %q %+v", g.HealthCode, g.Checks)
	}
}

func TestNormalizeCardCheckDetailFoldsEveryRun(t *testing.T) {
	d := CardCheckDetail{
		Last:        &CardCheckRun{Status: "skip", Code: "busy"},
		LastRan:     &CardCheckRun{Status: "fail", Code: "runtime_unusable"},
		LastProblem: nil,
	}
	NormalizeCardCheckDetail(&d)
	if d.Last.Code != CardCheckSkipBusy || d.LastRan.Status != CheckFail || d.LastRan.Code != "RUNTIME_UNUSABLE" {
		t.Fatalf("detail = %+v %+v", d.Last, d.LastRan)
	}
}

// A doctor report from an agent that predates check statuses, exactly as it
// sent it: levels become statuses, codes UPPER_SNAKE, and the level key is
// gone when the report is encoded again.
func TestNormalizeDoctorReportReadsAnOlderAgentsReport(t *testing.T) {
	const old = `{"report":"text","accelerators":{"vendor":"nvidia","gpus":[{"index":0,"name":"A100","memory_bytes":1,"numa_node":0,"health_status":"Error","health_code":"runtime_unusable"}]},` +
		`"findings":[{"code":"disk_low","level":"warning"},{"code":"runtime_unusable","level":"error","targets":[{"index":0}]},{"code":"x","level":"severe"}]}`
	var r DoctorReport
	if err := json.Unmarshal([]byte(old), &r); err != nil {
		t.Fatal(err)
	}
	NormalizeDoctorReport(&r)
	got := []string{}
	for _, f := range r.Findings {
		got = append(got, f.Status+" "+f.Code+" "+f.Level)
	}
	// A level this server does not know is a failure: it is never read as
	// passed.
	if want := []string{"warn DISK_LOW ", "fail RUNTIME_UNUSABLE ", "fail X "}; !reflect.DeepEqual(got, want) {
		t.Fatalf("findings = %q, want %q", got, want)
	}
	if code := r.Accelerators.GPUs[0].HealthCode; code != "RUNTIME_UNUSABLE" {
		t.Fatalf("inventory code = %q", code)
	}
	out, err := json.Marshal(r.Findings[0])
	if err != nil || string(out) != `{"status":"warn","code":"DISK_LOW"}` {
		t.Fatalf("encoded = %s, %v", out, err)
	}
}

func TestWorseCheckStatusRanksSkipAbovePass(t *testing.T) {
	for _, tc := range []struct{ a, b, want string }{
		{"", CheckPass, CheckPass},
		{CheckPass, CheckSkip, CheckSkip},
		{CheckSkip, CheckPass, CheckSkip},
		{CheckSkip, CheckWarn, CheckWarn},
		{CheckFail, CheckWarn, CheckFail},
		{CheckNA, CheckPass, CheckPass},
		{CheckPass, CheckNA, CheckPass},
	} {
		if got := WorseCheckStatus(tc.a, tc.b); got != tc.want {
			t.Errorf("WorseCheckStatus(%q, %q) = %q, want %q", tc.a, tc.b, got, tc.want)
		}
	}
}
