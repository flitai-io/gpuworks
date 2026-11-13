//go:build cloudgpu && partition

package l4

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

const partitionHostProbe = `set +e
printf 'arch='; uname -m
printf 'docker='; command -v docker >/dev/null 2>&1 && echo yes || echo no
printf 'docker_runtime='; docker info --format '{{.DefaultRuntime}}' 2>/dev/null
printf 'docker_runtimes='; docker info --format '{{range $name, $_ := .Runtimes}}{{$name}},{{end}}' 2>/dev/null
printf 'nerdctl='; command -v nerdctl >/dev/null 2>&1 && nerdctl info >/dev/null 2>&1 && echo yes || echo no
printf 'nvidia='; command -v nvidia-smi >/dev/null 2>&1 && nvidia-smi --query-gpu=index,name,memory.total --format=csv,noheader,nounits 2>/dev/null | tr '\n' ';'
printf '\nascend='; command -v npu-smi >/dev/null 2>&1 && npu-smi info 2>/dev/null | awk '/^[|][[:space:]]*[0-9]+[[:space:]]+910/{gsub(/[|]/, ""); printf "%s,%s,%s;", $1, $2, $3}'
printf '\nppu='; command -v ppu-smi >/dev/null 2>&1 && ppu-smi --query-ppu=index,name,architecture,uuid --format=csv,noheader,nounits 2>/dev/null | tr '\n' ';'
printf '\nhggc_runtime='; command -v ppu-smi >/dev/null 2>&1 && ppu-smi -q -d VERSION 2>/dev/null | awk -F: '/HGGC Runtime Version/{gsub(/^[ \t]+|[ \t]+$/, "", $2); print $2; exit}'
printf 'gpuworks_agent='; pgrep -x gpuworks-agent >/dev/null 2>&1 && echo running || echo absent
printf 'containers='; command -v docker >/dev/null 2>&1 && docker ps --format '{{.Names}}' 2>/dev/null | sort | tr '\n' ','
printf '\n'`

func TestPartitionHardwarePreflight(t *testing.T) {
	t.Run("AscendPrimary", func(t *testing.T) {
		out := probePartitionHost(t, requiredHostEnv(t, "GPUWORKS_ASCEND_HOST"), hostEnv("GPUWORKS_ASCEND_SSH_JUMP", ""))
		// The primary Ascend host has been nerdctl-only since its containerd
		// repurposing; a working container runtime of either kind is enough.
		if !strings.Contains(out, "docker=yes") && !strings.Contains(out, "nerdctl=yes") {
			t.Fatalf("primary Ascend host exposes neither docker nor a working nerdctl/containerd:\n%s", out)
		}
		requireProbeContains(t, out, "arch=aarch64", "ascend=", "910B4")
		indexes := ascendIndexes(out)
		if len(indexes) < 1 {
			t.Fatalf("no visible Ascend indexes in probe:\n%s", out)
		}
		for i := 1; i < len(indexes); i++ {
			if indexes[i] <= indexes[i-1] {
				t.Fatalf("Ascend indexes are not stable/increasing: %v", indexes)
			}
		}
		t.Logf("observed Ascend indexes %v; health is a live observation and may differ from the historical Warning-card fixture", indexes)
	})

	t.Run("AscendSecondary", func(t *testing.T) {
		out := probePartitionHost(t, requiredHostEnv(t, "GPUWORKS_ASCEND_HOST_2"), hostEnv("GPUWORKS_ASCEND_SSH_JUMP_2", ""))
		requireProbeContains(t, out, "arch=aarch64", "ascend=", "910")
	})

	t.Run("PPUDockerPath", func(t *testing.T) {
		out := probePartitionHost(t, requiredHostEnv(t, "GPUWORKS_PPU_HOST"), hostEnv("GPUWORKS_PPU_SSH_JUMP", "gpu3"))
		requireProbeContains(t, out, "arch=x86_64", "docker=yes", "docker_runtime=runc", "ppu=", "PPU-ZW810E", "hggc_runtime=N/A")
		t.Log("host HGGC is absent by observation; the container-level probe, not this host version, decides partition readiness")
	})

	t.Run("NVIDIARegressionHost", func(t *testing.T) {
		out := probePartitionHost(t, hostEnv("GPUWORKS_NVIDIA_HOST", "gpu3"), hostEnv("GPUWORKS_NVIDIA_SSH_JUMP", ""))
		requireProbeContains(t, out, "arch=x86_64", "docker=yes", "nvidia=", "RTX", "nvidia,")
		if !strings.Contains(out, "gpuworks_agent=running") {
			t.Log("no pre-existing gpuworks-agent was observed; the destructive deployment suite may use an isolated installation")
		} else {
			t.Log("pre-existing gpuworks-agent observed; this preflight intentionally made no installation or service changes")
		}
	})
}

func TestPPUDockerPartitionQuota(t *testing.T) {
	host := requiredHostEnv(t, "GPUWORKS_PPU_HOST")
	jump := hostEnv("GPUWORKS_PPU_SSH_JUMP", "gpu3")
	image := hostEnv("GPUWORKS_PPU_L4_IMAGE", "swr.cn-south-1.myhuaweicloud.com/gpustack/runner:hggc13.0-vllm0.23.0")
	runID := "gpuworks-ppu-docker-l4-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	remoteDir := "/tmp/" + runID
	name := runID

	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve repository path")
	}
	repo := filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
	assetDir := filepath.Join(repo, "internal", "assets", "bin", "runtime-assets", "ppu", "linux-amd64", "hggc-13.0")
	quota := filepath.Join(assetDir, "hggc_quota.so")
	hook := filepath.Join(assetDir, "hgml_dlsym_hook.so")
	for _, path := range []string{quota, hook} {
		if stat, err := os.Stat(path); err != nil || stat.Size() < 4096 {
			t.Fatalf("PPU L4 asset %s is unavailable", path)
		}
	}

	runSSH := func(command string) (string, error) {
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
		defer cancel()
		cmd := partitionSSHCommand(ctx, host, jump, command)
		data, err := cmd.CombinedOutput()
		return string(data), err
	}
	if out, err := runSSH("mkdir -p " + shellQuoteL4(remoteDir) + " && test -d " + shellQuoteL4(remoteDir)); err != nil {
		if os.Getenv("GPUWORKS_TEST_STRICT") == "1" {
			t.Fatalf("prepare PPU L4 directory: %v: %s", err, out)
		}
		skipOrFailExternal(t, "PPU SSH unavailable: %v", err)
	}
	t.Cleanup(func() {
		// testing.T cancels its context before running cleanup callbacks. Use a
		// fresh bounded context so remote cleanup cannot be skipped silently.
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), time.Minute)
		defer cleanupCancel()
		command := "docker rm -f " + shellQuoteL4(name) + " >/dev/null 2>&1 || true; rm -f " + shellQuoteL4(filepath.Join(remoteDir, "hggc_quota.so")) + " " + shellQuoteL4(filepath.Join(remoteDir, "hgml_dlsym_hook.so")) + "; rmdir " + shellQuoteL4(remoteDir)
		data, cleanupErr := partitionSSHCommand(cleanupCtx, host, jump, command).CombinedOutput()
		if cleanupErr != nil {
			t.Errorf("cleanup PPU Docker quota probe: %v: %s", cleanupErr, data)
		}
	})

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	scp := partitionSCPCommand(ctx, jump, []string{quota, hook}, host+":"+remoteDir+"/")
	if data, err := scp.CombinedOutput(); err != nil {
		t.Fatalf("copy PPU L4 assets: %v: %s", err, data)
	}
	command := fmt.Sprintf(`set -eu
name=%s
dir=%s
test -d "$dir"
if docker ps -a --format '{{.Names}}' | grep -qx "$name"; then
  echo 'probe container already exists' >&2
  exit 1
fi
docker run --rm --name "$name" \
  --device /dev/alixpu --device /dev/alixpu_ctl --device /dev/alixpu_ppu0 \
  -e CUDA_VISIBLE_DEVICES=0 -e NVIDIA_VISIBLE_DEVICES=void \
  -e HGGC_DEVICE_MEMORY_LIMIT_0=1024 -e HGGC_DEVICE_SM_LIMIT_0=100 \
  -e HGGC_LEDGER_PATH=/dev/shm/"$name" \
  -e LD_PRELOAD=/opt/gpuworks/partition/hgml_dlsym_hook.so:/opt/gpuworks/partition/hggc_quota.so \
  -v "$dir/hggc_quota.so:/opt/gpuworks/partition/hggc_quota.so:ro" \
  -v "$dir/hgml_dlsym_hook.so:/opt/gpuworks/partition/hgml_dlsym_hook.so:ro" \
  --entrypoint /bin/bash %s -lc 'python3 - <<"PY"
import torch
free, total = torch.cuda.mem_get_info(0)
print("slice_total_mib", total // 1024 // 1024)
small = torch.ones(128 * 1024 * 1024, dtype=torch.uint8, device="cuda")
print("small_allocation_mib", small.numel() // 1024 // 1024)
try:
    torch.ones(2 * 1024 * 1024 * 1024, dtype=torch.uint8, device="cuda")
except torch.OutOfMemoryError as error:
    print("over_quota_denied", str(error).splitlines()[0])
else:
    raise SystemExit("unexpected: 2 GiB allocation succeeded under 1 GiB quota")
PY'`, shellQuoteL4(name), shellQuoteL4(remoteDir), shellQuoteL4(image))
	out, err := runSSH(command)
	archivePartitionEvidence(t, strings.ReplaceAll(t.Name(), "/", "-")+".txt", out)
	if err != nil {
		t.Fatalf("PPU Docker quota probe: %v\n%s", err, out)
	}
	requireProbeContains(t, out, "slice_total_mib 1024", "small_allocation_mib 128", "over_quota_denied CUDA out of memory")
}

func TestNVIDIADockerHAMiPartitionQuota(t *testing.T) {
	host := hostEnv("GPUWORKS_NVIDIA_HOST", "gpu3")
	jump := hostEnv("GPUWORKS_NVIDIA_SSH_JUMP", "")
	image := hostEnv("GPUWORKS_NVIDIA_L4_IMAGE", "quay.io/gpustack/runner:cuda13.0-vllm0.25.1")
	runID := "gpuworks-nvidia-dgpu-l4-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	remoteDir := "/tmp/" + runID
	name := runID

	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve repository path")
	}
	repo := filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
	libvgpu := filepath.Join(repo, "internal", "assets", "bin", "libvgpu.so")
	if stat, err := os.Stat(libvgpu); err != nil || stat.Size() < 4096 {
		t.Fatalf("NVIDIA L4 HAMi asset %s is unavailable", libvgpu)
	}

	runSSH := func(command string) (string, error) {
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
		defer cancel()
		cmd := partitionSSHCommand(ctx, host, jump, command)
		data, err := cmd.CombinedOutput()
		return string(data), err
	}
	prepare := "set -eu; used=$(nvidia-smi --id=0 --query-gpu=memory.used --format=csv,noheader,nounits); test \"$used\" -lt 1024; mkdir " + shellQuoteL4(remoteDir) + "; chmod 1777 " + shellQuoteL4(remoteDir) + "; touch " + shellQuoteL4(filepath.Join(remoteDir, runID+".cache")) + "; chmod 666 " + shellQuoteL4(filepath.Join(remoteDir, runID+".cache"))
	if out, err := runSSH(prepare); err != nil {
		if os.Getenv("GPUWORKS_TEST_STRICT") == "1" {
			t.Fatalf("prepare NVIDIA HAMi L4 directory or reserve idle GPU 0: %v: %s", err, out)
		}
		skipOrFailExternal(t, "NVIDIA HAMi L4 host unavailable or GPU 0 is busy: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), time.Minute)
		defer cleanupCancel()
		command := "docker rm -f " + shellQuoteL4(name) + " >/dev/null 2>&1 || true; rm -f " + shellQuoteL4(filepath.Join(remoteDir, "libvgpu.so")) + " " + shellQuoteL4(filepath.Join(remoteDir, runID+".cache")) + "; rmdir " + shellQuoteL4(remoteDir)
		data, cleanupErr := partitionSSHCommand(cleanupCtx, host, jump, command).CombinedOutput()
		if cleanupErr != nil {
			t.Errorf("cleanup NVIDIA Docker HAMi quota probe: %v: %s", cleanupErr, data)
		}
	})

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	scp := partitionSCPCommand(ctx, jump, []string{libvgpu}, host+":"+remoteDir+"/")
	if data, err := scp.CombinedOutput(); err != nil {
		t.Fatalf("copy NVIDIA HAMi L4 asset: %v: %s", err, data)
	}
	command := fmt.Sprintf(`set -eu
name=%s
dir=%s
test -d "$dir"
if docker ps -a --format '{{.Names}}' | grep -qx "$name"; then
  echo 'probe container already exists' >&2
  exit 1
fi
docker run --rm --name "$name" \
  --gpus 'device=0' \
  -e CUDA_VISIBLE_DEVICES=0 \
  -e CUDA_DEVICE_MEMORY_LIMIT_0=4294967296 \
  -e CUDA_DEVICE_MEMORY_SHARED_CACHE=/gpuworks/cache/%s.cache \
  -e LD_PRELOAD=/gpuworks/libvgpu.so \
  -v "$dir/libvgpu.so:/gpuworks/libvgpu.so:ro" \
  -v "$dir:/gpuworks/cache" \
  --entrypoint /bin/bash %s -lc 'python3 - <<"PY"
import torch
free, total = torch.cuda.mem_get_info(0)
print("slice_total_mib", total // 1024 // 1024)
small = torch.ones(256 * 1024 * 1024, dtype=torch.uint8, device="cuda")
print("small_allocation_mib", small.numel() // 1024 // 1024)
try:
    torch.ones(5 * 1024 * 1024 * 1024, dtype=torch.uint8, device="cuda")
except torch.OutOfMemoryError as error:
    print("over_quota_denied", str(error).splitlines()[0])
else:
    raise SystemExit("unexpected: 5 GiB allocation succeeded under 4 GiB quota")
PY'`, shellQuoteL4(name), shellQuoteL4(remoteDir), runID, shellQuoteL4(image))
	out, err := runSSH(command)
	archivePartitionEvidence(t, strings.ReplaceAll(t.Name(), "/", "-")+".txt", out)
	if err != nil {
		t.Fatalf("NVIDIA Docker HAMi quota probe: %v\n%s", err, out)
	}
	requireProbeContains(t, out, "slice_total_mib 4096", "small_allocation_mib 256", "over_quota_denied CUDA out of memory")
}

func TestAscendDockerPartitionInferenceAndWholeGPU(t *testing.T) {
	host := requiredHostEnv(t, "GPUWORKS_ASCEND_PRIMARY_HOST")
	jump := hostEnv("GPUWORKS_ASCEND_SSH_JUMP", "")
	image := requiredHostEnv(t, "GPUWORKS_ASCEND_L4_IMAGE")
	modelDir := hostEnv("GPUWORKS_ASCEND_L4_MODEL", "/opt/gpuworks/models/qwen3.5-0.8b")
	card := hostEnv("GPUWORKS_ASCEND_L4_CARD", "3")
	runID := "gpuworks-ascend-dgpu-l4-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	remoteDir := "/tmp/" + runID
	name := runID

	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve repository path")
	}
	repo := filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
	libvruntime := filepath.Join(repo, "internal", "assets", "bin", "runtime-assets", "ascend", "linux-arm64", "libvruntime.so")
	if stat, err := os.Stat(libvruntime); err != nil || stat.Size() < 4096 {
		t.Fatalf("Ascend L4 vcann-rt asset %s is unavailable", libvruntime)
	}

	configDir := t.TempDir()
	preload := filepath.Join(configDir, "ld.so.preload")
	config := filepath.Join(configDir, "npu_info.config")
	if err := os.WriteFile(preload, []byte("/usr/local/dcmi/libdcmi.so\n/usr/local/Ascend/driver/lib64/driver/libdcmi.so\n/opt/gpuworks/partition/libvruntime.so\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	configBody := fmt.Sprintf("physical-npu-id=%s\nvirtual-npu-id=0\naicore-quota=100\nmemory-quota=8192\nshm-id=%s\nscheduling-policy=2\n", card, runID)
	if err := os.WriteFile(config, []byte(configBody), 0o644); err != nil {
		t.Fatal(err)
	}

	runSSH := func(timeout time.Duration, command string) (string, error) {
		ctx, cancel := context.WithTimeout(t.Context(), timeout)
		defer cancel()
		cmd := partitionSSHCommand(ctx, host, jump, command)
		data, err := cmd.CombinedOutput()
		return string(data), err
	}
	prepare := "set -eu; test -d " + shellQuoteL4(modelDir) + "; docker image inspect " + shellQuoteL4(image) + " >/dev/null; npu-smi info | grep -Fq " + shellQuoteL4("No running processes found in NPU "+card) + "; mkdir " + shellQuoteL4(remoteDir)
	if out, err := runSSH(time.Minute, prepare); err != nil {
		if os.Getenv("GPUWORKS_TEST_STRICT") == "1" {
			t.Fatalf("prepare idle Ascend card %s, image, and model: %v: %s", card, err, out)
		}
		skipOrFailExternal(t, "Ascend inference fixture unavailable or card %s is busy: %v", card, err)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), time.Minute)
		defer cleanupCancel()
		command := "docker rm -f " + shellQuoteL4(name) + " >/dev/null 2>&1 || true; rm -f " + shellQuoteL4(filepath.Join(remoteDir, "libvruntime.so")) + " " + shellQuoteL4(filepath.Join(remoteDir, "ld.so.preload")) + " " + shellQuoteL4(filepath.Join(remoteDir, "npu_info.config")) + "; rmdir " + shellQuoteL4(remoteDir)
		data, cleanupErr := partitionSSHCommand(cleanupCtx, host, jump, command).CombinedOutput()
		if cleanupErr != nil {
			t.Errorf("cleanup Ascend vcann-rt inference probe: %v: %s", cleanupErr, data)
		}
	})

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	scp := partitionSCPCommand(ctx, jump, []string{libvruntime, preload, config}, host+":"+remoteDir+"/")
	if data, err := scp.CombinedOutput(); err != nil {
		t.Fatalf("copy Ascend vcann-rt L4 assets: %v: %s", err, data)
	}
	start := fmt.Sprintf(`set -eu
docker run -d --name %s \
  --runtime=ascend --shm-size=16g \
  -e ASCEND_VISIBLE_DEVICES=%s -e ENPU_LOG_LEVEL=1 -e ENPU_DSMI_HOOK=1 \
  -v %s:/etc/ld.so.preload:ro \
  -v %s:/etc/enpu/vcann-rt/npu_info.config:ro \
  -v %s:/opt/gpuworks/partition/libvruntime.so:ro \
  -v %s:/model:ro -v /dev/shm:/dev/shm \
  %s vllm serve /model \
    --served-model-name qwen35-08b-ascend-vgpu \
    --host 0.0.0.0 --port 8000 --dtype bfloat16 \
    --max-model-len 2048 --max-num-batched-tokens 2048 \
    --max-num-seqs 4 --gpu-memory-utilization 0.80 --enforce-eager`,
		shellQuoteL4(name), shellQuoteL4(card), shellQuoteL4(filepath.Join(remoteDir, "ld.so.preload")), shellQuoteL4(filepath.Join(remoteDir, "npu_info.config")), shellQuoteL4(filepath.Join(remoteDir, "libvruntime.so")), shellQuoteL4(modelDir), shellQuoteL4(image))
	if out, err := runSSH(time.Minute, start); err != nil {
		t.Fatalf("start Ascend vcann-rt vLLM: %v: %s", err, out)
	}
	wait := fmt.Sprintf(`set -eu
for attempt in $(seq 1 100); do
  if docker exec %s curl -fsS http://127.0.0.1:8000/health >/dev/null 2>&1; then
    echo partition_health=ok
    exit 0
  fi
  if [ "$(docker inspect --format '{{.State.Running}}' %s 2>/dev/null || true)" != true ]; then
    docker logs --tail 160 %s 2>&1
    exit 1
  fi
  sleep 3
done
docker logs --tail 160 %s 2>&1
exit 1`, shellQuoteL4(name), shellQuoteL4(name), shellQuoteL4(name), shellQuoteL4(name))
	waitOut, err := runSSH(6*time.Minute, wait)
	if err != nil {
		t.Fatalf("wait for Ascend vcann-rt vLLM: %v\n%s", err, waitOut)
	}
	verify := fmt.Sprintf(`set -eu
docker exec %s curl -fsS http://127.0.0.1:8000/v1/models | grep -q 'qwen35-08b-ascend-vgpu'
echo models=ok
docker exec %s curl -fsS -H 'Content-Type: application/json' \
  -d '{"model":"qwen35-08b-ascend-vgpu","messages":[{"role":"user","content":"Reply with: Ascend vGPU works"}],"max_tokens":16,"temperature":0}' \
  http://127.0.0.1:8000/v1/chat/completions | grep -q 'choices'
echo chat=ok
docker exec -i %s python3 - <<'PY'
import acl
acl.init()
acl.rt.set_device(0)
context, result = acl.rt.create_context(0)
if result != 0:
    raise SystemExit("create_context failed: %%s" %% result)
pointer, result = acl.rt.malloc(8 * 1024 * 1024 * 1024, 0)
if result == 0:
    acl.rt.free(pointer)
    raise SystemExit("unexpected: 8 GiB allocation succeeded while vLLM occupied the 8 GiB slice")
print("over_quota_denied_acl", result)
acl.rt.destroy_context(context)
acl.rt.reset_device(0)
acl.finalize()
PY
docker exec %s curl -fsS http://127.0.0.1:8000/health >/dev/null
echo health_after_denial=ok
docker rm -f %s >/dev/null
docker run --rm -i --runtime=ascend -e ASCEND_VISIBLE_DEVICES=%s --entrypoint python3 %s - <<'PY'
import torch
free, total = torch.npu.mem_get_info(0)
print("whole_gpu_total_mib", total // 1024 // 1024)
allocation = torch.ones(128 * 1024 * 1024, dtype=torch.uint8, device="npu")
print("whole_gpu_allocation_mib", allocation.numel() // 1024 // 1024)
if total < 24 * 1024 * 1024 * 1024:
    raise SystemExit("whole-GPU path still appears sliced")
PY`, shellQuoteL4(name), shellQuoteL4(name), shellQuoteL4(name), shellQuoteL4(name), shellQuoteL4(name), shellQuoteL4(card), shellQuoteL4(image))
	verifyOut, err := runSSH(4*time.Minute, verify)
	combined := waitOut + verifyOut
	archivePartitionEvidence(t, strings.ReplaceAll(t.Name(), "/", "-")+".txt", combined)
	if err != nil {
		t.Fatalf("verify Ascend vcann-rt inference, quota, and whole-GPU path: %v\n%s", err, combined)
	}
	requireProbeContains(t, combined, "partition_health=ok", "models=ok", "chat=ok", "over_quota_denied_acl", "health_after_denial=ok", "whole_gpu_total_mib", "whole_gpu_allocation_mib 128")
}

func probePartitionHost(t *testing.T, host, jump string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	cmd := partitionSSHCommand(ctx, host, jump, partitionHostProbe)
	data, err := cmd.CombinedOutput()
	if err != nil {
		message := fmt.Sprintf("SSH probe %s failed: %v", host, err)
		if os.Getenv("GPUWORKS_TEST_STRICT") == "1" {
			t.Fatalf("%s\n%s", message, data)
		}
		skipOrFailExternal(t, "%s", message)
	}
	out := string(data)
	archivePartitionEvidence(t, strings.ReplaceAll(t.Name(), "/", "-")+".txt", out)
	return out
}

func hostEnv(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func requiredHostEnv(t *testing.T, name string) string {
	t.Helper()
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	message := name + " is required; load the private test-host configuration from secrets/test-environments.md"
	if os.Getenv("GPUWORKS_TEST_STRICT") == "1" {
		t.Fatal(message)
	}
	skipOrFailExternal(t, "%s", message)
	return ""
}

func requireProbeContains(t *testing.T, output string, values ...string) {
	t.Helper()
	for _, value := range values {
		if !strings.Contains(output, value) {
			t.Fatalf("probe output does not contain %q:\n%s", value, output)
		}
	}
}

func probeValue(output, name string) string {
	prefix := name + "="
	for _, line := range strings.Split(output, "\n") {
		if strings.HasPrefix(line, prefix) {
			return strings.TrimSpace(strings.TrimPrefix(line, prefix))
		}
	}
	return ""
}

var ascendIndexPattern = regexp.MustCompile(`(?:^|;)([0-9]+),910`)

func ascendIndexes(output string) []string {
	line := probeValue(output, "ascend")
	var out []string
	for _, match := range ascendIndexPattern.FindAllStringSubmatch(line, -1) {
		out = append(out, match[1])
	}
	sort.Strings(out)
	return out
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func archivePartitionEvidence(t *testing.T, name, content string) {
	t.Helper()
	dir := strings.TrimSpace(os.Getenv("GPUWORKS_L4_ARTIFACT_DIR"))
	if dir == "" {
		_, file, _, ok := runtime.Caller(0)
		if !ok {
			t.Fatal("resolve L4 artifact directory")
		}
		dir = filepath.Join(filepath.Dir(file), "..", "..", "artifacts", "partition-framework-l4")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
