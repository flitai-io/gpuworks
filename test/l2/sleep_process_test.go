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

// TestProcessSleepWakeReconcilesTheRealEngine proves the sleep contract across
// the real process boundary: a deployment created with sleepable reaches the
// engine with vllm's sleep mode enabled, the server's sleep action drives the
// agent to reconcile the engine's own sleep endpoints, and the reported sleep
// level tracks what the engine actually did. Sleep is reconciled against a
// live engine process, so the case runs the fake engine in a real container;
// the recording-runtime L2 tests cannot express it. Skipped when Docker is
// unavailable (the ordinary L2 tests remain daemon-free).
func TestProcessSleepWakeReconcilesTheRealEngine(t *testing.T) {
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
	server.Stdout, server.Stderr = io.Discard, io.Discard
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stopProcess(server) })
	waitHTTP(t, base+"/api/v1/healthz", serverHealthTimeout,
		func(r *http.Response) bool { return r.StatusCode == http.StatusUnauthorized })
	adminToken := login(t, base)
	seed, nodeToken := createNode(t, base, adminToken, "sleep-wake-node")

	agent := exec.Command(agentBin, "run", "--server", base, "--token", nodeToken,
		"--state", filepath.Join(t.TempDir(), "agent-state"))
	agent.Env = append(os.Environ(),
		"GPUWORKS_FAKE_GPU=A100x1,24576",
		"GPUWORKS_FAKE_GPU_NO_INJECT=1",
		"GPUWORKS_RUNTIME_DRIVER=docker",
		"GPUWORKS_LOG_LEVEL=warn")
	agent.Stdout, agent.Stderr = io.Discard, io.Discard
	if err := agent.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stopProcess(agent) })
	waitNodeWithCards(t, base, adminToken, seed.UUID)

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
	localBody, _ := json.Marshal(map[string]any{"node_uuid": seed.UUID, "path": modelDir, "name": "sleep-wake-model"})
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

	deployBody, _ := json.Marshal(map[string]any{
		"model_id": model.ID, "node_uuid": seed.UUID, "served_name": "sleep-wake",
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
	hasSleepFlag := false
	for _, arg := range instance.Argv {
		hasSleepFlag = hasSleepFlag || arg == "--enable-sleep-mode"
	}
	if !hasSleepFlag {
		t.Fatalf("a sleepable deployment's engine command lacks --enable-sleep-mode: %v", instance.Argv)
	}
	if engineSleeping(t, instance.APIPort) {
		t.Fatalf("the engine must start awake")
	}

	sleepResp, err := authedPost(t, base, adminToken, "/api/v1/deployments/"+itoa(created.ID)+"/sleep?level=1", nil)
	if err != nil || sleepResp == nil || sleepResp.StatusCode != http.StatusOK {
		status := "<nil>"
		if sleepResp != nil {
			status = sleepResp.Status
		}
		t.Fatalf("sleep action failed: %v status=%s", err, status)
	}
	sleepResp.Body.Close()
	waitSleepLevel(t, base, adminToken, instance.UUID, 1)
	// The reported level must reflect the engine, not just the bookkeeping:
	// the agent reconciles against the engine's own /sleep endpoint.
	if !engineSleeping(t, instance.APIPort) {
		t.Fatalf("server reports L1 but the engine never slept")
	}

	wakeResp, err := authedPost(t, base, adminToken, "/api/v1/deployments/"+itoa(created.ID)+"/wake", nil)
	if err != nil || wakeResp == nil || wakeResp.StatusCode != http.StatusOK {
		status := "<nil>"
		if wakeResp != nil {
			status = wakeResp.Status
		}
		t.Fatalf("wake action failed: %v status=%s", err, status)
	}
	wakeResp.Body.Close()
	waitSleepLevel(t, base, adminToken, instance.UUID, 0)
	if engineSleeping(t, instance.APIPort) {
		t.Fatalf("server reports awake but the engine is still sleeping")
	}
}

// engineSleeping asks the running fake engine directly. It is the ground truth
// the agent's reported sleep level has to match.
func engineSleeping(t *testing.T, port int) bool {
	t.Helper()
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/is_sleeping", port))
	if err != nil {
		t.Fatalf("query engine /is_sleeping on port %d: %v", port, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("engine /is_sleeping status=%d", resp.StatusCode)
	}
	var body struct {
		IsSleeping bool `json:"is_sleeping"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	return body.IsSleeping
}

func waitSleepLevel(t *testing.T, base, token, instanceUUID string, level int) processInstance {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		_, data := authedDo(t, http.MethodGet, base, token, "/api/v1/instances", nil)
		var instances []processInstance
		if err := apiwire.UnmarshalList(data, &instances); err != nil {
			t.Fatalf("decode instance list: %v (%s)", err, data)
		}
		for _, instance := range instances {
			if instance.UUID == instanceUUID && instance.SleepLevel == level {
				return instance
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	_, data := authedDo(t, http.MethodGet, base, token, "/api/v1/instances", nil)
	t.Fatalf("instance %s never reached sleep level %d: %s", instanceUUID, level, data)
	return processInstance{}
}
