package planning

import (
	"encoding/json"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// A multi-node deployment stored with a manual command that references no
// platform variable predates the rule evaluation and create enforce
// (MANUAL_COMMAND_RANK_BLIND). The controller's launch path must keep
// resolving it: every rank runs the command as written, now with the system
// environment a rank needs.
func TestStoredRankBlindManualCommandStillLaunches(t *testing.T) {
	st, err := store.Open("file:manual-rank-blind?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	script := "vllm serve ${MODEL_PATH} --tensor-parallel-size 2 --nnodes 2"
	overrides, err := json.Marshal(deploy.ContainerOverrides{CommandMode: string(deploy.CommandManual), CmdDisplay: script})
	if err != nil {
		t.Fatal(err)
	}
	dep := &store.Deployment{ID: 7, Image: "vllm/vllm-openai:v0.25.1", ContainerOverrides: string(overrides)}
	for rank := range 2 {
		req := LaunchRequest{
			Engine: "vllm", Model: &store.Model{ID: 1, WeightBytes: 1 << 30}, Quick: launchConfig(t, deploy.EngineVLLM),
			Node:      &store.Node{ID: uint(rank + 1), UUID: "n", OS: "linux", Arch: "amd64", RuntimeDriver: "docker", GPUVendor: "nvidia", GPUs: []store.GPU{{Index: 0, UUID: "g0", Name: "H100", Vendor: "nvidia", ArchID: "sm_90", MemoryTotal: 80 << 30}}},
			Source:    LaunchSourceFromDeployment(dep),
			Topology:  DistTopology{NNodes: 2, NodeRank: rank, Headless: rank > 0, NodeAddress: "192.0.2.1" + string(rune('0'+rank))},
			Placement: Placement{TP: 2}, MasterAddr: "192.0.2.10", MasterPort: 29500, APIPort: 31000, ModelMount: "/model",
		}
		plan, err := PlanLaunch(t.Context(), st, storeNetworks(st), req)
		if err != nil {
			t.Fatalf("rank %d: %v", rank, err)
		}
		if !plan.Output.Result.OK {
			t.Fatalf("rank %d not scheduled: %+v", rank, plan.Output.Result)
		}
		launch := plan.Resolved
		command := strings.Join(append(append([]string(nil), launch.Entrypoint...), launch.Argv...), " ")
		if launch.CommandMode != string(deploy.CommandManual) || !strings.Contains(command, script) {
			t.Fatalf("rank %d command = %q (mode %s), want the stored script unchanged (bare names are the shell's)", rank, command, launch.CommandMode)
		}
		if launch.Env["VLLM_HOST_IP"] != req.Topology.NodeAddress {
			t.Fatalf("rank %d env = %v, want the rank's system environment kept", rank, launch.Env)
		}
	}
}
