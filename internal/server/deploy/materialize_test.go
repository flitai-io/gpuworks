package deploy

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/messages"
)

// The tests in this file are the acceptance baseline for ADR 0020: one
// dictionary is the only source of defaults, every deployment carries a
// complete launch configuration, and argv only ever holds flags the
// dictionary does not know.

// A request that states nothing states the dictionary's defaults: that is the
// contract that let the deploy form stop sending the values its controls
// display. Defaults are the floor, never a source that beats a stated value,
// and only a stored row keeps the older behaviour of inventing nothing.
func TestMaterializeLeavesUnstatedKeysUnset(t *testing.T) {
	out, err := Materialize(MaterializeInput{Engine: EngineVLLM})
	if err != nil {
		t.Fatal(err)
	}
	q := out.Quick
	if q.GPUMemUtil != 0.9 || q.Dtype != "auto" || q.KVCacheDtype != "auto" || q.PerformanceMode != "balanced" || q.EnableCacheReport == nil || !*q.EnableCacheReport {
		t.Fatalf("dictionary defaults must be stated for a request: %+v", q)
	}
	if q.EnableChunkedPrefill != nil || q.EnablePrefixCaching != nil {
		t.Fatalf("rows without a default must stay unset: chunked=%v prefix=%v", q.EnableChunkedPrefill, q.EnablePrefixCaching)
	}
	if q.EnforceEager != nil || q.EnableMetrics || q.MaxModelLen != 0 {
		t.Fatalf("rows whose default is silence or automatic must stay unset: %+v", q)
	}
	// Tier companions stay unset while their tier is off, even though they have
	// defaults: the defaults ride with the tier, not on their own.
	if q.KVOffloadReadThreads != 0 || q.KVOffloadWriteThreads != 0 || q.KVDiskOffload {
		t.Fatalf("disk tier companions woke up with their defaults: %+v", q)
	}
	// A stored row is the other half of the contract: re-materializing it
	// invents nothing, so an upgrade cannot change a running command.
	stored, err := materialize(MaterializeInput{Engine: EngineVLLM}, true)
	if err != nil {
		t.Fatal(err)
	}
	if encoded, err := json.Marshal(stored.Quick); err != nil || string(encoded) != "{}" {
		t.Fatalf("a stored row that states nothing must serialize to nothing, got %s (%v)", encoded, err)
	}
	// The per-engine defaults still exist: they are what the estimator resolves
	// an unset utilization through, and what each engine runs at unprompted.
	if DefaultGPUMemUtil(EngineVLLM) != 0.9 {
		t.Fatalf("vllm default=%v, want 0.9", DefaultGPUMemUtil(EngineVLLM))
	}
	if DefaultGPUMemUtil(EngineSGLang) != 0.88 {
		t.Fatalf("sglang default=%v, want 0.88", DefaultGPUMemUtil(EngineSGLang))
	}
}

func TestMaterializeSplitsTemplateArgsIntoFieldsAndResidual(t *testing.T) {
	template := []string{
		"--trust-remote-code",
		"--kv-cache-dtype", "fp8",
		"--max-model-len", "200000",
		"--gpu-memory-utilization", "0.95",
		"--max-num-seqs", "16",
		"--no-enable-flashinfer-autotune",
		"--compilation-config", `{"mode":0,"cudagraph_mode":"FULL_DECODE_ONLY"}`,
	}
	out, err := Materialize(MaterializeInput{Engine: EngineVLLM, TemplateArgs: template})
	if err != nil {
		t.Fatal(err)
	}
	q := out.Quick
	if !q.TrustRemote || q.KVCacheDtype != "fp8" || q.MaxModelLen != 200000 || q.GPUMemUtil != 0.95 || q.MaxNumSeqs != 16 {
		t.Fatalf("template values did not reach the fields: %+v", q)
	}
	wantResidual := []string{"--no-enable-flashinfer-autotune", "--compilation-config", `{"mode":0,"cudagraph_mode":"FULL_DECODE_ONLY"}`}
	if !reflect.DeepEqual(out.TemplateResidual, wantResidual) {
		t.Fatalf("template residual=%#v, want %#v", out.TemplateResidual, wantResidual)
	}
	if len(out.Moved) != 0 {
		t.Fatalf("a template split is not a move: %+v", out.Moved)
	}
	for key, want := range map[string]any{"kv_cache_dtype": "fp8", "max_model_len": int64(200000), "gpu_memory_utilization": 0.95, "max_num_seqs": int64(16), "trust_remote_code": true} {
		if got, ok := out.TemplateParams[key]; !ok || fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("TemplateParams[%s]=%v (%T), want %v", key, got, got, want)
		}
	}
	if out.Quick.GPUMemUtil != 0.95 {
		t.Errorf("a template-provided key is the configuration's value: %v", out.Quick.GPUMemUtil)
	}
}

func TestMaterializeMovesExtraArgsIntoFieldsWithAliases(t *testing.T) {
	extra := []string{"--max-model-len=8192", "--dtype", "half", "--enforce-eager", "--foo", "bar"}
	out, err := Materialize(MaterializeInput{Engine: EngineVLLM, ExtraArgs: extra})
	if err != nil {
		t.Fatal(err)
	}
	q := out.Quick
	if q.MaxModelLen != 8192 || q.Dtype != "float16" || q.EnforceEager == nil || !*q.EnforceEager {
		t.Fatalf("extra_args values did not reach the fields (alias half→float16 expected): %+v", q)
	}
	if !reflect.DeepEqual(out.UserResidual, []string{"--foo", "bar"}) {
		t.Fatalf("user residual=%#v, want [--foo bar]", out.UserResidual)
	}
	moved := map[string]MovedFlag{}
	for _, m := range out.Moved {
		moved[m.Key] = m
	}
	if len(moved) != 3 {
		t.Fatalf("moved=%+v, want three entries", out.Moved)
	}
	if m := moved["max_model_len"]; m.Flag != "--max-model-len" || fmt.Sprint(m.Value) != "8192" || m.Source != "extra_args" {
		t.Errorf("moved max_model_len=%+v", m)
	}
	if m := moved["dtype"]; fmt.Sprint(m.Value) != "float16" {
		t.Errorf("moved dtype must carry the canonical value, got %+v", m)
	}
	if m := moved["enforce_eager"]; fmt.Sprint(m.Value) != "true" {
		t.Errorf("moved enforce_eager=%+v", m)
	}
}

func TestMaterializeRejectsFieldAndExtraArgsConflict(t *testing.T) {
	_, err := Materialize(MaterializeInput{
		Engine:    EngineVLLM,
		Raw:       map[string]any{"max_model_len": 8192},
		ExtraArgs: []string{"--max-model-len", "16384"},
	})
	var product *messages.ProductError
	if !errors.As(err, &product) {
		t.Fatalf("conflict must be a ProductError, got %v", err)
	}
	if product.Code() != ErrorLaunchConfigConflict {
		t.Fatalf("code=%q, want %q", product.Code(), ErrorLaunchConfigConflict)
	}
	params := product.Params()
	if params["key"] != "max_model_len" || params["flag"] != "--max-model-len" || fmt.Sprint(params["field_value"]) != "8192" || fmt.Sprint(params["extra_value"]) != "16384" {
		t.Fatalf("conflict params=%v, want key/flag and both values", params)
	}

	same, err := Materialize(MaterializeInput{
		Engine:    EngineVLLM,
		Raw:       map[string]any{"max_model_len": 8192},
		ExtraArgs: []string{"--max-model-len", "8192", "--foo"},
	})
	if err != nil {
		t.Fatalf("same value on both sides is not a conflict: %v", err)
	}
	if same.Quick.MaxModelLen != 8192 || !reflect.DeepEqual(same.UserResidual, []string{"--foo"}) {
		t.Fatalf("duplicate must be dropped from residual: %+v residual=%v", same.Quick, same.UserResidual)
	}
}

func TestMaterializePrecedenceFieldOverExtraOverTemplate(t *testing.T) {
	out, err := Materialize(MaterializeInput{
		Engine:       EngineVLLM,
		Raw:          map[string]any{"gpu_memory_utilization": 0.9},
		TemplateArgs: []string{"--gpu-memory-utilization", "0.95", "--max-num-seqs", "16"},
		ExtraArgs:    []string{"--max-num-seqs", "32"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Quick.GPUMemUtil != 0.9 {
		t.Fatalf("an explicit field beats the template without error, got util=%v", out.Quick.GPUMemUtil)
	}
	if out.Quick.MaxNumSeqs != 32 {
		t.Fatalf("extra_args beats the template, got max_num_seqs=%d", out.Quick.MaxNumSeqs)
	}
	if len(out.TemplateResidual) != 0 || len(out.UserResidual) != 0 {
		t.Fatalf("every flag was dictionary-owned; residuals must be empty: %v %v", out.TemplateResidual, out.UserResidual)
	}
	if fmt.Sprint(out.TemplateParams["gpu_memory_utilization"]) != "0.95" {
		t.Fatalf("TemplateParams still reports what the template said: %v", out.TemplateParams)
	}
}

func TestSGLangCacheLimitsRemainUnpersistedArgs(t *testing.T) {
	args := []string{"--max-total-tokens", "8192", "--max-mamba-cache-size", "16"}
	out, err := Materialize(MaterializeInput{Engine: EngineSGLang, ExtraArgs: args})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(out.UserResidual, args) {
		t.Fatalf("cache args were not retained: %v", out.UserResidual)
	}
	data, err := json.Marshal(out.Quick)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "max_total_tokens") || strings.Contains(string(data), "max_mamba_cache_size") {
		t.Fatalf("ephemeral cache limits persisted: %s", data)
	}
}

func TestReverseArgsHonorsBooleanPolarityPerEngine(t *testing.T) {
	sg, residual, err := ReverseArgs(EngineSGLang, []string{"--disable-radix-cache", "--disable-cuda-graph", "--enable-metrics"})
	if err != nil {
		t.Fatal(err)
	}
	if sg["enable_prefix_caching"] != false || sg["enforce_eager"] != true || sg["enable_metrics"] != true || len(residual) != 0 {
		t.Fatalf("sglang polarity: params=%v residual=%v", sg, residual)
	}
	vl, residual, err := ReverseArgs(EngineVLLM, []string{"--no-enable-prefix-caching", "--no-enable-chunked-prefill", "--enforce-eager"})
	if err != nil {
		t.Fatal(err)
	}
	if vl["enable_prefix_caching"] != false || vl["enable_chunked_prefill"] != false || vl["enforce_eager"] != true || len(residual) != 0 {
		t.Fatalf("vllm polarity: params=%v residual=%v", vl, residual)
	}
	on, _, err := ReverseArgs(EngineVLLM, []string{"--enable-prefix-caching"})
	if err != nil || on["enable_prefix_caching"] != true {
		t.Fatalf("vllm on-form: params=%v err=%v", on, err)
	}
}

// roundTripKeys are the dictionary keys whose engine flag is a plain
// "flag value" / bare flag. Composite flags (--speculative-config,
// --kv-transfer-config, -O<level>) are intentionally lossy and excluded.
var roundTripSamples = map[string]any{
	"log_level":                    "error",
	"max_model_len":                int64(4096),
	"dtype":                        "bfloat16",
	"kv_cache_dtype":               "fp8_e5m2",
	"gpu_memory_utilization":       0.85,
	"max_num_seqs":                 int64(64),
	"max_num_batched_tokens":       int64(8192),
	"quantization":                 "awq",
	"trust_remote_code":            true,
	"enforce_eager":                true,
	"reasoning_parser":             "qwen3",
	"tool_call_parser":             "hermes",
	"enable_auto_tool_choice":      true,
	"data_parallel_size":           int64(2),
	"lora":                         true,
	"enable_metrics":               true,
	"enable_cache_report":          true,
	"cpu_offload_gb":               4.0,
	"stream_interval":              int64(3),
	"watermark":                    0.05,
	"max_cudagraph_capture_size":   int64(256),
	"mm_encoder_tp_mode":           "data",
	"api_server_count":             int64(2),
	"async_scheduling":             true,
	"prefix_caching_hash_algo":     "sha256",
	"long_prefill_token_threshold": int64(2048),
	"kv_cache_memory_bytes":        int64(1 << 30),
}

func TestDictionaryRoundTripThroughEngineArgv(t *testing.T) {
	for _, engine := range []string{EngineVLLM, EngineSGLang} {
		profile, err := ProfileForEngine(engine)
		if err != nil {
			t.Fatal(err)
		}
		for key, sample := range roundTripSamples {
			flag, known := profile.ParamDict[key]
			if !known || flag == "" {
				continue // the engine has no flag for this key
			}
			t.Run(engine+"/"+key, func(t *testing.T) {
				raw, _ := json.Marshal(map[string]any{key: sample})
				var quick LaunchConfig
				if err := json.Unmarshal(raw, &quick); err != nil {
					t.Fatal(err)
				}
				eng, _ := SelectEngine(engine)
				built := eng.Build(Params{ModelPath: "/m", APIPort: 30000, TensorParallel: 1, AssignedGPUs: 1, Quick: quick})
				argv := append(append([]string(nil), built.Entrypoint...), built.Argv...)
				params, _, err := ReverseArgs(engine, argv)
				if err != nil {
					t.Fatal(err)
				}
				got, ok := params[key]
				if !ok {
					t.Fatalf("%s emitted %v but ReverseArgs did not recover %q", engine, argv, key)
				}
				if fmt.Sprint(got) != fmt.Sprint(sample) {
					t.Fatalf("round trip %s: got %v (%T), want %v (%T); argv=%v", key, got, got, sample, sample, argv)
				}
			})
		}
	}
}

func TestNormalizeStoredLaunchConfigIsIdempotent(t *testing.T) {
	legacy := StoredLaunchConfig{
		Engine:          EngineVLLM,
		QuickParamsJSON: `{"max_model_len":4096}`,
		ExtraArgsText:   "--max-model-len 8192 --foo",
		TemplateArgs:    []string{"--dtype", "bfloat16", "--kv-cache-dtype", "fp8", "--bar"},
	}
	first, changed, err := NormalizeStoredLaunchConfig(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("a legacy row must be reported as changed")
	}
	var quick map[string]any
	if err := json.Unmarshal([]byte(first.QuickParamsJSON), &quick); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(quick["max_model_len"]) != "8192" {
		t.Fatalf("legacy precedence keeps extra_args winning, got %v", quick["max_model_len"])
	}
	if quick["dtype"] != "bfloat16" || quick["kv_cache_dtype"] != "fp8" {
		t.Fatalf("template split missing: %v", quick)
	}
	// Normalization restructures; it never fills. A row that never stated a
	// utilization must still not state one, or the upgrade would change the
	// command line of a deployment that is already running.
	if _, filled := quick["gpu_memory_utilization"]; filled {
		t.Fatalf("normalization invented a utilization: %v", quick)
	}
	if first.ExtraArgsText != "--foo" {
		t.Fatalf("user residual=%q, want --foo", first.ExtraArgsText)
	}
	if !reflect.DeepEqual(first.TemplateArgs, []string{"--bar"}) {
		t.Fatalf("template residual=%v, want [--bar]", first.TemplateArgs)
	}
	second, changed, err := NormalizeStoredLaunchConfig(first)
	if err != nil {
		t.Fatal(err)
	}
	if changed || !reflect.DeepEqual(second, first) {
		t.Fatalf("second pass must be a no-op: changed=%v first=%+v second=%+v", changed, first, second)
	}
}
