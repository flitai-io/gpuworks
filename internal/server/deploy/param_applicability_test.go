package deploy

import (
	"reflect"
	"sort"
	"testing"
)

// sglangDiffusionUnstated are the SGLang keys with a dictionary default whose
// flag only the LLM server takes: a diffusion launch leaves them to the engine
// on every release (python/sglang/multimodal_gen/runtime/server_args/
// server_args.py).
var sglangDiffusionUnstated = []string{"dtype", "enable_auto_tool_choice", "enforce_eager", "gpu_memory_utilization", "kv_cache_dtype", "lora"}

func withKeys(base []string, extra ...string) []string {
	keys := append(append([]string(nil), base...), extra...)
	sort.Strings(keys)
	return keys
}

func TestEngineDecidesParameterKeysFollowEntrypointAndRelease(t *testing.T) {
	diffusionUnknown := withKeys(sglangDiffusionUnstated, "enable_cache_report", "enable_metrics")
	tests := []struct {
		name       string
		engine     string
		entrypoint []string
		args       []string
		version    string
		want       []string
	}{
		{name: "SGLang default LLM", engine: EngineSGLang},
		{name: "SGLang LLM with catalog flag", engine: EngineSGLang, args: []string{"--enable-metrics"}},
		{name: "SGLang LLM on an old release", engine: EngineSGLang, version: "0.5.12"},
		{name: "SGLang serve LLM", engine: EngineSGLang, entrypoint: []string{"sglang", "serve"}, args: []string{"--model-type", "llm"}},
		{name: "absolute Python LLM entrypoint", engine: EngineSGLang, entrypoint: []string{"/usr/bin/python3", "-m", "sglang.launch_server"}},
		{name: "SGLang diffusion", engine: EngineSGLang, entrypoint: []string{"sglang", "serve"}, args: []string{"--model-type", "diffusion"}, want: diffusionUnknown},
		{name: "absolute SGLang executable", engine: EngineSGLang, entrypoint: []string{"/usr/local/bin/sglang", "serve", "--model-type", "diffusion"}, want: diffusionUnknown},
		{name: "diffusion with catalog flag", engine: EngineSGLang, entrypoint: []string{"sglang", "serve"}, args: []string{"--model-type=diffusion", "--enable-metrics"}, want: diffusionUnknown},
		{name: "entrypoint arguments precede final args", engine: EngineSGLang, entrypoint: []string{"sglang", "serve", "--model-type", "diffusion"}, args: []string{"--model-type", "llm"}},
		{name: "last repeated model type selects diffusion", engine: EngineSGLang, entrypoint: []string{"sglang", "serve"}, args: []string{"--model-type", "llm", "--model-type=diffusion"}, want: diffusionUnknown},
		{name: "entrypoint override changes the dialect", engine: EngineSGLang, entrypoint: []string{"python", "-m", "sglang.launch_server"}, args: []string{"--model-type", "diffusion"}},
		// The diffusion server's shared switches arrive release by release:
		// --enable-cache-report in v0.5.18, --enable-metrics in v0.5.21.
		{name: "diffusion v0.5.12", engine: EngineSGLang, entrypoint: []string{"sglang", "serve"}, args: []string{"--model-type", "diffusion"}, version: "0.5.12", want: diffusionUnknown},
		{name: "diffusion v0.5.18", engine: EngineSGLang, entrypoint: []string{"sglang", "serve"}, args: []string{"--model-type", "diffusion"}, version: "0.5.18", want: withKeys(sglangDiffusionUnstated, "enable_metrics")},
		{name: "diffusion v0.5.21", engine: EngineSGLang, entrypoint: []string{"sglang", "serve"}, args: []string{"--model-type", "diffusion"}, version: "0.5.21", want: sglangDiffusionUnstated},
		// Without --model-type SGLang may pick either server, so a default goes
		// out only where both take its flag.
		{name: "serve detecting the model type", engine: EngineSGLang, entrypoint: []string{"sglang", "serve"}, args: []string{"--num-gpus", "1"}, want: diffusionUnknown},
		{name: "serve detecting the model type on v0.5.21", engine: EngineSGLang, entrypoint: []string{"sglang", "serve"}, version: "0.5.21", want: sglangDiffusionUnstated},
		{name: "unknown executable", engine: EngineSGLang, entrypoint: []string{"custom-server"}, want: []string{"enable_metrics"}},
		{name: "vLLM release with per-request metrics", engine: EngineVLLM, entrypoint: []string{"vllm", "serve"}, version: "0.30.0"},
		{name: "vLLM release nobody recorded", engine: EngineVLLM, entrypoint: []string{"vllm", "serve"}, want: []string{"enable_request_metrics", "max_model_len"}},
		{name: "vLLM release before per-request metrics", engine: EngineVLLM, version: "0.23.0", want: []string{"enable_request_metrics"}},
		{name: "vLLM with stats disabled", engine: EngineVLLM, args: []string{"--disable-log-stats"}, version: "0.30.0", want: []string{"enable_request_metrics"}},
		{name: "vLLM omni", engine: EngineVLLM, entrypoint: []string{"vllm", "serve"}, args: []string{"--omni"}, version: "0.30.0", want: []string{"gpu_memory_utilization", "max_model_len"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := EngineDecidesParameterKeys(test.engine, LaunchShape{Entrypoint: test.entrypoint, Args: test.args, EngineVersion: test.version})
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("EngineDecidesParameterKeys()=%v, want %v", got, test.want)
			}
		})
	}

	vllm, err := ProfileForEngine(EngineVLLM)
	if err != nil {
		t.Fatal(err)
	}
	for _, definition := range vllm.ParamDefinitions {
		if definition.Key == "enable_metrics" {
			t.Fatal("vLLM metrics are always exposed by the engine and must not publish an enable_metrics toggle")
		}
	}
}

// A diffusion launch states none of the LLM server's defaults, so its command
// carries no --mem-fraction-static; a switch its release takes keeps its
// default.
func TestMaterializeLeavesLLMDefaultsOffADiffusionLaunch(t *testing.T) {
	command, err := ResolveEffectiveCommand(ContainerOverrides{}, []string{EngineSGLang, "serve"})
	if err != nil {
		t.Fatal(err)
	}
	out, err := Materialize(MaterializeInput{Engine: EngineSGLang, Command: &command, TemplateArgs: []string{"--model-type", "diffusion"}, EngineVersion: "0.5.21"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(out.EngineDecidesParamKeys, sglangDiffusionUnstated) {
		t.Fatalf("engine decides %v, want %v", out.EngineDecidesParamKeys, sglangDiffusionUnstated)
	}
	built := mustEngine(t, EngineSGLang).Build(goldenParams(out.Quick))
	if commandHasFlag(built.Entrypoint, "--mem-fraction-static") || !commandHasFlag(built.Entrypoint, "--enable-metrics") || !commandHasFlag(built.Entrypoint, "--enable-cache-report") {
		t.Fatalf("diffusion command: %v", built.Entrypoint)
	}
}

// Leaving a key to the engine is a judgement that can be wrong, so it never
// refuses a value: whatever the request, its extra_args or the template state
// is kept and emitted, on or off.
func TestMaterializeKeepsValuesStatedForKeysLeftToTheEngine(t *testing.T) {
	command, err := ResolveEffectiveCommand(ContainerOverrides{}, []string{EngineSGLang, "serve"})
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name  string
		input MaterializeInput
		want  bool
	}{
		{name: "quick true", input: MaterializeInput{Raw: map[string]any{"enable_metrics": true}}, want: true},
		{name: "quick false", input: MaterializeInput{Raw: map[string]any{"enable_metrics": false}}},
		{name: "extra args", input: MaterializeInput{ExtraArgs: []string{"--enable-metrics"}}, want: true},
		{name: "template args", input: MaterializeInput{TemplateArgs: []string{"--model-type", "diffusion", "--enable-metrics"}}, want: true},
		{name: "quick false over the template", input: MaterializeInput{Raw: map[string]any{"enable_metrics": false}, TemplateArgs: []string{"--model-type", "diffusion", "--enable-metrics"}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			input := test.input
			input.Engine = EngineSGLang
			input.Command = &command
			if len(input.TemplateArgs) == 0 {
				input.TemplateArgs = []string{"--model-type", "diffusion"}
			}
			out, err := Materialize(input)
			if err != nil {
				t.Fatal(err)
			}
			if out.Quick.EnableMetrics != test.want {
				t.Fatalf("enable_metrics = %v, want %v", out.Quick.EnableMetrics, test.want)
			}
		})
	}
}

func TestMaterializeJudgesTheFinalEntrypoint(t *testing.T) {
	llmOverride, err := ResolveEffectiveCommand(ContainerOverrides{EntrypointDisplay: `["python","-m","sglang.launch_server"]`}, []string{EngineSGLang, "serve"})
	if err != nil {
		t.Fatal(err)
	}
	out, err := Materialize(MaterializeInput{Engine: EngineSGLang, Command: &llmOverride, TemplateArgs: []string{"--model-type", "diffusion"}})
	if err != nil || !out.Quick.EnableMetrics || len(out.EngineDecidesParamKeys) != 0 {
		t.Fatalf("LLM override materialize = %+v, err=%v", out, err)
	}

	diffusionOverride, err := ResolveEffectiveCommand(ContainerOverrides{EntrypointDisplay: `["/usr/local/bin/sglang","serve"]`}, []string{"python", "-m", "sglang.launch_server"})
	if err != nil {
		t.Fatal(err)
	}
	out, err = Materialize(MaterializeInput{Engine: EngineSGLang, Command: &diffusionOverride, ExtraArgs: []string{"--model-type=diffusion"}})
	if err != nil || out.Quick.EnableMetrics || out.Quick.GPUMemUtil != 0 {
		t.Fatalf("diffusion override materialize = %+v, err=%v", out.Quick, err)
	}
}

func TestMaterializeDoesNotInterpretManualCommand(t *testing.T) {
	manual, err := ResolveEffectiveCommand(ContainerOverrides{CommandMode: string(CommandManual), CmdDisplay: `sglang serve --model-type diffusion --enable-metrics`}, []string{EngineSGLang, "serve"})
	if err != nil {
		t.Fatal(err)
	}
	out, err := Materialize(MaterializeInput{
		Engine: EngineSGLang, Command: &manual,
		Raw: map[string]any{"enable_metrics": true}, TemplateArgs: []string{"--model-type", "diffusion"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !out.Quick.EnableMetrics || len(out.EngineDecidesParamKeys) != 0 {
		t.Fatalf("manual materialize = %+v", out)
	}
}
