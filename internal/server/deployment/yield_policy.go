package deployment

import (
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/yieldpolicy"
)

// YieldPolicyRefusal is the refusal of a policy other than never for a
// deployment that may hold none, nil when the policy is allowed: refusal is
// the deployment's planning.YieldPolicyRefusal code, engine its engine.
func YieldPolicyRefusal(p yieldpolicy.Policy, refusal, engine string) error {
	switch {
	case p.Yield == yieldpolicy.Never || refusal == "":
		return nil
	case refusal == messages.ErrorYieldPolicyShape:
		return productError(refusal, "only a single-node deployment shares its cards by yield policy", nil)
	default:
		return sleepRefusalError(&planning.SleepRefusal{Code: refusal, Params: map[string]any{"engine": engine}})
	}
}

// createYieldPolicy is the policy a new deployment is stored with: never when
// the request states none. It checks it as setYieldPolicy checks a change.
func createYieldPolicy(req dto.DeployRequest, engine string, pd bool) (yieldpolicy.Policy, error) {
	if req.YieldPolicy == nil {
		return yieldpolicy.Policy{Yield: yieldpolicy.Never}, nil
	}
	p, v := req.YieldPolicy.Parse()
	if v != nil {
		return p, YieldPolicyViolation(v)
	}
	return p, YieldPolicyRefusal(p, planning.DraftYieldPolicyRefusal(req.Sleepable, engine, req.NNodes, pd), engine)
}

// requestYield is the yield policy a request states, for the placement
// previews that must not refuse what the create would: never when it states
// none or a malformed one.
func requestYield(stated *dto.YieldPolicyRequest) string {
	if stated == nil {
		return yieldpolicy.Never
	}
	if p, v := stated.Parse(); v == nil {
		return p.Yield
	}
	return yieldpolicy.Never
}
