//go:build cloudgpu && (compshare || aliyun)

package l4

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/imagecatalog/pullref"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/testenv/cloudprovider"
)

// vmBackend is what one VM provider contributes to the scenarios the
// providers share (single-VM tensor parallel + dGPU, one-VM two-logical-node
// and two-VM Docker distributed inference): the rented machines, how a card
// shape is picked, and where the Docker image and the model come from. The
// scenario bodies are provider-neutral and take a backend, so a new provider
// adds a constructor here and thin Test functions, not a copy of the cases.
type vmBackend struct {
	// Name labels logs and served model names ("compshare", "aliyun").
	Name     string
	Provider cloudprovider.CloudProvider
	// EachShape calls try for each card type and GPU count with at least cards
	// GPUs that looks rentable, until try returns true (hosted selects the
	// shapes the multi-VM control-plane cases use). It returns why every shape
	// was passed over when none worked.
	EachShape func(t *testing.T, hosted bool, cards, minVRAMMB int, try func(model string, count int) bool) []string
	// DiskGB is the system disk of every VM.
	DiskGB int
	// ModelEndpoint is an optional model mirror site rule.
	ModelEndpoint string
	// Image is the vLLM runner single-VM cases deploy; DistributedImage the one
	// multi-node cases do.
	Image, DistributedImage string
	// Model is the small model every deployment case serves: the one the
	// provider's VMs carry at PreloadedModelPath.
	Model l4TestModel
	// Engine is the vLLM image and extra engine arguments a card type runs, and
	// false for a card no image of ours can run (CompShare: V100S takes the
	// 1Cat-vLLM Volta image, P40 is skipped). EnsureEngineImage makes that image
	// present on the VM before the expensive part of a single-VM case.
	Engine            func(gpuModel string) (image, extraArgs string, ok bool)
	EnsureEngineImage func(t *testing.T, env *cloudTestEnv, gpuModel string)
	// HostedRoot is the directory the hosted control plane and agents keep their
	// state in; empty means /tmp (a non-root login cannot write elsewhere).
	HostedRoot string
	// UdockerDiskGB is the system disk of the VMs of the udocker case, which
	// keeps the runner image twice (layers, then an unpacked container: 30+ GiB
	// each way) on the VM itself; zero means DiskGB. UdockerOnDemand rents that
	// case on-demand because it runs over an hour, long enough that a spot
	// reclaim is likely.
	UdockerDiskGB   int
	UdockerOnDemand bool
	// UdockerImageFromURL gives the udocker nodes the runner image from the
	// VM's own Docker store instead of a registry pull: each VM saves it as an
	// archive, serves it on its private address, and the platform imports it to
	// the node from that URL (see l4_hosted_image_archive_test.go). For
	// providers whose golden image carries the image in Docker.
	UdockerImageFromURL bool
	// DockerTestImage is the image the Docker NCCL probe starts; empty means
	// DistributedImage.
	DockerTestImage string
	// PreloadedModelPath resolves the model directory present on every VM, or
	// skips/fails the case when the provider has none configured.
	PreloadedModelPath func(t *testing.T) string
	// EnsureDocker makes Docker 29 with the NVIDIA runtime usable on every VM
	// (CompShare installs it; Aliyun's golden image already carries it, so the
	// backend only verifies). EnsureImage makes the runner image present
	// without a registry pull.
	EnsureDocker func(t *testing.T, envs []*cloudTestEnv)
	EnsureImage  func(t *testing.T, envs []*cloudTestEnv)
}

// withEngine fills the engine, image and extra arguments a card type needs
// into a deployment body.
func (b *vmBackend) withEngine(body map[string]any, gpuModel string) map[string]any {
	image, args, ok := b.Engine(gpuModel)
	if !ok {
		panic(fmt.Sprintf("no engine image runs on %s %s", b.Name, gpuModel))
	}
	body["engine"] = "vllm"
	body["image"] = image
	if args != "" {
		body["extra_args"] = args
	}
	return body
}

func (b *vmBackend) hostedRoot() string {
	if b.HostedRoot != "" {
		return strings.TrimRight(b.HostedRoot, "/")
	}
	return "/tmp"
}

func (b *vmBackend) dockerTestImage() string {
	if b.DockerTestImage != "" {
		return b.DockerTestImage
	}
	return b.DistributedImage
}

// servedName builds a model name unique to the provider and scenario.
func (b *vmBackend) servedName(scenario string) string {
	return b.Model.ServedPrefix + "-" + b.Name + "-" + scenario
}

// l4TestModel is a model's registration: where it comes from and the
// metadata the estimator reads, as config.json of the repository states it.
type l4TestModel struct {
	SourceID, Name, Arch, Meta string
	WeightBytes                int64
	// ServedPrefix starts the served names of the deployments.
	ServedPrefix string
}

var (
	// l4Qwen3Model is the model the Aliyun and CompShare golden images carry
	// (goldenimage.DefaultModelRepo).
	l4Qwen3Model = l4TestModel{
		SourceID: "Qwen/Qwen3-0.6B", Name: "Qwen3-0.6B", Arch: "qwen3", WeightBytes: 3 << 29, ServedPrefix: "qwen3",
		Meta: `{"model_type":"qwen3","num_hidden_layers":28,"hidden_size":1024,"num_attention_heads":16,"num_key_value_heads":8,"head_dim":128,"max_position_embeddings":40960}`,
	}
)

// registerModel stores the model the backend's VMs carry.
func (b *vmBackend) registerModel(t *testing.T, env *cloudTestEnv) *store.Model {
	t.Helper()
	model, err := env.Control.Store().UpsertModel(&store.Model{
		Source: "modelscope", SourceID: b.Model.SourceID, Revision: "master",
		Name: b.Model.Name, Arch: b.Model.Arch, WeightBytes: b.Model.WeightBytes, Dtype: "bfloat16", Meta: b.Model.Meta,
	})
	if err != nil {
		t.Fatal(err)
	}
	return model
}

// compshareAcceptanceDiskGB: the externally maintained golden image currently
// requires a system disk larger than 100 GiB. Every CompShare shape stays
// compatible with both that image and the ordinary fallback image.
const compshareAcceptanceDiskGB = 160

func compshareBackend(t *testing.T) *vmBackend {
	t.Helper()
	p := compshareProvider(t)
	return &vmBackend{
		Name: "compshare", Provider: p, DiskGB: 160,
		EachShape: func(t *testing.T, hosted bool, cards, minVRAMMB int, try func(string, int) bool) []string {
			// Single-VM cases start an engine on the card, so only types an engine
			// image exists for (see compshareEngineImage).
			models := compshareRunnableGPUOrder()
			if hosted {
				models = hostedCompshareGPUModels
			}
			return eachCompshareShape(t, p, models, cards, minVRAMMB, 160, try)
		},
		ModelEndpoint: compshareModelEndpoint(),
		Image:         compshareTestImage(), DistributedImage: compshareDistributedTestImage(),
		Model:           l4Qwen3Model,
		DockerTestImage: strings.TrimSpace(os.Getenv("COMPSHARE_DOCKER_TEST_IMAGE")),
		PreloadedModelPath: func(t *testing.T) string {
			path := strings.TrimSpace(os.Getenv("COMPSHARE_PRELOADED_MODEL_PATH"))
			if path == "" {
				skipOrFailExternal(t, "COMPSHARE_PRELOADED_MODEL_PATH is required for CompShare deployment cases; use TestCompshareModelScopePublicDownload for public download acceptance")
			}
			return path
		},
		Engine: func(gpuModel string) (string, string, bool) {
			image, ok := compshareEngineImage(gpuModel)
			return image, compshareEngineExtraArgs(gpuModel), ok
		},
		EnsureEngineImage: func(t *testing.T, env *cloudTestEnv, gpuModel string) { prepareCompshareEngineImage(t, env, gpuModel) },
		EnsureDocker:      installCompshareDocker29OnNodes,
		EnsureImage:       prepareCompshareDockerImageCache,
	}
}

// countGPUCards counts nvidia-smi rows naming the card. A provider's GPU type
// (CompShare's 4090_48G) is not what nvidia-smi prints (NVIDIA GeForce RTX
// 4090), so callers pass the display name and spacing differences are ignored.
func countGPUCards(nvidiaSMI, displayName string) int {
	count := 0
	for _, line := range strings.Split(nvidiaSMI, "\n") {
		name, _, _ := strings.Cut(line, ",")
		if strings.TrimSpace(name) != "" && compactGPUStringContains(name, displayName) {
			count++
		}
	}
	return count
}

func compactGPUStringContains(output, model string) bool {
	compact := func(value string) string {
		return strings.ToUpper(strings.NewReplacer(" ", "", "-", "", "_", "", "\n", "", "\r", "").Replace(value))
	}
	return strings.Contains(compact(output), compact(model))
}

// logRentedInstance records where and how a provider that chooses its own
// placement (Aliyun: zone, instance type, spot or on-demand) bought the VM,
// with the refusals on the way, so a fallback is visible in the job log.
func logRentedInstance(t *testing.T, backend *vmBackend, instance *cloudprovider.Instance) {
	t.Helper()
	t.Logf("%s VM %s: gpu=%s x%d region=%s zone=%s type=%s billing=%s", backend.Name, instance.InstanceID, instance.GPUModel, instance.GPUCount, instance.Region, instance.Zone, instance.InstanceType, instance.Billing)
	if attempts, ok := backend.Provider.(interface{ Attempts() []string }); ok {
		for _, line := range attempts.Attempts() {
			t.Logf("%s rental attempt: %s", backend.Name, line)
		}
	}
}

// requireInstancesRunImage asserts that every instance of a deployment was
// launched with the engine image the case chose for its card type, so a
// fallback to a different image cannot pass as the Volta run.
func requireInstancesRunImage(t *testing.T, instances []store.Instance, want string) {
	t.Helper()
	if len(instances) == 0 {
		t.Fatal("deployment has no instances to check the engine image of")
	}
	for _, instance := range instances {
		var spec proto.InstanceDesiredState
		if err := json.Unmarshal([]byte(instance.DesiredStateJSON), &spec); err != nil {
			t.Fatalf("instance %s desired state: %v", instance.UUID, err)
		}
		if !pullref.SameRef(spec.Image, want) {
			t.Fatalf("instance %s runs image %q, want %q", instance.UUID, spec.Image, want)
		}
	}
}

// gpuWarmupCommand starts the engine image on every GPU of the VM the way the
// agent does (--gpus "device=N", CUDA_VISIBLE_DEVICES=0) and creates a CUDA
// context in it. A fresh VM's first container is sometimes started before the
// GPU stack serves containers: on Aliyun 2026-10-27 the first vLLM start on a
// new A10 VM died inside torch._C._cuda_init() with "No CUDA GPUs are
// available" (and once with NVMLError_Unknown), and the platform's retry then
// started the identical container fine. Running this first, with retries,
// absorbs that instead of leaving it to a deployment's retry budget.
func gpuWarmupCommand(image string) string {
	return `set -u
n=$(nvidia-smi -L | wc -l)
[ "$n" -ge 1 ] || { echo "no GPU listed" >&2; exit 1; }
i=0
while [ "$i" -lt "$n" ]; do
  docker run --rm --gpus "\"device=$i\"" -e CUDA_VISIBLE_DEVICES=0 --entrypoint python3 ` + shellQuoteForTest(image) + ` -c 'import torch; assert torch.cuda.is_available(); torch.zeros(1, device="cuda"); print("GPUWORKS_GPU_READY")' 2>&1 | tail -3 | grep -q GPUWORKS_GPU_READY || exit 1
  i=$((i + 1))
done
echo "GPUWORKS_GPUS_READY $n"`
}

// waitGPUsReady runs the warm-up on every VM until it passes, within a few
// minutes, and logs how many tries the VM needed (the evidence for how often
// a cold VM's first container fails). A VM that never gets there fails the case.
func waitGPUsReady(t *testing.T, envs []*cloudTestEnv, image string) {
	t.Helper()
	for rank, env := range envs {
		var out string
		var err error
		tries := 0
		for deadline := time.Now().Add(4 * time.Minute); time.Now().Before(deadline); {
			tries++
			out, err = runCloudCommand(t.Context(), env.Instance, gpuWarmupCommand(image))
			if err == nil && strings.Contains(out, "GPUWORKS_GPUS_READY") {
				break
			}
			select {
			case <-t.Context().Done():
				t.Fatalf("rank %d GPU warm-up canceled: %v", rank, t.Context().Err())
			case <-time.After(10 * time.Second):
			}
		}
		if err != nil || !strings.Contains(out, "GPUWORKS_GPUS_READY") {
			t.Fatalf("rank %d: containers cannot use the GPUs after %d tries: %v\n%s", rank, tries, err, strings.TrimSpace(out))
		}
		t.Logf("rank %d GPUs ready for containers after %d warm-up tr%s", rank, tries, map[bool]string{true: "y", false: "ies"}[tries == 1])
	}
}
