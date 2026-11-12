//go:build kind

package l3kind

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/installcred"
	"gpuworks.ai/gpuworks/internal/testenv/apiwire"
)

const (
	fakeEngineImage     = "gpuworks/fake-engine:test"
	defaultAgentImage   = "gpuworks/gpuworks-agent:l3kind"
	defaultKindNodeImg  = "kindest/node:v1.34.0"
	kubeNamespace       = "gpuworks-system"
	adminPassword       = "gpuworks-test-password-2026"
	servedName          = "kind-chat"
	fakeGPUEnv          = "GPUWORKS_FAKE_GPU=A100x1,24576"
	nodeKubeconfig      = "/etc/kubernetes/admin.conf"
	agentLabelSelector  = "app.kubernetes.io/name=gpuworks-agent"
	modelDirInNode      = "/opt/gpuworks/models/m1"
	qwen3TinyConfigJSON = `{"model_type":"qwen3","architectures":["Qwen3ForCausalLM"],"max_position_embeddings":2048,"num_hidden_layers":2,"hidden_size":64,"num_attention_heads":4,"num_key_value_heads":4,"torch_dtype":"bfloat16"}`
)

// TestKubeDaemonSetDeployChatKind proves Kubernetes node support end to end on
// a real (kind) cluster: a cluster created through the API is joined with the
// manifest it renders, the DaemonSet agent enrolls as a node, a model is
// deployed as an instance Pod and answers chat through the gateway, the
// instance survives sleep/wake and an agent restart (adopted, not recreated),
// a kubectl cordon shows up as the node's scheduling state, and deleting the
// manifest removes everything gpuworks put in the cluster.
//
// The agent and fake-engine images must exist in the local docker daemon
// (`make test-l3-kind` builds them). The case skips when docker or kind is
// missing, and fails instead with GPUWORKS_TEST_STRICT=1. Run it with:
//
//	make agent-image AGENT_IMAGE=gpuworks/gpuworks-agent:l3kind && make fake-engine-image
//	GPUWORKS_KIND_AGENT_IMAGE=gpuworks/gpuworks-agent:l3kind CGO_ENABLED=0 \
//	  go test -count=1 -v -tags=kind -timeout=20m ./test/l3kind/ -run '^TestKubeDaemonSetDeployChatKind$'
//
// GPUWORKS_KEEP_ON_FAILURE=1 keeps the kind cluster and the server's files
// after a failure so the state can be inspected by hand.
func TestKubeDaemonSetDeployChatKind(t *testing.T) {
	agentImage := envOr("GPUWORKS_KIND_AGENT_IMAGE", defaultAgentImage)
	nodeImage := envOr("GPUWORKS_KIND_NODE_IMAGE", defaultKindNodeImg)
	kindBin := requireTools(t, agentImage)

	// ---- 2. kind cluster with both images loaded.
	cluster := fmt.Sprintf("gw-l3kind-%d", time.Now().UnixNano()%1_000_000_000)
	node := cluster + "-control-plane"
	t.Cleanup(func() {
		if t.Failed() && os.Getenv("GPUWORKS_KEEP_ON_FAILURE") == "1" {
			t.Logf("GPUWORKS_KEEP_ON_FAILURE=1: kind cluster %s kept; inspect with `docker exec -e KUBECONFIG=%s %s kubectl get pods -A`, delete with `%s delete cluster --name %s`",
				cluster, nodeKubeconfig, node, kindBin, cluster)
			return
		}
		out, err := command(2*time.Minute, kindBin, "delete", "cluster", "--name", cluster)
		if err != nil {
			t.Logf("delete kind cluster %s: %v\n%s", cluster, err, out)
		}
	})
	step(t, "create kind cluster %s (%s)", cluster, nodeImage)
	mustRun(t, 6*time.Minute, kindBin, "create", "cluster", "--name", cluster, "--image", nodeImage, "--wait", "180s")
	for _, image := range []string{agentImage, fakeEngineImage} {
		step(t, "kind load docker-image %s", image)
		mustRun(t, 5*time.Minute, kindBin, "load", "docker-image", image, "--name", cluster)
	}

	k := &kubectl{t: t, node: node}
	t.Cleanup(func() {
		if t.Failed() {
			k.dumpDiagnostics()
		}
	})

	// ---- 3. server.
	root := repoRoot()
	binDir := t.TempDir()
	serverBin := filepath.Join(binDir, "gpuworks")
	step(t, "build the server binary")
	build := exec.Command("go", "build", "-trimpath", "-o", serverBin, "./cmd/gpuworks")
	build.Dir = root
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build ./cmd/gpuworks: %v\n%s", err, out)
	}
	dataDir := t.TempDir()
	// A new node's agent takes the server's own agent before it registers
	// (kubeboot): serve the one built from this tree rather than whatever
	// agents an earlier build left embedded.
	stageAgent(t, root, filepath.Join(dataDir, "dl"))
	// The first start creates the admin account from initial-credentials, the
	// way an installed server receives it.
	if err := installcred.WriteInitialCredentials(dataDir, "admin", adminPassword); err != nil {
		t.Fatalf("write initial credentials: %v", err)
	}
	port := freePort(t)
	base := "http://127.0.0.1:" + port
	serverLog, err := os.Create(filepath.Join(dataDir, "server.out.log"))
	if err != nil {
		t.Fatal(err)
	}
	server := exec.Command(serverBin, "serve", "--addr", "0.0.0.0:"+port, "--data", dataDir,
		"--recovery-enabled=false", "--log-level", "info")
	server.Env = os.Environ()
	server.Stdout, server.Stderr = serverLog, serverLog
	if err := server.Start(); err != nil {
		t.Fatalf("start gpuworks serve: %v", err)
	}
	t.Cleanup(func() {
		stopProcess(server)
		serverLog.Close()
		if t.Failed() {
			if data, err := os.ReadFile(serverLog.Name()); err == nil {
				t.Logf("server log tail:\n%s", tail(string(data), 6000))
			}
		}
	})
	step(t, "wait for the server on port %s", port)
	eventually(t, "server /api/v1/healthz answers 401 (up, auth required)", time.Minute, 200*time.Millisecond, func() (bool, string) {
		resp, err := httpClient.Get(base + "/api/v1/healthz")
		if err != nil {
			return false, err.Error()
		}
		resp.Body.Close()
		return resp.StatusCode == http.StatusUnauthorized, resp.Status
	})
	api := &apiClient{t: t, base: base}
	api.login()

	// ---- 4. host address as seen from the kind node.
	gateway := kindGatewayIPv4(t)
	serverURL := "http://" + gateway + ":" + port
	step(t, "kind network gateway %s, server_url %s", gateway, serverURL)

	// ---- 5. create the cluster and apply its manifest inside the node.
	var created struct {
		ID                uint   `json:"id"`
		UUID              string `json:"uuid"`
		Name              string `json:"name"`
		Namespace         string `json:"namespace"`
		ServerURL         string `json:"server_url"`
		AgentImage        string `json:"agent_image"`
		RuntimeClass      string `json:"runtime_class"`
		EnginePorts       string `json:"engine_ports"`
		IdleReturnSeconds int    `json:"idle_return_seconds"`
		ClusterUID        string `json:"cluster_uid"`
		Revoked           bool   `json:"revoked"`
		NodeCount         int    `json:"node_count"`
		ApplyCommand      string `json:"apply_command"`
	}
	step(t, "create the Kubernetes cluster through the API")
	api.mustJSON(http.MethodPost, "/api/v1/kube/clusters", map[string]any{
		"name": "l3kind", "server_url": serverURL, "agent_image": agentImage,
		"runtime_class": "", "idle_return_seconds": 120,
	}, http.StatusCreated, &created)
	if created.ID == 0 || created.UUID == "" || created.Name != "l3kind" || created.Namespace != kubeNamespace ||
		created.ServerURL != serverURL || created.AgentImage != agentImage || created.RuntimeClass != "" ||
		created.IdleReturnSeconds != 120 || created.EnginePorts != "40000-42767" || created.ClusterUID != "" ||
		created.Revoked || created.NodeCount != 0 {
		t.Fatalf("created cluster has unexpected fields: %+v", created)
	}
	wantPrefix := "kubectl apply -f '" + serverURL + "/kube/manifest.yaml?token="
	if !strings.HasPrefix(created.ApplyCommand, wantPrefix) || !strings.HasSuffix(created.ApplyCommand, "'") {
		t.Fatalf("apply_command = %q, want prefix %q", created.ApplyCommand, wantPrefix)
	}
	manifestURL := strings.TrimSuffix(strings.TrimPrefix(created.ApplyCommand, "kubectl apply -f '"), "'")
	step(t, "apply the cluster manifest inside the kind node")
	k.mustShell(2*time.Minute, created.ApplyCommand)
	k.waitDaemonSetReady("after the first apply")

	// The kind node has no card: its agent stays up, standing by, and does
	// not join.
	eventually(t, "the agent stands by on a node without a card", 2*time.Minute, time.Second, func() (bool, string) {
		logs, _ := k.run(30*time.Second, "-n", kubeNamespace, "logs", "ds/gpuworks-agent")
		return strings.Contains(logs, "standing by"), lastLine(logs)
	})
	for _, n := range api.nodes() {
		if n.label(labelKubeNode) == node {
			t.Fatalf("a node without a card joined: %+v", n)
		}
	}

	// ---- 6. fake GPU on the DaemonSet (the kind node has no GPU).
	step(t, "set the fake GPU environment on the DaemonSet")
	k.mustRun(time.Minute, "-n", kubeNamespace, "set", "env", "ds/gpuworks-agent", fakeGPUEnv, "GPUWORKS_FAKE_GPU_NO_INJECT=1")
	k.waitDaemonSetReady("after set env")

	// ---- 7. the node enrolls.
	var nodeInfo apiNode
	eventually(t, "node online in the API with 1 GPU, runtime_driver kube and kube.node_name "+node, 3*time.Minute, time.Second, func() (bool, string) {
		nodes := api.nodes()
		for _, n := range nodes {
			if n.label(labelKubeNode) == node {
				nodeInfo = n
				return n.State == "online" && n.GPUCount == 1 && n.RuntimeDriver == "kube" && n.label(labelKubeCluster) == created.UUID,
					fmt.Sprintf("state=%s gpu_count=%d runtime_driver=%q cluster=%q", n.State, n.GPUCount, n.RuntimeDriver, n.label(labelKubeCluster))
			}
		}
		return false, fmt.Sprintf("no node with system label %s=%s among %d node(s)", labelKubeNode, node, len(nodes))
	})
	if nodeInfo.Scheduling != "schedulable" {
		t.Fatalf("freshly enrolled node should be schedulable: %+v", nodeInfo)
	}

	// ---- 8. model + deployment.
	step(t, "create the model directory inside the node and register it")
	mustRunShell(t, node, time.Minute, fmt.Sprintf("mkdir -p %[1]s && cat > %[1]s/config.json <<'EOF'\n%[2]s\nEOF\nprintf fixture > %[1]s/model.safetensors", modelDirInNode, qwen3TinyConfigJSON))
	var model struct {
		ID uint `json:"id"`
	}
	api.mustJSON(http.MethodPost, "/api/v1/models/local",
		map[string]any{"node_uuid": nodeInfo.UUID, "path": modelDirInNode, "name": "kind-model"}, http.StatusCreated, &model)
	step(t, "create the deployment")
	var deployment struct {
		ID uint `json:"id"`
	}
	api.mustJSON(http.MethodPost, "/api/v1/deployments", map[string]any{
		"model_id": model.ID, "node_uuid": nodeInfo.UUID, "served_name": servedName,
		"engine": "vllm", "image": fakeEngineImage, "sleepable": true,
	}, http.StatusCreated, &deployment)
	deploymentPath := fmt.Sprintf("/api/v1/deployments/%d", deployment.ID)

	// ---- 9. instance pod and chat.
	var instance apiInstance
	eventually(t, "the deployment's instance is running", 5*time.Minute, time.Second, func() (bool, string) {
		for _, in := range api.instances() {
			if in.DeploymentID == deployment.ID {
				instance = in
				return in.State == "running", fmt.Sprintf("state=%s sleep_level=%d message=%q", in.State, in.SleepLevel, in.Message)
			}
		}
		return false, "no instance yet"
	})
	podName := "gpuworks-" + instance.UUID[:12]
	pod := k.pod(podName)
	if pod.Spec.NodeName != node || pod.Spec.RestartPolicy != "Never" || !pod.Spec.HostNetwork {
		t.Fatalf("instance pod %s: nodeName=%q restartPolicy=%q hostNetwork=%v, want %q/Never/true",
			podName, pod.Spec.NodeName, pod.Spec.RestartPolicy, pod.Spec.HostNetwork, node)
	}
	if len(pod.Metadata.OwnerReferences) != 1 || pod.Metadata.OwnerReferences[0].Kind != "DaemonSet" ||
		pod.Metadata.OwnerReferences[0].Name != "gpuworks-agent" {
		t.Fatalf("instance pod %s ownerReferences = %+v, want the gpuworks-agent DaemonSet", podName, pod.Metadata.OwnerReferences)
	}
	// The entry's PriorityClass, named for its namespace, with the default
	// priority and no preemption.
	if pod.Spec.PriorityClassName != "gpuworks-inference-"+kubeNamespace {
		t.Fatalf("instance pod %s priorityClassName = %q", podName, pod.Spec.PriorityClassName)
	}
	if pc := strings.TrimSpace(k.mustRun(time.Minute, "get", "priorityclass", pod.Spec.PriorityClassName, "-o", "jsonpath={.value} {.preemptionPolicy}")); pc != "10000 Never" {
		t.Fatalf("PriorityClass %s = %q, want \"10000 Never\"", pod.Spec.PriorityClassName, pc)
	}
	instancePodUID := pod.Metadata.UID
	if instancePodUID == "" {
		t.Fatalf("instance pod %s has no uid", podName)
	}
	var key struct {
		Key string `json:"key"`
	}
	api.mustJSON(http.MethodPost, "/api/v1/apikeys", map[string]any{"name": "l3kind"}, http.StatusCreated, &key)
	if key.Key == "" {
		t.Fatal("create api key returned no plaintext key")
	}
	step(t, "chat through the gateway")
	api.chat(key.Key, "first chat")

	// ---- 10. sleep and wake.
	step(t, "sleep the deployment")
	api.mustJSON(http.MethodPost, deploymentPath+"/sleep?level=1", nil, http.StatusOK, nil)
	api.waitSleepLevel(instance.UUID, 1)
	step(t, "wake the deployment")
	api.mustJSON(http.MethodPost, deploymentPath+"/wake", nil, http.StatusOK, nil)
	api.waitSleepLevel(instance.UUID, 0)
	api.chat(key.Key, "chat after wake")

	// ---- 11. agent restart: the instance pod is adopted, not recreated.
	oldAgent := k.agentPods()
	if len(oldAgent) != 1 {
		t.Fatalf("expected exactly one agent pod before the restart, got %d", len(oldAgent))
	}
	step(t, "delete the agent pod %s", oldAgent[0].Metadata.Name)
	k.mustRun(2*time.Minute, "-n", kubeNamespace, "delete", "pod", "-l", agentLabelSelector, "--wait=true")
	eventually(t, "a replacement agent pod is Running and Ready", 3*time.Minute, time.Second, func() (bool, string) {
		pods := k.agentPods()
		if len(pods) != 1 {
			return false, fmt.Sprintf("%d agent pod(s)", len(pods))
		}
		p := pods[0]
		return p.Metadata.UID != oldAgent[0].Metadata.UID && p.ready(), fmt.Sprintf("pod=%s phase=%s ready=%v", p.Metadata.Name, p.Status.Phase, p.ready())
	})
	eventually(t, "node online again with the same instance running", 3*time.Minute, time.Second, func() (bool, string) {
		for _, n := range api.nodes() {
			if n.UUID == nodeInfo.UUID && n.State != "online" {
				return false, "node state=" + n.State
			}
		}
		for _, in := range api.instances() {
			if in.UUID == instance.UUID {
				return in.State == "running" && in.SleepLevel == 0, fmt.Sprintf("instance state=%s sleep_level=%d", in.State, in.SleepLevel)
			}
		}
		return false, "instance missing from the API"
	})
	if after := k.pod(podName); after.Metadata.UID != instancePodUID {
		t.Fatalf("instance pod %s was recreated across the agent restart: uid %s -> %s", podName, instancePodUID, after.Metadata.UID)
	}
	api.chat(key.Key, "chat after agent restart")

	// ---- 12. cordon / uncordon.
	step(t, "kubectl cordon %s", node)
	k.mustRun(time.Minute, "cordon", node)
	// Kubernetes' cordon is the platform's own cordon reason.
	waitScheduling := func(want, reason string) {
		eventually(t, "node scheduling "+want, 90*time.Second, time.Second, func() (bool, string) {
			for _, n := range api.nodes() {
				if n.UUID == nodeInfo.UUID {
					return n.Scheduling == want && n.CordonReason == reason,
						fmt.Sprintf("scheduling=%s cordon_reason=%q", n.Scheduling, n.CordonReason)
				}
			}
			return false, "node missing"
		})
	}
	waitScheduling("cordoned", "kube_unschedulable")
	step(t, "kubectl uncordon %s", node)
	k.mustRun(time.Minute, "uncordon", node)
	waitScheduling("schedulable", "")

	// ---- 13. delete the deployment; the instance pod goes away.
	step(t, "delete the deployment")
	api.mustJSON(http.MethodDelete, deploymentPath, nil, 0, nil)
	eventually(t, "instance pod "+podName+" is gone", 3*time.Minute, time.Second, func() (bool, string) {
		out, err := k.run(time.Minute, "-n", kubeNamespace, "get", "pod", podName, "-o", "name")
		if err != nil && strings.Contains(out, "NotFound") {
			return true, ""
		}
		return false, strings.TrimSpace(out)
	})

	// ---- 14. uninstall with the same manifest.
	step(t, "kubectl delete -f the manifest URL")
	k.mustShell(5*time.Minute, "kubectl delete -f '"+manifestURL+"' --wait=true --timeout=240s")
	eventually(t, "namespace "+kubeNamespace+" is gone", 3*time.Minute, time.Second, func() (bool, string) {
		out, err := k.run(time.Minute, "get", "namespace", kubeNamespace, "-o", "name")
		if err != nil && strings.Contains(out, "NotFound") {
			return true, ""
		}
		return false, strings.TrimSpace(out)
	})
	leftover := k.allPodNames()
	for _, name := range leftover {
		if strings.HasPrefix(name, "gpuworks-") {
			t.Fatalf("pod %s still exists after the manifest was deleted (all pods: %v)", name, leftover)
		}
	}
}

// ---- environment ----------------------------------------------------------

var httpClient = &http.Client{
	Timeout:   10 * time.Second,
	Transport: &http.Transport{Proxy: nil},
}

func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

func repoRoot() string {
	dir, err := os.Getwd()
	if err != nil {
		return "."
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "."
		}
		dir = parent
	}
}

// unavailable skips the case, or fails it under GPUWORKS_TEST_STRICT=1.
func unavailable(t *testing.T, format string, args ...any) {
	t.Helper()
	if os.Getenv("GPUWORKS_TEST_STRICT") == "1" {
		t.Fatalf(format+" (GPUWORKS_TEST_STRICT=1)", args...)
	}
	t.Skipf(format, args...)
}

// requireTools returns the kind binary and checks docker and both images.
func requireTools(t *testing.T, agentImage string) string {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		unavailable(t, "docker unavailable: %v", err)
	}
	if out, err := command(30*time.Second, "docker", "info"); err != nil {
		unavailable(t, "docker daemon unavailable: %v\n%s", err, out)
	}
	kindBin, err := exec.LookPath("kind")
	if err != nil {
		if gobin, goErr := exec.Command("go", "env", "GOPATH").Output(); goErr == nil {
			candidate := filepath.Join(strings.TrimSpace(string(gobin)), "bin", "kind")
			if _, statErr := os.Stat(candidate); statErr == nil {
				kindBin, err = candidate, nil
			}
		}
	}
	if err != nil {
		unavailable(t, "kind unavailable (not on PATH or in $(go env GOPATH)/bin): %v", err)
	}
	for _, image := range []string{agentImage, fakeEngineImage} {
		if out, err := command(30*time.Second, "docker", "image", "inspect", image, "--format", "{{.Id}}"); err != nil {
			unavailable(t, "image %s is not in the local docker daemon (run `make test-l3-kind`): %v\n%s", image, err, out)
		}
	}
	return kindBin
}

// noProxyEnv keeps proxies away from loopback and the docker bridge networks
// the kind nodes and the server talk over.
func noProxyEnv() []string {
	extra := "127.0.0.1,localhost,172.16.0.0/12,10.0.0.0/8,192.168.0.0/16,.svc,.cluster.local"
	env := os.Environ()
	for _, name := range []string{"NO_PROXY", "no_proxy"} {
		value := extra
		if existing := os.Getenv(name); existing != "" {
			value = existing + "," + extra
		}
		env = append(env, name+"="+value)
	}
	return env
}

func command(timeout time.Duration, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = noProxyEnv()
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	if ctx.Err() != nil {
		err = fmt.Errorf("%w (timed out after %s)", err, timeout)
	}
	return out.String(), err
}

func mustRun(t *testing.T, timeout time.Duration, name string, args ...string) string {
	t.Helper()
	out, err := command(timeout, name, args...)
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, tail(out, 4000))
	}
	return out
}

func mustRunShell(t *testing.T, node string, timeout time.Duration, script string) {
	t.Helper()
	mustRun(t, timeout, "docker", "exec", node, "sh", "-c", script)
}

func step(t *testing.T, format string, args ...any) {
	t.Helper()
	t.Logf("[%s] "+format, append([]any{time.Now().Format("15:04:05")}, args...)...)
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "..." + s[len(s)-n:]
}

// eventually polls check until it reports true. check returns a short
// description of the current state, which the failure message carries.
func eventually(t *testing.T, what string, timeout, interval time.Duration, check func() (bool, string)) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	last := ""
	for {
		ok, state := check()
		if ok {
			return
		}
		last = state
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for: %s (last state: %s)", timeout, what, last)
		}
		time.Sleep(interval)
	}
}

func freePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return fmt.Sprint(ln.Addr().(*net.TCPAddr).Port)
}

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
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()
		<-done
	}
}

// kindGatewayIPv4 is the host address on the docker network kind nodes use.
func kindGatewayIPv4(t *testing.T) string {
	t.Helper()
	out := mustRun(t, 30*time.Second, "docker", "network", "inspect", "kind", "-f", "{{range .IPAM.Config}}{{.Gateway}} {{end}}")
	for _, field := range strings.Fields(out) {
		if ip := net.ParseIP(field); ip != nil && ip.To4() != nil {
			return field
		}
	}
	t.Fatalf("docker network kind has no IPv4 gateway: %q", out)
	return ""
}

// ---- kubectl inside the kind node ----------------------------------------

type kubectl struct {
	t    *testing.T
	node string
}

func (k *kubectl) run(timeout time.Duration, args ...string) (string, error) {
	full := append([]string{"exec", "-e", "KUBECONFIG=" + nodeKubeconfig, k.node, "kubectl"}, args...)
	return command(timeout, "docker", full...)
}

func (k *kubectl) mustRun(timeout time.Duration, args ...string) string {
	k.t.Helper()
	out, err := k.run(timeout, args...)
	if err != nil {
		k.t.Fatalf("kubectl %s: %v\n%s", strings.Join(args, " "), err, tail(out, 4000))
	}
	return out
}

// mustShell runs a shell command line (an apply_command, say) in the node.
func (k *kubectl) mustShell(timeout time.Duration, line string) string {
	k.t.Helper()
	out, err := command(timeout, "docker", "exec", "-e", "KUBECONFIG="+nodeKubeconfig, k.node, "sh", "-c", line)
	if err != nil {
		k.t.Fatalf("%s: %v\n%s", line, err, tail(out, 4000))
	}
	return out
}

func (k *kubectl) waitDaemonSetReady(when string) {
	k.t.Helper()
	eventually(k.t, "DaemonSet gpuworks-agent has its pod ready "+when, 3*time.Minute, time.Second, func() (bool, string) {
		out, err := k.run(time.Minute, "-n", kubeNamespace, "get", "ds", "gpuworks-agent", "-o", "json")
		if err != nil {
			return false, strings.TrimSpace(out)
		}
		var ds struct {
			Metadata struct {
				Generation int64 `json:"generation"`
			} `json:"metadata"`
			Status struct {
				ObservedGeneration     int64 `json:"observedGeneration"`
				DesiredNumberScheduled int   `json:"desiredNumberScheduled"`
				UpdatedNumberScheduled int   `json:"updatedNumberScheduled"`
				NumberReady            int   `json:"numberReady"`
			} `json:"status"`
		}
		if err := json.Unmarshal([]byte(out), &ds); err != nil {
			return false, err.Error()
		}
		s := ds.Status
		return s.ObservedGeneration == ds.Metadata.Generation && s.DesiredNumberScheduled == 1 && s.UpdatedNumberScheduled == 1 && s.NumberReady == 1,
			fmt.Sprintf("desired=%d updated=%d ready=%d", s.DesiredNumberScheduled, s.UpdatedNumberScheduled, s.NumberReady)
	})
}

type kubePod struct {
	Metadata struct {
		Name            string `json:"name"`
		UID             string `json:"uid"`
		OwnerReferences []struct {
			Kind string `json:"kind"`
			Name string `json:"name"`
		} `json:"ownerReferences"`
	} `json:"metadata"`
	Spec struct {
		NodeName          string `json:"nodeName"`
		RestartPolicy     string `json:"restartPolicy"`
		HostNetwork       bool   `json:"hostNetwork"`
		PriorityClassName string `json:"priorityClassName"`
	} `json:"spec"`
	Status struct {
		Phase      string `json:"phase"`
		Conditions []struct {
			Type   string `json:"type"`
			Status string `json:"status"`
		} `json:"conditions"`
	} `json:"status"`
}

func (p kubePod) ready() bool {
	if p.Status.Phase != "Running" {
		return false
	}
	for _, c := range p.Status.Conditions {
		if c.Type == "Ready" {
			return c.Status == "True"
		}
	}
	return false
}

func (k *kubectl) pod(name string) kubePod {
	k.t.Helper()
	out := k.mustRun(time.Minute, "-n", kubeNamespace, "get", "pod", name, "-o", "json")
	var p kubePod
	if err := json.Unmarshal([]byte(out), &p); err != nil {
		k.t.Fatalf("decode pod %s: %v\n%s", name, err, out)
	}
	return p
}

func (k *kubectl) agentPods() []kubePod {
	k.t.Helper()
	out := k.mustRun(time.Minute, "-n", kubeNamespace, "get", "pods", "-l", agentLabelSelector, "-o", "json")
	var list struct {
		Items []kubePod `json:"items"`
	}
	if err := json.Unmarshal([]byte(out), &list); err != nil {
		k.t.Fatalf("decode agent pods: %v\n%s", err, out)
	}
	return list.Items
}

func (k *kubectl) allPodNames() []string {
	k.t.Helper()
	out := k.mustRun(time.Minute, "get", "pods", "-A", "-o", "jsonpath={range .items[*]}{.metadata.name}{\"\\n\"}{end}")
	return strings.Fields(out)
}

// dumpDiagnostics logs the cluster state a failed run needs.
func (k *kubectl) dumpDiagnostics() {
	for _, args := range [][]string{
		{"get", "pods", "-A", "-o", "wide"},
		{"-n", kubeNamespace, "describe", "pods"},
		{"-n", kubeNamespace, "logs", "-l", agentLabelSelector, "--tail=80", "--all-containers"},
		{"-n", kubeNamespace, "get", "events", "--sort-by=.lastTimestamp"},
	} {
		out, err := k.run(time.Minute, args...)
		k.t.Logf("kubectl %s (err=%v):\n%s", strings.Join(args, " "), err, tail(out, 5000))
	}
}

// ---- management API client -----------------------------------------------

type apiClient struct {
	t     *testing.T
	base  string
	token string
}

type apiNode struct {
	UUID          string `json:"uuid"`
	State         string `json:"state"`
	Scheduling    string `json:"scheduling"`
	GPUCount      int    `json:"gpu_count"`
	RuntimeDriver string `json:"runtime_driver"`
	CordonReason  string `json:"cordon_reason"`
	// SystemLabels carry the node's Kubernetes placement
	// (gpuworks.ai/kubernetes-cluster, gpuworks.ai/kubernetes-node).
	SystemLabels []struct {
		Key   string `json:"key"`
		Value string `json:"value"`
	} `json:"system_labels"`
}

// label is the node's system label value for key, or "".
func (n apiNode) label(key string) string {
	for _, l := range n.SystemLabels {
		if l.Key == key {
			return l.Value
		}
	}
	return ""
}

const (
	labelKubeCluster = "gpuworks.ai/kubernetes-cluster"
	labelKubeNode    = "gpuworks.ai/kubernetes-node"
)

type apiInstance struct {
	UUID         string `json:"uuid"`
	DeploymentID uint   `json:"deployment_id"`
	State        string `json:"state"`
	SleepLevel   int    `json:"sleep_level"`
	Message      string `json:"message"`
}

func (c *apiClient) login() {
	c.t.Helper()
	eventually(c.t, "admin login", 30*time.Second, 200*time.Millisecond, func() (bool, string) {
		body, _ := json.Marshal(map[string]string{"username": "admin", "password": adminPassword})
		resp, err := httpClient.Post(c.base+"/api/v1/auth/login", "application/json", bytes.NewReader(body))
		if err != nil {
			return false, err.Error()
		}
		defer resp.Body.Close()
		var out struct {
			Token string `json:"token"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&out)
		if resp.StatusCode == http.StatusOK && out.Token != "" {
			c.token = out.Token
			return true, ""
		}
		return false, resp.Status
	})
}

func (c *apiClient) do(method, path string, body any) (int, []byte, error) {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, c.base+path, reader)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, data, nil
}

// mustJSON sends a request and checks the status (any 2xx when want is 0).
func (c *apiClient) mustJSON(method, path string, body any, want int, out any) {
	c.t.Helper()
	status, data, err := c.do(method, path, body)
	if err != nil {
		c.t.Fatalf("%s %s: %v", method, path, err)
	}
	if (want != 0 && status != want) || (want == 0 && (status < 200 || status > 299)) {
		c.t.Fatalf("%s %s: status %d, want %d; body: %s", method, path, status, want, data)
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			c.t.Fatalf("%s %s: decode response: %v\n%s", method, path, err, data)
		}
	}
}

func (c *apiClient) nodes() []apiNode {
	c.t.Helper()
	status, data, err := c.do(http.MethodGet, "/api/v1/nodes", nil)
	if err != nil || status != http.StatusOK {
		c.t.Fatalf("GET /api/v1/nodes: status=%d err=%v body=%s", status, err, data)
	}
	var nodes []apiNode
	if err := apiwire.UnmarshalList(data, &nodes); err != nil {
		c.t.Fatalf("decode /api/v1/nodes: %v\n%s", err, data)
	}
	return nodes
}

func (c *apiClient) instances() []apiInstance {
	c.t.Helper()
	status, data, err := c.do(http.MethodGet, "/api/v1/instances", nil)
	if err != nil || status != http.StatusOK {
		c.t.Fatalf("GET /api/v1/instances: status=%d err=%v body=%s", status, err, data)
	}
	var instances []apiInstance
	if err := apiwire.UnmarshalList(data, &instances); err != nil {
		c.t.Fatalf("decode /api/v1/instances: %v\n%s", err, data)
	}
	return instances
}

func (c *apiClient) waitSleepLevel(instanceUUID string, level int) {
	c.t.Helper()
	eventually(c.t, fmt.Sprintf("instance %s reports sleep level %d", instanceUUID, level), 2*time.Minute, time.Second, func() (bool, string) {
		for _, in := range c.instances() {
			if in.UUID == instanceUUID {
				return in.SleepLevel == level && in.State == "running", fmt.Sprintf("state=%s sleep_level=%d", in.State, in.SleepLevel)
			}
		}
		return false, "instance missing"
	})
}

// chat sends a chat completion through the gateway until it answers 200 with
// choices (the route table may take a moment to include a fresh engine).
func (c *apiClient) chat(apiKey, prompt string) {
	c.t.Helper()
	body, _ := json.Marshal(map[string]any{
		"model": servedName, "messages": []map[string]string{{"role": "user", "content": prompt}},
	})
	eventually(c.t, "chat completion through the gateway ("+prompt+")", 90*time.Second, 2*time.Second, func() (bool, string) {
		req, _ := http.NewRequest(http.MethodPost, c.base+"/v1/chat/completions", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+apiKey)
		req.Header.Set("Content-Type", "application/json")
		resp, err := httpClient.Do(req)
		if err != nil {
			return false, err.Error()
		}
		defer resp.Body.Close()
		data, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			return false, fmt.Sprintf("status %d: %s", resp.StatusCode, tail(string(data), 500))
		}
		if !bytes.Contains(data, []byte(`"choices"`)) {
			return false, "200 without choices: " + tail(string(data), 500)
		}
		return true, ""
	})
}

func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		return s[i+1:]
	}
	return s
}
