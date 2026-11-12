//go:build kind

package l3kind

import (
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/agentbin"
	"gpuworks.ai/gpuworks/internal/server/installcred"
	"gpuworks.ai/gpuworks/internal/testenv/apiwire"
)

// stateBinary is where the image's agent hands over to (internal/agent/kubeboot):
// the manifest's state directory, /opt/gpuworks.
const stateBinary = "/opt/gpuworks/agent/gpuworks-agent"

// TestKubeAgentUpgradesInPlaceKind proves that a DaemonSet agent runs from
// its node's state directory and is upgraded there like an installed agent:
// the image's agent hands over to a binary it seeds on the node, takes the
// server's own binary before it registers, and a later server's upgrade
// replaces that binary in place, with the pod and the DaemonSet's image left
// as they are, the upgraded binary outliving the pod.
//
// Like TestKubeDaemonSetDeployChatKind it needs docker, kind and the agent
// image (`make test-l3-kind` builds it). Run it alone with:
//
//	make agent-image AGENT_IMAGE=gpuworks/gpuworks-agent:l3kind
//	GPUWORKS_KIND_AGENT_IMAGE=gpuworks/gpuworks-agent:l3kind CGO_ENABLED=0 \
//	  go test -count=1 -v -tags=kind -timeout=20m ./test/l3kind/ -run '^TestKubeAgentUpgradesInPlaceKind$'
func TestKubeAgentUpgradesInPlaceKind(t *testing.T) {
	agentImage := envOr("GPUWORKS_KIND_AGENT_IMAGE", defaultAgentImage)
	nodeImage := envOr("GPUWORKS_KIND_NODE_IMAGE", defaultKindNodeImg)
	kindBin := requireTools(t, agentImage)

	cluster := fmt.Sprintf("gw-l3up-%d", time.Now().UnixNano()%1_000_000_000)
	node := cluster + "-control-plane"
	t.Cleanup(func() {
		if t.Failed() && os.Getenv("GPUWORKS_KEEP_ON_FAILURE") == "1" {
			t.Logf("GPUWORKS_KEEP_ON_FAILURE=1: kind cluster %s kept; delete with `%s delete cluster --name %s`", cluster, kindBin, cluster)
			return
		}
		if out, err := command(2*time.Minute, kindBin, "delete", "cluster", "--name", cluster); err != nil {
			t.Logf("delete kind cluster %s: %v\n%s", cluster, err, out)
		}
	})
	step(t, "create kind cluster %s (%s)", cluster, nodeImage)
	mustRun(t, 6*time.Minute, kindBin, "create", "cluster", "--name", cluster, "--image", nodeImage, "--wait", "180s")
	step(t, "kind load docker-image %s", agentImage)
	mustRun(t, 5*time.Minute, kindBin, "load", "docker-image", agentImage, "--name", cluster)
	k := &kubectl{t: t, node: node}
	t.Cleanup(func() {
		if t.Failed() {
			k.dumpDiagnostics()
		}
	})

	// ---- the server, at a version of its own, with its agent binary.
	root := repoRoot()
	dataDir := t.TempDir()
	if err := installcred.WriteInitialCredentials(dataDir, "admin", adminPassword); err != nil {
		t.Fatalf("write initial credentials: %v", err)
	}
	port := freePort(t)
	base := "http://127.0.0.1:" + port
	server := startVersionedServer(t, root, dataDir, port, "l3kind-one")
	api := &apiClient{t: t, base: base}
	api.login()
	serverURL := "http://" + kindGatewayIPv4(t) + ":" + port

	var created struct {
		UUID         string `json:"uuid"`
		ApplyCommand string `json:"apply_command"`
	}
	step(t, "create the cluster and apply its manifest")
	api.mustJSON(http.MethodPost, "/api/v1/kube/clusters", map[string]any{
		"name": "l3up", "server_url": serverURL, "agent_image": agentImage,
	}, http.StatusCreated, &created)
	k.mustShell(2*time.Minute, created.ApplyCommand)
	k.waitDaemonSetReady("after the apply")

	// ---- the image's agent hands over and takes the server's binary.
	node1 := waitAgent(t, api, node, "l3kind-one")
	if !slices.Contains(node1.Caps, proto.CapKubeBinaryUpgrade) {
		t.Fatalf("the agent does not run from the state directory: caps %v", node1.Caps)
	}
	pods := k.agentPods()
	if len(pods) != 1 {
		t.Fatalf("%d agent pods, want 1", len(pods))
	}
	pod := pods[0]
	// The pod has hostPID: the agent is found among the node's processes.
	procs := mustRun(t, time.Minute, "docker", "exec", node, "sh", "-c",
		`for p in /proc/[0-9]*; do tr '\0' ' ' < "$p/cmdline" 2>/dev/null; echo; done | grep -- '--driver=kube' || true`)
	if !strings.Contains(procs, stateBinary+" run ") {
		t.Fatalf("no agent runs as the state directory's %s; agent processes:\n%s", stateBinary, procs)
	}
	dsImage := strings.TrimSpace(k.mustRun(time.Minute, "-n", kubeNamespace, "get", "ds", "gpuworks-agent", "-o", "jsonpath={.spec.template.spec.containers[0].image}"))
	if dsImage != agentImage {
		t.Fatalf("DaemonSet image = %q, want %q", dsImage, agentImage)
	}

	// ---- a newer server upgrades the agent in place.
	step(t, "restart the server at a newer version")
	stopProcess(server)
	startVersionedServer(t, root, dataDir, port, "l3kind-two")
	api.login()
	waitAgent(t, api, node, "l3kind-one")
	step(t, "upgrade the node")
	status, body, err := api.do(http.MethodPost, "/api/v1/nodes/"+node1.UUID+"/upgrade", nil)
	if err != nil || status != http.StatusAccepted || !strings.Contains(string(body), `"status":"upgrading"`) {
		t.Fatalf("upgrade = %d %s %v; want the in-place upgrade, not a DaemonSet roll", status, body, err)
	}
	waitAgent(t, api, node, "l3kind-two")
	after := k.agentPods()
	if len(after) != 1 || after[0].Metadata.UID != pod.Metadata.UID {
		t.Fatalf("the agent pod was replaced by the upgrade: %v -> %v", pod.Metadata.UID, after)
	}
	if got := strings.TrimSpace(k.mustRun(time.Minute, "-n", kubeNamespace, "get", "ds", "gpuworks-agent", "-o", "jsonpath={.spec.template.spec.containers[0].image}")); got != dsImage {
		t.Fatalf("the upgrade changed the DaemonSet image: %q -> %q", dsImage, got)
	}

	// ---- the upgraded binary outlives the pod.
	step(t, "delete the agent pod")
	k.mustRun(2*time.Minute, "-n", kubeNamespace, "delete", "pod", "-l", agentLabelSelector, "--wait=true")
	eventually(t, "a replacement agent pod is ready", 3*time.Minute, time.Second, func() (bool, string) {
		pods := k.agentPods()
		if len(pods) != 1 {
			return false, fmt.Sprintf("%d agent pod(s)", len(pods))
		}
		return pods[0].Metadata.UID != pod.Metadata.UID && pods[0].ready(), "phase=" + pods[0].Status.Phase
	})
	waitAgent(t, api, node, "l3kind-two")
}

type upgradeNode struct {
	UUID         string   `json:"uuid"`
	State        string   `json:"state"`
	AgentVersion string   `json:"agent_version"`
	Caps         []string `json:"caps"`
	SystemLabels []struct {
		Key   string `json:"key"`
		Value string `json:"value"`
	} `json:"system_labels"`
}

// waitAgent waits for the kind node's agent online at version.
func waitAgent(t *testing.T, api *apiClient, kubeNode, version string) upgradeNode {
	t.Helper()
	var found upgradeNode
	eventually(t, "the node's agent online at "+version, 4*time.Minute, time.Second, func() (bool, string) {
		var nodes []upgradeNode
		status, body, err := api.do(http.MethodGet, "/api/v1/nodes", nil)
		if err != nil || status != http.StatusOK {
			return false, fmt.Sprintf("nodes = %d %v", status, err)
		}
		if err := apiwire.UnmarshalList(body, &nodes); err != nil {
			return false, err.Error()
		}
		for _, n := range nodes {
			for _, l := range n.SystemLabels {
				if l.Key == labelKubeNode && l.Value == kubeNode {
					found = n
					return n.State == "online" && n.AgentVersion == version, fmt.Sprintf("state=%s agent_version=%s caps=%v", n.State, n.AgentVersion, n.Caps)
				}
			}
		}
		return false, "no node for " + kubeNode
	})
	return found
}

// startVersionedServer builds the server and the node's agent at version,
// stages the agent where the server serves and upgrades from (<data>/dl, with
// the build manifest an upgrade verifies), and starts the server.
func startVersionedServer(t *testing.T, root, dataDir, port, version string) *exec.Cmd {
	t.Helper()
	ldflags := "-X gpuworks.ai/gpuworks/internal/version.Version=" + version
	binDir := t.TempDir()
	serverBin := filepath.Join(binDir, "gpuworks")
	dl := filepath.Join(dataDir, "dl")
	if err := os.MkdirAll(dl, 0o755); err != nil {
		t.Fatal(err)
	}
	agent := filepath.Join(dl, proto.AgentBinaryName("linux", runtime.GOARCH))
	step(t, "build the server and agent at %s", version)
	for _, b := range []struct {
		out, pkg, goos string
	}{{serverBin, "./cmd/gpuworks", runtime.GOOS}, {agent, "./cmd/gpuworks-agent", "linux"}} {
		cmd := exec.Command("go", "build", "-trimpath", "-ldflags", ldflags, "-o", b.out, b.pkg)
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS="+b.goos)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("go build %s: %v\n%s", b.pkg, err, out)
		}
	}
	if err := agentbin.WriteManifest(version, filepath.Join(dl, agentbin.ManifestName), []string{agent}); err != nil {
		t.Fatalf("write the agent manifest: %v", err)
	}
	log, err := os.OpenFile(filepath.Join(dataDir, "server.out.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	server := exec.Command(serverBin, "serve", "--addr", "0.0.0.0:"+port, "--data", dataDir, "--recovery-enabled=false", "--log-level", "info")
	server.Env = os.Environ()
	server.Stdout, server.Stderr = log, log
	if err := server.Start(); err != nil {
		t.Fatalf("start gpuworks serve: %v", err)
	}
	t.Cleanup(func() {
		stopProcess(server)
		log.Close()
		if t.Failed() {
			if data, err := os.ReadFile(log.Name()); err == nil {
				t.Logf("server log tail:\n%s", tail(string(data), 6000))
			}
		}
	})
	eventually(t, "server "+version+" answers", time.Minute, 200*time.Millisecond, func() (bool, string) {
		resp, err := httpClient.Get("http://127.0.0.1:" + port + "/api/v1/healthz")
		if err != nil {
			return false, err.Error()
		}
		resp.Body.Close()
		return resp.StatusCode == http.StatusUnauthorized, resp.Status
	})
	return server
}

// stageAgent builds the node's agent from this tree into dl, where the
// server serves /dl/ from.
func stageAgent(t *testing.T, root, dl string) {
	t.Helper()
	if err := os.MkdirAll(dl, 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("go", "build", "-trimpath", "-o", filepath.Join(dl, proto.AgentBinaryName("linux", runtime.GOARCH)), "./cmd/gpuworks-agent")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build ./cmd/gpuworks-agent: %v\n%s", err, out)
	}
}
