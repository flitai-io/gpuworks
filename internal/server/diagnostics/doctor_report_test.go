package diagnostics

import (
	"encoding/json"
	"reflect"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
)

func TestUnifiedDoctorPreservesDetailsAndFindingsWithoutClaimingChecksPassed(t *testing.T) {
	agent := proto.DoctorReport{
		Report:       "original text",
		Host:         &proto.DoctorHost{Hostname: "node"},
		Runtimes:     []proto.DoctorRuntime{{Name: "docker", Status: proto.DoctorRuntimeQueryFailed, Selected: true}},
		Accelerators: &proto.DoctorAccelerators{Vendor: "nvidia", GPUs: []proto.DoctorGPU{{Index: 0, HealthStatus: "Error"}}},
		Findings: []proto.DoctorFinding{
			{Code: proto.DoctorRuntimeUnavailable, Level: proto.DoctorLevelError, Probe: "docker info", Evidence: []proto.DoctorField{{Name: "error", Value: "unavailable"}}, Recheck: "docker info"},
			{Code: proto.DoctorMultiGPUCheckFailed, Level: proto.DoctorLevelWarning, Targets: []proto.DoctorTarget{{Index: 0, Actions: []string{"reset"}, Processes: []proto.DoctorProcess{{PID: 123, Name: "worker"}}}}},
		},
		Facts: []proto.DoctorFact{{Code: "future_fact", Value: "raw evidence"}},
	}
	got := AgentDoctorReport(agent)
	if got.Summary != "failed" || len(got.Checks) != 5 || got.Report != agent.Report || !reflect.DeepEqual(got.Facts, agent.Facts) || !reflect.DeepEqual(got.Host, agent.Host) || !reflect.DeepEqual(got.Runtimes, agent.Runtimes) || !reflect.DeepEqual(got.Accelerators, agent.Accelerators) {
		t.Fatalf("lost report details: %+v", got)
	}
	for _, c := range got.Checks[:3] {
		if c.Status != "info" || c.ObservedAt != nil {
			t.Fatalf("inventory invented a health verdict or measurement time: %+v", c)
		}
	}
	for i, finding := range agent.Findings {
		if !reflect.DeepEqual(got.Checks[i+3].Finding, &finding) {
			t.Fatalf("lost finding evidence: %+v", got.Checks[i+3])
		}
	}
	if got.Checks[4].Status != "warning" || !reflect.DeepEqual(got.Findings, agent.Findings) || len(agent.Findings) != 2 {
		t.Fatal("legacy findings changed or input modified")
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

func TestUnifiedDoctorLegacyAndUnknownEvidenceRemainUnavailable(t *testing.T) {
	for _, agent := range []proto.DoctorReport{{Report: "legacy"}, {Report: "future", Findings: []proto.DoctorFinding{{Code: "future_check", Level: "future_level"}}}} {
		got := AgentDoctorReport(agent)
		if got.Summary != "unavailable" || got.Report != agent.Report {
			t.Fatalf("%+v", got)
		}
		for _, c := range got.Checks {
			if c.Status != "unavailable" {
				t.Fatalf("missing evidence became success: %+v", c)
			}
		}
	}
}
