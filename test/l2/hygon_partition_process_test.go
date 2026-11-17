//go:build multiproc

package l2

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/messages"
)

// TestHygonUnavailableSurvivesAgentReconnect exercises the real server/agent
// wire. A fake GPU is an inventory fixture, not evidence that native vdev
// assets and a supported runtime behavior probe succeeded on this host.
func TestHygonUnavailableSurvivesAgentReconnect(t *testing.T) {
	for _, sku := range []string{"Z100", "K100AI", "BW"} {
		t.Run(sku, func(t *testing.T) { testHygonUnavailableReconnect(t, sku) })
	}
}

func testHygonUnavailableReconnect(t *testing.T, sku string) {
	ensureEmbeddedAgentFor(t, runtime.GOARCH)
	serverBin := buildBinary(t, "./cmd/gpuworks")
	agentBin := buildBinary(t, "./cmd/gpuworks-agent")
	port := freePort(t)
	base := "http://127.0.0.1:" + port
	server := serverCommand(t, "multiproc-password", serverBin, "serve", "--addr", "127.0.0.1:"+port, "--data", t.TempDir())
	serverLogPath := filepath.Join(t.TempDir(), "server.log")
	serverLog, err := os.Create(serverLogPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = serverLog.Close() })
	server.Stdout, server.Stderr = serverLog, serverLog
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stopProcess(server) })
	waitHTTP(t, base+"/api/v1/healthz", serverHealthTimeout, func(resp *http.Response) bool { return resp.StatusCode == http.StatusUnauthorized }, serverLogPath)
	adminToken := login(t, base)
	seed, nodeToken := createNode(t, base, adminToken, "hygon-partition-process")
	stateDir := filepath.Join(t.TempDir(), "agent")
	recordPath := filepath.Join(t.TempDir(), "workload.json")
	startAgent := func() *exec.Cmd {
		cmd := exec.Command(agentBin, "run", "--server", base, "--token", nodeToken, "--state", stateDir)
		cmd.Env = append(os.Environ(),
			"GPUWORKS_FAKE_GPU="+sku+"x1,65536",
			"GPUWORKS_FAKE_GPU_VENDOR=hygon",
			"GPUWORKS_FAKE_GPU_ARCH="+sku,
			"GPUWORKS_FAKE_GPU_FRAMEWORK=dtk",
			"GPUWORKS_FAKE_GPU_FRAMEWORK_VERSION=25.04",
			"GPUWORKS_FAKE_GPU_NO_INJECT=1",
			"GPUWORKS_FAKE_RUNTIME_RECORD="+recordPath,
			"GPUWORKS_LOG_LEVEL=warn",
		)
		cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		return cmd
	}
	agent := startAgent()
	t.Cleanup(func() { stopProcess(agent) })
	waitUnavailable := func() {
		t.Helper()
		node := waitNode(t, base, adminToken, func(n processNode) bool {
			if n.UUID != seed.UUID || n.State != "online" || len(n.GPUs) != 1 {
				return false
			}
			for _, cap := range n.GPUs[0].PartitionCapabilities {
				if cap.Backend == "hygon-dgpu" && cap.Status == "unavailable" {
					return true
				}
			}
			return false
		})
		found := false
		for _, cap := range node.GPUs[0].PartitionCapabilities {
			if cap.Backend == "hygon-dgpu" {
				found = true
				if cap.DisplayName != "Hygon dGPU" || cap.ReasonCode != messages.ErrorPartitionRuntimeUnsupported ||
					cap.ReasonParams.Runtime != "recording" || !slices.Equal(cap.ReasonParams.SupportedRuntimes, []string{"docker", "udocker"}) {
					t.Fatalf("Hygon capability = %+v, want runtime-unavailable with server display name", cap)
				}
			}
		}
		if !found {
			t.Fatal("Hygon capability missing")
		}
	}
	waitUnavailable()

	modelDir := filepath.Join(t.TempDir(), "model")
	if err := os.MkdirAll(modelDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(modelDir, "config.json"), []byte(`{"model_type":"qwen3","architectures":["Qwen3ForCausalLM"],"max_position_embeddings":2048}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(modelDir, "model.safetensors"), []byte("fixture"), 0o644); err != nil {
		t.Fatal(err)
	}
	localBody, _ := json.Marshal(map[string]any{"node_uuid": seed.UUID, "path": modelDir, "name": "hygon-process-model"})
	localResp, localData := authedDo(t, http.MethodPost, base, adminToken, "/api/v1/models/local", localBody)
	if localResp.StatusCode != http.StatusCreated {
		t.Fatalf("register local model status=%d body=%s", localResp.StatusCode, localData)
	}
	var model struct {
		ID uint `json:"id"`
	}
	if err := json.Unmarshal(localData, &model); err != nil || model.ID == 0 {
		t.Fatalf("local model = %+v err=%v", model, err)
	}
	deployment := map[string]any{
		"name": "hygon-process", "served_name": "hygon-process", "model_id": model.ID, "node_uuid": seed.UUID,
		"engine": "vllm", "image": "validated/hygon:test", "quick_params": map[string]any{"max_model_len": 128},
		"partition_backend": "hygon-dgpu", "vram_limit_gib": 8,
	}
	limitedBody, _ := json.Marshal(deployment)
	limitedResp, limitedData := authedDo(t, http.MethodPost, base, adminToken, "/api/v1/deployments", limitedBody)
	if limitedResp.StatusCode < 400 || limitedResp.StatusCode >= 500 || !strings.Contains(string(limitedData), "hygon-dgpu") {
		t.Fatalf("unavailable Hygon quota accepted or gave unrelated error: status=%d body=%s", limitedResp.StatusCode, limitedData)
	}
	if _, err := os.Stat(recordPath); !os.IsNotExist(err) {
		t.Fatalf("rejected quota reached agent runtime, stat err=%v", err)
	}

	stopProcess(agent)
	waitNode(t, base, adminToken, func(n processNode) bool { return n.UUID == seed.UUID && n.State == "offline" })
	agent = startAgent()
	waitUnavailable()
	limitedResp, limitedData = authedDo(t, http.MethodPost, base, adminToken, "/api/v1/deployments", limitedBody)
	if limitedResp.StatusCode < 400 || limitedResp.StatusCode >= 500 || !strings.Contains(string(limitedData), "hygon-dgpu") {
		t.Fatalf("reconnected agent retained ready quota: status=%d body=%s", limitedResp.StatusCode, limitedData)
	}

	delete(deployment, "partition_backend")
	delete(deployment, "vram_limit_gib")
	wholeBody, _ := json.Marshal(deployment)
	wholeResp, wholeData := authedDo(t, http.MethodPost, base, adminToken, "/api/v1/deployments", wholeBody)
	if wholeResp.StatusCode != http.StatusCreated {
		t.Fatalf("whole-card deployment status=%d body=%s", wholeResp.StatusCode, wholeData)
	}
	var created l2Deployment
	if err := json.Unmarshal(wholeData, &created); err != nil || created.ID == 0 {
		t.Fatalf("whole-card deployment = %+v err=%v", created, err)
	}
	deadline := time.Now().Add(30 * time.Second)
	workloadSeen := false
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(recordPath)
		if err == nil {
			var spec struct {
				GPUIndexes []int
				Env        map[string]string
			}
			if json.Unmarshal(data, &spec) == nil && len(spec.GPUIndexes) == 1 && spec.GPUIndexes[0] == 0 {
				if spec.Env["HYGON_VISIBLE_DEVICES"] != "" {
					t.Fatalf("whole-card workload inherited Hygon vdev visibility: %+v", spec.Env)
				}
				workloadSeen = true
				break
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !workloadSeen {
		t.Fatalf("whole-card workload never reached agent runtime: %s", recordPath)
	}
	deleteResp, deleteData := authedDo(t, http.MethodDelete, base, adminToken, "/api/v1/deployments/"+itoa(created.ID), nil)
	if deleteResp.StatusCode != http.StatusOK && deleteResp.StatusCode != http.StatusAccepted {
		t.Fatalf("delete whole-card deployment status=%d body=%s", deleteResp.StatusCode, deleteData)
	}
}
