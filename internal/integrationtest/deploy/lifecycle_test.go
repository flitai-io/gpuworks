package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/testenv"
)

// switchableDriver fails every launch while failing is set, like an engine
// whose image is broken until the operator fixes it.
type switchableDriver struct {
	*testenv.FakeDriver
	failing atomic.Bool
	runs    atomic.Int32
}

func (d *switchableDriver) Run(ctx context.Context, spec runtime.WorkloadSpec) (string, error) {
	d.runs.Add(1)
	if d.failing.Load() {
		return "", errors.New("engine image is broken")
	}
	return d.FakeDriver.Run(ctx, spec)
}

// lifecycleEnv is one node with two GPUs, a present model and an ordinary
// two-replica deployment created through the public API.
func lifecycleEnv(t *testing.T, driver runtime.Driver) (*testenv.Env, *testenv.APIClient, uint) {
	t.Helper()
	env := testenv.New(t)
	env.AddNode("lifecycle-node", testenv.WithGPUs(2, testenv.GPUSpec{Model: "A100", VRAMMB: 24576}), testenv.WithDriver(driver))
	env.WaitNodeOnline(t, "lifecycle-node")
	nodes, err := env.Store().ListNodes()
	if err != nil || len(nodes) != 1 {
		t.Fatalf("nodes=%+v err=%v", nodes, err)
	}
	model, err := env.Store().UpsertModel(&store.Model{
		Source: "huggingface", SourceID: "lifecycle-model", Revision: "main", Name: "lifecycle-model",
		Arch: "Qwen3ForCausalLM", WeightBytes: 1 << 30,
		Meta: `{"model_type":"qwen3","num_hidden_layers":1,"hidden_size":256,"num_attention_heads":8,"num_key_value_heads":8,"max_position_embeddings":2048}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.Store().UpsertModelFile(&store.ModelFile{ModelID: model.ID, NodeID: nodes[0].ID, State: proto.MFPresent, Path: testenv.ModelDir(t, "lifecycle")}); err != nil {
		t.Fatal(err)
	}
	api := env.API(t)
	body, _ := json.Marshal(map[string]any{
		"model_id": model.ID, "node_uuid": nodes[0].UUID, "served_name": "lifecycle-model",
		"replicas": 2, "quick_params": map[string]any{"max_model_len": 256},
	})
	resp, err := api.PostJSON("/api/v1/deployments", body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		data, _ := io.ReadAll(resp.Body)
		t.Fatalf("create status=%d body=%s", resp.StatusCode, data)
	}
	var dep struct {
		ID uint `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&dep); err != nil || dep.ID == 0 {
		t.Fatalf("create response: %+v %v", dep, err)
	}
	return env, api, dep.ID
}

func postAction(t *testing.T, api *testenv.APIClient, path string, body string, want int) []byte {
	t.Helper()
	resp, err := api.PostJSON(path, []byte(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != want {
		t.Fatalf("POST %s = %d %s, want %d", path, resp.StatusCode, data, want)
	}
	return data
}

// waitRunning waits until the deployment has exactly n running instances,
// none of them in avoid, and returns their UUIDs.
func waitRunning(t *testing.T, env *testenv.Env, depID uint, n int, avoid []string) []string {
	t.Helper()
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		insts, _ := env.Store().InstancesForDeployment(depID)
		var running []string
		fresh := true
		for _, inst := range insts {
			if inst.State == proto.InstRunning {
				running = append(running, inst.UUID)
				fresh = fresh && !slices.Contains(avoid, inst.UUID)
			}
		}
		if len(running) == n && len(insts) == n && fresh {
			return running
		}
		time.Sleep(25 * time.Millisecond)
	}
	insts, _ := env.Store().InstancesForDeployment(depID)
	t.Fatalf("deployment %d did not reach %d fresh running instances: %+v", depID, n, insts)
	return nil
}

// Stop, start and restart through the public API on a real server and agent:
// restarting all at once replaces every instance, stop removes them and keeps
// the configuration, start brings the same deployment back.
func TestStopStartAndRestartAnOrdinaryDeployment(t *testing.T) {
	env, api, depID := lifecycleEnv(t, testenv.NewFakeDriver("docker"))
	first := waitRunning(t, env, depID, 2, nil)

	postAction(t, api, fmt.Sprintf("/api/v1/deployments/%d/restart", depID), `{"strategy":"all"}`, http.StatusAccepted)
	restarted := waitRunning(t, env, depID, 2, first)
	if event, found, _ := env.Store().LatestScheduleEvent(depID); !found || event.Code != store.EventDeploymentRestarted {
		t.Fatalf("restart event = %+v found=%v", event, found)
	}

	postAction(t, api, fmt.Sprintf("/api/v1/deployments/%d/stop", depID), ``, http.StatusOK)
	deadline := time.Now().Add(30 * time.Second)
	for {
		insts, _ := env.Store().InstancesForDeployment(depID)
		if len(insts) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("stopped deployment kept instances: %+v", insts)
		}
		time.Sleep(25 * time.Millisecond)
	}
	// A restart of a stopped deployment is refused; start brings it back.
	postAction(t, api, fmt.Sprintf("/api/v1/deployments/%d/restart", depID), ``, http.StatusConflict)
	data := postAction(t, api, fmt.Sprintf("/api/v1/deployments/%d/start", depID), ``, http.StatusOK)
	var started struct {
		DesiredState string `json:"desired_state"`
		Replicas     int    `json:"replicas"`
	}
	if err := json.Unmarshal(data, &started); err != nil || started.DesiredState != "active" || started.Replicas != 2 {
		t.Fatalf("start response %s (%v)", data, err)
	}
	waitRunning(t, env, depID, 2, append(first, restarted...))
}

// A replica that spent its retry budget is named in the deployment response;
// once the cause is fixed, retrying that replica rebuilds it without touching
// the running one.
func TestOrdinaryReplicaRetryAfterTheBudgetIsSpent(t *testing.T) {
	driver := &switchableDriver{FakeDriver: testenv.NewFakeDriver("docker")}
	env, api, depID := lifecycleEnv(t, driver)
	env.SetControllerRetryPolicy(50*time.Millisecond, 2)
	running := waitRunning(t, env, depID, 2, nil)

	driver.failing.Store(true)
	// The runtime reports the first replica's engine gone: its unit rebuilds,
	// fails twice and gives up.
	insts, _ := env.Store().InstancesForDeployment(depID)
	var victim store.Instance
	for _, inst := range insts {
		if inst.UUID == running[0] {
			victim = inst
		}
	}
	if err := env.Store().SetInstanceFailed(victim.UUID, proto.RuntimeFailure{Code: proto.ErrorContainerExited}, "engine exited"); err != nil {
		t.Fatal(err)
	}
	var exhausted struct {
		RetryExhausted *struct {
			Index          int    `json:"index"`
			FailedAttempts int    `json:"failed_attempts"`
			LastFailure    string `json:"last_failure"`
		} `json:"retry_exhausted"`
	}
	deadline := time.Now().Add(45 * time.Second)
	for {
		resp, err := api.Get(fmt.Sprintf("/api/v1/deployments/%d", depID))
		if err != nil {
			t.Fatal(err)
		}
		_ = json.NewDecoder(resp.Body).Decode(&exhausted)
		resp.Body.Close()
		if exhausted.RetryExhausted != nil {
			break
		}
		if time.Now().After(deadline) {
			all, _ := env.Store().InstancesForDeployment(depID)
			t.Fatalf("the replica never gave up: %+v", all)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if exhausted.RetryExhausted.FailedAttempts != 2 {
		t.Fatalf("exhausted = %+v, want 2 failed attempts", exhausted.RetryExhausted)
	}
	runsAtGiveUp := driver.runs.Load()
	time.Sleep(300 * time.Millisecond)
	if driver.runs.Load() != runsAtGiveUp {
		t.Fatal("the controller kept launching a replica that gave up")
	}

	driver.failing.Store(false)
	postAction(t, api, fmt.Sprintf("/api/v1/deployments/%d/units/retry", depID), fmt.Sprintf(`{"role":"","index":%d}`, exhausted.RetryExhausted.Index), http.StatusAccepted)
	after := waitRunning(t, env, depID, 2, []string{victim.UUID})
	if !slices.Contains(after, running[1]) {
		t.Fatalf("the healthy replica %s was replaced: %v", running[1], after)
	}
}
