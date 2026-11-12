package deploy

import (
	"encoding/json"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/netplan"
)

// vllm-mooncake (upstream vLLM's MooncakeConnector on the Hygon image): the
// prefill instance starts a bootstrap server on its extra port, every
// instance hands peers its node address through vLLM's get_ip, and the
// transfer engine runs RDMA on a host RDMA network and TCP on any other.
func TestVLLMMooncakeLaunch(t *testing.T) {
	rdma := ComputeNetwork{Type: netplan.TypeHostRDMA, Interface: "eth0", HCAs: []string{"mlx5_0:1"}}
	for _, c := range []struct {
		name, role string
		network    ComputeNetwork
		config     string
		bootstrap  string
	}{
		{"prefill tcp", "prefill", ComputeNetwork{Type: netplan.TypeHostEthernet, Interface: "br0"},
			`{"kv_connector":"MooncakeConnector","kv_connector_extra_config":{"mooncake_protocol":"tcp"},"kv_load_failure_policy":"fail","kv_role":"kv_producer"}`, "30001"},
		{"decode tcp on one host", "decode", ComputeNetwork{},
			`{"kv_connector":"MooncakeConnector","kv_connector_extra_config":{"mooncake_protocol":"tcp"},"kv_load_failure_policy":"fail","kv_role":"kv_consumer"}`, ""},
		{"prefill rdma", "prefill", rdma,
			`{"kv_connector":"MooncakeConnector","kv_connector_extra_config":{"device_name":"mlx5_0","mooncake_protocol":"rdma"},"kv_load_failure_policy":"fail","kv_role":"kv_producer"}`, "30001"},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := pdParams(c.role, LaunchConfig{}, c.network)
			p.Vendor = "hygon"
			p.PD.Mode = "vllm-mooncake"
			launch := BuildVLLM(p)
			if got := argValue(t, launch.Argv, "--kv-transfer-config"); got != c.config {
				t.Fatalf("config = %s", got)
			}
			if launch.Env["VLLM_HOST_IP"] != "10.0.0.5" {
				t.Fatalf("VLLM_HOST_IP = %q", launch.Env["VLLM_HOST_IP"])
			}
			if got, ok := launch.Env["VLLM_MOONCAKE_BOOTSTRAP_PORT"]; got != c.bootstrap || ok != (c.bootstrap != "") {
				t.Fatalf("VLLM_MOONCAKE_BOOTSTRAP_PORT = %q (set %v)", got, ok)
			}
			for _, key := range []string{"VLLM_NIXL_SIDE_CHANNEL_HOST", "VLLM_NIXL_SIDE_CHANNEL_PORT", "UCX_TLS", "VLLM_SSM_CONV_STATE_LAYOUT"} {
				if _, ok := launch.Env[key]; ok {
					t.Fatalf("Mooncake launch carries %s", key)
				}
			}
		})
	}
	// KV offload on the prefill role composes with the connector, which keeps
	// its protocol inside the MultiConnector.
	p := pdParams("prefill", LaunchConfig{KVOffloadingSize: 8}, ComputeNetwork{})
	p.PD.Mode = "vllm-mooncake"
	var config struct {
		Connector string `json:"kv_connector"`
		Extra     struct {
			Connectors []map[string]any `json:"connectors"`
		} `json:"kv_connector_extra_config"`
	}
	if err := json.Unmarshal([]byte(argValue(t, BuildVLLM(p).Argv, "--kv-transfer-config")), &config); err != nil {
		t.Fatal(err)
	}
	if config.Connector != "MultiConnector" || len(config.Extra.Connectors) != 2 || config.Extra.Connectors[0]["kv_connector"] != "MooncakeConnector" {
		t.Fatalf("config = %+v", config)
	}
	if extra, _ := config.Extra.Connectors[0]["kv_connector_extra_config"].(map[string]any); extra["mooncake_protocol"] != "tcp" {
		t.Fatalf("connector = %v", config.Extra.Connectors[0])
	}
}

func TestMooncakeProtocol(t *testing.T) {
	if MooncakeProtocol(ComputeNetwork{Type: netplan.TypeHostRDMA, HCAs: []string{"mlx5_0:1"}}) != "rdma" {
		t.Fatal("host RDMA")
	}
	for _, n := range []ComputeNetwork{{}, {Type: netplan.TypeHostEthernet, Interface: "eth0"}} {
		if MooncakeProtocol(n) != "tcp" {
			t.Fatalf("%+v", n)
		}
	}
}

// A model with recurrent layers (Qwen3.5's gated delta net) under
// vllm-mooncake needs vLLM's hybrid KV cache manager on, which vLLM turns off
// whenever a connector is set: both roles get the flag, a model without such
// layers and a mode that does not ask get none, and a deployment that states
// either polarity keeps it.
func TestVLLMMooncakeHybridKVCacheManager(t *testing.T) {
	const flag = "--no-disable-hybrid-kv-cache-manager"
	count := func(argv []string, want string) int {
		n := 0
		for _, arg := range argv {
			if arg == want {
				n++
			}
		}
		return n
	}
	for _, c := range []struct {
		name      string
		mode      string
		recurrent bool
		extra     []string
		want      int
	}{
		{"hybrid prefill", "vllm-mooncake", true, nil, 1},
		{"full attention model", "vllm-mooncake", false, nil, 0},
		{"mode without the requirement", "vllm-nixl", true, nil, 0},
		{"user disables it", "vllm-mooncake", true, []string{"--disable-hybrid-kv-cache-manager"}, 0},
		{"user enables it", "vllm-mooncake", true, []string{flag}, 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			for _, role := range []string{"prefill", "decode"} {
				p := pdParams(role, LaunchConfig{}, ComputeNetwork{})
				p.Vendor = "hygon"
				p.PD.Mode = c.mode
				p.PD.RecurrentLayers = c.recurrent
				p.ExtraArgs = c.extra
				argv := BuildVLLM(p).Argv
				if got := count(argv, flag); got != c.want {
					t.Fatalf("%s: %s appears %d times, want %d: %v", role, flag, got, c.want, argv)
				}
				if c.name == "user disables it" && count(argv, "--disable-hybrid-kv-cache-manager") != 1 {
					t.Fatalf("%s: the user's --disable-hybrid-kv-cache-manager is gone: %v", role, argv)
				}
			}
		})
	}
}
