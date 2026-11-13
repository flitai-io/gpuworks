package partitioncap

import (
	"encoding/json"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func testNode(t *testing.T, vendor string, reports []proto.AssetReport) *store.Node {
	t.Helper()
	raw := ""
	if reports != nil {
		data, err := json.Marshal(reports)
		if err != nil {
			t.Fatal(err)
		}
		raw = string(data)
	}
	probes := "null"
	if reports != nil {
		probeData, err := json.Marshal([]proto.PartitionProbeResult{{Backend: accelerator.BackendAscendLogical, GPUIndex: new(int), Status: proto.PartitionProbePassed}})
		if err != nil {
			t.Fatal(err)
		}
		probes = string(probeData)
	}
	node := &store.Node{GPUVendor: vendor, Arch: "amd64", Framework: "cann", FrameworkVersion: "8.5.1", RuntimeDriver: "docker", PartitionAssets: raw, PartitionProbes: probes, PartitionSafetyProtocol: 1}
	node.GPUs = []store.GPU{{Index: 0, Vendor: vendor, ArchID: "910B4"}}
	return node
}

func TestNVIDIADGPURequiresSafeTeardownProtocol(t *testing.T) {
	node := testNode(t, "nvidia", []proto.AssetReport{{Backend: accelerator.BackendNVIDIALogical, Root: "/assets", Files: []string{"libvgpu.so"}}})
	node.GPUs[0].ArchID = "sm_80"
	node.PartitionSafetyProtocol = 0
	capabilities := ForGPU(node, &node.GPUs[0])
	if len(capabilities) != 1 || capabilities[0].Status != StatusUnavailable || !strings.Contains(capabilities[0].Reason, "upgrade the agent") {
		t.Fatalf("old agent capability = %+v", capabilities)
	}
	node.PartitionSafetyProtocol = 1
	capabilities = ForGPU(node, &node.GPUs[0])
	if len(capabilities) != 1 || capabilities[0].Status != StatusReady {
		t.Fatalf("safe agent capability = %+v", capabilities)
	}
}

func TestCapabilityDerivationMatrix(t *testing.T) {
	t.Run("probe passed is ready", func(t *testing.T) {
		node := testNode(t, "ascend", []proto.AssetReport{{Backend: accelerator.BackendAscendLogical, Root: "/assets", Files: []string{"libvruntime.so"}}})
		got := ForGPU(node, &node.GPUs[0])
		if len(got) != 1 || got[0].Status != StatusReady || got[0].Reason != "" {
			t.Fatalf("capabilities = %+v", got)
		}
	})
	t.Run("probe failed includes detail", func(t *testing.T) {
		node := testNode(t, "ascend", []proto.AssetReport{{Backend: accelerator.BackendAscendLogical, Root: "/assets", Files: []string{"libvruntime.so"}}})
		node.PartitionProbes = `[{"backend":"ascend-dgpu","gpu_index":0,"status":"failed","detail":"quota was 32768 MB"}]`
		got := ForGPU(node, &node.GPUs[0])
		if len(got) != 1 || got[0].Status != StatusUnavailable || !strings.Contains(got[0].Reason, "quota was 32768 MB") || got[0].ProbeStatus != proto.PartitionProbeFailed {
			t.Fatalf("capabilities = %+v", got)
		}
	})
	t.Run("missing one file", func(t *testing.T) {
		node := testNode(t, "ppu", []proto.AssetReport{{Backend: accelerator.BackendPPULogical, Root: "/assets", Files: []string{"hggc_quota.so"}}})
		node.Framework, node.FrameworkVersion = "hggc", "13.0"
		node.PartitionProbes = `[{"backend":"ppu-dgpu","gpu_index":0,"status":"passed"}]`
		got := ForGPU(node, &node.GPUs[0])
		if len(got) != 1 || got[0].Status != StatusUnavailable || !strings.Contains(got[0].Reason, "hgml_dlsym_hook.so") {
			t.Fatalf("capabilities = %+v", got)
		}
	})
	t.Run("files split across roots stay unavailable", func(t *testing.T) {
		node := testNode(t, "ppu", []proto.AssetReport{
			{Backend: accelerator.BackendPPULogical, Root: "/assets/a", Files: []string{"hggc_quota.so"}},
			{Backend: accelerator.BackendPPULogical, Root: "/assets/b", Files: []string{"hgml_dlsym_hook.so"}},
		})
		node.Framework, node.FrameworkVersion = "hggc", "13.0"
		node.PartitionProbes = `[{"backend":"ppu-dgpu","gpu_index":0,"status":"passed"}]`
		got := ForGPU(node, &node.GPUs[0])
		if len(got) != 1 || got[0].Status != StatusUnavailable || !strings.Contains(got[0].Reason, "hgml_dlsym_hook.so") {
			t.Fatalf("capabilities = %+v", got)
		}
	})
	t.Run("version outside table stays ready with warning", func(t *testing.T) {
		node := testNode(t, "ascend", []proto.AssetReport{{Backend: accelerator.BackendAscendLogical, Root: "/assets", Files: []string{"libvruntime.so"}}})
		node.FrameworkVersion = "10.1"
		got := ForGPU(node, &node.GPUs[0])
		if len(got) != 1 || got[0].Status != StatusReady || len(got[0].CompatibilityNotes) == 0 || !strings.Contains(strings.Join(got[0].CompatibilityNotes, " "), "outside the built matrix") {
			t.Fatalf("capabilities = %+v", got)
		}
	})
	t.Run("new agent empty report is missing assets", func(t *testing.T) {
		node := testNode(t, "ascend", []proto.AssetReport{})
		got := ForGPU(node, &node.GPUs[0])
		if len(got) != 1 || !strings.Contains(got[0].Reason, "libvruntime.so") || strings.Contains(got[0].Reason, "upgrade the agent") {
			t.Fatalf("capabilities = %+v", got)
		}
	})
	t.Run("old agent", func(t *testing.T) {
		node := testNode(t, "ascend", nil)
		got := ForGPU(node, &node.GPUs[0])
		if len(got) != 1 || !strings.Contains(got[0].Reason, "upgrade the agent") {
			t.Fatalf("capabilities = %+v", got)
		}
	})
	t.Run("vendor mismatch has no entry", func(t *testing.T) {
		// Apple has no registered logical partition backend at all (unlike
		// AMD, which now owns amd-dgpu), so it stays the case with zero
		// matching backends rather than one unavailable for a wrong reason.
		node := testNode(t, "apple", []proto.AssetReport{{Backend: accelerator.BackendAscendLogical, Files: []string{"libvruntime.so"}}})
		if got := ForGPU(node, &node.GPUs[0]); len(got) != 0 {
			t.Fatalf("capabilities = %+v", got)
		}
	})
}

func TestReasonTextComesFromSingleSwitch(t *testing.T) {
	if got := reason(unavailableMissingAssets, "lib.so"); got != "missing dGPU assets: lib.so" {
		t.Fatalf("reason = %q", got)
	}
}

func TestCapabilitiesAreDerivedFromMatchingGPUProbe(t *testing.T) {
	node := testNode(t, "ascend", []proto.AssetReport{{Backend: accelerator.BackendAscendLogical, Root: "/assets", Files: []string{"libvruntime.so"}}})
	node.GPUs = append(node.GPUs, store.GPU{Index: 3, Vendor: "ascend", ArchID: "910B4"})
	node.PartitionProbes = `[{"backend":"ascend-dgpu","gpu_index":0,"status":"passed"},{"backend":"ascend-dgpu","gpu_index":3,"status":"failed","detail":"quota mismatch"}]`
	first, second := ForGPU(node, &node.GPUs[0]), ForGPU(node, &node.GPUs[1])
	if first[0].Status != StatusReady || second[0].Status != StatusUnavailable || !strings.Contains(second[0].Reason, "quota mismatch") {
		t.Fatalf("per-GPU capabilities = %+v / %+v", first, second)
	}
	node.PartitionProbes = `[{"backend":"ascend-dgpu","status":"passed"}]`
	if got := ForGPU(node, &node.GPUs[0]); got[0].Status != StatusUnavailable || !strings.Contains(got[0].Reason, "upgrade the agent") {
		t.Fatalf("legacy backend-wide probe was accepted: %+v", got)
	}
}

func TestPPUDockerDoesNotRequireHostHGGCRuntime(t *testing.T) {
	node := &store.Node{
		Arch: "amd64", Framework: "hggc", RuntimeDriver: "docker",
		PartitionAssets: `[{"backend":"ppu-dgpu","root":"/assets","files":["hggc_quota.so","hgml_dlsym_hook.so"]}]`,
		PartitionProbes: `[{"backend":"ppu-dgpu","gpu_index":0,"status":"passed"}]`,
	}
	gpu := &store.GPU{Vendor: "ppu", ArchID: "ZW810E"}
	got := ForGPU(node, gpu)
	if len(got) != 1 || got[0].Status != StatusReady || got[0].Reason != "" {
		t.Fatalf("PPU capability = %+v, want ready Docker path", got)
	}
}

func TestPPUUdockerRemainsUnavailableUntilValidated(t *testing.T) {
	node := &store.Node{
		Arch: "amd64", Framework: "hggc", RuntimeDriver: "udocker",
		PartitionAssets: `[{"backend":"ppu-dgpu","root":"/assets","files":["hggc_quota.so","hgml_dlsym_hook.so"]}]`,
		PartitionProbes: `[{"backend":"ppu-dgpu","gpu_index":0,"status":"skipped","detail":"PPU is currently verified on the Docker runtime only"}]`,
	}
	gpu := &store.GPU{Vendor: "ppu", ArchID: "ZW810E"}
	got := ForGPU(node, gpu)
	if len(got) != 1 || got[0].Status != StatusUnavailable || !strings.Contains(got[0].Reason, "Docker runtime only") {
		t.Fatalf("PPU capability = %+v, want Docker-only reason", got)
	}
}

// A quota that names no backend takes the node's own ready one. An Ascend node
// must answer its vcann-rt backend: answering NVIDIA's, the historical default,
// refused every Ascend node a placement had not named a backend for.
func TestPreferredIsTheNodesReadyBackend(t *testing.T) {
	ascend := testNode(t, "ascend", []proto.AssetReport{{Backend: accelerator.BackendAscendLogical, Root: "/assets", Files: []string{"libvruntime.so"}}})
	if got, ok := Preferred(ascend); !ok || got.Backend != accelerator.BackendAscendLogical {
		t.Fatalf("ascend preferred = %+v ok=%v, want %s", got, ok, accelerator.BackendAscendLogical)
	}
	nvidia := testNode(t, "nvidia", []proto.AssetReport{{Backend: accelerator.BackendNVIDIALogical, Root: "/assets", Files: []string{"libvgpu.so"}}})
	nvidia.GPUs[0].ArchID = "sm_80"
	if got, ok := Preferred(nvidia); !ok || got.Backend != accelerator.BackendNVIDIALogical {
		t.Fatalf("nvidia preferred = %+v ok=%v, want %s", got, ok, accelerator.BackendNVIDIALogical)
	}
	nvidia.PartitionSafetyProtocol = 0
	if got, ok := Preferred(nvidia); ok {
		t.Fatalf("an unavailable backend was preferred: %+v", got)
	}
	amd := testNode(t, "amd", []proto.AssetReport{{Backend: accelerator.BackendAMDLogical, Root: "/assets", Files: []string{"libvrocm.so"}}})
	if got, ok := Preferred(amd); !ok || got.Backend != accelerator.BackendAMDLogical {
		t.Fatalf("amd preferred = %+v ok=%v, want %s", got, ok, accelerator.BackendAMDLogical)
	}
	// No assets reported: nothing can be ready, answered without deriving a
	// capability per card.
	bare := testNode(t, "nvidia", nil)
	bare.GPUs[0].ArchID = "sm_80"
	if got, ok := Preferred(bare); ok {
		t.Fatalf("a node without assets preferred %+v", got)
	}
}

// A capability no probe has answered for carries no check time on the wire; a
// zero time rendered as "last checked 0001-01-01".
func TestUnprobedCapabilityHasNoCheckTime(t *testing.T) {
	node := testNode(t, "nvidia", []proto.AssetReport{{Backend: accelerator.BackendNVIDIALogical, Root: "/assets", Files: []string{"libvgpu.so"}}})
	node.GPUs[0].ArchID = "sm_80"
	capabilities := ForGPU(node, &node.GPUs[0])
	if len(capabilities) != 1 {
		t.Fatalf("capabilities = %+v", capabilities)
	}
	wire, err := json.Marshal(capabilities[0])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(wire), "probe_checked_at") {
		t.Fatalf("wire = %s, want no probe_checked_at", wire)
	}
	ascend := testNode(t, "ascend", []proto.AssetReport{{Backend: accelerator.BackendAscendLogical, Root: "/assets", Files: []string{"libvruntime.so"}}})
	if got := ForGPU(ascend, &ascend.GPUs[0]); len(got) != 1 || got[0].ProbeCheckedAt != nil {
		t.Fatalf("a probe result without a time = %+v, want no check time", got)
	}
}

func TestHygonSupportedModelsRequireAssetsAndPerGPUProbe(t *testing.T) {
	for _, model := range []string{"Z100", "K100AI", "BW"} {
		t.Run(model, func(t *testing.T) { testHygonCapabilityPrerequisites(t, model) })
	}
}

func testHygonCapabilityPrerequisites(t *testing.T, model string) {
	node := &store.Node{
		PartitionSafetyProtocol: 1, GPUVendor: "hygon", Arch: "amd64", Framework: "dtk", FrameworkVersion: "26.04", RuntimeDriver: "docker",
		PartitionAssets: `[ {"backend":"hygon-dgpu","root":"/opt","files":["libamdhip64.so","libhydmi.so","libhydmi_mig.so","librocm_smi64.so","gpuworks-hygon-vdev-helper"]} ]`,
		PartitionProbes: `[ {"backend":"hygon-dgpu","gpu_index":0,"status":"passed"} ]`,
	}
	node.GPUs = []store.GPU{{Index: 0, Vendor: "hygon", ArchID: model}, {Index: 1, Vendor: "hygon", ArchID: "K100"}}
	ready := ForGPU(node, &node.GPUs[0])
	if len(ready) != 1 || ready[0].Backend != accelerator.BackendHygonLogical || ready[0].Status != StatusReady {
		t.Fatalf("%s capability = %+v", model, ready)
	}
	if got := ForGPU(node, &node.GPUs[1]); len(got) != 0 {
		t.Fatalf("unverified K100 capability = %+v", got)
	}
	node.RuntimeDriver = "udocker"
	if got := ForGPU(node, &node.GPUs[0]); len(got) != 1 || got[0].Status != StatusReady {
		t.Fatalf("udocker with a passed native quota probe = %+v", got)
	}
	node.PartitionProbes = `[{"backend":"hygon-dgpu","gpu_index":0,"status":"failed","detail":"permission denied"}]`
	if got := ForGPU(node, &node.GPUs[0]); len(got) != 1 || got[0].Status != StatusUnavailable || !strings.Contains(got[0].Reason, "permission denied") {
		t.Fatalf("udocker with a failed quota probe = %+v", got)
	}
	node.PartitionProbes = `[{"backend":"hygon-dgpu","gpu_index":0,"status":"passed"}]`
	node.RuntimeDriver = "nerdctl"
	if got := ForGPU(node, &node.GPUs[0]); len(got) != 1 || got[0].Status != StatusUnavailable || !strings.Contains(got[0].Reason, "Docker") {
		t.Fatalf("nerdctl capability = %+v", got)
	}
	node.RuntimeDriver = "docker"
	node.PartitionProbes = `[]`
	if got := ForGPU(node, &node.GPUs[0]); len(got) != 1 || got[0].Status != StatusUnavailable || got[0].ProbeStatus == proto.PartitionProbePassed {
		t.Fatalf("missing probe capability = %+v", got)
	}
}

// AMD is probe-exempt like NVIDIA: readiness rests on the agent's safety
// protocol and reported assets alone, under Docker or nerdctl; no probe
// report is required or consulted.
func TestAMDDGPUIsProbeExemptAndRuntimeConstrained(t *testing.T) {
	node := &store.Node{
		PartitionSafetyProtocol: 1, GPUVendor: "amd", Arch: "amd64", Framework: "rocm", FrameworkVersion: "7.2.3", RuntimeDriver: "docker",
		PartitionAssets: `[{"backend":"amd-dgpu","root":"/opt","files":["libvrocm.so"]}]`,
	}
	node.GPUs = []store.GPU{{Index: 0, Vendor: "amd", ArchID: "gfx942"}}
	got := ForGPU(node, &node.GPUs[0])
	if len(got) != 1 || got[0].Backend != accelerator.BackendAMDLogical || got[0].Status != StatusReady || got[0].ProbeStatus != "" {
		t.Fatalf("AMD docker capability = %+v, want ready with no probe status", got)
	}
	node.RuntimeDriver = "nerdctl"
	if got := ForGPU(node, &node.GPUs[0]); len(got) != 1 || got[0].Status != StatusReady {
		t.Fatalf("AMD nerdctl capability = %+v", got)
	}
	node.RuntimeDriver = "udocker"
	if got := ForGPU(node, &node.GPUs[0]); len(got) != 1 || got[0].Status != StatusReady {
		t.Fatalf("AMD runveil capability = %+v", got)
	}
	node.RuntimeDriver = "host"
	if got := ForGPU(node, &node.GPUs[0]); len(got) != 1 || got[0].Status != StatusUnavailable || !strings.Contains(got[0].Reason, "Docker, nerdctl or udocker") {
		t.Fatalf("AMD host-process capability = %+v", got)
	}
	node.RuntimeDriver = "docker"
	node.PartitionSafetyProtocol = 0
	if got := ForGPU(node, &node.GPUs[0]); len(got) != 1 || got[0].Status != StatusUnavailable || !strings.Contains(got[0].Reason, "upgrade the agent") {
		t.Fatalf("old agent AMD capability = %+v", got)
	}
}
