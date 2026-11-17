package app

import (
	"fmt"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/proto"
)

// doctorProvision reports how the selected runtime's setup went, from what
// the driver remembers; it never starts the setup. A failed last attempt is a
// finding, because every deployment on the node fails the same way until the
// cause is fixed; the identity workloads run as, and why, is a fact.
func doctorProvision(r *proto.DoctorReport, driver runtime.Driver) {
	reporter, ok := driver.(runtime.ProvisionReporter)
	if !ok {
		return
	}
	state := reporter.ProvisionState()
	name := state.Runtime
	if name == "" {
		name = driver.Name()
	}
	switch {
	case state.Provisioned:
		r.Facts = append(r.Facts, proto.DoctorFact{Code: proto.DoctorFactRuntimeIdentity, Value: runIdentityText(state), Identity: &proto.DoctorRunIdentity{
			Runtime: name, User: state.RunUser, UID: state.RunUID, Fallback: state.FallbackReason,
		}})
	case state.LastError != "":
		evidence := []proto.DoctorField{{Name: "error", Value: state.LastError}}
		if !state.LastErrorAt.IsZero() {
			evidence = append(evidence, proto.DoctorField{Name: "failed_at", Value: state.LastErrorAt.UTC().Format(time.RFC3339)})
		}
		if !state.RetryAt.IsZero() {
			evidence = append(evidence, proto.DoctorField{Name: "retry_at", Value: state.RetryAt.UTC().Format(time.RFC3339)})
		}
		r.Findings = append(r.Findings, proto.DoctorFinding{
			ID: proto.DoctorCheckRuntimeProvision, Status: proto.CheckFail, Code: proto.DoctorRuntimeProvisionFailed,
			Probe: state.Probe, Evidence: evidence, Recheck: state.Recheck,
		})
	default:
		r.Facts = append(r.Facts, proto.DoctorFact{Code: proto.DoctorFactRuntimeIdentity, Value: name + " is set up on first use",
			Identity: &proto.DoctorRunIdentity{Runtime: name, Pending: true}})
	}
}

// runIdentityText states whom the workloads run as. A fallback names its
// reason, because the dedicated user is what the agent prefers.
func runIdentityText(state runtime.ProvisionState) string {
	if state.RunUser != "" {
		return fmt.Sprintf("dedicated user %s (uid %d)", state.RunUser, state.RunUID)
	}
	text := fmt.Sprintf("the agent's own user (uid %d)", state.RunUID)
	if state.FallbackReason != "" {
		text += ": the dedicated runtime user is not used because " + state.FallbackReason
	}
	return text
}
