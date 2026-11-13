//go:build multiproc

package l2

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/testenv/apiwire"
)

// TestCustomCallableImageDeploys is l2-custom-callable-image-deploy: the
// custom tab registers any image under the callable engine, the catalog lists
// it as an inference series the deploy page filters by engine, and a callable
// deployment that picks it hands exactly that image to a real agent process.
//
// The recording runtime cannot run the adapter, so the case ends at the
// workload the agent was asked to start: the custom image and the command the
// deployment stated. Reaching running needs a container runtime and an
// adapter image, which is the L4 template path's job.
func TestCustomCallableImageDeploys(t *testing.T) {
	testCustomCallableImageDeploys(t, false)
}

// The runtime is a recording fixture, but server and agent are real processes.
// Return the catalog wrapper's terminal HTTP response on the allocated port
// and verify that its error crosses the control channel into the public API.
func TestCallableLoaderFailureReachesInstanceAPI(t *testing.T) {
	testCustomCallableImageDeploys(t, true)
}

func testCustomCallableImageDeploys(t *testing.T, loaderFails bool) {
	t.Helper()
	imageRef := "gpuworks/strata:0.1-gw4"
	launchCommand := "strata-serve --model /model --port 8000"
	if loaderFails {
		imageRef = "gpuworks/callable-fixture:test"
		launchCommand = "python3 /opt/gpuworks/runtime/gpuworks_serve.py --model /model --port 8000 --adapter diffusers_adapter --operation openai.image.generate"
	}
	ensureEmbeddedAgentFor(t, runtime.GOARCH)
	serverBin := buildBinary(t, "./cmd/gpuworks")
	agentBin := buildBinary(t, "./cmd/gpuworks-agent")
	port := freePort(t)
	base := "http://127.0.0.1:" + port
	serverLogPath := filepath.Join(t.TempDir(), "server.log")
	serverLog, err := os.Create(serverLogPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = serverLog.Close() })
	server := serverCommand(t, "multiproc-password", serverBin, "serve", "--addr", "127.0.0.1:"+port, "--data", t.TempDir())
	server.Stdout, server.Stderr = serverLog, serverLog
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stopProcess(server) })
	waitHTTP(t, base+"/api/v1/healthz", serverHealthTimeout,
		func(r *http.Response) bool { return r.StatusCode == http.StatusUnauthorized }, serverLogPath)
	adminToken := login(t, base)
	seed, nodeToken := createNode(t, base, adminToken, "custom-callable")

	recordPath := filepath.Join(t.TempDir(), "workload.json")
	agent := exec.Command(agentBin, "run", "--server", base, "--token", nodeToken,
		"--state", filepath.Join(t.TempDir(), "agent-state"))
	agent.Env = append(os.Environ(),
		"GPUWORKS_FAKE_GPU=A100x1,81920",
		"GPUWORKS_FAKE_RUNTIME_RECORD="+recordPath,
		"GPUWORKS_LOG_LEVEL=warn")
	agent.Stdout, agent.Stderr = io.Discard, io.Discard
	if err := agent.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stopProcess(agent) })
	node := waitNode(t, base, adminToken, func(candidate processNode) bool {
		return candidate.UUID == seed.UUID && candidate.State == "online"
	})

	// 1. The custom tab's form: any reference, engine callable, framework
	// required. No purpose is sent; the server defaults it.
	importBody, _ := json.Marshal(map[string]any{"series": []map[string]any{{
		"name": "strata", "engine": "callable", "framework": "cuda", "source": "custom",
		"versions": []map[string]any{{
			"image_ref": imageRef, "engine": "callable", "framework": "cuda", "framework_version": "12.9", "source": "custom",
		}},
	}}})
	importResp, importData := authedDo(t, http.MethodPost, base, adminToken, "/api/v1/images/import", importBody)
	if importResp.StatusCode != http.StatusOK {
		t.Fatalf("import status=%d body=%s", importResp.StatusCode, importData)
	}

	// 2. The deploy page lists series whose engine is the chosen one and whose
	// purpose is inference.
	_, listData := authedDo(t, http.MethodGet, base, adminToken, "/api/v1/images?source=custom", nil)
	var series []struct {
		Name    string `json:"name"`
		Engine  string `json:"engine"`
		Purpose string `json:"purpose"`
	}
	if err := apiwire.UnmarshalList(listData, &series); err != nil {
		t.Fatalf("decode series: %v (%s)", err, listData)
	}
	if len(series) != 1 || series[0].Engine != "callable" || series[0].Purpose != "inference" {
		t.Fatalf("custom series = %+v, want one callable inference series", series)
	}

	// 3. A callable deployment that picks the image.
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
	localBody, _ := json.Marshal(map[string]any{"node_uuid": node.UUID, "path": modelDir, "name": "callable-model"})
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
	// Without a template the callable engine contributes no command line
	// (ADR 0036): the deployment states the adapter's command itself.
	deployBody, _ := json.Marshal(map[string]any{
		"model_id": model.ID, "node_uuid": node.UUID, "name": "callable-custom", "served_name": "callable-custom",
		"engine": "callable", "image": imageRef, "quick_params": map[string]any{},
		"container_overrides": map[string]any{"command_mode": "manual", "cmd_display": launchCommand},
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

	instance := waitLaunchConfigInstance(t, base, adminToken, created.ID)
	recorded := waitRecordedLaunchWorkload(t, recordPath, instance.UUID)
	if recorded.Image != imageRef {
		t.Fatalf("the agent was asked to run image %q, want the custom %q", recorded.Image, imageRef)
	}
	command := strings.Join(append(append([]string(nil), recorded.Entrypoint...), recorded.Cmd...), " ")
	// The platform keeps the command and only moves the port to the one it
	// allocated.
	if !strings.Contains(command, strings.Split(launchCommand, "8000")[0]) {
		t.Fatalf("command = %q, want the deployment's own adapter command", command)
	}
	if !loaderFails {
		return
	}
	// Manual commands may be wrapped in a shell argv; this fixture has no
	// quoted values, so inspect its command text for the rewritten port.
	ports := launchConfigFlagValues(strings.Fields(command), "--port")
	if len(ports) != 1 {
		t.Fatalf("expected one allocated API port, got %q", ports)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:"+ports[0])
	if err != nil {
		t.Fatal(err)
	}
	probe := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Server", "GPUWorksCallable/1 Python/3.10.12")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprint(w, `{"status":"failed","error":"ModuleNotFoundError: No module named 'diffusers'"}`)
	}))
	_ = probe.Listener.Close()
	probe.Listener = listener
	probe.Start()
	defer probe.Close()
	deadline := time.Now().Add(15 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		_, data := authedDo(t, http.MethodGet, base, adminToken, "/api/v1/instances", nil)
		var instances []struct{ UUID, State, Message string }
		if err := apiwire.UnmarshalList(data, &instances); err != nil {
			t.Fatal(err)
		}
		for _, current := range instances {
			if current.UUID == instance.UUID {
				last = current.State + ": " + current.Message
				if current.State == "failed" && strings.Contains(current.Message, "ModuleNotFoundError: No module named 'diffusers'") {
					return
				}
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("callable failure did not reach the instance API before startup timeout: %s", last)
}
