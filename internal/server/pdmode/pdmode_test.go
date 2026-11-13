package pdmode

import (
	"strings"
	"testing"
)

func TestVLLMNixlDeclaration(t *testing.T) {
	mode, ok := Lookup("vllm-nixl")
	if !ok {
		t.Fatal("vllm-nixl missing")
	}
	if mode.Roles[0] != RoleDecode || !mode.HasRole(RolePrefill) || mode.HasRole("encode") {
		t.Fatalf("roles = %v", mode.Roles)
	}
	if mode.ConnectorRole(RolePrefill) != "kv_producer" || mode.ConnectorRole(RoleDecode) != "kv_consumer" {
		t.Fatalf("connector roles = %v", mode.ConnectorRoles)
	}
	if !mode.TwoStage("/v1/chat/completions") || !mode.TwoStage("/v1/completions") || mode.TwoStage("/v1/embeddings") {
		t.Fatal("two-stage paths wrong")
	}
	if mode.TransferMetricsRole != RoleDecode {
		t.Fatal("NIXL transfer counters live on the decoder")
	}
	if !mode.SupportsVendor("nvidia") || mode.SupportsVendor("ascend") {
		t.Fatal("vendor list wrong")
	}
}

func TestCheckTP(t *testing.T) {
	nixl, _ := Lookup("vllm-nixl")
	for _, c := range []struct {
		p, d int
		ok   bool
	}{{1, 1, true}, {1, 4, true}, {4, 2, false}} {
		if err := nixl.CheckTP(c.p, c.d); (err == nil) != c.ok {
			t.Fatalf("CheckTP(%d,%d) = %v", c.p, c.d, err)
		}
	}
	equal := Mode{Name: "x", TP: TPEqual}
	if equal.CheckTP(2, 2) != nil || equal.CheckTP(2, 4) == nil {
		t.Fatal("equal rule")
	}
}

func TestSideChannelPortsAndValues(t *testing.T) {
	nixl, _ := Lookup("vllm-nixl")
	if nixl.ExtraPorts(RolePrefill, 0) != 1 || nixl.ExtraPorts(RoleDecode, 4) != 4 {
		t.Fatal("side channel block")
	}
	sglang, ok := Lookup("sglang-nixl")
	if !ok || sglang.ExtraPorts(RolePrefill, 4) != 1 || sglang.ExtraPorts(RoleDecode, 4) != 0 {
		t.Fatalf("sglang ports: %+v", sglang)
	}
	if sglang.DecodeServesAlone || sglang.MayBeEmpty(RolePrefill) || !nixl.DecodeServesAlone || sglang.Wire != WireConcurrent {
		t.Fatal("sglang has no decode-only path")
	}
	// Each mode states its own policy values and defaults.
	if !nixl.ValidFallback("") || !nixl.ValidFallback(FallbackDecode) || !nixl.ValidFallback(FallbackFail) || nixl.ValidFallback("drop") {
		t.Fatal("vllm-nixl fallback values")
	}
	if nixl.EffectiveFallback("") != FallbackDecode || nixl.EffectiveFallback(FallbackFail) != FallbackFail {
		t.Fatal("vllm-nixl falls back to its decoder by default")
	}
	// A decoder that cannot serve alone has nothing to fall back to.
	if sglang.ValidFallback(FallbackDecode) || !sglang.ValidFallback(FallbackFail) || sglang.EffectiveFallback("") != FallbackFail {
		t.Fatalf("sglang fallbacks = %v", sglang.Fallbacks())
	}
	if !nixl.ValidKVLoadFailure(KVLoadRecompute) || nixl.ValidKVLoadFailure("retry") || nixl.EffectiveKVLoadFailure("") != KVLoadFail {
		t.Fatal("vllm-nixl kv load failure values")
	}
	// SGLang has no KV load failure setting at all.
	if sglang.ValidKVLoadFailure(KVLoadFail) || !sglang.ValidKVLoadFailure("") || sglang.EffectiveKVLoadFailure("") != "" {
		t.Fatal("sglang kv load failure values")
	}
	if got := strings.Join(ForEngine("vllm"), " "); got != "vllm-ascend-mooncake vllm-mooncake vllm-nixl vllm-ppu-mooncake" {
		t.Fatalf("ForEngine = %v", got)
	}
	if got := strings.Join(ForEngine("sglang"), " "); got != "sglang-nixl sglang-ppu-ipc sglang-ppu-mooncake" {
		t.Fatalf("sglang modes = %v", got)
	}
}

func TestPPUMooncakeDeclarations(t *testing.T) {
	for _, name := range []string{"sglang-ppu-mooncake", "vllm-ppu-mooncake"} {
		mode, ok := Lookup(name)
		if !ok || !mode.SupportsVendor("ppu") || mode.SupportsVendor("nvidia") || mode.Unverified {
			t.Fatalf("%s = %+v", name, mode)
		}
		if mode.Transport != TransportMooncakeRDMA || !mode.PeerMemory || mode.ExtraPorts(RolePrefill, 1) != 1 || mode.ExtraPorts(RoleDecode, 1) != 0 {
			t.Fatalf("%s = %+v", name, mode)
		}
	}
	sglang, _ := Lookup("sglang-ppu-mooncake")
	if sglang.Wire != WireConcurrent || sglang.Connector != "mooncake" || sglang.SynthesizedHandoff {
		t.Fatalf("sglang-ppu-mooncake = %+v", sglang)
	}
	vllm, _ := Lookup("vllm-ppu-mooncake")
	if vllm.Wire != WireSequential || vllm.Connector != "MooncakeConnector" || !vllm.SynthesizedHandoff || vllm.ConnectorExtra["mooncake_protocol"] != "rdma" {
		t.Fatalf("vllm-ppu-mooncake = %+v", vllm)
	}
	if !vllm.Transport.MooncakeBootstrap() || !vllm.Transport.OnHostNetwork() || sglang.Transport.MooncakeBootstrap() != true {
		t.Fatalf("transports: vllm %s sglang %s", vllm.Transport, sglang.Transport)
	}
}

func TestAscendMooncakeDeclaration(t *testing.T) {
	mode, ok := Lookup("vllm-ascend-mooncake")
	if !ok || !mode.SupportsVendor("ascend") || mode.SupportsVendor("nvidia") || mode.Unverified {
		t.Fatalf("mode = %+v", mode)
	}
	if mode.CheckTP(4, 1) != nil || mode.CheckTP(1, 4) == nil {
		t.Fatal("Mooncake needs prefill TP >= decode TP")
	}
	if mode.Connector != "MooncakeConnectorV1" || mode.ExtraPorts(RoleDecode, 2) != 2 || mode.Transport != "hccl" {
		t.Fatalf("mode = %+v", mode)
	}
}

// sglang-ppu-ipc carries the KV card to card inside one PPU host, so it
// needs both roles on one node, the IPC switches of the PPU runner's patches
// and a scheduler that never overlaps a forward pass with a copy.
func TestPPUIPCDeclaration(t *testing.T) {
	mode, ok := Lookup("sglang-ppu-ipc")
	if !ok || mode.Engine != "sglang" || !mode.SupportsVendor("ppu") || mode.SupportsVendor("nvidia") || mode.Unverified {
		t.Fatalf("mode = %+v", mode)
	}
	if !mode.SameNode || mode.Transport != TransportIPC || mode.Wire != WireConcurrent || mode.Connector != "mooncake" {
		t.Fatalf("mode = %+v", mode)
	}
	if mode.CheckTP(1, 1) != nil || mode.CheckTP(1, 2) == nil {
		t.Fatal("both roles take the same TP")
	}
	if mode.ExtraPorts(RolePrefill, 1) != 1 || mode.ExtraPorts(RoleDecode, 1) != 0 {
		t.Fatal("the prefill instance needs one bootstrap port")
	}
	for key, want := range map[string]string{"SGLANG_PPU_PD_IPC": "1", "SGLANG_PPU_PD_SERIAL_TRANSFER": "1", "MC_FORCE_MNNVL": "1", "MC_USE_NVLINK_IPC": "1", "SGLANG_ENABLE_HEALTH_ENDPOINT_GENERATION": "0"} {
		if mode.Env[key] != want {
			t.Fatalf("env %s = %q", key, mode.Env[key])
		}
	}
	if len(mode.EngineArgs) != 1 || mode.EngineArgs[0] != "--disable-overlap-schedule" {
		t.Fatalf("engine args = %v", mode.EngineArgs)
	}
	// No other mode is held to one node.
	for _, name := range Names() {
		if other, _ := Lookup(name); name != "sglang-ppu-ipc" && other.SameNode {
			t.Fatalf("%s requires one node", name)
		}
	}
}

func TestVLLMMooncakeDeclaration(t *testing.T) {
	mode, ok := Lookup("vllm-mooncake")
	// Verified on hardware: evaluation no longer warns.
	if !ok || !mode.SupportsVendor("hygon") || mode.SupportsVendor("nvidia") || mode.SupportsVendor("ppu") || mode.Unverified {
		t.Fatalf("mode = %+v", mode)
	}
	if mode.Connector != "MooncakeConnector" || mode.ConnectorRole(RolePrefill) != "kv_producer" || mode.ConnectorRole(RoleDecode) != "kv_consumer" {
		t.Fatalf("connector = %s %v", mode.Connector, mode.ConnectorRoles)
	}
	// The prefill instance answers no kv_transfer_params; the gateway names
	// the handoff and finds the prefiller through its bootstrap server, on
	// whatever host network the group shares.
	if mode.Wire != WireSequential || !mode.SynthesizedHandoff || mode.Transport != TransportMooncake || mode.PeerMemory || len(mode.ConnectorExtra) != 0 {
		t.Fatalf("wire %s synthesized %v transport %s", mode.Wire, mode.SynthesizedHandoff, mode.Transport)
	}
	if mode.ExtraPorts(RolePrefill, 4) != 1 || mode.ExtraPorts(RoleDecode, 4) != 0 {
		t.Fatal("one bootstrap port on the prefill instance, none on the decoder")
	}
	if !mode.DecodeServesAlone || !mode.MayBeEmpty(RolePrefill) || mode.EffectiveFallback("") != FallbackDecode || mode.EffectiveKVLoadFailure("") != KVLoadFail {
		t.Fatal("a decoder without kv_transfer_params computes the prefill itself")
	}
	if mode.CheckTP(2, 2) != nil || mode.CheckTP(1, 2) == nil {
		t.Fatal("only equal TP is verified")
	}
	if !TransportMooncake.OnHostNetwork() || !TransportMooncake.MooncakeBootstrap() || !TransportUCX.OnHostNetwork() || TransportUCX.MooncakeBootstrap() || TransportHCCL.OnHostNetwork() || TransportIPC.OnHostNetwork() {
		t.Fatal("host network transports")
	}
}
