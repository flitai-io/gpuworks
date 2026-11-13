//go:build cloudgpu && aliyun

package l4

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/testenv"
	"gpuworks.ai/gpuworks/internal/testenv/cloudprovider"
	"gpuworks.ai/gpuworks/internal/testenv/goldenimage"
)

// Aliyun L4 cases rent ECS GPU VMs spot-first and on-demand when spot has no
// stock, from a golden image that already carries the NVIDIA driver, Docker
// 29 with the NVIDIA runtime, the vLLM runner image and the test model
// (test/l4/cmd/prepare-aliyun-golden). They install and download nothing.

const aliyunAcceptanceDiskGB = 160

func aliyunBackend(t *testing.T) *vmBackend {
	t.Helper()
	p := aliyunProvider(t)
	secrets := readCloudSecrets()
	// image runs on every card of the table (see goldenimage.TuringDockerImage):
	// the multi-node cases and the GPU warm-up use it whatever was rented.
	// ALIYUN_TEST_IMAGE replaces every image with one of the caller's.
	image, engineImage := goldenimage.TuringDockerImage, aliyunEngineImage
	if override := strings.TrimSpace(secrets["ALIYUN_TEST_IMAGE"]); override != "" {
		image, engineImage = override, func(string) string { return override }
	}
	modelEndpoint := strings.TrimSpace(secrets["ALIYUN_MODEL_ENDPOINT"])
	if modelEndpoint == "" {
		modelEndpoint = strings.TrimSpace(os.Getenv("GPUWORKS_MODEL_ENDPOINT"))
	}
	return &vmBackend{
		Name: "aliyun", Provider: p, DiskGB: aliyunAcceptanceDiskGB,
		EachShape: func(t *testing.T, _ bool, cards, minVRAMMB int, try func(string, int) bool) []string {
			return eachAliyunShape(t, p, cards, minVRAMMB, try)
		},
		ModelEndpoint: modelEndpoint, Image: image, DistributedImage: image, Model: l4Qwen3Model,
		// 300 GiB: the udocker case keeps the runner image four times on the VM
		// at once while it imports: the golden Docker store, the saved tar it
		// serves, the tar the node downloads, and udocker's own layers (the tar
		// copies are deleted as soon as they are loaded); the container it then
		// unpacks is a fifth. The 160 GiB disk had 3.1 GiB left after a pull.
		UdockerDiskGB: 300, UdockerOnDemand: true,
		// The golden image carries the runner in Docker only, and udocker has its
		// own store: the case saves it and imports it through the platform's
		// "import from URL" path instead of pulling ~30 GiB from a registry per
		// rank. No pull mirror is set, so an import that failed cannot be hidden
		// by a deployment that quietly pulls.
		UdockerImageFromURL: true,
		// The login is root: keep agent state off /tmp (see newHostedControlPlane).
		HostedRoot: "/opt",
		PreloadedModelPath: func(*testing.T) string {
			if path := strings.TrimSpace(secrets["ALIYUN_PRELOADED_MODEL_PATH"]); path != "" {
				return path
			}
			return goldenimage.DefaultModelPath
		},
		// The table holds only sm_75+ cards (no V100 shapes); single-VM cases run
		// the image the built-in templates give the rented card. A Volta shape
		// would take the 1Cat-vLLM image the golden image also carries.
		Engine: func(gpuModel string) (string, string, bool) { return engineImage(gpuModel), "", true },
		EnsureEngineImage: func(t *testing.T, env *cloudTestEnv, gpuModel string) {
			requireAliyunGoldenImage(t, []*cloudTestEnv{env}, engineImage(gpuModel))
			firstAliyunImageStart(t, env, engineImage(gpuModel))
		},
		EnsureDocker: func(t *testing.T, envs []*cloudTestEnv) {
			requireAliyunGoldenDocker(t, envs)
			waitGPUsReady(t, envs, image)
		},
		EnsureImage: func(t *testing.T, envs []*cloudTestEnv) { requireAliyunGoldenImage(t, envs, image) },
	}
}

// aliyunEngineImage is the vLLM runner a card type runs: the CUDA 13 build on
// compute capability 8.0 and later, the CUDA 12.9 build on Turing (T4), whose
// kernels the CUDA 13 build no longer carries.
func aliyunEngineImage(gpuModel string) string {
	if strings.EqualFold(strings.TrimSpace(gpuModel), "T4") {
		return goldenimage.TuringDockerImage
	}
	return goldenimage.DefaultDockerImage
}

// eachAliyunShape offers every card type that has stock for exactly cards GPUs
// until try accepts one: cards whose best placement is spot first, cheapest
// card first, then those that are on-demand only (spot-first across cards, not
// just within one). Stock is a snapshot: try returns false when the create is
// refused and the next card is offered. The adapter's own fallback handles spot
// being refused for a shape that still has on-demand stock.
func eachAliyunShape(t *testing.T, p *cloudprovider.Aliyun, cards, minVRAMMB int, try func(string, int) bool) []string {
	t.Helper()
	var checked []string
	type offer struct {
		model string
		best  cloudprovider.AliyunPlacement
	}
	var spot, onDemand []offer
	for _, model := range cloudprovider.AliyunGPUModels() {
		if vram, _ := cloudprovider.AliyunGPUVRAMMB(model); minVRAMMB > 0 && vram < minVRAMMB {
			checked = append(checked, fmt.Sprintf("%s: %d MiB per card, below the case's %d MiB", model, vram, minVRAMMB))
			continue
		}
		if cards >= 2 && !cloudprovider.AliyunTensorParallel(model, cards) {
			checked = append(checked, model+": its multi-GPU shape cannot run vLLM tensor parallelism (see aliyun_shapes.go)")
			continue
		}
		best, err := p.SelectPlacement(t.Context(), model, cards)
		if err != nil {
			checked = append(checked, model+": "+err.Error())
			continue
		}
		if best.Spot {
			spot = append(spot, offer{model, best})
		} else {
			onDemand = append(onDemand, offer{model, best})
		}
	}
	for _, o := range append(spot, onDemand...) {
		t.Logf("selected live Aliyun shape: %s x%d at %s (%s)", o.model, cards, o.best, o.best.Stock)
		if try(o.model, cards) {
			return nil
		}
		checked = append(checked, fmt.Sprintf("%s x%d: could not be created", o.model, cards))
	}
	return checked
}

// requireAliyunGoldenDocker checks, on a fresh login like the hosted agent's,
// that the golden image's Docker is the pinned series and has the NVIDIA
// runtime. A miss is a broken golden image, not a missing capability.
func requireAliyunGoldenDocker(t *testing.T, envs []*cloudTestEnv) {
	t.Helper()
	for rank, env := range envs {
		out, err := runCloudCommand(t.Context(), env.Instance,
			`set -eu; docker version --format '{{.Server.Version}}'; docker info --format '{{json .Runtimes}}' | grep -q nvidia; printf 'GPUWORKS_DOCKER_AGENT_ACCESS ok\n'`)
		if err != nil || !strings.Contains(out, "GPUWORKS_DOCKER_AGENT_ACCESS ok") || !strings.HasPrefix(strings.TrimSpace(out), "29.") {
			t.Fatalf("rank %d golden image lacks a working Docker 29 with the NVIDIA runtime (rebuild it with prepare-aliyun-golden): %v\n%s", rank, err, strings.TrimSpace(out))
		}
		t.Logf("rank %d Docker %s with NVIDIA runtime (golden image)", rank, strings.SplitN(strings.TrimSpace(out), "\n", 2)[0])
	}
}

// requireAliyunGoldenImage fails when the runner image is not already in the
// VM's Docker store: cases never pull.
func requireAliyunGoldenImage(t *testing.T, envs []*cloudTestEnv, image string) {
	t.Helper()
	for rank, env := range envs {
		if out, err := runCloudCommand(t.Context(), env.Instance, "docker image inspect "+shellQuoteForTest(image)+" >/dev/null"); err != nil {
			t.Fatalf("rank %d golden image does not carry %s (rebuild it with prepare-aliyun-golden): %v\n%s", rank, image, err, strings.TrimSpace(out))
		}
	}
}

// firstAliyunImageStart imports vLLM once in a container of image and logs how
// long it took. It is the first read of the image's files on a VM just
// created from the golden image; done here, that time is reported on its own
// instead of being part of a deployment's wait for the engine.
func firstAliyunImageStart(t *testing.T, env *cloudTestEnv, image string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Minute)
	defer cancel()
	started := time.Now()
	out, err := runCloudCommand(ctx, env.Instance, "docker run --rm --entrypoint python3 "+shellQuoteForTest(image)+" -c 'import vllm' 2>&1 | tail -n 5")
	if err != nil {
		t.Fatalf("first start of %s: %v\n%s", image, err, strings.TrimSpace(out))
	}
	t.Logf("first start of %s (import vllm) took %s", image, time.Since(started).Round(time.Second))
}

// aliyunOneGPUModel returns a card with a rentable single-GPU shape.
func aliyunOneGPUModel(t *testing.T, b *vmBackend) string {
	t.Helper()
	var model string
	checked := b.EachShape(t, false, 1, 0, func(m string, _ int) bool {
		model = m
		return true
	})
	if model == "" {
		skipOrFailExternal(t, "no Aliyun single-GPU VM could be rented: %s", strings.Join(checked, "; "))
	}
	return model
}

// TestAliyunDirectServerDialOnboarding proves a live one-GPU Aliyun VM can be
// rented (spot first, on-demand when spot has no stock), connected and observed
// accurately with the server dialing the VM's public address (server_dial). The
// agent is installed through the product's Aliyun provider driver, which is how
// an operator onboards an ECS instance with a public IP: no tunnel back to the
// control plane.
func TestAliyunDirectServerDialOnboarding(t *testing.T) {
	b := aliyunBackend(t)
	model := aliyunOneGPUModel(t, b)
	spec := cloudprovider.GPUSpec{GPUModel: model, GPUCount: 1, DiskGB: b.DiskGB}
	env := setupCloudNodeWithInstance(t, testenv.New(t), b.Provider, spec, nil, withServerDial())
	logRentedInstance(t, b, env.Instance)
	requireOnlineNode(t, env)
	node, err := env.Control.Store().NodeWithGPUs(env.NodeUUID)
	if err != nil {
		t.Fatal(err)
	}
	if node.ConnectMode != "server_dial" || node.ServerDialAddress == "" {
		t.Fatalf("aliyun node mode=%q server_dial_address=%q, want a server-dialed node", node.ConnectMode, node.ServerDialAddress)
	}
	if !strings.HasPrefix(node.ServerDialAddress, env.Instance.PublicIP+":") {
		t.Fatalf("aliyun server_dial address %q is not the VM's public address %s", node.ServerDialAddress, env.Instance.PublicIP)
	}
	if env.Instance.Kind != "vm" {
		t.Fatalf("aliyun product kind=%q, want vm", env.Instance.Kind)
	}
	if node.RuntimeDriver != "docker" {
		t.Fatalf("aliyun runtime driver = %q, want docker (the golden image carries it)", node.RuntimeDriver)
	}
	if len(node.GPUs) != 1 || node.GPUs[0].MemoryTotal <= 0 {
		t.Fatalf("aliyun GPU inventory = %+v", node.GPUs)
	}
	out, err := runCloudCommand(t.Context(), env.Instance, "nvidia-smi --query-gpu=name,memory.total --format=csv,noheader,nounits 2>&1 || true")
	if err != nil || !compactGPUStringContains(out, model) {
		t.Fatalf("aliyun SSH GPU check for %s output=%q err=%v", model, strings.TrimSpace(out), err)
	}
	t.Logf("aliyun node online: %s x1 billing=%s zone=%s type=%s", model, env.Instance.Billing, env.Instance.Zone, env.Instance.InstanceType)
}

// TestAliyunNodeOnboarding proves the same through the common agent_dial path:
// the agent dials the control plane through an SSH remote forward and is
// installed by Cloud Assistant (the instance's own agent), without any direct
// route from the server to the VM.
func TestAliyunNodeOnboarding(t *testing.T) {
	b := aliyunBackend(t)
	model := aliyunOneGPUModel(t, b)
	spec := cloudprovider.GPUSpec{GPUModel: model, GPUCount: 1, DiskGB: b.DiskGB}
	env := setupCloudNodeWithInstance(t, testenv.New(t), b.Provider, spec, nil)
	logRentedInstance(t, b, env.Instance)
	requireOnlineNode(t, env)
	node, err := env.Control.Store().NodeWithGPUs(env.NodeUUID)
	if err != nil {
		t.Fatal(err)
	}
	if node.ConnectMode != "agent_dial" || node.ServerDialAddress != "" {
		t.Fatalf("aliyun agent_dial node mode=%q server_dial_address=%q, want agent_dial with no server_dial address", node.ConnectMode, node.ServerDialAddress)
	}
	if env.Instance.Kind != "vm" || node.RuntimeDriver != "docker" {
		t.Fatalf("aliyun product kind=%q runtime=%q, want vm with docker", env.Instance.Kind, node.RuntimeDriver)
	}
	if len(node.GPUs) != 1 || node.GPUs[0].MemoryTotal <= 0 {
		t.Fatalf("aliyun GPU inventory = %+v", node.GPUs)
	}
	out, err := runCloudCommand(t.Context(), env.Instance, "nvidia-smi --query-gpu=name,memory.total --format=csv,noheader,nounits 2>&1 || true")
	if err != nil || !compactGPUStringContains(out, model) {
		t.Fatalf("aliyun SSH GPU check for %s output=%q err=%v", model, strings.TrimSpace(out), err)
	}
}

// TestAliyunCapacityShapes is allocation-free. It asks the stock API about
// every shape the cases may rent and logs the spot and on-demand placements, so
// a live case never guesses at inventory. Having no stock is not a failure;
// a stock query that errors is.
func TestAliyunCapacityShapes(t *testing.T) {
	p := aliyunProvider(t)
	available := 0
	for _, count := range []int{1, 2} {
		for _, model := range cloudprovider.AliyunGPUModels() {
			if len(cloudprovider.AliyunShapes(model, count)) == 0 {
				continue
			}
			best, err := p.SelectPlacement(t.Context(), model, count)
			switch {
			case err == nil:
				available++
				t.Logf("%s x%d: best placement %s (%s)", model, count, best, best.Stock)
			case strings.Contains(err.Error(), "stock query errors"):
				t.Fatalf("%s x%d: %v", model, count, err)
			default:
				t.Logf("%s x%d: %v", model, count, err)
			}
		}
	}
	if available == 0 {
		skipOrFailExternal(t, "no Aliyun shape has stock in any golden-image region right now")
	}
}

// TestAliyunTwoGPUProvisioning verifies the GPU count of a two-card shape is
// honored by the actual VM, rather than only checking a one-card smoke.
func TestAliyunTwoGPUProvisioning(t *testing.T) {
	b := aliyunBackend(t)
	ctx, cancel := context.WithTimeout(t.Context(), cloudSetupTimeout)
	defer cancel()
	spec := cloudprovider.GPUSpec{GPUCount: 2, DiskGB: b.DiskGB}
	rented, err := b.Provider.Rent(ctx, spec)
	if err != nil {
		if isCloudCapacityError(err) {
			skipOrFailExternal(t, "no Aliyun two-GPU VM could be rented right now: %v", err)
		}
		t.Fatal(err)
	}
	spec.GPUModel = rented.GPUModel
	env := setupCloudNodeWithInstance(t, testenv.New(t), b.Provider, spec, rented)
	logRentedInstance(t, b, env.Instance)
	requireOnlineNode(t, env)
	node, err := env.Control.Store().NodeWithGPUs(env.NodeUUID)
	if err != nil {
		t.Fatal(err)
	}
	if len(node.GPUs) != 2 {
		t.Fatalf("aliyun %s x2 inventory has %d GPUs: %+v", rented.GPUModel, len(node.GPUs), node.GPUs)
	}
	out, err := runCloudCommand(t.Context(), env.Instance, "nvidia-smi --query-gpu=name,memory.total --format=csv,noheader,nounits 2>&1 || true")
	if err != nil || countGPUCards(out, rented.GPUModel) != 2 {
		t.Fatalf("aliyun %s x2 SSH check output=%q err=%v", rented.GPUModel, strings.TrimSpace(out), err)
	}
}

// TestAliyunModelScopePublicDownload is the single low-frequency external
// download acceptance: the VM acquires one published 8 MiB shard from
// ModelScope over its public egress. No inference image is started.
func TestAliyunModelScopePublicDownload(t *testing.T) {
	b := aliyunBackend(t)
	model := aliyunOneGPUModel(t, b)
	env := setupCloudTest(t, b.Provider, cloudprovider.GPUSpec{GPUModel: model, GPUCount: 1, DiskGB: b.DiskGB})
	logRentedInstance(t, b, env.Instance)
	requireOnlineNode(t, env)
	verifyCloudPublicShard(t, env, "modelscope", "zstack/qwen3-4b-fake", "main")
}

// TestAliyunSingleVMTensorParallelAndDGPU reuses one VM with two GPUs and the
// preloaded image and model across tensor parallelism and dGPU sharing.
func TestAliyunSingleVMTensorParallelAndDGPU(t *testing.T) {
	runSingleVMTensorParallelAndDGPU(t, aliyunBackend(t))
}

// TestAliyunOneVMTwoLogicalNodeDistributedInference runs two gpuworks agents
// on one two-GPU Aliyun VM, one card each, and serves nnodes=2 through the
// gateway.
func TestAliyunOneVMTwoLogicalNodeDistributedInference(t *testing.T) {
	runOneVMTwoLogicalNodes(t, aliyunBackend(t))
}

// TestAliyunTwoVMDockerDistributedInference runs a two-rank, four-GPU vLLM
// deployment across two Aliyun VMs in one zone, with the control plane on VM1
// and both agents on the VPC's private network.
func TestAliyunTwoVMDockerDistributedInference(t *testing.T) {
	runTwoVMDockerDistributed(t, aliyunBackend(t))
}

// TestAliyunPDDeployChatTwoVMDocker is the cross-node Ethernet PD contract on
// two Aliyun VMs in one zone: prefill on VM1, decode on VM2, KV over the VPC.
func TestAliyunPDDeployChatTwoVMDocker(t *testing.T) {
	runPDDeployChatTwoVMDocker(t, aliyunBackend(t))
}

// TestAliyunTwoVMUdockerDistributedInference is the udocker-runtime variant of
// the two-VM distributed case; each VM imports the runner image through its
// own udocker store.
func TestAliyunTwoVMUdockerDistributedInference(t *testing.T) {
	runTwoVMUdockerDistributed(t, aliyunBackend(t))
}
