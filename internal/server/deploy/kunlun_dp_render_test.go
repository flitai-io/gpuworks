package deploy

import (
	"os"
	"slices"
	"strconv"
	"testing"

	"gopkg.in/yaml.v3"
)

// These launches use the Qwen3-0.6B Kunlun template's vLLM args and its
// two-card/single-card placements. DP is an engine dimension, not a gpuworks
// deployment replica count.
func TestKunlunQwenSmallDPRender(t *testing.T) {
	raw, err := os.ReadFile("../../../build/inference-templates/kunlun-p800/catalog/models/qwen/qwen3-0.6b/model.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var model struct {
		Specs []struct {
			Args []string `yaml:"args"`
		} `yaml:"specs"`
	}
	if err := yaml.Unmarshal(raw, &model); err != nil || len(model.Specs) != 1 {
		t.Fatalf("Kunlun Qwen template: specs=%d, err=%v", len(model.Specs), err)
	}
	for _, tc := range []struct {
		name                   string
		nodes, rank, localGPUs int
		headless               bool
		address                string
	}{
		{name: "one node", nodes: 1, localGPUs: 2},
		{name: "leader", nodes: 2, localGPUs: 1, address: "192.0.2.10"},
		{name: "follower", nodes: 2, rank: 1, localGPUs: 1, headless: true, address: "192.0.2.11"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// vLLM 0.25.1 engine/arg_utils.py infers local DP size and DP
			// rank from nnodes/node_rank, and uses master_addr as the DP
			// coordinator address. Passing start-rank here would select a
			// different load-balancing mode for this dense model.
			p := Params{Vendor: "kunlunxin", ModelPath: "/model", APIPort: 30000, TensorParallel: 1, PipelineParallel: 1,
				AssignedGPUs: tc.localGPUs, NNodes: tc.nodes, NodeRank: tc.rank, Headless: tc.headless,
				MasterAddr: "192.0.2.10", MasterPort: 29500, NodeAddress: tc.address,
				TemplateArgs: model.Specs[0].Args,
			}
			p.Quick.DP = 2
			engine, err := SelectEngine("vllm")
			if err != nil {
				t.Fatal(err)
			}
			launch, err := ResolveLaunch(ResolveLaunchInput{Engine: engine, Params: p})
			if err != nil {
				t.Fatal(err)
			}
			argv := launch.Argv
			has := func(flag, value string) bool {
				for i := 0; i+1 < len(argv); i++ {
					if argv[i] == flag && argv[i+1] == value {
						return true
					}
				}
				return false
			}
			if !has("--tensor-parallel-size", "1") || !has("--data-parallel-size", "2") {
				t.Fatalf("parallel argv: %v", argv)
			}
			if err := validateEngineTopology(vLLMEngine{}, launch, p); err != nil {
				t.Fatalf("topology: %v; argv=%v", err, argv)
			}
			if tc.nodes == 2 {
				if !has("--nnodes", "2") || !has("--node-rank", strconv.Itoa(tc.rank)) || !has("--master-addr", "192.0.2.10") || !has("--master-port", "29500") || launch.Env["VLLM_HOST_IP"] != tc.address {
					t.Fatalf("distributed launch: %+v", launch)
				}
				if !has("--distributed-executor-backend", "mp") || launch.Env["GLOO_SOCKET_IFNAME"] != "" || launch.Env["NCCL_SOCKET_IFNAME"] != "" {
					t.Fatalf("distributed backend/network selection: %+v", launch)
				}
				if slices.Contains(argv, "--headless") != tc.headless {
					t.Fatalf("headless argv: %v", argv)
				}
			} else if slices.Contains(argv, "--nnodes") {
				t.Fatalf("single-node argv: %v", argv)
			}
			for _, flag := range []string{"--data-parallel-rank", "--data-parallel-start-rank", "--data-parallel-size-local", "--data-parallel-external-lb", "--data-parallel-hybrid-lb"} {
				if slices.Contains(argv, flag) {
					t.Fatalf("unexpected explicit DP mode %s: %v", flag, argv)
				}
			}
		})
	}
}
