package deploy

import (
	"reflect"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/netplan"
)

func TestComputeNetworkEnvByType(t *testing.T) {
	multi := func(n ComputeNetwork) Params { return Params{NNodes: 2, ComputeNetwork: n} }
	cases := []struct {
		name   string
		params Params
		want   map[string]string
	}{
		{"single node ignores the network", Params{NNodes: 1, ComputeNetwork: ComputeNetwork{Type: netplan.TypeHostRDMA, Interface: "bond0"}}, nil},
		{"single-node Kunlun TP uses the real interface", Params{NNodes: 1, Vendor: "kunlunxin", TensorParallel: 2, ComputeNetwork: ComputeNetwork{Type: netplan.TypeHostEthernet, Interface: "br_eno1_31"}}, map[string]string{
			"NCCL_SOCKET_IFNAME": "br_eno1_31", "GLOO_SOCKET_IFNAME": "br_eno1_31",
		}},
		{"single-node Kunlun PP uses the real interface", Params{NNodes: 1, Vendor: "kunlunxin", TensorParallel: 1, PipelineParallel: 2, ComputeNetwork: ComputeNetwork{Type: netplan.TypeHostEthernet, Interface: "br_eno1_31"}}, map[string]string{
			"NCCL_SOCKET_IFNAME": "br_eno1_31", "GLOO_SOCKET_IFNAME": "br_eno1_31",
		}},
		{"single-node Kunlun DP uses the real interface", Params{NNodes: 1, Vendor: "kunlunxin", TensorParallel: 1, DataParallel: 2, ComputeNetwork: ComputeNetwork{Type: netplan.TypeHostEthernet, Interface: "br_eno1_31"}}, map[string]string{
			"NCCL_SOCKET_IFNAME": "br_eno1_31", "GLOO_SOCKET_IFNAME": "br_eno1_31",
		}},
		{"single-node Kunlun single card keeps loopback", Params{NNodes: 1, Vendor: "kunlunxin", TensorParallel: 1, PipelineParallel: 1, DataParallel: 1, ComputeNetwork: ComputeNetwork{Type: netplan.TypeHostEthernet, Interface: "br_eno1_31"}}, nil},
		{"single-node Ascend keeps existing behavior", Params{NNodes: 1, Vendor: "ascend", TensorParallel: 2, ComputeNetwork: ComputeNetwork{Type: netplan.TypeAcceleratorRDMA, Interface: "br_ens3_36"}}, nil},
		{"no network keeps automatic selection", Params{NNodes: 2}, nil},
		{"ethernet", multi(ComputeNetwork{Type: netplan.TypeHostEthernet, Interface: "ens3f0"}), map[string]string{
			"NCCL_SOCKET_IFNAME": "ens3f0", "GLOO_SOCKET_IFNAME": "ens3f0", "NCCL_IB_DISABLE": "1",
		}},
		{"ethernet without a matched interface", multi(ComputeNetwork{Type: netplan.TypeHostEthernet}), map[string]string{"NCCL_IB_DISABLE": "1"}},
		{"ib multi-rail", multi(ComputeNetwork{Type: netplan.TypeHostRDMA, Interface: "bond0", HCAs: []string{"mlx5_0:1", "mlx5_1:1"}}), map[string]string{
			"NCCL_SOCKET_IFNAME": "bond0", "GLOO_SOCKET_IFNAME": "bond0", "NCCL_IB_DISABLE": "0", "NCCL_NET": "IB",
			"NCCL_IB_HCA": "mlx5_0:1,mlx5_1:1", "UCX_NET_DEVICES": "mlx5_0:1",
		}},
		{"roce with no matched hca still forces IB", multi(ComputeNetwork{Type: netplan.TypeHostRDMA}), map[string]string{"NCCL_IB_DISABLE": "0", "NCCL_NET": "IB"}},
		// HCCL builds its communicator over the host address and interface it
		// is told; the cards are not named, and nothing NCCL reads is set.
		{"accelerator cards", Params{NNodes: 2, Vendor: "ascend", NodeAddress: "198.51.100.31", ComputeNetwork: ComputeNetwork{Type: netplan.TypeAcceleratorRDMA, Interface: "br_ens3_36", HCAs: []string{"npu3", "npu4"}, LinkLayer: "roce"}}, map[string]string{
			"HCCL_IF_IP": "198.51.100.31", "HCCL_SOCKET_IFNAME": "br_ens3_36", "GLOO_SOCKET_IFNAME": "br_ens3_36",
		}},
		{"accelerator cards without a bootstrap", Params{NNodes: 2, Vendor: "ascend", ComputeNetwork: ComputeNetwork{Type: netplan.TypeAcceleratorRDMA, HCAs: []string{"npu3"}}}, map[string]string{}},
		{"accelerator HCCS", Params{NNodes: 2, Vendor: "ascend", NodeAddress: "198.51.100.31", ComputeNetwork: ComputeNetwork{Type: netplan.TypeAcceleratorHCCS, Interface: "br_ens3_36", HCAs: []string{"npu3", "npu4"}}}, map[string]string{
			"GLOO_SOCKET_IFNAME": "br_ens3_36", "HCCL_SOCKET_IFNAME": "br_ens3_36", "ASCEND_GLOBAL_RESOURCE_CONFIG": `{"comm_resource_config.protocol_desc":["hccs:device"]}`, "ACL_OP_INIT_MODE": "1", "ASCEND_ENABLE_USE_FABRIC_MEM": "1",
		}},
		// Which variables say it is the vendor's business: a vendor whose
		// cards own no ports has none, and gets only the rendezvous interface.
		{"accelerator network, vendor without card ports", Params{NNodes: 2, Vendor: "nvidia", NodeAddress: "10.0.0.1", ComputeNetwork: ComputeNetwork{Type: netplan.TypeAcceleratorRDMA, Interface: "eth0"}}, map[string]string{"GLOO_SOCKET_IFNAME": "eth0"}},
		{"accelerator cards on a single node", Params{NNodes: 1, NodeAddress: "198.51.100.31", ComputeNetwork: ComputeNetwork{Type: netplan.TypeAcceleratorRDMA, Interface: "br_ens3_36"}}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := computeNetworkEnv(tc.params); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("env = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestComputeNetworkDockerArgs(t *testing.T) {
	ib := Params{NNodes: 2, ComputeNetwork: ComputeNetwork{Type: netplan.TypeHostRDMA}}
	got := withComputeNetworkDockerArgs([]string{"--shm-size", "16g"}, ib)
	want := []string{"--shm-size", "16g", "--device", "/dev/infiniband", "--ulimit", "memlock=-1", "--cap-add", "IPC_LOCK"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("args = %v", got)
	}
	// Options the image or user already set are not repeated, in either form.
	got = withComputeNetworkDockerArgs([]string{"--device=/dev/infiniband", "--cap-add", "IPC_LOCK"}, ib)
	if want := []string{"--device=/dev/infiniband", "--cap-add", "IPC_LOCK", "--ulimit", "memlock=-1"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("dedup args = %v", got)
	}
	// The cards' ports are not under /dev/infiniband.
	accelerator := ComputeNetwork{Type: netplan.TypeAcceleratorRDMA, HCAs: []string{"npu3"}}
	for _, p := range []Params{{NNodes: 1, ComputeNetwork: ComputeNetwork{Type: netplan.TypeHostRDMA}}, {NNodes: 2, ComputeNetwork: ComputeNetwork{Type: netplan.TypeHostEthernet}}, {NNodes: 2},
		{NNodes: 2, ComputeNetwork: accelerator}, {NNodes: 1, PD: &PDLaunch{Mode: "vllm-ascend-mooncake", Role: "decode"}, ComputeNetwork: accelerator}} {
		if got := withComputeNetworkDockerArgs([]string{"--x"}, p); !reflect.DeepEqual(got, []string{"--x"}) {
			t.Fatalf("non-RDMA args changed: %v", got)
		}
	}
}

func TestResolveLaunchCarriesComputeNetwork(t *testing.T) {
	for _, engineName := range []string{EngineVLLM, EngineSGLang} {
		t.Run(engineName, func(t *testing.T) {
			eng, err := SelectEngine(engineName)
			if err != nil {
				t.Fatal(err)
			}
			launch, err := ResolveLaunch(ResolveLaunchInput{
				Engine: eng,
				Params: Params{
					ModelPath: "/model", TensorParallel: 8, PipelineParallel: 2, NNodes: 2, NodeRank: 1, Headless: true,
					MasterAddr: "10.20.1.11", MasterPort: 29500, NodeAddress: "10.20.1.12",
					Quick:          materializedConfig(t, engineName),
					ComputeNetwork: ComputeNetwork{Type: netplan.TypeHostRDMA, Interface: "bond0", HCAs: []string{"mlx5_0:1", "mlx5_1:1"}},
				},
				ContainerOverrides: ContainerOverrides{Env: map[string]string{"NCCL_IB_HCA": "mlx5_0:1"}},
			})
			if err != nil {
				t.Fatal(err)
			}
			for key, want := range map[string]string{
				"NCCL_SOCKET_IFNAME": "bond0", "GLOO_SOCKET_IFNAME": "bond0", "NCCL_NET": "IB",
				"UCX_NET_DEVICES": "mlx5_0:1",
				// The user layer still wins over the platform's rails.
				"NCCL_IB_HCA": "mlx5_0:1",
			} {
				if got := launch.Env[key]; got != want {
					t.Fatalf("%s = %q, want %q (env %v)", key, got, want, launch.Env)
				}
			}
			if got := launch.EnvLayers.System["NCCL_IB_HCA"]; got != "mlx5_0:1,mlx5_1:1" {
				t.Fatalf("system layer NCCL_IB_HCA = %q", got)
			}
			if !hasDockerArgValue(launch.DockerArgs, "--device", "/dev/infiniband") || !hasDockerArgValue(launch.DockerArgs, "--cap-add", "IPC_LOCK") {
				t.Fatalf("docker args = %v", launch.DockerArgs)
			}
		})
	}
}

func TestComputeNetworkMounts(t *testing.T) {
	accelerator := ComputeNetwork{Type: netplan.TypeAcceleratorRDMA, Interface: "br0", HCAs: []string{"npu3"}}
	want := []Mount{{Source: "/etc/hccn.conf", Target: "/etc/hccn.conf", ReadOnly: true, Purpose: "compute_network"}}
	for name, tc := range map[string]struct {
		params Params
		want   []Mount
	}{
		"multi-node rank on cards":         {Params{NNodes: 2, Vendor: "ascend", ComputeNetwork: accelerator}, want},
		"vendor without card ports":        {Params{NNodes: 2, Vendor: "nvidia", ComputeNetwork: accelerator}, nil},
		"single node ignores the network":  {Params{NNodes: 1, Vendor: "ascend", ComputeNetwork: accelerator}, nil},
		"multi-node rank on host RDMA":     {Params{NNodes: 2, ComputeNetwork: ComputeNetwork{Type: netplan.TypeHostRDMA}}, nil},
		"multi-node rank on host Ethernet": {Params{NNodes: 2, ComputeNetwork: ComputeNetwork{Type: netplan.TypeHostEthernet}}, nil},
		"multi-node rank without network":  {Params{NNodes: 2}, nil},
		"PD instance on cards":             {Params{NNodes: 1, Vendor: "ascend", PD: &PDLaunch{Mode: "vllm-ascend-mooncake", Role: "decode"}, ComputeNetwork: accelerator}, want},
		"PD instance on HCCS":              {Params{NNodes: 1, Vendor: "ascend", PD: &PDLaunch{Mode: "vllm-ascend-mooncake", Role: "decode"}, ComputeNetwork: ComputeNetwork{Type: netplan.TypeAcceleratorHCCS}}, want},
		// One host has no network to be on, and HCCL still reads the file.
		"HCCL PD instance on one host": {Params{NNodes: 1, Vendor: "ascend", PD: &PDLaunch{Mode: "vllm-ascend-mooncake", Role: "decode"}}, want},
		"UCX PD instance on host RDMA": {Params{NNodes: 1, PD: &PDLaunch{Mode: "vllm-nixl", Role: "decode"}, ComputeNetwork: ComputeNetwork{Type: netplan.TypeHostRDMA}}, nil},
	} {
		if got := computeNetworkMounts(tc.params); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: mounts = %+v, want %+v", name, got, tc.want)
		}
	}
}

// An ordinary multi-node rank on an accelerator network gets the three things
// the two 910B4 hosts needed to build an HCCL communicator: hccn.conf, the
// host address and the host interface.
func TestResolveLaunchOnAcceleratorNetwork(t *testing.T) {
	eng, err := SelectEngine(EngineVLLM)
	if err != nil {
		t.Fatal(err)
	}
	params := Params{
		ModelPath: "/model", TensorParallel: 2, NNodes: 2, NodeRank: 1, Headless: true,
		MasterAddr: "198.51.100.31", MasterPort: 29500, NodeAddress: "198.51.100.32", Vendor: "ascend",
		Quick:          materializedConfig(t, EngineVLLM),
		ComputeNetwork: ComputeNetwork{Type: netplan.TypeAcceleratorRDMA, Interface: "br_ens3_36", HCAs: []string{"npu3", "npu4"}, LinkLayer: "roce"},
	}
	launch, err := ResolveLaunch(ResolveLaunchInput{Engine: eng, Params: params})
	if err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{"HCCL_IF_IP": "198.51.100.32", "HCCL_SOCKET_IFNAME": "br_ens3_36", "GLOO_SOCKET_IFNAME": "br_ens3_36"} {
		if got := launch.Env[key]; got != want {
			t.Fatalf("%s = %q, want %q (env %v)", key, got, want, launch.Env)
		}
		if got := launch.EnvLayers.System[key]; got != want {
			t.Fatalf("system layer %s = %q, want %q", key, got, want)
		}
	}
	for _, key := range []string{"NCCL_IB_DISABLE", "NCCL_NET", "NCCL_IB_HCA", "UCX_NET_DEVICES"} {
		if value, ok := launch.Env[key]; ok {
			t.Fatalf("accelerator launch carries %s=%q", key, value)
		}
	}
	mounts := 0
	for _, m := range launch.Mounts {
		if m.Target == "/etc/hccn.conf" {
			mounts++
			if m.Source != "/etc/hccn.conf" || !m.ReadOnly || m.Purpose != "compute_network" {
				t.Fatalf("hccn.conf mount = %+v", m)
			}
		}
	}
	if mounts != 1 {
		t.Fatalf("hccn.conf mounted %d times: %+v", mounts, launch.Mounts)
	}
	if hasDockerArgValue(launch.DockerArgs, "--device", "/dev/infiniband") {
		t.Fatalf("docker args = %v", launch.DockerArgs)
	}
	// The user layer still overrides the platform's choice.
	launch, err = ResolveLaunch(ResolveLaunchInput{Engine: eng, Params: params, ContainerOverrides: ContainerOverrides{Env: map[string]string{"HCCL_SOCKET_IFNAME": "eth9"}}})
	if err != nil {
		t.Fatal(err)
	}
	if launch.Env["HCCL_SOCKET_IFNAME"] != "eth9" {
		t.Fatalf("user override lost: %q", launch.Env["HCCL_SOCKET_IFNAME"])
	}
	// The same rank on the management network gets none of it.
	params.ComputeNetwork = ComputeNetwork{Type: netplan.TypeHostEthernet, Interface: "br_ens3_36"}
	launch, err = ResolveLaunch(ResolveLaunchInput{Engine: eng, Params: params})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := launch.Env["HCCL_IF_IP"]; ok || launch.Env["NCCL_IB_DISABLE"] != "1" {
		t.Fatalf("host Ethernet env = %v", launch.Env)
	}
}
