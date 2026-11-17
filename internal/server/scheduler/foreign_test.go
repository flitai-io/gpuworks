package scheduler

import (
	"testing"

	"gpuworks.ai/gpuworks/internal/accelerator"
	serverestimate "gpuworks.ai/gpuworks/internal/server/estimate"
)

// The card from the report (flitai-io/gpuworks#20): an H20-3e of 143771 MiB
// that a GPUStack container outside the platform holds 63982 MiB of
// (nvidia-smi: pid 41235, python3, 67089334272 bytes).
const (
	mib                  = int64(1) << 20
	reporterCardTotal    = 143771 * mib
	reporterForeignBytes = int64(67089334272)
)

// reporterCard is that card as the planner snapshots it for an agent that
// keeps foreign processes reported beside its own instances: held outside
// the platform, so it takes no whole-card work, but shareable by memory.
func reporterCard(index int) GPU {
	return GPU{
		Index: index, MemoryTotal: reporterCardTotal, Schedulable: false,
		HeldOutsidePlatform: true, ForeignShareable: true, ForeignBytes: reporterForeignBytes,
		PartitionBackends: []string{accelerator.BackendNVIDIALogical},
	}
}

func freeH20(index int) GPU {
	return GPU{Index: index, MemoryTotal: reporterCardTotal, Schedulable: true,
		PartitionBackends: []string{accelerator.BackendNVIDIALogical}}
}

// reporterPartition is the deployment of the report: vllm on nvidia-dgpu
// with a 50 GiB memory limit, pinned to GPU 0 at TP 1.
func reporterPartition(limitGiB int64) Request {
	return Request{
		WeightBytes: 15 * giB, HeadCount: 32, GPUMemUtil: 0.9, ReserveBytes: 3 * giB,
		PerGPULimitBytes: limitGiB * giB, PartitionBackend: accelerator.BackendNVIDIALogical,
		AllowedGPUIndexes: []int{0}, UtilizationIsShare: true,
	}
}

func partitionClaim(req Request) int64 {
	return serverestimate.PartitionClaimBytes(req.PerGPULimitBytes, req.PartitionBackend, func() int64 { return reserveFor(req) })
}

// nvidiaNode allows partitions beside processes outside the platform: the
// tests of what may share a card by memory describe a node that allows it
// (gates_test.go covers one that does not).
func nvidiaNode(gpus ...GPU) NodeSnapshot {
	return NodeSnapshot{UUID: "h20", Vendor: accelerator.VendorNVIDIA, DtypeSupported: true, DiskAvailBytes: 1 << 50, GPUs: gpus,
		Policy: NodePolicy{ShareWithForeign: true}}
}

// A pin names the cards a request may use; the filters must judge those,
// not the node. A pinned card held outside the platform was reported as
// "no free GPU" because the node's other cards were counted, which sent the
// reader looking for a deployment to stop.
func TestAPinnedCardHeldOutsideThePlatformIsNamedNotCountedAway(t *testing.T) {
	held := reporterCard(0)
	held.ForeignShareable = false // memory not known: nothing may share it
	busy := freeH20(1)
	busy.AllocatedBytes, busy.WholeGPUAllocated = busy.MemoryTotal, true
	req := reporterPartition(50)
	res := Schedule(req, nvidiaNode(held, busy))
	if res.OK || res.Message.Code != ReasonGPUsHeldOutsidePlatform {
		t.Fatalf("refusal = %+v, want %s for the pinned card", res.Message, ReasonGPUsHeldOutsidePlatform)
	}
	whole := req
	whole.PerGPULimitBytes, whole.PartitionBackend = 0, ""
	if res := Schedule(whole, nvidiaNode(held, busy)); res.OK || res.Message.Code != ReasonGPUsHeldOutsidePlatform {
		t.Fatalf("whole-card refusal = %+v, want %s", res.Message, ReasonGPUsHeldOutsidePlatform)
	}
}

// The reported deployment fits in what the foreign process leaves, after
// the headroom kept beside it, and is placed on the pinned card.
func TestAPartitionIsAdmittedOnACardHeldOutsideThePlatformByRemainingMemory(t *testing.T) {
	req := reporterPartition(50)
	res := Schedule(req, nvidiaNode(reporterCard(0), freeH20(1)))
	if !res.OK || res.Plan == nil || len(res.Plan.GPUIndexes) != 1 || res.Plan.GPUIndexes[0] != 0 {
		t.Fatalf("schedule = ok %v %+v plan %+v, want GPU 0", res.OK, res.Message, res.Plan)
	}
	if res.Plan.PerGPUBytes != partitionClaim(req) {
		t.Fatalf("per-GPU bytes = %d, want the partition claim %d", res.Plan.PerGPUBytes, partitionClaim(req))
	}
	card := reporterCard(0)
	wantAvailable := reporterCardTotal - reporterForeignBytes - serverestimate.ForeignHeadroomBytes(reporterCardTotal)
	if card.Available() != wantAvailable {
		t.Fatalf("available = %d, want total - foreign - headroom = %d", card.Available(), wantAvailable)
	}
}

// Once the first partition runs there, the foreign process and the claim
// together leave too little for a second 50 GiB one; a smaller one still
// fits. The refusal names the foreign memory so the figures add up.
func TestASecondPartitionIsRefusedWhenForeignMemoryAndClaimsFillTheCard(t *testing.T) {
	first := reporterPartition(50)
	card := reporterCard(0)
	card.AllocatedBytes = partitionClaim(first)
	res := Schedule(reporterPartition(50), nvidiaNode(card))
	if res.OK || res.Message.Code != ReasonNoFit {
		t.Fatalf("second 50 GiB partition = ok %v %+v, want %s", res.OK, res.Message, ReasonNoFit)
	}
	if got := res.Message.Params["foreign_bytes"]; got != reporterForeignBytes {
		t.Fatalf("foreign_bytes = %v, want %d in %+v", got, reporterForeignBytes, res.Message.Params)
	}
	if res.Message.Params["foreign_gib"] != gib(reporterForeignBytes) {
		t.Fatalf("foreign_gib = %v", res.Message.Params["foreign_gib"])
	}
	small := reporterPartition(16)
	if res := Schedule(small, nvidiaNode(card)); !res.OK {
		t.Fatalf("a 16 GiB partition beside them = %+v, want it placed", res.Message)
	}
}

// A request for the whole card is still refused: the card is not free.
// Pinned, it is refused naming the holder; unpinned, it goes to a free card.
func TestAWholeCardRequestStillAvoidsACardHeldOutsideThePlatform(t *testing.T) {
	whole := reporterPartition(50)
	whole.PerGPULimitBytes, whole.PartitionBackend = 0, ""
	if res := Schedule(whole, nvidiaNode(reporterCard(0), freeH20(1))); res.OK || res.Message.Code != ReasonGPUsHeldOutsidePlatform {
		t.Fatalf("pinned whole-card = ok %v %+v, want %s", res.OK, res.Message, ReasonGPUsHeldOutsidePlatform)
	}
	whole.AllowedGPUIndexes = nil
	res := Schedule(whole, nvidiaNode(reporterCard(0), freeH20(1)))
	if !res.OK || res.Plan.GPUIndexes[0] != 1 {
		t.Fatalf("unpinned whole-card = ok %v %+v plan %+v, want the free GPU 1", res.OK, res.Message, res.Plan)
	}
}

// A deployment that sleeps would be refused its wake on that card for as
// long as the process holds it, so it is not placed there.
func TestASleepableDeploymentIsNotPlacedBesideAProcessOutsideThePlatform(t *testing.T) {
	req := reporterPartition(16)
	req.Features = []accelerator.Feature{accelerator.FeatureEngineSleep}
	res := Schedule(req, nvidiaNode(reporterCard(0), freeH20(1)))
	if res.OK || res.Message.Code != ReasonHeldOutsidePlatformSleep {
		t.Fatalf("sleepable partition = ok %v %+v, want %s", res.OK, res.Message, ReasonHeldOutsidePlatformSleep)
	}
	req.AllowedGPUIndexes = nil
	if res := Schedule(req, nvidiaNode(reporterCard(0), freeH20(1))); !res.OK || res.Plan.GPUIndexes[0] != 1 {
		t.Fatalf("unpinned sleepable partition = ok %v %+v plan %+v, want the free GPU 1", res.OK, res.Message, res.Plan)
	}
}

// What the foreign process holds is not known (the tool gave no figure, a
// Kubernetes holder, a list that cannot be read): nothing shares the card.
func TestUnknownForeignMemoryKeepsTheCardRefused(t *testing.T) {
	card := reporterCard(0)
	card.ForeignShareable, card.ForeignBytes = false, 0
	res := Schedule(reporterPartition(16), nvidiaNode(card))
	if res.OK || res.Message.Code != ReasonGPUsHeldOutsidePlatform {
		t.Fatalf("partition on an unknown holder = ok %v %+v, want %s", res.OK, res.Message, ReasonGPUsHeldOutsidePlatform)
	}
}

// The deploy form's per-card verdicts agree with the plan.
func TestGPUCandidatesOfferACardHeldOutsideThePlatformToPartitionsOnly(t *testing.T) {
	partition := reporterPartition(50)
	whole := partition
	whole.PerGPULimitBytes, whole.PartitionBackend = 0, ""
	sleepy := reporterPartition(16)
	sleepy.Features = []accelerator.Feature{accelerator.FeatureEngineSleep}
	unknown := reporterCard(0)
	unknown.ForeignShareable, unknown.ForeignBytes = false, 0
	filled := reporterCard(0)
	filled.AllocatedBytes = partitionClaim(partition)
	for _, tc := range []struct {
		name string
		card GPU
		req  Request
		want string
	}{
		{"partition fits", reporterCard(0), partition, ""},
		{"whole card", reporterCard(0), whole, ReasonGPUsHeldOutsidePlatform},
		{"sleepable partition", reporterCard(0), sleepy, ReasonHeldOutsidePlatformSleep},
		{"unknown foreign memory", unknown, partition, ReasonGPUsHeldOutsidePlatform},
		{"filled by foreign and claims", filled, partition, ReasonNoFit},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := GPUCandidates(tc.req, NodeSnapshot{GPUs: []GPU{tc.card}})
			code := ""
			if got[0].Reason != nil {
				code = got[0].Reason.Code
			}
			if got[0].Available != (tc.want == "") || code != tc.want {
				t.Fatalf("candidate = %+v (reason %q), want %q", got[0], code, tc.want)
			}
		})
	}
}

// An operator who allowed work on a held card keeps that override: the card
// is schedulable, and a partition is still charged what the process holds.
func TestAnOverriddenHeldCardStillCountsTheForeignMemoryForPartitions(t *testing.T) {
	card := reporterCard(0)
	card.Schedulable = true
	big := reporterPartition(80)
	if res := Schedule(big, nvidiaNode(card)); res.OK {
		t.Fatalf("an 80 GiB partition beside 62.5 GiB held outside = %+v, want refused", res.Plan)
	}
}

// A shareable held card whose foreign memory and claims leave no room is
// refused with the figures that explain it (no_fit with the foreign memory),
// not "no free GPU", which sends the reader looking for a deployment to stop.
func TestAHeldCardWithNoRoomLeftNamesTheForeignMemory(t *testing.T) {
	card := GPU{Index: 0, MemoryTotal: 80 * giB, HeldOutsidePlatform: true, ForeignShareable: true,
		ForeignBytes: 74 * giB, AllocatedBytes: 2 * giB, PartitionBackends: []string{accelerator.BackendNVIDIALogical}}
	res := Schedule(reporterPartition(16), nvidiaNode(card))
	if res.OK || res.Message.Code != ReasonNoFit || res.Message.Params["foreign_bytes"] != 74*giB {
		t.Fatalf("refusal = ok %v %+v, want %s with foreign_bytes", res.OK, res.Message, ReasonNoFit)
	}
}

// An operator who allowed work on a held card keeps that override for
// whole-card work: whole-card capacity is the engine's share of the card,
// which never subtracted the foreign memory, so the free-card gate must not
// either - and a whole-card refusal does not name foreign figures it never
// applied.
func TestAnOverriddenHeldCardStillTakesWholeCardWork(t *testing.T) {
	card := GPU{Index: 0, MemoryTotal: 80 * giB, Schedulable: true, HeldOutsidePlatform: true,
		ForeignBytes: 77 * giB, PartitionBackends: []string{accelerator.BackendNVIDIALogical}}
	whole := reporterPartition(50)
	whole.PerGPULimitBytes, whole.PartitionBackend = 0, ""
	if res := Schedule(whole, nvidiaNode(card)); !res.OK {
		t.Fatalf("whole-card on an overridden held card = %+v, want placed", res.Message)
	}
	if got := GPUCandidates(whole, NodeSnapshot{GPUs: []GPU{card}}); !got[0].Available {
		t.Fatalf("candidate = %+v, want offered", got[0])
	}
	huge := whole
	huge.WeightBytes = 100 * giB
	res := Schedule(huge, nvidiaNode(card))
	if res.OK || res.Message.Code != ReasonNoFit {
		t.Fatalf("oversized whole-card = ok %v %+v, want %s", res.OK, res.Message, ReasonNoFit)
	}
	if _, named := res.Message.Params["foreign_bytes"]; named {
		t.Fatalf("whole-card no_fit names foreign memory it never subtracted: %+v", res.Message.Params)
	}
}
