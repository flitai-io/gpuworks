package deploy

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/testenv"
)

// A container that exits after its instance reached running is reported by
// the agent and rebuilt by the controller like any failed attempt, within the
// same retry budget: here two attempts, the second ending in an OOM kill.
func TestRunningInstanceWhoseContainerExitsIsRebuiltWithinTheRetryBudget(t *testing.T) {
	env := testenv.New(t)
	env.SetControllerRetryPolicy(100*time.Millisecond, 2)
	node := env.AddNode("exit-node", testenv.WithGPUs(1, testenv.GPUSpec{Model: "A100", VRAMMB: 24576}), testenv.WithDriverName("docker"))
	env.WaitNodeOnline(t, "exit-node")
	nodes, err := env.Store().ListNodes()
	if err != nil || len(nodes) != 1 {
		t.Fatalf("nodes = %+v, err=%v", nodes, err)
	}
	model, err := env.Store().UpsertModel(&store.Model{Source: "huggingface", SourceID: "zstackai/qwen3-4b-fake", Revision: "main", Name: "qwen3-4b-fake", Arch: "Qwen3ForCausalLM", WeightBytes: 1 << 30, Meta: `{"model_type":"qwen3","architectures":["Qwen3ForCausalLM"],"num_hidden_layers":1,"hidden_size":256,"num_attention_heads":8,"num_key_value_heads":8,"max_position_embeddings":2048}`})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.Store().UpsertModelFile(&store.ModelFile{ModelID: model.ID, NodeID: nodes[0].ID, State: proto.MFPresent, Path: testenv.ModelDir(t, "fake")}); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{"model_id": model.ID, "node_uuid": nodes[0].UUID, "served_name": "fake", "quick_params": map[string]any{"max_model_len": 512}})
	resp, err := env.API(t).PostJSON("/api/v1/deployments", body)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create deployment status = %d", resp.StatusCode)
	}

	first := waitRunningInstance(t, env, nil)
	// The engine dies on its own (no one asked the agent to stop it).
	node.Driver.Exit(containerNameOf(first.UUID), 3)
	failed := waitInstanceState(t, env, first.UUID, proto.InstFailed)
	// Not an OOM kill (exit 3), but the failure carries the last error the
	// engine printed, the fake engine's CUDA OOM line.
	if failed.Message != "container exited while running (code 3): ERROR torch.cuda.OutOfMemoryError: CUDA out of memory" {
		t.Fatalf("first failure = %q", failed.Message)
	}
	second := waitRunningInstance(t, env, map[string]bool{first.UUID: true})

	// The fake engine's log carries a CUDA OOM, so exit 137 reads as an OOM kill.
	node.Driver.Exit(containerNameOf(second.UUID), 137)
	failed = waitInstanceState(t, env, second.UUID, proto.InstFailed)
	if failed.Message != "container OOM-killed while running (code 137)" {
		t.Fatalf("second failure = %q", failed.Message)
	}
	deadline := time.Now().Add(30 * time.Second)
	exhausted := false
	for time.Now().Before(deadline) && !exhausted {
		events, _ := env.Store().InstanceEventsByUUID(second.UUID, 0)
		for _, ev := range events {
			exhausted = exhausted || ev.Code == store.EventUnitRetryExhausted
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !exhausted {
		t.Fatal("the unit did not record that it spent its retry budget")
	}
	// The budget is spent: nothing is rebuilt, and each attempt failed once.
	time.Sleep(500 * time.Millisecond)
	instances, _ := env.Store().ListInstances()
	if len(instances) != 2 {
		t.Fatalf("instances = %+v, want the two failed attempts only", instances)
	}
	for _, inst := range instances {
		if inst.State != proto.InstFailed {
			t.Fatalf("instance %s = %s, want failed", inst.UUID, inst.State)
		}
		events, _ := env.Store().InstanceEventsByUUID(inst.UUID, 0)
		reports := 0
		for _, ev := range events {
			if ev.Code == "INSTANCE_STATUS_UPDATED" && strings.Contains(ev.Data, `"state":"failed"`) {
				reports++
			}
		}
		if reports > 1 {
			t.Fatalf("instance %s recorded the exit %d times: %+v", inst.UUID, reports, events)
		}
	}
}

func containerNameOf(uuid string) string {
	if len(uuid) > 12 {
		uuid = uuid[:12]
	}
	return "gpuworks-" + uuid
}

func waitRunningInstance(t *testing.T, env *testenv.Env, skip map[string]bool) store.Instance {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		instances, _ := env.Store().ListInstances()
		for _, inst := range instances {
			if !skip[inst.UUID] && inst.State == proto.InstRunning {
				return inst
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	instances, _ := env.Store().ListInstances()
	t.Fatalf("no new running instance: %+v", instances)
	return store.Instance{}
}

func waitInstanceState(t *testing.T, env *testenv.Env, uuid, state string) store.Instance {
	t.Helper()
	// One liveness interval (15 s) plus the report's trip to the server.
	deadline := time.Now().Add(45 * time.Second)
	var last store.Instance
	for time.Now().Before(deadline) {
		if inst, err := env.Store().InstanceByUUID(uuid); err == nil {
			last = *inst
			if inst.State == state {
				return *inst
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("instance %s stayed %s (%q), want %s", uuid, last.State, last.Message, state)
	return store.Instance{}
}
