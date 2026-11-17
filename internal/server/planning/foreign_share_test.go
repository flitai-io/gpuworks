package planning

import (
	"encoding/json"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/nodepolicy"
	"gpuworks.ai/gpuworks/internal/server/scheduler"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// reporterForeign is the holder of flitai-io/gpuworks#20 as nvidia-smi
// reported it: a GPUStack container's python3 holding 63982 MiB.
var reporterForeign = proto.CardProcess{PID: 41235, Name: "python3", MemoryBytes: 67089334272}

// foreignHeldNode is an H20-3e node whose GPU 0 that container holds and
// whose nvidia-dgpu backend is ready, reported by an agent at the given
// partition safety level; GPU 1 runs a whole-card deployment of ours in the
// cases that say so.
func foreignHeldNode(t *testing.T, st *store.Store, name string, safety int, holders ...proto.CardProcess) *store.Node {
	t.Helper()
	node, err := st.RegisterNode(name, store.HashToken(name), name)
	if err != nil {
		t.Fatal(err)
	}
	node.GPUVendor, node.Arch, node.OS, node.RuntimeDriver = "nvidia", "amd64", "linux", "docker"
	held, _ := json.Marshal(holders)
	node.GPUs = []store.GPU{
		{Index: 0, UUID: "GPU-h20-0", Name: "NVIDIA H20-3e", MemoryTotal: 143771 << 20, Vendor: "nvidia", ArchID: "sm_90", ComputeCapability: "9.0", ForeignProcesses: string(held)},
		{Index: 1, UUID: "GPU-h20-1", Name: "NVIDIA H20-3e", MemoryTotal: 143771 << 20, Vendor: "nvidia", ArchID: "sm_90", ComputeCapability: "9.0"},
	}
	var probes []proto.PartitionProbeResult
	for index := range node.GPUs {
		probes = append(probes, proto.PartitionProbeResult{Backend: accelerator.BackendNVIDIALogical, GPUIndex: &index, Status: proto.PartitionProbePassed})
	}
	assets, _ := json.Marshal([]proto.AssetReport{{Backend: accelerator.BackendNVIDIALogical, Root: "/assets", Files: []string{"libvgpu.so"}}})
	probeJSON, _ := json.Marshal(probes)
	node.PartitionAssets, node.PartitionProbes, node.PartitionSafetyProtocol = string(assets), string(probeJSON), safety
	return node
}

func foreignStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open("file:"+strings.ReplaceAll(t.Name(), "/", "_")+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// An agent that predates keeping foreign processes reported once a
// partition of ours runs beside them would drop them from its next report,
// and the server would then offer the whole card. Only an agent that keeps
// them may share a held card; what the holders take is counted either way.
func TestNodeSnapshotSharesAHeldCardOnlyWithAnAgentThatKeepsReportingTheHolders(t *testing.T) {
	st := foreignStore(t)
	for _, tc := range []struct {
		name    string
		safety  int
		holders []proto.CardProcess
		share   bool
		bytes   int64
	}{
		{"current agent", proto.PartitionSafetyForeignTracked, []proto.CardProcess{reporterForeign}, true, reporterForeign.MemoryBytes},
		{"agent before foreign tracking", 1, []proto.CardProcess{reporterForeign}, false, reporterForeign.MemoryBytes},
		{"agent without the safety protocol", 0, []proto.CardProcess{reporterForeign}, false, reporterForeign.MemoryBytes},
		{"memory not reported", proto.PartitionSafetyForeignTracked, []proto.CardProcess{{PID: 41235, Name: "python3"}}, false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			node := foreignHeldNode(t, st, strings.ReplaceAll(tc.name, " ", "-"), tc.safety, tc.holders...)
			snap := NodeSnapshot(st, node, true, "")
			card := snap.GPUs[0]
			if card.Schedulable || !card.HeldOutsidePlatform {
				t.Fatalf("held card = %+v, want it held and taking no whole-card work", card)
			}
			if card.ForeignShareable != tc.share || card.ForeignBytes != tc.bytes {
				t.Fatalf("held card shareable=%v bytes=%d, want %v, %d", card.ForeignShareable, card.ForeignBytes, tc.share, tc.bytes)
			}
			if free := snap.GPUs[1]; free.ForeignShareable || free.ForeignBytes != 0 || !free.Schedulable {
				t.Fatalf("free card = %+v", free)
			}
		})
	}
}

// The deploy form's evaluation of the reported deployment: a 50 GiB
// nvidia-dgpu partition an operator pinned to the held GPU 0 is placed
// there (picking the card is consent), the card is offered to it, and a
// whole-card deployment is still refused the card.
func TestEvaluatePlacesTheReportedPartitionBesideTheForeignProcess(t *testing.T) {
	st := foreignStore(t)
	node := foreignHeldNode(t, st, "h20", proto.PartitionSafetyForeignTracked, reporterForeign)
	in := Input{
		Model: &store.Model{WeightBytes: 15 << 30}, Node: node, Engine: "vllm",
		Quick: deploy.LaunchConfig{GPUMemUtil: 0.9}, GPUIndexes: []int{0}, Consent: nodepolicy.Consent{CardsChosen: true},
		VRAMLimitBytes: 50 << 30, PartitionBackend: accelerator.BackendNVIDIALogical,
	}
	out := in.Evaluate(st)
	if !out.Result.OK || out.Result.Plan == nil || out.Result.Plan.GPUIndexes[0] != 0 {
		t.Fatalf("evaluation = ok %v %+v plan %+v, want GPU 0", out.Result.OK, out.Result.Message, out.Result.Plan)
	}
	if !candidateAvailable(t, out.GPUCandidates, 0) {
		t.Fatalf("held card not offered to the partition: %+v", out.GPUCandidates)
	}

	whole := in
	whole.VRAMLimitBytes, whole.PartitionBackend = 0, ""
	out = whole.Evaluate(st)
	if out.Result.OK || out.Result.Message.Code != scheduler.ReasonGPUsHeldOutsidePlatform {
		t.Fatalf("whole-card evaluation = ok %v %+v, want %s", out.Result.OK, out.Result.Message, scheduler.ReasonGPUsHeldOutsidePlatform)
	}
	if candidateAvailable(t, out.GPUCandidates, 0) {
		t.Fatal("held card offered to a whole-card deployment")
	}

	old := foreignHeldNode(t, st, "h20-old-agent", 1, reporterForeign)
	in.Node = old
	if out := in.Evaluate(st); out.Result.OK || out.Result.Message.Code != scheduler.ReasonGPUsHeldOutsidePlatform {
		t.Fatalf("old agent evaluation = ok %v %+v, want %s", out.Result.OK, out.Result.Message, scheduler.ReasonGPUsHeldOutsidePlatform)
	}
}
