package diagnostics

import (
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
)

// DoctorReport is the management API report. Agent RPCs retain their own
// protocol; the Server normalizes both diagnostic sources into Checks.
type DoctorReport struct {
	proto.DoctorReport
	CheckedAt time.Time     `json:"checked_at"`
	Summary   string        `json:"summary"`
	Checks    []DoctorCheck `json:"checks"`
}

type DoctorCheck struct {
	Stage      string               `json:"stage"`
	Status     string               `json:"status"`
	Reason     string               `json:"reason"`
	Vantage    string               `json:"vantage"`
	Target     string               `json:"target,omitempty"`
	Source     string               `json:"source,omitempty"`
	ObservedAt *time.Time           `json:"observed_at,omitempty"`
	Finding    *proto.DoctorFinding `json:"finding,omitempty"`
}

// AgentDoctorReport preserves hardware details and legacy text. Inventory
// availability is information, not proof that every hardware check passed.
func AgentDoctorReport(agent proto.DoctorReport) DoctorReport {
	out := DoctorReport{DoctorReport: agent, CheckedAt: time.Now().UTC()}
	for _, group := range []struct {
		stage     string
		available bool
	}{
		{"host", agent.Host != nil},
		{"runtimes", len(agent.Runtimes) > 0},
		{"accelerators", agent.Accelerators != nil},
	} {
		check := DoctorCheck{Stage: group.stage, Status: "unavailable", Reason: "not_reported", Vantage: "node", Source: "agent_rpc"}
		if group.available {
			check.Status, check.Reason = "info", "information_collected"
		}
		out.Checks = append(out.Checks, check)
	}
	for i := range agent.Findings {
		finding := &agent.Findings[i]
		status := "unavailable"
		switch finding.Level {
		case proto.DoctorLevelError:
			status = "failed"
		case proto.DoctorLevelWarning:
			status = "warning"
		}
		out.Checks = append(out.Checks, DoctorCheck{Stage: "finding", Status: status, Reason: finding.Code, Vantage: "node", Source: "agent_rpc", Finding: finding})
	}
	out.Summary = doctorSummary(out.Checks)
	return out
}

func doctorSummary(checks []DoctorCheck) string {
	for _, status := range []string{"failed", "warning", "unavailable"} {
		for _, check := range checks {
			if check.Status == status {
				return status
			}
		}
	}
	return "completed"
}
