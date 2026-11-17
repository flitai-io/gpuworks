package deploy

import (
	"bytes"
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
)

func TestQuickParamsAndParamSpecsStayConsistent(t *testing.T) {
	specKeys := map[string]bool{}
	for _, definition := range parameterDefinitions {
		specKeys[definition.Key] = true
	}
	internalKeys := map[string]bool{
		"cpu_cores": true, "estimated_gpu_kv_gib": true,
		"gpu_block_size": true, "kv_offload_block_size": true,
		"python_hash_seed": true, "served_model_name": true,
	}
	typeOfQuick := reflect.TypeOf(LaunchConfig{})
	for index := 0; index < typeOfQuick.NumField(); index++ {
		field := typeOfQuick.Field(index)
		key := strings.Split(field.Tag.Get("json"), ",")[0]
		if key == "" || key == "-" {
			continue
		}
		if !specKeys[key] && !internalKeys[key] {
			t.Errorf("LaunchConfig key %q has no ParamDefinition and is not internal", key)
		}
	}
	for _, key := range []string{"data_parallel_size", "lora"} {
		if !specKeys[key] {
			t.Errorf("public LaunchConfig key %q is missing from ParamDefinition", key)
		}
	}
}

func TestQuickFormKeysAreBackedByParamSpecs(t *testing.T) {
	for _, engine := range EngineNames() {
		profile, err := ProfileForEngine(engine)
		if err != nil {
			t.Fatal(err)
		}
		specs := make(map[string]bool, len(profile.ParamDefinitions))
		for _, spec := range profile.ParamDefinitions {
			specs[spec.Key] = true
		}
		if len(profile.ParamDefinitions) == 0 {
			if len(profile.QuickFormKeys) != 0 {
				t.Errorf("%s exposes quick form keys without parameter definitions", engine)
			}
			continue
		}
		if len(profile.QuickFormKeys) == 0 {
			t.Errorf("%s has no default quick form keys", engine)
		}
		for _, key := range profile.QuickFormKeys {
			if !specs[key] {
				t.Errorf("%s quick form key %q has no ParamDefinition", engine, key)
			}
		}
	}
}

// The quick form's KV Offload row is driven by the server-owned whitelist: a
// tier key missing from it renders as "not provided by this engine" even
// though the dictionary and the adapter support it.
func TestVLLMQuickFormExposesKVTiers(t *testing.T) {
	profile, err := ProfileForEngine(EngineVLLM)
	if err != nil {
		t.Fatal(err)
	}
	keys := make(map[string]bool, len(profile.QuickFormKeys))
	for _, key := range profile.QuickFormKeys {
		keys[key] = true
	}
	for _, key := range []string{"kv_offloading_size", "kv_disk_offload", "shared_kv_dir", "kv_offload_read_threads", "kv_offload_write_threads"} {
		if !keys[key] {
			t.Errorf("vllm quick form keys omit KV tier key %q: %v", key, profile.QuickFormKeys)
		}
	}
	sglang, err := ProfileForEngine(EngineSGLang)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range sglang.QuickFormKeys {
		if key == "kv_offloading_size" || key == "kv_disk_offload" {
			t.Errorf("sglang quick form advertises a vLLM-only KV tier key %q", key)
		}
	}
}

// The disk KV tier is one --kv-transfer-config blob: the switch and the three
// settings that go into it share that flag, and the deploy form reads the
// companion set off the shared flag rather than keeping a key list of its own
// (ADR 0020 decision 1). A key added to or removed from that flag changes what
// the form renders and submits, so the set is pinned here.
func TestVLLMDiskTierSharesOneTransferConfigFlag(t *testing.T) {
	profile, err := ProfileForEngine(EngineVLLM)
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, spec := range profile.ParamDefinitions {
		if spec.Flag == "--kv-transfer-config" {
			keys = append(keys, spec.Key)
		}
	}
	want := []string{"kv_disk_offload", "shared_kv_dir", "kv_offload_read_threads", "kv_offload_write_threads"}
	if !slices.Equal(keys, want) {
		t.Fatalf("--kv-transfer-config keys = %v, want %v", keys, want)
	}
}

// argvHas returns true if the argv contains "flag value" adjacently.
func argvHas(argv []string, flag, value string) bool {
	for i := 0; i+1 < len(argv); i++ {
		if argv[i] == flag && argv[i+1] == value {
			return true
		}
	}
	return false
}

func TestBuildVLLMBasics(t *testing.T) {
	r := BuildVLLM(Params{
		ModelPath:      "/models/qwen3-8b",
		ServedName:     "qwen3-8b",
		APIPort:        30000,
		APIKey:         "sk-abc",
		TensorParallel: 2,
		Quick:          LaunchConfig{MaxModelLen: 8192, Dtype: "bfloat16"},
	})
	if !argvHas(r.Argv, "--model", "/models/qwen3-8b") {
		t.Errorf("missing --model: %v", r.Argv)
	}
	if !argvHas(r.Argv, "--host", "127.0.0.1") {
		t.Error("default host not injected")
	}
	if !argvHas(r.Argv, "--port", "30000") {
		t.Error("port not injected")
	}
	if !argvHas(r.Argv, "--tensor-parallel-size", "2") {
		t.Error("tp not injected")
	}
	if !argvHas(r.Argv, "--max-model-len", "8192") {
		t.Error("max-model-len not mapped")
	}
	if !argvHas(r.Argv, "--dtype", "bfloat16") {
		t.Error("dtype not mapped")
	}
}

func TestBuildVLLMDoesNotRepeatEquivalentCatalogModelIdentity(t *testing.T) {
	result := BuildVLLM(Params{
		ModelPath: "/models/target", ServedName: "target", APIPort: 8000, PositionalModel: true,
		TemplateArgs: []string{"--model", "/models/target", "--speculative-config", `{"draft_model":"/models/draft"}`},
	})
	command := append(append([]string{}, result.Entrypoint...), result.Argv...)
	if got := strings.Count(strings.Join(command, " "), "/models/target"); got != 1 {
		t.Fatalf("primary model path occurs %d times in %v", got, command)
	}
	if !strings.Contains(strings.Join(result.Argv, " "), `--speculative-config {"draft_model":"/models/draft"}`) {
		t.Fatalf("speculative config was lost: %v", command)
	}
}

func TestVLLMEngineUsesModernServeEntrypoint(t *testing.T) {
	eng, err := SelectEngine("vllm")
	if err != nil {
		t.Fatal(err)
	}
	r := eng.Build(Params{ModelPath: "/models/qwen3-8b", APIPort: 30000})
	if got, want := strings.Join(r.Entrypoint, " "), "vllm serve /models/qwen3-8b"; got != want {
		t.Fatalf("entrypoint = %q, want %q", got, want)
	}
	if argvHas(r.Argv, "--model", "/models/qwen3-8b") {
		t.Fatalf("registry vllm engine should use positional model, got argv %v", r.Argv)
	}
}

func TestUserExtraArgsWin(t *testing.T) {
	// Residual arguments are the only ones left by the time a build runs: a
	// flag the dictionary knows has already become a field. What remains true
	// here is that a residual flag beats platform injection and is passed on
	// verbatim. A dictionary flag written in extra_args is a move or a conflict
	// instead, which materialization decides (materialize_test.go).
	r := BuildVLLM(Params{
		ModelPath: "/m",
		APIPort:   30000,
		ExtraArgs: []string{"--host", "0.0.0.0", "--foo"},
	})
	// Platform host injection suppressed → only the user's 0.0.0.0 remains.
	if argvHas(r.Argv, "--host", "127.0.0.1") {
		t.Error("platform injected --host despite user override")
	}
	if !argvHas(r.Argv, "--host", "0.0.0.0") {
		t.Error("user --host missing")
	}
	if got := strings.Count(strings.Join(r.Argv, " "), "--host"); got != 1 {
		t.Errorf("--host appears %d times: %v", got, r.Argv)
	}
	// User's extra args appended verbatim.
	if !slices.Contains(r.Argv, "--foo") {
		t.Error("user --foo missing")
	}
}

func TestVLLMMultimodalQuickParamsAndUserOverride(t *testing.T) {
	r := BuildVLLM(Params{
		ModelPath: "/model", APIPort: 30000,
		Quick: LaunchConfig{
			LimitMMPerPrompt:      `{"image":2,"audio":1}`,
			MMProcessorKwargs:     `{"max_dynamic_patch":12}`,
			MediaIOKwargs:         `{"video":{"num_frames":16}}`,
			AllowedLocalMediaPath: "/model/media",
		},
		ExtraArgs: []string{"--limit-mm-per-prompt", `{"image":4}`},
	})
	for _, pair := range [][2]string{
		{"--limit-mm-per-prompt", `{"image":4}`},
		{"--mm-processor-kwargs", `{"max_dynamic_patch":12}`},
		{"--media-io-kwargs", `{"video":{"num_frames":16}}`},
		{"--allowed-local-media-path", "/model/media"},
	} {
		if !argvHas(r.Argv, pair[0], pair[1]) {
			t.Fatalf("missing %s %s in %v", pair[0], pair[1], r.Argv)
		}
	}
	if got := strings.Count(strings.Join(r.Argv, " "), "--limit-mm-per-prompt"); got != 1 {
		t.Fatalf("user override duplicated limit flag %d times: %v", got, r.Argv)
	}
	prof, err := ProfileForEngine("vllm")
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"limit_mm_per_prompt", "mm_processor_kwargs", "media_io_kwargs", "allowed_local_media_path"} {
		if prof.ParamDict[key] == "" {
			t.Fatalf("vLLM ParamDict missing %q: %#v", key, prof.ParamDict)
		}
	}
}

func TestValidateQuickParams(t *testing.T) {
	valid := configured(t, EngineVLLM, func(q *LaunchConfig) {
		q.LimitMMPerPrompt, q.MMProcessorKwargs, q.MediaIOKwargs = `{"image":2}`, `{}`, `{"audio":{}}`
		q.AllowedLocalMediaPath = "/model/media"
	})
	if err := ValidateLaunchConfig(valid); err != nil {
		t.Fatalf("valid quick params: %v", err)
	}
	for name, apply := range map[string]func(*LaunchConfig){
		"limit JSON":     func(q *LaunchConfig) { q.LimitMMPerPrompt = `{bad` },
		"processor JSON": func(q *LaunchConfig) { q.MMProcessorKwargs = `{bad` },
		"media JSON":     func(q *LaunchConfig) { q.MediaIOKwargs = `{bad` },
		"rope JSON":      func(q *LaunchConfig) { q.RopeScaling = `{bad` },
		"relative path":  func(q *LaunchConfig) { q.AllowedLocalMediaPath = "model/media" },
		"root path":      func(q *LaunchConfig) { q.AllowedLocalMediaPath = "/" },
	} {
		t.Run(name, func(t *testing.T) {
			quick := configured(t, EngineVLLM, apply)
			if err := ValidateLaunchConfig(quick); err == nil {
				t.Fatalf("ValidateLaunchConfig(%+v) unexpectedly succeeded", quick)
			}
		})
	}
}

func TestVLLMProfileExposesKVOffloadControls(t *testing.T) {
	profile, err := ProfileForEngine("vllm")
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	for _, spec := range profile.ParamDefinitions {
		found[spec.Key] = true
	}
	for _, key := range []string{"kv_offloading_size", "kv_disk_offload", "shared_kv_dir", "kv_offload_read_threads", "kv_offload_write_threads", "long_prefill_token_threshold"} {
		if !found[key] {
			t.Fatalf("vLLM param_specs missing %q", key)
		}
	}
}

// A boolean the dictionary gives no default is three-state on the form: auto
// stays unstated, so no flag is emitted and the engine keeps its own default.
// Prefix caching and chunked prefill are deliberately that — the engines enable
// both themselves and disable them for architectures that cannot carry them
// (omni-class encoder-decoder models), and a published default would let the
// form force the flag on and turn that judgement into a startup failure.
func TestEngineDecidedBooleansPublishNoDefault(t *testing.T) {
	published := map[string]map[string]bool{EngineVLLM: {}, EngineSGLang: {}}
	for engine := range published {
		profile, err := ProfileForEngine(engine)
		if err != nil {
			t.Fatal(err)
		}
		for _, spec := range profile.ParamDefinitions {
			published[engine][spec.Key] = spec.Default != nil
		}
	}
	for engine, keys := range map[string][]string{
		EngineVLLM:   {"enable_prefix_caching", "enable_chunked_prefill"},
		EngineSGLang: {"enable_prefix_caching"},
	} {
		for _, key := range keys {
			if withDefault, known := published[engine][key]; !known {
				t.Fatalf("%s param_specs missing %q", engine, key)
			} else if withDefault {
				t.Fatalf("%s param_specs publishes a default for %q: an engine-decided boolean must stay unstated", engine, key)
			}
		}
	}
}

func TestVLLMPerformanceCommandMappings(t *testing.T) {
	res := BuildVLLM(Params{ModelPath: "/model", APIPort: 30000, Quick: LaunchConfig{
		EnableChunkedPrefill: boolp(false), EnablePrefixCaching: boolp(true),
		SpeculativeAlgorithm: "draft_model", SpeculativeDraftModel: "/draft", SpeculativeTokens: 5,
		OptimizationLevel: "O2", AsyncScheduling: true, CPUOffloadGB: 12,
		KVCacheMemoryBytes: 1024, PrefixCachingHashAlgo: "sha256", StreamInterval: 2,
		Watermark: 0.05, MaxCudagraphCaptureSize: 128, MMEncoderTPMode: "data", APIServerCount: 3,
		ReasoningParser: "qwen3", ToolCallParser: "hermes", EnableAutoToolChoice: true,
	}})
	for _, flag := range []string{"--no-enable-chunked-prefill", "--enable-prefix-caching", "-O2", "--async-scheduling", "--enable-auto-tool-choice"} {
		if !slices.Contains(res.Argv, flag) {
			t.Fatalf("missing %s in %v", flag, res.Argv)
		}
	}
	for _, pair := range [][2]string{
		{"--cpu-offload-gb", "12"}, {"--kv-cache-memory-bytes", "1024"}, {"--prefix-caching-hash-algo", "sha256"},
		{"--stream-interval", "2"}, {"--watermark", "0.05"}, {"--max-cudagraph-capture-size", "128"},
		{"--mm-encoder-tp-mode", "data"}, {"--api-server-count", "3"}, {"--reasoning-parser", "qwen3"}, {"--tool-call-parser", "hermes"},
	} {
		if !argvHas(res.Argv, pair[0], pair[1]) {
			t.Fatalf("missing %s %s in %v", pair[0], pair[1], res.Argv)
		}
	}
	var speculative map[string]any
	for i := range res.Argv {
		if res.Argv[i] == "--speculative-config" && i+1 < len(res.Argv) {
			if err := json.Unmarshal([]byte(res.Argv[i+1]), &speculative); err != nil {
				t.Fatal(err)
			}
		}
	}
	if speculative["method"] != "draft_model" || speculative["model"] != "/draft" || speculative["num_speculative_tokens"] != float64(5) {
		t.Fatalf("unexpected speculative config: %#v", speculative)
	}
}

func TestVLLMDiskKVTransferConfig(t *testing.T) {
	res := BuildVLLM(Params{ModelPath: "/model", APIPort: 30000, Quick: LaunchConfig{
		KVDiskOffload: true, KVOffloadingSize: 4, SharedKVDir: "/cache/kv",
		KVOffloadReadThreads: 8, KVOffloadWriteThreads: 12,
	}})
	var raw string
	for i := range res.Argv {
		if res.Argv[i] == "--kv-transfer-config" && i+1 < len(res.Argv) {
			raw = res.Argv[i+1]
		}
	}
	if raw == "" {
		t.Fatalf("missing disk KV config: %v", res.Argv)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatal(err)
	}
	extra, ok := got["kv_connector_extra_config"].(map[string]any)
	if !ok || got["kv_connector"] != "OffloadingConnector" || got["kv_role"] != "kv_both" || extra["spec_name"] != "TieringOffloadingSpec" || extra["cpu_bytes_to_use"] != float64(4<<30) {
		t.Fatalf("unexpected disk KV config: %#v", got)
	}
	tiers, ok := extra["secondary_tiers"].([]any)
	if !ok || len(tiers) != 1 {
		t.Fatalf("unexpected secondary tiers: %#v", extra["secondary_tiers"])
	}
	tier, ok := tiers[0].(map[string]any)
	if !ok || tier["type"] != "fs" || tier["root_dir"] != "/cache/kv" || tier["n_read_threads"] != float64(8) || tier["n_write_threads"] != float64(12) {
		t.Fatalf("unexpected fs tier: %#v", tier)
	}
}

func TestSGLangPerformanceAndSpeculativeMappings(t *testing.T) {
	engine, _ := SelectEngine("sglang")
	res := engine.Build(Params{ModelPath: "/model", APIPort: 30000, Quick: LaunchConfig{
		GPUMemUtil: 0.92, MaxNumBatchedTokens: 4096, MaxNumSeqs: 128, EnablePrefixCaching: boolp(false),
		EnforceEager: true, SpeculativeAlgorithm: "eagle3", SpeculativeDraftModel: "/draft",
		SpeculativeNumSteps: 3, SpeculativeEagleTopK: 4, SpeculativeTokens: 5,
		ReasoningParser: "qwen3", ToolCallParser: "hermes", EnableAutoToolChoice: true,
	}})
	for _, flag := range []string{"--disable-radix-cache", "--disable-cuda-graph", "--enable-auto-tool-choice"} {
		if !slices.Contains(res.Entrypoint, flag) {
			t.Fatalf("missing %s in %v", flag, res.Entrypoint)
		}
	}
	for _, pair := range [][2]string{
		{"--mem-fraction-static", "0.92"}, {"--chunked-prefill-size", "4096"}, {"--max-running-requests", "128"},
		{"--speculative-algorithm", "EAGLE3"}, {"--speculative-draft-model-path", "/draft"},
		{"--speculative-num-steps", "3"}, {"--speculative-eagle-topk", "4"}, {"--speculative-num-draft-tokens", "5"},
		{"--reasoning-parser", "qwen3"}, {"--tool-call-parser", "hermes"},
	} {
		if !argvHas(res.Entrypoint, pair[0], pair[1]) {
			t.Fatalf("missing %s %s in %v", pair[0], pair[1], res.Entrypoint)
		}
	}
	if err := ValidateLaunchConfigForEngine("sglang", configured(t, EngineSGLang, func(q *LaunchConfig) { q.PerformanceMode = "balanced" })); err != nil {
		t.Fatalf("UI balanced default must be accepted for SGLang: %v", err)
	}
}

func TestCacheReportFlagFollowsTheConfiguredValue(t *testing.T) {
	vllmArgv := func(q LaunchConfig) []string {
		return BuildVLLM(Params{ModelPath: "/model", APIPort: 30000, Quick: q}).Argv
	}
	if n := strings.Count(strings.Join(vllmArgv(LaunchConfig{EnableCacheReport: boolp(true)}), " "), "--enable-prompt-tokens-details"); n != 1 {
		t.Fatalf("vllm: --enable-prompt-tokens-details count=%d, want exactly one", n)
	}
	if slices.Contains(vllmArgv(LaunchConfig{EnableCacheReport: boolp(false)}), "--enable-prompt-tokens-details") {
		t.Fatal("vllm: an explicit off must not emit the flag")
	}
	if slices.Contains(vllmArgv(LaunchConfig{}), "--enable-prompt-tokens-details") {
		t.Fatal("vllm: an unstated switch must not emit the flag")
	}
	sglangEntrypoint := func(q LaunchConfig) []string {
		engine, _ := SelectEngine(EngineSGLang)
		return engine.Build(Params{ModelPath: "/model", APIPort: 30000, Quick: q}).Entrypoint
	}
	if n := strings.Count(strings.Join(sglangEntrypoint(LaunchConfig{EnableCacheReport: boolp(true)}), " "), "--enable-cache-report"); n != 1 {
		t.Fatalf("sglang: --enable-cache-report count=%d, want exactly one", n)
	}
	if slices.Contains(sglangEntrypoint(LaunchConfig{EnableCacheReport: boolp(false)}), "--enable-cache-report") {
		t.Fatal("sglang: an explicit off must not emit the flag")
	}
	if slices.Contains(sglangEntrypoint(LaunchConfig{}), "--enable-cache-report") {
		t.Fatal("sglang: an unstated switch must not emit the flag")
	}
	// An untouched form states the dictionary's default for a switch that has
	// one; only a row with no default stays nil.
	out, err := Materialize(MaterializeInput{Engine: EngineVLLM})
	if err != nil || out.Quick.EnableCacheReport == nil || !*out.Quick.EnableCacheReport {
		t.Fatalf("an unstated switch with a default must materialize to it: quick=%+v err=%v", out.Quick, err)
	}
	plain, err := Materialize(MaterializeInput{Engine: EngineVLLM})
	if err != nil || plain.Quick.EnablePrefixCaching != nil {
		t.Fatalf("an unstated switch without a default must stay nil: quick=%+v err=%v", plain.Quick, err)
	}
}

func TestValidatePerformanceRules(t *testing.T) {
	if err := ValidateLaunchConfig(configured(t, EngineVLLM, func(q *LaunchConfig) {
		q.MaxModelLen, q.LongPrefillTokenThreshold = 4096, 4097
	})); err == nil {
		t.Fatal("R3 threshold validation missing")
	}
	if err := ValidateLaunchConfig(configured(t, EngineVLLM, func(q *LaunchConfig) { q.KVDiskOffload = true })); err == nil {
		t.Fatal("R5 disk tier validation missing")
	}
	if err := ValidateLaunchConfig(configured(t, EngineVLLM, func(q *LaunchConfig) {
		q.KVOffloadBlockSize, q.GPUBlockSize = 3, 2
	})); err == nil {
		t.Fatal("R6 block-size validation missing")
	}
	if err := ValidateLaunchConfig(configured(t, EngineVLLM, func(q *LaunchConfig) {
		q.LoRA, q.SpeculativeTokens, q.MaxNumSeqs, q.MaxNumBatchedTokens = true, 2, 4, 8
	})); err == nil {
		t.Fatal("R4 speculative validation missing")
	}
}

func TestPerformanceWarningsCoverW1ToW10(t *testing.T) {
	w := PerformanceWarnings(LaunchConfig{MaxNumBatchedTokens: 100, MaxNumSeqs: 1, MaxModelLen: 10, KVOffloadingSize: 1, EstimatedGPUKVGiB: 2, KVCacheMemoryBytes: 1, PerformanceMode: "throughput", EnforceEager: true, SharedKVDir: "/kv", PrefixCachingHashAlgo: "xxhash", AsyncScheduling: true, SpeculativeAlgorithm: "draft_model", APIServerCount: 4, DP: 2, CPUCores: 1}, "--kv-transfer-config cudagraph", "encoder-decoder", 2)
	for _, code := range []string{"W1", "W2", "W3", "W4", "W5", "W6", "W7", "W8", "W9", "W10"} {
		found := false
		for _, item := range w {
			if item.Code == code {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("missing %s in %v", code, w)
		}
	}
	if w[0].Params["max_num_batched_tokens"] != 100 || w[len(w)-1].Params["required_cores"] != 9 {
		t.Fatalf("warning params do not describe the evaluated values: %#v", w)
	}
}

func TestPerformanceModeAndAutomaticBatchFlags(t *testing.T) {
	res := BuildVLLM(Params{ModelPath: "/model", ServedName: "qwen", APIPort: 30000, Quick: LaunchConfig{PerformanceMode: "throughput"}})
	joined := strings.Join(res.Argv, " ")
	if !strings.Contains(joined, "--performance-mode throughput") {
		t.Fatalf("throughput mode missing: %v", res.Argv)
	}
	if strings.Contains(joined, "--max-num-seqs") || strings.Contains(joined, "--max-num-batched-tokens") {
		t.Fatalf("automatic batch values must not generate flags: %v", res.Argv)
	}
	if err := ValidateLaunchConfig(configured(t, EngineVLLM, func(q *LaunchConfig) {
		q.MaxNumBatchedTokens, q.MaxNumSeqs = 128, 256
	})); err == nil {
		t.Fatal("invalid batch ordering accepted")
	}
	if err := ValidateLaunchConfig(configured(t, EngineVLLM, func(q *LaunchConfig) {
		q.MaxModelLen, q.MaxNumBatchedTokens, q.EnableChunkedPrefill = 8192, 2048, boolp(false)
	})); err == nil {
		t.Fatal("R1 chunked-prefill validation missing")
	}
}

func TestValidateQuickParamsEnumByEngine(t *testing.T) {
	kvDtype := func(value string) func(*LaunchConfig) {
		return func(q *LaunchConfig) { q.KVCacheDtype = value }
	}
	if err := ValidateLaunchConfigForEngine("vllm", configured(t, EngineVLLM, kvDtype("fp8_e4m3"))); err != nil {
		t.Fatalf("vllm fp8_e4m3 rejected: %v", err)
	}
	if err := ValidateLaunchConfigForEngine("sglang", configured(t, EngineSGLang, kvDtype("fp8"))); err == nil {
		t.Fatal("sglang accepted vllm-only fp8 alias")
	}
	if err := ValidateLaunchConfigForEngine("sglang", configured(t, EngineSGLang, kvDtype("fp8_e5m2"))); err != nil {
		t.Fatalf("sglang fp8_e5m2 rejected: %v", err)
	}
}

func TestValidateQuickParamsBoundsAndDtype(t *testing.T) {
	for name, apply := range map[string]func(*LaunchConfig){
		"dtype":     func(q *LaunchConfig) { q.Dtype = "int8" },
		"util high": func(q *LaunchConfig) { q.GPUMemUtil = 1.01 },
		"util low":  func(q *LaunchConfig) { q.GPUMemUtil = -0.1 },
		"length":    func(q *LaunchConfig) { q.MaxModelLen = -2 },
	} {
		t.Run(name, func(t *testing.T) {
			if err := ValidateLaunchConfig(configured(t, EngineVLLM, apply)); err == nil {
				t.Fatal("invalid quick params accepted")
			}
		})
	}
}

func TestStructuredParamSpecsHaveEngineSpecificEnumsAndHelp(t *testing.T) {
	vllm, _ := ProfileForEngine("vllm")
	sglang, _ := ProfileForEngine("sglang")
	var vllmKV, sglangKV ParamDefinition
	for _, spec := range vllm.ParamDefinitions {
		if spec.Key == "kv_cache_dtype" {
			vllmKV = spec
		}
	}
	for _, spec := range sglang.ParamDefinitions {
		if spec.Key == "kv_cache_dtype" {
			sglangKV = spec
		}
	}
	if !slices.Equal(vllmKV.Enum, vllmKVCacheDtypes) || !slices.Contains(vllmKV.Enum, "fp8") ||
		!slices.Equal(sglangKV.Enum, []string{"auto", "bfloat16", "fp8_e4m3", "fp8_e5m2", "mxfp8", "nvfp4", "fp4_mx_block16"}) ||
		!hasRequiredLocalizedText(vllmKV.Placeholder) || !hasRequiredLocalizedText(vllmKV.Help) {
		t.Fatalf("param specs = %#v / %#v", vllmKV, sglangKV)
	}
	keys := map[string]bool{}
	for _, spec := range vllm.ParamDefinitions {
		keys[spec.Key] = true
	}
	for _, key := range []string{"max_num_seqs", "quantization", "rope_scaling", "trust_remote_code", "enforce_eager"} {
		if !keys[key] {
			t.Fatalf("vllm param dictionary missing %q: %#v", key, vllm.ParamDefinitions)
		}
	}
	for _, key := range []string{"performance_mode", "long_prefill_token_threshold", "kv_offloading_size", "limit_mm_per_prompt"} {
		if _, ok := sglang.ParamDict[key]; ok {
			t.Fatalf("sglang advertises unsupported parameter %q: %#v", key, sglang.ParamDict)
		}
	}
}

func TestParamDictionaryCoversPlannedPerformanceAndModelControls(t *testing.T) {
	vllm, _ := ProfileForEngine("vllm")
	sglang, _ := ProfileForEngine("sglang")
	vllmSpecs := map[string]ParamDefinition{}
	for _, spec := range vllm.ParamDefinitions {
		vllmSpecs[spec.Key] = spec
		if !hasRequiredLocalizedText(spec.Label) || spec.Control == "" || spec.Group == "" {
			t.Fatalf("incomplete vLLM metadata for %q: %#v", spec.Key, spec)
		}
	}
	for _, key := range []string{
		"performance_mode", "gpu_memory_utilization", "enable_chunked_prefill", "max_num_batched_tokens", "max_num_seqs",
		"enable_prefix_caching", "kv_cache_dtype", "kv_offloading_size", "kv_disk_offload", "shared_kv_dir",
		"kv_offload_read_threads", "kv_offload_write_threads", "enforce_eager", "speculative_algorithm",
		"speculative_draft_model", "speculative_tokens", "optimization_level", "async_scheduling",
		"long_prefill_token_threshold", "kv_cache_memory_bytes", "prefix_caching_hash_algo", "cpu_offload_gb",
		"stream_interval", "watermark", "max_cudagraph_capture_size", "mm_encoder_tp_mode", "api_server_count",
		"reasoning_parser", "tool_call_parser", "enable_auto_tool_choice",
	} {
		if _, ok := vllmSpecs[key]; !ok {
			t.Fatalf("vLLM parameter dictionary missing %q", key)
		}
	}
	util := vllmSpecs["gpu_memory_utilization"]
	if util.Control != "slider" || util.Default != 0.9 || util.Min == nil || *util.Min != 0.01 || util.Max == nil || *util.Max != 0.98 || util.Step == nil || *util.Step != 0.01 {
		t.Fatalf("GPU utilization metadata does not match UI contract: %#v", util)
	}
	// enable_chunked_prefill and enable_prefix_caching carry no default on
	// purpose: they are engine-decided booleans, and
	// TestEngineDecidedBooleansPublishNoDefault owns that assertion.
	for _, key := range []string{"limit_mm_per_prompt", "mm_processor_kwargs", "media_io_kwargs", "allowed_local_media_path"} {
		if !vllmSpecs[key].MultimodalOnly {
			t.Fatalf("%s must be multimodal-only: %#v", key, vllmSpecs[key])
		}
	}
	sglangKeys := map[string]bool{}
	for _, spec := range sglang.ParamDefinitions {
		sglangKeys[spec.Key] = true
	}
	for _, key := range []string{"performance_mode", "enable_chunked_prefill", "kv_offloading_size", "kv_disk_offload", "optimization_level"} {
		if sglangKeys[key] {
			t.Fatalf("SGLang advertises unsupported %q", key)
		}
	}
	for _, key := range []string{"max_total_tokens", "max_mamba_cache_size"} {
		if _, ok := vllmSpecs[key]; ok {
			t.Fatalf("vLLM must not expose SGLang-only cache control %q", key)
		}
	}
	for _, key := range []string{"gpu_memory_utilization", "max_num_batched_tokens", "max_num_seqs", "enable_prefix_caching", "enforce_eager", "speculative_num_steps", "speculative_eagle_top_k"} {
		if !sglangKeys[key] {
			t.Fatalf("SGLang parameter dictionary missing %q", key)
		}
	}
	for _, forbidden := range []string{"num_gpu_blocks_override", "VLLM_USE_SIMPLE_KV_OFFLOAD"} {
		if _, ok := vllmSpecs[forbidden]; ok {
			t.Fatalf("forbidden control exposed: %s", forbidden)
		}
	}
}

func hasRequiredLocalizedText(text map[string]string) bool {
	for _, language := range requiredParameterLocales {
		if text[language] == "" {
			return false
		}
	}
	return true
}

func TestParameterLocalesCoverEveryDefinitionAndOption(t *testing.T) {
	locales, err := loadParameterLocales()
	if err != nil {
		t.Fatal(err)
	}
	if len(locales) != len(requiredParameterLocales) {
		t.Fatalf("loaded locales = %v, want %v", locales, requiredParameterLocales)
	}
	for _, definition := range parameterDefinitions {
		if !hasRequiredLocalizedText(definition.Label) {
			t.Errorf("parameter %q has incomplete labels: %#v", definition.Key, definition.Label)
		}
		for fieldName, text := range map[string]map[string]string{
			"placeholder": definition.Placeholder,
			"help":        definition.Help,
		} {
			if len(text) > 0 && !hasRequiredLocalizedText(text) {
				t.Errorf("parameter %q has incomplete %s: %#v", definition.Key, fieldName, text)
			}
		}
		for _, option := range definition.ParamDefinition.Options {
			if option.Value == "" || !hasRequiredLocalizedText(option.Label) {
				t.Errorf("parameter %q has incomplete option: %#v", definition.Key, option)
			}
		}
		// A per-engine override list is part of the profile those engines
		// publish, so its options are labeled too (by value, or by the option's
		// own spelling when only one engine has it).
		for engine, options := range definition.Options {
			for _, option := range options {
				if option.Value == "" || !hasRequiredLocalizedText(option.Label) {
					t.Errorf("parameter %q option for %s has incomplete label: %#v", definition.Key, engine, option)
				}
			}
		}
	}
}

func TestParamSpecJSONUsesMultilingualMapsAndStableOptionValues(t *testing.T) {
	profile, err := ProfileForEngine("vllm")
	if err != nil {
		t.Fatal(err)
	}
	var dtype ParamDefinition
	for _, spec := range profile.ParamDefinitions {
		if spec.Key == "dtype" {
			dtype = spec
			break
		}
	}
	if dtype.Key == "" {
		t.Fatal("dtype parameter is missing")
	}
	values := make([]string, 0, len(dtype.Options))
	for _, option := range dtype.Options {
		values = append(values, option.Value)
	}
	if want := []string{"auto", "float16", "bfloat16", "float32"}; !slices.Equal(values, want) {
		t.Fatalf("dtype option values = %v, want %v", values, want)
	}

	raw, err := json.Marshal(dtype)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Label   map[string]string `json:"label"`
		Options []struct {
			Value string            `json:"value"`
			Label map[string]string `json:"label"`
		} `json:"options"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	if !hasRequiredLocalizedText(wire.Label) || len(wire.Options) != len(dtype.Options) {
		t.Fatalf("unexpected multilingual wire contract: %s", raw)
	}
	for _, option := range wire.Options {
		if option.Value == "" || !hasRequiredLocalizedText(option.Label) {
			t.Fatalf("unexpected multilingual option contract: %s", raw)
		}
	}
}

func TestApplyParameterLocalesRejectsIncompleteContracts(t *testing.T) {
	validEntry := parameterLocaleEntry{Label: "Label", Options: map[string]string{"stable": "Stable"}}
	for name, locales := range map[string]map[string]map[string]parameterLocaleEntry{
		"missing language": {
			"en": {"example": validEntry},
		},
		"missing parameter": {
			"en": {"example": validEntry},
			"zh": {},
		},
		"unknown parameter": {
			"en": {"example": validEntry, "extra": {Label: "Extra"}},
			"zh": {"example": validEntry},
		},
		"missing option": {
			"en": {"example": validEntry},
			"zh": {"example": {Label: "Label"}},
		},
		"stale option label": {
			"en": {"example": {Label: "Label", Options: map[string]string{"stable": "Stable", "gone": "Gone"}}},
			"zh": {"example": validEntry},
		},
		"partial optional field": {
			"en": {"example": {Label: "Label", Help: "Help", Options: map[string]string{"stable": "Stable"}}},
			"zh": {"example": validEntry},
		},
	} {
		t.Run(name, func(t *testing.T) {
			definitions := []paramDefinition{{ParamDefinition: ParamDefinition{Key: "example", Options: options("stable")}}}
			if err := applyParameterLocales(definitions, locales); err == nil {
				t.Fatalf("applyParameterLocales unexpectedly accepted %#v", locales)
			}
		})
	}
}

// An option only one engine offers is labeled from the locale files too, so no
// product text lives in Go; an engine-only option the files do not label still
// shows its own value.
func TestApplyParameterLocalesLabelsEngineOnlyOptions(t *testing.T) {
	entry := func(label string) parameterLocaleEntry {
		return parameterLocaleEntry{Label: "Label", Options: map[string]string{"stable": "Stable", "FAST": label}}
	}
	definitions := []paramDefinition{{
		ParamDefinition: ParamDefinition{Key: "example", Options: options("stable")},
		Options:         map[string][]ParamOption{EngineSGLang: options("stable", "FAST", "RAW")},
	}}
	if err := applyParameterLocales(definitions, map[string]map[string]parameterLocaleEntry{
		"en": {"example": entry("Fast")}, "zh": {"example": entry("快速")},
	}); err != nil {
		t.Fatal(err)
	}
	got := definitions[0].Options[EngineSGLang]
	if got[1].Label["zh"] != "快速" || got[1].Label["en"] != "Fast" || got[2].Label["en"] != "RAW" {
		t.Fatalf("engine-only labels = %#v", got)
	}
}

func TestSleepableInjectsEnvAndFlag(t *testing.T) {
	r := BuildVLLM(Params{ModelPath: "/m", APIPort: 30000, Sleepable: true})
	if !slices.Contains(r.Argv, "--enable-sleep-mode") {
		t.Error("sleep flag missing")
	}
	if r.Env["VLLM_SERVER_DEV_MODE"] != "1" {
		t.Error("dev mode env missing")
	}
}

func TestQuirkTuringDtype(t *testing.T) {
	r := BuildVLLM(Params{ModelPath: "/m", APIPort: 30000, Vendor: "nvidia", ComputeCapability: "7.5"})
	if !argvHas(r.Argv, "--dtype", "float16") {
		t.Errorf("turing quirk should pin float16: %v", r.Argv)
	}
}

func TestQuirkTuringUsesNVIDIAComputeCapability(t *testing.T) {
	low := BuildVLLM(Params{ModelPath: "/m", APIPort: 30000, Vendor: "nvidia", ComputeCapability: "7.5"})
	if !argvHas(low.Argv, "--dtype", "float16") {
		t.Fatalf("cc 7.5 should pin float16: %v", low.Argv)
	}
	high := BuildVLLM(Params{ModelPath: "/m", APIPort: 30000, Vendor: "nvidia", ComputeCapability: "8.0"})
	if argvHas(high.Argv, "--dtype", "float16") {
		t.Fatalf("cc 8.0 should not pin dtype: %v", high.Argv)
	}
	ascend := BuildVLLM(Params{ModelPath: "/m", APIPort: 30000, Vendor: "ascend", ComputeCapability: "7.5"})
	if argvHas(ascend.Argv, "--dtype", "float16") {
		t.Fatalf("non-NVIDIA cc 7.5 should not pin dtype: %v", ascend.Argv)
	}
}

func TestQuirkDeepSeekKV(t *testing.T) {
	r := BuildVLLM(Params{ModelPath: "/m", APIPort: 30000, ModelType: "deepseek_v3"})
	if !argvHas(r.Argv, "--kv-cache-dtype", "fp8") {
		t.Errorf("deepseek quirk should force fp8 kv: %v", r.Argv)
	}
}

func TestQuirkDeepSeekKVSkippedForAscend(t *testing.T) {
	r := BuildVLLM(Params{ModelPath: "/m", APIPort: 30000, ModelType: "deepseek_v3", Vendor: "ascend", ArchID: "910B4"})
	if argvHas(r.Argv, "--kv-cache-dtype", "fp8") {
		t.Errorf("ascend should not auto-inject fp8 kv: %v", r.Argv)
	}
}

func TestAscendDoesNotOverrideUserKVCacheDtype(t *testing.T) {
	r := BuildVLLM(Params{
		ModelPath: "/m", APIPort: 30000, ModelType: "deepseek_v3", Vendor: "ascend", ArchID: "910B4",
		Quick: LaunchConfig{KVCacheDtype: "fp8"},
	})
	if !argvHas(r.Argv, "--kv-cache-dtype", "fp8") {
		t.Errorf("configured kv-cache-dtype should be preserved: %v", r.Argv)
	}
	if got := strings.Count(strings.Join(r.Argv, " "), "--kv-cache-dtype"); got != 1 {
		t.Errorf("--kv-cache-dtype appears %d times: %v", got, r.Argv)
	}
}

// A dictionary key set in the configuration is the user's explicit choice, so a
// quirk that would inject the same flag must stand down - the flag reaching the
// engine can only be the one the deployment configured.
func TestQuirkDoesNotOverrideConfiguredDictionaryValue(t *testing.T) {
	r := BuildVLLM(Params{
		ModelPath: "/m", APIPort: 30000, ModelType: "deepseek_v3",
		Quick: LaunchConfig{KVCacheDtype: "fp8_e5m2"},
	})
	if !argvHas(r.Argv, "--kv-cache-dtype", "fp8_e5m2") {
		t.Fatalf("deepseek quirk overrode the configured kv-cache-dtype: %v", r.Argv)
	}
	if got := strings.Count(strings.Join(r.Argv, " "), "--kv-cache-dtype"); got != 1 {
		t.Fatalf("--kv-cache-dtype appears %d times: %v", got, r.Argv)
	}
	turing := BuildVLLM(Params{
		ModelPath: "/m", APIPort: 30000, Vendor: "nvidia", ComputeCapability: "7.5",
		Quick: LaunchConfig{Dtype: "bfloat16"},
	})
	if !argvHas(turing.Argv, "--dtype", "bfloat16") || strings.Count(strings.Join(turing.Argv, " "), "--dtype") != 1 {
		t.Fatalf("turing quirk overrode the configured dtype: %v", turing.Argv)
	}
}

func TestQuirkDoesNotOverrideUser(t *testing.T) {
	// User pins bf16 on a Turing node; quirk must NOT override the user.
	r := BuildVLLM(Params{
		ModelPath: "/m", APIPort: 30000, Vendor: "nvidia", ComputeCapability: "7.5",
		ExtraArgs: []string{"--dtype", "bfloat16"},
	})
	if strings.Count(strings.Join(r.Argv, " "), "--dtype") != 1 {
		t.Errorf("quirk overrode user --dtype: %v", r.Argv)
	}
	if !argvHas(r.Argv, "--dtype", "bfloat16") {
		t.Error("user dtype lost")
	}
}

func TestSGLangEngineBuild(t *testing.T) {
	eng, err := SelectEngine("sglang")
	if err != nil {
		t.Fatal(err)
	}
	if eng.Profile().Sleep.Supported {
		t.Fatal("sglang v2 profile should not advertise sleep support")
	}
	r := eng.Build(Params{
		ModelPath:      "/model",
		ServedName:     "qwen3",
		APIPort:        31000,
		TensorParallel: 2,
		Quick:          LaunchConfig{EnableMetrics: true, MaxModelLen: 8192, GPUMemUtil: 0.82, KVCacheDtype: "fp8"},
		ExtraArgs:      []string{"--trust-remote-code"},
	})
	if !slices.Contains(r.Entrypoint, "sglang.launch_server") {
		t.Fatalf("expected sglang launcher, got %v", r.Entrypoint)
	}
	for _, pair := range [][2]string{
		{"--model-path", "/model"},
		{"--tp-size", "2"},
		{"--port", "31000"},
		{"--context-length", "8192"},
		{"--mem-fraction-static", "0.82"},
		{"--kv-cache-dtype", "fp8"},
	} {
		if !argvHas(r.Entrypoint, pair[0], pair[1]) {
			t.Fatalf("missing %s %s in %v", pair[0], pair[1], r.Entrypoint)
		}
	}
	if !slices.Contains(r.Entrypoint, "--trust-remote-code") {
		t.Fatalf("extra args missing: %v", r.Entrypoint)
	}
	if strings.Count(strings.Join(r.Entrypoint, " "), "--enable-metrics") != 1 {
		t.Fatalf("expected one --enable-metrics, got %v", r.Entrypoint)
	}
}

func TestSGLangUserEnableMetricsDoesNotDuplicate(t *testing.T) {
	eng, err := SelectEngine("sglang")
	if err != nil {
		t.Fatal(err)
	}
	r := eng.Build(Params{ModelPath: "/model", APIPort: 31000, ExtraArgs: []string{"--enable-metrics"}})
	if strings.Count(strings.Join(r.Entrypoint, " "), "--enable-metrics") != 1 {
		t.Fatalf("expected one --enable-metrics, got %v", r.Entrypoint)
	}
}

func TestDistributedFollowersAreHeadless(t *testing.T) {
	vllm := BuildVLLM(Params{ModelPath: "/model", ServedName: "qwen", APIPort: 30000, TensorParallel: 2, PipelineParallel: 2, NNodes: 2, NodeRank: 1, MasterAddr: "10.0.0.1", MasterPort: 29500, Headless: true, NodeAddress: "10.0.0.2"})
	if argvHas(vllm.Argv, "--port", "30000") || argvHas(vllm.Argv, "--served-model-name", "qwen") {
		t.Fatalf("headless vllm exposed API flags: %v", vllm.Argv)
	}
	if !argvHas(vllm.Argv, "--nnodes", "2") || !argvHas(vllm.Argv, "--node-rank", "1") || !slices.Contains(vllm.Argv, "--headless") {
		t.Fatalf("distributed flags missing: %v", vllm.Argv)
	}
	if !argvHas(vllm.Argv, "--pipeline-parallel-size", "2") || vllm.Env["VLLM_HOST_IP"] != "10.0.0.2" {
		t.Fatalf("vLLM multi-node layout/env mismatch: argv=%v env=%v", vllm.Argv, vllm.Env)
	}
	eng, _ := SelectEngine("sglang")
	sglang := eng.Build(Params{ModelPath: "/model", ServedName: "qwen", APIPort: 0, TensorParallel: 4, NNodes: 2, NodeRank: 1, MasterAddr: "10.0.0.1", MasterPort: 29500, Headless: true})
	if argvHas(sglang.Entrypoint, "--served-model-name", "qwen") {
		t.Fatalf("headless sglang exposed API flags: %v", sglang.Entrypoint)
	}
	if !argvHas(sglang.Entrypoint, "--port", "0") || !argvHas(sglang.Entrypoint, "--tp-size", "4") || !argvHas(sglang.Entrypoint, "--dist-init-addr", "10.0.0.1:29500") {
		t.Fatalf("SGLang multi-node follower contract mismatch: %v", sglang.Entrypoint)
	}
}

func TestVLLM0251DistributedCommandContract(t *testing.T) {
	eng, _ := SelectEngine("vllm")
	r := eng.Build(Params{
		ModelPath: "/model", TensorParallel: 2, PipelineParallel: 2, NNodes: 2, NodeRank: 1,
		MasterAddr: "10.0.0.1", MasterPort: 29500, Headless: true, NodeAddress: "10.0.0.2",
		Quick: materializedConfig(t, EngineVLLM),
	})
	if got, want := strings.Join(r.Entrypoint, " "), "vllm serve /model"; got != want {
		t.Fatalf("entrypoint = %q, want %q", got, want)
	}
	// A deployment that configured nothing carries the platform's distributed
	// layout plus the dictionary defaults an untouched form shows; 0.25.1 takes
	// the per-request metrics flag.
	want := "--tensor-parallel-size 2 --pipeline-parallel-size 2 --distributed-executor-backend mp --nnodes 2 --node-rank 1 --headless --master-addr 10.0.0.1 --master-port 29500 --enable-prompt-tokens-details --enable-per-request-metrics --gpu-memory-utilization 0.9"
	if got := strings.Join(r.Argv, " "); got != want {
		t.Fatalf("vLLM 0.25.1 argv = %q, want %q", got, want)
	}
}

func TestSGLang0515DistributedCommandContract(t *testing.T) {
	eng, _ := SelectEngine("sglang")
	r := eng.Build(Params{
		ModelPath: "/model", TensorParallel: 4, NNodes: 2, NodeRank: 1,
		MasterAddr: "10.0.0.1", MasterPort: 29500, Headless: true,
		Quick: materializedConfig(t, EngineSGLang),
	})
	want := "python -m sglang.launch_server --model-path /model --tp-size 4 --host 127.0.0.1 --port 0 --nnodes 2 --node-rank 1 --dist-init-addr 10.0.0.1:29500 --log-level info --enable-metrics --mem-fraction-static 0.88 --enable-cache-report"
	if got := strings.Join(r.Entrypoint, " "); got != want {
		t.Fatalf("SGLang 0.5.15.post1 entrypoint = %q, want %q", got, want)
	}
}

func TestSGLangProfileMapsProductionMetrics(t *testing.T) {
	eng, err := SelectEngine("sglang")
	if err != nil {
		t.Fatal(err)
	}
	m := eng.Profile().MetricMap
	for source, want := range map[string]string{
		"sglang:num_running_reqs":        "gpuworks_infer_requests_running",
		"sglang:num_queue_reqs":          "gpuworks_infer_requests_waiting",
		"sglang:token_usage":             "gpuworks_infer_gpu_cache_usage_percent",
		"sglang:prompt_tokens_total":     "gpuworks_infer_prompt_tokens_total",
		"sglang:generation_tokens_total": "gpuworks_infer_generation_tokens_total",
		"sglang:cache_hit_rate":          "gpuworks_infer_kv_hit_rate_instant",
		"sglang:spec_accept_rate":        "gpuworks_infer_spec_accept_rate",
		"sglang:spec_accept_length":      "gpuworks_infer_spec_accept_length",
	} {
		if got := m[source]; got != want {
			t.Fatalf("MetricMap[%q] = %q, want %q", source, got, want)
		}
	}
}

func TestVLLMProfileMapsProductionMetrics(t *testing.T) {
	eng, err := SelectEngine("vllm")
	if err != nil {
		t.Fatal(err)
	}
	m := eng.Profile().MetricMap
	for source, want := range map[string]string{
		"vllm:num_requests_running":                  "gpuworks_infer_requests_running",
		"vllm:num_requests_waiting":                  "gpuworks_infer_requests_waiting",
		"vllm:gpu_cache_usage_perc":                  "gpuworks_infer_gpu_cache_usage_percent",
		"vllm:kv_cache_usage_perc":                   "gpuworks_infer_gpu_cache_usage_percent",
		"vllm:prompt_tokens_total":                   "gpuworks_infer_prompt_tokens_total",
		"vllm:generation_tokens_total":               "gpuworks_infer_generation_tokens_total",
		"vllm:prefix_cache_queries_total":            "gpuworks_infer_kv_queries_total",
		"vllm:prefix_cache_hits_total":               "gpuworks_infer_kv_hits_total",
		"vllm:spec_decode_num_draft_tokens_total":    "gpuworks_infer_spec_draft_tokens_total",
		"vllm:spec_decode_num_accepted_tokens_total": "gpuworks_infer_spec_accepted_tokens_total",
		// vLLM >= 0.10 renamed time_per_output_token_seconds to
		// inter_token_latency_seconds; both must map to the same gpuworks
		// family so old and new images both feed TPOT.
		"vllm:time_per_output_token_seconds": "gpuworks_infer_tpot_seconds",
		"vllm:inter_token_latency_seconds":   "gpuworks_infer_tpot_seconds",
	} {
		if got := m[source]; got != want {
			t.Fatalf("MetricMap[%q] = %q, want %q", source, got, want)
		}
	}
}

func TestAssembleCommandModes(t *testing.T) {
	built := ResolvedLaunch{Entrypoint: []string{"vllm", "serve", "/model"}, Argv: []string{"--host", "127.0.0.1"}, Env: map[string]string{"SYSTEM": "1"}}
	image := ImageCommand{Entrypoint: []string{"/entrypoint.sh"}, Cmd: []string{"--from-image"}}
	engine := AssembleCommand(vLLMEngine{}, built, image)
	if strings.Join(engine.Entrypoint, " ") != "vllm serve /model" || engine.Env["SYSTEM"] != "1" {
		t.Fatalf("engine mode mismatch: %#v", engine)
	}
	entry := AssembleCommand(vLLMEngine{}, built, ImageCommand{Mode: CommandImageEntry, Entrypoint: image.Entrypoint, Cmd: image.Cmd})
	if strings.Join(entry.Entrypoint, " ") != "/entrypoint.sh" || strings.Join(entry.Argv, " ") != "--from-image /model --host 127.0.0.1" || entry.Env["SYSTEM"] != "1" {
		t.Fatalf("image-entry mismatch: %#v", entry)
	}
	// A manual command owns its whole environment: the engine's system layer
	// is dropped, and ResolveLaunch adds template and user values after this.
	manual := AssembleCommand(vLLMEngine{}, built, ImageCommand{Mode: CommandManual, Entrypoint: image.Entrypoint, Cmd: image.Cmd})
	if strings.Join(manual.Argv, " ") != "--from-image" || len(manual.Env) != 0 {
		t.Fatalf("manual mismatch: %#v", manual)
	}
}

func TestResolveLaunchManualTextOverrideRunsAsShellScript(t *testing.T) {
	command := "# environment\nHF_HUB_OFFLINE=1\n\n# launch\nvllm serve /model --served-model-name old/model --host 127.0.0.1 --port 30000"
	launch, err := ResolveLaunch(ResolveLaunchInput{
		Engine: launchTestEngine{launch: ResolvedLaunch{
			Entrypoint: []string{"vllm", "serve", "/model"},
			Argv:       []string{"--host", "127.0.0.1"},
		}},
		Params: Params{APIPort: 31000, ServedName: "public-model"},
		ContainerOverrides: ContainerOverrides{
			CommandMode: string(CommandManual),
			CmdDisplay:  command,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(launch.Entrypoint, " "); got != "/bin/sh" {
		t.Fatalf("entrypoint = %q, want /bin/sh", got)
	}
	wantCommand := strings.Replace(command, "--port 30000", "--port 31000", 1)
	wantCommand = strings.Replace(wantCommand, "--served-model-name old/model", "--served-model-name 'public-model'", 1)
	if len(launch.Argv) != 2 || launch.Argv[0] != "-c" || launch.Argv[1] != "set -a\n"+wantCommand {
		t.Fatalf("argv = %#v", launch.Argv)
	}
}

// The engine's own holdback is declared by its profile because the estimator
// may not know which engine it is charging (estimate.ReserveBytes takes the
// base as an input). SGLang measured 205 MiB flat - it carves its pool out of a
// fraction of the card - so neither the architecture nor the graph switch moves
// it; vLLM declares none and is charged the estimator's graph-shaped model.
func TestEnginesDeclareTheirReserveBase(t *testing.T) {
	if got := (sglangEngine{}).Profile().ReserveBase; got != 205<<20 {
		t.Fatalf("SGLang reserve base = %d, want 205 MiB", got)
	}
	if got := (vLLMEngine{}).Profile().ReserveBase; got != 0 {
		t.Fatalf("vLLM reserve base = %d, want the estimator's default model", got)
	}
}

// An engine with no dictionary of its own - a catalog-owned Serve Adapter, and
// in tests a harness engine - still has to be schedulable. Its utilization
// default is the platform's measured one; zero would make the scheduler reject
// every deployment of that engine, which is what an L3 run caught.
func TestDictionaryLessEngineKeepsASchedulableUtilization(t *testing.T) {
	want := DefaultGPUMemUtil(DefaultEngine)
	if want <= 0 {
		t.Fatalf("default engine utilization = %v, want a schedulable value", want)
	}
	for _, engine := range []string{EngineCallable, "harness-engine"} {
		if got := DefaultGPUMemUtil(engine); got != want {
			t.Fatalf("%s utilization = %v, want %v", engine, got, want)
		}
	}
}

func TestReplaceManualServedName(t *testing.T) {
	command := `vllm serve /model --served-model-name="old/model" --dtype auto`
	want := `vllm serve /model --served-model-name 'public'"'"'model' --dtype auto`
	if got := replaceManualServedName(command, "public'model"); got != want {
		t.Fatalf("replaceManualServedName() = %q, want %q", got, want)
	}
}

func TestParserParametersAreOptionalComboboxes(t *testing.T) {
	for _, engine := range []string{EngineVLLM, EngineSGLang} {
		profile, err := ProfileForEngine(engine)
		if err != nil {
			t.Fatal(err)
		}
		for _, key := range []string{"reasoning_parser", "tool_call_parser"} {
			var found *ParamDefinition
			for index := range profile.ParamDefinitions {
				if profile.ParamDefinitions[index].Key == key {
					found = &profile.ParamDefinitions[index]
					break
				}
			}
			if found == nil || found.Type != "string" || found.Control != "combobox" || found.Default != nil {
				t.Fatalf("%s %s definition = %+v", engine, key, found)
			}
		}
	}

	sglang, err := ProfileForEngine(EngineSGLang)
	if err != nil {
		t.Fatal(err)
	}
	parserDefinitions := 0
	for _, definition := range sglang.ParamDefinitions {
		if definition.Key != "reasoning_parser" && definition.Key != "tool_call_parser" {
			continue
		}
		parserDefinitions++
		if len(definition.Options) == 0 || len(definition.Enum) != len(definition.Options) {
			t.Fatalf("sglang %s suggestions are missing or inconsistent: options=%+v enum=%v", definition.Key, definition.Options, definition.Enum)
		}
	}
	if parserDefinitions != 2 {
		t.Fatalf("sglang parser definition count = %d, want 2", parserDefinitions)
	}
	if err := ValidateLaunchConfigForEngine(EngineSGLang, configured(t, EngineSGLang, func(q *LaunchConfig) {
		q.ReasoningParser, q.ToolCallParser = "future-reasoning-parser", "future-tool-parser"
	})); err != nil {
		t.Fatalf("SGLang custom parser names must remain valid: %v", err)
	}
}

func TestReplaceManualCommandPort(t *testing.T) {
	tests := []struct {
		name    string
		command string
		want    string
	}{
		{name: "separate value", command: "vllm serve /model --port 30000 --dtype auto", want: "vllm serve /model --port 31000 --dtype auto"},
		{name: "equals value", command: "sglang serve --port=30000", want: "sglang serve --port 31000"},
		{name: "unrelated text", command: "echo --portal && vllm serve /model", want: "echo --portal && vllm serve /model"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := replaceManualCommandPort(test.command, 31000); got != test.want {
				t.Fatalf("replaceManualCommandPort() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestResolveLaunchManualJSONArrayKeepsArgvContract(t *testing.T) {
	launch, err := ResolveLaunch(ResolveLaunchInput{
		Engine: launchTestEngine{launch: ResolvedLaunch{Entrypoint: []string{"engine"}}},
		ContainerOverrides: ContainerOverrides{
			CommandMode:       string(CommandManual),
			EntrypointDisplay: `["/deployment-entrypoint"]`,
			CmdDisplay:        `["serve","/model"]`,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(launch.Entrypoint, " "); got != "/deployment-entrypoint" {
		t.Fatalf("entrypoint = %q, want the deployment entrypoint", got)
	}
	if got := strings.Join(launch.Argv, " "); got != "serve /model" {
		t.Fatalf("argv = %q, want JSON array argv", got)
	}
}

func TestResolveLaunchMatchesFinalMountsAndOverrides(t *testing.T) {
	launch, err := ResolveLaunch(ResolveLaunchInput{
		Engine: launchTestEngine{launch: ResolvedLaunch{Entrypoint: []string{"vllm", "serve", "/model"}, Argv: []string{"--host", "127.0.0.1"}, Env: map[string]string{"SYSTEM": "1"}}},
		ContainerOverrides: ContainerOverrides{
			CommandMode: string(CommandImageEntry), EntrypointDisplay: `["/entry"]`,
			Env: map[string]string{"USER": "1"}, DockerArgs: []string{"--ipc=host"},
			ExtraMounts:    []ContainerMount{{Volume: "data", ContainerPath: "/data"}, {HostPath: "/srv/cache", ContainerPath: "/cache", Purpose: "kv-cache"}},
			ModelMountPath: "/override-model", CmdDisplay: `["serve","/model"]`,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(launch.Entrypoint, " "); got != "/entry" {
		t.Fatalf("entrypoint = %q", got)
	}
	if got := strings.Join(launch.Argv, " "); got != "serve /override-model" {
		t.Fatalf("argv = %q", got)
	}
	if launch.ModelMount != "/override-model" || len(launch.Mounts) != 2 || !reflect.DeepEqual(launch.DockerArgs, []string{"--ipc=host"}) {
		t.Fatalf("launch = %+v", launch)
	}
	if launch.EnvLayers.System["SYSTEM"] != "1" || launch.EnvLayers.User["USER"] != "1" || launch.Env["SYSTEM"] != "1" || launch.Env["USER"] != "1" {
		t.Fatalf("env layers = %+v env = %+v", launch.EnvLayers, launch.Env)
	}
}

func TestValidateResolvedMountsRejectsNestedTargets(t *testing.T) {
	err := validateResolvedMounts([]proto.MountSpec{{Target: "/model/frontend"}}, "/model")
	if err == nil || !strings.Contains(err.Error(), "nested mount targets") {
		t.Fatalf("nested mount validation error=%v", err)
	}
}

// env_unset removes a key whichever lower layer set it - the engine's system
// env or the template's - and the deployment's own value for a key still
// applies after the removal. The layers keep recording where values came from.
func TestResolveLaunchEnvUnsetRemovesAnyLowerLayer(t *testing.T) {
	launch, err := ResolveLaunch(ResolveLaunchInput{
		Engine: launchTestEngine{launch: ResolvedLaunch{
			Env:       map[string]string{"HF_HUB_OFFLINE": "1", "KEEP": "system"},
			EnvLayers: proto.LaunchEnvLayers{Template: map[string]string{"TEMPLATE_ONLY": "yes"}},
		}},
		ContainerOverrides: ContainerOverrides{
			EnvUnset: []string{"HF_HUB_OFFLINE", "TEMPLATE_ONLY", "REPLACED"},
			Env:      map[string]string{"REPLACED": "user"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"HF_HUB_OFFLINE", "TEMPLATE_ONLY"} {
		if _, ok := launch.Env[key]; ok {
			t.Fatalf("%s survived env_unset: %+v", key, launch.Env)
		}
	}
	if launch.Env["KEEP"] != "system" || launch.Env["REPLACED"] != "user" {
		t.Fatalf("env = %+v", launch.Env)
	}
	if launch.EnvLayers.System["HF_HUB_OFFLINE"] != "1" || launch.EnvLayers.Template["TEMPLATE_ONLY"] != "yes" {
		t.Fatalf("layers lost their provenance: %+v", launch.EnvLayers)
	}
}

// A template component's primary mount is where the weights go; an override
// may restate it but not move it.
func TestResolveLaunchComponentMountRefusesAContradictingOverride(t *testing.T) {
	engine := launchTestEngine{launch: ResolvedLaunch{Entrypoint: []string{"vllm", "serve", "/model"}}}
	launch, err := ResolveLaunch(ResolveLaunchInput{Engine: engine, ComponentModelMount: "/models/target"})
	if err != nil || launch.ModelMount != "/models/target" || strings.Join(launch.Entrypoint, " ") != "vllm serve /models/target" {
		t.Fatalf("component mount: launch=%+v err=%v", launch, err)
	}
	if _, err := ResolveLaunch(ResolveLaunchInput{Engine: engine, ComponentModelMount: "/models/target", ContainerOverrides: ContainerOverrides{ModelMountPath: "/elsewhere"}}); err == nil || !strings.Contains(err.Error(), "component primary mount") {
		t.Fatalf("contradicting override err=%v", err)
	}
	if _, err := ResolveLaunch(ResolveLaunchInput{Engine: engine, ComponentModelMount: "/models/target", ContainerOverrides: ContainerOverrides{ModelMountPath: "/models/target"}}); err != nil {
		t.Fatalf("restating the component mount: %v", err)
	}
}

func TestResolveLaunchUsesAPIContractHealthEndpoint(t *testing.T) {
	launch, err := ResolveLaunch(ResolveLaunchInput{
		Engine:      launchTestEngine{launch: ResolvedLaunch{}},
		HealthPath:  "/engine-health",
		APIContract: json.RawMessage(`{"health_endpoint":"/diffusion-health","operations":[{"type":"openai.video.generate","endpoint":"/v1/videos","mode":"async"}]}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if launch.HealthPath != "/diffusion-health" {
		t.Fatalf("health path=%q, want API contract endpoint", launch.HealthPath)
	}
	if !bytes.Equal(launch.APIContract, []byte(`{"health_endpoint":"/diffusion-health","operations":[{"type":"openai.video.generate","endpoint":"/v1/videos","mode":"async"}]}`)) {
		t.Fatalf("API contract snapshot=%s", launch.APIContract)
	}
}

func TestResolveLaunchValidatesEngineTopologyAfterVariableExpansion(t *testing.T) {
	tests := []struct {
		name    string
		engine  Engine
		params  Params
		args    []string
		wantErr string
	}{
		{
			name: "diffusion topology relation remains catalog owned", engine: sglangEngine{}, params: Params{TensorParallel: 8},
			args: []string{"--model-type", "diffusion", "--num-gpus", "8", "--tp-size", "2", "--sp-degree", "2"},
		},
		{
			name: "diffusion total differs from placement", engine: sglangEngine{}, params: Params{TensorParallel: 8},
			args:    []string{"--model-type", "diffusion", "--num-gpus", "4", "--tp-size", "2", "--sp-degree", "2"},
			wantErr: "does not match the 8 GPUs assigned",
		},
		{
			name: "vLLM TP and PP consume assigned world", engine: vLLMEngine{}, params: Params{TensorParallel: 2},
			args: []string{"--tensor-parallel-size", "1", "--pipeline-parallel-size", "2"},
		},
		{
			name: "vLLM topology under-consumes placement", engine: vLLMEngine{}, params: Params{TensorParallel: 2},
			args:    []string{"--tensor-parallel-size", "1", "--pipeline-parallel-size", "1"},
			wantErr: "requires 1 GPUs, but placement assigned 2",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			params := test.params
			params.ModelPath = "/model"
			params.TemplateArgs = test.args
			_, err := ResolveLaunch(ResolveLaunchInput{Engine: test.engine, Params: params})
			if test.wantErr == "" && err != nil {
				t.Fatal(err)
			}
			if test.wantErr != "" && (err == nil || !strings.Contains(err.Error(), test.wantErr)) {
				t.Fatalf("error=%v, want %q", err, test.wantErr)
			}
		})
	}
}

func TestWithEntrypointRetainsEngineParameters(t *testing.T) {
	for _, test := range []struct {
		name   string
		engine Engine
		built  ResolvedLaunch
		want   []string
	}{
		{
			name:   "sglang embeds parameters in entrypoint",
			engine: sglangEngine{},
			built:  ResolvedLaunch{Entrypoint: []string{"python", "-m", "sglang.launch_server", "--model-path", "/model", "--tp-size", "8"}},
			want:   []string{"--model-path", "/model", "--tp-size", "8"},
		},
		{
			name:   "vllm keeps positional model and argv",
			engine: vLLMEngine{},
			built:  ResolvedLaunch{Entrypoint: []string{"vllm", "serve", "/model"}, Argv: []string{"--tensor-parallel-size", "8"}},
			want:   []string{"/model", "--tensor-parallel-size", "8"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := WithEntrypoint(test.engine, test.built, []string{"python3", "-m", "catalog.adapter"})
			if !reflect.DeepEqual(got.Entrypoint, []string{"python3", "-m", "catalog.adapter"}) || !reflect.DeepEqual(got.Argv, test.want) {
				t.Fatalf("WithEntrypoint() = %#v, want argv %#v", got, test.want)
			}
		})
	}
}

type launchTestEngine struct{ launch ResolvedLaunch }

func (launchTestEngine) Name() string     { return "test" }
func (launchTestEngine) Profile() Profile { return Profile{Name: "test"} }
func (engine launchTestEngine) Build(Params) ResolvedLaunch {
	return engine.launch
}

// materializedConfig is the launch configuration a deployment that touched
// nothing actually carries. Engine builds receive a complete configuration in
// production, so contract tests measure argv against that rather than against a
// zero struct no write path can produce.
// boolp states a boolean explicitly. A nil field means "not stated", which is
// not the same as stating false, so a test that means false has to say so.
func boolp(value bool) *bool { return &value }

// configured is a complete launch configuration with one case's fields applied
// on top. Validation only ever sees a materialized configuration in production,
// so a fixture built as a bare literal would be missing every key that has a
// default - and an invalid-input case could then pass for a reason the case was
// not about.
func configured(t *testing.T, engine string, apply func(*LaunchConfig)) LaunchConfig {
	t.Helper()
	quick := materializedConfig(t, engine)
	if apply != nil {
		apply(&quick)
	}
	return quick
}

func materializedConfig(t *testing.T, engine string) LaunchConfig {
	t.Helper()
	out, err := Materialize(MaterializeInput{Engine: engine})
	if err != nil {
		t.Fatal(err)
	}
	return out.Quick
}

// TestExplicitConfigurationArgvGolden pins the two ends of the contract: what a
// deployment that states these parameters sends to each engine, and what one
// that states nothing sends - which is nothing at all, so an engine keeps its
// own defaults and an upgrade cannot change a running command line.
func TestExplicitConfigurationArgvGolden(t *testing.T) {
	stated := func(engine string) LaunchConfig {
		out, err := Materialize(MaterializeInput{Engine: engine, Raw: map[string]any{
			"gpu_memory_utilization": DefaultGPUMemUtil(engine),
			"enable_prefix_caching":  true,
			"enable_chunked_prefill": true,
			"enable_cache_report":    true,
		}})
		if err != nil {
			t.Fatal(err)
		}
		return out.Quick
	}

	vllm := BuildVLLM(Params{ModelPath: "/model", APIPort: 30000, TensorParallel: 1, Quick: stated(EngineVLLM)})
	joined := strings.Join(vllm.Argv, " ")
	if !argvHas(vllm.Argv, "--gpu-memory-utilization", "0.9") {
		t.Fatalf("a stated utilization must reach the engine: %v", vllm.Argv)
	}
	for _, flag := range []string{"--gpu-memory-utilization", "--enable-prefix-caching", "--enable-chunked-prefill", "--enable-prompt-tokens-details"} {
		if strings.Count(joined, flag) != 1 {
			t.Fatalf("%s must appear exactly once: %v", flag, vllm.Argv)
		}
	}
	for _, flag := range []string{"--dtype", "--kv-cache-dtype", "--max-model-len", "--max-num-seqs", "--max-num-batched-tokens"} {
		if strings.Contains(joined, flag) {
			t.Fatalf("%s must not be emitted for a key nobody stated: %v", flag, vllm.Argv)
		}
	}

	engine, _ := SelectEngine(EngineSGLang)
	sglang := engine.Build(Params{ModelPath: "/model", APIPort: 30000, TensorParallel: 1, Quick: stated(EngineSGLang)})
	sglangJoined := strings.Join(sglang.Entrypoint, " ")
	if !argvHas(sglang.Entrypoint, "--mem-fraction-static", "0.88") {
		t.Fatalf("a stated utilization must reach SGLang: %v", sglang.Entrypoint)
	}
	if strings.Count(sglangJoined, "--enable-cache-report") != 1 {
		t.Fatalf("--enable-cache-report must appear exactly once: %v", sglang.Entrypoint)
	}
	// SGLang has no positive radix-cache flag, so stating prefix caching on
	// emits nothing; stating chunked prefill is not something SGLang expresses.
	for _, flag := range []string{"--dtype", "--kv-cache-dtype", "--disable-radix-cache", "--disable-cuda-graph"} {
		if strings.Contains(sglangJoined, flag) {
			t.Fatalf("%s must not be emitted by this configuration: %v", flag, sglang.Entrypoint)
		}
	}

	// A configuration that states nothing emits the defaults an untouched form
	// shows, and nothing else: the polarity switches with no default stay
	// silent, and the enum defaults are no-ops the adapters do not spell.
	bare := BuildVLLM(Params{ModelPath: "/model", APIPort: 30000, TensorParallel: 1, Quick: materializedConfig(t, EngineVLLM)})
	bareJoined := strings.Join(bare.Argv, " ")
	bareSGLang := engine.Build(Params{ModelPath: "/model", APIPort: 30000, TensorParallel: 1, Quick: materializedConfig(t, EngineSGLang)})
	bareSGLangJoined := strings.Join(bareSGLang.Entrypoint, " ")
	if !strings.Contains(bareJoined, "--gpu-memory-utilization 0.9") || !strings.Contains(bareJoined, "--enable-prompt-tokens-details") {
		t.Fatalf("an untouched form's defaults must reach the command: %v", bare.Argv)
	}
	if !strings.Contains(bareSGLangJoined, "--mem-fraction-static 0.88") || !strings.Contains(bareSGLangJoined, "--enable-cache-report") || !strings.Contains(bareSGLangJoined, "--log-level info") {
		t.Fatalf("an untouched form's defaults must reach the command: %v", bareSGLang.Entrypoint)
	}
	for _, flag := range []string{"--enable-prefix-caching", "--enable-chunked-prefill", "--no-enable-prefix-caching", "--no-enable-chunked-prefill", "--dtype", "--kv-cache-dtype", "--performance-mode"} {
		if strings.Contains(bareJoined, flag) {
			t.Fatalf("a defaulted configuration must not emit %s: %v", flag, bare.Argv)
		}
	}
	for _, flag := range []string{"--disable-radix-cache", "--disable-cuda-graph", "--dtype", "--kv-cache-dtype"} {
		if strings.Contains(bareSGLangJoined, flag) {
			t.Fatalf("a defaulted configuration must not emit %s: %v", flag, bareSGLang.Entrypoint)
		}
	}
}

// The dictionary is the authority on what an engine can express, so a stated
// value for a key with no flag for that engine is rejected instead of being
// dropped on the floor. The exception is the neutral default: a form that
// submits the value the engine runs at anyway has stated nothing.
func TestStatedValueTheEngineCannotExpressIsRejected(t *testing.T) {
	for _, test := range []struct {
		name   string
		engine string
		quick  LaunchConfig
		want   bool // accepted
	}{
		{"sglang performance mode", EngineSGLang, LaunchConfig{PerformanceMode: "throughput"}, false},
		{"sglang neutral performance mode", EngineSGLang, LaunchConfig{PerformanceMode: "balanced"}, true},
		{"sglang memory offload tier", EngineSGLang, LaunchConfig{KVOffloadingSize: 8}, false},
		{"sglang disk offload tier", EngineSGLang, LaunchConfig{KVDiskOffload: true}, false},
		{"sglang optimization level", EngineSGLang, LaunchConfig{OptimizationLevel: "O1"}, false},
		{"sglang vllm-only json parameter", EngineSGLang, LaunchConfig{RopeScaling: `{"type":"yarn"}`}, false},
		{"vllm sglang-only parameter", EngineVLLM, LaunchConfig{SpeculativeNumSteps: 3}, false},
		{"vllm keeps its own parameters", EngineVLLM, LaunchConfig{PerformanceMode: "throughput"}, true},
		{"sglang keeps its own parameters", EngineSGLang, LaunchConfig{SpeculativeNumSteps: 3}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := ValidateLaunchConfigForEngine(test.engine, test.quick)
			if test.want && err != nil {
				t.Fatalf("want accepted, got %v", err)
			}
			if !test.want && err == nil {
				t.Fatal("want rejected, got accepted")
			}
		})
	}
}

// The collector's NIXL fixture (internal/agent/collector/nixl_metrics_test.go)
// replays these exact entries; keep the two in step.
func TestVLLMMapsNIXLTransferMetrics(t *testing.T) {
	p, err := ProfileForEngine(EngineVLLM)
	if err != nil {
		t.Fatal(err)
	}
	for src, dst := range map[string]string{
		"vllm:nixl_xfer_time_seconds":          "gpuworks_infer_nixl_transfer_seconds",
		"vllm:nixl_bytes_transferred":          "gpuworks_infer_nixl_transfer_bytes",
		"vllm:nixl_num_failed_transfers_total": "gpuworks_infer_nixl_failed_transfers_total",
		"vllm:nixl_num_kv_expired_reqs_total":  "gpuworks_infer_nixl_kv_expired_total",
	} {
		if got := p.MetricMap[src]; got != dst {
			t.Fatalf("MetricMap[%q] = %q, want %q", src, got, dst)
		}
	}
}

// macOS names its loopback lo0; vLLM's engine core dies joining its gloo group
// on "lo", and a Mac has no NCCL.
func TestQuirkAppleLoopback(t *testing.T) {
	mac := BuildVLLM(Params{ModelPath: "/m", APIPort: 30000, Vendor: "apple", ArchID: "M1 Max"})
	if mac.Env["GLOO_SOCKET_IFNAME"] != "lo0" {
		t.Fatalf("GLOO_SOCKET_IFNAME = %q on a Mac", mac.Env["GLOO_SOCKET_IFNAME"])
	}
	if _, ok := mac.Env["NCCL_SOCKET_IFNAME"]; ok {
		t.Fatalf("NCCL_SOCKET_IFNAME set on a Mac: %v", mac.Env)
	}
	linux := BuildVLLM(Params{ModelPath: "/m", APIPort: 30000, Vendor: "nvidia"})
	if linux.Env["GLOO_SOCKET_IFNAME"] != "lo" || linux.Env["NCCL_SOCKET_IFNAME"] != "lo" {
		t.Fatalf("Linux loopback changed: %v", linux.Env)
	}
}
