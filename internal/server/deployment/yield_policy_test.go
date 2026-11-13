package deployment

import (
	"encoding/json"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/yieldpolicy"
)

func TestANewDeploymentsYieldPolicyIsCheckedLikeAChange(t *testing.T) {
	policy := func(raw string) *dto.YieldPolicyRequest {
		var r dto.YieldPolicyRequest
		if err := json.Unmarshal([]byte(raw), &r); err != nil {
			t.Fatal(err)
		}
		return &r
	}
	for _, tc := range []struct {
		name   string
		req    dto.DeployRequest
		pd     bool
		ok     bool
		code   string
		stored string
	}{
		{name: "none stated", req: dto.DeployRequest{}, ok: true, stored: "never"},
		{name: "anytime", req: dto.DeployRequest{YieldPolicy: policy(`{"policy":"anytime"}`), Sleepable: true}, ok: true, stored: "anytime"},
		{name: "idle with parameters", req: dto.DeployRequest{YieldPolicy: policy(`{"policy":"idle","idle_sec":30}`), Sleepable: true}, ok: true, stored: "idle"},
		{name: "unknown policy", req: dto.DeployRequest{YieldPolicy: policy(`{"policy":"gold"}`), Sleepable: true}, code: "YIELD_POLICY_INVALID"},
		{name: "empty object", req: dto.DeployRequest{YieldPolicy: policy(`{}`), Sleepable: true}, code: "YIELD_POLICY_INVALID"},
		{name: "not sleepable", req: dto.DeployRequest{YieldPolicy: policy(`{"policy":"idle"}`)}, code: "SLEEP_NOT_ENABLED"},
		{name: "multi-node", req: dto.DeployRequest{YieldPolicy: policy(`{"policy":"timeslice"}`), Sleepable: true, NNodes: 2}, code: "YIELD_POLICY_SHAPE"},
		{name: "pd", req: dto.DeployRequest{YieldPolicy: policy(`{"policy":"timeslice"}`), Sleepable: true}, pd: true, code: "YIELD_POLICY_SHAPE"},
		{name: "never multi-node", req: dto.DeployRequest{YieldPolicy: policy(`{"policy":"never"}`), NNodes: 2}, ok: true, stored: "never"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, err := createYieldPolicy(tc.req, "vllm", tc.pd)
			if (err == nil) != tc.ok {
				t.Fatalf("err = %v, want ok=%v", err, tc.ok)
			}
			if err != nil {
				if !containsProductCode(err, tc.code) {
					t.Fatalf("err = %v, want %s", err, tc.code)
				}
				return
			}
			if p.Yield != tc.stored {
				t.Fatalf("stored as %q, want %q", p.Yield, tc.stored)
			}
			// What placement previews plan for is the stored policy's yield.
			if got, want := requestYield(tc.req.YieldPolicy), p.Yield; got != want {
				t.Fatalf("requestYield = %q, want %q", got, want)
			}
		})
	}
	if requestYield(policy(`{"policy":"gold"}`)) != yieldpolicy.Never {
		t.Fatal("a malformed policy plans as never")
	}
}
