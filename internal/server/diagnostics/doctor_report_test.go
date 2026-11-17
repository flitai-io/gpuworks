package diagnostics

import (
	"encoding/json"
	"reflect"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/messages"
)

func TestUnifiedDoctorPreservesDetailsAndFindingsWithoutClaimingChecksPassed(t *testing.T) {
	agent := proto.DoctorReport{
		Report:       "original text",
		Host:         &proto.DoctorHost{Hostname: "node"},
		Runtimes:     []proto.DoctorRuntime{{Name: "docker", Status: proto.DoctorRuntimeQueryFailed, Selected: true}},
		Accelerators: &proto.DoctorAccelerators{Vendor: "nvidia", GPUs: []proto.DoctorGPU{{Index: 0, HealthStatus: "Error"}}},
		Findings: []proto.DoctorFinding{
			{ID: proto.DoctorCheckRuntimeAvailable, Status: proto.CheckFail, Code: proto.DoctorRuntimeUnavailable, Probe: "docker info", Evidence: []proto.DoctorField{{Name: "error", Value: "unavailable"}}, Recheck: "docker info"},
			{ID: proto.DoctorCheckNCCL, Status: proto.CheckWarn, Code: proto.DoctorMultiGPUCheckFailed, Targets: []proto.DoctorTarget{{Index: 0, Actions: []string{"reset"}, Processes: []proto.DoctorProcess{{PID: 123, Name: "worker"}}}}},
		},
		Facts: []proto.DoctorFact{{Code: "future_fact", Value: "raw evidence"}},
	}
	got := AgentDoctorReport(agent)
	if got.Summary != proto.CheckFail || len(got.Checks) != 5 || got.Report != agent.Report || !reflect.DeepEqual(got.Facts, agent.Facts) || !reflect.DeepEqual(got.Host, agent.Host) || !reflect.DeepEqual(got.Runtimes, agent.Runtimes) || !reflect.DeepEqual(got.Accelerators, agent.Accelerators) {
		t.Fatalf("lost report details: %+v", got)
	}
	// A stage is as serious as its worst finding: the host stage has none
	// and passed; the runtime stage holds a failure and the accelerator
	// stage a warning, so neither reads as passed.
	for i, want := range []string{proto.CheckPass, proto.CheckFail, proto.CheckWarn} {
		if c := got.Checks[i]; c.Status != want || c.Code != messages.DoctorInformationCollected || c.ObservedAt != nil {
			t.Fatalf("stage %s = %+v, want %s", c.Stage, c, want)
		}
	}
	for i, finding := range agent.Findings {
		if !reflect.DeepEqual(got.Checks[i+3].Finding, &finding) || got.Checks[i+3].Code != finding.Code {
			t.Fatalf("lost finding evidence: %+v", got.Checks[i+3])
		}
	}
	if got.Checks[4].Status != proto.CheckWarn || !reflect.DeepEqual(got.Findings, agent.Findings) || len(agent.Findings) != 2 {
		t.Fatal("findings changed or input modified")
	}
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]json.RawMessage
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	if wire["access"] != nil || wire["checks"] == nil || wire["findings"] == nil || wire["host"] == nil {
		t.Fatalf("wire=%s", raw)
	}
	var legacyConsumer proto.DoctorReport
	if err := json.Unmarshal(raw, &legacyConsumer); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(legacyConsumer, agent) {
		t.Fatalf("legacy consumer lost Agent evidence: %+v", legacyConsumer)
	}
	var roundtrip DoctorReport
	if err := json.Unmarshal(raw, &roundtrip); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(roundtrip.Checks, got.Checks) {
		t.Fatalf("checks changed in transit: %+v", roundtrip.Checks)
	}
}

// A report the agent gave no structure (an agent that predates it), and one
// whose finding status this server does not know, never read as passed.
func TestUnifiedDoctorLegacyAndUnknownEvidenceNeverPass(t *testing.T) {
	got := AgentDoctorReport(proto.DoctorReport{Report: "legacy"})
	if got.Summary != proto.CheckSkip || got.Report != "legacy" {
		t.Fatalf("%+v", got)
	}
	for _, c := range got.Checks {
		if c.Status != proto.CheckSkip || c.Code != messages.DoctorNotReported {
			t.Fatalf("missing evidence became success: %+v", c)
		}
	}
	future := AgentDoctorReport(proto.DoctorReport{Report: "future", Findings: []proto.DoctorFinding{{ID: "future_check", Status: "degraded", Code: "FUTURE"}}})
	if future.Summary != proto.CheckFail || future.Checks[3].Status != proto.CheckFail {
		t.Fatalf("unknown status = %+v", future.Checks)
	}
}

// An agent that predates check statuses sends levels and lower-case codes,
// exactly as below; the report reads in the current words, with no level.
func TestUnifiedDoctorReadsAnOlderAgentsFindings(t *testing.T) {
	const old = `{"report":"text","host":{"hostname":"n","os":"linux","arch":"amd64","kernel":"","cpu_model":"","cpu_cores":1,"mem_total_bytes":1,"mem_avail_bytes":1,"disks":[]},` +
		`"findings":[{"code":"runtime_unavailable","level":"error"},{"code":"disk_low","level":"warning"}]}`
	var agent proto.DoctorReport
	if err := json.Unmarshal([]byte(old), &agent); err != nil {
		t.Fatal(err)
	}
	got := AgentDoctorReport(agent)
	if got.Summary != proto.CheckFail {
		t.Fatalf("summary = %q", got.Summary)
	}
	raw, err := json.Marshal(got.Checks[3:])
	if err != nil {
		t.Fatal(err)
	}
	want := `[{"stage":"finding","status":"fail","code":"RUNTIME_UNAVAILABLE","vantage":"node","source":"agent_rpc","finding":{"status":"fail","code":"RUNTIME_UNAVAILABLE"}},` +
		`{"stage":"finding","status":"warn","code":"DISK_LOW","vantage":"node","source":"agent_rpc","finding":{"status":"warn","code":"DISK_LOW"}}]`
	if string(raw) != want {
		t.Fatalf("checks =\n%s\nwant\n%s", raw, want)
	}
	// An older agent's findings carry no check id, so they hold no stage
	// back: the host stage it reported reads as collected.
	if c := got.Checks[0]; c.Status != proto.CheckPass {
		t.Fatalf("host = %+v", c)
	}
}
