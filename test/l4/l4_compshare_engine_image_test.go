//go:build cloudgpu && (compshare || aliyun)

package l4

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// compshareV100DefaultImage is the Volta build of vLLM (1Cat-vLLM, matrix id
// cuda12.8-v100-vllm1cat1.5.1 in build/runner-images/matrix.yaml), by its
// SWR copy: the image is not in the system catalog, so nothing rewrites a
// docker.io reference for the mainland network, and the VM pulls from the
// registry its provider reaches best. Upstream vLLM's images carry no sm_70
// kernels and die with "no kernel image is available for execution on the
// device" on a V100S, which is why this case cannot use compshareTestImage()
// there.
const compshareV100DefaultImage = "swr.cn-southwest-2.myhuaweicloud.com/gpuworks/gpuworks-runner:cuda12.8-v100-vllm1cat1.5.1-r1"

// compshareV100AttentionArgs selects 1Cat-vLLM's Volta attention backend
// explicitly. Its documented container launch passes it, and the engine
// should not depend on the backend being auto-picked on a rented VM.
const compshareV100AttentionArgs = "--attention-backend FLASH_ATTN_V100"

// compshareV100TestImage is the engine image for Volta cards;
// COMPSHARE_V100_TEST_IMAGE overrides it (a private copy, a newer revision).
func compshareV100TestImage() string {
	if image := strings.TrimSpace(os.Getenv("COMPSHARE_V100_TEST_IMAGE")); image != "" {
		return image
	}
	return compshareV100DefaultImage
}

// compshareVolta reports whether a CompShare card type is a Volta part
// (compute capability 7.0): V100 and V100S.
func compshareVolta(gpuModel string) bool {
	return strings.HasPrefix(strings.ToUpper(strings.TrimSpace(gpuModel)), "V100")
}

// compshareEngineImage is the vLLM image a card type can run, and false for a
// card no image of ours can run. Pascal (P40, sm_61) is in that second group:
// neither upstream vLLM nor 1Cat-vLLM carries kernels for it.
func compshareEngineImage(gpuModel string) (string, bool) {
	switch {
	case compshareVolta(gpuModel):
		return compshareV100TestImage(), true
	case strings.EqualFold(strings.TrimSpace(gpuModel), "P40"):
		return "", false
	default:
		return compshareTestImage(), true
	}
}

// compshareEngineExtraArgs are the engine arguments a card type needs beyond
// the platform's own (the platform already pins fp16 below compute
// capability 8.0).
func compshareEngineExtraArgs(gpuModel string) string {
	if compshareVolta(gpuModel) {
		return compshareV100AttentionArgs
	}
	return ""
}

// compshareRunnableGPUOrder is compshareGPUOrder without the card types no
// engine image can run, for the cases that start an engine on a rented card.
func compshareRunnableGPUOrder() []string {
	var models []string
	for _, model := range compshareGPUOrder() {
		if _, ok := compshareEngineImage(model); ok {
			models = append(models, model)
		}
	}
	return models
}

// withCompshareEngine fills the image and engine arguments a card type needs
// into a deployment body.
func withCompshareEngine(body map[string]any, gpuModel string) map[string]any {
	image, ok := compshareEngineImage(gpuModel)
	if !ok {
		panic(fmt.Sprintf("no engine image runs on CompShare %s", gpuModel))
	}
	body["engine"] = "vllm"
	body["image"] = image
	if args := compshareEngineExtraArgs(gpuModel); args != "" {
		body["extra_args"] = args
	}
	return body
}

// prepareCompshareEngineImage makes the engine image present on the VM before
// the expensive part of the case. The generic image keeps the strict cache
// check (a golden image or COMPSHARE_DOCKER_IMAGE_TAR must supply it). The
// Volta image has no golden-image copy yet: a tar named by
// COMPSHARE_V100_DOCKER_IMAGE_TAR is loaded when the VM lacks the image, and
// without one the platform pulls it from the registry in the deployment's
// startup window.
func prepareCompshareEngineImage(t *testing.T, env *cloudTestEnv, gpuModel string) {
	t.Helper()
	if !compshareVolta(gpuModel) {
		prepareCompshareDockerImageCache(t, []*cloudTestEnv{env})
		return
	}
	prepareCompshareImageCache(t, []*cloudTestEnv{env}, compshareV100TestImage(), strings.TrimSpace(os.Getenv("COMPSHARE_V100_DOCKER_IMAGE_TAR")), false)
}

func TestCompshareEngineImageFollowsTheCardGeneration(t *testing.T) {
	t.Setenv("COMPSHARE_TEST_IMAGE", "")
	t.Setenv("COMPSHARE_V100_TEST_IMAGE", "")
	for _, volta := range []string{"V100S", "V100", " v100s "} {
		image, ok := compshareEngineImage(volta)
		if !ok || image != compshareV100DefaultImage || !strings.Contains(image, "v100-vllm1cat") {
			t.Errorf("%s: image=%q ok=%v, want the 1Cat-vLLM Volta image", volta, image, ok)
		}
		if got := compshareEngineExtraArgs(volta); got != compshareV100AttentionArgs {
			t.Errorf("%s: extra args=%q", volta, got)
		}
	}
	for _, other := range []string{"4090", "3090", "2080Ti", "5090", "A100"} {
		image, ok := compshareEngineImage(other)
		if !ok || image != compshareTestImage() || strings.Contains(image, "1cat") {
			t.Errorf("%s: image=%q ok=%v, want the generic vLLM image", other, image, ok)
		}
		if got := compshareEngineExtraArgs(other); got != "" {
			t.Errorf("%s: extra args=%q, want none", other, got)
		}
	}
	if image, ok := compshareEngineImage("P40"); ok || image != "" {
		t.Errorf("P40 (Pascal) has no engine image, got %q ok=%v", image, ok)
	}
}

func TestCompshareEngineImageEnvironmentOverrides(t *testing.T) {
	t.Setenv("COMPSHARE_TEST_IMAGE", "example.test/vllm:generic")
	t.Setenv("COMPSHARE_V100_TEST_IMAGE", "example.test/vllm:volta")
	if image, _ := compshareEngineImage("V100S"); image != "example.test/vllm:volta" {
		t.Errorf("V100S image=%q", image)
	}
	if image, _ := compshareEngineImage("4090"); image != "example.test/vllm:generic" {
		t.Errorf("4090 image=%q", image)
	}
	// The Volta override does not leak into the other cards, and the generic
	// one does not reach Volta.
	t.Setenv("COMPSHARE_V100_TEST_IMAGE", "")
	if image, _ := compshareEngineImage("V100S"); image != compshareV100DefaultImage {
		t.Errorf("V100S with only the generic override set: %q", image)
	}
}

// The runnable order keeps every other card in its place, drops the card no
// image runs on, and keeps V100S, which now has one.
func TestCompshareEngineRunnableGPUOrderDropsOnlyUnrunnableCards(t *testing.T) {
	t.Setenv("COMPSHARE_GPU_MODELS", "")
	got := strings.Join(compshareRunnableGPUOrder(), ",")
	want := "3090,4090,5090,4090_48G,2080Ti,V100S,A800,H20,A100"
	if got != want {
		t.Fatalf("runnable order=%s, want %s", got, want)
	}
}

func TestCompshareEngineDeploymentBodyCarriesTheCardsEngine(t *testing.T) {
	t.Setenv("COMPSHARE_TEST_IMAGE", "")
	t.Setenv("COMPSHARE_V100_TEST_IMAGE", "")
	volta := withCompshareEngine(map[string]any{"served_name": "x"}, "V100S")
	if volta["engine"] != "vllm" || volta["image"] != compshareV100DefaultImage || volta["extra_args"] != compshareV100AttentionArgs || volta["served_name"] != "x" {
		t.Fatalf("V100S body=%v", volta)
	}
	ampere := withCompshareEngine(map[string]any{}, "3090")
	if ampere["image"] != compshareTestImage() {
		t.Fatalf("3090 body=%v", ampere)
	}
	if _, has := ampere["extra_args"]; has {
		t.Fatalf("3090 body carries Volta engine arguments: %v", ampere)
	}
}

func TestCompshareEngineRequireInstancesRunImageComparesTheLaunchedImage(t *testing.T) {
	instance := func(image string) store.Instance {
		raw, err := json.Marshal(proto.InstanceDesiredState{Image: image})
		if err != nil {
			t.Fatal(err)
		}
		return store.Instance{UUID: "i-" + image, DesiredStateJSON: string(raw)}
	}
	// Matching, including the registry spelling the pull plan normalises.
	requireInstancesRunImage(t, []store.Instance{instance(compshareV100DefaultImage), instance(compshareV100DefaultImage)}, compshareV100DefaultImage)

	for name, instances := range map[string][]store.Instance{
		"another image":      {instance("gpuworks/gpuworks-runner:cuda13.0-vllm0.30.0-r4")},
		"one of two differs": {instance(compshareV100DefaultImage), instance("gpuworks/gpuworks-runner:cuda13.0-vllm0.30.0-r4")},
		"no instances":       nil,
		"unparseable state":  {{UUID: "bad", DesiredStateJSON: "{"}},
	} {
		inner := &testing.T{}
		failed := runCaptured(func() { requireInstancesRunImage(inner, instances, compshareV100DefaultImage) })
		if !failed {
			t.Errorf("%s: requireInstancesRunImage accepted the launch", name)
		}
	}
}

// runCaptured reports whether fn ended its goroutine through t.Fatal.
// requireInstancesRunImage calls t.Fatalf, which stops the calling goroutine
// with runtime.Goexit, so it has to run on a goroutine of its own.
func runCaptured(fn func()) (failed bool) {
	done := make(chan struct{})
	completed := false
	go func() {
		defer close(done)
		fn()
		completed = true
	}()
	<-done
	return !completed
}

func TestCompshareGPUModelsEnvironmentRestrictsTheOrder(t *testing.T) {
	t.Setenv("COMPSHARE_GPU_MODELS", " V100S, ,P40 ")
	if got := strings.Join(compshareGPUOrder(), ","); got != "V100S,P40" {
		t.Fatalf("order=%s", got)
	}
	// The runnable order still drops the card no engine image runs on.
	if got := strings.Join(compshareRunnableGPUOrder(), ","); got != "V100S" {
		t.Fatalf("runnable order=%s", got)
	}
}
