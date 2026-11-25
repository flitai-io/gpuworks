package deploy

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/testenv"
)

// multiNodeGenerations waits until the deployment has exactly want running
// multi-node replicas (leader plus follower of one generation each) and no
// other live instance, and returns the leaders. Failed attempts stay as
// evidence and are skipped: in-process nodes share one loopback, so a
// follower's ephemeral port can collide with a leader's port, which a real
// cluster of separate hosts never sees.
func multiNodeGenerations(t *testing.T, env *testenv.Env, depID uint, want int) []store.Instance {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		all, _ := env.Store().InstancesForDeployment(depID)
		var insts []store.Instance
		for _, inst := range all {
			if inst.State != proto.InstFailed {
				insts = append(insts, inst)
			}
		}
		var leaders []store.Instance
		complete := len(insts) == 2*want
		for _, inst := range insts {
			complete = complete && inst.State == proto.InstRunning
			if !inst.Headless {
				leaders = append(leaders, inst)
			}
		}
		if complete && len(leaders) == want {
			for _, leader := range leaders {
				followers := 0
				for _, inst := range insts {
					if inst.Headless && inst.DistGroup == leader.DistGroup && inst.DistAttempt == leader.DistAttempt {
						followers++
					}
				}
				if followers != 1 {
					complete = false
				}
			}
			if complete {
				return leaders
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	insts, _ := env.Store().InstancesForDeployment(depID)
	t.Fatalf("deployment %d did not reach %d complete multi-node replicas: %+v", depID, want, insts)
	return nil
}

func masterAddrOf(t *testing.T, inst store.Instance) string {
	t.Helper()
	var spec proto.InstanceDesiredState
	if err := json.Unmarshal([]byte(inst.DesiredStateJSON), &spec); err != nil {
		t.Fatal(err)
	}
	argv := spec.ResolvedLaunch.Argv
	for i := 0; i+1 < len(argv); i++ {
		if argv[i] == "--master-addr" {
			return argv[i+1]
		}
	}
	return ""
}

// A two-node deployment grows to two multi-node replicas through the public
// API and back: the replica it had keeps running, the new one is its own
// generation rendezvousing at its own leader, requests reach both leaders,
// and the replica taken off the list drains and goes with its follower.
func TestMultiNodeDeploymentScalesToTwoReplicas(t *testing.T) {
	env := testenv.New(t)
	gates := &gatedEngines{}
	nodes := []*testenv.Node{
		env.AddNode("replica-node-a", testenv.WithGPUs(2, testenv.GPUSpec{Model: "A100", VRAMMB: 24576}), testenv.WithDriverName("docker"), testenv.WithManagementAddress("10.42.1.10")),
		env.AddNode("replica-node-b", testenv.WithGPUs(2, testenv.GPUSpec{Model: "A100", VRAMMB: 24576}), testenv.WithDriverName("docker"), testenv.WithManagementAddress("10.42.1.11")),
	}
	model, err := env.Store().UpsertModel(&store.Model{
		Source: "huggingface", SourceID: "zstackai/qwen3-4b-replicas", Revision: "main",
		Name: "qwen3-4b-replicas", Arch: "Qwen3ForCausalLM", WeightBytes: 1 << 30,
		Meta: `{"model_type":"qwen3","architectures":["Qwen3ForCausalLM"],"num_hidden_layers":2,"hidden_size":256,"num_attention_heads":8,"num_key_value_heads":8,"max_position_embeddings":2048}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, node := range nodes {
		node.Driver.SetEngineHandler(gates.handler)
		env.WaitNodeOnline(t, node.Name)
		row, _ := env.Store().NodeByUUID(node.UUID)
		if _, err := env.Store().UpsertModelFile(&store.ModelFile{ModelID: model.ID, NodeID: row.ID, State: proto.MFPresent, Path: testenv.ModelDir(t, "replicas")}); err != nil {
			t.Fatal(err)
		}
	}
	api := env.API(t)
	payload, _ := json.Marshal(map[string]any{
		"model_id": model.ID, "node_uuid": nodes[0].UUID, "node_uuids": []string{nodes[0].UUID, nodes[1].UUID},
		"served_name": "multi-replicas", "engine": "vllm", "image": "gpuworks/fake-engine:test",
		"nnodes": 2, "master_addr": "10.42.1.10", "master_port": 29500,
		"placement": map[string]any{"tp": 1, "pp": 2}, "quick_params": map[string]any{"max_model_len": 512},
	})
	resp, err := api.PostJSON("/api/v1/deployments", payload)
	if err != nil {
		t.Fatal(err)
	}
	var created dto.Deployment
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated || json.Unmarshal(raw, &created) != nil {
		t.Fatalf("create = %d %s", resp.StatusCode, raw)
	}
	original := multiNodeGenerations(t, env, created.ID, 1)[0]

	scale := func(body string) dto.Deployment {
		t.Helper()
		resp, err := api.Do(mustRequest(t, http.MethodPut, env.URL()+fmt.Sprintf("/api/v1/deployments/%d/replicas", created.ID), body))
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		var out dto.Deployment
		if resp.StatusCode != http.StatusOK || json.Unmarshal(raw, &out) != nil {
			t.Fatalf("scale %s = %d %s", body, resp.StatusCode, raw)
		}
		return out
	}
	grown := scale(fmt.Sprintf(`{"units":[{"node_uuid":%q,"followers":[{"node_uuid":%q}]},{"node_uuid":%q,"followers":[{"node_uuid":%q}]}]}`, nodes[0].UUID, nodes[1].UUID, nodes[1].UUID, nodes[0].UUID))
	if grown.Replicas != 2 || len(grown.Units) != 2 {
		t.Fatalf("grown = replicas %d units %+v", grown.Replicas, grown.Units)
	}
	leaders := multiNodeGenerations(t, env, created.ID, 2)
	byNode := map[uint]store.Instance{}
	for _, leader := range leaders {
		byNode[leader.NodeID] = leader
	}
	rowA, _ := env.Store().NodeByUUID(nodes[0].UUID)
	rowB, _ := env.Store().NodeByUUID(nodes[1].UUID)
	first, second := byNode[rowA.ID], byNode[rowB.ID]
	if first.UUID != original.UUID {
		t.Fatalf("the replica the deployment had was replaced: %s -> %s", original.UUID, first.UUID)
	}
	if second.UUID == "" || first.DistGroup == second.DistGroup {
		t.Fatalf("replicas share a group or one is missing: %+v %+v", first, second)
	}
	if got := masterAddrOf(t, first); got != "10.42.1.10" {
		t.Fatalf("first replica rendezvous at %q", got)
	}
	if got := masterAddrOf(t, second); got != "10.42.1.11" {
		t.Fatalf("second replica rendezvous at %q, want its own leader", got)
	}

	// Two requests in flight at once reach both leaders.
	key, err := env.Store().CreateAPIKey("multi-replicas", 0)
	if err != nil {
		t.Fatal(err)
	}
	gates.hold()
	done := make(chan int, 2)
	for i := 0; i < 2; i++ {
		go func() {
			status, _ := chatModel(t, env, key, "multi-replicas")
			done <- status
		}()
	}
	deadline := time.Now().Add(10 * time.Second)
	for len(gates.arrived()) < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("requests did not reach the engines: %v", gates.arrived())
		}
		time.Sleep(10 * time.Millisecond)
	}
	gates.open()
	for i := 0; i < 2; i++ {
		if status := <-done; status != http.StatusOK {
			t.Fatalf("chat = %d", status)
		}
	}
	arrived := gates.arrived()
	if !slices.Contains(arrived, containerOf(first.UUID)) || !slices.Contains(arrived, containerOf(second.UUID)) {
		t.Fatalf("requests reached %v, want both leaders %s and %s", arrived, first.UUID, second.UUID)
	}

	// Back to the first replica: the second drains and goes with its
	// follower.
	scale(fmt.Sprintf(`{"units":[{"node_uuid":%q,"followers":[{"node_uuid":%q}]}]}`, nodes[0].UUID, nodes[1].UUID))
	if left := multiNodeGenerations(t, env, created.ID, 1); left[0].UUID != first.UUID {
		t.Fatalf("remaining replica = %s, want %s", left[0].UUID, first.UUID)
	}
}
