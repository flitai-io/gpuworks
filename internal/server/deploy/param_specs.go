package deploy

import (
	"embed"
	"fmt"
	"slices"

	"gpuworks.ai/gpuworks/pkg/localization"
)

var requiredParameterLocales = []string{localization.English, localization.Chinese}

//go:embed locales/*.json
var parameterLocaleFiles embed.FS

type parameterLocaleEntry struct {
	Label       string            `json:"label"`
	Placeholder string            `json:"placeholder,omitempty"`
	Help        string            `json:"help,omitempty"`
	Options     map[string]string `json:"options,omitempty"`
}

// ParamOption gives the UI a stable value and server-owned display label.
type ParamOption struct {
	Value string            `json:"value"`
	Label localization.Text `json:"label"`
}

// ParamDefinition is the structured parameter dictionary consumed by the deploy UI.
// ParamDict and Enum remain wire-compatible aliases for older clients.
type ParamDefinition struct {
	Key  string `json:"key"`
	Flag string `json:"flag"`
	// FlagOff is the engine's negative form of a boolean parameter, when it has
	// one (vLLM's --no-enable-prefix-caching, SGLang's --disable-radix-cache).
	// Empty means the engine expresses that polarity by emitting nothing.
	FlagOff string `json:"flag_off,omitempty"`
	// Aliases maps accepted spellings to the canonical value, so a catalog that
	// writes `--dtype half` and a form that submits `float16` are one value.
	Aliases        map[string]string `json:"aliases,omitempty"`
	Label          localization.Text `json:"label"`
	Type           string            `json:"type"`
	Control        string            `json:"control,omitempty"`
	Group          string            `json:"group,omitempty"`
	Enum           []string          `json:"enum,omitempty"`
	Options        []ParamOption     `json:"options,omitempty"`
	Default        any               `json:"default,omitempty"`
	Min            *float64          `json:"min,omitempty"`
	Max            *float64          `json:"max,omitempty"`
	Step           *float64          `json:"step,omitempty"`
	Placeholder    localization.Text `json:"placeholder,omitempty"`
	Help           localization.Text `json:"help,omitempty"`
	Engines        []string          `json:"engines,omitempty"`
	Advanced       bool              `json:"advanced,omitempty"`
	Automatic      bool              `json:"automatic,omitempty"`
	MultimodalOnly bool              `json:"multimodal_only,omitempty"`
	// DependsOn names the boolean/tier key that makes this parameter effective.
	// The UI uses it to preserve a dependent value while omitting a disabled
	// layer; validation remains authoritative on the server.
	DependsOn string `json:"depends_on,omitempty"`
	// Layout says where the deploy form shows the parameter: "quick" is the
	// shortcut form's main surface, "expert" the folded advanced list, and
	// "hidden" a row the form must not render at all. Empty reads as "expert";
	// the form may not invent a placement the dictionary does not state.
	Layout string `json:"layout,omitempty"`
	// Order sequences the rows inside a layout, so the form renders the
	// surface in the order the product intends instead of a key list of its
	// own. Rows without an order keep the table's order.
	Order int `json:"order,omitempty"`
}

// flagPair is one engine's spelling of a parameter: the "on" form that carries
// the value (for a boolean, the flag that turns it on) and the "off" form. An
// empty side means the engine says nothing for that polarity.
//
// Composed marks a spelling the engine adapter writes itself, because the
// generic "flag value" pair is not what the engine wants: one flag carries
// several keys at once (vLLM's --speculative-config takes three keys and
// --kv-transfer-config four, as one JSON object) or the value is glued into the
// token (-O3). The dictionary still owns the spelling and the reverse mapping -
// only emission stays with the adapter, which is why a stated value for a
// composed key is never emitted generically.
type flagPair struct {
	On, Off  string
	Composed bool
}

// paramDefinition is one row of the parameter dictionary: the wire-visible
// ParamDefinition plus the per-engine columns the server needs to translate a
// value into argv and back. This table is the only place a default, an alias,
// an enum, a bound or a key-to-flag mapping is written down.
type paramDefinition struct {
	ParamDefinition
	// Platform marks a row the platform itself consumes. It has no engine flag
	// by construction and never reaches argv, the command preview or
	// ReverseArgs (ADR 0021); the engines it applies to are listed in the
	// embedded ParamDefinition.Engines instead of being read off a flag column.
	Platform bool
	// Flags is where each engine's spellings live, keyed by engine name. A row
	// with no entry for an engine has no flag there, and the engine cannot
	// express that parameter at all (ADR 0021) - which is exactly what makes
	// engineParams omit it. Adding an engine is adding a column to the rows it
	// supports, never editing a reader.
	Flags map[string]flagPair
	// Options overrides the row's own ParamDefinition.Options for the named
	// engines: SGLang accepts a smaller KV cache dtype set and ships its own
	// parser vocabulary. The row's own list is what every other engine
	// publishes, and the only list the locale files are validated against.
	Options map[string][]ParamOption
	// Aliases are accepted spellings mapped to the canonical value.
	Aliases map[string]string
	// DefaultByEngine overrides Default for the named engines.
	DefaultByEngine map[string]any
	// ClosedFor names the engines where a free-text row with options is a
	// closed list (an enum picked from a select) instead of a combobox: a
	// value none of the options names is refused there rather than passed.
	ClosedFor []string
}

func number(value float64) *float64 { return &value }

func options(values ...string) []ParamOption {
	out := make([]ParamOption, 0, len(values))
	for _, value := range values {
		out = append(out, ParamOption{Value: value})
	}
	return out
}

// vllmKVCacheDtypes is vLLM's CacheDType (vllm/config/cache.py) plus the
// Ascend extension's int8 and mxfp8 caches - mxfp8 is what the Ascend 950
// recipes ask for, and it exists only on that card's kernel package. The engine
// decides which of them a model, accelerator and attention backend can run.
var vllmKVCacheDtypes = []string{
	"auto", "float16", "bfloat16", "fp8", "fp8_e4m3", "fp8_e5m2", "fp8_inc", "fp8_per_token_head", "fp8_ds_mla",
	"nvfp4", "nvfp4_4over6", "nvfp4_ds_mla", "int8", "int8_per_token_head", "int4_per_token_head", "mxfp8",
	"turboquant_k8v4", "turboquant_4bit_nc", "turboquant_k3v4_nc", "turboquant_3bit_nc", "ultraquant_4bit",
}

// sglangKVCacheDtypes is SGLang's --kv-cache-dtype choice list. Its "bf16" is
// a second spelling of "bfloat16", not a separate option.
var sglangKVCacheDtypes = options("auto", "bfloat16", "fp8_e4m3", "fp8_e5m2", "mxfp8", "nvfp4", "fp4_mx_block16")

// Tool-call and reasoning parser names the engines register, as of the
// runner images' versions (vLLM 0.30.0 and 0.31.0: vllm/tool_parsers and
// vllm/reasoning; SGLang 0.5.21: function_call/parser_names.py and
// parser/reasoning_parser_names.py). They are suggestions for the form's
// combobox, not a gate: any other name, typed or in a template, is passed to
// the engine as written. Refresh them when a runner image moves to a newer
// engine.
var (
	vllmToolCallParsers    = []string{"apertus", "cohere_command3", "cohere_command4", "deepseek_v3", "deepseek_v31", "deepseek_v32", "deepseek_v4", "deepseek_v41", "dots", "ernie45", "functiongemma", "gemma4", "gigachat3", "glm45", "glm47", "granite", "granite-20b-fc", "granite4", "hermes", "hunyuan_a13b", "hy_v3", "hy_v4", "inkling", "internlm", "jamba", "k2_horizon", "kimi_k2", "kimi_k3", "lfm2", "ling3", "llama3_json", "llama4_json", "llama4_pythonic", "longcat", "mimo", "minicpm5", "minimax_m2", "minimax_m3", "mistral", "muse_glimmer", "olmo3", "openai", "phi4_mini_json", "poolside_v1", "pythonic", "qwen3_coder", "qwen3_xml", "seed_oss", "step3", "step3p5", "xlam"}
	vllmReasoningParsers   = []string{"cohere_command3", "cohere_command4", "deepseek_r1", "deepseek_v3", "deepseek_v4", "deepseek_v41", "ernie45", "gemma4", "glm45", "glm47", "granite", "granite_thinking_parser", "holo2", "hunyuan_a13b", "hy_v3", "hy_v4", "inkling", "k2_horizon", "kimi_k2", "kimi_k3", "ling3", "mimo", "minimax_m2", "minimax_m2_append_think", "minimax_m3", "mistral", "muse_glimmer", "nemotron_v3", "olmo3", "openai_gptoss", "poolside_v1", "qwen3", "seed_oss", "step3", "step3p5"}
	sglangToolCallParsers  = []string{"apertus2509", "cohere_command4", "deepseekv3", "deepseekv31", "deepseekv32", "deepseekv4", "deepseekv41", "dots", "gemma4", "gigachat3", "gigachat35", "glm", "glm45", "glm47", "gpt-oss", "hermes", "hunyuan", "inkling", "interns1", "iquest_q1", "k2_horizon", "kimi_k2", "kimi_k3", "lfm2", "ling3", "llama3", "mimo", "minicpm5", "minimax-m2", "minimax-m3", "mistral", "muse", "nanbeige", "poolside_v1", "pythonic", "qwen", "qwen25", "qwen3_coder", "spark25", "step3", "step3p5", "trinity"}
	sglangReasoningParsers = []string{"apertus2509", "cohere_command4", "deepseek-r1", "deepseek-v3", "deepseek-v4", "deepseek-v41", "dots", "gemma4", "gigachat35", "glm45", "gpt-oss", "granite_thinking_parser", "hunyuan", "inkling", "interns1", "iquest_q1", "k2_horizon", "kimi", "kimi_k2", "kimi_k3", "ling3", "mimo", "minimax", "minimax-append-think", "minimax-m3", "mistral", "muse", "nanbeige", "nemotron_3", "poolside_v1", "qwen3", "qwen3-thinking", "step3", "step3p5"}
)

// sglangSpeculativeAlgorithms is SGLang's own vocabulary, spelled as SGLang
// spells it: its model hooks compare the raw --speculative-algorithm string
// against upper-case names before normalizing it, so "dspark" fails an
// assertion that "DSPARK" passes. MTP is NEXTN there and a separate draft
// model is STANDALONE; vLLM's method names are not SGLang algorithms. The list
// stays closed for SGLang (ClosedFor): a stored configuration renames vLLM's
// names to these (storedValueRenames), which only holds while a value outside
// the list is never read into the field.
var sglangSpeculativeAlgorithms = options("EAGLE", "EAGLE3", "NEXTN", "DSPARK", "NGRAM", "STANDALONE")

// vllmSpeculativeMethods are suggestions for the form's combobox, like the
// parser lists above, not a gate: any other method, typed or in a template,
// reaches --speculative-config as written, and whether the engine in the
// chosen image knows it is the deployer's to check. dflash and dspark need a
// draft the deployment mounts (an extra mount of a registered model) unless
// the checkpoint fuses its own draft module.
var vllmSpeculativeMethods = []string{"eagle", "eagle3", "mtp", "ngram", "draft_model", "dflash", "dspark"}

func technicalOptions(values ...string) []ParamOption {
	out := options(values...)
	for i := range out {
		out[i].Label = localization.Text{
			localization.English: out[i].Value,
			localization.Chinese: out[i].Value,
		}
	}
	return out
}

var parameterDefinitions = []paramDefinition{
	{ParamDefinition: ParamDefinition{Key: "log_level", Type: "enum", Control: "select", Group: "performance", Default: "info", Options: technicalOptions("debug", "info", "warning", "error", "critical", "notset"), Layout: "quick", Order: 36}, Flags: map[string]flagPair{EngineSGLang: {On: "--log-level"}}, Aliases: map[string]string{"warn": "warning", "fatal": "critical"}},
	// Defaults on where the server takes it: SGLang's diffusion server only
	// from v0.5.21 (sglangDiffusionFlagsSince).
	{ParamDefinition: ParamDefinition{Key: "enable_metrics", Type: "boolean", Control: "switch", Group: "performance", Default: true, Layout: "quick", Order: 37}, Flags: map[string]flagPair{EngineSGLang: {On: "--enable-metrics"}}},
	{ParamDefinition: ParamDefinition{Key: "max_model_len", Type: "integer", Control: "number", Group: "model", Automatic: true, Layout: "quick", Order: 10}, Flags: map[string]flagPair{EngineVLLM: {On: "--max-model-len"}, EngineSGLang: {On: "--context-length"}}},
	{ParamDefinition: ParamDefinition{Key: "dtype", Type: "enum", Control: "select", Group: "model", Default: "auto", Options: options("auto", "float16", "bfloat16", "float32")}, Flags: map[string]flagPair{EngineVLLM: {On: "--dtype"}, EngineSGLang: {On: "--dtype"}}, Aliases: map[string]string{"half": "float16", "float": "float32"}},
	{ParamDefinition: ParamDefinition{Key: "quantization", Type: "string", Control: "text", Group: "model"}, Flags: map[string]flagPair{EngineVLLM: {On: "--quantization"}, EngineSGLang: {On: "--quantization"}}},
	{ParamDefinition: ParamDefinition{Key: "rope_scaling", Type: "json", Control: "json", Group: "model"}, Flags: map[string]flagPair{EngineVLLM: {On: "--rope-scaling"}}},
	{ParamDefinition: ParamDefinition{Key: "trust_remote_code", Type: "boolean", Control: "switch", Group: "model", Default: false}, Flags: map[string]flagPair{EngineVLLM: {On: "--trust-remote-code"}, EngineSGLang: {On: "--trust-remote-code"}}},
	{ParamDefinition: ParamDefinition{Key: "reasoning_parser", Type: "string", Control: "combobox", Group: "model", Options: technicalOptions(vllmReasoningParsers...), Layout: "quick", Order: 31}, Flags: map[string]flagPair{EngineVLLM: {On: "--reasoning-parser"}, EngineSGLang: {On: "--reasoning-parser"}}, Options: map[string][]ParamOption{EngineSGLang: technicalOptions(sglangReasoningParsers...)}},
	{ParamDefinition: ParamDefinition{Key: "tool_call_parser", Type: "string", Control: "combobox", Group: "model", Options: technicalOptions(vllmToolCallParsers...), Layout: "quick", Order: 30}, Flags: map[string]flagPair{EngineVLLM: {On: "--tool-call-parser"}, EngineSGLang: {On: "--tool-call-parser"}}, Options: map[string][]ParamOption{EngineSGLang: technicalOptions(sglangToolCallParsers...)}},
	{ParamDefinition: ParamDefinition{Key: "enable_auto_tool_choice", Type: "boolean", Control: "switch", Group: "model", Default: false}, Flags: map[string]flagPair{EngineVLLM: {On: "--enable-auto-tool-choice"}, EngineSGLang: {On: "--enable-auto-tool-choice"}}},
	{ParamDefinition: ParamDefinition{Key: "limit_mm_per_prompt", Type: "json", Control: "json", Group: "multimodal", MultimodalOnly: true}, Flags: map[string]flagPair{EngineVLLM: {On: "--limit-mm-per-prompt"}}},
	{ParamDefinition: ParamDefinition{Key: "mm_processor_kwargs", Type: "json", Control: "json", Group: "multimodal", MultimodalOnly: true}, Flags: map[string]flagPair{EngineVLLM: {On: "--mm-processor-kwargs"}}},
	{ParamDefinition: ParamDefinition{Key: "media_io_kwargs", Type: "json", Control: "json", Group: "multimodal", MultimodalOnly: true}, Flags: map[string]flagPair{EngineVLLM: {On: "--media-io-kwargs"}}},
	{ParamDefinition: ParamDefinition{Key: "allowed_local_media_path", Type: "string", Control: "path", Group: "multimodal", MultimodalOnly: true}, Flags: map[string]flagPair{EngineVLLM: {On: "--allowed-local-media-path"}}},

	{ParamDefinition: ParamDefinition{Key: "performance_mode", Type: "enum", Control: "segmented", Group: "performance", Default: "balanced", Options: options("interactivity", "balanced", "throughput")}, Flags: map[string]flagPair{EngineVLLM: {On: "--performance-mode"}}},
	// The floor is one step rather than a tuning recommendation: a share this
	// low wastes the card, but it is the deployer's card to waste - leaving room
	// for another process, or starting an engine at all on a card something else
	// already holds. Zero and below are refused because the parameter cannot mean
	// them, and the ceiling stays where it is because the engine needs the
	// remainder for its own allocations.
	{ParamDefinition: ParamDefinition{Key: "gpu_memory_utilization", Type: "number", Control: "slider", Group: "performance", Default: 0.9, Min: number(0.01), Max: number(0.98), Step: number(0.01), Layout: "quick", Order: 11}, Flags: map[string]flagPair{EngineVLLM: {On: "--gpu-memory-utilization"}, EngineSGLang: {On: "--mem-fraction-static"}}, DefaultByEngine: map[string]any{EngineSGLang: 0.88}},
	// A boolean the dictionary gives no default is three-state on the form: the
	// deployer can force it on or off, or leave it unstated so the engine keeps
	// its own default. Both engines below decide these themselves at startup —
	// vLLM V1 enables chunked prefill and prefix caching by default and disables
	// them for architectures that cannot carry them (omni-class encoder-decoder
	// models), and an explicit flag would turn that judgement into a startup
	// failure — so the platform states nothing unless asked to.
	{ParamDefinition: ParamDefinition{Key: "enable_chunked_prefill", Type: "boolean", Control: "switch", Group: "performance", Layout: "quick", Order: 38}, Flags: map[string]flagPair{EngineVLLM: {On: "--enable-chunked-prefill", Off: "--no-enable-chunked-prefill"}}},
	{ParamDefinition: ParamDefinition{Key: "max_num_batched_tokens", Type: "integer", Control: "number", Group: "performance", Automatic: true}, Flags: map[string]flagPair{EngineVLLM: {On: "--max-num-batched-tokens"}, EngineSGLang: {On: "--chunked-prefill-size"}}},
	{ParamDefinition: ParamDefinition{Key: "max_num_seqs", Type: "integer", Control: "number", Group: "performance", Automatic: true}, Flags: map[string]flagPair{EngineVLLM: {On: "--max-num-seqs"}, EngineSGLang: {On: "--max-running-requests"}}},
	// SGLang has no "on" form: radix cache is its default and only the negative
	// flag exists, so the on column is deliberately empty. No default either,
	// for the reason the chunked prefill row above states.
	{ParamDefinition: ParamDefinition{Key: "enable_prefix_caching", Type: "boolean", Control: "switch", Group: "performance", Layout: "quick", Order: 33}, Flags: map[string]flagPair{EngineVLLM: {On: "--enable-prefix-caching", Off: "--no-enable-prefix-caching"}, EngineSGLang: {Off: "--disable-radix-cache"}}},
	// Cache hit statistics ride on each response's usage; both engines spell it as a positive switch.
	{ParamDefinition: ParamDefinition{Key: "enable_cache_report", Type: "boolean", Control: "switch", Group: "performance", Default: true, Layout: "quick", Order: 34}, Flags: map[string]flagPair{EngineVLLM: {On: "--enable-prompt-tokens-details"}, EngineSGLang: {On: "--enable-cache-report"}}},
	// Per-request timings ride on the same usage: vLLM 0.25.0 and later write
	// the request's queue, prefill and decode times into the reply's last
	// event. The default applies only to an image known to accept the flag
	// (vllmAcceptsRequestMetrics); elsewhere the switch starts at automatic, so
	// the default cannot stop an older image from starting.
	{ParamDefinition: ParamDefinition{Key: "enable_request_metrics", Type: "boolean", Control: "switch", Group: "performance", Default: true, Layout: "quick", Order: 39}, Flags: map[string]flagPair{EngineVLLM: {On: "--enable-per-request-metrics"}}},
	{ParamDefinition: ParamDefinition{Key: "kv_cache_dtype", Type: "enum", Control: "select", Group: "performance", Default: "auto", Options: options(vllmKVCacheDtypes...)}, Flags: map[string]flagPair{EngineVLLM: {On: "--kv-cache-dtype"}, EngineSGLang: {On: "--kv-cache-dtype"}}, Options: map[string][]ParamOption{EngineSGLang: sglangKVCacheDtypes}},
	// The memory tier's size is also the cpu_bytes_to_use of the disk tier's
	// transfer config, so the adapter decides which spelling carries it: that
	// is the "composed" marker, not a second fact about the key.
	{ParamDefinition: ParamDefinition{Key: "kv_offloading_size", Type: "number", Control: "tier", Group: "performance", Min: number(1), Step: number(1), Layout: "quick", Order: 20}, Flags: map[string]flagPair{EngineVLLM: {On: "--kv-offloading-size", Composed: true}}},
	{ParamDefinition: ParamDefinition{Key: "kv_disk_offload", Type: "boolean", Control: "tier", Group: "performance", Default: false, DependsOn: "kv_offloading_size", Layout: "quick", Order: 21}, Flags: map[string]flagPair{EngineVLLM: {On: "--kv-transfer-config", Composed: true}}},
	{ParamDefinition: ParamDefinition{Key: "shared_kv_dir", Type: "string", Control: "path", Group: "performance", DependsOn: "kv_disk_offload", Layout: "quick", Order: 22}, Flags: map[string]flagPair{EngineVLLM: {On: "--kv-transfer-config", Composed: true}}},
	{ParamDefinition: ParamDefinition{Key: "kv_offload_read_threads", Type: "integer", Control: "number", Group: "performance", Default: 16, Min: number(1), Step: number(1), DependsOn: "kv_disk_offload", Layout: "quick", Order: 23}, Flags: map[string]flagPair{EngineVLLM: {On: "--kv-transfer-config", Composed: true}}},
	{ParamDefinition: ParamDefinition{Key: "kv_offload_write_threads", Type: "integer", Control: "number", Group: "performance", Default: 16, Min: number(1), Step: number(1), DependsOn: "kv_disk_offload", Layout: "quick", Order: 24}, Flags: map[string]flagPair{EngineVLLM: {On: "--kv-transfer-config", Composed: true}}},
	// The disk tier's capacity ceiling is enforced by the agent's sweeper, not
	// by vLLM, which has no flag for it: the tier writes until the filesystem
	// is full. So this row is a platform parameter (ADR 0021) - no flag column,
	// never emitted, and independent of the disk tier switch. Unset means the
	// server derives it from the model's weight bytes. The value is effective
	// only while the disk tier is enabled, so disabling that tier can preserve
	// the draft without leaving an active sweeper ceiling behind.
	{ParamDefinition: ParamDefinition{Key: "kv_disk_limit_gib", Type: "number", Control: "number", Group: "performance", Automatic: true, Min: number(1), Step: number(1), Engines: []string{EngineVLLM}, DependsOn: "kv_disk_offload", Layout: "quick", Order: 25}, Platform: true},
	// --disable-cuda-graph is SGLang's spelling of enforce_eager=true, not an off
	// form: turning it off means emitting nothing.
	{ParamDefinition: ParamDefinition{Key: "enforce_eager", Type: "boolean", Control: "switch", Group: "performance", Default: false, Layout: "quick", Order: 35}, Flags: map[string]flagPair{EngineVLLM: {On: "--enforce-eager"}, EngineSGLang: {On: "--disable-cuda-graph"}}},

	{ParamDefinition: ParamDefinition{Key: "speculative_algorithm", Type: "string", Control: "combobox", Group: "speculative", Options: options(vllmSpeculativeMethods...), Layout: "quick", Order: 32}, Flags: map[string]flagPair{EngineVLLM: {On: "--speculative-config", Composed: true}, EngineSGLang: {On: "--speculative-algorithm"}}, Options: map[string][]ParamOption{EngineSGLang: sglangSpeculativeAlgorithms}, ClosedFor: []string{EngineSGLang}},
	{ParamDefinition: ParamDefinition{Key: "speculative_draft_model", Type: "string", Control: "text", Group: "speculative"}, Flags: map[string]flagPair{EngineVLLM: {On: "--speculative-config", Composed: true}, EngineSGLang: {On: "--speculative-draft-model-path"}}},
	{ParamDefinition: ParamDefinition{Key: "speculative_tokens", Type: "integer", Control: "number", Group: "speculative", Min: number(1), Step: number(1)}, Flags: map[string]flagPair{EngineVLLM: {On: "--speculative-config", Composed: true}, EngineSGLang: {On: "--speculative-num-draft-tokens"}}},
	{ParamDefinition: ParamDefinition{Key: "speculative_num_steps", Type: "integer", Control: "number", Group: "speculative", Min: number(1), Step: number(1)}, Flags: map[string]flagPair{EngineSGLang: {On: "--speculative-num-steps"}}},
	{ParamDefinition: ParamDefinition{Key: "speculative_eagle_top_k", Type: "integer", Control: "number", Group: "speculative", Min: number(1), Step: number(1)}, Flags: map[string]flagPair{EngineSGLang: {On: "--speculative-eagle-topk"}}},

	{ParamDefinition: ParamDefinition{Key: "optimization_level", Type: "enum", Control: "select", Group: "performance_advanced", Advanced: true, Options: options("O0", "O1", "O2", "O3")}, Flags: map[string]flagPair{EngineVLLM: {On: "-O", Composed: true}}},
	{ParamDefinition: ParamDefinition{Key: "async_scheduling", Type: "boolean", Control: "switch", Group: "performance_advanced", Advanced: true, Default: false}, Flags: map[string]flagPair{EngineVLLM: {On: "--async-scheduling"}}},
	{ParamDefinition: ParamDefinition{Key: "long_prefill_token_threshold", Type: "integer", Control: "number", Group: "performance_advanced", Advanced: true, Automatic: true}, Flags: map[string]flagPair{EngineVLLM: {On: "--long-prefill-token-threshold"}}},
	{ParamDefinition: ParamDefinition{Key: "kv_cache_memory_bytes", Type: "integer", Control: "number", Group: "performance_advanced", Advanced: true, Automatic: true}, Flags: map[string]flagPair{EngineVLLM: {On: "--kv-cache-memory-bytes"}}},
	{ParamDefinition: ParamDefinition{Key: "prefix_caching_hash_algo", Type: "enum", Control: "select", Group: "performance_advanced", Advanced: true, Options: options("builtin", "sha256", "sha256_cbor", "xxhash")}, Flags: map[string]flagPair{EngineVLLM: {On: "--prefix-caching-hash-algo"}}},
	{ParamDefinition: ParamDefinition{Key: "cpu_offload_gb", Type: "number", Control: "number", Group: "performance_advanced", Advanced: true, Min: number(0), Step: number(1)}, Flags: map[string]flagPair{EngineVLLM: {On: "--cpu-offload-gb"}}},
	{ParamDefinition: ParamDefinition{Key: "stream_interval", Type: "integer", Control: "number", Group: "performance_advanced", Advanced: true, Min: number(1), Step: number(1)}, Flags: map[string]flagPair{EngineVLLM: {On: "--stream-interval"}}},
	{ParamDefinition: ParamDefinition{Key: "watermark", Type: "number", Control: "number", Group: "performance_advanced", Advanced: true, Min: number(0), Max: number(1), Step: number(0.01)}, Flags: map[string]flagPair{EngineVLLM: {On: "--watermark"}}},
	{ParamDefinition: ParamDefinition{Key: "max_cudagraph_capture_size", Type: "integer", Control: "number", Group: "performance_advanced", Advanced: true, Min: number(1), Step: number(1)}, Flags: map[string]flagPair{EngineVLLM: {On: "--max-cudagraph-capture-size"}}},
	{ParamDefinition: ParamDefinition{Key: "mm_encoder_tp_mode", Type: "enum", Control: "select", Group: "performance_advanced", Advanced: true, Options: options("weights", "data")}, Flags: map[string]flagPair{EngineVLLM: {On: "--mm-encoder-tp-mode"}}},
	{ParamDefinition: ParamDefinition{Key: "api_server_count", Type: "integer", Control: "number", Group: "performance_advanced", Advanced: true, Min: number(1), Step: number(1)}, Flags: map[string]flagPair{EngineVLLM: {On: "--api-server-count"}}},
	{ParamDefinition: ParamDefinition{Key: "data_parallel_size", Type: "integer", Control: "number", Group: "parallel", Min: number(1), Step: number(1)}, Flags: map[string]flagPair{EngineVLLM: {On: "--data-parallel-size"}, EngineSGLang: {On: "--dp-size"}}},
	{ParamDefinition: ParamDefinition{Key: "lora", Type: "boolean", Control: "switch", Group: "model", Default: false}, Flags: map[string]flagPair{EngineVLLM: {On: "--enable-lora"}, EngineSGLang: {On: "--enable-lora"}}},
}

func init() {
	locales, err := loadParameterLocales()
	if err == nil {
		err = applyParameterLocales(parameterDefinitions, locales)
	}
	if err == nil {
		err = validateParameterDependencies(parameterDefinitions)
	}
	if err != nil {
		panic(fmt.Sprintf("load deploy parameter locales: %v", err))
	}
}

func validateParameterDependencies(definitions []paramDefinition) error {
	byKey := make(map[string]paramDefinition, len(definitions))
	for _, definition := range definitions {
		byKey[definition.Key] = definition
	}
	for _, definition := range definitions {
		if definition.DependsOn == "" {
			continue
		}
		if _, ok := byKey[definition.DependsOn]; !ok {
			return fmt.Errorf("parameter %q depends on unknown parameter %q", definition.Key, definition.DependsOn)
		}
	}
	visiting := map[string]bool{}
	visited := map[string]bool{}
	var visit func(string) error
	visit = func(key string) error {
		if visiting[key] {
			return fmt.Errorf("parameter dependency cycle contains %q", key)
		}
		if visited[key] {
			return nil
		}
		visiting[key] = true
		if parent := byKey[key].DependsOn; parent != "" {
			if err := visit(parent); err != nil {
				return err
			}
		}
		visiting[key] = false
		visited[key] = true
		return nil
	}
	for key := range byKey {
		if err := visit(key); err != nil {
			return err
		}
	}
	return nil
}

func loadParameterLocales() (map[string]map[string]parameterLocaleEntry, error) {
	return localization.LoadJSONFiles[map[string]parameterLocaleEntry](parameterLocaleFiles, "locales/%s.json", requiredParameterLocales)
}

func applyParameterLocales(definitions []paramDefinition, locales map[string]map[string]parameterLocaleEntry) error {
	definitionKeys := make(map[string]struct{}, len(definitions))
	for i := range definitions {
		definition := &definitions[i]
		if definition.Key == "" {
			return fmt.Errorf("parameter definition at index %d has no key", i)
		}
		if _, duplicate := definitionKeys[definition.Key]; duplicate {
			return fmt.Errorf("duplicate parameter definition %q", definition.Key)
		}
		definitionKeys[definition.Key] = struct{}{}
	}

	for _, language := range requiredParameterLocales {
		entries, ok := locales[language]
		if !ok {
			return fmt.Errorf("missing required locale %q", language)
		}
		for key := range entries {
			if _, known := definitionKeys[key]; !known {
				return fmt.Errorf("locale %s contains unknown parameter %q", language, key)
			}
		}
		for i := range definitions {
			definition := &definitions[i]
			entry, ok := entries[definition.Key]
			if !ok {
				return fmt.Errorf("locale %s is missing parameter %q", language, definition.Key)
			}
			if entry.Label == "" {
				return fmt.Errorf("locale %s parameter %q has no label", language, definition.Key)
			}
			// The row's own options and every engine-only option an override
			// list adds are labeled here; a label for anything else is stale.
			labeled := slices.Clone(definition.ParamDefinition.Options)
			for _, override := range definition.Options {
				labeled = append(labeled, override...)
			}
			known := map[string]bool{}
			for _, option := range labeled {
				known[option.Value] = true
			}
			for value := range entry.Options {
				if !known[value] {
					return fmt.Errorf("locale %s parameter %q labels unknown option %q", language, definition.Key, value)
				}
			}
			for _, option := range definition.ParamDefinition.Options {
				if entry.Options[option.Value] == "" {
					return fmt.Errorf("locale %s parameter %q is missing option %q", language, definition.Key, option.Value)
				}
			}
		}
	}

	for i := range definitions {
		definition := &definitions[i]
		definition.Label = make(localization.Text, len(requiredParameterLocales))
		for _, language := range requiredParameterLocales {
			definition.Label[language] = locales[language][definition.Key].Label
		}
		placeholder, err := localizedOptionalField(definition.Key, locales, func(entry parameterLocaleEntry) string { return entry.Placeholder })
		if err != nil {
			return err
		}
		definition.Placeholder = placeholder
		help, err := localizedOptionalField(definition.Key, locales, func(entry parameterLocaleEntry) string { return entry.Help })
		if err != nil {
			return err
		}
		definition.Help = help
		for optionIndex := range definition.ParamDefinition.Options {
			option := &definition.ParamDefinition.Options[optionIndex]
			option.Label = make(localization.Text, len(requiredParameterLocales))
			for _, language := range requiredParameterLocales {
				option.Label[language] = locales[language][definition.Key].Options[option.Value]
			}
		}
		// A per-engine override list carries its own labels: by value where the
		// locale files know the option (SGLang's smaller KV cache dtype set),
		// and its own value otherwise (the parser vocabularies only one engine
		// has, which technicalOptions already labels that way).
		for _, override := range definition.Options {
			for optionIndex := range override {
				option := &override[optionIndex]
				if len(option.Label) > 0 {
					continue
				}
				option.Label = make(localization.Text, len(requiredParameterLocales))
				for _, language := range requiredParameterLocales {
					label := locales[language][definition.Key].Options[option.Value]
					if label == "" {
						label = option.Value
					}
					option.Label[language] = label
				}
			}
		}
	}
	return nil
}

func localizedOptionalField(key string, locales map[string]map[string]parameterLocaleEntry, field func(parameterLocaleEntry) string) (localization.Text, error) {
	text := make(localization.Text, len(requiredParameterLocales))
	present := 0
	for _, language := range requiredParameterLocales {
		value := field(locales[language][key])
		if value != "" {
			present++
			text[language] = value
		}
	}
	if present == 0 {
		return nil, nil
	}
	if present != len(requiredParameterLocales) {
		return nil, fmt.Errorf("parameter %q optional text is not translated in every required locale", key)
	}
	return text, nil
}

// sglangParallelFlags is every process-parallel dimension an SGLang command can
// carry: the engine's own TP aliases (sglang_topology.go) plus the pipeline,
// data and expert dimensions. Topology validation and logging compatibility
// share it; a dictConfig cannot preserve their rank prefixes.
var sglangParallelFlags = append(append(append([]string(nil), sglangTensorParallelFlags...), sglangPipelineParallelFlags...), []string{
	"--dp", "--data-parallel-size", "--ep", "--ep-size", "--expert-parallel-size", "--nnodes", "--moe-dp-size",
}...)
