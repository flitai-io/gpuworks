//go:build cloudgpu && runpod

package l4

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/partitioncap"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/testenv"
	"gpuworks.ai/gpuworks/internal/testenv/apiwire"
	"gpuworks.ai/gpuworks/internal/testenv/cloudprovider"
	"gpuworks.ai/gpuworks/internal/testenv/sshutil"
)

type runpodDeployment struct {
	env          *cloudTestEnv
	deploymentID uint
	servedName   string
	instanceUUID string
	containerID  string
	apiKey       string
}

const (
	// The CUDA 12.9 builds run on any R525+ driver through CUDA minor
	// version compatibility; RunPod hosts are not guaranteed the R580 driver
	// the CUDA 13.0 builds need.
	runpodDefaultVLLMImage   = "gpuworks/gpuworks-runner:cuda12.9-vllm0.31.0-r2"
	runpodDefaultSGLangImage = "gpuworks/gpuworks-runner:cuda12.9-sglang0.5.18-r2"
)

// RunPod containers do not necessarily run systemd. When no service manager
// owns the agent, the deliberate crash is recovered through the
// agent-restart.sh helper every install persists next to the binary: it
// relaunches the process with its install-time environment and arguments.
// Reconstructing that environment from /proc/<pid>/environ — as an earlier
// revision of this test did — fails with EACCES on RunPod images whose
// container root lacks CAP_SYS_PTRACE, so the persisted script is the only
// reliable restart path there. The script sits beside the running binary,
// found through /proc/<pid>/exe. That link stops being readable once the agent
// has run udocker work as another user: the credential change leaves the
// process non-dumpable, and a container root without CAP_SYS_PTRACE (every
// RunPod pod) is then refused. The command line stays readable and the agent
// is started by absolute path, so it is the fallback.
const runpodAgentCrashRestartCommand = `set -eu
old_pid="$(pgrep -o -x gpuworks-agent)"
agent_bin="$(readlink -f "/proc/$old_pid/exe" 2>/dev/null || true)"
if [ -z "$agent_bin" ]; then agent_bin="$(tr '\0' '\n' < "/proc/$old_pid/cmdline" | head -n 1)"; fi
restart_sh="$(dirname "$agent_bin")/agent-restart.sh"
kill -9 "$old_pid"
if ! command -v systemctl >/dev/null 2>&1 || ! systemctl is-active --quiet gpuworks-agent; then
  if [ ! -x "$restart_sh" ]; then
    echo "missing $restart_sh; agent installed by a gpuworks build without the persisted restart helper" >&2
    exit 1
  fi
  sleep 1
  "$restart_sh"
fi
for attempt in $(seq 1 60); do
  new_pid="$(pgrep -n -x gpuworks-agent || true)"
  if [ -n "$new_pid" ] && [ "$new_pid" != "$old_pid" ]; then
    echo "$old_pid -> $new_pid"
    exit 0
  fi
  sleep 1
done
exit 1`

func runpodEngineImage(environmentName, fallback string) string {
	if image := strings.TrimSpace(os.Getenv(environmentName)); image != "" {
		return image
	}
	return fallback
}

func runpodInferenceTemplateFakeImage() string {
	if image := strings.TrimSpace(os.Getenv("RUNPOD_INFERENCE_TEMPLATE_FAKE_IMAGE")); image != "" {
		return image
	}
	return "docker.io/wangweinoo1/fake-engine:test"
}

// mirrorFakeEngineThroughQuay registers a quay.io/zstackai mirror for the
// fake-engine Docker Hub namespace through the operator-facing pull source
// rules, exactly as a site would. RunPod egress to
// registry-1.docker.io has been observed answering the manifest path with an
// HTML page over HTTP 200 (2026-10 L4 failures), which dies in manifest
// parsing before any layer transfer; quay.io/zstackai/fake-engine is the
// documented acceptance source for hosts that cannot reach Docker Hub
// (test/fake-engine/IMAGES.md) and carries the same multi-arch index. With
// this rule the deployment's pull chain becomes
// [quay.io/zstackai/fake-engine:test, docker.io/wangweinoo1/fake-engine:test]
// instead of the single direct ref. The engine images need no rule: they are
// catalog images whose fetch addresses already include their quay.io copy.
// The rule names quay.io only (no swr.cn-south-1.myhuaweicloud.com): RunPod pods run
// outside China, where a China-hosted mirror is the slowest source, and a
// source that is merely slow would cost a full
// crawl-detection window (imagepull.CrawlWindow) before the walk reaches the
// upstream registry behind it.
func mirrorFakeEngineThroughQuay(t *testing.T, env *cloudTestEnv) {
	t.Helper()
	createPullSourceRules(t, env.Control.API(t),
		l4PullSourceRule{Match: "docker.io/wangweinoo1", Endpoints: []string{"quay.io/zstackai"}},
	)
}

func deployRunpodModel(t *testing.T, env *cloudTestEnv, model *store.Model, engine, image, servedName string, sleepable bool, timeout time.Duration, extraQuick map[string]any) *runpodDeployment {
	t.Helper()
	// The controller intentionally requires an explicit public-download choice
	// when no online peer owns the model. Make that choice part of this E2E test
	// so it exercises the real model-download progress path before scheduling.
	prepareCloudModelDownload(t, env, model)
	quick := map[string]any{"max_model_len": 1024}
	for key, value := range extraQuick {
		quick[key] = value
	}
	body, _ := json.Marshal(map[string]any{
		"model_id": model.ID, "node_uuid": env.NodeUUID, "served_name": servedName,
		"engine": engine, "image": image, "sleepable": sleepable,
		"quick_params": quick,
	})
	resp, err := env.Control.API(t).PostJSON("/api/v1/deployments", body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		data, _ := io.ReadAll(resp.Body)
		t.Fatalf("create deployment status=%d body=%s", resp.StatusCode, data)
	}
	var created map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	deploymentID := uint(created["id"].(float64))
	instance := waitDeploymentInstance(t, env, deploymentID, timeout)
	key, err := env.Control.Store().CreateAPIKey("l4", 0)
	if err != nil {
		t.Fatal(err)
	}
	return &runpodDeployment{env: env, deploymentID: deploymentID, servedName: servedName, instanceUUID: instance.UUID, containerID: instance.ContainerID, apiKey: key}
}

func (d *runpodDeployment) chat(t *testing.T, want int) []byte {
	t.Helper()
	body := fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"reply with ready"}],"max_tokens":16}`, d.servedName)
	req, _ := http.NewRequest(http.MethodPost, d.env.Control.URL()+"/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+d.apiKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != want {
		t.Fatalf("chat status=%d want=%d body=%s", resp.StatusCode, want, data)
	}
	return data
}

// TestRunpodHuggingFacePublicDownload is the single low-frequency RunPod network
// acceptance. RunPod supplies the container environment, which downloads one
// published 8 MiB shard from Hugging Face. The product downloader itself is
// exhaustively fixture-tested offline; this case isolates public egress/CDN
// availability and does not pull or start an inference image.
func TestRunpodHuggingFacePublicDownload(t *testing.T) {
	env := setupCloudTest(t, runpodProvider(t), cloudprovider.GPUSpec{GPUModel: "auto", GPUCount: 1, DiskGB: 40})
	requireOnlineNode(t, env)
	if env.Instance.Kind != "container" {
		t.Fatalf("RunPod product kind=%q, want container", env.Instance.Kind)
	}
	verifyCloudPublicShard(t, env, "huggingface", "zsatckai/qwen3-4b-fake", "main")
}

// TestRunpodVLLMSGLangLifecycle reuses one Pod and one real model download while exercising
// vLLM and SGLang sequentially so the engines never compete for GPU memory.
func TestRunpodVLLMSGLangLifecycle(t *testing.T) {
	// udocker expands both fixed engine images into private root filesystems.
	// The two compressed images total more than 30 GiB and their extracted
	// trees do not fit alongside the model and agent state on a 120 GiB disk.
	env := setupCloudTest(t, runpodProvider(t), cloudprovider.GPUSpec{GPUModel: "auto", GPUCount: 1, DiskGB: 240})
	requireOnlineNode(t, env)
	mirrorFakeEngineThroughQuay(t, env)
	// Run the inference-template contract before the real-model lifecycle. The template path
	// uses a registered model file and avoids another large model download.
	t.Run("SeedInferenceTemplateFake", func(t *testing.T) {
		d := deploySeedInferenceTemplateFake(t, env, runpodInferenceTemplateFakeImage(), "qwen3-inference-template-fake")
		d.stop(t)
	})

	model := cloudTestModel(t, env)
	prepareCloudModelDownload(t, env, model)

	t.Run("vllm", func(t *testing.T) {
		image := runpodEngineImage("RUNPOD_VLLM_TEST_IMAGE", runpodDefaultVLLMImage)
		d := deployRunpodModel(t, env, model, "vllm", image, "runpod-vllm", true, 30*time.Minute, nil)
		defer d.stop(t)
		testRunpodDeploy(t, d)
		testRunpodMetrics(t, env)
		waitRunpodMetric(t, env, "gpuworks_infer_requests_running", map[string]string{"instance": d.instanceUUID})
		testRunpodSleepWake(t, d)
		t.Run("wake-admission-with-co-tenant", func(t *testing.T) {
			testRunpodWakeAdmissionWithCoTenant(t, d, model, image)
		})
		testRunpodCrash(t, d)
	})

	t.Run("sglang", func(t *testing.T) {
		image := runpodEngineImage("RUNPOD_SGLANG_TEST_IMAGE", runpodDefaultSGLangImage)
		// SGLang serves Prometheus metrics only behind --enable-metrics; the
		// param defaults off, unlike vLLM's always-on /metrics. Without it this
		// deployment emits no series at all, so the wait below can never be
		// satisfied by this instance.
		d := deployRunpodModel(t, env, model, "sglang", image, "runpod-sglang", false, 30*time.Minute, map[string]any{"enable_metrics": true})
		defer d.stop(t)
		testRunpodDeploy(t, d)
		// Scoping to this instance rejects leftover series from the earlier
		// subtests on the same node: only this deployment's own metric path
		// satisfies the wait.
		waitRunpodMetric(t, env, "gpuworks_infer_requests_running", map[string]string{"instance": d.instanceUUID})
	})
}

func testRunpodDeploy(t *testing.T, d *runpodDeployment) {
	req, _ := http.NewRequest(http.MethodGet, d.env.Control.URL()+"/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+d.apiKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	models, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(models), d.servedName) {
		t.Fatalf("models status=%d body=%s", resp.StatusCode, models)
	}
	data := d.chat(t, http.StatusOK)
	var result map[string]any
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	choices, ok := result["choices"].([]any)
	if !ok || len(choices) == 0 {
		t.Fatalf("real inference returned no choices: %s", data)
	}
	first, ok := choices[0].(map[string]any)
	if !ok {
		t.Fatalf("first choice has unexpected shape: %s", data)
	}
	message, ok := first["message"].(map[string]any)
	if !ok || strings.TrimSpace(fmt.Sprint(message["content"])) == "" {
		t.Fatalf("real inference returned empty message content: %s", data)
	}
}

func (d *runpodDeployment) stop(t *testing.T) {
	t.Helper()
	if d == nil || d.deploymentID == 0 {
		return
	}
	resp, err := d.env.Control.API(t).PostJSON(fmt.Sprintf("/api/v1/deployments/%d/stop", d.deploymentID), nil)
	if err != nil {
		t.Errorf("stop RunPod deployment: %v", err)
		return
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("stop RunPod deployment status=%d", resp.StatusCode)
		return
	}
	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		rows, _ := d.env.Control.Store().InstancesForDeployment(d.deploymentID)
		if len(rows) == 0 {
			d.deploymentID = 0
			return
		}
		time.Sleep(time.Second)
	}
	t.Errorf("RunPod deployment %d did not stop", d.deploymentID)
}

func (d *runpodDeployment) delete(t *testing.T) {
	t.Helper()
	if d == nil || d.deploymentID == 0 {
		return
	}
	id := d.deploymentID
	resp, err := d.env.Control.API(t).Delete(fmt.Sprintf("/api/v1/deployments/%d", id))
	if err != nil {
		t.Fatalf("delete RunPod deployment: %v", err)
	}
	data, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete RunPod deployment status=%d body=%s", resp.StatusCode, data)
	}
	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		if _, err := d.env.Control.Store().GetDeployment(id); err != nil {
			d.deploymentID = 0
			return
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("RunPod deployment %d was not deleted", id)
}

func testRunpodHAMi(t *testing.T) {
	// HAMi must observe a real multi-GPU node, not a single-card placement.
	// The provider chooses any available model; the two GPUs are requested as
	// one Pod so RunPod keeps them on the same host and model.
	// HAMi keeps the image, model, udocker extraction tree, and libvgpu
	// artifacts on the rented host. Leave enough headroom for all of them.
	d := setupCloudNode(t, testenv.New(t), runpodProvider(t), cloudprovider.GPUSpec{GPUModel: "auto", GPUCount: 2, MinVRAMMB: 8192, DiskGB: 120})
	// Reuse the deployment flow after provisioning the two-card node.
	model := cloudTestModel(t, d)
	node, err := d.Control.Store().NodeByUUID(d.NodeUUID)
	if err != nil {
		t.Fatal(err)
	}
	prepareCloudModelDownload(t, d, model)
	body, _ := json.Marshal(map[string]any{"model_id": model.ID, "node_uuid": d.NodeUUID, "served_name": "qwen2-hami", "engine": "vllm", "image": runpodDefaultVLLMImage, "vram_limit_gib": 8, "quick_params": map[string]any{"max_model_len": 1024}})
	resp, err := d.Control.API(t).PostJSON("/api/v1/deployments", body)
	if err != nil || resp.StatusCode != http.StatusCreated {
		t.Fatalf("HAMi deployment status=%v err=%v", responseStatus(resp), err)
	}
	var created map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()
	deploymentID := uint(created["id"].(float64))
	instance := waitDeploymentInstance(t, d, deploymentID, 30*time.Minute)
	node, err = d.Control.Store().NodeByUUID(d.NodeUUID)
	if err != nil {
		t.Fatal(err)
	}
	ready := partitioncap.ReadyBackendIDs(node)
	if len(ready) != 1 || ready[0] != accelerator.BackendNVIDIALogical {
		t.Fatalf("ready partition backends=%v, want %s", ready, accelerator.BackendNVIDIALogical)
	}
	var spec proto.InstanceDesiredState
	if err := json.Unmarshal([]byte(instance.DesiredStateJSON), &spec); err != nil {
		t.Fatal(err)
	}
	if spec.VRAMLimitBytes != 8<<30 {
		t.Fatalf("VRAM limit=%d, want %d", spec.VRAMLimitBytes, int64(8<<30))
	}
	out, err := sshutil.RunCommand(t.Context(), d.Instance.PublicIP, d.Instance.SSHPort, d.Instance.Username, d.Instance.PrivateKey, "find /opt/gpuworks -type f -name '*.cache' -size +0c -print -quit")
	if err != nil || strings.TrimSpace(out) == "" {
		t.Fatalf("HAMi cache was not created: output=%q err=%v", out, err)
	}
}

func testRunpodSleepWake(t *testing.T, d *runpodDeployment) {
	t.Run("sleep-level-1", func(t *testing.T) {
		testRunpodSleepWakeLevel(t, d, 1)
	})
	t.Run("sleep-level-2", func(t *testing.T) {
		testRunpodSleepWakeLevel(t, d, 2)
	})
}

// testRunpodWakeAdmissionWithCoTenant proves the real-GPU version of the
// sleep/eviction contract: A releases the card, B starts on that card and can
// serve, A's wake is refused with B named as the occupant, and A wakes after B
// sleeps. Both deployments reuse the already downloaded model and image.
func testRunpodWakeAdmissionWithCoTenant(t *testing.T, a *runpodDeployment, model *store.Model, image string) {
	t.Helper()
	api := a.env.Control.API(t)
	sleep, err := api.PostJSON(fmt.Sprintf("/api/v1/deployments/%d/sleep?level=1&mode=wait", a.deploymentID), nil)
	if err != nil || sleep.StatusCode != http.StatusOK {
		t.Fatalf("sleep A for co-tenant test: status=%v err=%v", responseStatus(sleep), err)
	}
	sleep.Body.Close()
	a.env.Control.Eventually(t, 2*time.Minute, func() bool {
		instance, _ := a.env.Control.Store().InstanceByUUID(a.instanceUUID)
		return instance != nil && instance.State == proto.InstRunning && instance.SleepLevel == 1
	})

	b := deployRunpodModel(t, a.env, model, "vllm", image, "runpod-vllm-cotenant", true, 30*time.Minute, nil)
	defer b.stop(t)
	b.chat(t, http.StatusOK)
	aInstance, err := a.env.Control.Store().InstanceByUUID(a.instanceUUID)
	if err != nil {
		t.Fatal(err)
	}
	bInstance, err := a.env.Control.Store().InstanceByUUID(b.instanceUUID)
	if err != nil {
		t.Fatal(err)
	}
	if aInstance.GPUIndexes != bInstance.GPUIndexes {
		t.Fatalf("co-tenant landed on GPUs %s, want A's released GPUs %s", bInstance.GPUIndexes, aInstance.GPUIndexes)
	}

	wake, err := api.PostJSON(fmt.Sprintf("/api/v1/deployments/%d/wake", a.deploymentID), nil)
	if err != nil {
		t.Fatal(err)
	}
	wakeBody, _ := io.ReadAll(wake.Body)
	wake.Body.Close()
	if wake.StatusCode != http.StatusConflict || !strings.Contains(string(wakeBody), `"code":"WAKE_INSUFFICIENT_GPU_MEMORY"`) || !strings.Contains(string(wakeBody), b.servedName) {
		t.Fatalf("wake A over running B: status=%d body=%s", wake.StatusCode, wakeBody)
	}
	if dep, _ := a.env.Control.Store().GetDeployment(a.deploymentID); dep == nil || dep.DesiredSleep != 1 {
		t.Fatalf("refused wake changed A's desired sleep: %+v", dep)
	}

	sleepB, err := api.PostJSON(fmt.Sprintf("/api/v1/deployments/%d/sleep?level=1&mode=wait", b.deploymentID), nil)
	if err != nil || sleepB.StatusCode != http.StatusOK {
		t.Fatalf("sleep B before retrying A wake: status=%v err=%v", responseStatus(sleepB), err)
	}
	sleepB.Body.Close()
	a.env.Control.Eventually(t, 2*time.Minute, func() bool {
		instance, _ := a.env.Control.Store().InstanceByUUID(b.instanceUUID)
		return instance != nil && instance.State == proto.InstRunning && instance.SleepLevel == 1
	})

	wake, err = api.PostJSON(fmt.Sprintf("/api/v1/deployments/%d/wake", a.deploymentID), nil)
	if err != nil || wake.StatusCode != http.StatusOK {
		var body []byte
		if wake != nil {
			body, _ = io.ReadAll(wake.Body)
			wake.Body.Close()
		}
		t.Fatalf("wake A after B sleeps: status=%v err=%v body=%s", responseStatus(wake), err, body)
	}
	wake.Body.Close()
	a.env.Control.Eventually(t, 3*time.Minute, func() bool {
		instance, _ := a.env.Control.Store().InstanceByUUID(a.instanceUUID)
		return instance != nil && instance.State == proto.InstRunning && instance.SleepLevel == 0 && instance.ContainerID == a.containerID
	})
	a.chat(t, http.StatusOK)
}

// testRunpodSleepWakeLevel drives the real vLLM endpoint through the product
// API and then checks the engine's own boolean probe. Level 2 is deliberately
// part of the cloud lifecycle: the fake-engine tests cover the three-step
// protocol, while this case proves that a real GPU releases weight memory and
// can reload it before serving again.
func testRunpodSleepWakeLevel(t *testing.T, d *runpodDeployment, level int) {
	t.Helper()
	api := d.env.Control.API(t)
	awakeMemory, memoryReadable := runpodGPUUsedMemory(t, d)
	if runpodEngineSleeping(t, d) {
		t.Fatalf("engine is already sleeping before level %d transition", level)
	}
	sleepPath := fmt.Sprintf("/api/v1/deployments/%d/sleep?level=%d&mode=wait", d.deploymentID, level)
	resp, err := api.PostJSON(sleepPath, nil)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("sleep request status=%v err=%v", responseStatus(resp), err)
	}
	resp.Body.Close()
	sleepTimeout := 2 * time.Minute
	if level == 2 {
		// A wait-mode L2 sleep can drain in-flight work before discarding the
		// weights; use the same order budget as the agent for the real engine.
		sleepTimeout = 10 * time.Minute
	}
	d.env.Control.Eventually(t, sleepTimeout, func() bool {
		instance, _ := d.env.Control.Store().InstanceByUUID(d.instanceUUID)
		return instance != nil && instance.SleepLevel == level && instance.ContainerID == d.containerID
	})
	if !runpodEngineSleeping(t, d) {
		t.Fatalf("server reports L%d but the real engine is awake", level)
	}
	sleepMemory, sleepMemoryReadable := runpodGPUUsedMemory(t, d)
	if level == 2 && memoryReadable && sleepMemoryReadable {
		if sleepMemory < 0 || awakeMemory-sleepMemory < 256 {
			t.Fatalf("L2 sleep did not release expected GPU memory: awake=%dMiB sleeping=%dMiB", awakeMemory, sleepMemory)
		}
	} else if level == 2 {
		t.Logf("GPU memory is not readable on this RunPod shape; keeping the engine/proxy L2 assertions")
	}
	d.chat(t, http.StatusServiceUnavailable)
	resp, err = api.PostJSON(fmt.Sprintf("/api/v1/deployments/%d/wake", d.deploymentID), nil)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("wake request status=%v err=%v", responseStatus(resp), err)
	}
	resp.Body.Close()
	wakeTimeout := 3 * time.Minute
	if level == 2 {
		// L2 reloads the model from the mounted directory. Keep this above the
		// measured multi-minute cold-wake path while still bounding the test.
		wakeTimeout = 12 * time.Minute
	}
	d.env.Control.Eventually(t, wakeTimeout, func() bool {
		instance, _ := d.env.Control.Store().InstanceByUUID(d.instanceUUID)
		return instance != nil && instance.SleepLevel == 0 && instance.ContainerID == d.containerID
	})
	if runpodEngineSleeping(t, d) {
		t.Fatalf("server reports awake after level %d wake but the real engine is still sleeping", level)
	}
	wakeMemory, wakeMemoryReadable := runpodGPUUsedMemory(t, d)
	if level == 2 && memoryReadable && sleepMemoryReadable && wakeMemoryReadable && wakeMemory-sleepMemory < 256 {
		t.Fatalf("L2 wake did not restore expected GPU memory: sleeping=%dMiB awake=%dMiB", sleepMemory, wakeMemory)
	}
	d.chat(t, http.StatusOK)
}

func runpodEngineSleeping(t *testing.T, d *runpodDeployment) bool {
	t.Helper()
	instance, err := d.env.Control.Store().InstanceByUUID(d.instanceUUID)
	if err != nil {
		t.Fatalf("load RunPod instance for sleep probe: %v", err)
	}
	if instance.APIPort <= 0 {
		t.Fatalf("RunPod instance has no engine API port: %+v", instance)
	}
	out, err := runCloudCommand(t.Context(), d.env.Instance, fmt.Sprintf("curl -fsS --max-time 15 http://127.0.0.1:%d/is_sleeping", instance.APIPort))
	if err != nil {
		t.Fatalf("probe real vLLM sleep state on port %d: %v (%s)", instance.APIPort, err, strings.TrimSpace(out))
	}
	var plain bool
	if json.Unmarshal([]byte(strings.TrimSpace(out)), &plain) == nil {
		return plain
	}
	var body struct {
		IsSleeping bool `json:"is_sleeping"`
		Sleeping   bool `json:"sleeping"`
	}
	if err := json.Unmarshal([]byte(out), &body); err != nil {
		t.Fatalf("decode real vLLM sleep probe %q: %v", strings.TrimSpace(out), err)
	}
	return body.IsSleeping || body.Sleeping
}

// runpodGPUUsedMemory returns the selected GPU's used memory in MiB. Some
// host-sliced RunPod SKUs intentionally report N/A; the endpoint and proxy
// assertions still remain authoritative for those shapes.
func runpodGPUUsedMemory(t *testing.T, d *runpodDeployment) (int64, bool) {
	t.Helper()
	instance, err := d.env.Control.Store().InstanceByUUID(d.instanceUUID)
	if err != nil {
		t.Fatalf("load RunPod instance for GPU memory probe: %v", err)
	}
	var indexes []int
	if err := json.Unmarshal([]byte(instance.GPUIndexes), &indexes); err != nil || len(indexes) == 0 {
		t.Fatalf("decode selected GPU indexes %q: %v", instance.GPUIndexes, err)
	}
	out, err := runCloudCommand(t.Context(), d.env.Instance, fmt.Sprintf("nvidia-smi --id=%d --query-gpu=memory.used --format=csv,noheader,nounits", indexes[0]))
	if err != nil {
		t.Fatalf("read RunPod GPU %d memory: %v (%s)", indexes[0], err, strings.TrimSpace(out))
	}
	raw := strings.TrimSpace(strings.Split(strings.TrimSpace(out), "\n")[0])
	if strings.EqualFold(raw, "N/A") || strings.Contains(raw, "Insufficient") {
		return 0, false
	}
	value, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil {
		t.Fatalf("parse RunPod GPU %d memory %q: %v", indexes[0], raw, err)
	}
	return value, true
}

// testRunpodMetrics waits for the node's GPU series. Some rentable SKUs are
// host-sliced GPUs (the allowlist's RTX PRO 6000 Blackwell MIG 1g.24gb among
// them) whose tenant nvidia-smi renders "[N/A]" and "[Insufficient
// Permissions]" for utilization and memory while temperature and power stay
// readable; the agent, correctly, emits no series for a field it cannot read,
// so those waits can never succeed on such an instance. Probe which fields
// this instance can really report (runpodMetricQuery) and wait only for
// those. A probe that fails or shows no valid GPU row keeps waiting for every
// metric — a broken nvidia-smi must fail loudly, not quietly skip the verdict.
func testRunpodMetrics(t *testing.T, env *cloudTestEnv) {
	out, err := runCloudCommand(t.Context(), env.Instance, runpodMetricQuery)
	readable, sawGPURow := parseReadableGPUMetrics(out)
	probeOK := err == nil && sawGPURow
	if !probeOK {
		t.Logf("GPU field probe inconclusive (err=%v saw_gpu_row=%t); waiting for all metrics", err, sawGPURow)
	}
	for _, metric := range []string{"gpuworks_gpu_utilization", "gpuworks_gpu_memory_used_bytes", "gpuworks_gpu_temperature_celsius"} {
		if probeOK && !readable[metric] {
			t.Logf("%s: this instance's nvidia-smi cannot read the field (host-sliced GPU?); not waiting for it", metric)
			continue
		}
		waitRunpodMetric(t, env, metric, nil)
	}
}

func testRunpodMultiNode(t *testing.T) {
	control := testenv.New(t)
	p := runpodProvider(t)
	// Multi-node coverage needs two independent nodes with the same GPU model;
	// it must not depend on a particular RTX 4090/A100 inventory combination.
	first := setupCloudNode(t, control, p, cloudprovider.GPUSpec{GPUModel: "auto", GPUCount: 1, MinVRAMMB: 8192, DiskGB: 40})
	model := strings.TrimSpace(first.Instance.GPUModel)
	if model == "" {
		t.Fatal("RunPod did not report the selected GPU model for the first node")
	}
	second := setupCloudNode(t, control, p, cloudprovider.GPUSpec{GPUModel: model, GPUCount: 1, MinVRAMMB: 8192, DiskGB: 40})
	if got := cloudprovider.NormalizeGPUModel(second.Instance.GPUModel); got != cloudprovider.NormalizeGPUModel(model) {
		t.Fatalf("multi-node GPU models differ: first=%q second=%q", model, second.Instance.GPUModel)
	}
}

func testRunpodCrash(t *testing.T, d *runpodDeployment) {
	node, err := d.env.Control.Store().NodeByUUID(d.env.NodeUUID)
	if err != nil {
		t.Fatal(err)
	}
	beforeRestart, err := d.env.Control.Store().InstanceByUUID(d.instanceUUID)
	if err != nil {
		t.Fatal(err)
	}
	if output, err := runCloudCommand(t.Context(), d.env.Instance, runpodAgentCrashRestartCommand); err != nil {
		t.Fatalf("restart RunPod agent: %v: %s", err, strings.TrimSpace(output))
	}
	if node.ConnectMode == "server_dial" {
		if d.env.Tunnel == nil || d.env.NodeToken == "" {
			t.Fatal("server_dial RunPod test is missing its tunnel or node token")
		}
		if err := dialCloudAgent(t.Context(), d.env.Control, d.env.Tunnel.Addr(), d.env.NodeToken); err != nil {
			t.Fatal(err)
		}
	}
	d.env.Control.WaitNodeOnlineUUID(t, d.env.NodeUUID)
	d.env.Control.Eventually(t, 3*time.Minute, func() bool {
		instance, _ := d.env.Control.Store().InstanceByUUID(d.instanceUUID)
		return instance != nil && instance.UpdatedAt.After(beforeRestart.UpdatedAt) && instance.State == proto.InstRunning && instance.ContainerID == d.containerID
	})
	d.chat(t, http.StatusOK)
}

// waitRunpodMetric waits for a metric query on this node to return points. A
// non-empty wantLabels additionally requires the points to come from a series
// whose labels match every wanted entry (e.g. {"instance": uuid}), so series
// left over from earlier deployments on the same node cannot satisfy the wait.
func waitRunpodMetric(t *testing.T, env *cloudTestEnv, metric string, wantLabels map[string]string) {
	t.Helper()
	api := env.Control.API(t)
	deadline := time.Now().Add(2 * time.Minute)
	var lastStatus int
	var lastBody []byte
	for time.Now().Before(deadline) {
		resp, err := api.Get("/api/v1/metrics/query?metric=" + metric + "&range=5m&step=10s&node=" + env.NodeUUID)
		if err != nil {
			time.Sleep(250 * time.Millisecond)
			continue
		}
		data, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		lastStatus, lastBody = resp.StatusCode, data
		if resp.StatusCode != http.StatusOK {
			time.Sleep(250 * time.Millisecond)
			continue
		}
		var series []apiwire.MetricSeries
		if err := apiwire.UnmarshalList(data, &series); err != nil {
			t.Fatalf("metric %s: response off the documented wire shape: %v (%s)", metric, err, data)
		}
		if hasMetricDataFor(series, wantLabels) {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	diagnostic, diagErr := runCloudCommand(t.Context(), env.Instance, runpodMetricQuery)
	if diagErr != nil {
		diagnostic = fmt.Sprintf("diagnostic failed: %v", diagErr)
	}
	t.Fatalf("metric %s did not arrive for labels %v: status=%d body=%s nvidia_smi=%q", metric, wantLabels, lastStatus, strings.TrimSpace(string(lastBody)), strings.TrimSpace(diagnostic))
}

// hasMetricDataFor reports whether any series carries points and every wanted
// label.
func hasMetricDataFor(series []apiwire.MetricSeries, want map[string]string) bool {
	for _, item := range series {
		if len(item.Points) > 0 && seriesHasLabels(item.Labels, want) {
			return true
		}
	}
	return false
}

func seriesHasLabels(labels map[string]string, want map[string]string) bool {
	for name, wantValue := range want {
		if got, ok := labels[name]; !ok || got != wantValue {
			return false
		}
	}
	return true
}

func responseStatus(resp *http.Response) any {
	if resp == nil {
		return nil
	}
	return resp.StatusCode
}

// TestRunpodServerDialNodeOnboarding installs a server-dialed agent on a pod
// the platform created (which exposes the agent's port with SSH), then edits
// the pod's ports: RunPod restarts it and renumbers every port, SSH's and the
// agent's, and recovery must follow both to bring the node back.
//
// go test -tags=cloudgpu,runpod -run '^TestRunpodServerDialNodeOnboarding$' -timeout=55m ./test/l4
func TestRunpodServerDialNodeOnboarding(t *testing.T) {
	provider := runpodProvider(t)
	// Recovery is what brings the node back after the edit, so the harness
	// runs with it on, as a default server does.
	env := setupCloudNodeWithInstance(t, testenv.NewWithRecovery(t), provider, cloudprovider.GPUSpec{GPUModel: "auto", GPUCount: 1, DiskGB: 40}, nil, withServerDial())
	requireOnlineNode(t, env)
	before, err := env.Control.Store().NodeByUUID(env.NodeUUID)
	if err != nil {
		t.Fatal(err)
	}
	if before.ConnectMode != "server_dial" || before.ServerURL != "" || before.ServerDialListen != "0.0.0.0:18800" || before.ServerDialAddress == "" {
		t.Fatalf("runpod server_dial node = mode=%q server_url=%q listen=%q address=%q", before.ConnectMode, before.ServerURL, before.ServerDialListen, before.ServerDialAddress)
	}

	renumberRunpodPod(t, env.Instance.InstanceID)
	env.Control.WaitNodeStateUUID(t, env.NodeUUID, store.NodeStateOffline, 5*time.Minute)
	api := env.Control.API(t)
	deadline := time.Now().Add(20 * time.Minute)
	for {
		node, err := env.Control.Store().NodeByUUID(env.NodeUUID)
		if err == nil && node.State == store.NodeStateOnline {
			if node.ServerDialAddress == before.ServerDialAddress {
				t.Fatalf("the node came back at its old address %s; the edit did not renumber it", node.ServerDialAddress)
			}
			t.Logf("node back online at %s (was %s)", node.ServerDialAddress, before.ServerDialAddress)
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the node did not come back after the pod was renumbered: %+v", node)
		}
		// Recovery's own schedule waits minutes between rounds; ask it now.
		resp, err := api.PostJSON("/api/v1/nodes/"+env.NodeUUID+"/recovery/probe", []byte(`{}`))
		if err == nil {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			t.Logf("recovery: %d %s", resp.StatusCode, strings.TrimSpace(string(body)))
		}
		time.Sleep(20 * time.Second)
	}
}

// renumberRunpodPod edits a pod's exposed ports, adding one, as its console
// does: RunPod restarts the pod and renumbers every port, SSH's included (a
// plain restart keeps them; observed 2026-10-19).
func renumberRunpodPod(t *testing.T, podID string) {
	t.Helper()
	key := strings.TrimSpace(readCloudSecrets()["RUNPOD_API_KEY"])
	if key == "" {
		t.Fatal("RUNPOD_API_KEY is required to edit the pod")
	}
	call := func(method, body string) []byte {
		req, err := http.NewRequestWithContext(t.Context(), method, "https://api.runpod.io/v2/pods/"+podID, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s pod %s: %v", method, podID, err)
		}
		data, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode/100 != 2 {
			t.Fatalf("%s pod %s: status %d: %s", method, podID, resp.StatusCode, data)
		}
		return data
	}
	var pod struct {
		Ports []string `json:"ports"`
	}
	if err := json.Unmarshal(call(http.MethodGet, ""), &pod); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{"ports": append(pod.Ports, "18899/tcp")})
	call(http.MethodPatch, string(body))
}
