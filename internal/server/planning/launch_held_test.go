package planning

import (
	"testing"

	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/scheduler"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/server/yieldpolicy"
)

// held is launchHeld for a newcomer of policy needing gpus cards, asserted to
// be a launch neither launchYield nor launchReserved explains, as the
// planner calls it.
func (c *yieldCards) held(t *testing.T, policy string, gpus int) *LaunchHeld {
	t.Helper()
	req := scheduler.Request{WeightBytes: int64(gpus) * 4 << 30, HeadCount: 8, GPUMemUtil: 0.6, AllowedTPs: []int{gpus}}
	view := ledgerView{reserved: reservedAgainst(c.st, policy)}
	insts := nodeInstances(t, c.st, c.node)
	if scheduler.Schedule(req, nodeSnapshot(c.st, c.node, true, "", view)).OK {
		t.Fatal("the cards are free with the holders awake")
	}
	if y := launchYield(c.st, c.node, insts, 0, policy, req, true, "", view); y != nil {
		t.Fatalf("a %q launch waits for holders: %+v", policy, y)
	}
	if r := launchReserved(c.st, c.node, insts, req, true, "", view); r != nil {
		t.Fatalf("a %q launch is kept off sleepers' cards: %+v", policy, r)
	}
	return launchHeld(c.st, c.node, insts, 0, policy, req, true, "", view)
}

func (c *yieldCards) heldRefusal(t *testing.T, policy string, gpus int) scheduler.Reason {
	t.Helper()
	held := c.held(t, policy, gpus)
	if held == nil {
		t.Fatalf("a %q launch of %d cards is not held by yield policy", policy, gpus)
	}
	p, _ := yieldpolicy.Parse(policy, yieldpolicy.Overrides{})
	return HeldRefusal(c.node, p, held)
}

// A launch that never yields waits for no holder, so cards held awake by a
// deployment that would yield to one that shares refuse it by that holder's
// name and policy; a newcomer that shares would wait for it instead.
func TestANeverLaunchOnASharersCardsNamesTheSharer(t *testing.T) {
	c := newYieldCards(t, 1)
	asr := c.deployment(t, "qwen3-asr", yieldpolicy.Idle)
	c.instance(t, asr, "asr-1", "[0]", 0)
	reason := c.heldRefusal(t, yieldpolicy.Never, 1)
	if reason.Code != scheduler.ReasonDoesNotWaitForYield || jsonText(t, reason.Params) !=
		`{"blockers":[],"gpus":[0],"holders":[{"deployment":"qwen3-asr","policy":"idle"}],"node":"cards-node"}` {
		t.Fatalf("refusal = %s %s", reason.Code, jsonText(t, reason.Params))
	}
	// An empty policy reads as never.
	if got := c.heldRefusal(t, "", 1); jsonText(t, got) != jsonText(t, reason) {
		t.Fatalf("an unset policy = %s, want %s", jsonText(t, got), jsonText(t, reason))
	}
	// A never launch beside a never holder is plain cards in use: no
	// policy of its own would make it wait.
	t.Run("never holder", func(t *testing.T) {
		c := newYieldCards(t, 1)
		batch := c.deployment(t, "batch", yieldpolicy.Never)
		c.instance(t, batch, "batch-1", "[0]", 0)
		if got := c.held(t, yieldpolicy.Never, 1); got != nil {
			t.Fatalf("a never launch beside a never holder = %+v", got)
		}
	})
}

// A launch that shares cards beside a holder that never yields is refused by
// the holder's name: it does not yield to the launch.
func TestASharingLaunchOnANeverHoldersCardsNamesTheHolder(t *testing.T) {
	c := newYieldCards(t, 1)
	batch := c.deployment(t, "batch", yieldpolicy.Never)
	c.instance(t, batch, "batch-1", "[0]", 0)
	reason := c.heldRefusal(t, yieldpolicy.Idle, 1)
	if reason.Code != scheduler.ReasonHeldByNonYielding || jsonText(t, reason.Params) !=
		`{"blockers":[{"deployment":"batch","policy":"never"}],"gpus":[0],"holders":[],"node":"cards-node"}` {
		t.Fatalf("refusal = %s %s", reason.Code, jsonText(t, reason.Params))
	}
}

// A two-card launch over a sharing holder on one card and a never holder on
// the other names both: for a launch that shares, the sharer with the terms
// it gives way on and the never holder as what keeps it off; for one that
// never yields, the sharer as what it would wait for and the never holder
// beside it.
func TestATwoCardLaunchOverMixedHoldersNamesEach(t *testing.T) {
	c := newYieldCards(t, 2)
	shared := c.deployment(t, "shared", yieldpolicy.TimeSlice)
	batch := c.deployment(t, "batch", yieldpolicy.Never)
	c.instance(t, shared, "shared-1", "[0]", 0)
	c.instance(t, batch, "batch-1", "[1]", 0)

	reason := c.heldRefusal(t, yieldpolicy.TimeSlice, 2)
	if reason.Code != scheduler.ReasonHeldByNonYielding || jsonText(t, reason.Params) !=
		`{"blockers":[{"deployment":"batch","policy":"never"}],"gpus":[0,1],"holders":[{"at_once":false,"deployment":"shared","idle_sec":30,"max_serve_sec":120,"policy":"timeslice"}],"node":"cards-node"}` {
		t.Fatalf("sharing launch = %s %s", reason.Code, jsonText(t, reason.Params))
	}
	reason = c.heldRefusal(t, yieldpolicy.Never, 2)
	if reason.Code != scheduler.ReasonDoesNotWaitForYield || jsonText(t, reason.Params) !=
		`{"blockers":[{"deployment":"batch","policy":"never"}],"gpus":[0,1],"holders":[{"deployment":"shared","policy":"timeslice"}],"node":"cards-node"}` {
		t.Fatalf("never launch = %s %s", reason.Code, jsonText(t, reason.Params))
	}
	// One card: the sharer's card would do for a launch that shares, so it
	// waits for it (launchYield) and is no held refusal.
	req := scheduler.Request{WeightBytes: 4 << 30, HeadCount: 8, GPUMemUtil: 0.6, AllowedTPs: []int{1}}
	view := ledgerView{reserved: reservedAgainst(c.st, yieldpolicy.Idle)}
	if launchYield(c.st, c.node, nodeInstances(t, c.st, c.node), 0, yieldpolicy.Idle, req, true, "", view) == nil {
		t.Fatal("a one-card launch that shares does not wait for the sharer")
	}
}

// Held names only what keeps the launch off: a launch that would not fit with
// every holder asleep, a holder already ordered to sleep, and the launching
// deployment's own replica are not named.
func TestAHeldLaunchNamesOnlyTheHoldersInItsWay(t *testing.T) {
	c := newYieldCards(t, 1)
	batch := c.deployment(t, "batch", yieldpolicy.Never)
	c.instance(t, batch, "batch-1", "[0]", 0)
	// Too big for the card even when it is free.
	req := scheduler.Request{WeightBytes: 32 << 30, HeadCount: 8, GPUMemUtil: 0.6, AllowedTPs: []int{1}}
	if got := launchHeld(c.st, c.node, nodeInstances(t, c.st, c.node), 0, yieldpolicy.Idle, req, true, "", ledgerView{}); got != nil {
		t.Fatalf("a launch that never fits = %+v", got)
	}
	// The launching deployment's own replica is no holder.
	req = scheduler.Request{WeightBytes: 4 << 30, HeadCount: 8, GPUMemUtil: 0.6, AllowedTPs: []int{1}}
	if got := launchHeld(c.st, c.node, nodeInstances(t, c.st, c.node), batch.ID, yieldpolicy.Idle, req, true, "", ledgerView{}); got != nil {
		t.Fatalf("a launch held by its own replica = %+v", got)
	}
	// Nor is an instance an edit preview leaves out.
	if got := launchHeld(c.st, c.node, nodeInstances(t, c.st, c.node), 0, yieldpolicy.Idle, req, true, "", ledgerView{excludeDeployment: batch.ID}); got != nil {
		t.Fatalf("a launch held by the deployment being edited = %+v", got)
	}
}

// Only an evaluation that asks (ExplainHolders) names the holders: the
// controller's plan of the same launch keeps the scheduler's own refusal and
// does not pay for the re-plan.
func TestOnlyAnEvaluationThatAsksNamesTheHolders(t *testing.T) {
	c := newYieldCards(t, 1)
	asr := c.deployment(t, "qwen3-asr", yieldpolicy.Idle)
	c.instance(t, asr, "asr-1", "[0]", 0)
	input := Input{
		Model: &store.Model{ID: 1, Name: "m", WeightBytes: 4 << 30}, Node: c.node, Engine: "vllm",
		Quick: deploy.LaunchConfig{GPUMemUtil: 0.6}, YieldPolicy: yieldpolicy.Never,
	}
	controller := input.Evaluate(c.st)
	if controller.Fits() || controller.Held != nil {
		t.Fatalf("controller plan = fits %v held %+v, want a refusal without holders", controller.Fits(), controller.Held)
	}
	if got := controller.Refusal(c.node, yieldpolicy.Policy{Yield: yieldpolicy.Never}); got.Code == scheduler.ReasonDoesNotWaitForYield {
		t.Fatalf("controller refusal = %+v", got)
	}
	input.ExplainHolders = true
	preview := input.Evaluate(c.st)
	if preview.Held == nil {
		t.Fatal("an evaluation that asks names no holder")
	}
	if got := preview.Refusal(c.node, yieldpolicy.Policy{Yield: yieldpolicy.Never}); got.Code != scheduler.ReasonDoesNotWaitForYield {
		t.Fatalf("evaluation refusal = %+v", got)
	}
}
