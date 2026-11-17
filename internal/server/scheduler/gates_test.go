package scheduler

import (
	"reflect"
	"testing"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/server/nodepolicy"
)

// heldNode is the reporter's node with the placement policy given: cards
// held outside the platform, shareable by memory.
func heldNode(allow bool, gpus ...GPU) NodeSnapshot {
	node := nvidiaNode(gpus...)
	node.Policy = nodepolicy.Policy{ShareWithForeign: allow}
	return node
}

// automatic is the reporter's partition with the scheduler choosing cards.
func automatic(limitGiB int64) Request {
	req := reporterPartition(limitGiB)
	req.AllowedGPUIndexes = nil
	return req
}

// Switched off, the node keeps automatic placement off held cards, and the
// refusal names the setting that would allow it.
func TestHeldCardsAreWithheldWhileTheNodeDoesNotAllowSharing(t *testing.T) {
	res := Schedule(automatic(16), heldNode(false, reporterCard(0), reporterCard(1)))
	if res.OK || res.Message.Code != ReasonHeldOutsidePlatformNotAllowed {
		t.Fatalf("switched off = ok %v %+v, want %s", res.OK, res.Message, ReasonHeldOutsidePlatformNotAllowed)
	}
	params := res.Message.Params
	if params["setting"] != string(nodepolicy.KeyShareWithForeign) || params["count"] != 2 ||
		!reflect.DeepEqual(params["gpus"], []int{0, 1}) || params["node"] != "h20" {
		t.Fatalf("params = %+v", params)
	}
	if res := Schedule(automatic(16), heldNode(true, reporterCard(0), reporterCard(1))); !res.OK {
		t.Fatalf("switched on = %+v, want placed", res.Message)
	}
}

// A node with a clean card that is busy and three held ones is the common
// case: switched off, the refusal still points at the switch, because
// allowing it would place the deployment - not at the busy card.
func TestABusyCleanCardStillNamesTheSwitch(t *testing.T) {
	busy := freeH20(0)
	busy.AllocatedBytes, busy.WholeGPUAllocated = busy.MemoryTotal, true
	res := Schedule(automatic(16), heldNode(false, busy, reporterCard(1), reporterCard(2), reporterCard(3)))
	if res.OK || res.Message.Code != ReasonHeldOutsidePlatformNotAllowed {
		t.Fatalf("refusal = ok %v %+v, want %s", res.OK, res.Message, ReasonHeldOutsidePlatformNotAllowed)
	}
	if res.Message.Params["setting"] != string(nodepolicy.KeyShareWithForeign) || res.Message.Params["count"] != 3 {
		t.Fatalf("params = %+v", res.Message.Params)
	}
	// Allowing it would not help: the ordinary reason stands.
	huge := automatic(130)
	if res := Schedule(huge, heldNode(false, busy, reporterCard(1))); res.OK || res.Message.Code == ReasonHeldOutsidePlatformNotAllowed {
		t.Fatalf("a request too large for the held card = %+v, want the memory reason", res.Message)
	}
}

// Picking the cards is consent: switched off, a manual placement on a held
// card that fits by memory is admitted.
func TestChosenCardsConsentToSharing(t *testing.T) {
	req := reporterPartition(16)
	req.Consent = nodepolicy.Consent{CardsChosen: true}
	res := Schedule(req, heldNode(false, reporterCard(0), freeH20(1)))
	if !res.OK || res.Plan.GPUIndexes[0] != 0 {
		t.Fatalf("chosen held card = ok %v %+v plan %+v, want GPU 0", res.OK, res.Message, res.Plan)
	}
	// A pin without consent - a plan echoed back, a probe - is refused.
	unconsented := reporterPartition(16)
	if res := Schedule(unconsented, heldNode(false, reporterCard(0), freeH20(1))); res.OK || res.Message.Code != ReasonHeldOutsidePlatformNotAllowed {
		t.Fatalf("pinned without consent = ok %v %+v", res.OK, res.Message)
	}
}

// Consent does not stretch what may share a card at all: not whole-card
// work, not holders of unknown memory, not a deployment that sleeps (whose
// gate comes first and never yields).
func TestConsentDoesNotOverrideWhatCannotShare(t *testing.T) {
	whole := reporterPartition(16)
	whole.PerGPULimitBytes, whole.PartitionBackend = 0, ""
	whole.Consent = nodepolicy.Consent{CardsChosen: true}
	if res := Schedule(whole, heldNode(true, reporterCard(0))); res.OK || res.Message.Code != ReasonGPUsHeldOutsidePlatform {
		t.Fatalf("whole card = ok %v %+v", res.OK, res.Message)
	}
	unknown := reporterCard(0)
	unknown.ForeignShareable, unknown.ForeignBytes = false, 0
	chosen := reporterPartition(16)
	chosen.Consent = nodepolicy.Consent{CardsChosen: true}
	if res := Schedule(chosen, heldNode(true, unknown)); res.OK || res.Message.Code != ReasonGPUsHeldOutsidePlatform {
		t.Fatalf("unknown holder = ok %v %+v", res.OK, res.Message)
	}
	sleepy := reporterPartition(16)
	sleepy.Features = []accelerator.Feature{accelerator.FeatureEngineSleep}
	sleepy.Consent = nodepolicy.Consent{CardsChosen: true}
	for _, allow := range []bool{false, true} {
		if res := Schedule(sleepy, heldNode(allow, reporterCard(0))); res.OK || res.Message.Code != ReasonHeldOutsidePlatformSleep {
			t.Fatalf("sleepable, allow=%v = ok %v %+v, want %s", allow, res.OK, res.Message, ReasonHeldOutsidePlatformSleep)
		}
	}
}

// A card an operator allowed work on is not held for this rule: the switch
// does not take it away (ADR 0068).
func TestAnOverriddenHeldCardIsNotWithheld(t *testing.T) {
	card := reporterCard(0)
	card.Schedulable = true
	if res := Schedule(automatic(16), heldNode(false, card)); !res.OK {
		t.Fatalf("overridden card = %+v, want placed", res.Message)
	}
}

// The gates read the policy; they never change what the snapshot says the
// card is.
func TestGatesLeaveTheCardsFactsAlone(t *testing.T) {
	node := heldNode(false, reporterCard(0))
	resolved := resolveGates(automatic(16), node)
	if !resolved.GPUs[0].ForeignShareable || resolved.GPUs[0].withheld == 0 {
		t.Fatalf("resolved card = %+v, want shareable and withheld", resolved.GPUs[0])
	}
	if node.GPUs[0].withheld != 0 {
		t.Fatal("resolveGates changed the caller's snapshot")
	}
}

// A future gate is a table entry, and it may withhold a card that takes
// work: the mechanism does not assume held cards.
func TestAFurtherGateCanWithholdASchedulableCard(t *testing.T) {
	saved := cardGates
	t.Cleanup(func() { cardGates = saved })
	cardGates = append(append([]cardGate(nil), saved...), cardGate{
		name:    "test_odd_cards",
		applies: func(g GPU, _ Request) bool { return g.Index%2 == 1 },
		refusal: func(Request, string, []GPU) Reason { return Reason{Code: "test_odd_cards"} },
	})
	whole := Request{WeightBytes: giB, HeadCount: 32, GPUMemUtil: 0.9}
	res := Schedule(whole, NodeSnapshot{UUID: "n", DtypeSupported: true, DiskAvailBytes: 1 << 50, GPUs: []GPU{gpu(1, 48, true, 0), gpu(3, 48, true, 0)}})
	if res.OK || res.Message.Code != "test_odd_cards" {
		t.Fatalf("odd cards only = ok %v %+v", res.OK, res.Message)
	}
	res = Schedule(whole, NodeSnapshot{UUID: "n", DtypeSupported: true, DiskAvailBytes: 1 << 50, GPUs: []GPU{gpu(1, 48, true, 0), gpu(2, 48, true, 0)}})
	if !res.OK || res.Plan.GPUIndexes[0] != 2 {
		t.Fatalf("plan = %+v %+v, want GPU 2", res.Plan, res.Message)
	}
}
