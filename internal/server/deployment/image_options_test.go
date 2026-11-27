package deployment

import (
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/imagecatalog"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// A template holds the options to its engine. Without one the other engines'
// images follow the target engine's and say which engine they select.
func TestDeploymentImageOptionsMarkAnotherEnginesImages(t *testing.T) {
	st, err := store.Open("file:deployment_image_options_engine_test?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	// The options come from the system catalog a started server has.
	if _, err := imagecatalog.SyncSeed(st); err != nil {
		t.Fatal(err)
	}
	h := New(Deps{Store: st})
	target := deploy.ImageTarget{
		Engine: "vllm", Vendor: "nvidia", Framework: "cuda", FrameworkVersion: "13.0", PlatformArch: "amd64",
	}

	withoutTemplate := h.deploymentImageOptions(target, templateSelection{}, "", nil)
	seenOther := false
	for _, option := range withoutTemplate {
		if option.Engine == "vllm-omni" {
			t.Fatalf("option %q is an image family no draft can switch to", option.Image)
		}
		own := option.Engine == "vllm"
		if own != (option.SelectsEngine == "") {
			t.Fatalf("option %q of engine %q selects %q", option.Image, option.Engine, option.SelectsEngine)
		}
		if !own && option.SelectsEngine != option.Engine {
			t.Fatalf("option %q of engine %q selects %q", option.Image, option.Engine, option.SelectsEngine)
		}
		if own && seenOther {
			t.Fatalf("option %q of the target engine follows another engine's: %+v", option.Image, withoutTemplate)
		}
		seenOther = seenOther || !own
	}
	if !hasImageOptionContaining(withoutTemplate, "vllm") || !hasImageOptionContaining(withoutTemplate, "sglang") {
		t.Fatalf("options without template did not include both engines: %+v", withoutTemplate)
	}

	withTemplate := h.deploymentImageOptions(target, templateSelection{Spec: &store.InferenceSpecRow{Engine: "vllm"}}, "", nil)
	if !hasImageOptionContaining(withTemplate, "vllm") {
		t.Fatalf("template options omitted its engine: %+v", withTemplate)
	}
	for _, option := range withTemplate {
		if strings.Contains(strings.ToLower(option.Image), "sglang") || option.SelectsEngine != "" {
			t.Fatalf("template options crossed the template engine: %+v", option)
		}
	}
}

func hasImageOptionContaining(options []dto.ImageOption, fragment string) bool {
	for _, option := range options {
		if strings.Contains(strings.ToLower(option.Image), fragment) {
			return true
		}
	}
	return false
}

// TestDeploymentImageTargetCarriesEveryNodeFact pins the contract of the one
// node-derived catalog query: every call site that resolves a runtime image
// for a concrete node goes through this constructor, so a fact it forgets to
// copy would silently change the automatic pick at that call site. The driver
// version once went missing exactly this way and made preview and create
// deploy different images.
func TestDeploymentImageTargetCarriesEveryNodeFact(t *testing.T) {
	node := &store.Node{
		Arch: "amd64", Framework: "cuda", FrameworkVersion: "13.2", GPUDriver: "595.99.02",
		GPUs: []store.GPU{{Name: "NVIDIA GeForce RTX 3080 Ti", Vendor: "nvidia", ArchID: "sm_86", ComputeCapability: "8.6"}},
	}
	target := deploymentImageTarget("vllm", "", node)
	if target.Engine != "vllm" || target.Vendor != "nvidia" ||
		target.PlatformArch != "amd64" || target.Framework != "cuda" ||
		target.FrameworkVersion != "13.2" || target.DriverVersion != "595.99.02" {
		t.Fatalf("target dropped a node fact: %+v", target)
	}
	if got := deploymentImageTarget("vllm", "nvidia", nil); got.Vendor != "nvidia" || got.DriverVersion != "" || got.PlatformArch != "" {
		t.Fatalf("vendor-only target = %+v", got)
	}
	node.GPUs = append(node.GPUs, store.GPU{Name: "Ascend 910B4", Vendor: "ascend", ArchID: "910B4"})
	selected := deploymentImageTargetForGPU("vllm", "", node, &node.GPUs[1])
	if selected.Vendor != "ascend" || selected.PlatformArch != node.Arch || selected.DriverVersion != node.GPUDriver {
		t.Fatalf("selected-profile target = %+v", selected)
	}
}
