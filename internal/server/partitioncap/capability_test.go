package partitioncap

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/messages"
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
	if len(capabilities) != 1 || capabilities[0].Status != StatusUnavailable || capabilities[0].ReasonCode != messages.ErrorPartitionAgentUpgradeRequired {
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
		if len(got) != 1 || got[0].Status != StatusUnavailable || !hasReason(got[0], messages.ErrorPartitionProbeFailed, proto.PartitionReasonParams{ProbeDetail: "quota was 32768 MB"}) || got[0].ProbeStatus != proto.PartitionProbeFailed {
			t.Fatalf("capabilities = %+v", got)
		}
	})
	t.Run("missing one file", func(t *testing.T) {
		node := testNode(t, "ppu", []proto.AssetReport{{Backend: accelerator.BackendPPULogical, Root: "/assets", Files: []string{"hggc_quota.so"}}})
		node.Framework, node.FrameworkVersion = "hggc", "13.0"
		node.PartitionProbes = `[{"backend":"ppu-dgpu","gpu_index":0,"status":"passed"}]`
		got := ForGPU(node, &node.GPUs[0])
		if len(got) != 1 || got[0].Status != StatusUnavailable || !hasReason(got[0], messages.ErrorPartitionAssetsMissing, proto.PartitionReasonParams{MissingAssets: []string{"hgml_dlsym_hook.so"}}) {
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
		if len(got) != 1 || got[0].Status != StatusUnavailable || !hasReason(got[0], messages.ErrorPartitionAssetsMissing, proto.PartitionReasonParams{MissingAssets: []string{"hgml_dlsym_hook.so"}}) {
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
		if len(got) != 1 || !hasReason(got[0], messages.ErrorPartitionAssetsMissing, proto.PartitionReasonParams{MissingAssets: []string{"libvruntime.so"}}) || got[0].ReasonCode == messages.ErrorPartitionAgentUpgradeRequired {
			t.Fatalf("capabilities = %+v", got)
		}
	})
	t.Run("old agent", func(t *testing.T) {
		node := testNode(t, "ascend", nil)
		got := ForGPU(node, &node.GPUs[0])
		if len(got) != 1 || got[0].ReasonCode != messages.ErrorPartitionAgentUpgradeRequired {
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

// The English fallback keeps the wording scripts and older clients read; the
// code and its typed params are what clients render.
func TestReasonFallbackKeepsTheEnglishWording(t *testing.T) {
	gpu := 3
	for _, tc := range []struct {
		code   string
		params proto.PartitionReasonParams
		want   string
	}{
		{messages.ErrorPartitionRuntimeMissing, proto.PartitionReasonParams{}, "container inference unverified: no usable docker/udocker runtime on the node"},
		{messages.ErrorPartitionRuntimeUnsupported, proto.PartitionReasonParams{Runtime: "udocker", SupportedRuntimes: []string{"docker", "kube"}}, "container inference unverified: the backend requires the docker, kube runtime, the node runs udocker"},
		{messages.ErrorPartitionAgentUpgradeRequired, proto.PartitionReasonParams{}, "agent has not reported current dGPU safety capability; upgrade the agent"},
		{messages.ErrorPartitionAssetsMissing, proto.PartitionReasonParams{MissingAssets: []string{"a.so", "b.so"}}, "missing dGPU assets: a.so, b.so"},
		{messages.ErrorPartitionProbePending, proto.PartitionReasonParams{GPUIndex: &gpu}, "dGPU probe has not completed: no result received for GPU 3"},
		{messages.ErrorPartitionProbePending, proto.PartitionReasonParams{}, "dGPU probe has not completed"},
		{messages.ErrorPartitionProbeSkipped, proto.PartitionReasonParams{ProbeDetail: "no container runtime available on the node"}, "container inference unverified: no container runtime available on the node"},
		{messages.ErrorPartitionProbeFailed, proto.PartitionReasonParams{ProbeDetail: "quota mismatch"}, "dGPU probe failed: quota mismatch"},
		{messages.ErrorPartitionProbeFailed, proto.PartitionReasonParams{}, "dGPU probe failed"},
	} {
		var capability proto.PartitionCapability
		unavailable(&capability, tc.code, tc.params)
		if capability.Status != StatusUnavailable || capability.ReasonCode != tc.code || capability.Reason != tc.want {
			t.Errorf("%s %+v = %+v, want reason %q", tc.code, tc.params, capability, tc.want)
		}
	}
}

// The capability's code and typed params go on the wire next to the
// fallback, with exactly these keys.
func TestUnavailableCapabilityWire(t *testing.T) {
	node := testNode(t, "ascend", []proto.AssetReport{{Backend: accelerator.BackendAscendLogical, Root: "/assets", Files: []string{"libvruntime.so"}}})
	node.PartitionProbes = `[{"backend":"ascend-dgpu","gpu_index":0,"status":"failed","detail":"quota was 32768 MB"}]`
	got := ForGPU(node, &node.GPUs[0])
	if len(got) != 1 {
		t.Fatalf("capabilities = %+v", got)
	}
	got[0].CompatibilityNotes, got[0].Compatibility = nil, ""
	wire, err := json.Marshal(got[0])
	if err != nil {
		t.Fatal(err)
	}
	want := `{"backend":"ascend-dgpu","display_name":"` + got[0].DisplayName + `","vendor":"ascend","maturity":"` + got[0].Maturity + `","status":"unavailable",` +
		`"reason":"dGPU probe failed: quota was 32768 MB","reason_code":"PARTITION_PROBE_FAILED","reason_params":{"probe_detail":"quota was 32768 MB"},"probe_status":"failed"}`
	if string(wire) != want {
		t.Fatalf("wire = %s\nwant  %s", wire, want)
	}
	var back proto.PartitionCapability
	if err := json.Unmarshal(wire, &back); err != nil || !reflect.DeepEqual(back, got[0]) {
		t.Fatalf("round trip = %+v (%v), want %+v", back, err, got[0])
	}
	pending := testNode(t, "ascend", []proto.AssetReport{{Backend: accelerator.BackendAscendLogical, Root: "/assets", Files: []string{"libvruntime.so"}}})
	pending.PartitionProbes = `[]`
	got = ForGPU(pending, &pending.GPUs[0])
	if len(got) != 1 {
		t.Fatalf("capabilities = %+v", got)
	}
	wire, _ = json.Marshal(got[0].ReasonParams)
	if got[0].ReasonCode != messages.ErrorPartitionProbePending || string(wire) != `{"gpu_index":0}` {
		t.Fatalf("pending reason = %s %s", got[0].ReasonCode, wire)
	}
	ready := testNode(t, "ascend", []proto.AssetReport{{Backend: accelerator.BackendAscendLogical, Root: "/assets", Files: []string{"libvruntime.so"}}})
	got = ForGPU(ready, &ready.GPUs[0])
	wire, _ = json.Marshal(got[0])
	if strings.Contains(string(wire), "reason") {
		t.Fatalf("ready wire = %s, want no reason keys", wire)
	}
}

// A refused backend is a ProductError carrying the capability's own code and
// params, plus the backend's display name and the node.
func TestValidateReadyRefusesWithTheReasonCode(t *testing.T) {
	node := testNode(t, "ascend", []proto.AssetReport{{Backend: accelerator.BackendAscendLogical, Root: "/assets", Files: []string{"libvruntime.so"}}})
	node.Name = "npu-1"
	if err := ValidateReady(node, accelerator.BackendAscendLogical); err != nil {
		t.Fatalf("ready backend refused: %v", err)
	}
	node.PartitionProbes = `[{"backend":"ascend-dgpu","gpu_index":0,"status":"failed","detail":"quota mismatch"}]`
	var product *messages.ProductError
	err := ValidateReady(node, accelerator.BackendAscendLogical)
	if !errors.As(err, &product) || product.Code() != messages.ErrorPartitionProbeFailed {
		t.Fatalf("err = %v, want %s", err, messages.ErrorPartitionProbeFailed)
	}
	display := ForNode(node)[0].DisplayName
	wantParams := map[string]any{"backend": display, "node": "npu-1", "reason_code": messages.ErrorPartitionProbeFailed, "probe_detail": "quota mismatch"}
	if !reflect.DeepEqual(product.Params(), wantParams) || err.Error() != "partition backend ascend-dgpu is unavailable: dGPU probe failed: quota mismatch" {
		t.Fatalf("params = %#v, message = %q", product.Params(), err.Error())
	}
	if err := ValidateReady(node, "no-such-dgpu"); !errors.As(err, &product) || product.Code() != messages.ErrorPartitionBackendUnknown || product.Params()["backend"] != "no-such-dgpu" {
		t.Fatalf("unknown backend err = %v", err)
	}
	if err := ValidateReady(node, accelerator.BackendNVIDIALogical); !errors.As(err, &product) || product.Code() != messages.ErrorPartitionBackendNotOnNode || product.Params()["node"] != "npu-1" {
		t.Fatalf("foreign backend err = %v", err)
	}
}

func hasReason(capability proto.PartitionCapability, code string, params proto.PartitionReasonParams) bool {
	if capability.ReasonCode != code || capability.ReasonParams == nil {
		return false
	}
	return reflect.DeepEqual(*capability.ReasonParams, params)
}

func TestCapabilitiesAreDerivedFromMatchingGPUProbe(t *testing.T) {
	node := testNode(t, "ascend", []proto.AssetReport{{Backend: accelerator.BackendAscendLogical, Root: "/assets", Files: []string{"libvruntime.so"}}})
	node.GPUs = append(node.GPUs, store.GPU{Index: 3, Vendor: "ascend", ArchID: "910B4"})
	node.PartitionProbes = `[{"backend":"ascend-dgpu","gpu_index":0,"status":"passed"},{"backend":"ascend-dgpu","gpu_index":3,"status":"failed","detail":"quota mismatch"}]`
	first, second := ForGPU(node, &node.GPUs[0]), ForGPU(node, &node.GPUs[1])
	if first[0].Status != StatusReady || second[0].Status != StatusUnavailable || !hasReason(second[0], messages.ErrorPartitionProbeFailed, proto.PartitionReasonParams{ProbeDetail: "quota mismatch"}) {
		t.Fatalf("per-GPU capabilities = %+v / %+v", first, second)
	}
	node.PartitionProbes = `[{"backend":"ascend-dgpu","status":"passed"}]`
	if got := ForGPU(node, &node.GPUs[0]); got[0].Status != StatusUnavailable || got[0].ReasonCode != messages.ErrorPartitionAgentUpgradeRequired {
		t.Fatalf("legacy backend-wide probe was accepted: %+v", got)
	}
}

func TestPPUDockerDoesNotRequireHostHGGCRuntime(t *testing.T) {
	for _, driver := range []string{"docker", "kube"} {
		t.Run(driver, func(t *testing.T) {
			node := &store.Node{
				Arch: "amd64", Framework: "hggc", RuntimeDriver: driver,
				PartitionAssets: `[{"backend":"ppu-dgpu","root":"/assets","files":["hggc_quota.so","hgml_dlsym_hook.so"]}]`,
				PartitionProbes: `[{"backend":"ppu-dgpu","gpu_index":0,"status":"passed"}]`,
			}
			gpu := &store.GPU{Vendor: "ppu", ArchID: "ZW810E"}
			got := ForGPU(node, gpu)
			if len(got) != 1 || got[0].Status != StatusReady || got[0].Reason != "" {
				t.Fatalf("PPU capability under %s = %+v, want ready", driver, got)
			}
		})
	}
}

func TestPPUUdockerRemainsUnavailableUntilValidated(t *testing.T) {
	node := &store.Node{
		Arch: "amd64", Framework: "hggc", RuntimeDriver: "udocker",
		PartitionAssets: `[{"backend":"ppu-dgpu","root":"/assets","files":["hggc_quota.so","hgml_dlsym_hook.so"]}]`,
		PartitionProbes: `[{"backend":"ppu-dgpu","gpu_index":0,"status":"skipped","detail":"PPU is currently verified on the Docker or Kubernetes runtime only"}]`,
	}
	gpu := &store.GPU{Vendor: "ppu", ArchID: "ZW810E"}
	got := ForGPU(node, gpu)
	if len(got) != 1 || got[0].Status != StatusUnavailable || !hasReason(got[0], messages.ErrorPartitionRuntimeUnsupported, proto.PartitionReasonParams{Runtime: "udocker", SupportedRuntimes: []string{"docker", "kube"}}) {
		t.Fatalf("PPU capability = %+v, want Docker/Kubernetes-only reason", got)
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
	if got := ForGPU(node, &node.GPUs[0]); len(got) != 1 || got[0].Status != StatusUnavailable || !hasReason(got[0], messages.ErrorPartitionProbeFailed, proto.PartitionReasonParams{ProbeDetail: "permission denied"}) {
		t.Fatalf("udocker with a failed quota probe = %+v", got)
	}
	node.PartitionProbes = `[{"backend":"hygon-dgpu","gpu_index":0,"status":"passed"}]`
	node.RuntimeDriver = "nerdctl"
	if got := ForGPU(node, &node.GPUs[0]); len(got) != 1 || got[0].Status != StatusUnavailable || !hasReason(got[0], messages.ErrorPartitionRuntimeUnsupported, proto.PartitionReasonParams{Runtime: "nerdctl", SupportedRuntimes: []string{"docker", "udocker"}}) {
		t.Fatalf("nerdctl capability = %+v", got)
	}
	// Hygon native vdev stays unsupported on Kubernetes, unlike AMD and PPU.
	node.RuntimeDriver = "kube"
	if got := ForGPU(node, &node.GPUs[0]); len(got) != 1 || got[0].Status != StatusUnavailable || !hasReason(got[0], messages.ErrorPartitionRuntimeUnsupported, proto.PartitionReasonParams{Runtime: "kube", SupportedRuntimes: []string{"docker", "udocker"}}) {
		t.Fatalf("kube capability = %+v, want unavailable", got)
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
	node.RuntimeDriver = "kube"
	if got := ForGPU(node, &node.GPUs[0]); len(got) != 1 || got[0].Status != StatusReady {
		t.Fatalf("AMD kube capability = %+v", got)
	}
	node.RuntimeDriver = "host"
	if got := ForGPU(node, &node.GPUs[0]); len(got) != 1 || got[0].Status != StatusUnavailable || !hasReason(got[0], messages.ErrorPartitionRuntimeUnsupported, proto.PartitionReasonParams{Runtime: "host", SupportedRuntimes: []string{"docker", "nerdctl", "udocker", "kube"}}) {
		t.Fatalf("AMD host-process capability = %+v", got)
	}
	node.RuntimeDriver = "docker"
	node.PartitionSafetyProtocol = 0
	if got := ForGPU(node, &node.GPUs[0]); len(got) != 1 || got[0].Status != StatusUnavailable || got[0].ReasonCode != messages.ErrorPartitionAgentUpgradeRequired {
		t.Fatalf("old agent AMD capability = %+v", got)
	}
}
