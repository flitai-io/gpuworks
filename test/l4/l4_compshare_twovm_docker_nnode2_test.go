//go:build cloudgpu && (compshare || aliyun)

package l4

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/testenv/apiwire"
)

const compshareTwoVMDockerDeploymentTimeout = 75 * time.Minute

// TestCompshareTwoVMDockerDistributedInference is the Docker-runtime control for the default
// two-VM udocker distributed case. It intentionally keeps the provider shape,
// model, image, topology and gateway assertion the same; the principal changed
// variable is Docker CE 29.x plus NVIDIA Container Toolkit. The server runs on
// VM1 and both agents connect over the cloud private network, so neither runtime
// group depends on a workstation-hosted SSH reverse forward.
func TestCompshareTwoVMDockerDistributedInference(t *testing.T) {
	runTwoVMDockerDistributed(t, compshareBackend(t))
}

// runTwoVMDockerDistributed is the provider-neutral body of the case: the
// backend rents the VMs and supplies Docker, the image cache and the model.
func runTwoVMDockerDistributed(t *testing.T, b *vmBackend) {
	testStarted := time.Now()
	provisionStarted := time.Now()
	var dockerInstallDuration time.Duration
	harness := newHostedControlPlane(t, hostedControlPlaneConfig{
		RunID: cloudTestRunID(), RuntimeDrivers: [2]string{"docker", "docker"},
		Backend: b,
		PrepareVMs: func(t *testing.T, envs []*cloudTestEnv) {
			started := time.Now()
			b.EnsureDocker(t, envs)
			b.EnsureImage(t, envs)
			installDistributedVLLMFix(t, envs)
			dockerInstallDuration = time.Since(started)
		},
	})
	t.Logf("TIMING parallel Docker29+NVIDIA-toolkit install=%s", dockerInstallDuration.Round(time.Second))
	t.Logf("TIMING two-VM provision+Docker+hosted-control=%s", time.Since(provisionStarted).Round(time.Second))
	assertTwoVMDockerGPUInventories(t, harness.Nodes, harness.gpusPerVM)

	preflightStarted := time.Now()
	hostedRequireDistributedPreflight(t, harness, 29500)
	t.Logf("TIMING private-network preflight=%s", time.Since(preflightStarted).Round(time.Second))

	modelStarted := time.Now()
	model := hostedPrepareRealModel(t, harness)
	t.Logf("TIMING preloaded model registration on both nodes=%s", time.Since(modelStarted).Round(time.Second))

	deploymentStarted := time.Now()
	d := createHostedDeployment(t, harness, map[string]any{
		"model_id": model.ID, "node_uuid": harness.Nodes[0].UUID,
		"node_uuids":  []string{harness.Nodes[0].UUID, harness.Nodes[1].UUID},
		"served_name": b.servedName("twovm-docker-nnode2"),
		"engine":      "vllm", "image": b.DistributedImage,
		"nnodes": 2, "master_addr": harness.VMs[0].Instance.PrivateIP, "master_port": 29500,
		"placement":           map[string]any{"tp": 2, "pp": 2},
		"quick_params":        map[string]any{"max_model_len": 1024},
		"container_overrides": distributedVLLMContainerOverrides(),
	}, 2, compshareTwoVMDockerDeploymentTimeout)
	readyAt := time.Now()
	t.Logf("TIMING deployment create-to-both-running=%s", readyAt.Sub(deploymentStarted).Round(time.Second))

	assertTwoVMDockerDistributedInstances(t, harness.VMs, harness.Nodes, d.instances, harness.VMs[0].Instance.PrivateIP)
	logTwoVMDockerInstanceTimings(t, harness.API, d.instances)
	d.models(t)
	chatStarted := time.Now()
	d.chat(t)
	t.Logf("TIMING gateway chat=%s", time.Since(chatStarted).Round(time.Millisecond))
	t.Logf("two real %s VMs completed gpuworks Docker nnodes=2 gateway chat", b.Name)
	d.stop(t)
	t.Logf("TIMING complete two-VM Docker case=%s", time.Since(testStarted).Round(time.Second))
}

// prepareCompshareDockerImageCache is the single image-cache entry point for
// CompShare Docker cases. It first reuses an image already in the template and
// otherwise loads the configured docker-save archive. It never falls back to
// a registry pull; public registry acceptance belongs in a separate low-rate
// test, not in distributed topology coverage.
func prepareCompshareDockerImageCache(t *testing.T, envs []*cloudTestEnv) {
	t.Helper()
	prepareCompshareImageCache(t, envs, compshareDistributedTestImage(), strings.TrimSpace(os.Getenv("COMPSHARE_DOCKER_IMAGE_TAR")), true)
}

// prepareCompshareImageCache makes image present on every VM, loading tar
// when one is named and the VM lacks the image. With required, an image that
// is neither preloaded nor loadable skips the case (or fails it under strict
// mode); without it the image is left for the platform to pull and the case
// only logs that.
func prepareCompshareImageCache(t *testing.T, envs []*cloudTestEnv, imageRef, tar string, required bool) {
	t.Helper()
	image := shellQuoteForTest(imageRef)
	for rank, env := range envs {
		var cmd string
		if tar == "" {
			cmd = fmt.Sprintf("docker image inspect %s >/dev/null 2>&1", image)
		} else {
			cmd = fmt.Sprintf("set -eu; if docker image inspect %s >/dev/null 2>&1; then echo GPUWORKS_IMAGE_CACHE=hit; else test -f %s; docker load -i %s; fi; docker image inspect %s >/dev/null", image, shellQuoteForTest(tar), shellQuoteForTest(tar), image)
		}
		out, err := runCloudCommand(t.Context(), env.Instance, cmd)
		if err != nil {
			if tar == "" {
				if !required {
					t.Logf("CompShare rank %d has no preloaded image %s; the platform pulls it during deployment startup", rank, imageRef)
					continue
				}
				skipOrFailExternal(t, "CompShare rank %d lacks preloaded image %s and COMPSHARE_DOCKER_IMAGE_TAR is unset", rank, imageRef)
			}
			t.Fatalf("prepare preloaded Docker image %s on %s: %v\n%s", tar, env.Instance.InstanceID, err, strings.TrimSpace(out))
		}
	}
}

// hostedCompshareGPUModels lists the shapes the hosted control-plane tests
// may run on, cheapest first. Both VM regions rarely have inventory at the
// same time, so pinning a single model turns provider stock-outs into nightly
// failures even though a sibling shape is bookable. The cases serve
// Qwen2-0.5B with TP2 x PP2 (a few hundred MB per card), so any card with
// hostedMinVRAMMB will do: 3080Ti (12GB) and 2080Ti (11GB, only offered as
// x4 or x8, of which a case uses two cards per VM) widen the choice. Turing
// serves fp16 (the platform pins it for GPUs without bf16). V100S (Volta,
// sm_70) and P40 (Pascal, sm_61) are left out: the cuda12.8 runner image's
// PyTorch carries no kernels below sm_75, so the engine starts, answers its
// health check and then dies on the first sampled token with "no kernel image
// is available for execution on the device" (the nightly failed on V100S
// that way, as a 500 from the gateway). A800 and H20 close the list; CompShare
// refuses custom images for them, so placement passes over them anyway.
var hostedCompshareGPUModels = []string{"4090", "4090_48G", "5090", "3090", "3080Ti", "2080Ti", "A800", "H20"}

// hostedMinVRAMMB is the memory per card the hosted cases need.
const hostedMinVRAMMB = 10240

func installCompshareDocker29OnNodes(t *testing.T, envs []*cloudTestEnv) {
	t.Helper()
	ctx := t.Context()
	type result struct {
		index    int
		evidence string
		duration time.Duration
		err      error
	}
	results := make(chan result, len(envs))
	var wg sync.WaitGroup
	for index, env := range envs {
		wg.Add(1)
		go func(index int, env *cloudTestEnv) {
			defer wg.Done()
			started := time.Now()
			installCtx, cancel := context.WithTimeout(ctx, 35*time.Minute)
			defer cancel()
			out, err := runCloudCommand(installCtx, env.Instance, compshareDocker29InstallCommand())
			if err == nil {
				// runCloudCommand creates a fresh SSH login. This is intentional:
				// usermod does not alter supplementary groups in the install shell,
				// while the subsequently launched hosted agent also uses a new login.
				// Refuse to start the expensive model/image path unless that exact
				// non-sudo daemon access is already effective.
				accessCtx, accessCancel := context.WithTimeout(ctx, 2*time.Minute)
				access, accessErr := runCloudCommand(accessCtx, env.Instance,
					`set -eu; docker version --format '{{.Server.Version}}'; docker info --format '{{json .Runtimes}}' | grep -q nvidia; printf 'GPUWORKS_DOCKER_AGENT_ACCESS ok\n'`)
				accessCancel()
				out += "\n" + access
				if accessErr != nil {
					err = fmt.Errorf("new SSH login cannot access Docker daemon without sudo: %w", accessErr)
				}
			}
			results <- result{index: index, evidence: strings.TrimSpace(out), duration: time.Since(started), err: err}
		}(index, env)
	}
	wg.Wait()
	close(results)
	ordered := make([]result, len(envs))
	for result := range results {
		ordered[result.index] = result
	}
	for _, result := range ordered {
		if result.err != nil {
			t.Fatalf("install Docker 29/NVIDIA toolkit on rank %d after %s: %v\n%s", result.index, result.duration.Round(time.Second), result.err, result.evidence)
		}
		evidence, ok := compshareDockerInstallEvidence(result.evidence)
		if !ok {
			t.Fatalf("rank %d Docker installation output did not contain valid evidence; final output=%q", result.index, tailLine(result.evidence))
		}
		t.Logf("rank %d Docker installation %s: %s", result.index, result.duration.Round(time.Second), evidence)
	}
}

const compshareDockerInstallEvidencePrefix = "GPUWORKS_DOCKER_INSTALL_EVIDENCE "

func compshareDockerInstallEvidence(output string) (string, bool) {
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, compshareDockerInstallEvidencePrefix) &&
			strings.Contains(line, "docker=29.") && strings.Contains(line, "nvidia_runtime=yes") {
			return line, true
		}
	}
	return "", false
}

func tailLine(output string) string {
	lines := strings.Split(strings.TrimSpace(output), "\n")
	if len(lines) == 0 {
		return ""
	}
	return strings.TrimSpace(lines[len(lines)-1])
}

func compshareDocker29InstallCommand() string {
	return `set -eu
root=""
if [ "$(id -u)" -ne 0 ]; then root="sudo -n"; fi
$root sh -c 'printf "nameserver 100.90.90.90\nnameserver 100.90.90.100\n" > /etc/resolv.conf' || true
apt_retry() {
  attempt=1
  while [ "$attempt" -le 12 ]; do
    if "$@"; then return 0; fi
    if [ "$attempt" -eq 12 ]; then return 1; fi
    attempt=$((attempt + 1))
    sleep 10
  done
}
bootstrap_packages=""
command -v curl >/dev/null 2>&1 || bootstrap_packages="$bootstrap_packages curl"
command -v gpg >/dev/null 2>&1 || bootstrap_packages="$bootstrap_packages gnupg"
command -v update-ca-certificates >/dev/null 2>&1 || bootstrap_packages="$bootstrap_packages ca-certificates"
if [ -n "$bootstrap_packages" ]; then
  apt_retry $root apt-get update -o Acquire::Retries=3 -o Acquire::http::Timeout=30 -o Acquire::https::Timeout=30
  DEBIAN_FRONTEND=noninteractive apt_retry $root apt-get install -y --no-upgrade $bootstrap_packages
fi
. /etc/os-release
case "${ID:-}" in ubuntu|debian) ;; *) echo "unsupported Docker apt distribution: ${ID:-unknown}" >&2; exit 1;; esac
codename="${VERSION_CODENAME:-}"
[ -n "$codename" ]
arch="$(dpkg --print-architecture)"
# Images that already ship Docker 29 (Ubuntu docker.io on the cn-sh2 golden
# image) keep it: download.docker.com stalls from some CompShare regions.
preinstalled="$($root docker version --format '{{.Server.Version}}' 2>/dev/null || true)"
case "$preinstalled" in
29.*) echo "keeping preinstalled Docker $preinstalled" ;;
*)
  $root install -m 0755 -d /etc/apt/keyrings
  $root install -m 0755 -d /etc/apt/sources.list.d
  curl -fsSL "https://download.docker.com/linux/$ID/gpg" | $root tee /etc/apt/keyrings/docker.asc >/dev/null
  $root chmod a+r /etc/apt/keyrings/docker.asc
  printf 'deb [arch=%s signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/%s %s stable\n' "$arch" "$ID" "$codename" | $root tee /etc/apt/sources.list.d/docker.list >/dev/null
  apt_retry $root apt-get update -o Acquire::Retries=3 -o Acquire::http::Timeout=30 -o Acquire::https::Timeout=30
  docker_version="$(apt-cache madison docker-ce | awk '$3 ~ /^5:29\./ {print $3; exit}')"
  [ -n "$docker_version" ] || { echo 'Docker CE 29.x package is unavailable' >&2; exit 1; }
  DEBIAN_FRONTEND=noninteractive apt_retry $root apt-get install -y --allow-downgrades docker-ce="$docker_version" docker-ce-cli="$docker_version" containerd.io docker-buildx-plugin docker-compose-plugin
;;
esac
if ! command -v nvidia-ctk >/dev/null 2>&1; then
  curl -fsSL https://nvidia.github.io/libnvidia-container/gpgkey | $root gpg --dearmor --yes -o /usr/share/keyrings/nvidia-container-toolkit-keyring.gpg
  curl -fsSL https://nvidia.github.io/libnvidia-container/stable/deb/nvidia-container-toolkit.list | sed 's#deb https://#deb [signed-by=/usr/share/keyrings/nvidia-container-toolkit-keyring.gpg] https://#g' | $root tee /etc/apt/sources.list.d/nvidia-container-toolkit.list >/dev/null
  apt_retry $root apt-get update -o Acquire::Retries=3 -o Acquire::http::Timeout=30 -o Acquire::https::Timeout=30
  DEBIAN_FRONTEND=noninteractive apt_retry $root apt-get install -y nvidia-container-toolkit
fi
$root nvidia-ctk runtime configure --runtime=docker
$root python3 -c 'import json; p="/etc/docker/daemon.json"; d={};
try: d=json.load(open(p))
except Exception: pass
d["dns"]=["100.90.90.90","100.90.90.100"]
json.dump(d,open(p,"w"))'
# Replacing Ubuntu's docker.io with docker-ce leaves docker.socket failed, and
# dockerd -H fd:// then exits with "no sockets found via socket activation".
$root systemctl reset-failed docker.socket docker.service 2>/dev/null || true
$root systemctl restart docker.socket 2>/dev/null || true
$root systemctl restart docker 2>/dev/null || $root service docker restart
if [ "$(id -u)" -ne 0 ]; then
  $root usermod -aG docker "$(id -un)"
fi
docker_cmd="docker"
if ! docker info >/dev/null 2>&1; then docker_cmd="$root docker"; fi
server_version="$($docker_cmd version --format '{{.Server.Version}}')"
case "$server_version" in 29.*) ;; *) echo "unexpected Docker server version $server_version" >&2; exit 1;; esac
$docker_cmd info --format '{{json .Runtimes}}' | grep -q 'nvidia'
printf 'GPUWORKS_DOCKER_INSTALL_EVIDENCE docker=%s nvidia_ctk=%s nvidia_runtime=yes\n' "$server_version" "$(nvidia-ctk --version | head -n1 | tr ' ' '_')"`
}

func assertTwoVMDockerGPUInventories(t *testing.T, nodes []dto.Node, gpusPerVM int) {
	t.Helper()
	seen := map[string]bool{}
	for rank, node := range nodes {
		if node.RuntimeDriver != "docker" || len(node.GPUs) != gpusPerVM {
			t.Fatalf("rank %d inventory runtime=%q GPUs=%d, want %d", rank, node.RuntimeDriver, len(node.GPUs), gpusPerVM)
		}
		indices := make([]int, 0, len(node.GPUs))
		for _, gpu := range node.GPUs {
			indices = append(indices, gpu.Index)
		}
		sort.Ints(indices)
		for want, got := range indices {
			if got != want {
				t.Fatalf("rank %d GPU indices=%v, want 0..%d", rank, indices, gpusPerVM-1)
			}
		}
		for _, gpu := range node.GPUs {
			if !gpu.Schedulable || strings.TrimSpace(gpu.UUID) == "" || seen[gpu.UUID] {
				t.Fatalf("rank %d GPU inventory is unschedulable/overlapping: index=%d schedulable=%v", rank, gpu.Index, gpu.Schedulable)
			}
			seen[gpu.UUID] = true
		}
	}
	if len(seen) != 2*gpusPerVM {
		t.Fatalf("two Docker VMs exposed %d distinct GPU UUIDs, want %d", len(seen), 2*gpusPerVM)
	}
}

func assertTwoVMDockerDistributedInstances(t *testing.T, envs []*cloudTestEnv, nodes []dto.Node, instances []dto.Instance, masterAddr string) {
	t.Helper()
	if len(instances) != 2 {
		t.Fatalf("Docker distributed instances=%d, want 2", len(instances))
	}
	byNodeUUID := make(map[string]int, len(nodes))
	for rank, node := range nodes {
		byNodeUUID[node.UUID] = rank
	}
	seenRanks := map[int]bool{}
	seenContainerGPUs := map[string]bool{}
	for _, instance := range instances {
		rank, ok := byNodeUUID[instance.NodeUUID]
		if !ok {
			t.Fatalf("instance belongs to unexpected node %q", instance.NodeUUID)
		}
		gpuIndices := append([]int(nil), instance.GPUIndexes...)
		sort.Ints(gpuIndices)
		if instance.NNodes != 2 || instance.NodeRank != rank || len(gpuIndices) != 2 || gpuIndices[0] != 0 || gpuIndices[1] != 1 {
			t.Fatalf("rank %d Docker distributed topology=%+v", rank, instance)
		}
		if (rank == 0 && instance.Headless) || (rank == 1 && !instance.Headless) {
			t.Fatalf("rank %d headless=%v", rank, instance.Headless)
		}
		for _, required := range []string{
			"--tensor-parallel-size", "2", "--pipeline-parallel-size", "2",
			"--distributed-executor-backend", "mp", "--nnodes", "2",
			"--node-rank", fmt.Sprint(rank), "--master-addr", masterAddr, "--master-port", "29500",
		} {
			if !containsString(instance.Argv, required) {
				t.Fatalf("rank %d missing distributed arg %q in %v", rank, required, instance.Argv)
			}
		}
		if rank == 1 && !containsString(instance.Argv, "--headless") {
			t.Fatalf("rank 1 missing --headless in %v", instance.Argv)
		}
		seenRanks[rank] = true

		command := fmt.Sprintf(`set -eu
docker_cmd=docker; docker info >/dev/null 2>&1 || docker_cmd='sudo -n docker'
$docker_cmd inspect --format 'device_requests={{json .HostConfig.DeviceRequests}}' %s
$docker_cmd exec %s nvidia-smi --query-gpu=uuid --format=csv,noheader,nounits`, shellQuoteForTest(instance.ContainerID), shellQuoteForTest(instance.ContainerID))
		out, err := runCloudCommand(t.Context(), envs[rank].Instance, command)
		if err != nil {
			t.Fatalf("inspect rank %d Docker vLLM container: %v", rank, err)
		}
		lines := strings.Split(strings.TrimSpace(out), "\n")
		// Each rank runs on two cards; on a wider VM the scheduler may pick
		// any two, and the UUID check below ties them to this node.
		if len(lines) != 3 || len(dockerDeviceIDs(lines[0])) != 2 {
			t.Fatalf("rank %d Docker device request/GPU output=%q", rank, strings.TrimSpace(out))
		}
		want := map[string]bool{}
		for _, gpu := range nodes[rank].GPUs {
			want[gpu.UUID] = true
		}
		for _, uuid := range lines[1:] {
			uuid = strings.TrimSpace(uuid)
			if !want[uuid] || seenContainerGPUs[uuid] {
				t.Fatalf("rank %d container reported unexpected/overlapping GPU", rank)
			}
			seenContainerGPUs[uuid] = true
		}
	}
	if !seenRanks[0] || !seenRanks[1] || len(seenContainerGPUs) != 4 {
		t.Fatalf("Docker distributed evidence ranks=%v distinct_container_GPUs=%d", seenRanks, len(seenContainerGPUs))
	}
}

// dockerDeviceIDs extracts the DeviceIDs of a `docker inspect` device_requests line.
func dockerDeviceIDs(line string) []string {
	_, rest, ok := strings.Cut(line, `"DeviceIDs":[`)
	if !ok {
		return nil
	}
	list, _, _ := strings.Cut(rest, "]")
	var ids []string
	for _, id := range strings.Split(list, ",") {
		if id = strings.Trim(strings.TrimSpace(id), `"`); id != "" {
			ids = append(ids, id)
		}
	}
	return ids
}

func logTwoVMDockerInstanceTimings(t *testing.T, api *hostedAPIClient, instances []dto.Instance) {
	t.Helper()
	for _, instance := range instances {
		var events apiwire.CursorPage[dto.InstanceEvent]
		if err := api.GetJSON(t.Context(), fmt.Sprintf("/api/v1/instances/%s/events", instance.UUID), &events); err != nil {
			t.Logf("TIMING rank %d events unavailable: %v", instance.NodeRank, err)
			continue
		}
		var pullAt, engineAt, runningAt time.Time
		for _, event := range events.Items {
			if pullAt.IsZero() && strings.Contains(event.Message, "[image_pull]") {
				pullAt = event.CreatedAt
			}
			if engineAt.IsZero() && (strings.Contains(event.Message, "[container_start]") || strings.Contains(event.Message, "[health_check]")) {
				engineAt = event.CreatedAt
			}
			if strings.Contains(event.Message, "state → running") {
				runningAt = event.CreatedAt
			}
		}
		pullDuration, engineDuration := "unavailable", "unavailable"
		if !pullAt.IsZero() && !engineAt.IsZero() && !engineAt.Before(pullAt) {
			pullDuration = engineAt.Sub(pullAt).Round(time.Second).String()
		}
		if !engineAt.IsZero() && !runningAt.IsZero() && !runningAt.Before(engineAt) {
			engineDuration = runningAt.Sub(engineAt).Round(time.Second).String()
		}
		instanceTotal := "unavailable"
		if len(events.Items) > 0 && !runningAt.IsZero() && !runningAt.Before(events.Items[0].CreatedAt) {
			instanceTotal = runningAt.Sub(events.Items[0].CreatedAt).Round(time.Second).String()
		}
		t.Logf("TIMING rank %d docker image-pull=%s engine-startup=%s instance-total=%s", instance.NodeRank, pullDuration, engineDuration, instanceTotal)
	}
}
