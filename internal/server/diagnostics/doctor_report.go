package diagnostics

import (
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/messages"
)

// DoctorReport is the management API report. Agent RPCs retain their own
// protocol; the Server normalizes both diagnostic sources into Checks.
type DoctorReport struct {
	proto.DoctorReport
	CheckedAt time.Time `json:"checked_at"`
	// Summary is the most serious status of the checks: fail, warn, skip or
	// pass (ADR 0067).
	Summary string        `json:"summary"`
	Checks  []DoctorCheck `json:"checks"`
}

// DoctorCheck is one stage of a node diagnosis: id (Stage), one of the five
// check statuses, and a code saying what it found or why it did not run.
type DoctorCheck struct {
	Stage      string               `json:"stage"`
	Status     string               `json:"status"`
	Code       string               `json:"code"`
	Vantage    string               `json:"vantage"`
	Target     string               `json:"target,omitempty"`
	Source     string               `json:"source,omitempty"`
	ObservedAt *time.Time           `json:"observed_at,omitempty"`
	Finding    *proto.DoctorFinding `json:"finding,omitempty"`
}

// Stages of the agent's self-check, and the agent checks each holds.
const (
	stageHost         = "host"
	stageRuntimes     = "runtimes"
	stageAccelerators = "accelerators"
	stageFinding      = "finding"
)

var doctorCheckStage = map[string]string{
	proto.DoctorCheckDiskFree:              stageHost,
	proto.DoctorCheckMounts:                stageHost,
	proto.DoctorCheckPathExec:              stageHost,
	proto.DoctorCheckPathWrite:             stageHost,
	proto.DoctorCheckPathPersist:           stageHost,
	proto.DoctorCheckMacOSVersion:          stageHost,
	proto.DoctorCheckMacOSSleep:            stageHost,
	proto.DoctorCheckUCXNetlink:            stageHost,
	proto.DoctorCheckRuntimeAvailable:      stageRuntimes,
	proto.DoctorCheckRuntimeProvision:      stageRuntimes,
	proto.DoctorCheckRuntimeStateAccess:    stageRuntimes,
	proto.DoctorCheckAcceleratorInventory:  stageAccelerators,
	proto.DoctorCheckAcceleratorRecognized: stageAccelerators,
	proto.DoctorCheckAcceleratorHealth:     stageAccelerators,
	proto.DoctorCheckCardRuntime:           stageAccelerators,
	proto.DoctorCheckCUDAInit:              stageAccelerators,
	proto.DoctorCheckPeerAccess:            stageAccelerators,
	proto.DoctorCheckNCCL:                  stageAccelerators,
	proto.DoctorCheckPeerMemory:            stageAccelerators,
}

// AgentDoctorReport folds an agent's report into the current vocabulary (an
// older agent's levels and lower-case codes, proto.NormalizeDoctorReport) and
// lays it out as checks: one per stage, then one per finding. A stage that was
// reported is as serious as the most serious of its findings, so it never
// reads as passed beside a finding that failed; a stage the agent did not
// report is skipped.
func AgentDoctorReport(agent proto.DoctorReport) DoctorReport {
	proto.NormalizeDoctorReport(&agent)
	out := DoctorReport{DoctorReport: agent, CheckedAt: time.Now().UTC()}
	worst := map[string]string{}
	for _, f := range agent.Findings {
		if stage := doctorCheckStage[f.ID]; stage != "" {
			worst[stage] = proto.WorseCheckStatus(worst[stage], f.Status)
		}
	}
	for _, group := range []struct {
		stage     string
		available bool
	}{
		{stageHost, agent.Host != nil},
		{stageRuntimes, len(agent.Runtimes) > 0},
		{stageAccelerators, agent.Accelerators != nil},
	} {
		check := DoctorCheck{Stage: group.stage, Status: proto.CheckSkip, Code: messages.DoctorNotReported, Vantage: "node", Source: "agent_rpc"}
		if group.available {
			check.Status, check.Code = proto.WorseCheckStatus(proto.CheckPass, worst[group.stage]), messages.DoctorInformationCollected
		}
		out.Checks = append(out.Checks, check)
	}
	for i := range agent.Findings {
		finding := &out.Findings[i]
		out.Checks = append(out.Checks, DoctorCheck{Stage: stageFinding, Status: finding.Status, Code: finding.Code, Vantage: "node", Source: "agent_rpc", Finding: finding})
	}
	out.Summary = doctorSummary(out.Checks)
	return out
}

func doctorSummary(checks []DoctorCheck) string {
	summary := proto.CheckPass
	for _, check := range checks {
		summary = proto.WorseCheckStatus(summary, check.Status)
	}
	return summary
}
