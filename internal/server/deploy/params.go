// Package deploy turns a deployment request into a concrete engine launch
// deterministically (pure functions, unit-testable): the Engine interface plus
// its registry in engine.go, one adapter file per engine, and the
// engine-neutral inputs in this file.
//
// params.go holds those engine-neutral deployment inputs: the launch
// configuration a deployment carries, its validation and non-blocking
// warnings, and the Params struct every engine Build consumes.
package deploy

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/messages"
)

// LaunchConfig is a deployment's engine parameters: the values the deployer
// stated, materialized once at the write boundary and read unchanged by the
// estimator, the scheduler, argv assembly and the form. The JSON keys are the
// dictionary's keys, and the column that stores it is still called quick_params.
//
// A key can be unset, and unset is not a value: it means the deployer chose
// nothing and the engine's own default applies. The platform does not fill it
// in, because filling it in would put a flag on the command line that nobody
// asked for - and on an upgrade that would silently change how an existing
// deployment starts. Unset is spelled the way Go and JSON already spell it: a
// nil pointer for a boolean, a zero for a number, an empty string. The one
// reader that must have a number, the estimator, resolves it through
// DefaultGPUMemUtil, which reads the engine default the dictionary records.
type LaunchConfig struct {
	LogLevel string `json:"log_level,omitempty"`
	// ServedModelName is an internal pointer: nil keeps legacy behavior, while
	// an empty value records that the public alias was deliberately cleared.
	ServedModelName *string `json:"served_model_name,omitempty"`
	EnableMetrics   bool    `json:"enable_metrics,omitempty"`
	PerformanceMode string  `json:"performance_mode,omitempty"`
	// A nil boolean is unset: the engine decides. Only a stated value reaches
	// the command line, in whichever polarity the dictionary says this engine
	// spells it.
	EnableChunkedPrefill *bool `json:"enable_chunked_prefill,omitempty"`
	EnablePrefixCaching  *bool `json:"enable_prefix_caching,omitempty"`
	EnableCacheReport    *bool `json:"enable_cache_report,omitempty"`
	// EnableRequestMetrics gets its default only on an image whose engine
	// release takes the flag (vllmAcceptsRequestMetrics).
	EnableRequestMetrics *bool  `json:"enable_request_metrics,omitempty"`
	MaxModelLen          int    `json:"max_model_len,omitempty"`
	Dtype                string `json:"dtype,omitempty"`          // auto|bfloat16|float16|float32
	KVCacheDtype         string `json:"kv_cache_dtype,omitempty"` // auto|fp8|fp8_e5m2
	// GPUMemUtil zero is unset: no --gpu-memory-utilization is emitted and the
	// engine runs at its own default. Materialize rejects an explicit zero, so
	// a zero here can only mean "not stated".
	GPUMemUtil                float64 `json:"gpu_memory_utilization,omitempty"`
	RopeScaling               string  `json:"rope_scaling,omitempty"` // JSON string (yarn etc.)
	MaxNumSeqs                int     `json:"max_num_seqs,omitempty"`
	MaxNumBatchedTokens       int     `json:"max_num_batched_tokens,omitempty"`
	LongPrefillTokenThreshold int     `json:"long_prefill_token_threshold,omitempty"`
	SpeculativeTokens         int     `json:"speculative_tokens,omitempty"`
	LoRA                      bool    `json:"lora,omitempty"`
	KVOffloadingSize          float64 `json:"kv_offloading_size,omitempty"`
	KVDiskOffload             bool    `json:"kv_disk_offload,omitempty"`
	// Zero thread counts are unset; the connector config falls back to the
	// dictionary default rather than asking the engine for zero threads.
	KVOffloadReadThreads  int `json:"kv_offload_read_threads,omitempty"`
	KVOffloadWriteThreads int `json:"kv_offload_write_threads,omitempty"`
	// KVDiskLimitGiB is a platform parameter (ADR 0021): the agent's sweeper
	// keeps the disk tier directory under it, and no engine flag carries it.
	// Zero is unset, and unset means the server derives the ceiling from the
	// model's weight bytes without writing a number back here.
	KVDiskLimitGiB          float64 `json:"kv_disk_limit_gib,omitempty"`
	KVOffloadBlockSize      int     `json:"kv_offload_block_size,omitempty"`
	GPUBlockSize            int     `json:"gpu_block_size,omitempty"`
	KVCacheMemoryBytes      int64   `json:"kv_cache_memory_bytes,omitempty"`
	EstimatedGPUKVGiB       float64 `json:"estimated_gpu_kv_gib,omitempty"`
	PrefixCachingHashAlgo   string  `json:"prefix_caching_hash_algo,omitempty"`
	SharedKVDir             string  `json:"shared_kv_dir,omitempty"`
	PythonHashSeed          string  `json:"python_hash_seed,omitempty"`
	AsyncScheduling         bool    `json:"async_scheduling,omitempty"`
	SpeculativeAlgorithm    string  `json:"speculative_algorithm,omitempty"`
	SpeculativeDraftModel   string  `json:"speculative_draft_model,omitempty"`
	SpeculativeNumSteps     int     `json:"speculative_num_steps,omitempty"`
	SpeculativeEagleTopK    int     `json:"speculative_eagle_top_k,omitempty"`
	APIServerCount          int     `json:"api_server_count,omitempty"`
	DP                      int     `json:"data_parallel_size,omitempty"`
	CPUCores                int     `json:"cpu_cores,omitempty"`
	OptimizationLevel       string  `json:"optimization_level,omitempty"`
	CPUOffloadGB            float64 `json:"cpu_offload_gb,omitempty"`
	StreamInterval          int     `json:"stream_interval,omitempty"`
	Watermark               float64 `json:"watermark,omitempty"`
	MaxCudagraphCaptureSize int     `json:"max_cudagraph_capture_size,omitempty"`
	MMEncoderTPMode         string  `json:"mm_encoder_tp_mode,omitempty"`
	Quantization            string  `json:"quantization,omitempty"`
	TrustRemote             bool    `json:"trust_remote_code,omitempty"`
	EnforceEager            bool    `json:"enforce_eager,omitempty"`
	ReasoningParser         string  `json:"reasoning_parser,omitempty"`
	ToolCallParser          string  `json:"tool_call_parser,omitempty"`
	EnableAutoToolChoice    bool    `json:"enable_auto_tool_choice,omitempty"`
	LimitMMPerPrompt        string  `json:"limit_mm_per_prompt,omitempty"`
	MMProcessorKwargs       string  `json:"mm_processor_kwargs,omitempty"`
	MediaIOKwargs           string  `json:"media_io_kwargs,omitempty"`
	AllowedLocalMediaPath   string  `json:"allowed_local_media_path,omitempty"`
}

// Launch-config validation error codes produced in this package. They are
// respelled in the API error inventory; TestLaunchConfigConflictCodeMatchesItsProducer
// keeps the two spellings equal.
const (
	// ErrorQuickParamJSON reports a json-typed parameter whose value is not
	// valid JSON. The key param names the form field the error belongs to.
	ErrorQuickParamJSON = "QUICK_PARAM_JSON_INVALID"
	// ErrorQuickParamPath reports a path parameter that is not an absolute
	// non-root container path.
	ErrorQuickParamPath = "QUICK_PARAM_PATH_INVALID"
)

// ValidateLaunchConfig rejects a malformed configuration before it is
// persisted. Residual arguments remain the explicit advanced escape hatch.
func ValidateLaunchConfig(q LaunchConfig) error {
	return ValidateLaunchConfigForEngine(DefaultEngine, q)
}

// ValidateLaunchConfigForEngine checks a launch configuration against the engine
// it will run on. Per-parameter facts - which values an enum accepts, how large
// a number may be - are read from the parameter dictionary, so adding a
// parameter never means editing this function. What stays here is the part no
// single dictionary row can express: rules that relate two parameters to each
// other, and the parameters one engine does not support at all.
func ValidateLaunchConfigForEngine(engine string, q LaunchConfig) error {
	params := engineParams(engine)
	if err := validateAgainstDictionary(engine, params, q); err != nil {
		return err
	}
	if err := validateParameterDependencyValues(params, q); err != nil {
		return err
	}
	_, chunkedPrefillSupported := params["enable_chunked_prefill"]
	if chunkedPrefillSupported && q.EnableChunkedPrefill != nil && !*q.EnableChunkedPrefill && q.MaxNumBatchedTokens > 0 && q.MaxModelLen > 0 && q.MaxNumBatchedTokens < q.MaxModelLen {
		return fmt.Errorf("max_num_batched_tokens must be at least max_model_len when chunked prefill is disabled")
	}
	if q.MaxNumBatchedTokens > 0 && q.MaxNumSeqs > 0 && q.MaxNumBatchedTokens < q.MaxNumSeqs {
		return fmt.Errorf("max_num_batched_tokens must be at least max_num_seqs")
	}
	if q.LongPrefillTokenThreshold > 0 && q.MaxModelLen > 0 && q.LongPrefillTokenThreshold > q.MaxModelLen {
		return fmt.Errorf("long_prefill_token_threshold must not exceed max_model_len")
	}
	if q.LoRA && q.SpeculativeTokens > 0 && q.MaxNumBatchedTokens > 0 && q.MaxNumSeqs > 0 && q.MaxNumBatchedTokens < q.MaxNumSeqs*(q.SpeculativeTokens+1) {
		return fmt.Errorf("max_num_batched_tokens is too small for LoRA speculative decoding")
	}
	if q.KVDiskOffload && q.KVOffloadingSize <= 0 {
		return fmt.Errorf("disk KV offload requires the memory offload tier")
	}
	if q.KVDiskOffload {
		clean := filepath.Clean(q.SharedKVDir)
		if !filepath.IsAbs(clean) || clean == "/" {
			return messages.NewProductError(ErrorQuickParamPath,
				"disk KV offload requires an absolute non-root shared_kv_dir",
				map[string]any{"key": "shared_kv_dir"})
		}
	}
	if q.KVOffloadBlockSize > 0 && q.GPUBlockSize > 0 && q.KVOffloadBlockSize%q.GPUBlockSize != 0 {
		return fmt.Errorf("KV offload block size must be a multiple of the GPU block size")
	}
	for name, value := range map[string]string{
		"rope_scaling":        q.RopeScaling,
		"limit_mm_per_prompt": q.LimitMMPerPrompt,
		"mm_processor_kwargs": q.MMProcessorKwargs,
		"media_io_kwargs":     q.MediaIOKwargs,
	} {
		if value != "" && !json.Valid([]byte(value)) {
			return messages.NewProductError(ErrorQuickParamJSON,
				fmt.Sprintf("%s must be valid JSON", name),
				map[string]any{"key": name})
		}
	}
	if q.AllowedLocalMediaPath != "" {
		clean := filepath.Clean(q.AllowedLocalMediaPath)
		if !filepath.IsAbs(clean) || clean == "/" {
			return messages.NewProductError(ErrorQuickParamPath,
				"allowed_local_media_path must be an absolute non-root container path",
				map[string]any{"key": "allowed_local_media_path"})
		}
	}
	return nil
}

func validateParameterDependencyValues(params map[string]engineParam, q LaunchConfig) error {
	encoded, err := json.Marshal(q)
	if err != nil {
		return err
	}
	values := map[string]any{}
	if err := json.Unmarshal(encoded, &values); err != nil {
		return err
	}
	for key, spec := range params {
		if spec.DependsOn == "" || !dependencyValueEnabled(values[key]) {
			continue
		}
		if !dependencyValueEnabled(values[spec.DependsOn]) {
			return fmt.Errorf("%s requires %s to be enabled", key, spec.DependsOn)
		}
	}
	return nil
}

func dependencyValueEnabled(value any) bool {
	switch typed := value.(type) {
	case bool:
		return typed
	case float64:
		return typed > 0
	case string:
		return strings.TrimSpace(typed) != ""
	default:
		return value != nil
	}
}

// neutralParameterValue reports whether a stated value for a key the engine
// cannot express is only the value that key takes when nobody states it: the
// engine-neutral default. It exists because a form may submit the neutral value
// of a parameter the selected engine has no flag for - "balanced" performance
// mode is the documented example - and rejecting that would reject a request
// that asked for nothing.
func neutralParameterValue(definition paramDefinition, engine string, value any) bool {
	neutral := definition.Default
	if override, ok := definition.DefaultByEngine[engine]; ok {
		neutral = override
	}
	if neutral == nil {
		return false
	}
	return sameValue(neutral, value)
}

// validateAgainstDictionary applies the per-parameter facts the dictionary
// holds: an enum accepts only its own options (for the engine in question),
// and a number stays within the bounds its row declares. A zero integer or
// float is how LaunchConfig spells "this optional parameter was not set", so
// only a value that is actually present is measured - except that a negative
// value is never a legitimate "unset" and is always rejected.
func validateAgainstDictionary(engine string, params map[string]engineParam, q LaunchConfig) error {
	values := map[string]any{}
	encoded, err := json.Marshal(q)
	if err != nil {
		return fmt.Errorf("invalid quick_params: %w", err)
	}
	if err := json.Unmarshal(encoded, &values); err != nil {
		return fmt.Errorf("invalid quick_params: %w", err)
	}
	for _, definition := range parameterDefinitions {
		value, present := values[definition.Key]
		if !present || value == nil {
			continue
		}
		spec, supported := params[definition.Key]
		if !supported {
			// The dictionary is the authority on what an engine can express: a
			// key with no flag for this engine can only become a silent no-op,
			// so a stated value is an error here rather than something an
			// adapter quietly drops. The exception is the value the engine runs
			// at anyway - a form that submits the neutral "balanced" for an
			// engine with no performance mode has stated nothing at all.
			if neutralParameterValue(definition, engine, value) {
				continue
			}
			return fmt.Errorf("%s is not supported by %s", definition.Key, engine)
		}
		switch spec.Type {
		case "enum":
			text, _ := value.(string)
			if text == "" || len(spec.Enum) == 0 || containsString(spec.Enum, text) {
				continue
			}
			return fmt.Errorf("%s %q is not supported by %s", spec.Key, text, engine)
		case "integer", "number":
			number, ok := toFloat(value)
			if !ok {
				continue
			}
			if number < 0 {
				return fmt.Errorf("%s must not be negative", spec.Key)
			}
			if number == 0 {
				continue // nobody stated this parameter
			}
			if spec.Min != nil && number < *spec.Min {
				return fmt.Errorf("%s must be at least %v", spec.Key, *spec.Min)
			}
			if spec.Max != nil && number > *spec.Max {
				return fmt.Errorf("%s must be at most %v", spec.Key, *spec.Max)
			}
		}
	}
	return nil
}

// PerformanceWarnings returns non-blocking warnings for cross-field tuning
// combinations. Evaluate and create both call this function so an accepted
// deployment cannot silently lose a warning shown in the wizard.
func PerformanceWarnings(q LaunchConfig, extraArgs, modelArch string, gpuCount int) []messages.Warning {
	var out []messages.Warning
	if q.MaxNumBatchedTokens > 0 && q.MaxNumSeqs > 0 && q.MaxModelLen > 0 && q.MaxNumBatchedTokens > q.MaxNumSeqs*q.MaxModelLen {
		out = append(out, messages.NewWarning("W1", map[string]any{"max_num_batched_tokens": q.MaxNumBatchedTokens, "max_num_seqs": q.MaxNumSeqs, "max_model_len": q.MaxModelLen}))
	}
	if q.KVOffloadingSize > 0 && q.EstimatedGPUKVGiB > 0 && q.KVOffloadingSize <= q.EstimatedGPUKVGiB {
		out = append(out, messages.NewWarning("W2", map[string]any{"kv_offloading_size": q.KVOffloadingSize, "estimated_gpu_kv_gib": q.EstimatedGPUKVGiB}))
	}
	if q.KVCacheMemoryBytes > 0 {
		out = append(out, messages.NewWarning("W3", map[string]any{"kv_cache_memory_bytes": q.KVCacheMemoryBytes}))
	}
	if q.KVOffloadingSize > 0 && strings.Contains(extraArgs, "--kv-transfer-config") {
		out = append(out, messages.NewWarning("W4", nil))
	}
	if q.PerformanceMode == "throughput" && (q.MaxNumBatchedTokens > 0 || q.MaxNumSeqs > 0) {
		out = append(out, messages.NewWarning("W5", nil))
	}
	if q.EnforceEager && strings.Contains(strings.ToLower(extraArgs), "cudagraph") {
		out = append(out, messages.NewWarning("W6", nil))
	}
	if q.SharedKVDir != "" && strings.Contains(strings.ToLower(q.PrefixCachingHashAlgo), "xxhash") && q.PythonHashSeed == "" {
		out = append(out, messages.NewWarning("W7", nil))
	}
	if q.AsyncScheduling && q.SpeculativeAlgorithm != "" && !map[string]bool{"eagle": true, "eagle3": true, "mtp": true, "ngram": true}[strings.ToLower(q.SpeculativeAlgorithm)] {
		out = append(out, messages.NewWarning("W8", map[string]any{"speculative_algorithm": q.SpeculativeAlgorithm}))
	}
	if strings.Contains(strings.ToLower(modelArch), "encoder-decoder") {
		out = append(out, messages.NewWarning("W9", map[string]any{"model_arch": modelArch}))
	}
	need := q.APIServerCount + q.DP + gpuCount
	if q.DP > 1 {
		need++
	}
	if q.CPUCores > 0 && need > 0 && q.CPUCores < need {
		out = append(out, messages.NewWarning("W10", map[string]any{"cpu_cores": q.CPUCores, "required_cores": need}))
	}
	return out
}

// Params is the engine-neutral input every Engine.Build consumes.
type Params struct {
	// Distributed identity is launch topology, even when a worker omits NNodes.
	DistGroup, DistRole string
	DistAttempt         int
	ModelPath           string // container-visible model path
	ServedName          string
	Host                string // default 127.0.0.1
	APIPort             int
	APIKey              string
	// AssignedGPUs is the number of GPUs placement assigned to this node. It is
	// a fact written by the scheduler (or the deployment preview), never by
	// engine policy, and is what topology validation measures the effective
	// argv against.
	AssignedGPUs int
	// TensorParallel, PipelineParallel and DataParallel are the degrees the
	// engine injects, over the whole deployment (package parallelism): on N
	// nodes each runs TensorParallel×PipelineParallel×DataParallel/N GPUs,
	// which is AssignedGPUs.
	TensorParallel    int
	PipelineParallel  int
	DataParallel      int
	Sleepable         bool
	VRAMLimitBytes    int64
	SMLimitPct        int
	Partitioned       bool
	PartitionBackend  string
	Quick             LaunchConfig
	TemplateArgs      []string // template passthrough (below structured form values)
	TemplateEnv       map[string]string
	ExtraArgs         []string // user free-form (highest priority)
	Arch              string   // architecture family / model type for quirks
	ArchID            string   // vendor-specific arch identifier, e.g. sm_90, 910B4
	ComputeCapability string   // NVIDIA compute capability, e.g. 7.5 or 8.0
	Vendor            string   // nvidia | ascend | amd | hygon
	ModelType         string
	// PositionalModel uses the modern `vllm serve <model>` CLI (model positional
	// via Entrypoint) instead of the `--model` api_server flag. Default true.
	PositionalModel bool

	// Distributed (multi-machine): when NNodes>1, inject the mp distributed
	// params. The leader (node_rank 0) serves the endpoint; followers are headless.
	NNodes     int
	NodeRank   int
	MasterAddr string
	MasterPort int
	Headless   bool
	// NodeAddress is this rank's routable host/IP. vLLM uses it to advertise
	// the local worker endpoint; it must not be replaced by the leader address.
	NodeAddress string
	// ComputeNetwork is this rank's resolved compute network, if any.
	ComputeNetwork ComputeNetwork
	// PD is set on a PD disaggregated instance (ADR 0028).
	PD *PDLaunch
	// RuntimeDriver is the container runtime the target node reports
	// ("docker", "udocker", "host", ...); empty when no node is known.
	RuntimeDriver string
}

// ResolvedLaunch is the single launch contract shared with the agent protocol.
// Engine builders populate its command/system/template portions; ResolveLaunch
// adds the deployment's container overrides, catalog assets, API contract and
// placement output.
type ResolvedLaunch = proto.ResolvedLaunch
