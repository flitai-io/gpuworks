//go:build multiproc

package l2

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/testenv/apiwire"
)

// TestProcessAScheduledWakeDrivesTheRealEngine proves the scheduling
// automation across the real process boundary: a deployment an operator put
// to sleep is woken by a scheduled wake at its minute, recorded on the
// timeline. The engine is the fake engine in a real
// container, so the levels are the engine's own. Skipped without Docker.
//
//	go test -tags=multiproc -run TestProcessAScheduledWakeDrivesTheRealEngine -timeout 15m ./test/l2/
func TestProcessAScheduledWakeDrivesTheRealEngine(t *testing.T) {
	base, adminToken, seed, modelID := startDockerAutomationRig(t, "automation")

	deployBody, _ := json.Marshal(map[string]any{
		"model_id": modelID, "node_uuid": seed, "served_name": "automation",
		"engine": "vllm", "image": "gpuworks/fake-engine:test", "sleepable": true,
	})
	deployResp, deployData := authedDo(t, http.MethodPost, base, adminToken, "/api/v1/deployments", deployBody)
	if deployResp.StatusCode != http.StatusCreated {
		t.Fatalf("create deployment status=%d body=%s", deployResp.StatusCode, deployData)
	}
	var created struct {
		ID uint `json:"id"`
	}
	if err := json.Unmarshal(deployData, &created); err != nil {
		t.Fatal(err)
	}

	instance := waitInstance(t, base, adminToken, func(inst processInstance) bool {
		return inst.DeploymentID == created.ID && inst.State == "running" && inst.ContainerID != "" && len(inst.Argv) > 0
	})
	// The fake engine must not outlive this test: it keeps answering /health
	// on the instance port, and the next recording-runtime case in the same
	// job requires that port to be silent (its instances must never reach
	// running). The delete asks the agent to reap the container while the
	// agent process is still up; docker rm -f is the last resort. Cleanups
	// run LIFO, so this runs before the server and agent are stopped.
	t.Cleanup(func() {
		deleteDeployment(t, base, adminToken, created.ID)
		if instance.ContainerID != "" {
			_ = exec.Command("docker", "rm", "-f", instance.ContainerID).Run()
		}
	})

	// An operator's sleep, which only a wake ends: the schedule's.
	if resp, data := authedDo(t, http.MethodPost, base, adminToken, fmt.Sprintf("/api/v1/deployments/%d/sleep?level=1", created.ID), nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("operator sleep status=%d body=%s", resp.StatusCode, data)
	}
	waitSleepLevelWithin(t, base, adminToken, instance.UUID, 1, time.Minute)
	if !engineSleeping(t, instance.APIPort) {
		t.Fatalf("server reports the sleep but the engine never slept")
	}

	// A wake every day at the next whole minute, in the server's zone (the
	// server runs on this host).
	at := time.Now().Add(time.Minute)
	if at.Second() > 45 {
		at = at.Add(time.Minute)
	}
	order, _ := json.Marshal(map[string]any{
		"action": "wake", "weekdays": []int{0, 1, 2, 3, 4, 5, 6}, "time": at.Format("15:04"), "timezone": "",
	})
	resp, data := authedDo(t, http.MethodPost, base, adminToken, fmt.Sprintf("/api/v1/deployments/%d/schedule", created.ID), order)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create schedule status=%d body=%s", resp.StatusCode, data)
	}
	waitSleepLevelWithin(t, base, adminToken, instance.UUID, 0, 3*time.Minute)
	if engineSleeping(t, instance.APIPort) {
		t.Fatalf("server reports the scheduled wake but the engine is still sleeping")
	}
	if params := waitEvent(t, base, adminToken, created.ID, "AUTO_WAKE"); params["trigger"] != "schedule" {
		t.Fatalf("AUTO_WAKE params = %v, want trigger schedule", params)
	}
}

// waitSleepLevelWithin is waitSleepLevel with a longer deadline, for orders
// the automation gives on its own time.
func waitSleepLevelWithin(t *testing.T, base, token, instanceUUID string, level int, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		_, data := authedDo(t, http.MethodGet, base, token, "/api/v1/instances", nil)
		var instances []processInstance
		if err := apiwire.UnmarshalList(data, &instances); err != nil {
			t.Fatalf("decode instance list: %v (%s)", err, data)
		}
		for _, instance := range instances {
			if instance.UUID == instanceUUID && instance.SleepLevel == level {
				return
			}
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("instance %s did not reach sleep level %d within %s", instanceUUID, level, within)
}

// waitEvent waits for an event of code on the deployment's timeline and
// returns its params.
func waitEvent(t *testing.T, base, token string, deploymentID uint, code string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		resp, data := authedDo(t, http.MethodGet, base, token, fmt.Sprintf("/api/v1/deployments/%d/events?limit=200", deploymentID), nil)
		if resp.StatusCode == http.StatusOK {
			var events struct {
				Items []struct {
					Code   string         `json:"code"`
					Params map[string]any `json:"params"`
				} `json:"items"`
			}
			_ = json.Unmarshal(data, &events)
			for _, ev := range events.Items {
				if ev.Code == code {
					return ev.Params
				}
			}
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("no %s event on deployment %d", code, deploymentID)
	return nil
}

// startDockerAutomationRig starts a real server and a real agent with one
// fake 24 GiB GPU driving Docker, and registers a tiny local model on it; the
// engine is the fake engine image. It returns the server's base URL, an admin
// token, the node's UUID and the model's id. Skips without Docker.
func startDockerAutomationRig(t *testing.T, name string) (string, string, string, uint) {
	t.Helper()
	return startDockerAutomationRigOn(t, name, "A100x1,24576")
}

// startDockerAutomationRigOn is startDockerAutomationRig on the fake GPUs
// fakeGPU names (GPUWORKS_FAKE_GPU: <model>x<count>[,<MiB>]).
func startDockerAutomationRigOn(t *testing.T, name, fakeGPU string) (string, string, string, uint) {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		if os.Getenv("GPUWORKS_TEST_STRICT") == "1" {
			t.Fatalf("docker required in strict mode: %v", err)
		}
		t.Skipf("docker unavailable: %v", err)
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		if os.Getenv("GPUWORKS_TEST_STRICT") == "1" {
			t.Fatalf("docker daemon unavailable in strict mode: %v", err)
		}
		t.Skipf("docker daemon unavailable: %v", err)
	}
	root := repoRoot()
	if err := ensureFakeEngineImage(t, root); err != nil {
		t.Fatal(err)
	}
	ensureEmbeddedAgentFor(t, runtime.GOARCH)
	serverBin := buildBinary(t, "./cmd/gpuworks")
	agentBin := buildBinary(t, "./cmd/gpuworks-agent")
	port := freePort(t)
	base := "http://127.0.0.1:" + port
	server := serverCommand(t, "multiproc-password", serverBin, "serve", "--addr", "127.0.0.1:"+port, "--data", t.TempDir())
	serverLog, err := os.Create(filepath.Join(t.TempDir(), "server.log"))
	if err != nil {
		t.Fatal(err)
	}
	server.Stdout, server.Stderr = serverLog, serverLog
	t.Cleanup(func() {
		if t.Failed() {
			if data, err := os.ReadFile(serverLog.Name()); err == nil {
				t.Logf("server log:\n%s", data)
			}
		}
	})
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stopProcess(server) })
	waitHTTP(t, base+"/api/v1/healthz", serverHealthTimeout,
		func(r *http.Response) bool { return r.StatusCode == http.StatusUnauthorized })
	adminToken := login(t, base)
	seed, nodeToken := createNode(t, base, adminToken, name+"-node")

	agent := exec.Command(agentBin, "run", "--server", base, "--token", nodeToken,
		"--state", filepath.Join(t.TempDir(), "agent-state"))
	agent.Env = append(os.Environ(),
		"GPUWORKS_FAKE_GPU="+fakeGPU,
		"GPUWORKS_FAKE_GPU_NO_INJECT=1",
		"GPUWORKS_RUNTIME_DRIVER=docker",
		"GPUWORKS_LOG_LEVEL=warn")
	agent.Stdout, agent.Stderr = io.Discard, io.Discard
	if err := agent.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stopProcess(agent) })
	waitNode(t, base, adminToken, func(candidate processNode) bool {
		return candidate.UUID == seed.UUID && candidate.State == "online"
	})

	return base, adminToken, seed.UUID, registerFixtureModel(t, base, adminToken, seed.UUID, name)
}

// registerFixtureModel registers a tiny local model directory on the node, as
// the fake engine serves it, and returns its id.
func registerFixtureModel(t *testing.T, base, adminToken, nodeUUID, name string) uint {
	t.Helper()
	modelDir := filepath.Join(t.TempDir(), "model")
	if err := os.MkdirAll(modelDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(modelDir, "config.json"),
		[]byte(`{"model_type":"qwen3","architectures":["Qwen3ForCausalLM"],"max_position_embeddings":2048,"num_hidden_layers":2,"hidden_size":64,"num_attention_heads":4,"num_key_value_heads":4,"torch_dtype":"bfloat16"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(modelDir, "model.safetensors"), []byte("fixture"), 0o644); err != nil {
		t.Fatal(err)
	}
	localBody, _ := json.Marshal(map[string]any{"node_uuid": nodeUUID, "path": modelDir, "name": name + "-model"})
	localResp, localData := authedDo(t, http.MethodPost, base, adminToken, "/api/v1/models/local", localBody)
	if localResp.StatusCode != http.StatusCreated {
		t.Fatalf("register local model status=%d body=%s", localResp.StatusCode, localData)
	}
	var model struct {
		ID uint `json:"id"`
	}
	if err := json.Unmarshal(localData, &model); err != nil {
		t.Fatal(err)
	}

	return model.ID
}
