package deploy

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/netplan"
	"gpuworks.ai/gpuworks/internal/server/pdmode"
)

func argValue(t *testing.T, argv []string, flag string) string {
	t.Helper()
	for i, arg := range argv {
		if arg == flag && i+1 < len(argv) {
			return argv[i+1]
		}
	}
	return ""
}

func countFlag(argv []string, flag string) int {
	n := 0
	for _, arg := range argv {
		if arg == flag {
			n++
		}
	}
	return n
}

func pdParams(role string, q LaunchConfig, network ComputeNetwork) Params {
	return Params{
		ModelPath: "/model", ServedName: "qwen", APIPort: 30000, AssignedGPUs: 1, TensorParallel: 1,
		Quick: PDRoleLaunchConfig(EngineVLLM, q, role), ComputeNetwork: network, NNodes: 1,
		PD: &PDLaunch{Mode: "vllm-nixl", Role: role, SideChannelHost: "10.0.0.5", SideChannelPort: 30001},
	}
}

func TestVLLMPDRoleArgvAndEnv(t *testing.T) {
	ethernet := ComputeNetwork{Type: netplan.TypeHostEthernet, Interface: "br_bond0_31"}
	for role, want := range map[string]string{
		"prefill": `{"kv_connector":"NixlConnector","kv_load_failure_policy":"fail","kv_role":"kv_producer"}`,
		"decode":  `{"kv_connector":"NixlConnector","kv_load_failure_policy":"fail","kv_role":"kv_consumer"}`,
	} {
		launch := BuildVLLM(pdParams(role, LaunchConfig{}, ethernet))
		if got := argValue(t, launch.Argv, "--kv-transfer-config"); got != want {
			t.Fatalf("%s config = %s", role, got)
		}
		wantEnv := map[string]string{
			"VLLM_NIXL_SIDE_CHANNEL_HOST": "10.0.0.5", "VLLM_NIXL_SIDE_CHANNEL_PORT": "30001",
			"UCX_NET_DEVICES": "br_bond0_31", "UCX_TCP_BRIDGE_ENABLE": "y",
			"VLLM_SSM_CONV_STATE_LAYOUT": "DS",
		}
		for key, value := range wantEnv {
			if launch.Env[key] != value {
				t.Fatalf("%s env %s = %q, want %q", role, key, launch.Env[key], value)
			}
		}
		if _, ok := launch.Env["UCX_TLS"]; ok {
			t.Fatal("ethernet must not restrict UCX transports")
		}
		// A PD instance is single-node: the intra-node collective stays on loopback.
		if launch.Env["NCCL_SOCKET_IFNAME"] != "lo" {
			t.Fatalf("NCCL_SOCKET_IFNAME = %q", launch.Env["NCCL_SOCKET_IFNAME"])
		}
	}
	hccs := pdParams("decode", LaunchConfig{}, ComputeNetwork{Type: netplan.TypeAcceleratorHCCS, Interface: "br0"})
	hccs.PD.Mode = "vllm-ascend-mooncake"
	launch := BuildVLLM(hccs)
	for key, want := range map[string]string{
		"ASCEND_GLOBAL_RESOURCE_CONFIG": `{"comm_resource_config.protocol_desc":["hccs:device"]}`,
		"ACL_OP_INIT_MODE":              "1",
		"ASCEND_ENABLE_USE_FABRIC_MEM":  "1",
	} {
		if launch.Env[key] != want {
			t.Fatalf("HCCS env %s = %q, want %q", key, launch.Env[key], want)
		}
	}
}

// NixlConnector refuses a hybrid model (Qwen3.5 gated delta net, Kimi KDA,
// Mamba) unless its conv state is dim-first, so every vllm-nixl instance gets
// the layout; a template or the deployment that names one still wins.
func TestVLLMNixlConvStateLayout(t *testing.T) {
	engine, _ := SelectEngine("vllm")
	for name, tc := range map[string]struct {
		template, user map[string]string
		want           string
	}{
		"mode default":             {want: "DS"},
		"template value":           {template: map[string]string{"VLLM_SSM_CONV_STATE_LAYOUT": "SD"}, want: "SD"},
		"deployment over template": {template: map[string]string{"VLLM_SSM_CONV_STATE_LAYOUT": "SD"}, user: map[string]string{"VLLM_SSM_CONV_STATE_LAYOUT": "DS"}, want: "DS"},
		"deployment over mode":     {user: map[string]string{"VLLM_SSM_CONV_STATE_LAYOUT": "SD"}, want: "SD"},
	} {
		t.Run(name, func(t *testing.T) {
			p := pdParams("prefill", LaunchConfig{}, ComputeNetwork{})
			p.TemplateEnv = tc.template
			resolved, err := ResolveLaunch(ResolveLaunchInput{Engine: engine, Params: p, ContainerOverrides: ContainerOverrides{Env: tc.user}})
			if err != nil {
				t.Fatal(err)
			}
			if got := resolved.Env["VLLM_SSM_CONV_STATE_LAYOUT"]; got != tc.want {
				t.Fatalf("VLLM_SSM_CONV_STATE_LAYOUT = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestVLLMPDRecomputePolicyAndRDMA(t *testing.T) {
	p := pdParams("decode", LaunchConfig{}, ComputeNetwork{Type: netplan.TypeHostRDMA, Interface: "eth0", HCAs: []string{"mlx5_0:1", "mlx5_1:1"}})
	p.PD.KVLoadFailure = "recompute"
	launch := BuildVLLM(p)
	var config map[string]any
	if err := json.Unmarshal([]byte(argValue(t, launch.Argv, "--kv-transfer-config")), &config); err != nil {
		t.Fatal(err)
	}
	if config["kv_load_failure_policy"] != "recompute" {
		t.Fatalf("policy = %v", config["kv_load_failure_policy"])
	}
	if launch.Env["UCX_NET_DEVICES"] != "mlx5_0:1" || launch.Env["UCX_TLS"] != "rc,cuda_copy,cuda_ipc,gdr_copy" {
		t.Fatalf("rdma env = %v", launch.Env)
	}
	if _, ok := launch.Env["UCX_TCP_BRIDGE_ENABLE"]; ok {
		t.Fatal("RDMA does not use the TCP bridge switch")
	}
	// On ROCm the CUDA transport list would leave UCX without a GPU copy
	// path; naming the HCA is enough for it to pick rocm_copy/rocm_ipc.
	amd := p
	amd.Vendor = "amd"
	if env := BuildVLLM(amd).Env; env["UCX_NET_DEVICES"] != "mlx5_0:1" || env["UCX_TLS"] != "" {
		t.Fatalf("amd rdma env = %v", env)
	}
	args := withComputeNetworkDockerArgs(nil, p)
	if !reflect.DeepEqual(args, []string{"--device", "/dev/infiniband", "--ulimit", "memlock=-1", "--cap-add", "IPC_LOCK"}) {
		t.Fatalf("docker args = %v", args)
	}
	// An ordinary single-node launch on the same network gets none of it.
	p.PD = nil
	if got := withComputeNetworkDockerArgs(nil, p); len(got) != 0 {
		t.Fatalf("non-PD single node docker args = %v", got)
	}
}

func TestVLLMPDComposesKVOffloadOnPrefillOnly(t *testing.T) {
	cpu := LaunchConfig{KVOffloadingSize: 2}
	prefill := BuildVLLM(pdParams("prefill", cpu, ComputeNetwork{}))
	want := `{"kv_connector":"MultiConnector","kv_connector_extra_config":{"connectors":[{"kv_connector":"NixlConnector","kv_role":"kv_producer"},{"kv_connector":"OffloadingConnector","kv_connector_extra_config":{"cpu_bytes_to_use":2147483648},"kv_role":"kv_both"}]},"kv_load_failure_policy":"fail","kv_role":"kv_producer"}`
	if got := argValue(t, prefill.Argv, "--kv-transfer-config"); got != want {
		t.Fatalf("prefill config =\n%s\nwant\n%s", got, want)
	}
	// The CPU tier flag must not appear beside the composed config: vLLM
	// would replace the NIXL connector with it.
	if countFlag(prefill.Argv, "--kv-offloading-size") != 0 || countFlag(prefill.Argv, "--kv-transfer-config") != 1 {
		t.Fatalf("argv = %v", prefill.Argv)
	}
	decode := BuildVLLM(pdParams("decode", cpu, ComputeNetwork{}))
	if got := argValue(t, decode.Argv, "--kv-transfer-config"); got != `{"kv_connector":"NixlConnector","kv_load_failure_policy":"fail","kv_role":"kv_consumer"}` {
		t.Fatalf("decode config = %s", got)
	}
	if countFlag(decode.Argv, "--kv-offloading-size") != 0 {
		t.Fatal("decode carries a KV cache tier")
	}

	disk := LaunchConfig{KVOffloadingSize: 4, KVDiskOffload: true, SharedKVDir: "/kv", KVOffloadReadThreads: 2, KVOffloadWriteThreads: 3}
	withDisk := BuildVLLM(pdParams("prefill", disk, ComputeNetwork{}))
	var config struct {
		Connector string `json:"kv_connector"`
		Extra     struct {
			Connectors []map[string]any `json:"connectors"`
		} `json:"kv_connector_extra_config"`
	}
	if err := json.Unmarshal([]byte(argValue(t, withDisk.Argv, "--kv-transfer-config")), &config); err != nil {
		t.Fatal(err)
	}
	if config.Connector != "MultiConnector" || len(config.Extra.Connectors) != 2 || config.Extra.Connectors[1]["kv_connector"] != "OffloadingConnector" {
		t.Fatalf("disk composition = %+v", config)
	}
	if got := PDRoleLaunchConfig(EngineVLLM, disk, "decode"); got.KVDiskOffload || got.SharedKVDir != "" || got.KVOffloadingSize != 0 {
		t.Fatalf("decode kept a tier: %+v", got)
	}
	if got := PDRoleLaunchConfig(EngineVLLM, disk, "prefill"); !reflect.DeepEqual(got, disk) {
		t.Fatal("prefill config changed")
	}
}

func TestVLLMWithoutPDKeepsDiskTierShape(t *testing.T) {
	q := LaunchConfig{KVOffloadingSize: 4, KVDiskOffload: true, SharedKVDir: "/kv"}
	launch := BuildVLLM(Params{ModelPath: "/model", APIPort: 1, Quick: q})
	if got, want := argValue(t, launch.Argv, "--kv-transfer-config"), diskKVTransferConfig(q); got != want {
		t.Fatalf("disk tier = %s want %s", got, want)
	}
	for _, key := range []string{"VLLM_NIXL_SIDE_CHANNEL_HOST", "UCX_NET_DEVICES", "VLLM_SSM_CONV_STATE_LAYOUT"} {
		if _, ok := launch.Env[key]; ok {
			t.Fatalf("non-PD launch has %s", key)
		}
	}
}

// The PPU IPC mode adds its scheduler flag and the PPU runner's IPC
// switches to both roles, keeps the flag once when a template states it, and
// sets no UCX device: no network carries its KV.
func TestSGLangPPUIPCRoleLaunch(t *testing.T) {
	engine, _ := SelectEngine("sglang")
	for _, role := range []string{"prefill", "decode"} {
		launch := engine.Build(Params{ModelPath: "/model", APIPort: 30000, TensorParallel: 1,
			ComputeNetwork: ComputeNetwork{Type: netplan.TypeHostEthernet, Interface: "eth1"},
			PD:             &PDLaunch{Mode: "sglang-ppu-ipc", Role: role, SideChannelHost: "10.0.0.5", SideChannelPort: 30001}})
		argv := launch.Entrypoint
		if argValue(t, argv, "--disaggregation-mode") != role || argValue(t, argv, "--disaggregation-transfer-backend") != "mooncake" || countFlag(argv, "--disable-overlap-schedule") != 1 {
			t.Fatalf("%s argv = %v", role, argv)
		}
		for key, want := range map[string]string{"SGLANG_PPU_PD_IPC": "1", "SGLANG_PPU_PD_SERIAL_TRANSFER": "1", "MC_FORCE_MNNVL": "1", "MC_USE_NVLINK_IPC": "1"} {
			if launch.Env[key] != want {
				t.Fatalf("%s env %s = %q (%v)", role, key, launch.Env[key], launch.Env)
			}
		}
		if _, ok := launch.Env["UCX_NET_DEVICES"]; ok {
			t.Fatalf("%s env names a UCX device: %v", role, launch.Env)
		}
		// The bootstrap calls go to the PD address, never through a proxy.
		if want := "localhost,127.0.0.1,::1,0.0.0.0,10.0.0.5"; launch.Env["NO_PROXY"] != want || launch.Env["no_proxy"] != want {
			t.Fatalf("%s no_proxy = %q / %q", role, launch.Env["NO_PROXY"], launch.Env["no_proxy"])
		}
	}
	stated := engine.Build(Params{ModelPath: "/model", APIPort: 30000, TensorParallel: 1, TemplateArgs: []string{"--disable-overlap-schedule"},
		PD: &PDLaunch{Mode: "sglang-ppu-ipc", Role: "decode"}})
	if countFlag(stated.Entrypoint, "--disable-overlap-schedule") != 1 {
		t.Fatalf("argv = %v", stated.Entrypoint)
	}
	nixl := engine.Build(Params{ModelPath: "/model", APIPort: 30000, TensorParallel: 1, PD: &PDLaunch{Mode: "sglang-nixl", Role: "decode"}})
	if countFlag(nixl.Entrypoint, "--disable-overlap-schedule") != 0 || nixl.Env["SGLANG_PPU_PD_IPC"] != "" {
		t.Fatalf("sglang-nixl got the PPU switches: %v %v", nixl.Entrypoint, nixl.Env)
	}
}

func TestSGLangPDRoleArgv(t *testing.T) {
	engine, _ := SelectEngine("sglang")
	network := ComputeNetwork{Type: netplan.TypeHostEthernet, Interface: "eth1"}
	prefill := engine.Build(Params{ModelPath: "/model", APIPort: 30000, TensorParallel: 1, ComputeNetwork: network,
		PD: &PDLaunch{Mode: "sglang-nixl", Role: "prefill", SideChannelHost: "10.0.0.5", SideChannelPort: 30001}})
	argv := prefill.Entrypoint
	if argValue(t, argv, "--disaggregation-mode") != "prefill" || argValue(t, argv, "--disaggregation-transfer-backend") != "nixl" || argValue(t, argv, "--disaggregation-bootstrap-port") != "30001" {
		t.Fatalf("prefill argv = %v", argv)
	}
	if argValue(t, argv, "--host") != "0.0.0.0" {
		t.Fatalf("prefill bootstrap server must be reachable: --host %s", argValue(t, argv, "--host"))
	}
	if prefill.Env["UCX_NET_DEVICES"] != "eth1" || prefill.Env["UCX_TCP_BRIDGE_ENABLE"] != "y" {
		t.Fatalf("env = %v", prefill.Env)
	}
	decode := engine.Build(Params{ModelPath: "/model", APIPort: 30002, TensorParallel: 1,
		PD: &PDLaunch{Mode: "sglang-nixl", Role: "decode"}})
	if argValue(t, decode.Entrypoint, "--disaggregation-mode") != "decode" || countFlag(decode.Entrypoint, "--disaggregation-bootstrap-port") != 0 || argValue(t, decode.Entrypoint, "--host") != "127.0.0.1" {
		t.Fatalf("decode argv = %v", decode.Entrypoint)
	}
	plain := engine.Build(Params{ModelPath: "/model", APIPort: 30000})
	if countFlag(plain.Entrypoint, "--disaggregation-mode") != 0 {
		t.Fatal("an ordinary SGLang launch got a disaggregation role")
	}
}

func TestVLLMAscendMooncakeLaunch(t *testing.T) {
	p := Params{ModelPath: "/model", APIPort: 30000, TensorParallel: 1, NNodes: 1, Vendor: "ascend",
		ComputeNetwork: ComputeNetwork{Interface: "br_ens3_36"},
		PD:             &PDLaunch{Mode: "vllm-ascend-mooncake", Role: "decode", SideChannelHost: "198.51.100.32", SideChannelPort: 30010, RoleTP: map[string]int{"prefill": 2, "decode": 1}}}
	launch := BuildVLLM(p)
	var config map[string]any
	if err := json.Unmarshal([]byte(argValue(t, launch.Argv, "--kv-transfer-config")), &config); err != nil {
		t.Fatal(err)
	}
	extra, _ := config["kv_connector_extra_config"].(map[string]any)
	prefill, _ := extra["prefill"].(map[string]any)
	if config["kv_connector"] != "MooncakeConnectorV1" || config["kv_role"] != "kv_consumer" || config["kv_port"] != "30010" || prefill["tp_size"] != float64(2) {
		t.Fatalf("config = %v", config)
	}
	// The connector reads the node address through vLLM's get_ip: the
	// loopback default of a single-node launch would hand peers 127.0.0.1.
	for key, want := range map[string]string{"VLLM_HOST_IP": "198.51.100.32", "HCCL_IF_IP": "198.51.100.32", "HCCL_SOCKET_IFNAME": "br_ens3_36", "TP_SOCKET_IFNAME": "br_ens3_36"} {
		if launch.Env[key] != want {
			t.Fatalf("env %s = %q, want %q", key, launch.Env[key], want)
		}
	}
	for _, key := range []string{"VLLM_NIXL_SIDE_CHANNEL_HOST", "UCX_NET_DEVICES", "VLLM_SSM_CONV_STATE_LAYOUT"} {
		if _, ok := launch.Env[key]; ok {
			t.Fatalf("Mooncake launch carries %s", key)
		}
	}
	engine, _ := SelectEngine("vllm")
	resolved, err := ResolveLaunch(ResolveLaunchInput{Engine: engine, Params: p})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range resolved.Mounts {
		if m.Target == "/etc/hccn.conf" && m.Source == "/etc/hccn.conf" && m.ReadOnly {
			found = true
		}
	}
	if !found {
		t.Fatalf("mounts = %+v", resolved.Mounts)
	}
}

func TestSGLangPPUMooncakeLaunch(t *testing.T) {
	engine, _ := SelectEngine("sglang")
	network := ComputeNetwork{Type: netplan.TypeHostRDMA, Interface: "bond0", HCAs: []string{"mlx5_0:1", "mlx5_1:1", "mlx5_0:2"}}
	for _, role := range []string{"prefill", "decode"} {
		p := Params{ModelPath: "/model", APIPort: 30000, TensorParallel: 1, ComputeNetwork: network,
			PD: &PDLaunch{Mode: "sglang-ppu-mooncake", Role: role, SideChannelHost: "10.0.0.5", SideChannelPort: 30001}}
		launch := engine.Build(p)
		argv := launch.Entrypoint
		if argValue(t, argv, "--disaggregation-transfer-backend") != "mooncake" || argValue(t, argv, "--disaggregation-ib-device") != "mlx5_0,mlx5_1" {
			t.Fatalf("%s argv = %v", role, argv)
		}
		// No same-host IPC switches: the KV goes over the NIC.
		for _, key := range []string{"SGLANG_PPU_PD_IPC", "MC_USE_NVLINK_IPC", "UCX_NET_DEVICES"} {
			if _, ok := launch.Env[key]; ok {
				t.Fatalf("%s env carries %s: %v", role, key, launch.Env)
			}
		}
		if launch.Env["SGLANG_ENABLE_HEALTH_ENDPOINT_GENERATION"] != "0" || launch.Env["NO_PROXY"] != "localhost,127.0.0.1,::1,0.0.0.0,10.0.0.5" {
			t.Fatalf("%s env = %v", role, launch.Env)
		}
		// The host RDMA network brings the devices and the locked memory.
		resolved, err := ResolveLaunch(ResolveLaunchInput{Engine: engine, Params: p})
		if err != nil {
			t.Fatal(err)
		}
		if !hasDockerArgValue(resolved.DockerArgs, "--device", "/dev/infiniband") || !hasDockerArgValue(resolved.DockerArgs, "--cap-add", "IPC_LOCK") {
			t.Fatalf("%s docker args = %v", role, resolved.DockerArgs)
		}
	}
	// Without HCAs the engine picks its own device.
	bare := engine.Build(Params{ModelPath: "/model", APIPort: 30000, TensorParallel: 1, ComputeNetwork: ComputeNetwork{Type: netplan.TypeHostRDMA},
		PD: &PDLaunch{Mode: "sglang-ppu-mooncake", Role: "decode"}})
	if countFlag(bare.Entrypoint, "--disaggregation-ib-device") != 0 {
		t.Fatalf("argv = %v", bare.Entrypoint)
	}
}

func TestVLLMPPUMooncakeLaunch(t *testing.T) {
	network := ComputeNetwork{Type: netplan.TypeHostRDMA, Interface: "bond0", HCAs: []string{"mlx5_0:1"}}
	config := func(role string) (map[string]any, ResolvedLaunch) {
		launch := BuildVLLM(Params{ModelPath: "/model", APIPort: 30000, TensorParallel: 1, NNodes: 1, Vendor: "ppu", ComputeNetwork: network,
			PD: &PDLaunch{Mode: "vllm-ppu-mooncake", Role: role, SideChannelHost: "10.0.0.5", SideChannelPort: 30001}})
		var c map[string]any
		if err := json.Unmarshal([]byte(argValue(t, launch.Argv, "--kv-transfer-config")), &c); err != nil {
			t.Fatal(err)
		}
		return c, ResolvedLaunch{Env: launch.Env}
	}
	prefill, prefillLaunch := config("prefill")
	extra, _ := prefill["kv_connector_extra_config"].(map[string]any)
	// No pinned engine_id: the gateway reads the engine's own from its
	// bootstrap server (pdmode.Mode.SynthesizedHandoff).
	if prefill["kv_connector"] != "MooncakeConnector" || prefill["kv_role"] != "kv_producer" || prefill["engine_id"] != nil ||
		extra["mooncake_protocol"] != "rdma" || extra["device_name"] != "mlx5_0" {
		t.Fatalf("prefill config = %v", prefill)
	}
	for key, want := range map[string]string{"VLLM_MOONCAKE_BOOTSTRAP_PORT": "30001", "VLLM_HOST_IP": "10.0.0.5", "NO_PROXY": "localhost,127.0.0.1,::1,0.0.0.0,10.0.0.5"} {
		if prefillLaunch.Env[key] != want {
			t.Fatalf("prefill env %s = %q (%v)", key, prefillLaunch.Env[key], prefillLaunch.Env)
		}
	}
	decode, decodeLaunch := config("decode")
	if decode["kv_role"] != "kv_consumer" || decode["engine_id"] != nil {
		t.Fatalf("decode config = %v", decode)
	}
	if _, ok := decodeLaunch.Env["VLLM_MOONCAKE_BOOTSTRAP_PORT"]; ok {
		t.Fatalf("decode env = %v", decodeLaunch.Env)
	}
	// Other modes keep their configs.
	nixl := BuildVLLM(Params{ModelPath: "/model", APIPort: 30000, TensorParallel: 1, NNodes: 1, ComputeNetwork: network,
		PD: &PDLaunch{Mode: "vllm-nixl", Role: "prefill", SideChannelHost: "10.0.0.5", SideChannelPort: 30001}})
	if raw := argValue(t, nixl.Argv, "--kv-transfer-config"); strings.Contains(raw, "engine_id") || strings.Contains(raw, "device_name") {
		t.Fatalf("vllm-nixl config = %s", raw)
	}
}

// TestVLLMPDLaunchesStayPDWithKVOffload: every vLLM PD mode's launch is a PD
// request in both roles, also when the prefill composes the PD connector with
// a KV tier in one MultiConnector.
func TestVLLMPDLaunchesStayPDWithKVOffload(t *testing.T) {
	for _, name := range pdmode.ForEngine(EngineVLLM) {
		mode, _ := pdmode.Lookup(name)
		for _, role := range mode.Roles {
			for _, quick := range []LaunchConfig{{}, {KVOffloadingSize: 8}, {KVOffloadingSize: 16, KVDiskOffload: true, SharedKVDir: "/kv-cache"}} {
				p := pdParams(role, quick, ComputeNetwork{})
				p.PD.Mode, p.Vendor = name, mode.Vendors[0]
				launch, err := ResolveLaunch(ResolveLaunchInput{Engine: vLLMEngine{}, Params: p, TargetValidation: CollectTargetCapabilities})
				if err != nil {
					t.Fatalf("%s %s: %v", name, role, err)
				}
				if f := launch.RequestedFeatures; !f.PD || !f.CommandKnown {
					t.Fatalf("%s %s %+v: requested features %+v", name, role, quick, f)
				}
			}
		}
	}
}
