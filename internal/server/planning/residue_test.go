package planning

import (
	"fmt"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/lifecycle"
	"gpuworks.ai/gpuworks/internal/server/sleep"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// residueFixture is one 48 GiB card on one node and a sleepable deployment of
// a model of the given size and dtype, its one instance on the card.
func residueFixture(t *testing.T, weightBytes int64, dtype string) (*store.Store, *store.Node, *store.Deployment) {
	t.Helper()
	st, err := store.Open("file:planning_residue_"+strings.ReplaceAll(t.Name(), "/", "_")+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	node, err := st.RegisterNode("residue-node", store.HashToken("token"), "residue-node")
	if err != nil {
		t.Fatal(err)
	}
	node.GPUs = []store.GPU{{NodeID: node.ID, Index: 0, UUID: "gpu-0", Vendor: "nvidia", ComputeCapability: "8.0", MemoryTotal: 48 << 30}}
	if err := st.DB().Create(&node.GPUs[0]).Error; err != nil {
		t.Fatal(err)
	}
	model, err := st.UpsertModel(&store.Model{Name: "m", Source: "huggingface", SourceID: "org/" + strings.ReplaceAll(t.Name(), "/", "_"), WeightBytes: weightBytes, Dtype: dtype})
	if err != nil {
		t.Fatal(err)
	}
	dep, err := st.CreateDeployment(&store.Deployment{Name: "residue", ModelID: model.ID, NodeID: node.ID, ServedName: "residue", Engine: "vllm", Sleepable: true, State: "active"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateInstance(&store.Instance{UUID: "residue-1", DeploymentID: dep.ID, NodeID: node.ID, State: proto.InstRunning, GPUIndexes: "[0]", NNodes: 1, DesiredStateJSON: sleepSpec("residue-1", 0)}); err != nil {
		t.Fatal(err)
	}
	return st, node, dep
}

func sleepReported(t *testing.T, st *store.Store, dep *store.Deployment, residue int64) {
	t.Helper()
	if err := st.SetDeploymentSleep(dep.ID, 2); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateInstanceStatus(proto.InstanceStatus{UUID: "residue-1", State: proto.InstRunning, SleepLevel: 2, SleepResidueBytes: residue}); err != nil {
		t.Fatal(err)
	}
}

// A sleeper the agent could not measure is charged the estimate for its
// model's size: an 8B bf16 model on one GPU is 1 GiB plus the CUDA graph pool.
func TestASleeperIsChargedTheEstimateForItsModel(t *testing.T) {
	st, node, dep := residueFixture(t, 16<<30, "bfloat16")
	sleepReported(t, st, dep, 0)
	if got, want := GPUAllocations(st, node)[0].SleepingBytes, sleep.EstimateResidueBytes(8<<30, 1); got != want || want != 3<<29 {
		t.Fatalf("sleeping bytes = %d, want %d (1.5 GiB)", got, want)
	}
}

// What the agent measured replaces the estimate, and a later report without a
// measurement (an adopted container after an agent restart) keeps it.
func TestASleepersMeasuredResidueReplacesTheEstimate(t *testing.T) {
	st, node, dep := residueFixture(t, 16<<30, "bfloat16")
	sleepReported(t, st, dep, 700<<20)
	if got := GPUAllocations(st, node)[0].SleepingBytes; got != 700<<20 {
		t.Fatalf("sleeping bytes = %d, want the measured 700 MiB", got)
	}
	if err := st.UpdateInstanceStatus(proto.InstanceStatus{UUID: "residue-1", State: proto.InstRunning, SleepLevel: 2}); err != nil {
		t.Fatal(err)
	}
	if got := GPUAllocations(st, node)[0].SleepingBytes; got != 700<<20 {
		t.Fatalf("sleeping bytes after a report without a measurement = %d, want 700 MiB kept", got)
	}
}

// A wake hands back exactly the residue the ledger charged the sleeper, so a
// lone sleeper always fits its own card back, measured or estimated.
func TestAWakeHandsBackTheResidueTheLedgerCharged(t *testing.T) {
	for _, measured := range []int64{0, 900 << 20} {
		t.Run(fmt.Sprintf("measured-%d", measured), func(t *testing.T) {
			st, _, dep := residueFixture(t, 16<<30, "bfloat16")
			sleepReported(t, st, dep, measured)
			inst, err := st.InstanceByUUID("residue-1")
			if err != nil {
				t.Fatal(err)
			}
			woken, refused, err := AdmitWake(st, [][]store.Instance{{*inst}}, false)
			if err != nil || len(refused) != 0 || len(woken) != 1 {
				t.Fatalf("lone sleeper wake = woken %d refused %+v err %v", len(woken), refused, err)
			}
		})
	}
}

// A card a process outside the platform holds refuses the wake even though
// the ledger shows room: the platform cannot see how much that process uses.
func TestAWakeIsRefusedOnACardHeldOutsideThePlatform(t *testing.T) {
	st, node, dep := residueFixture(t, 16<<30, "bfloat16")
	sleepReported(t, st, dep, 0)
	if err := st.DB().Model(&store.GPU{}).Where("node_id = ? AND `index` = 0", node.ID).Update("foreign_processes", `[{"pid":4242,"name":"python3"}]`).Error; err != nil {
		t.Fatal(err)
	}
	inst, err := st.InstanceByUUID("residue-1")
	if err != nil {
		t.Fatal(err)
	}
	_, refused, err := AdmitWake(st, [][]store.Instance{{*inst}}, false)
	if err != nil || len(refused) != 1 || len(refused[0].Verdict.Shortfalls) != 1 || !refused[0].Verdict.Shortfalls[0].HeldOutside {
		t.Fatalf("wake onto a card held outside = %+v err %v", refused, err)
	}
}

func TestModelParamsReadsTheStoredPrecision(t *testing.T) {
	for _, tc := range []struct {
		quant, dtype string
		weightBytes  int64
		want         float64
	}{
		{dtype: "bfloat16", weightBytes: 16e9, want: 8e9},
		{quant: "fp8", dtype: "bfloat16", weightBytes: 8e9, want: 8e9},
		{quant: "nvfp4", weightBytes: 250e9, want: 250e9 / 0.56},
		{quant: "GSQ-RCO IQ2_XS", weightBytes: 68e9, want: 68e9 / 0.32},
		{weightBytes: 2e9, want: 1e9}, // unknown format reads as 16-bit
		{weightBytes: 0, want: 0},
	} {
		if got := modelParams(&store.Model{QuantLabel: tc.quant, Dtype: tc.dtype, WeightBytes: tc.weightBytes}); got != tc.want {
			t.Fatalf("modelParams(%q/%q, %d) = %g, want %g", tc.quant, tc.dtype, tc.weightBytes, got, tc.want)
		}
	}
}

// An L1 sleep moves the weights into host memory: it is refused when the host
// cannot hold them with its headroom, allowed when it can, and not judged when
// the host's memory was not observed recently.
func TestL1SleepIsRefusedWhenTheHostCannotHoldTheWeights(t *testing.T) {
	st, node, dep := residueFixture(t, 40<<30, "bfloat16")
	insts, err := st.InstancesForDeployment(dep.ID)
	if err != nil {
		t.Fatal(err)
	}
	host := func(avail, total int64) HostMemory {
		return func(uuid string) (int64, int64, bool) {
			if uuid != node.UUID {
				t.Fatalf("host memory read for %q", uuid)
			}
			return avail, total, true
		}
	}
	refusal := L1HostMemoryRefusal(st, dep, insts, host(40<<30, 64<<30))
	if refusal == nil || refusal.Code != sleep.CodeHostMemoryInsufficient || refusal.Params["need_gib"] != "40.0" || refusal.Params["available_gib"] != "40.0" {
		t.Fatalf("40 GiB of weights onto 40 GiB free = %+v", refusal)
	}
	if refusal := L1HostMemoryRefusal(st, dep, insts, host(100<<30, 256<<30)); refusal != nil {
		t.Fatalf("40 GiB of weights onto 100 GiB free refused: %+v", refusal)
	}
	unknown := func(string) (int64, int64, bool) { return 0, 0, false }
	if refusal := L1HostMemoryRefusal(st, dep, insts, unknown); refusal != nil {
		t.Fatalf("unknown host memory refused: %+v", refusal)
	}
	// An instance already asleep moved its weights already: it needs nothing more.
	sleepReported(t, st, dep, 0)
	insts, _ = st.InstancesForDeployment(dep.ID)
	if refusal := L1HostMemoryRefusal(st, dep, insts, host(1<<30, 64<<30)); refusal != nil {
		t.Fatalf("an already sleeping instance was charged again: %+v", refusal)
	}
}

// A failed sleep transition holds its whole card until the agent confirms
// the engine gone; that confirmation (a stopped report) ends its sleep phase
// while the failed row stays as the unit's spent attempt, and the card is
// free again. Before, the report was dropped and the card stayed held for
// good (seen on gpu3).
func TestAFailedSleepReleasesItsCardOnceTheEngineIsGone(t *testing.T) {
	st, node, dep := residueFixture(t, 16<<30, "bfloat16")
	sleepReported(t, st, dep, 0)
	if err := st.UpdateInstanceStatus(proto.InstanceStatus{UUID: "residue-1", State: proto.InstFailed, Phase: proto.PhaseSleep, Message: "sleep level uncertain"}); err != nil {
		t.Fatal(err)
	}
	if got := GPUAllocations(st, node)[0]; got.AllocatedBytes != 48<<30 {
		t.Fatalf("an uncertain failed sleep = %+v, want the whole card held", got)
	}
	if err := st.UpdateInstanceStatus(proto.InstanceStatus{UUID: "residue-1", State: proto.InstStopped}); err != nil {
		t.Fatal(err)
	}
	inst, err := st.InstanceByUUID("residue-1")
	if err != nil || inst.State != proto.InstFailed || inst.Phase != "" {
		t.Fatalf("row after the stop = %+v err=%v, want failed with its sleep phase ended", inst, err)
	}
	if got := GPUAllocations(st, node)[0]; got.AllocatedBytes != 0 || got.SleepingBytes != 0 {
		t.Fatalf("card after the engine is gone = %+v, want free", got)
	}
}

// Two wakes given at once - an operator's and the automation's - for two
// sleepers on one card that only one of them fits: under the server's order
// lock (lifecycle.Orders) exactly one is admitted and the other is refused,
// never both.
func TestConcurrentWakesUnderTheLifecycleLockAdmitOnlyOne(t *testing.T) {
	st, node, dep := residueFixture(t, 16<<30, "bfloat16")
	// A second sleepable deployment on the same card.
	other, err := st.CreateDeployment(&store.Deployment{Name: "other", ModelID: dep.ModelID, NodeID: node.ID, ServedName: "other", Engine: "vllm", Sleepable: true, State: store.DeployActive})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateInstance(&store.Instance{UUID: "other-1", DeploymentID: other.ID, NodeID: node.ID, State: proto.InstRunning, GPUIndexes: "[0]", NNodes: 1, DesiredStateJSON: sleepSpec("other-1", 0)}); err != nil {
		t.Fatal(err)
	}
	for _, d := range []*store.Deployment{dep, other} {
		if err := st.SetDeploymentSleep(d.ID, 2); err != nil {
			t.Fatal(err)
		}
	}
	for _, uuid := range []string{"residue-1", "other-1"} {
		if err := st.UpdateInstanceStatus(proto.InstanceStatus{UUID: uuid, State: proto.InstRunning, SleepLevel: 2}); err != nil {
			t.Fatal(err)
		}
	}
	var orders lifecycle.Orders
	wake := func(d *store.Deployment) (int, error) {
		defer orders.Lock()()
		insts, err := st.InstancesForDeployment(d.ID)
		if err != nil {
			return 0, err
		}
		match, err := DeploymentMatch(st, d, insts)
		if err != nil {
			return 0, err
		}
		woken, _, err := OrderWake(st, d, match, Sleepers(insts), false, true)
		return len(woken), err
	}
	results := make(chan int, 2)
	for _, d := range []*store.Deployment{dep, other} {
		go func(d *store.Deployment) {
			n, err := wake(d)
			if err != nil {
				t.Error(err)
			}
			results <- n
		}(d)
	}
	if total := <-results + <-results; total != 1 {
		t.Fatalf("woken across both orders = %d, want exactly 1", total)
	}
}
