package seed

import (
	"context"
	"fmt"
	"reflect"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/chipmatch"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/placement"
	"gpuworks.ai/gpuworks/pkg/modelrepo"
)

const strataDeepSeekModelID = "ista-daslab/deepseek-v4.1-flash-1017-strata"
const strataDeepSeekSpecName = "iq2-xxs-strata-nvidia"

// The flat upstream repository also holds other quantizations. A directory
// selection at its root would fetch hundreds of GB this recipe cannot use.
func TestStrataDeepSeekAcquiresOnlyItsSixExternalInputs(t *testing.T) {
	model, spec := catalogSpec(t, strataDeepSeekModelID, strataDeepSeekSpecName)
	if len(model.Specs) != 1 || !reflect.DeepEqual(model.InputModalities, []string{"text"}) {
		t.Fatal("DeepSeek must expose only the text-only ISTA-DAS 2.00 bpw recipe")
	}
	if model.ContextLength != 1048576 || len(model.KVBytesPerToken) != 0 || spec.RuntimeWeightBytes != 0 {
		t.Fatal("native context must not manufacture a KV measurement or weight-based VRAM estimate")
	}
	const ista = "ISTA-DASLab/DeepSeek-V4.1-Flash-1017-GSQ-RCO-GGUF"
	const unsloth = "unsloth/DeepSeek-V4.1-Flash-1017-GGUF"
	const istaRevision = "cb02422c900df489cea5833e8a92171224059b3c"
	const unslothRevision = "8a6ec9cf118e785a362ea94ee123c61c70553be6"
	inputs := []struct {
		name, role, repo, revision, file, mount, env string
		bytes                                        int64
	}{
		{"target", "primary", ista, istaRevision, "DeepSeek-V4.1-Flash-1017-GSQ-RCO-IQ2_XXS-00001-of-00004.gguf", "/model", "", 39075714624},
		{"shard2", "auxiliary", ista, istaRevision, "DeepSeek-V4.1-Flash-1017-GSQ-RCO-IQ2_XXS-00002-of-00004.gguf", "/models/deepseek-shard2", "STRATA_DSV41_SHARD2_DIR", 39388449504},
		{"shard3", "auxiliary", ista, istaRevision, "DeepSeek-V4.1-Flash-1017-GSQ-RCO-IQ2_XXS-00003-of-00004.gguf", "/models/deepseek-shard3", "STRATA_DSV41_SHARD3_DIR", 39246891616},
		{"shard4", "auxiliary", ista, istaRevision, "DeepSeek-V4.1-Flash-1017-GSQ-RCO-IQ2_XXS-00004-of-00004.gguf", "/models/deepseek-shard4", "STRATA_DSV41_SHARD4_DIR", 20527837312},
		{"engram", "auxiliary", ista, istaRevision, "DeepSeek-V4.1-Flash-Engram-IQ4_NL.gguf", "/models/deepseek-engram", "STRATA_DSV41_ENGRAM_DIR", 110595290816},
		{"dspark", "auxiliary", unsloth, unslothRevision, "dspark-DeepSeek-V4.1-Flash-1017-Q8_0.gguf", "/models/deepseek-dspark", "STRATA_DSV41_DSPARK_DIR", 7976914240},
	}
	if len(spec.Components) != len(inputs) {
		t.Fatalf("components = %d, want %d", len(spec.Components), len(inputs))
	}
	files := []modelrepo.File{
		{Path: "DeepSeek-V4.1-Flash-1017-GSQ-RCO-IQ2_XS-00001-of-00004.gguf", Size: 39535776992},
		{Path: "DeepSeek-V4.1-Flash-1017-GSQ-RCO-IQ1_M-00001-of-00004.gguf", Size: 39358830464},
		{Path: "UD-Q8_K_XL/DeepSeek-V4.1-Flash-1017-UD-Q8_K_XL-00002-of-00009.gguf", Size: 50536121984},
	}
	for _, input := range inputs {
		files = append(files, modelrepo.File{Path: input.file, Size: input.bytes})
	}
	var total int64
	for index, input := range inputs {
		component := spec.Components[index]
		if component.Name != input.name || component.Role != input.role || component.MountPath != input.mount || component.WeightBytes != input.bytes {
			t.Fatalf("component %d has an unexpected identity, role, mount or size", index)
		}
		if len(component.Weights.Sources) != 1 {
			t.Fatalf("%s requires one verified source", input.name)
		}
		source := component.Weights.Sources[0]
		if source.Provider != "huggingface" || source.RepoID != input.repo || source.Revision != input.revision || source.Subfolder != input.file {
			t.Fatalf("%s does not select its exact pinned file", input.name)
		}
		selected := modelrepo.FilesUnderSubfolder(files, source.Subfolder)
		if len(selected) != 1 || selected[0].Path != input.file || selected[0].Size != input.bytes {
			t.Fatalf("%s selects files outside its component", input.name)
		}
		if input.env != "" && spec.Env[input.env] != input.mount {
			t.Fatalf("%s is not handed to the image entrypoint", input.name)
		}
		total += component.WeightBytes
	}
	if total != 256811098112 {
		t.Fatalf("external input bytes = %d, want 256811098112", total)
	}
	// The zero-tensor metadata GGUF is an image input, not an independently
	// registrable model or an acquisition component.
	if _, exists := spec.Env["STRATA_DSV41_ENGRAM_META_DIR"]; exists || len(spec.Env) != 5 {
		t.Fatal("metadata must come from the pinned image, without an external mount")
	}
	if spec.WeightBytes != inputs[0].bytes || !reflect.DeepEqual(spec.Weights, spec.Components[0].Weights) {
		t.Fatal("the spec's primary artifact must be shard one; planning adds auxiliaries")
	}
}

func TestStrataDeepSeekLaunchIsIndependentOfQwenSetup(t *testing.T) {
	_, spec := catalogSpec(t, strataDeepSeekModelID, strataDeepSeekSpecName)
	if spec.Engine != "callable" || spec.Image != "quay.io/gpuworks/gpuworks-runner:cuda13.0-strata0.1.59-deepseek41-amd64-r4" ||
		len(spec.ImageSources) != 0 || len(spec.Assets) != 0 {
		t.Fatal("DeepSeek requires its own candidate image and must not inherit Qwen mirrors or queue assets")
	}
	engine, err := deploy.SelectEngine(spec.Engine)
	if err != nil {
		t.Fatal(err)
	}
	launch, err := deploy.ResolveLaunch(deploy.ResolveLaunchInput{
		Engine: engine, Entrypoint: spec.Entrypoint, APIContract: spec.API,
		ComponentModelMount: "/model", StartTimeoutSec: 7200, DefaultStartTimeoutSec: 1800,
		Params: deploy.Params{ModelPath: "/model", APIPort: 30000, TemplateArgs: spec.Args, TemplateEnv: spec.Env},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(launch.Entrypoint, []string{"/opt/gpuworks/strata-deepseek/entrypoint.py"}) ||
		!reflect.DeepEqual(launch.Argv, []string{"--model", "/model", "--host", "0.0.0.0", "--port", "30000", "--context", "8192"}) {
		t.Fatalf("unexpected DeepSeek entrypoint or arguments: %v %v", launch.Entrypoint, launch.Argv)
	}
	if launch.StartTimeoutSeconds != 7200 || launch.EngineStartTimeoutSeconds != 0 {
		t.Fatal("the callable outer budget must preserve the deployment value independently of engine readiness")
	}
	for _, key := range []string{"CUDA_VISIBLE_DEVICES", "VLLM_ENGINE_READY_TIMEOUT_S"} {
		if _, exists := launch.Env[key]; exists {
			t.Fatalf("template must not set %s", key)
		}
	}
	if spec.API == nil || spec.API.TokenizerRef != "hf://deepseek-ai/DeepSeek-V4.1-Flash-1017@1bae0e1088993b9b82de08f59de5be5a34e42170" {
		t.Fatal("the GGUF recipe requires a pinned independent tokenizer source")
	}
	if spec.API.Metrics == nil || spec.API.Metrics.Endpoint != "/metrics/prometheus" || spec.API.Metrics.Format != "" {
		t.Fatal("DeepSeek must use the native Prometheus endpoint")
	}
	for _, target := range spec.API.Metrics.Map {
		if target.To == "requests_success_total" {
			t.Fatal("finished engine requests include cancellation and cannot declare success")
		}
	}
	if len(spec.API.RequestStats) != 2 || spec.API.RequestStats["timings.prompt_ms"].To != "prefill_seconds" ||
		spec.API.RequestStats["timings.predicted_ms"].To != "decode_seconds" ||
		spec.API.RequestStats["timings.prompt_ms"].Scale != 0.001 || spec.API.RequestStats["timings.predicted_ms"].Scale != 0.001 {
		t.Fatal("only native prefill/decode milliseconds may be mapped; queue time is unknown")
	}
}

func TestStrataDeepSeekSelectsOnlyItsCandidateCards(t *testing.T) {
	_, spec := catalogSpec(t, strataDeepSeekModelID, strataDeepSeekSpecName)
	predicate := chipmatch.Predicate{Vendor: spec.GPUs.Vendor, CC: spec.GPUs.CC, Name: spec.GPUs.Name}
	for _, tc := range []struct {
		name   string
		cc     float64
		accept bool
	}{
		{"Quadro RTX 8000", 7.5, true}, {"NVIDIA GeForce RTX 3080", 8.6, true},
		{"NVIDIA A100-SXM4-80GB", 8.0, false}, {"NVIDIA H100 80GB HBM3", 9.0, false},
		{"NVIDIA GeForce RTX 4090", 8.9, false}, {"NVIDIA GeForce RTX 30800", 8.6, false},
		{"NVIDIA GeForce RTX 3080 Ti", 8.6, false}, {"NVIDIA GeForce RTX 3080 Laptop GPU", 8.6, false},
	} {
		if got := chipmatch.Evaluate(chipmatch.GPU{Name: tc.name, Vendor: "nvidia", CC: tc.cc}, predicate); got.Matched != tc.accept {
			t.Errorf("%s cc=%v matches=%t, want %t (%s)", tc.name, tc.cc, got.Matched, tc.accept, got.Reason)
		}
	}
	for _, count := range []int{1, 2} {
		for _, memory := range []float64{11, 12, 20, 48} {
			t.Run(fmt.Sprintf("%dcards-%gGiB", count, memory), func(t *testing.T) {
				result, err := placement.Resolve(context.Background(), spec.Placement.Resolver, placement.GPU{
					Count: count, Vendor: "nvidia", MinMemoryGiB: memory,
				})
				if err != nil {
					t.Fatal(err)
				}
				if (result.Reason == "") != (memory >= 12) {
					t.Fatalf("memory admission: %s", result.Reason)
				}
				if memory >= 12 && (result.GPUMemoryMiBPerDevice != 12288 || len(result.Variables) != 0) {
					t.Fatal("admission must state the per-card floor without pinning GPU count or tensor parallelism")
				}
			})
		}
	}
}
