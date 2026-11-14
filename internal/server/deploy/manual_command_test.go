package deploy

import (
	"slices"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/netplan"
)

func manualInput(params Params, command string) ResolveLaunchInput {
	return ResolveLaunchInput{Engine: vLLMEngine{}, Params: params,
		ContainerOverrides: ContainerOverrides{CommandMode: string(CommandManual), CmdDisplay: command}}
}

func manualScript(t *testing.T, input ResolveLaunchInput) string {
	t.Helper()
	launch, err := ResolveLaunch(input)
	if err != nil {
		t.Fatalf("ResolveLaunch: %v", err)
	}
	if !slices.Equal(launch.Entrypoint, []string{"/bin/sh"}) || len(launch.Argv) != 2 {
		t.Fatalf("manual launch = %q %q, want /bin/sh -c script", launch.Entrypoint, launch.Argv)
	}
	return strings.TrimPrefix(launch.Argv[1], "set -a\n")
}

// A manual script stored before manual commands shared the template's
// variables must start exactly as it did: names this launch does not state are
// the shell's, whatever they are called.
func TestManualCommandLeavesUnstatedNamesToTheShell(t *testing.T) {
	single := Params{ModelPath: "/model", APIPort: 30000, AssignedGPUs: 1, TensorParallel: 1, NNodes: 1}
	for _, tc := range []struct{ name, script, want string }{
		{"multi-node name on a single node", `exec vllm serve /model --master ${GPUWORKS_MASTER_ADDR}:${GPUWORKS_MASTER_PORT} --advertise ${GPUWORKS_NODE_ADDR}`, `exec vllm serve /model --master ${GPUWORKS_MASTER_ADDR}:${GPUWORKS_MASTER_PORT} --advertise ${GPUWORKS_NODE_ADDR}`},
		{"PD name without PD", `case "${GPUWORKS_PD_ROLE}" in *) exec vllm serve /model ;; esac`, `case "${GPUWORKS_PD_ROLE}" in *) exec vllm serve /model ;; esac`},
		{"the script's own variable", "FOO=1\nexec vllm serve /model --seed ${FOO}", "FOO=1\nexec vllm serve /model --seed ${FOO}"},
		{"bare platform names are the script's own", "MODEL_PATH=/weights\nexec vllm serve ${MODEL_PATH} --metrics-port ${API_PORT} --rank ${NODE_RANK}", "MODEL_PATH=/weights\nexec vllm serve ${MODEL_PATH} --metrics-port ${API_PORT} --rank ${NODE_RANK}"},
		{"unbraced and lowercase names", `exec vllm serve /model --rank $NODE_RANK --x ${node_rank} --y ${NODE_RANK:-0}`, `exec vllm serve /model --rank $NODE_RANK --x ${node_rank} --y ${NODE_RANK:-0}`},
		{"stated names are the launch's facts", `exec vllm serve ${GPUWORKS_MODEL_PATH} --rank ${GPUWORKS_NODE_RANK}/${GPUWORKS_NNODES} --headless ${GPUWORKS_HEADLESS}`, `exec vllm serve /model --rank 0/1 --headless 0`},
		{"port flag and port variable agree", `exec vllm serve /model --port 8000 --metrics-port ${GPUWORKS_API_PORT}`, `exec vllm serve /model --port 30000 --metrics-port 30000`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := manualScript(t, manualInput(single, tc.script)); got != tc.want {
				t.Fatalf("script =\n%s\nwant\n%s", got, tc.want)
			}
		})
	}
}

// The environment of a single-node manual launch is the script's own business
// and stays empty; a rank of a group or a PD instance keeps what only the
// platform knows about it.
func TestManualCommandSystemEnvironment(t *testing.T) {
	single := Params{ModelPath: "/model", APIPort: 30000, AssignedGPUs: 1, TensorParallel: 1, NNodes: 1}
	launch, err := ResolveLaunch(manualInput(single, "exec vllm serve /model"))
	if err != nil {
		t.Fatal(err)
	}
	if len(launch.Env) != 0 {
		t.Fatalf("single-node manual environment = %v, want none", launch.Env)
	}
	follower := Params{ModelPath: "/model", AssignedGPUs: 1, TensorParallel: 2, NNodes: 2, NodeRank: 1, Headless: true,
		MasterAddr: "10.0.0.1", MasterPort: 29500, NodeAddress: "10.0.0.2"}
	input := manualInput(follower, "exec vllm serve /model --node-rank ${GPUWORKS_NODE_RANK}")
	input.ContainerOverrides.Env = map[string]string{"HF_HUB_OFFLINE": "0"}
	launch, err = ResolveLaunch(input)
	if err != nil {
		t.Fatal(err)
	}
	if launch.Env["VLLM_HOST_IP"] != "10.0.0.2" {
		t.Fatalf("follower VLLM_HOST_IP = %q, want its own address", launch.Env["VLLM_HOST_IP"])
	}
	if launch.Env["HF_HUB_OFFLINE"] != "0" {
		t.Fatalf("the deployment's own env lost to the system layer: %v", launch.Env)
	}
	pd := pdParams("prefill", LaunchConfig{}, ComputeNetwork{Type: netplan.TypeHostEthernet, Interface: "eth1"})
	launch, err = ResolveLaunch(manualInput(pd, `exec vllm serve /model --role ${GPUWORKS_PD_ROLE} --side ${GPUWORKS_PD_SIDE_CHANNEL_HOST}:${GPUWORKS_PD_SIDE_CHANNEL_PORT}`))
	if err != nil {
		t.Fatal(err)
	}
	if launch.Env["VLLM_NIXL_SIDE_CHANNEL_PORT"] != "30001" {
		t.Fatalf("PD side channel env = %v", launch.Env)
	}
	if got, want := launch.Argv[1], "set -a\nexec vllm serve /model --role prefill --side 10.0.0.5:30001"; got != want {
		t.Fatalf("PD script = %q, want %q", got, want)
	}
}

func TestManualCommandJSONArgvIsExpandedPerRank(t *testing.T) {
	follower := Params{ModelPath: "/model", AssignedGPUs: 1, TensorParallel: 2, NNodes: 2, NodeRank: 1, Headless: true,
		MasterAddr: "10.0.0.1", MasterPort: 29500, NodeAddress: "10.0.0.2"}
	launch, err := ResolveLaunch(manualInput(follower, `["worker","--rank","${GPUWORKS_NODE_RANK}","--master","${GPUWORKS_MASTER_ADDR}:${GPUWORKS_MASTER_PORT}","--keep","${FOO}"]`))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := launchArguments(launch), []string{"worker", "--rank", "1", "--master", "10.0.0.1:29500", "--keep", "${FOO}"}; !slices.Equal(got, want) {
		t.Fatalf("argv = %q, want %q", got, want)
	}
}

// A preview whose placement is not settled has no addresses yet; the script
// keeps the references instead of failing.
func TestManualCommandPreviewKeepsUnsettledReferences(t *testing.T) {
	leader := Params{ModelPath: "/model", APIPort: 30000, TensorParallel: 2, NNodes: 2}
	input := manualInput(leader, "exec vllm serve /model --node-rank ${GPUWORKS_NODE_RANK} --master-addr ${GPUWORKS_MASTER_ADDR}")
	input.AllowUnresolved = true
	if got, want := manualScript(t, input), "exec vllm serve /model --node-rank 0 --master-addr ${GPUWORKS_MASTER_ADDR}"; got != want {
		t.Fatalf("preview script = %q, want %q", got, want)
	}
}

func TestManualCommandRankBlind(t *testing.T) {
	for _, tc := range []struct {
		name    string
		script  string
		nnodes  int
		pd      bool
		missing []string
	}{
		{"single node needs nothing", "exec vllm serve /model", 1, false, nil},
		{"multi-node without a rank", "exec vllm serve /model --nnodes ${GPUWORKS_NNODES} --master-addr ${GPUWORKS_MASTER_ADDR}", 2, false, []string{"GPUWORKS_NODE_RANK"}},
		{"multi-node by rank", "exec vllm serve /model --node-rank ${GPUWORKS_NODE_RANK}", 2, false, nil},
		{"multi-node by headless", `[ "${GPUWORKS_HEADLESS}" = 1 ] && exec worker; exec leader`, 3, false, nil},
		{"shell spelling is not a reference", "exec vllm serve /model --node-rank $NODE_RANK", 2, false, []string{"GPUWORKS_NODE_RANK"}},
		{"the bare name is not a reference", "exec vllm serve /model --node-rank ${NODE_RANK}", 2, false, []string{"GPUWORKS_NODE_RANK"}},
		{"PD without a role", "exec vllm serve /model --port ${GPUWORKS_API_PORT}", 1, true, []string{"GPUWORKS_PD_ROLE"}},
		{"PD by role", `case "${GPUWORKS_PD_ROLE}" in prefill) exec a ;; *) exec b ;; esac`, 1, true, nil},
		{"multi-node PD needs both", `case "${GPUWORKS_PD_ROLE}" in prefill) exec a ;; *) exec b ;; esac`, 2, true, []string{"GPUWORKS_NODE_RANK"}},
		{"multi-node PD blind to both", "exec vllm serve /model", 2, true, []string{"GPUWORKS_PD_ROLE", "GPUWORKS_NODE_RANK"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ManualCommandRankBlind(tc.script, tc.nnodes, tc.pd); !slices.Equal(got, tc.missing) {
				t.Fatalf("missing = %v, want %v", got, tc.missing)
			}
		})
	}
}

func twoNodeInputs(mutate func(rank int, p *Params)) []ResolveLaunchInput {
	var ranks []ResolveLaunchInput
	for rank := range 2 {
		p := Params{ModelPath: "/model", ServedName: "qwen", AssignedGPUs: 1, TensorParallel: 2, NNodes: 2, NodeRank: rank, Headless: rank > 0,
			MasterAddr: "10.0.0.1", MasterPort: 29500, NodeAddress: "10.0.0." + string(rune('1'+rank))}
		if rank == 0 {
			p.APIPort = 30000
		}
		if mutate != nil {
			mutate(rank, &p)
		}
		ranks = append(ranks, ResolveLaunchInput{Engine: vLLMEngine{}, Params: p})
	}
	return ranks
}

// The draft's shape, as a user first sees it.
func TestManualCommandDraftGolden(t *testing.T) {
	draft, ok, err := ManualCommandDraft(twoNodeInputs(nil))
	if err != nil || !ok {
		t.Fatalf("draft ok=%v err=%v", ok, err)
	}
	want := `set -eu
case "${GPUWORKS_HEADLESS}" in
  0)
    exec vllm serve /model --served-model-name qwen --host 127.0.0.1 --port ${GPUWORKS_API_PORT} --tensor-parallel-size 2 --distributed-executor-backend mp --nnodes 2 --node-rank ${GPUWORKS_NODE_RANK} --master-addr ${GPUWORKS_MASTER_ADDR} --master-port ${GPUWORKS_MASTER_PORT}
    ;;
  1)
    exec vllm serve /model --tensor-parallel-size 2 --distributed-executor-backend mp --nnodes 2 --node-rank ${GPUWORKS_NODE_RANK} --headless --master-addr ${GPUWORKS_MASTER_ADDR} --master-port ${GPUWORKS_MASTER_PORT}
    ;;
  *)
    echo "unexpected launch class ${GPUWORKS_HEADLESS}" >&2
    exit 1
    ;;
esac`
	if draft != want {
		t.Fatalf("draft =\n%s\nwant\n%s", draft, want)
	}
}

// No draft is better than one that is right for the previewed ranks only.
func TestManualCommandDraftUnavailable(t *testing.T) {
	t.Run("ranks of one class differ in a fact no variable carries", func(t *testing.T) {
		var ranks []ResolveLaunchInput
		for index, hca := range []string{"mlx5_0", "mlx5_1"} {
			network := ComputeNetwork{Type: netplan.TypeHostRDMA, HCAs: []string{hca}}
			p := Params{ModelPath: "/model", APIPort: 30000 + index, AssignedGPUs: 1, TensorParallel: 1, NNodes: 1, ComputeNetwork: network,
				PD: &PDLaunch{Mode: "sglang-ppu-mooncake", Role: "decode", SideChannelHost: "10.0.0.5", SideChannelPort: 30001}}
			ranks = append(ranks, ResolveLaunchInput{Engine: sglangEngine{}, Params: p})
		}
		concrete, err := ResolveLaunch(ranks[0])
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Contains(launchArguments(concrete), "mlx5_0") {
			t.Fatalf("the fixture no longer puts the HCA in the command: %q", launchArguments(concrete))
		}
		if draft, ok, err := ManualCommandDraft(ranks); ok || err != nil {
			t.Fatalf("draft ok=%v err=%v:\n%s", ok, err, draft)
		}
	})
	t.Run("the engine command holds text the expansion would rewrite", func(t *testing.T) {
		ranks := twoNodeInputs(func(_ int, p *Params) {
			p.ExtraArgs = []string{"--chat-template", "${GPUWORKS_MODEL_PATH}/template.jinja"}
		})
		if draft, ok, err := ManualCommandDraft(ranks); ok || err != nil {
			t.Fatalf("draft ok=%v err=%v:\n%s", ok, err, draft)
		}
	})
	t.Run("no ranks", func(t *testing.T) {
		if _, ok, err := ManualCommandDraft(nil); ok || err != nil {
			t.Fatalf("draft ok=%v err=%v", ok, err)
		}
	})
}
