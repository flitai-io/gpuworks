//go:build cloudgpu && (compshare || aliyun)

package l4

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/agent/engine"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/partitioncap"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/testenv"
	"gpuworks.ai/gpuworks/internal/testenv/cloudprovider"
)

const compshareDockerTestImage = "swr.cn-south-1.myhuaweicloud.com/gpustack/runner:cuda12.8-vllm0.17.1"

func compshareTestImage() string {
	if image := strings.TrimSpace(os.Getenv("COMPSHARE_TEST_IMAGE")); image != "" {
		return image
	}
	return "gpustack/runner:cuda12.8-vllm0.17.1"
}

func compshareDistributedTestImage() string {
	if image := strings.TrimSpace(os.Getenv("COMPSHARE_DISTRIBUTED_TEST_IMAGE")); image != "" {
		return image
	}
	if image := strings.TrimSpace(os.Getenv("COMPSHARE_TEST_IMAGE")); image != "" {
		return image
	}
	return compshareTestImage()
}

// TestCompshareSingleVMTensorParallelAndDGPU reuses one VM with at least two
// GPUs, preloaded model and image cache across the expensive inference and
// dGPU partitioning acceptance cases.
func TestCompshareSingleVMTensorParallelAndDGPU(t *testing.T) {
	runSingleVMTensorParallelAndDGPU(t, compshareBackend(t))
}

// runSingleVMTensorParallelAndDGPU is the provider-neutral body of the case.
func runSingleVMTensorParallelAndDGPU(t *testing.T, b *vmBackend) {
	// dGPU replicas take 5GiB and two share one card: 10GiB, which an 11GB
	// 2080Ti holds.
	var gpuModel string
	var gpuCount int
	checked := b.EachShape(t, false, 2, 10240, func(model string, count int) bool {
		gpuModel, gpuCount = model, count
		return true
	})
	if gpuModel == "" {
		skipOrFailExternal(t, "no %s VM with two or more GPUs is rentable: %s", b.Name, strings.Join(checked, "; "))
	}
	env := setupCloudNode(t, testenv.NewWithModelEndpoint(t, b.ModelEndpoint), b.Provider, cloudprovider.GPUSpec{GPUModel: gpuModel, GPUCount: gpuCount, MinVRAMMB: 10240, DiskGB: b.DiskGB})
	logRentedInstance(t, b, env.Instance)
	requireOnlineNode(t, env)
	// CompShare's UAAA route is optimized for the domestic SWR mirror. Prefer
	// it for this live suite so a fresh VM does not spend the acceptance window
	// waiting on a multi-gigabyte quay.io pull.
	if len(b.PullMirror) > 0 {
		if _, err := env.Control.Store().CreatePullSourceRule(store.PullSourceRuleSpec{Match: "gpustack/", Endpoints: b.PullMirror, AllowUpstream: true}); err != nil {
			t.Fatal(err)
		}
	}
	if !t.Run("ProvisionAndRuntime", func(t *testing.T) { testVMAndRuntime(t, env, gpuCount) }) {
		return
	}
	// Fail on a broken golden-image fixture instead of allowing the runtime to
	// silently fall back to a multi-gigabyte registry pull. The shared helper
	// may load a docker-save archive already present on the VM, but never pulls.
	b.EnsureEngineImage(t, env, gpuModel)
	model := b.registerModel(t, env)
	registerPreloadedModel(t, b, env, model)
	if !t.Run("VLLMTensorParallel2", func(t *testing.T) {
		// The suite-level timeout covers engine startup and inference; image
		// acquisition is intentionally completed by the offline cache check.
		d := createCloudDeploymentWithTimeout(t, []*cloudTestEnv{env}, b.withEngine(map[string]any{
			"model_id": model.ID, "node_uuid": env.NodeUUID, "served_name": b.servedName("tp2"),
			"placement":    map[string]any{"tp": 2},
			"quick_params": map[string]any{"max_model_len": 1024},
		}, gpuModel), 1, 45*time.Minute)
		wantImage, _, _ := b.Engine(gpuModel)
		requireInstancesRunImage(t, d.instances, wantImage)
		d.models(t)
		d.chat(t)
		d.stop(t)
	}) {
		return
	}
	t.Run("DGPUReplicasShareAGPU", func(t *testing.T) { testDGPUReplicas(t, b, env, model.ID, gpuCount, gpuModel) })
}

func registerPreloadedModel(t *testing.T, b *vmBackend, env *cloudTestEnv, model *store.Model) {
	t.Helper()
	path := b.PreloadedModelPath(t)
	if out, err := runCloudCommand(t.Context(), env.Instance, "test -d "+shellQuoteForTest(path)); err != nil {
		t.Fatalf("preloaded model path is unavailable: %v (%s)", err, strings.TrimSpace(out))
	}
	node, err := env.Control.Store().NodeByUUID(env.NodeUUID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.Control.Store().RegisterModelFile(store.FileRef{ModelID: model.ID, NodeID: node.ID}, path, 0); err != nil {
		t.Fatalf("register preloaded model path: %v", err)
	}
}

func testVMAndRuntime(t *testing.T, env *cloudTestEnv, wantGPUs int) {
	if env.Instance.Kind != "vm" {
		t.Fatalf("provider product kind=%q, want vm", env.Instance.Kind)
	}
	node, err := env.Control.Store().NodeWithGPUs(env.NodeUUID)
	if err != nil {
		t.Fatal(err)
	}
	if node.RuntimeDriver != "docker" && node.RuntimeDriver != "udocker" {
		t.Fatalf("VM runtime=%q, want docker or udocker", node.RuntimeDriver)
	}
	ready := partitioncap.ReadyBackendIDs(node)
	if len(ready) != 1 || ready[0] != accelerator.BackendNVIDIALogical {
		t.Fatalf("VM ready partition backends=%v, want %s", ready, accelerator.BackendNVIDIALogical)
	}
	// Keep the probes independent. A long compound command can return an
	// empty SSH buffer on some cloud images even though the agent is healthy;
	// the GPU count and node metadata are the authoritative assertions.
	virt, _ := runCloudCommand(t.Context(), env.Instance, "systemd-detect-virt 2>/dev/null || true")
	docker, _ := runCloudCommand(t.Context(), env.Instance, "if command -v docker >/dev/null 2>&1; then docker version --format '{{.Server.Version}}' 2>/dev/null || true; else printf absent; fi")
	var gpuCount int
	var gpuOutput string
	var gpuErr error
	for attempt := 0; attempt < 3; attempt++ {
		gpuOutput, gpuErr = runCloudCommand(t.Context(), env.Instance, "nvidia-smi -L 2>/dev/null | wc -l")
		if gpuErr == nil && strings.TrimSpace(gpuOutput) != "" {
			gpuCount, _ = strconv.Atoi(strings.TrimSpace(gpuOutput))
			if gpuCount > 0 {
				break
			}
		}
		time.Sleep(time.Duration(attempt+1) * time.Second)
	}
	if gpuErr != nil || gpuCount != wantGPUs {
		t.Fatalf("GPU evidence=%q count=%d want=%d err=%v", strings.TrimSpace(gpuOutput), gpuCount, wantGPUs, gpuErr)
	}
	if strings.EqualFold(strings.TrimSpace(virt), "none") {
		t.Fatalf("VM reports no virtualization: %q", strings.TrimSpace(virt))
	}
	t.Logf("VM evidence: virt=%s docker=%s runtime=%s gpu_count=%d", strings.TrimSpace(virt), strings.TrimSpace(docker), node.RuntimeDriver, gpuCount)
}

// engineCacheGlob returns the glob for libvgpu usage-cache files the agent
// writes under the given state dir on the VM host. It is derived from
// Engine.DGPUCachePath — the agent's single source for the directory name —
// so a future rename of the on-disk directory cannot silently orphan this
// assertion like the hami-cache → dgpu-cache rename did.
func engineCacheGlob(stateDir string) string {
	e := engine.New(nil, nil, nil, stateDir)
	return filepath.Join(filepath.Dir(e.DGPUCachePath("probe")), "*.cache")
}

// testDGPUReplicas starts one dGPU replica more than the node has
// GPUs, so at least one card must be shared, whatever the scheduler picks.
func testDGPUReplicas(t *testing.T, b *vmBackend, env *cloudTestEnv, modelID uint, gpus int, gpuModel string) {
	replicas := gpus + 1
	d := createCloudDeployment(t, []*cloudTestEnv{env}, b.withEngine(map[string]any{
		"model_id": modelID, "node_uuid": env.NodeUUID, "served_name": b.servedName("dgpu"),
		"replicas":  replicas,
		"placement": map[string]any{"tp": 1}, "vram_limit_gib": 5,
		"quick_params": map[string]any{"max_model_len": 1024, "gpu_memory_utilization": 0.8},
	}, gpuModel), replicas)
	wantImage, _, _ := b.Engine(gpuModel)
	requireInstancesRunImage(t, d.instances, wantImage)
	seenGPU := map[int]int{}
	for _, instance := range d.instances {
		var spec proto.InstanceDesiredState
		if err := json.Unmarshal([]byte(instance.DesiredStateJSON), &spec); err != nil {
			t.Fatal(err)
		}
		if spec.VRAMLimitBytes != 5<<30 || len(spec.GPUIndexes) != 1 {
			t.Fatalf("dGPU instance spec=%+v", spec)
		}
		seenGPU[spec.GPUIndexes[0]]++
	}
	shared := false
	for _, count := range seenGPU {
		if count >= 2 {
			shared = true
		}
	}
	if !shared {
		t.Fatalf("%d dGPU replicas on %d GPUs shared no GPU: %v", replicas, gpus, seenGPU)
	}
	d.chat(t)
	out, err := runCloudCommand(t.Context(), env.Instance, fmt.Sprintf("find /opt/gpuworks /tmp/gpuworks -type f \\( -path '*/hami-cache/*.cache' -o -path '%s' -o -path '%s' -o -path '*/ROOT/gpuworks/cache/*.cache' \\) -size +0c -printf '%%p\\n' 2>/dev/null | sort -u", engineCacheGlob("/opt/gpuworks"), engineCacheGlob("/tmp/gpuworks")))
	if err != nil {
		t.Fatal(err)
	}
	paths := strings.Fields(out)
	if len(paths) < replicas {
		diagnostic, diagnosticErr := runCloudCommand(t.Context(), env.Instance, "set +e; echo '== cache directories =='; for d in /opt/gpuworks/hami-cache /tmp/gpuworks/hami-cache /opt/gpuworks/dgpu-cache /tmp/gpuworks/dgpu-cache /opt/gpuworks/udocker/containers/*/ROOT/gpuworks/cache /tmp/gpuworks/udocker/containers/*/ROOT/gpuworks/cache; do [ -e \"$d\" ] && ls -ld \"$d\"; done; echo '== cache files =='; find /opt/gpuworks /tmp/gpuworks /tmp -type f -name '*.cache' -ls 2>/dev/null; echo '== agent files =='; find /opt/gpuworks /tmp/gpuworks -maxdepth 3 -type f -printf '%p\\n' 2>/dev/null | sort; echo '== container logs =='; for f in /opt/gpuworks/udocker-state/*.log /tmp/gpuworks/udocker-state/*.log; do [ -f \"$f\" ] || continue; echo --- $f; tail -n 120 \"$f\"; done")
		if diagnosticErr != nil {
			diagnostic += " (diagnostic error: " + diagnosticErr.Error() + ")"
		}
		t.Fatalf("dGPU (HAMi-core) produced %d non-empty cache files, want at least %d: %q\n%s", len(paths), replicas, strings.TrimSpace(out), strings.TrimSpace(diagnostic))
	}
	t.Logf("dGPU shared GPUs=%v cache_files=%v", seenGPU, paths)
}

// TestCompshareTwoVMUdockerDistributedInference proves four separate layers: two real VMs,
// two GPUs per VM, bidirectional private-network bandwidth, and a real vLLM
// request served by a two-rank/four-GPU distributed deployment.
func TestCompshareTwoVMUdockerDistributedInference(t *testing.T) {
	runTwoVMUdockerDistributed(t, compshareBackend(t))
}

// runTwoVMUdockerDistributed is the provider-neutral body of the case.
func runTwoVMUdockerDistributed(t *testing.T, b *vmBackend) {
	harness := newHostedControlPlane(t, hostedControlPlaneConfig{
		RunID: cloudTestRunID(), RuntimeDrivers: [2]string{"udocker", "udocker"},
		Backend: b, DiskGB: b.UdockerDiskGB, OnDemand: b.UdockerOnDemand,
		PrepareVMs: installDistributedVLLMFix, ArchiveFromVMs: b.UdockerImageFromURL,
	})
	for rank, node := range harness.Nodes {
		if node.State != "online" || node.GPUCount != harness.gpusPerVM || node.ManagementAddress != harness.VMs[rank].Instance.PrivateIP ||
			node.RuntimeDriver != "udocker" || node.ConnectMode != "agent_dial" || node.ServerURL != harness.PrivateURL {
			t.Fatalf("rank %d hosted udocker node state/gpus/management/runtime/mode/server=%q/%d/%q/%q/%q/%q",
				rank, node.State, node.GPUCount, node.ManagementAddress, node.RuntimeDriver, node.ConnectMode, node.ServerURL)
		}
	}
	// The workstation forward carries only REST/gateway traffic. Rebuild it and
	// prove that both cloud agents remain on their direct private sessions before
	// spending time on the distributed model and image path.
	harness.API.DropForward()
	for _, node := range hostedWaitNodes(t, harness, 2, 2*time.Minute) {
		if node.State != "online" {
			t.Fatalf("node %s disconnected when the local API forward was rebuilt", node.Name)
		}
	}
	hostedRequireDistributedPreflight(t, harness, 29500)
	model := hostedPrepareRealModel(t, harness)
	// udocker has its own image store. A backend whose VMs already hold the
	// runner image in Docker hands it over as an archive the platform imports
	// from a URL, so the deployment below finds the image present and never
	// pulls; otherwise each VM pulls the large image independently, with the
	// same startup allowance as the single-node CompShare real-model path so a
	// slow registry layer on either rank does not make the case stricter.
	var imported hostedImportedImage
	if b.UdockerImageFromURL {
		imported = hostedImportRunnerImageFromArchive(t, harness, b.DistributedImage)
	}
	d := createHostedDeployment(t, harness, map[string]any{
		"model_id": model.ID, "node_uuid": harness.Nodes[0].UUID,
		"node_uuids":  []string{harness.Nodes[0].UUID, harness.Nodes[1].UUID},
		"served_name": b.servedName("dist4"), "engine": "vllm", "image": b.DistributedImage,
		"nnodes": 2, "master_addr": harness.VMs[0].Instance.PrivateIP, "master_port": 29500,
		"placement": map[string]any{"tp": 2, "pp": 2}, "quick_params": map[string]any{"max_model_len": 1024},
		"container_overrides": distributedVLLMContainerOverrides(),
	}, 2, 75*time.Minute)
	ranks := []int{d.instances[0].NodeRank, d.instances[1].NodeRank}
	sort.Ints(ranks)
	if ranks[0] != 0 || ranks[1] != 1 {
		t.Fatalf("distributed ranks=%v, want [0 1]", ranks)
	}
	if b.UdockerImageFromURL {
		hostedRequireImportedImageStillPresent(t, harness, imported)
	}
	d.models(t)
	d.chat(t)
	d.stop(t)
}

func compshareModelEndpoint() string {
	if endpoint := strings.TrimSpace(os.Getenv("COMPSHARE_MODEL_ENDPOINT")); endpoint != "" {
		return endpoint
	}
	return strings.TrimSpace(os.Getenv("GPUWORKS_MODEL_ENDPOINT"))
}

func availableCompshareGPU(t *testing.T, p *cloudprovider.Compshare, gpuCount, minVRAMMB int) string {
	t.Helper()
	model, checked := findCompshareGPU(t, p, gpuCount, minVRAMMB)
	if model == "" {
		skipOrFailExternal(t, "no live CompShare %d-GPU shape available: %s", gpuCount, strings.Join(checked, "; "))
	}
	return model
}

// eachCompshareShape calls try for every card type and shape with at least
// cards GPUs that SelectPlacement says is in stock, until try returns true:
// exactly cards across every type first, then the smallest wider shape any
// type has. 2080Ti, the cn-sh2-02 mainstay, only comes as x4 or x8, and a case
// that needs two cards can use two of four.
//
// Stock is a snapshot: another tenant can take the last card between this
// check and the create, so a caller that rents returns false on a capacity
// refusal and is offered the next shape instead of failing the case. It
// returns why each shape was passed over.
func eachCompshareShape(t *testing.T, p *cloudprovider.Compshare, models []string, cards, minVRAMMB, diskGB int, try func(model string, count int) bool) []string {
	t.Helper()
	var checked []string
	counts := []int{cards}
	for _, wider := range []int{4, 8} {
		if wider > cards {
			counts = append(counts, wider)
		}
	}
	for _, count := range counts {
		for _, model := range models {
			if reason := belowVRAM(model, minVRAMMB); reason != "" {
				if count == cards {
					checked = append(checked, reason)
				}
				continue
			}
			selected, zone, err := p.SelectPlacement(t.Context(), model, count, diskGB)
			if err != nil {
				checked = append(checked, fmt.Sprintf("%s x%d: %v", model, count, err))
				continue
			}
			t.Logf("selected live CompShare VM shape: %s x%d cpu=%d memory_gb=%d region=%s zone=%s", model, count, selected.CPU, selected.MemoryGB, zone.Region, zone.Zone)
			if try(model, count) {
				return nil
			}
			checked = append(checked, fmt.Sprintf("%s x%d: could not be created", model, count))
		}
	}
	return checked
}

// findCompshareShape returns the first shape eachCompshareShape offers.
func findCompshareShape(t *testing.T, p *cloudprovider.Compshare, models []string, cards, minVRAMMB, diskGB int) (string, int, []string) {
	t.Helper()
	var model string
	var count int
	checked := eachCompshareShape(t, p, models, cards, minVRAMMB, diskGB, func(m string, c int) bool {
		model, count = m, c
		return true
	})
	return model, count, checked
}

// belowVRAM explains why model has too little memory per card, or "".
func belowVRAM(model string, minVRAMMB int) string {
	if vram, known := cloudprovider.GPUVRAMMB(model); known && minVRAMMB > 0 && vram < minVRAMMB {
		return fmt.Sprintf("%s: %d MiB per card, below the case's %d MiB", model, vram, minVRAMMB)
	}
	return ""
}

// compshareGPUOrder is the card types a case may run on, cheapest first.
// COMPSHARE_GPU_MODELS (comma-separated, e.g. "V100S") replaces the list, to
// aim a run at one card type, such as the Volta image acceptance.
func compshareGPUOrder() []string {
	if raw := strings.TrimSpace(os.Getenv("COMPSHARE_GPU_MODELS")); raw != "" {
		var models []string
		for _, model := range strings.Split(raw, ",") {
			if model = strings.TrimSpace(model); model != "" {
				models = append(models, model)
			}
		}
		return models
	}
	// Cheapest first, then wider when both regions are temporarily
	// exhausted. 2080Ti (11GB, the cn-sh2-02 mainstay) is eligible for every
	// count: the HAMi case stacks two 5GiB dGPU slices on one card, which it
	// holds; cases that need more memory per card say so with MinVRAMMB and
	// rentCompshareGPU skips smaller cards. V100S/P40/A800/H20 are the rest
	// of the datacenter catalog: a sweep of every other GpuType spelling
	// (4080/4090D/L20/L40/A10/A30/A40/H100/H800/V100/...) answered RetCode
	// 8433 in both zones, i.e. not offered (2026-09-25, re-checked
	// 2026-10-04). V100S and P40 have no GSP and need the proprietary kernel
	// modules, which the golden image loads at boot when the open ones
	// initialise no GPU (prepare-compshare-golden -dual-kmod; images made
	// before that fail on them). CompShare refuses custom images for A800 and
	// H20 (RetCode 226603), so SelectPlacement passes over them. A100 stays
	// the expensive last resort.
	return []string{"3090", "4090", "5090", "4090_48G", "2080Ti", "V100S", "P40", "A800", "H20", "A100"}
}

// findCompshareGPU returns the first card type with at least minVRAMMB per
// card and a rentable gpuCount shape, or "" and why each type was passed over.
func findCompshareGPU(t *testing.T, p *cloudprovider.Compshare, gpuCount, minVRAMMB int) (string, []string) {
	t.Helper()
	var checked []string
	for _, model := range compshareGPUOrder() {
		if reason := belowVRAM(model, minVRAMMB); reason != "" {
			checked = append(checked, reason)
			continue
		}
		selected, zone, err := p.SelectPlacement(t.Context(), model, gpuCount, compshareAcceptanceDiskGB)
		if err != nil {
			checked = append(checked, model+": "+err.Error())
			continue
		}
		t.Logf("selected live CompShare VM shape: %s x%d cpu=%d memory_gb=%d region=%s zone=%s", model, gpuCount, selected.CPU, selected.MemoryGB, zone.Region, zone.Zone)
		return model, nil
	}
	return "", checked
}

// rentCompshareGPU rents a gpuCount VM of the first card type that has stock
// and can still be created. Capacity is a snapshot: another tenant can take
// the last card between the check and the create, so a capacity or
// zone-image refusal moves on to the next type instead of failing the case.
// It returns nil and every type's reason when nothing could be rented.
func rentCompshareGPU(t *testing.T, p *cloudprovider.Compshare, spec cloudprovider.GPUSpec) (*cloudprovider.Instance, []string) {
	t.Helper()
	var checked []string
	for _, model := range compshareGPUOrder() {
		if reason := belowVRAM(model, spec.MinVRAMMB); reason != "" {
			checked = append(checked, reason)
			continue
		}
		selected, zone, err := p.SelectPlacement(t.Context(), model, spec.GPUCount, spec.DiskGB)
		if err != nil {
			checked = append(checked, model+": "+err.Error())
			continue
		}
		t.Logf("selected live CompShare VM shape: %s x%d cpu=%d memory_gb=%d region=%s zone=%s", model, spec.GPUCount, selected.CPU, selected.MemoryGB, zone.Region, zone.Zone)
		attempt := spec
		attempt.GPUModel = model
		instance, err := p.Rent(t.Context(), attempt)
		if err != nil {
			if isCloudCapacityError(err) || cloudprovider.IsImageUnsupportedInZone(err) || strings.Contains(err.Error(), "RetCode 226604") {
				t.Logf("CompShare %s x%d could not be created, trying the next type: %v", model, spec.GPUCount, err)
				checked = append(checked, model+": "+err.Error())
				continue
			}
			t.Fatal(err)
		}
		// The CompShare GpuType asked for, which callers compare against.
		instance.GPUModel = model
		return instance, nil
	}
	return nil, checked
}

// compshareContainerGPUOrder puts the GpuTypes the container zones carry
// first: a 2026-10-26 sweep of cn-bj2-03, cn-sh2-01 and cn-sh2-03 found only
// 4090, 4090_48G and 5090, every other type answering RetCode 230. The rest of
// the VM order follows in case the catalog grows.
func compshareContainerGPUOrder() []string {
	order := []string{"4090", "4090_48G", "5090"}
	for _, model := range compshareGPUOrder() {
		if !slices.Contains(order, model) {
			order = append(order, model)
		}
	}
	return order
}

// rentCompshareContainer rents a gpuCount container of the first card type
// that can be created in any container zone. There is no capacity snapshot to
// consult: the container zones differ from the VM zones SelectPlacement
// searches, so the driver places the container itself, walking every pod zone
// for each type (CompShare configures each GpuType in only some of them: on
// 2026-10-26 4090 only in cn-bj2-03, 5090 only in cn-sh2-01, 4090_48G only in
// cn-sh2-03, and any other zone answers RetCode 230). A type no zone can host
// moves on to the next one. It returns nil and, per type, every zone's reason
// when nothing could be rented.
//
// When every type was refused for lack of supply the whole walk is repeated
// every 5 minutes for COMPSHARE_STOCK_WAIT (default 30m, "0" disables); nothing
// is rented while waiting, and any other error fails the test at once.
func rentCompshareContainer(t *testing.T, p *cloudprovider.Compshare, spec cloudprovider.GPUSpec) (*cloudprovider.Instance, []string) {
	t.Helper()
	wait, err := cloudprovider.StockWaitFromEnv("COMPSHARE_STOCK_WAIT", cloudprovider.DefaultStockWait)
	if err != nil {
		t.Fatal(err)
	}
	walk := cloudprovider.CompshareWalk{
		Models: compshareContainerGPUOrder(),
		Skip:   func(model string) string { return belowVRAM(model, spec.MinVRAMMB) },
		Rent: func(ctx context.Context, model string) (*cloudprovider.Instance, error) {
			attempt := spec
			attempt.GPUModel = model
			instance, err := p.Rent(ctx, attempt)
			if err != nil {
				return nil, err
			}
			// The CompShare GpuType asked for, which callers compare against.
			instance.GPUModel = model
			return instance, nil
		},
		// Supply refusals include 230 "machine type not configured for zone":
		// the container zones carry fewer GPU types than the VM zones.
		TryNext: func(err error) bool {
			return isCloudCapacityError(err) || cloudprovider.IsCompshareSupplyRefusal(err)
		},
		OnTryNext: func(model string, err error) {
			t.Logf("CompShare container %s x%d could not be created in any container zone, trying the next type: %v", model, spec.GPUCount, err)
		},
	}
	instance, checked, err := walk.RentWithStockWait(t.Context(), wait)
	switch {
	case errors.Is(err, cloudprovider.ErrCompshareOutOfStock):
		return nil, append(checked, "stock wait: "+err.Error())
	case err != nil:
		t.Fatal(err)
	}
	return instance, checked
}

func chooseCompshareGPU(gpuCount int, check func(string) ([]cloudprovider.CapacitySpec, error)) (string, cloudprovider.CapacitySpec, []string) {
	var checked []string
	// Prefer the lower-cost 3090 shape and fall back to 4090 when its live
	// capacity is exhausted. The SSH assertion below still rejects a capacity
	// response whose provisioned VM exposes the wrong GPU count or model.
	for _, model := range []string{"3090", "4090"} {
		rows, err := check(model)
		if err != nil {
			checked = append(checked, model+": "+err.Error())
			continue
		}
		checked = append(checked, fmt.Sprintf("%s: %+v", model, rows))
		for _, row := range rows {
			if row.GPU == gpuCount && row.ResourceEnough {
				return model, row, checked
			}
		}
	}
	return "", cloudprovider.CapacitySpec{}, checked
}

func testCompsharePreflight(t *testing.T, first, second *cloudTestEnv) error {
	body, _ := json.Marshal(map[string]any{
		"node_uuids": []string{first.NodeUUID, second.NodeUUID}, "port": 29500,
		"master_addr": first.Instance.PrivateIP, "bandwidth": true, "seconds": 2,
	})
	resp, err := first.Control.API(t).PostJSON("/api/v1/deployments/preflight", body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("distributed preflight status=%d body=%s", resp.StatusCode, data)
	}
	var result struct {
		OK    bool `json:"ok"`
		Pairs []struct {
			SourceUUID      string  `json:"source_uuid"`
			TargetUUID      string  `json:"target_uuid"`
			Reachable       bool    `json:"reachable"`
			BandwidthTested bool    `json:"bandwidth_tested"`
			Gbps            float64 `json:"gbps"`
			Error           string  `json:"error"`
			BandwidthError  string  `json:"bandwidth_error"`
		} `json:"pairs"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return err
	}
	if !result.OK || len(result.Pairs) != 2 {
		return fmt.Errorf("distributed preflight failed: %s", data)
	}
	for _, pair := range result.Pairs {
		if !pair.Reachable || !pair.BandwidthTested || pair.Gbps <= 0 || pair.Error != "" || pair.BandwidthError != "" {
			return fmt.Errorf("distributed preflight pair failed: %+v", pair)
		}
		t.Logf("CompShare private network %s -> %s: %.3f Gbps", pair.SourceUUID, pair.TargetUUID, pair.Gbps)
	}
	return nil
}

// testCompshareDockerNCCL is the low-level runtime/network probe for the
// one-VM/two-logical-node product acceptance case. The caller already owns the
// VM and installed the pinned Docker/toolkit, so the probe's runner image cache
// is reused by the subsequent GPUWorks deployment.
func testDockerNCCL(t *testing.T, env *cloudTestEnv, image string) {
	t.Helper()
	const dockerCLI = "docker"
	if out, err := runCloudCommand(t.Context(), env.Instance, dockerCLI+" info --format '{{.ServerVersion}}'"); err != nil {
		t.Fatalf("docker daemon unavailable after toolkit install: %v\n%s", err, strings.TrimSpace(out))
	}

	network := "gpuworks-dist-" + strings.ToLower(hostedRunID(cloudTestRunID()))
	cleanup := fmt.Sprintf("%s rm -f %s-rank0 %s-rank1 >/dev/null 2>&1 || true; %s network rm %s >/dev/null 2>&1 || true", dockerCLI, network, network, dockerCLI, network)
	cleanupProbe := func() { _, _ = runCloudCommand(context.Background(), env.Instance, cleanup) }
	defer cleanupProbe()
	t.Cleanup(cleanupProbe)
	setup := compshareDockerNCCLSetupCommand(dockerCLI, network, image)
	if out, err := runCloudCommand(t.Context(), env.Instance, setup); err != nil {
		t.Fatalf("require cached Docker rank image=%q: %v\n%s", image, err, strings.TrimSpace(out))
	}
	for rank := 0; rank < 2; rank++ {
		name := fmt.Sprintf("%s-rank%d", network, rank)
		cmd := fmt.Sprintf("%s run -d --name %s --hostname rank%d --network %s --network-alias rank%d --gpus device=%d --entrypoint sleep %s 900", dockerCLI, shellQuoteForTest(name), rank, shellQuoteForTest(network), rank, rank, shellQuoteForTest(image))
		if out, err := runCloudCommand(t.Context(), env.Instance, cmd); err != nil {
			t.Fatalf("start Docker rank %d: %v\n%s", rank, err, strings.TrimSpace(out))
		}
	}
	for rank := 0; rank < 2; rank++ {
		name := fmt.Sprintf("%s-rank%d", network, rank)
		out, err := runCloudCommand(t.Context(), env.Instance, fmt.Sprintf("%s exec %s python3 -c %s", dockerCLI, shellQuoteForTest(name), shellQuoteForTest("import torch; assert torch.cuda.is_available(); print(torch.cuda.get_device_name(0))")))
		if err != nil || strings.TrimSpace(out) == "" {
			t.Fatalf("Docker rank %d GPU check err=%v output=%q", rank, err, strings.TrimSpace(out))
		}
		t.Logf("Docker rank %d GPU: %s", rank, strings.TrimSpace(out))
	}
	script := "import os, torch, torch.distributed as dist\n" +
		"rank = int(os.environ['RANK'])\n" +
		"dist.init_process_group('nccl', init_method='tcp://rank0:29500', rank=rank, world_size=2)\n" +
		"x = torch.tensor([rank + 1], device='cuda')\n" +
		"dist.all_reduce(x)\n" +
		"assert int(x.item()) == 3, x\n" +
		"print('rank', rank, 'all_reduce', int(x.item()), flush=True)\n" +
		"dist.destroy_process_group()\n"
	encoded := base64.StdEncoding.EncodeToString([]byte(script))
	var rank1Output string
	for rank := 0; rank < 2; rank++ {
		name := fmt.Sprintf("%s-rank%d", network, rank)
		cmd := fmt.Sprintf("timeout 180 %s exec -e RANK=%d %s sh -c 'echo %s | base64 -d > /tmp/gpuworks-dist.py && NCCL_DEBUG=INFO NCCL_IB_DISABLE=1 NCCL_SOCKET_IFNAME=eth0 python3 /tmp/gpuworks-dist.py'", dockerCLI, rank, shellQuoteForTest(name), encoded)
		if rank == 0 {
			cmd = "nohup sh -c " + shellQuoteForTest(cmd) + " >/tmp/gpuworks-rank0.log 2>&1 </dev/null &"
		}
		out, err := runCloudCommand(t.Context(), env.Instance, cmd)
		if rank == 1 {
			rank1Output = out
		}
		if err != nil {
			t.Fatalf("start NCCL rank %d: %v\n%s", rank, err, strings.TrimSpace(out))
		}
	}
	out, err := runCloudCommand(t.Context(), env.Instance, "sleep 5; cat /tmp/gpuworks-rank0.log")
	if err != nil {
		t.Fatalf("NCCL rank exchange failed: %v\n%s", err, strings.TrimSpace(out))
	}
	combined := out + "\n" + rank1Output
	if !strings.Contains(combined, "all_reduce 3") {
		t.Fatalf("NCCL rank exchange produced no all_reduce evidence: %s", strings.TrimSpace(combined))
	}
	t.Logf("Docker NCCL rank exchange: %s", strings.TrimSpace(combined))
}

func compshareDockerNCCLSetupCommand(dockerCLI, network, image string) string {
	return fmt.Sprintf("set -eu; %s image inspect %s >/dev/null; %s network create %s >/dev/null",
		dockerCLI, shellQuoteForTest(image), dockerCLI, shellQuoteForTest(network))
}

func waitCloudNodeByName(t *testing.T, env *testenv.Env, name string) string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		if nodes, err := env.Store().ListNodes(); err == nil {
			for _, node := range nodes {
				if node.Name == name {
					return node.UUID
				}
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatal("cloud node was not created")
	return ""
}
