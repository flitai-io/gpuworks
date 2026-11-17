package deployment

import (
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/messages"
)

// PrioritySchedulingOf is whether a deployment of engine on vendor may run
// with per-request priority scheduling, judged on the template and user
// arguments the engine will receive. Evaluation reports it and create and
// edit enforce it, so the form's switch and the stored deployment agree.
func PrioritySchedulingOf(engine, vendor string, pd bool, templateArgs, userArgs []string) dto.PrioritySchedulingEligibility {
	refusal := deploy.PrioritySchedulingRefusal(engine, vendor, pd, templateArgs, userArgs)
	if refusal == nil {
		return dto.PrioritySchedulingEligibility{OK: true}
	}
	return dto.PrioritySchedulingEligibility{Refusal: &messages.Warning{Code: refusal.Code, Params: refusal.Params}, FixArgs: refusal.FixArgs}
}

// ResolvePriorityScheduling is the switch a deployment is stored with: as
// the request says, and on when it says nothing and the launch allows it.
// Asking for it where the launch does not allow it is refused with the
// reason, on the priority_scheduling field.
func ResolvePriorityScheduling(requested *bool, eligibility dto.PrioritySchedulingEligibility) (bool, error) {
	if requested == nil {
		return eligibility.OK, nil
	}
	if *requested && !eligibility.OK && eligibility.Refusal != nil {
		code := eligibility.Refusal.Code
		params := map[string]any{"field": "priority_scheduling"}
		for key, value := range eligibility.Refusal.Params {
			params[key] = value
		}
		return false, productError(code, messages.PrioritySchedulingRefusalFallback(code), params)
	}
	return *requested, nil
}

// requestsPriorityScheduling is the draft's own ask, for previews and
// preflight: the engine adapter still leaves it out where the launch does
// not allow it.
func requestsPriorityScheduling(req *dto.DeployRequest) bool {
	return req.PriorityScheduling == nil || *req.PriorityScheduling
}
