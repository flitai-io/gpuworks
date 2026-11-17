//go:build cloudgpu && (compshare || aliyun)

package l4

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/testenv/apiwire"
	"gpuworks.ai/gpuworks/internal/testenv/cloudprovider"
)

func TestHostedControlPlaneCommandsStayOnPrivateNetwork(t *testing.T) {
	serverURL, err := hostedPrivateURL("10.42.0.8", hostedControlPlanePort)
	if err != nil || serverURL != "http://10.42.0.8:18880" {
		t.Fatalf("private URL=%q err=%v", serverURL, err)
	}
	server, err := hostedServerLaunchScript(hostedServerLaunch{
		Root: "/tmp/hosted-run", PrivateIP: "10.42.0.8", Port: hostedControlPlanePort,
		AdminPassword: "fixture-secret",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{
		"--addr '10.42.0.8:18880'",
		`--recovery-enabled=false`,
		`"$root/data/initial-credentials"`,
		`"$root/data"`,
		`"$root/log/server.log"`,
	} {
		if !strings.Contains(server, required) {
			t.Fatalf("server launcher missing %q:\n%s", required, server)
		}
	}
	// --public-url is gone from `gpuworks serve` (the server no longer keeps an
	// external address); passing it makes the binary exit with "flag provided
	// but not defined" before it listens.
	for _, forbidden := range []string{"0.0.0.0", "--password", "3389", "export GPUWORKS_", "--public-url"} {
		if strings.Contains(server, forbidden) {
			t.Fatalf("server launcher exposed forbidden value %q", forbidden)
		}
	}

	agent, err := hostedAgentLaunchScript(hostedAgentLaunch{
		Root: "/tmp/hosted-run", Rank: 1, ServerURL: serverURL,
		Token: "node-token", RuntimeDriver: "udocker",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{
		"GPUWORKS_SERVER='http://10.42.0.8:18880'",
		"--connect-mode agent_dial",
		"GPUWORKS_RUNTIME_DRIVER='udocker'",
		"agent-rank1",
		// Upload makes the root 0700; the dedicated udocker user of a root agent
		// must be able to traverse it to reach PRoot.
		`chmod 711 "$root"`,
	} {
		if !strings.Contains(agent, required) {
			t.Fatalf("agent launcher missing %q:\n%s", required, agent)
		}
	}
	if strings.Contains(agent, "127.0.0.1") || strings.Contains(agent, "server_dial") {
		t.Fatalf("cloud agent launcher depends on a local/server_dial tunnel:\n%s", agent)
	}
	if strings.Contains(agent, "--token") {
		t.Fatalf("cloud agent exposed its node token in process arguments:\n%s", agent)
	}
}

func TestHostedPrivateURLRejectsPublicAndListeningHazards(t *testing.T) {
	for _, test := range []struct {
		ip   string
		port int
	}{
		{ip: "203.0.113.8", port: hostedControlPlanePort},
		{ip: "0.0.0.0", port: hostedControlPlanePort},
		{ip: "127.0.0.1", port: hostedControlPlanePort},
		{ip: "10.0.0.8", port: 80},
	} {
		if got, err := hostedPrivateURL(test.ip, test.port); err == nil {
			t.Fatalf("unsafe endpoint %s:%d accepted as %q", test.ip, test.port, got)
		}
	}
}

func TestHostedAPIClientRebuildsForwardAndReauthenticates(t *testing.T) {
	const token = "fixture-admin-token"
	var logins, reads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/auth/login":
			logins.Add(1)
			var credentials map[string]string
			_ = json.NewDecoder(r.Body).Decode(&credentials)
			if credentials["username"] != "admin" || credentials["password"] != "secret" {
				http.Error(w, "bad credentials", http.StatusUnauthorized)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"token": token})
		case "/api/v1/nodes":
			if r.Header.Get("Authorization") != "Bearer "+token {
				http.Error(w, "missing bearer", http.StatusUnauthorized)
				return
			}
			reads.Add(1)
			_, _ = w.Write([]byte(`{"items":[{"name":"rank0","state":"online"}],"total":1}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	var factories atomic.Int32
	factory := func(context.Context) (hostedForward, error) {
		attempt := factories.Add(1)
		if attempt == 1 {
			return &fixtureHostedForward{url: "http://127.0.0.1:1"}, nil
		}
		return &fixtureHostedForward{url: server.URL}, nil
	}
	client := newHostedAPIClient(factory, "admin", "secret")
	defer client.Close()
	var nodes apiwire.List[map[string]any]
	if err := client.GetJSON(context.Background(), "/api/v1/nodes", &nodes); err != nil {
		t.Fatal(err)
	}
	if factories.Load() != 2 || logins.Load() != 1 || reads.Load() != 1 || len(nodes.Items) != 1 {
		t.Fatalf("forward rebuild factories=%d logins=%d reads=%d nodes=%v", factories.Load(), logins.Load(), reads.Load(), nodes)
	}
	client.DropForward()
	if err := client.GetJSON(context.Background(), "/api/v1/nodes", &nodes); err != nil {
		t.Fatal(err)
	}
	if factories.Load() != 3 || logins.Load() != 2 || reads.Load() != 2 {
		t.Fatalf("explicit forward rebuild factories=%d logins=%d reads=%d", factories.Load(), logins.Load(), reads.Load())
	}
}

func TestHostedAPIClientRetriesIncompleteGETJSON(t *testing.T) {
	const token = "fixture-admin-token"
	var logins atomic.Int32
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/auth/login":
			logins.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]string{"token": token})
		case "/api/v1/nodes":
			_, _ = w.Write([]byte(`{"items":[{"name":"rank0"`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer broken.Close()
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/auth/login":
			logins.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]string{"token": token})
		case "/api/v1/nodes":
			if r.Header.Get("Authorization") != "Bearer "+token {
				http.Error(w, "missing bearer", http.StatusUnauthorized)
				return
			}
			_, _ = w.Write([]byte(`{"items":[{"name":"rank0","state":"online"}],"total":1}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer good.Close()

	var factories atomic.Int32
	factory := func(context.Context) (hostedForward, error) {
		if factories.Add(1) == 1 {
			return &fixtureHostedForward{url: broken.URL}, nil
		}
		return &fixtureHostedForward{url: good.URL}, nil
	}
	client := newHostedAPIClient(factory, "admin", "secret")
	defer client.Close()
	var nodes apiwire.List[map[string]any]
	if err := client.GetJSON(context.Background(), "/api/v1/nodes", &nodes); err != nil {
		t.Fatal(err)
	}
	if factories.Load() != 2 || logins.Load() != 2 || len(nodes.Items) != 1 || nodes.Items[0]["state"] != "online" {
		t.Fatalf("incomplete JSON retry factories=%d logins=%d nodes=%v", factories.Load(), logins.Load(), nodes)
	}
}

func TestHostedAPIClientClearsOmittedFieldsBetweenPolls(t *testing.T) {
	const token = "fixture-admin-token"
	var reads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/auth/login":
			_ = json.NewEncoder(w).Encode(map[string]string{"token": token})
		case "/api/v1/deployments/1":
			if r.Header.Get("Authorization") != "Bearer "+token {
				http.Error(w, "missing bearer", http.StatusUnauthorized)
				return
			}
			if reads.Add(1) == 1 {
				_, _ = w.Write([]byte(`{"id":1,"instances":[{"uuid":"follower","node_rank":1,"headless":true}]}`))
				return
			}
			// node_rank=0 and headless=false are omitted by the real API's
			// omitempty tags. A reused DTO must not retain the prior 1/true.
			_, _ = w.Write([]byte(`{"id":1,"instances":[{"uuid":"leader"}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := newHostedAPIClient(func(context.Context) (hostedForward, error) {
		return &fixtureHostedForward{url: server.URL}, nil
	}, "admin", "secret")
	defer client.Close()

	var deployment dto.Deployment
	if err := client.GetJSON(context.Background(), "/api/v1/deployments/1", &deployment); err != nil {
		t.Fatal(err)
	}
	if len(deployment.Instances) != 1 || deployment.Instances[0].NodeRank != 1 || !deployment.Instances[0].Headless {
		t.Fatalf("first poll=%+v, want follower metadata", deployment.Instances)
	}
	if err := client.GetJSON(context.Background(), "/api/v1/deployments/1", &deployment); err != nil {
		t.Fatal(err)
	}
	if len(deployment.Instances) != 1 || deployment.Instances[0].UUID != "leader" || deployment.Instances[0].NodeRank != 0 || deployment.Instances[0].Headless {
		t.Fatalf("second poll retained stale follower metadata: %+v", deployment.Instances)
	}
}

func TestHostedCleanupTargetsOnlyHarnessPIDFiles(t *testing.T) {
	command := hostedStopCommand("/tmp/gpuworks-hosted-control-run-1", true)
	for _, required := range []string{"agent-rank*.pid", "server.pid", `kill "$pid"`} {
		if !strings.Contains(command, required) {
			t.Fatalf("cleanup missing %q: %s", required, command)
		}
	}
	for _, forbidden := range []string{"pkill", "killall", "rm -rf", "/tmp/*"} {
		if strings.Contains(command, forbidden) {
			t.Fatalf("cleanup contains broad operation %q: %s", forbidden, command)
		}
	}
}

func TestHostedPrivatePeerRanksAllowsOneVMLogicalTopology(t *testing.T) {
	for _, tc := range []struct {
		name    string
		vmCount int
		want    []int
	}{
		{name: "one VM has no remote peer", vmCount: 1, want: nil},
		{name: "two VMs probe rank one", vmCount: 2, want: []int{1}},
		{name: "all non-server VMs are peers", vmCount: 4, want: []int{1, 2, 3}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := hostedPrivatePeerRanks(tc.vmCount)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("hostedPrivatePeerRanks(%d)=%v, want %v", tc.vmCount, got, tc.want)
			}
		})
	}
}

func TestCompshareDistributedImageStaysFrameworkCompatible(t *testing.T) {
	t.Setenv("COMPSHARE_DISTRIBUTED_TEST_IMAGE", "")
	t.Setenv("COMPSHARE_TEST_IMAGE", "")
	image := compshareDistributedTestImage()
	if image != "gpuworks/gpuworks-runner:cuda12.9-vllm0.31.0-r2" {
		t.Fatalf("distributed image %q does not match the CompShare node framework", image)
	}
}

func TestCompshareDockerInstallPrefersGoldenImage(t *testing.T) {
	secrets := map[string]string{
		"COMPSHARE_IMAGE_ID":        "plain-image",
		"COMPSHARE_GOLDEN_IMAGE_ID": "golden-image",
	}
	if got := compshareImageID(secrets); got != "golden-image" {
		t.Fatalf("CompShare image=%q, want golden-image", got)
	}
	delete(secrets, "COMPSHARE_GOLDEN_IMAGE_ID")
	if got := compshareImageID(secrets); got != "plain-image" {
		t.Fatalf("CompShare fallback image=%q, want plain-image", got)
	}
}

func TestCompshareGPUSelectionPrefers3090AndFallsBackTo4090(t *testing.T) {
	shape := func(gpu int, available bool) cloudprovider.CapacitySpec {
		return cloudprovider.CapacitySpec{GPU: gpu, CPU: gpu * 16, MemoryGB: gpu * 64, ResourceEnough: available}
	}
	tests := []struct {
		name       string
		capacity   map[string][]cloudprovider.CapacitySpec
		gpuCount   int
		want       string
		wantChecks []string
	}{
		{
			name: "prefer 3090 when both are available", gpuCount: 1, want: "3090", wantChecks: []string{"3090"},
			capacity: map[string][]cloudprovider.CapacitySpec{"3090": {shape(1, true)}, "4090": {shape(1, true)}},
		},
		{
			name: "fall back to 4090 when 3090 is exhausted", gpuCount: 1, want: "4090", wantChecks: []string{"3090", "4090"},
			capacity: map[string][]cloudprovider.CapacitySpec{"3090": {shape(1, false)}, "4090": {shape(1, true)}},
		},
		{
			name: "match the requested GPU count", gpuCount: 2, want: "4090", wantChecks: []string{"3090", "4090"},
			capacity: map[string][]cloudprovider.CapacitySpec{"3090": {shape(1, true)}, "4090": {shape(2, true)}},
		},
		{
			name: "report no selection when neither is rentable", gpuCount: 1, wantChecks: []string{"3090", "4090"},
			capacity: map[string][]cloudprovider.CapacitySpec{"3090": {shape(1, false)}, "4090": {shape(1, false)}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var checks []string
			got, _, _ := chooseCompshareGPU(tc.gpuCount, func(model string) ([]cloudprovider.CapacitySpec, error) {
				checks = append(checks, model)
				return tc.capacity[model], nil
			})
			if got != tc.want || !reflect.DeepEqual(checks, tc.wantChecks) {
				t.Fatalf("selection=%q checks=%v, want %q checks=%v", got, checks, tc.want, tc.wantChecks)
			}
		})
	}
}

func TestHostedBuiltBinariesAreStaticAMD64(t *testing.T) {
	root, err := hostedRepoRoot()
	if err != nil {
		t.Fatal(err)
	}
	server := root + "/bin/gpuworks-linux-amd64"
	agent := root + "/bin/dl/gpuworks-agent-linux-amd64"
	if _, _, err := hostedBinaryPaths(hostedControlPlaneConfig{ServerBinary: server, AgentBinary: agent}); err != nil {
		t.Fatalf("hosted binary contract: %v", err)
	}
}

func TestCompshareDockerInstallCreatesAptSourceDirectory(t *testing.T) {
	command := compshareDocker29InstallCommand()
	create := "$root install -m 0755 -d /etc/apt/sources.list.d"
	write := "/etc/apt/sources.list.d/docker.list"
	if !strings.Contains(command, create) || !strings.Contains(command, write) || strings.Index(command, create) > strings.Index(command, write) {
		t.Fatalf("Docker install must create apt source directory before writing docker.list")
	}
}

func TestCompshareDockerInstallDoesNotUpgradeExistingBootstrapTools(t *testing.T) {
	command := compshareDocker29InstallCommand()
	for _, probe := range []string{
		"command -v curl >/dev/null 2>&1 ||",
		"command -v gpg >/dev/null 2>&1 ||",
		"command -v update-ca-certificates >/dev/null 2>&1 ||",
		"apt-get install -y --no-upgrade $bootstrap_packages",
	} {
		if !strings.Contains(command, probe) {
			t.Fatalf("Docker bootstrap install missing %q", probe)
		}
	}
	if strings.Contains(command, "apt-get install -y ca-certificates curl gnupg") {
		t.Fatal("Docker bootstrap unconditionally upgrades tools already present in the cloud image")
	}
}

func TestCompshareDockerInstallGrantsHostedAgentDaemonAccess(t *testing.T) {
	command := compshareDocker29InstallCommand()
	if !strings.Contains(command, `$root usermod -aG docker "$(id -un)"`) {
		t.Fatal("Docker install does not grant the non-root hosted agent access to docker.sock")
	}
}

func TestCompshareDockerNCCLSetupIsOffline(t *testing.T) {
	command := compshareDockerNCCLSetupCommand("docker", "gpuworks-test-network", "gpuworks/gpuworks-runner:test")
	if !strings.Contains(command, "docker image inspect") || !strings.Contains(command, "docker network create") {
		t.Fatalf("setup command does not verify the local image and create the network: %s", command)
	}
	if strings.Contains(command, " pull ") || strings.Contains(command, "docker pull") {
		t.Fatalf("NCCL setup must not contact a registry: %s", command)
	}
}

func TestCompshareDockerInstallEvidenceAllowsTrailingToolkitLog(t *testing.T) {
	want := "GPUWORKS_DOCKER_INSTALL_EVIDENCE docker=29.1.3 nvidia_ctk=NVIDIA_Container_Toolkit_CLI_version_1.17.8 nvidia_runtime=yes"
	output := "apt output\n" + want + "\ntime=\"2026-08-25T10:28:29+08:00\" level=info msg=\"It is recommended that docker daemon be restarted.\"\n"
	got, ok := compshareDockerInstallEvidence(output)
	if !ok || got != want {
		t.Fatalf("Docker evidence=(%q, %v), want (%q, true)", got, ok, want)
	}
}

func TestCompshareDockerInstallEvidenceRejectsIncompleteMarker(t *testing.T) {
	for _, output := range []string{
		"docker=29.1.3 nvidia_runtime=yes",
		"GPUWORKS_DOCKER_INSTALL_EVIDENCE docker=28.5.2 nvidia_runtime=yes",
		"GPUWORKS_DOCKER_INSTALL_EVIDENCE docker=29.1.3 nvidia_runtime=no",
	} {
		if got, ok := compshareDockerInstallEvidence(output); ok {
			t.Fatalf("accepted invalid Docker evidence %q from %q", got, output)
		}
	}
}

func TestHostedDeploymentExhaustionRequiresNoViableRanks(t *testing.T) {
	for _, tc := range []struct {
		name string
		dep  dto.Deployment
		want bool
	}{
		{name: "explicit failed", dep: dto.Deployment{RuntimeState: proto.InstFailed}, want: true},
		{name: "three failed leaders", dep: dto.Deployment{DesiredState: "active", Instances: []dto.Instance{{State: proto.InstFailed}, {State: proto.InstFailed}, {State: proto.InstFailed}}}, want: true},
		{name: "failed leaders and a torn-down follower", dep: dto.Deployment{DesiredState: "active", Instances: []dto.Instance{{State: proto.InstFailed}, {State: proto.InstFailed}, {State: proto.InstFailed}, {State: proto.InstRemoving, Headless: true}}}, want: true},
		{name: "retry remains", dep: dto.Deployment{DesiredState: "active", Instances: []dto.Instance{{State: proto.InstFailed}, {State: proto.InstFailed}}}},
		{name: "failed followers are not attempts", dep: dto.Deployment{DesiredState: "active", Instances: []dto.Instance{{State: proto.InstFailed}, {State: proto.InstFailed}, {State: proto.InstFailed, Headless: true}}}},
		{name: "pulling remains viable", dep: dto.Deployment{DesiredState: "active", Instances: []dto.Instance{{State: proto.InstFailed}, {State: proto.InstFailed}, {State: proto.InstFailed}, {State: proto.InstPulling}}}},
		{name: "running remains viable", dep: dto.Deployment{DesiredState: "active", Instances: []dto.Instance{{State: proto.InstFailed}, {State: proto.InstFailed}, {State: proto.InstFailed}, {State: proto.InstRunning}}}},
		{name: "stopped deployment", dep: dto.Deployment{DesiredState: "stopped", Instances: []dto.Instance{{State: proto.InstFailed}, {State: proto.InstFailed}, {State: proto.InstFailed}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := hostedDeploymentExhausted(tc.dep); got != tc.want {
				t.Fatalf("hostedDeploymentExhausted(%+v)=%t, want %t", tc.dep, got, tc.want)
			}
		})
	}
}

// A decode engine that dies at start on a cold VM is retried; the dead attempt's
// row stays beside the replacement. The wait must count live ranks only, or a
// deployment that is fully running never completes (seen on Aliyun, 2026-10-27:
// two running ranks and one failed row looped until the 75-minute timeout).
func TestHostedActiveInstancesIgnoreDeadAttempts(t *testing.T) {
	rows := []dto.Instance{
		{UUID: "a", State: proto.InstRunning, Role: "prefill"},
		{UUID: "b", State: proto.InstFailed, Role: "decode"},
		{UUID: "c", State: proto.InstRunning, Role: "decode"},
		{UUID: "d", State: proto.InstStopped},
		{UUID: "e", State: proto.InstRemoving},
	}
	active := hostedActiveInstances(rows)
	if len(active) != 2 || active[0].UUID != "a" || active[1].UUID != "c" {
		t.Fatalf("active = %+v, want only the two running ranks", active)
	}
	if len(hostedActiveInstances(nil)) != 0 {
		t.Fatal("no rows must give no active instances")
	}
}

func TestGPUWarmupCommandUsesTheAgentsLaunchShape(t *testing.T) {
	command := gpuWarmupCommand("gpuworks/gpuworks-runner:cuda12.9-vllm0.31.0-r2")
	for _, want := range []string{
		`--gpus "\"device=$i\""`, "CUDA_VISIBLE_DEVICES=0", "--entrypoint python3", "'gpuworks/gpuworks-runner:cuda12.9-vllm0.31.0-r2'",
		"torch.cuda.is_available()", "GPUWORKS_GPU_READY", "GPUWORKS_GPUS_READY $n", "nvidia-smi -L",
	} {
		if !strings.Contains(command, want) {
			t.Errorf("warm-up command lacks %q:\n%s", want, command)
		}
	}
	if strings.Contains(command, "docker pull") {
		t.Error("the warm-up must never pull an image")
	}
}

func TestHostedRootStaysOffTmpWhereTheBackendSaysSo(t *testing.T) {
	if got := (&vmBackend{}).hostedRoot(); got != "/tmp" {
		t.Fatalf("default hosted root = %q, want /tmp (a non-root login cannot write elsewhere)", got)
	}
	if got := (&vmBackend{HostedRoot: "/opt/"}).hostedRoot(); got != "/opt" {
		t.Fatalf("hosted root = %q, want /opt without a trailing slash", got)
	}
}

func TestHostedInstanceProgressIsConcise(t *testing.T) {
	long := strings.Repeat("x", 200)
	got := hostedInstanceProgress([]dto.Instance{
		{NodeRank: 0, State: proto.InstPulling, Phase: "image_pull", Progress: 0.42, Message: "old line\n" + long},
		{NodeRank: 1, State: proto.InstStarting, Phase: "health_check", Progress: 0.9, Message: "waiting"},
	})
	for _, want := range []string{"rank0=pulling/image_pull 42%", "rank1=starting/health_check 90%", strings.Repeat("x", 120)} {
		if !strings.Contains(got, want) {
			t.Fatalf("progress summary %q missing %q", got, want)
		}
	}
	if strings.Contains(got, "old line") || len(got) > 260 {
		t.Fatalf("progress summary is not concise: len=%d %q", len(got), got)
	}
}

type fixtureHostedForward struct {
	url    string
	closed atomic.Bool
}

func (f *fixtureHostedForward) BaseURL() string { return f.url }
func (f *fixtureHostedForward) Close() error {
	f.closed.Store(true)
	return nil
}
