package planning

import (
	"encoding/json"
	"testing"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/estimate"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// A partition admitted beside a process outside the platform may not be
// grown in place into what that process holds: the room a quota change is
// asked against keeps the foreign memory and its headroom out, as admission
// does (scheduler.GPU.Available).
func TestQuotaRoomKeepsTheForeignMemoryOutOfAHeldCard(t *testing.T) {
	st := foreignStore(t)
	node, err := st.RegisterNode("h20", store.HashToken("h20"), "h20")
	if err != nil {
		t.Fatal(err)
	}
	held, _ := json.Marshal([]proto.CardProcess{reporterForeign})
	card := &store.GPU{NodeID: node.ID, Index: 0, UUID: "GPU-h20-0", Name: "NVIDIA H20-3e", Vendor: "nvidia",
		ArchID: "sm_90", ComputeCapability: "9.0", MemoryTotal: 143771 << 20, ForeignProcesses: string(held)}
	if err := st.DB().Create(card).Error; err != nil {
		t.Fatal(err)
	}
	if err := st.DB().Model(&store.Node{}).Where("id = ?", node.ID).Updates(map[string]any{
		"gpu_vendor": "nvidia", "arch": "amd64", "os": "linux", "runtime_driver": "docker",
		"partition_safety_protocol": proto.PartitionSafetyForeignTracked,
	}).Error; err != nil {
		t.Fatal(err)
	}
	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "org/m", Name: "m", WeightBytes: 15 << 30})
	if err != nil {
		t.Fatal(err)
	}
	dep, err := st.CreateDeployment(&store.Deployment{Name: "sliced", ModelID: model.ID, NodeID: node.ID, Engine: "vllm",
		VRAMLimitBytes: 50 << 30, PartitionBackend: accelerator.BackendNVIDIALogical})
	if err != nil {
		t.Fatal(err)
	}
	spec, _ := json.Marshal(proto.InstanceDesiredState{UUID: "slice", Engine: "vllm", VRAMLimitBytes: 50 << 30, PartitionBackend: accelerator.BackendNVIDIALogical})
	if _, err := st.CreateInstance(&store.Instance{UUID: "slice", DeploymentID: dep.ID, NodeID: node.ID, State: proto.InstRunning,
		GPUIndexes: "[0]", DesiredStateJSON: string(spec)}); err != nil {
		t.Fatal(err)
	}
	short, err := QuotaRoom(st, dep, 120<<30, []string{"slice"})
	if err != nil {
		t.Fatal(err)
	}
	total := int64(143771 << 20)
	want := total - reporterForeign.MemoryBytes - estimate.ForeignHeadroomBytes(total)
	if short == nil || short.FreeBytes != want {
		t.Fatalf("shortfall = %+v, want the card refused with %d free (total less foreign and headroom)", short, want)
	}
	if short, err := QuotaRoom(st, dep, 60<<30, []string{"slice"}); err != nil || short != nil {
		t.Fatalf("a 60 GiB quota beside the process = %+v %v, want it kept in place", short, err)
	}
}

// A held card whose foreign memory is not known - a holder carried over from
// a previous boot without its PID, or one the vendor tool gave no figure -
// offers a quota change no room beyond what the partitions already claim:
// admission refuses such a card outright, and reading the unknown as nothing
// held would let a partition grow into the outside workload's memory. A
// shrink still fits.
func TestQuotaRoomRefusesGrowthOnAHeldCardWhoseForeignMemoryIsUnknown(t *testing.T) {
	for _, tc := range []struct {
		name   string
		holder proto.CardProcess
	}{
		{"holder from a previous boot", proto.CardProcess{Name: "python3", MemoryBytes: 60 << 30}},
		{"holder without a figure", proto.CardProcess{PID: 41235, Name: "python3"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := foreignStore(t)
			dep := quotaRoomHeldCard(t, st, tc.holder)
			short, err := QuotaRoom(st, dep, 70<<30, []string{"slice"})
			if err != nil {
				t.Fatal(err)
			}
			if short == nil || short.NeedBytes <= short.FreeBytes {
				t.Fatalf("shortfall = %+v, want growth refused on a card whose foreign memory is unknown", short)
			}
			if short, err := QuotaRoom(st, dep, 30<<30, []string{"slice"}); err != nil || short != nil {
				t.Fatalf("a shrink = %+v %v, want it kept in place", short, err)
			}
		})
	}
}

// quotaRoomHeldCard is a 140 GiB card held by holder with a 50 GiB
// nvidia-dgpu partition "slice" of the returned deployment running on it.
func quotaRoomHeldCard(t *testing.T, st *store.Store, holder proto.CardProcess) *store.Deployment {
	t.Helper()
	node, err := st.RegisterNode("h20", store.HashToken("h20"), "h20")
	if err != nil {
		t.Fatal(err)
	}
	held, _ := json.Marshal([]proto.CardProcess{holder})
	card := &store.GPU{NodeID: node.ID, Index: 0, UUID: "GPU-h20-0", Name: "NVIDIA H20-3e", Vendor: "nvidia",
		ArchID: "sm_90", ComputeCapability: "9.0", MemoryTotal: 143771 << 20, ForeignProcesses: string(held)}
	if err := st.DB().Create(card).Error; err != nil {
		t.Fatal(err)
	}
	if err := st.DB().Model(&store.Node{}).Where("id = ?", node.ID).Updates(map[string]any{
		"gpu_vendor": "nvidia", "arch": "amd64", "os": "linux", "runtime_driver": "docker",
		"partition_safety_protocol": proto.PartitionSafetyForeignTracked,
	}).Error; err != nil {
		t.Fatal(err)
	}
	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "org/m", Name: "m", WeightBytes: 15 << 30})
	if err != nil {
		t.Fatal(err)
	}
	dep, err := st.CreateDeployment(&store.Deployment{Name: "sliced", ModelID: model.ID, NodeID: node.ID, Engine: "vllm",
		VRAMLimitBytes: 50 << 30, PartitionBackend: accelerator.BackendNVIDIALogical})
	if err != nil {
		t.Fatal(err)
	}
	spec, _ := json.Marshal(proto.InstanceDesiredState{UUID: "slice", Engine: "vllm", VRAMLimitBytes: 50 << 30, PartitionBackend: accelerator.BackendNVIDIALogical})
	if _, err := st.CreateInstance(&store.Instance{UUID: "slice", DeploymentID: dep.ID, NodeID: node.ID, State: proto.InstRunning,
		GPUIndexes: "[0]", DesiredStateJSON: string(spec)}); err != nil {
		t.Fatal(err)
	}
	return dep
}
