package seed

import (
	"context"
	"regexp"
	"slices"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/inferenceapi"
	"gpuworks.ai/gpuworks/internal/server/inferencecatalog"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/placement"
)

var immutableCatalogRevision = regexp.MustCompile(`^[0-9a-f]{40,64}$`)

func TestEveryEmbeddedSpecHasImmutableInputsAndOfflinePreview(t *testing.T) {
	catalog, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}
	for _, model := range catalog.Models {
		for _, spec := range model.Specs {
			where := model.ModelID + "/" + spec.Name
			// A spec may not declare a plugin file it cannot supply. Catalog()
			// already enforces this through ValidateFlatCatalog, and this
			// asserts the property over the embedded catalog directly so the
			// regression that produced the nano_v3 defect - a spec that cannot
			// start under any image - fails here even if a future load path
			// stops validating.
			if flag, value, unresolved := inferencecatalog.UnresolvedFileValuedArgument(&spec); unresolved {
				t.Errorf("%s declares %s %s, which no image, asset, mount or start script of the spec supplies", where, flag, value)
			}
			for _, source := range spec.Weights.Sources {
				if !immutableCatalogRevision.MatchString(source.Revision) {
					t.Errorf("%s has mutable %s revision %q", where, source.Provider, source.Revision)
				}
			}
			contract, err := inferenceapi.EffectiveContract(spec.API, inferenceapi.DefaultsInput{Engine: spec.Engine})
			if err != nil {
				t.Errorf("%s API contract: %v", where, err)
				continue
			}
			for _, operation := range contract.Operations {
				definition, ok := inferenceapi.Lookup(operation.Type)
				if !ok || definition.Renderer == "" || len(definition.RequestEncodings) == 0 {
					t.Errorf("%s operation %q has no executable Playground definition", where, operation.Type)
				}
			}
			// The preview launches on a GPU count the resolver accepts, with the
			// degree it pins, as a deployment would: a command stating its own
			// data-parallel degree is only coherent on the count it was written for.
			acceptedCount, acceptedTP := 0, 0
			if spec.Placement != nil {
				accepted := false
			vectors:
				// 128 covers the Ascend 950PR class, whose profiles ask for
				// more than any 64 or 96 GiB card can offer.
				for _, memory := range []float64{64, 96, 128} {
					for _, count := range []int{1, 2, 4, 8, 16, 32} {
						memories := make([]float64, count)
						names := make([]string, count)
						for index := range memories {
							memories[index], names[index] = memory, spec.GPUs.Name
						}
						result, resolveErr := placement.Resolve(context.Background(), spec.Placement.Resolver, placement.GPU{Count: count, Vendor: spec.GPUs.Vendor, Names: names, MemoriesGiB: memories, MinMemoryGiB: memory})
						if resolveErr != nil {
							t.Errorf("%s placement test vector count=%d memory=%v: %v", where, count, memory, resolveErr)
							break vectors
						}
						if result.Reason == "" {
							accepted = true
							acceptedCount, acceptedTP = count, count
							if tp, ok := result.Variables[placement.VarTPSize].(int64); ok {
								acceptedTP = int(tp)
							}
							break vectors
						}
					}
				}
				if !accepted {
					t.Errorf("%s placement has no accepting release test vector", where)
				}
			}
			engine, err := deploy.SelectEngine(spec.Engine)
			if err != nil {
				t.Errorf("%s engine: %v", where, err)
				continue
			}
			params := deploy.Params{
				ModelPath: "/model", APIPort: 30000, TensorParallel: 1,
				TemplateArgs: spec.Args, TemplateEnv: spec.Env,
			}
			if acceptedCount > 0 {
				params.AssignedGPUs, params.TensorParallel = acceptedCount, acceptedTP
			}
			// A template that places the rendezvous address only launches
			// distributed (a single node has no MASTER_ADDR and is refused), so
			// it is previewed as rank 0 of two nodes.
			if slices.Contains(placement.References(strings.Join(spec.Args, " ")), placement.VarMasterAddr) {
				params.NNodes, params.MasterAddr, params.MasterPort, params.TensorParallel = 2, "10.0.0.1", 29500, 2
			}
			launch, err := deploy.ResolveLaunch(deploy.ResolveLaunchInput{
				Engine:          engine,
				Params:          params,
				Entrypoint:      spec.Entrypoint,
				PreStartArgv:    spec.PreStartArgv,
				PreStartScript:  spec.PreStartScript,
				APIContract:     contract,
				AllowUnresolved: spec.Placement != nil,
			})
			if err != nil {
				t.Errorf("%s offline preview: %v", where, err)
				continue
			}
			if len(launch.Entrypoint)+len(launch.Argv) == 0 {
				t.Errorf("%s produced an empty launch", where)
			}
			// A callable adapter parses its own command line, often strictly
			// (argparse.parse_args). Its argv is the template's arguments and
			// nothing else, and the port the gateway dials is among them.
			if spec.Engine == deploy.EngineCallable {
				if len(launch.Argv) != len(spec.Args) {
					t.Errorf("%s callable argv %v is not exactly its template args %v", where, launch.Argv, spec.Args)
				}
				if !slices.Contains(launch.Argv, "30000") {
					t.Errorf("%s callable argv %v never tells the adapter the platform port", where, launch.Argv)
				}
			}
		}
	}
}

func TestMiniMaxH3AscendProfileIsExecutableAndKeepsExactArtifactFacts(t *testing.T) {
	catalog, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, model := range catalog.Models {
		if model.ModelID != "minimaxai/minimax-h3" {
			continue
		}
		if model.ReadmeZHCN != "README.zh-CN.md" || model.ReadmeEN != "README.en.md" {
			t.Fatalf("H3 documentation=%q/%q", model.ReadmeZHCN, model.ReadmeEN)
		}
		for _, spec := range model.Specs {
			if spec.Name != "bf16-sglang-ascend-4x32g" {
				continue
			}
			found = true
			// The verified run used the ModelScope FL2VA directory; the Hugging
			// Face copy carries byte-identical weights (same LFS sha256).
			verified := false
			for _, source := range spec.Weights.Sources {
				if source.Subfolder != "FL2VA" {
					t.Fatalf("H3 source %+v is not the FL2VA partition", source)
				}
				verified = verified || (source.Provider == "modelscope" && source.Revision == "57559a6720ddaa2eaee52b09ec2ef011127997b2")
			}
			if spec.WeightBytes != 144051182625 || !verified {
				t.Fatalf("H3 artifact facts=%+v bytes=%d", spec.Weights, spec.WeightBytes)
			}
			if regexp.MustCompile(spec.GPUs.Name).MatchString("910C") {
				t.Fatalf("H3 910B4 profile must not claim 910C: %s", spec.GPUs.Name)
			}
			accepted, err := placement.Resolve(context.Background(), spec.Placement.Resolver, placement.GPU{Count: 4, Vendor: "ascend", Names: []string{"910B4", "910B4", "910B4", "910B4"}, MemoriesGiB: []float64{32, 32, 32, 32}, MinMemoryGiB: 32, Indexes: []int{0, 1, 2, 3}})
			if err != nil || accepted.Reason != "" || accepted.GPUMemoryMiBPerDevice != 29424 || accepted.Variables["TP_SIZE"] != int64(4) {
				t.Fatalf("H3 placement accepted=%+v err=%v", accepted, err)
			}
			for _, gpu := range []placement.GPU{{Count: 2, MinMemoryGiB: 32}, {Count: 4, MinMemoryGiB: 31}, {Count: 8, MinMemoryGiB: 32}} {
				rejected, err := placement.Resolve(context.Background(), spec.Placement.Resolver, gpu)
				if err != nil || rejected.Reason == "" {
					t.Fatalf("H3 unsupported topology=%+v result=%+v err=%v", gpu, rejected, err)
				}
			}
			engine, _ := deploy.SelectEngine(spec.Engine)
			launch, err := deploy.ResolveLaunch(deploy.ResolveLaunchInput{
				Engine: engine, Entrypoint: spec.Entrypoint,
				Params:             deploy.Params{ModelPath: "/model", APIPort: 30000, TensorParallel: 4, TemplateArgs: spec.Args, TemplateEnv: spec.Env},
				PlacementVariables: accepted.Variables,
				APIContract:        spec.API,
			})
			if err != nil {
				t.Fatal(err)
			}
			command := append(append([]string(nil), launch.Entrypoint...), launch.Argv...)
			for _, pair := range [][2]string{{"--model-variant", "fl2va"}, {"--num-gpus", "4"}, {"--tp-size", "4"}, {"--sp-degree", "1"}} {
				index := slices.Index(command, pair[0])
				if index < 0 || index+1 >= len(command) || command[index+1] != pair[1] {
					t.Fatalf("H3 launch lacks %s %s: %v", pair[0], pair[1], command)
				}
			}
			if !strings.Contains(string(launch.APIContract), `"endpoint":"/v1/videos"`) {
				t.Fatalf("H3 API contract=%s", launch.APIContract)
			}
		}
	}
	if !found {
		t.Fatal("MiniMax H3 Ascend profile is missing")
	}
}

func TestNVIDIASourceRemediationKeepsSemanticContractsAndExcludesForeignVariants(t *testing.T) {
	catalog, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}
	byModel := make(map[string]map[string]struct {
		quantization string
		cc           string
		args         []string
	})
	for _, model := range catalog.Models {
		byModel[model.ModelID] = make(map[string]struct {
			quantization string
			cc           string
			args         []string
		})
		for _, spec := range model.Specs {
			byModel[model.ModelID][spec.Name] = struct {
				quantization string
				cc           string
				args         []string
			}{spec.Weights.Quantization, spec.GPUs.CC, spec.Args}
		}
	}
	for _, forbidden := range [][2]string{
		{"minimaxai/minimax-m3", "mxfp4-vllm-nvidia-general"},
		{"moonshotai/kimi-k2.5", "mxfp4-vllm-nvidia-general"},
		{"qwen/qwen3.8-27b", "int8-vllm-nvidia-general"},
		{"thinkingmachines/inkling", "mxfp4-vllm-nvidia-general"},
		{"zai-org/glm-5.2", "mxfp4-vllm-nvidia-general"},
	} {
		if _, exists := byModel[forbidden[0]][forbidden[1]]; exists {
			t.Errorf("foreign hardware variant remains: %s/%s", forbidden[0], forbidden[1])
		}
	}
	for _, expected := range []struct{ model, spec, quantization, cc string }{
		{"deepseek-ai/deepseek-ocr-2", "fp16-sglang-nvidia-general", "fp16", ">=8.0"},
		{"deepseek-ai/deepseek-v4-flash", "fp4-sglang-nvidia-cc100", "fp4", ">=10.0"},
		{"deepseek-ai/deepseek-v4-pro", "fp4-sglang-nvidia-cc100", "fp4", ">=10.0"},
		{"minimaxai/minimax-m3", "mxfp8-sglang-nvidia-general", "mxfp8", ">=9.0"},
	} {
		actual, exists := byModel[expected.model][expected.spec]
		if !exists || actual.quantization != expected.quantization || actual.cc != expected.cc {
			t.Errorf("precision repair %s/%s=%+v exists=%v", expected.model, expected.spec, actual, exists)
		}
	}
	for _, specName := range []string{"fp8-fp8-vllm-nvidia-hopper", "nvfp4-vllm-nvidia-cc100"} {
		spec := byModel["deepseek-ai/deepseek-v4-pro"][specName]
		for _, pair := range [][2]string{{"--tokenizer-mode", "deepseek_v4"}, {"--tool-call-parser", "deepseek_v4"}, {"--reasoning-parser", "deepseek_v4"}} {
			if !containsArgumentPair(spec.args, pair[0], pair[1]) {
				t.Errorf("DeepSeek V4 Pro %s lacks %s %s: %v", specName, pair[0], pair[1], spec.args)
			}
		}
		if !slices.Contains(spec.args, "--enable-auto-tool-choice") {
			t.Errorf("DeepSeek V4 Pro %s lacks auto tool choice: %v", specName, spec.args)
		}
	}
	hopper := byModel["deepseek-ai/deepseek-v4-pro"]["fp8-fp8-vllm-nvidia-hopper"]
	for _, pair := range [][2]string{{"--kv-cache-dtype", "fp8"}, {"--block-size", "256"}, {"--no-enable-flashinfer-autotune", "--compilation-config"}, {"--compilation-config", `{"mode":0,"cudagraph_mode":"FULL_DECODE_ONLY"}`}} {
		if !containsArgumentPair(hopper.args, pair[0], pair[1]) {
			t.Errorf("DeepSeek V4 Pro Hopper lacks %s followed by %s: %v", pair[0], pair[1], hopper.args)
		}
	}
}

func TestNVIDIASourceRemediationAPIsAndH3Recipes(t *testing.T) {
	catalog, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}
	apiExpected := map[string]string{
		"mistralai/voxtral-mini-4b-realtime-2602": inferenceapi.OperationRealtime,
		"openmoss-team/moss-transcribe-diarize":   inferenceapi.OperationAudioTranscribe,
		"zai-org/glm-asr-nano-2512":               inferenceapi.OperationAudioTranscribe,
	}
	seenAPI := map[string]bool{}
	h3Specs := 0
	for _, model := range catalog.Models {
		for _, spec := range model.Specs {
			if operationType, tracked := apiExpected[model.ModelID]; tracked {
				for _, operation := range spec.API.Operations {
					seenAPI[model.ModelID] = seenAPI[model.ModelID] || operation.Type == operationType
				}
			}
			if model.ModelID == "minimaxai/minimax-h3" && spec.GPUs.Vendor == "nvidia" {
				h3Specs++
				if spec.Name == "nvfp4-vllm-nvidia-blackwell-fasth3-omni-1x" {
					// FastH3 Omni is a Ref2VA checkpoint; its only launch-time step
					// builds the offline Hugging Face cache entry for the base VAEs.
					if len(spec.Assets) != 0 || !strings.Contains(spec.PreStartScript, "snapshots/9bfb6693f2cf6de171db46d1aa586f67d773a1da") ||
						!containsArgumentPair(spec.Args, "--task-type", "ref2va") || spec.Env["HF_HOME"] == "" {
						t.Errorf("H3 %s FastH3 contract args=%v env=%v pre_start=%q", spec.Name, spec.Args, spec.Env, spec.PreStartScript)
					}
					continue
				}
				if strings.HasSuffix(spec.Name, "-turbo4") && (!containsArgumentPair(spec.Args, "--lora-path", "/models/MiniMax-H3-Turbo/minimax_h3_fl2v_turbo_4step_v1.3_768p_bf16.safetensors") ||
					!containsArgumentPair(spec.Args, "--default-sampling-params", `{"0":{"num_inference_steps":4,"guidance_scale":1}}`)) {
					t.Errorf("H3 %s does not load the Turbo adapter at its four-step schedule: %v", spec.Name, spec.Args)
				}
				// The vllm-omni 0.30 runner already carries the modular H3 pipeline;
				// the old source overlay would overwrite its engine files.
				if len(spec.Assets) != 0 || spec.PreStartScript != "" {
					t.Errorf("H3 %s still carries a source overlay: assets=%+v pre_start=%q", spec.Name, spec.Assets, spec.PreStartScript)
				}
				// Each profile is one recipe topology: it selects the FL2VA
				// partition and pins its GPU count through the resolver.
				if !slices.Contains(spec.Args, "--omni") || !containsArgumentPair(spec.Args, "--task-type", "fl2va") ||
					spec.Placement == nil || !strings.Contains(spec.Placement.Resolver, "gpu.count !=") {
					t.Errorf("H3 %s recipe contract args=%v placement=%+v", spec.Name, spec.Args, spec.Placement)
				}
			}
		}
	}
	for modelID := range apiExpected {
		if !seenAPI[modelID] {
			t.Errorf("%s lacks its audited API operation", modelID)
		}
	}
	if h3Specs != 8 {
		t.Fatalf("H3 NVIDIA specs=%d, want 8", h3Specs)
	}
}

// functionalArgsExempt lists non-NVIDIA vLLM specs whose launch arguments come
// from their own hardware-verified upstream recipe and are deliberately not
// rewritten to match the NVIDIA Hopper vLLM profiles, which were added later
// from the vLLM recipe. Each entry is a divergence to reconcile once the
// profile can be re-run on its hardware; none of them is a decision that the
// arguments should differ.
var functionalArgsExempt = map[string]string{
	"qwen/qwen3.8-flash-next/int8-vllm-ascend-a3-tp8":        "vLLM-Ascend's W8A8-MTP recipe uses --tool-call-parser qwen3_xml; the vLLM recipe for NVIDIA uses qwen3_coder",
	"google/gemma-4-26b-a4b-it/bf16-vllm-ascend-d8ff96c2":    "both Ascend profiles use the checkpoint's own chat template; the NVIDIA recipe references an example template",
	"google/gemma-4-26b-a4b-it/bf16-vllm-ascend-a3-d8ff96c2": "the A3 run uses the checkpoint's own chat template and eager mode; the NVIDIA recipe references an example template",
	"google/gemma-4-31b-it/bf16-vllm-ascend-c7a493cd":        "both Ascend profiles use the checkpoint's own chat template; the NVIDIA recipe references an example template",
	"google/gemma-4-31b-it/bf16-vllm-ascend-a3-c7a493cd":     "the A3 run uses the checkpoint's own chat template; the NVIDIA recipe references an example template",
	"zai-org/glm-5.3-flash/w8a8-vllm-910c-stock-graph-16npu": "the 2026-10-25 910C runs had no tool/reasoning parsers; adding them needs a re-run on 910C",
	"zai-org/glm-5.3-flash/w8a8-vllm-910c-stock-graph-8npu":  "the 2026-10-25 910C runs had no tool/reasoning parsers; adding them needs a re-run on 910C",
	"zai-org/glm-5.3-flash/w8a8-vllm-910c-dflash5-16npu":     "the 2026-10-25 910C runs had no tool/reasoning parsers; adding them needs a re-run on 910C",
}

func TestNonNVIDIAChatSpecsInheritModelFunctionalArgs(t *testing.T) {
	catalog, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}
	functionalFlags := map[string]bool{
		"--chat-template":           true,
		"--dllm-algorithm":          true,
		"--enable-auto-tool-choice": true,
		"--enable-multimodal":       true,
		"--logits-processors":       true,
		"--logits_processors":       true,
		"--mm-encoder-tp-mode":      true,
		"--reasoning-config":        true,
		"--reasoning-parser":        true,
		"--runner":                  true,
		"--tokenizer-mode":          true,
		"--tool-call-parser":        true,
	}
	type modelEngine struct {
		model  string
		engine string
	}
	byEngine := map[modelEngine][]inferencecatalog.InferenceSpec{}
	for _, model := range catalog.Models {
		for _, spec := range model.Specs {
			key := modelEngine{model: model.ModelID, engine: spec.Engine}
			byEngine[key] = append(byEngine[key], spec)
		}
	}
	for key, specs := range byEngine {
		var nvidia []inferencecatalog.InferenceSpec
		var nonNVIDIA []inferencecatalog.InferenceSpec
		for _, spec := range specs {
			if spec.GPUs.Vendor == "nvidia" {
				nvidia = append(nvidia, spec)
			} else {
				nonNVIDIA = append(nonNVIDIA, spec)
			}
		}
		if len(nvidia) == 0 || len(nonNVIDIA) == 0 {
			continue
		}
		want := map[string][]string{}
		for _, spec := range nvidia {
			for index, arg := range spec.Args {
				if !functionalFlags[arg] {
					continue
				}
				if index+1 < len(spec.Args) && !strings.HasPrefix(spec.Args[index+1], "-") {
					want[arg] = append(want[arg], spec.Args[index+1])
					continue
				}
				want[arg] = nil
			}
		}
		for _, spec := range nonNVIDIA {
			if reason, ok := functionalArgsExempt[key.model+"/"+spec.Name]; ok {
				t.Logf("%s/%s: not compared against the NVIDIA vLLM profile: %s", key.model, spec.Name, reason)
				continue
			}
			for flag, values := range want {
				if len(values) == 0 {
					if !slices.Contains(spec.Args, flag) {
						t.Errorf("%s/%s %s spec %s misses %s", key.model, key.engine, spec.GPUs.Vendor, spec.Name, flag)
					}
					continue
				}
				for _, value := range values {
					if !containsArgumentPair(spec.Args, flag, value) {
						t.Errorf("%s/%s %s spec %s misses %s %s", key.model, key.engine, spec.GPUs.Vendor, spec.Name, flag, value)
					}
				}
			}
		}
	}
}

func TestCatalogDoesNotDuplicateLogitsProcessors(t *testing.T) {
	catalog, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}
	for _, model := range catalog.Models {
		for _, spec := range model.Specs {
			seen := map[string]bool{}
			for index, arg := range spec.Args {
				if strings.ReplaceAll(arg, "_", "-") != "--logits-processors" || index+1 >= len(spec.Args) {
					continue
				}
				processor := spec.Args[index+1]
				if seen[processor] {
					t.Errorf("%s spec %s duplicates logits processor %s", model.ModelID, spec.Name, processor)
				}
				seen[processor] = true
			}
		}
	}
}

// TestDeclaredInputModalitiesCoverEveryConfigTower keeps the explicit
// input_modalities declaration in each model.yaml in step with the towers the
// model's own config proves. The declaration is the product fact - it decides
// whether the deploy form offers the multimodal parameters and whether the
// model list shows an image or audio tag - so it is written out rather than
// derived at load time. Deriving it would leave the two disagreeing silently,
// which is how a vision checkpoint ends up published as a text-only model.
//
// The assertion is one-directional on purpose. model_config is the estimate
// subset of config.json and only carries the tower keys the loader recognizes,
// so a model may legitimately declare a modality its subset does not show; the
// reverse - a proven tower that is not declared - is always a defect.
func TestDeclaredInputModalitiesCoverEveryConfigTower(t *testing.T) {
	catalog, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, model := range catalog.Models {
		if model.ModelConfig == nil {
			continue
		}
		checked++
		declared := map[string]bool{}
		for _, modality := range model.InputModalities {
			declared[modality] = true
		}
		if model.ModelConfig.VisionConfig != nil && !declared["image"] {
			t.Errorf("%s carries a vision tower but does not declare image input: %v", model.ModelID, model.InputModalities)
		}
		if model.ModelConfig.AudioConfig != nil && !declared["audio"] {
			t.Errorf("%s carries an audio tower but does not declare audio input: %v", model.ModelID, model.InputModalities)
		}
	}
	if checked == 0 {
		t.Fatal("no model carried a model_config; the rule would silently stop applying")
	}
}
func TestMingTTSVoiceCloningCanLoadItsAuxiliarySnapshot(t *testing.T) {
	catalog, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}
	for _, model := range catalog.Models {
		if model.ModelID != "inclusionai/ming-omni-tts-0.5b" {
			continue
		}
		for _, spec := range model.Specs {
			if spec.API == nil || len(spec.API.Operations) == 0 {
				continue
			}
			for _, operation := range spec.API.Operations {
				if operation.Type != inferenceapi.OperationAudioSpeech {
					continue
				}
				if spec.Env["HF_HUB_OFFLINE"] != "0" || spec.Env["TRANSFORMERS_OFFLINE"] != "0" {
					t.Errorf("%s voice-clone environment=%v; online auxiliary snapshot loading must be explicitly enabled", spec.Name, spec.Env)
				}
			}
		}
		return
	}
	t.Fatal("Ming-omni-tts model is absent from embedded catalog")
}

// The gateway and the health check send a callable engine the deployment's
// served name as is. A built-in callable spec that pins --served-model-name
// to a literal rejects every request a deployment of it receives.
func TestBuiltInCallableSpecsAnswerUnderTheServedName(t *testing.T) {
	catalog, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}
	for _, warning := range inferencecatalog.CatalogWarnings(catalog) {
		if warning.Code() == messages.WarningCatalogCallableServedNameFixed {
			t.Errorf("%v: %s", warning.Params(), warning.Error())
		}
	}
}
