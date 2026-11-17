//go:build multiproc

package l2

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/testenv/apiwire"
)

type processNode struct {
	UUID                  string              `json:"uuid"`
	State                 string              `json:"state"`
	GPUCount              int                 `json:"gpu_count"`
	ManagementAddress     string              `json:"management_address"`
	PartitionCapabilities []processCapability `json:"partition_capabilities"`
	GPUs                  []struct {
		PartitionCapabilities []processCapability `json:"partition_capabilities"`
	} `json:"gpus"`
}

type processCapability struct {
	Backend, Status, Reason string
	DisplayName             string `json:"display_name"`
	// ReasonCode and ReasonParams are what a test asserts on (ADR 0067);
	// Reason is only the English fallback.
	ReasonCode   string `json:"reason_code"`
	ReasonParams struct {
		Runtime           string   `json:"runtime"`
		SupportedRuntimes []string `json:"supported_runtimes"`
	} `json:"reason_params"`
}

var processHTTPClient = &http.Client{Timeout: 5 * time.Second}

func TestServerAndAgentProcessesRegisterKillAndReconnect(t *testing.T) {
	ensureEmbeddedAgentFor(t, runtime.GOARCH)
	serverBin := buildBinary(t, "./cmd/gpuworks")
	agentBin := buildBinary(t, "./cmd/gpuworks-agent")
	port := freePort(t)
	base := "http://127.0.0.1:" + port
	dataDir := t.TempDir()
	startServer := func() *exec.Cmd {
		server := serverCommand(t, "multiproc-password", serverBin, "serve", "--addr", "127.0.0.1:"+port, "--data", dataDir)
		server.Stdout, server.Stderr = io.Discard, io.Discard
		if err := server.Start(); err != nil {
			t.Fatal(err)
		}
		return server
	}
	server := startServer()
	t.Cleanup(func() {
		stopProcess(server)
	})
	waitHTTP(t, base+"/api/v1/healthz", serverHealthTimeout, func(resp *http.Response) bool { return resp.StatusCode == http.StatusUnauthorized })
	assertBootstrapScripts(t, base)
	adminToken := login(t, base)
	nodeASeed, tokenA := createNode(t, base, adminToken, "multiproc-a")
	nodeBSeed, tokenB := createNode(t, base, adminToken, "multiproc-b")
	stateRoot := t.TempDir()
	startAgent := func(state, fakeGPU string) *exec.Cmd {
		nodeToken := tokenA
		if strings.HasPrefix(fakeGPU, "A10x") {
			nodeToken = tokenB
		}
		agent := exec.Command(agentBin, "run", "--server", base, "--token", nodeToken, "--state", state)
		agent.Env = append(os.Environ(), "GPUWORKS_FAKE_GPU="+fakeGPU, "GPUWORKS_RUNTIME_DRIVER=none", "GPUWORKS_LOG_LEVEL=debug")
		agent.Stdout, agent.Stderr = io.Discard, io.Discard
		if err := agent.Start(); err != nil {
			t.Fatal(err)
		}
		return agent
	}
	stateA := filepath.Join(stateRoot, "agent-a")
	stateB := filepath.Join(stateRoot, "agent-b")
	agentA := startAgent(stateA, "A100x4,24576")
	agentB := startAgent(stateB, "A10x2,16384")
	t.Cleanup(func() {
		for _, agent := range []*exec.Cmd{agentA, agentB} {
			stopProcess(agent)
		}
	})
	nodeA := waitNode(t, base, adminToken, func(n processNode) bool { return n.State == "online" && n.GPUCount == 4 })
	nodeB := waitNode(t, base, adminToken, func(n processNode) bool { return n.State == "online" && n.GPUCount == 2 })
	if nodeA.UUID != nodeASeed.UUID || nodeB.UUID != nodeBSeed.UUID {
		t.Fatalf("pending node identities changed: A=%+v/%+v B=%+v/%+v", nodeA, nodeASeed, nodeB, nodeBSeed)
	}
	if nodeA.UUID == "" || nodeB.UUID == "" || nodeA.UUID == nodeB.UUID {
		t.Fatalf("online nodes have invalid identities: A=%+v B=%+v", nodeA, nodeB)
	}
	if err := agentA.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_, _ = agentA.Process.Wait()
	agentA = nil
	waitNode(t, base, adminToken, func(n processNode) bool { return n.UUID == nodeA.UUID && n.State == "offline" })
	agentA = startAgent(stateA, "A100x4,24576")
	waitNode(t, base, adminToken, func(n processNode) bool { return n.UUID == nodeA.UUID && n.State == "online" })
	if err := server.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_, _ = server.Process.Wait()
	server = startServer()
	waitHTTP(t, base+"/api/v1/healthz", serverHealthTimeout, func(resp *http.Response) bool { return resp.StatusCode == http.StatusUnauthorized })
	waitNode(t, base, adminToken, func(n processNode) bool { return n.UUID == nodeA.UUID && n.State == "online" })
	waitNode(t, base, adminToken, func(n processNode) bool { return n.UUID == nodeB.UUID && n.State == "online" })
}

// TestPartitionFieldsCrossRealProcessBoundary exercises independently built
// server and agent binaries. The recording runtime is test-explicit and writes
// the final WorkloadSpec only after the desired InstanceDesiredState crossed the wire.
func TestPartitionFieldsCrossRealProcessBoundary(t *testing.T) {
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
	t.Cleanup(func() {
		stopProcess(server)
	})
	// This test shares a CI runner with several build-heavy jobs. Allow enough
	// time for a cold server start and preserve its output when startup fails.
	waitHTTP(t, base+"/api/v1/healthz", serverHealthTimeout, func(resp *http.Response) bool { return resp.StatusCode == http.StatusUnauthorized }, serverLogPath)
	adminToken := login(t, base)
	seed, nodeToken := createNode(t, base, adminToken, "partition-process")
	recordPath := filepath.Join(t.TempDir(), "workload.json")
	stateDir := filepath.Join(t.TempDir(), "agent")
	agent := exec.Command(agentBin, "run", "--server", base, "--token", nodeToken, "--state", stateDir)
	agent.Env = append(os.Environ(),
		"GPUWORKS_FAKE_GPU=Ascend-910B4x1,32768",
		"GPUWORKS_FAKE_GPU_VENDOR=ascend",
		"GPUWORKS_FAKE_GPU_ARCH=910B4",
		"GPUWORKS_FAKE_GPU_FRAMEWORK=cann",
		"GPUWORKS_FAKE_GPU_FRAMEWORK_VERSION=9.0.0",
		"GPUWORKS_FAKE_RUNTIME_RECORD="+recordPath,
		"GPUWORKS_LOG_LEVEL=warn",
	)
	agent.Stdout, agent.Stderr = io.Discard, io.Discard
	if err := agent.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		stopProcess(agent)
	})
	var ascendDisplayName string
	node := waitNode(t, base, adminToken, func(n processNode) bool {
		if n.UUID != seed.UUID || n.State != "online" || len(n.GPUs) != 1 {
			return false
		}
		for _, capability := range n.GPUs[0].PartitionCapabilities {
			if capability.Backend == "ascend-dgpu" && capability.Status == "ready" {
				ascendDisplayName = capability.DisplayName
				return true
			}
		}
		return false
	})
	// The dGPU display copy is produced server-side from the accelerator
	// registry; assert it so the wire vocabulary and the copy cannot drift.
	if ascendDisplayName != "Ascend dGPU" {
		t.Fatalf("ascend-dgpu display_name = %q, want Ascend dGPU", ascendDisplayName)
	}
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
	localBody, _ := json.Marshal(map[string]any{"node_uuid": node.UUID, "path": modelDir, "name": "partition-process"})
	localResp, err := authedPost(t, base, adminToken, "/api/v1/models/local", localBody)
	if err != nil || localResp.StatusCode != http.StatusCreated {
		data := []byte(nil)
		if localResp != nil {
			data, _ = io.ReadAll(localResp.Body)
			localResp.Body.Close()
		}
		t.Fatalf("register local model status=%v body=%s err=%v", responseStatus(localResp), data, err)
	}
	var model struct {
		ID uint `json:"id"`
	}
	if err := json.NewDecoder(localResp.Body).Decode(&model); err != nil {
		t.Fatal(err)
	}
	localResp.Body.Close()
	deployBody, _ := json.Marshal(map[string]any{
		"name": "partition-process", "served_name": "partition-process", "model_id": model.ID, "node_uuid": node.UUID,
		"engine": "vllm", "image": "validated/ascend:test", "vram_limit_gib": 8, "partition_backend": "ascend-dgpu",
		"quick_params": map[string]any{"max_model_len": 128},
	})
	depResp, err := authedPost(t, base, adminToken, "/api/v1/deployments", deployBody)
	if err != nil || depResp.StatusCode != http.StatusCreated {
		data := []byte(nil)
		if depResp != nil {
			data, _ = io.ReadAll(depResp.Body)
			depResp.Body.Close()
		}
		t.Fatalf("create partition deployment status=%v body=%s err=%v", responseStatus(depResp), data, err)
	}
	depResp.Body.Close()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		data, readErr := os.ReadFile(recordPath)
		if readErr == nil {
			var spec struct {
				Env    map[string]string
				Mounts []struct{ Target string }
			}
			if json.Unmarshal(data, &spec) == nil {
				targets := map[string]bool{}
				for _, mount := range spec.Mounts {
					targets[mount.Target] = true
				}
				if spec.Env["ENPU_DSMI_HOOK"] == "1" && targets["/opt/gpuworks/partition/libvruntime.so"] && targets["/etc/enpu/vcann-rt/npu_info.config"] {
					return
				}
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("recorded WorkloadSpec never contained Ascend partition injection: %s", recordPath)
}

func responseStatus(resp *http.Response) any {
	if resp == nil {
		return "<nil>"
	}
	return resp.StatusCode
}

type processInstance struct {
	UUID         string   `json:"uuid"`
	DeploymentID uint     `json:"deployment_id"`
	NodeUUID     string   `json:"node_uuid"`
	State        string   `json:"state"`
	ContainerID  string   `json:"container_id"`
	SleepLevel   int      `json:"sleep_level"`
	GPUIndexes   []int    `json:"gpu_indexes"`
	APIPort      int      `json:"api_port"`
	Argv         []string `json:"argv"`
}

func authedPost(t *testing.T, base, token, path string, body []byte) (*http.Response, error) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, base+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	return processHTTPClient.Do(req)
}

func waitInstance(t *testing.T, base, token string, match func(processInstance) bool) processInstance {
	t.Helper()
	deadline := time.Now().Add(instanceRunningDeadline)
	for time.Now().Before(deadline) {
		req, _ := http.NewRequest(http.MethodGet, base+"/api/v1/instances", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := processHTTPClient.Do(req)
		if err == nil && resp.StatusCode == http.StatusOK {
			var instances []processInstance
			err := apiwire.ReadList(resp.Body, &instances)
			resp.Body.Close()
			if err != nil {
				t.Fatalf("decode /api/v1/instances: %v", err)
			}
			for _, instance := range instances {
				if match(instance) {
					return instance
				}
			}
		} else if resp != nil {
			resp.Body.Close()
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("instance condition timed out")
	return processInstance{}
}

func TestInstallScriptDownloadsAndStartsRealAgent(t *testing.T) {
	if runtime.GOOS != "linux" {
		if os.Getenv("GPUWORKS_TEST_STRICT") == "1" {
			t.Fatalf("install execution test requires Linux, got %s", runtime.GOOS)
		}
		t.Skipf("downloaded Linux agent cannot execute on %s", runtime.GOOS)
	}
	if _, err := exec.LookPath("curl"); err != nil {
		if os.Getenv("GPUWORKS_TEST_STRICT") == "1" {
			t.Fatalf("curl unavailable in strict mode: %v", err)
		}
		t.Skipf("curl unavailable: %v", err)
	}
	ensureEmbeddedAgentFor(t, runtime.GOARCH)
	serverBin := buildBinary(t, "./cmd/gpuworks")
	port := freePort(t)
	base := "http://127.0.0.1:" + port
	dataDir := t.TempDir()
	server := serverCommand(t, "install-password", serverBin, "serve", "--addr", "127.0.0.1:"+port, "--data", dataDir)
	server.Stdout, server.Stderr = io.Discard, io.Discard
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	defer stopProcess(server)
	waitHTTP(t, base+"/api/v1/healthz", serverHealthTimeout, func(resp *http.Response) bool { return resp.StatusCode == http.StatusUnauthorized })
	adminToken := loginWithPassword(t, base, "install-password")
	_, nodeToken := createNode(t, base, adminToken, "install-node")
	installDir := t.TempDir()
	stateDir := filepath.Join(t.TempDir(), "agent-state")
	installed := filepath.Join(installDir, "gpuworks-agent")
	// Register cleanup before downloading/executing the script so a failure in
	// any intermediate step cannot leave the non-systemd child behind.
	t.Cleanup(func() {
		if out, err := exec.Command("pgrep", "-f", installed+" run").Output(); err == nil {
			for _, line := range strings.Fields(string(out)) {
				if pid, parseErr := strconv.Atoi(line); parseErr == nil {
					_ = exec.Command("kill", strconv.Itoa(pid)).Run()
				}
			}
		}
	})
	scriptResp, err := processHTTPClient.Get(base + "/install.sh")
	if err != nil {
		t.Fatal(err)
	}
	script, err := io.ReadAll(scriptResp.Body)
	scriptResp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", "-s", "--", "--server", base, "--token", nodeToken, "--install-dir", installDir)
	cmd.Stdin = bytes.NewReader(script)
	cmd.Env = append(os.Environ(), "GPUWORKS_FAKE_GPU=A100x1,24576", "GPUWORKS_RUNTIME_DRIVER=none", "GPUWORKS_STATE_DIR="+stateDir, "GPUWORKS_NO_SYSTEMD=1", "GPUWORKS_LOG_LEVEL=warn")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("install script: %v\n%s", err, out)
	} else {
		t.Logf("install script output: %s", out)
	}
	if st, err := os.Stat(installed); err != nil || st.Mode()&0o111 == 0 {
		t.Fatalf("installed agent missing or not executable: %v", err)
	}
	waitInstalledNode(t, base, adminToken, installDir, stateDir)
}

func TestAgentUpgradeEndpointAtomicallyReplacesBinary(t *testing.T) {
	if runtime.GOOS != "linux" {
		if os.Getenv("GPUWORKS_TEST_STRICT") == "1" {
			t.Fatalf("agent self-upgrade requires Linux, got %s", runtime.GOOS)
		}
		t.Skipf("agent self-upgrade executes a Linux replacement binary; got %s", runtime.GOOS)
	}
	serverBin, agentBin, dataDir := upgradeProcessBinaries(t)
	port := freePort(t)
	base := "http://127.0.0.1:" + port
	server := serverCommand(t, "upgrade-password", serverBin, "serve", "--addr", "127.0.0.1:"+port, "--data", dataDir)
	server.Stdout, server.Stderr = io.Discard, io.Discard
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	defer stopProcess(server)
	waitHTTP(t, base+"/api/v1/healthz", serverHealthTimeout, func(resp *http.Response) bool { return resp.StatusCode == http.StatusUnauthorized })
	adminToken := loginWithPassword(t, base, "upgrade-password")
	node, nodeToken := createNode(t, base, adminToken, "upgrade-node")
	state := filepath.Join(t.TempDir(), "agent-state")
	agent := exec.Command(agentBin, "run", "--server", base, "--token", nodeToken, "--state", state)
	agent.Env = append(os.Environ(), "GPUWORKS_FAKE_GPU=A100x1,24576", "GPUWORKS_RUNTIME_DRIVER=none", "GPUWORKS_LOG_LEVEL=warn")
	agent.Stdout, agent.Stderr = io.Discard, io.Discard
	if err := agent.Start(); err != nil {
		t.Fatal(err)
	}
	defer stopProcess(agent)
	waitNode(t, base, adminToken, func(n processNode) bool { return n.UUID == node.UUID && n.State == "online" })
	waitAgentUpgradeEligible(t, base, adminToken, node.UUID)
	req, err := http.NewRequest(http.MethodPost, base+"/api/v1/nodes/"+node.UUID+"/upgrade", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+adminToken)
	resp, err := processHTTPClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted || !bytes.Contains(body, []byte("upgrading")) {
		t.Fatalf("upgrade endpoint status=%d body=%s", resp.StatusCode, body)
	}
	waitUpgradeConfirmed(t, agentBin)
	waitUpgradeRunFromResponse(t, base, adminToken, body)
}

// A server_dial agent cannot reach the server: its --server points at a port
// nothing listens on, the way a public node behind which the server sits on a
// private network would see it. The upgrade must still arrive, over the
// connection the server dialed, and be confirmed once the server dials the
// replacement process.
func TestServerDialAgentUpgradesWithoutReachingTheServer(t *testing.T) {
	if runtime.GOOS != "linux" {
		if os.Getenv("GPUWORKS_TEST_STRICT") == "1" {
			t.Fatalf("agent self-upgrade requires Linux, got %s", runtime.GOOS)
		}
		t.Skipf("agent self-upgrade executes a Linux replacement binary; got %s", runtime.GOOS)
	}
	serverBin, agentBin, dataDir := upgradeProcessBinaries(t)
	port := freePort(t)
	base := "http://127.0.0.1:" + port
	server := serverCommand(t, "upgrade-password", serverBin, "serve", "--addr", "127.0.0.1:"+port, "--data", dataDir, "--server-dial-reconnect-interval", "1s")
	server.Stdout, server.Stderr = io.Discard, io.Discard
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	defer stopProcess(server)
	waitHTTP(t, base+"/api/v1/healthz", serverHealthTimeout, func(resp *http.Response) bool { return resp.StatusCode == http.StatusUnauthorized })
	adminToken := loginWithPassword(t, base, "upgrade-password")

	listenPort := freePort(t)
	unreachable := "http://127.0.0.1:" + freePort(t)
	req, err := http.NewRequest(http.MethodPost, base+"/api/v1/nodes", bytes.NewBufferString(`{"name":"dial-upgrade","connect_mode":"server_dial","host_address":"127.0.0.1","server_dial_listen":":`+listenPort+`","server_url":"`+unreachable+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+adminToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := processHTTPClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var created struct {
		Node  processNode `json:"node"`
		Token string      `json:"token"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated || created.Token == "" {
		t.Fatalf("create server_dial node status=%d body=%+v", resp.StatusCode, created)
	}

	state := filepath.Join(t.TempDir(), "agent-state")
	agentLog, err := os.Create(filepath.Join(t.TempDir(), "agent.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer agentLog.Close()
	agent := exec.Command(agentBin, "run", "--server", unreachable, "--token", created.Token, "--state", state,
		"--connect-mode", "server_dial", "--listen", "127.0.0.1:"+listenPort)
	agent.Env = append(os.Environ(), "GPUWORKS_FAKE_GPU=A100x1,24576", "GPUWORKS_RUNTIME_DRIVER=none", "GPUWORKS_LOG_LEVEL=info")
	agent.Stdout, agent.Stderr = agentLog, agentLog
	if err := agent.Start(); err != nil {
		t.Fatal(err)
	}
	defer stopProcess(agent)
	defer func() {
		if t.Failed() {
			raw, _ := os.ReadFile(agentLog.Name())
			t.Logf("agent log:\n%s", raw)
		}
	}()
	waitNode(t, base, adminToken, func(n processNode) bool { return n.UUID == created.Node.UUID && n.State == "online" })
	waitAgentUpgradeEligible(t, base, adminToken, created.Node.UUID)

	req, err = http.NewRequest(http.MethodPost, base+"/api/v1/nodes/"+created.Node.UUID+"/upgrade", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+adminToken)
	resp, err = processHTTPClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("upgrade endpoint status=%d body=%s", resp.StatusCode, body)
	}
	waitUpgradeConfirmed(t, agentBin)
	waitUpgradeRunFromResponse(t, base, adminToken, body)
	waitNode(t, base, adminToken, func(n processNode) bool { return n.UUID == created.Node.UUID && n.State == "online" })
}

// waitUpgradeConfirmed waits for the replaced binary to register: the
// previous binary is kept as .old and the probation marker is gone, with no
// rollback recorded.
func waitUpgradeConfirmed(t *testing.T, agentBin string) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		_, pendingErr := os.Stat(agentBin + ".upgrade-pending")
		if st, err := os.Stat(agentBin + ".old"); err == nil && st.Size() > 0 && os.IsNotExist(pendingErr) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if st, err := os.Stat(agentBin + ".old"); err != nil || st.Size() == 0 {
		t.Fatalf("agent self-upgrade did not leave a backup: %v", err)
	}
	if _, err := os.Stat(agentBin + ".upgrade-pending"); err == nil {
		t.Fatal("upgraded agent was never confirmed")
	}
	if note, err := os.ReadFile(agentBin + ".upgrade-rolled-back"); err == nil {
		t.Fatalf("upgrade rolled back: %s", note)
	}
}

func waitInstalledNode(t *testing.T, base, token, installDir string, logDirs ...string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		req, _ := http.NewRequest(http.MethodGet, base+"/api/v1/nodes", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := processHTTPClient.Do(req)
		if err == nil && resp.StatusCode == http.StatusOK {
			var nodes []processNode
			err := apiwire.ReadList(resp.Body, &nodes)
			resp.Body.Close()
			if err != nil {
				t.Fatalf("decode /api/v1/nodes: %v", err)
			}
			for _, node := range nodes {
				if node.State == "online" && node.GPUCount == 1 {
					return
				}
			}
		} else if resp != nil {
			resp.Body.Close()
		}
		time.Sleep(100 * time.Millisecond)
	}
	if log, err := os.ReadFile(filepath.Join(installDir, "agent.log")); err == nil {
		t.Logf("installed agent log:\n%s", log)
	}
	for _, dir := range logDirs {
		if log, err := os.ReadFile(filepath.Join(dir, "agent.log")); err == nil {
			t.Logf("installed agent log (%s):\n%s", dir, log)
		}
	}
	t.Fatal("installed agent did not register")
}

func assertBootstrapScripts(t *testing.T, base string) {
	t.Helper()
	resp, err := processHTTPClient.Get(base + "/install.sh")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(string(body), "#!/bin/sh") {
		t.Fatalf("/install.sh status=%d body prefix=%q", resp.StatusCode, string(body)[:min(len(body), 20)])
	}
	if !strings.Contains(string(body), "/dl/gpuworks-agent-${GOOS}-${GOARCH}") {
		t.Fatalf("/install.sh does not download an embedded agent")
	}
	resp, err = processHTTPClient.Get(base + "/dl/gpuworks-agent-linux-" + runtime.GOARCH)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("embedded agent endpoint status=%d", resp.StatusCode)
	}
}

func createNode(t *testing.T, base, adminToken, name string) (processNode, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, base+"/api/v1/nodes", bytes.NewBufferString(`{"name":"`+name+`","server_url":"`+base+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+adminToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := processHTTPClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body struct {
		Node  processNode `json:"node"`
		Token string      `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusCreated || body.Node.UUID == "" || body.Token == "" {
		t.Fatalf("create node status=%d body=%+v", resp.StatusCode, body)
	}
	return body.Node, body.Token
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// Binaries are built once per package for the whole test binary rather than
// once per call. The suite asked for 52 builds a run, each relinking into a
// fresh t.TempDir for about 85s of the total; the compile cache made the
// compile free but never the link. Caching also makes the embedded agent a
// once-per-process fact: it has to be in the source tree before the server is
// built (agentbin uses go:embed), which is a shared input no test can own.
var (
	buildMu     sync.Mutex
	builtBins   = map[string]buildResult{}
	embeddedBin = map[string]error{}
	buildRoot   string
)

type buildResult struct {
	path string
	err  error
}

func buildBinary(t *testing.T, pkg string) string {
	t.Helper()
	// The server embeds the agent it serves at /dl/, so the agent has to be in
	// place before this build. Doing it here rather than at each call site is
	// what makes one cached server binary correct for every test: two of them
	// build a server without asking for the agent, and an embedded agent makes
	// no difference to what they assert.
	if pkg == "./cmd/gpuworks" {
		ensureEmbeddedAgentFor(t, runtime.GOARCH)
	}
	buildMu.Lock()
	defer buildMu.Unlock()
	if got, ok := builtBins[pkg]; ok {
		if got.err != nil {
			t.Fatalf("build %s: %v", pkg, got.err)
		}
		return got.path
	}
	out := filepath.Join(sharedBuildRootLocked(t), filepath.Base(pkg))
	args := []string{"build", "-trimpath"}
	if os.Getenv("GOCOVERDIR") != "" {
		// The main package must also be instrumented so it installs the
		// standalone coverage runtime that writes GOCOVERDIR on exit.
		args = append(args, "-cover", "-coverpkg=./cmd/...,./internal/...,./pkg/...")
	}
	args = append(args, "-o", out, pkg)
	cmd := exec.Command("go", args...)
	cmd.Dir = repoRoot()
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		err = fmt.Errorf("%w\n%s", err, stderr.String())
	}
	builtBins[pkg] = buildResult{path: out, err: err}
	if err != nil {
		t.Fatalf("build %s: %v", pkg, err)
	}
	return out
}

// sharedBuildRootLocked is the directory the cached binaries live in. It
// outlives individual tests, so it cannot be a t.TempDir; TestMain removes it.
func sharedBuildRootLocked(t *testing.T) string {
	t.Helper()
	if buildRoot != "" {
		return buildRoot
	}
	dir, err := os.MkdirTemp("", "gpuworks-l2-bin-")
	if err != nil {
		t.Fatalf("create the shared build directory: %v", err)
	}
	buildRoot = dir
	return buildRoot
}

func repoRoot() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Clean(filepath.Join(filepath.Dir(file), "../.."))
}

// stopProcess gives coverage-instrumented binaries a chance to flush their
// counters. Tests that explicitly verify crash/reconnect semantics still use
// Process.Kill at the point of failure; only final cleanup is graceful.
func stopProcess(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil || cmd.ProcessState != nil {
		return
	}
	_ = cmd.Process.Signal(os.Interrupt)
	done := make(chan struct{})
	go func() {
		_, _ = cmd.Process.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Kill()
		<-done
	}
}

// ensureEmbeddedAgentFor places the agent the server serves at /dl/ into the
// source tree, once per architecture for the whole test binary. The path is
// fixed because go:embed reads it at compile time, so it is shared state:
// repeating the build would also let two callers write the same file at once.
func ensureEmbeddedAgentFor(t *testing.T, arch string) {
	t.Helper()
	buildMu.Lock()
	defer buildMu.Unlock()
	if err, ok := embeddedBin[arch]; ok {
		if err != nil {
			t.Fatalf("build embedded agent: %v", err)
		}
		return
	}
	root := repoRoot()
	target := filepath.Join(root, "internal/server/agentbin/dl/gpuworks-agent-linux-"+arch)
	args := []string{"build", "-trimpath"}
	if os.Getenv("GOCOVERDIR") != "" && arch == runtime.GOARCH {
		args = append(args, "-cover", "-coverpkg=./cmd/...,./internal/...,./pkg/...")
	}
	args = append(args, "-o", target, "./cmd/gpuworks-agent")
	cmd := exec.Command("go", args...)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+arch)
	out, err := cmd.CombinedOutput()
	if err != nil {
		err = fmt.Errorf("%w: %s", err, out)
	}
	embeddedBin[arch] = err
	if err != nil {
		t.Fatalf("build embedded agent: %v", err)
	}
}
func freePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return fmt.Sprint(port)
}

// serverHealthTimeout bounds every wait for a freshly started control-plane
// process to serve /healthz. It is one budget because it is one operation:
// the sites that used 10s failed on a CI runner (process_test.go:58 and :320,
// both "timeout waiting for .../api/v1/healthz") while the 30s ones did not,
// and a coverage-instrumented server on a loaded runner is simply slower to
// come up than the fastest local run suggests.
const serverHealthTimeout = 30 * time.Second

func waitHTTP(t *testing.T, url string, timeout time.Duration, ok func(*http.Response) bool, diagnosticPaths ...string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := processHTTPClient.Get(url)
		if err == nil {
			good := ok(resp)
			resp.Body.Close()
			if good {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	for _, path := range diagnosticPaths {
		if data, err := os.ReadFile(path); err == nil && len(data) > 0 {
			t.Logf("process log (%s):\n%s", path, data)
		}
	}
	t.Fatalf("timeout waiting for %s", url)
}
func login(t *testing.T, base string) string {
	return loginWithPassword(t, base, "multiproc-password")
}

func loginWithPassword(t *testing.T, base, password string) string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		body := fmt.Sprintf(`{"username":"admin","password":%q}`, password)
		req, _ := http.NewRequest(http.MethodPost, base+"/api/v1/auth/login", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := processHTTPClient.Do(req)
		if err == nil {
			var body struct {
				Token string `json:"token"`
			}
			_ = json.NewDecoder(resp.Body).Decode(&body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK && body.Token != "" {
				return body.Token
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("management login timed out")
	return ""
}

func waitNode(t *testing.T, base, token string, want func(processNode) bool) processNode {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		req, _ := http.NewRequest(http.MethodGet, base+"/api/v1/nodes", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := processHTTPClient.Do(req)
		if err == nil && resp.StatusCode == http.StatusOK {
			var nodes []processNode
			err := apiwire.ReadList(resp.Body, &nodes)
			resp.Body.Close()
			if err != nil {
				t.Fatalf("decode /api/v1/nodes: %v", err)
			}
			for _, node := range nodes {
				if want(node) {
					return node
				}
			}
		} else if resp != nil {
			resp.Body.Close()
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("timeout waiting for node state")
	return processNode{}
}
