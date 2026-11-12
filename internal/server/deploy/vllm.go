// vllm.go is the vLLM engine adapter: registry identity, engine profile and the
// deterministic `vllm serve` argv assembly.

package deploy

import (
	"encoding/json"
	"path/filepath"
	"strconv"
	"strings"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/pdmode"
)

type vLLMEngine struct{ BaseEngine }

func (vLLMEngine) Name() string { return EngineVLLM }

// vLLMParamDict is the key→flag map this adapter publishes to clients.
func vLLMParamDict() map[string]string { return paramDictForEngine(EngineVLLM) }

// TPFlags: vLLM accepts three spellings of the tensor-parallel dimension and
// injects the first one.
func (vLLMEngine) TPFlags() []string { return vllmTensorParallelFlags }

// SplitCommand: a vLLM launch encodes the executable and its parameters in one
// entrypoint ("vllm serve <model> ..."), so replacing the executable has to
// hand the trailing parameters back. A launch that is not vLLM-shaped (an image
// entrypoint, a manual command) keeps its own entrypoint/argv pair.
func (vLLMEngine) SplitCommand(launch ResolvedLaunch) ([]string, []string) {
	entry := launch.Entrypoint
	if len(entry) >= 3 && entry[0] == EngineVLLM && entry[1] == "serve" {
		return entry[:2], append(append([]string(nil), entry[2:]...), launch.Argv...)
	}
	return launch.Entrypoint, launch.Argv
}

func (vLLMEngine) Profile() Profile {
	dict := vLLMParamDict()
	return Profile{
		Name: EngineVLLM, DefaultImage: "vllm/vllm-openai:latest", HealthPath: "/health", ShmBytes: 16 << 30,
		MetricPath: "/metrics",
		Sleep:      SleepCaps{Supported: true, Levels: []string{"L1", "L2"}}, MetricPrefix: EngineVLLM,
		// The mapping itself lives in internal/inferencemetrics (engines.go);
		// MetricMap is its stored-series view for the engines API.
		MetricMap:        EngineMetricMap(EngineVLLM),
		ParamDict:        dict,
		ParamDefinitions: structuredParams(EngineVLLM, dict),
		KVDiskMinKernel:  KVDiskMinKernel,
		QuickFormKeys: []string{
			"max_model_len", "dtype", "quantization", "gpu_memory_utilization",
			"max_num_seqs", "max_num_batched_tokens", "enable_prefix_caching",
			"data_parallel_size",
			"enable_cache_report",
			"kv_cache_dtype",
			// The KV tier row of the quick form is driven by these keys: without
			// them the memory and disk cards read "not provided by this engine".
			"kv_offloading_size", "kv_disk_offload", "shared_kv_dir",
			"kv_offload_read_threads", "kv_offload_write_threads",
			// The disk tier's capacity ceiling is a platform parameter, so it is
			// not a companion of the tier flag and lands in the quick form's
			// advanced list on its own.
			"kv_disk_limit_gib",
			"reasoning_parser", "tool_call_parser",
			"enable_auto_tool_choice", "trust_remote_code", "enforce_eager",
		},
	}
}
func (vLLMEngine) Build(p Params) ResolvedLaunch {
	p.PositionalModel = true
	return BuildVLLM(p)
}

// BuildVLLM assembles the vllm serve argv from p. Priority, low → high (plan §11):
//
//	engine defaults (not generated)
//	→ platform injection (--host/--port/--served-model-name/--api-key/-tp/sleep)
//	→ quick-param mapping (max_model_len/dtype/kv-cache-dtype/rope-scaling/...)
//	→ quirks corrections (data-driven per arch/model)
//	→ user extra_args (highest; extendIfAbsent — a flag the user wrote is never
//	  re-injected by the platform)
//
// It returns the full argv (the args appended to the vllm-openai image
// entrypoint).
func BuildVLLM(p Params) ResolvedLaunch {
	if p.Host == "" {
		p.Host = "127.0.0.1"
	}
	if p.TensorParallel <= 0 {
		p.TensorParallel = 1
	}
	// Modern `vllm serve` owns the model as a positional argument. Catalogs may
	// still carry an explicit identity flag copied from an older api_server or a
	// source recipe; remove only the equivalent template-layer identity so a
	// multi-component primary is not supplied twice. User args remain untouched
	// and therefore keep their existing highest-priority semantics.
	p.TemplateArgs = stripEquivalentModelIdentity(p.TemplateArgs, p.ModelPath)
	blockers, templateArgs := residualLayers(p.TemplateArgs, p.ExtraArgs)
	b := newBuilder(EngineVLLM, blockers)
	// The model is local and fully downloaded; keep vllm offline so it never
	// reaches HuggingFace during load (which also fails on egress-restricted nodes).
	env := map[string]string{
		"HF_HUB_OFFLINE":       "1",
		"TRANSFORMERS_OFFLINE": "1",
		// Force loopback for vllm's intra-node IPC (V1 EngineCore uses gloo/NCCL
		// over TCP); without this the engine subprocess can fail hostname
		// resolution (gloo getaddrinfo -2), especially under a dGPU/host-network
		// container. Targets with multi-device collectives override this default
		// through computeNetworkEnv.
		"VLLM_HOST_IP":       "127.0.0.1",
		"GLOO_SOCKET_IFNAME": "lo",
		"NCCL_SOCKET_IFNAME": "lo",
	}

	// --- platform injection (always present unless user overrode) ---
	var entrypoint []string
	if p.PositionalModel {
		// `vllm serve <model>`: model is positional via the entrypoint override.
		entrypoint = []string{EngineVLLM, "serve", p.ModelPath}
	} else {
		b.injectKV("--model", p.ModelPath)
	}
	if servedName := p.EngineServedName(); servedName != "" && !p.Headless {
		b.injectKV("--served-model-name", servedName)
	}
	if !p.Headless {
		b.injectKV("--host", p.Host)
		b.injectKV("--port", strconv.Itoa(p.APIPort))
	}
	if p.APIKey != "" {
		b.injectKV("--api-key", p.APIKey)
	}
	b.injectKV(tpFlagTensorParallelSize, strconv.Itoa(p.TensorParallel))
	if p.PipelineParallel > 1 {
		b.injectKV("--pipeline-parallel-size", strconv.Itoa(p.PipelineParallel))
	}
	if p.Sleepable {
		b.injectFlag("--enable-sleep-mode")
		env["VLLM_SERVER_DEV_MODE"] = "1"
		switch accelerator.NormalizeVendor(p.Vendor) {
		case accelerator.VendorAscend:
			b.injectKV(vllmAdditionalConfigFlag, vllmAscendSleepConfig)
		case accelerator.VendorHygon:
			env["VLLM_CUDART_SO_PATH"] = vllmHygonCUDARTPath
		}
	}
	// Multi-machine: mp backend + node coordination (plan §5). vllm gathers the
	// leader + followers into one engine; only the leader exposes the endpoint.
	if p.NNodes > 1 {
		b.injectKV("--distributed-executor-backend", "mp")
		b.injectKV("--nnodes", strconv.Itoa(p.NNodes))
		b.injectKV("--node-rank", strconv.Itoa(p.NodeRank))
		if p.Headless {
			b.injectFlag("--headless")
		}
		if p.MasterAddr != "" {
			b.injectKV("--master-addr", p.MasterAddr)
		}
		if p.MasterPort > 0 {
			b.injectKV("--master-port", strconv.Itoa(p.MasterPort))
		}
		if p.NodeAddress != "" {
			env["VLLM_HOST_IP"] = p.NodeAddress
		}
		// Empty values are agent-side auto-selection markers. The agent maps
		// VLLM_HOST_IP to the exact host-network interface before starting the
		// container; leaving selection to Gloo can fall back to loopback when a
		// cloud VM hostname is not locally resolvable.
		env["GLOO_SOCKET_IFNAME"] = ""
		env["NCCL_SOCKET_IFNAME"] = ""
	}

	// --- quick params (user-facing knobs) ---
	q := p.Quick
	// Every parameter below is injected by key: the builder reads the flag and
	// the value format from the table. What stays here is when a stated value
	// becomes a flag - an engine's own default is not a configuration - and the
	// order, which the golden commands pin.
	b.injectDictionaryBool("enable_chunked_prefill", q.EnableChunkedPrefill)
	b.injectDictionaryBool("enable_prefix_caching", q.EnablePrefixCaching)
	b.injectDictionaryBool("enable_cache_report", q.EnableCacheReport)
	b.injectDictionaryBool("enable_request_metrics", q.EnableRequestMetrics)
	if q.PerformanceMode != "" && q.PerformanceMode != "balanced" {
		b.injectDictionary("performance_mode", q.PerformanceMode)
	}
	if q.MaxModelLen > 0 {
		b.injectDictionary("max_model_len", q.MaxModelLen)
	}
	if q.Dtype != "" && q.Dtype != "auto" {
		b.injectDictionary("dtype", q.Dtype)
	}
	if q.KVCacheDtype != "" && q.KVCacheDtype != "auto" {
		b.injectDictionary("kv_cache_dtype", q.KVCacheDtype)
	}
	if q.GPUMemUtil > 0 {
		b.injectDictionary("gpu_memory_utilization", q.GPUMemUtil)
	}
	if q.MaxNumSeqs > 0 {
		b.injectDictionary("max_num_seqs", q.MaxNumSeqs)
	}
	if q.MaxNumBatchedTokens > 0 {
		b.injectDictionary("max_num_batched_tokens", q.MaxNumBatchedTokens)
	}
	if q.LongPrefillTokenThreshold > 0 {
		b.injectDictionary("long_prefill_token_threshold", q.LongPrefillTokenThreshold)
	}
	if config, ok := vllmKVTransferConfig(p); ok {
		// The disk tier and the PD connector are composite flags: one JSON
		// object carries them, so the adapter composes it in one place.
		b.injectKV("--kv-transfer-config", config)
		vllmHybridKVCacheManager(b, p)
	} else if q.KVOffloadingSize > 0 {
		b.injectDictionary("kv_offloading_size", q.KVOffloadingSize)
		b.injectKV("--kv-offloading-backend", "native")
	}
	if q.GPUBlockSize > 0 {
		b.injectKV("--block-size", strconv.Itoa(q.GPUBlockSize))
	}
	if q.KVCacheMemoryBytes > 0 {
		b.injectDictionary("kv_cache_memory_bytes", q.KVCacheMemoryBytes)
	}
	if q.PrefixCachingHashAlgo != "" {
		b.injectDictionary("prefix_caching_hash_algo", q.PrefixCachingHashAlgo)
	}
	if q.SpeculativeAlgorithm != "" || q.SpeculativeDraftModel != "" || q.SpeculativeTokens > 0 {
		// Composite: one flag carries the algorithm, the draft model and the
		// token count.
		b.injectKV("--speculative-config", speculativeConfig(q))
	}
	if q.OptimizationLevel != "" {
		// Composite: the level is glued into the flag token (-O3).
		b.injectFlag("-" + q.OptimizationLevel)
	}
	if q.AsyncScheduling {
		b.injectDictionary("async_scheduling", true)
	}
	if q.CPUOffloadGB > 0 {
		b.injectDictionary("cpu_offload_gb", q.CPUOffloadGB)
	}
	if q.StreamInterval > 0 {
		b.injectDictionary("stream_interval", q.StreamInterval)
	}
	if q.Watermark > 0 {
		b.injectDictionary("watermark", q.Watermark)
	}
	if q.MaxCudagraphCaptureSize > 0 {
		b.injectDictionary("max_cudagraph_capture_size", q.MaxCudagraphCaptureSize)
	}
	if q.MMEncoderTPMode != "" {
		b.injectDictionary("mm_encoder_tp_mode", q.MMEncoderTPMode)
	}
	if q.APIServerCount > 0 {
		b.injectDictionary("api_server_count", q.APIServerCount)
	}
	if q.DP > 0 {
		b.injectDictionary("data_parallel_size", q.DP)
	}
	if q.LoRA {
		b.injectDictionary("lora", true)
	}
	if q.Quantization != "" {
		b.injectDictionary("quantization", q.Quantization)
	}
	if q.RopeScaling != "" {
		b.injectDictionary("rope_scaling", q.RopeScaling)
	}
	if q.TrustRemote {
		b.injectDictionary("trust_remote_code", true)
	}
	if q.EnforceEager {
		b.injectDictionary("enforce_eager", true)
	}
	if q.ReasoningParser != "" {
		b.injectDictionary("reasoning_parser", q.ReasoningParser)
	}
	if q.ToolCallParser != "" {
		b.injectDictionary("tool_call_parser", q.ToolCallParser)
	}
	if q.EnableAutoToolChoice {
		b.injectDictionary("enable_auto_tool_choice", true)
	}
	if q.LimitMMPerPrompt != "" {
		b.injectDictionary("limit_mm_per_prompt", q.LimitMMPerPrompt)
	}
	if q.MMProcessorKwargs != "" {
		b.injectDictionary("mm_processor_kwargs", q.MMProcessorKwargs)
	}
	if q.MediaIOKwargs != "" {
		b.injectDictionary("media_io_kwargs", q.MediaIOKwargs)
	}
	if q.AllowedLocalMediaPath != "" {
		b.injectDictionary("allowed_local_media_path", filepath.Clean(q.AllowedLocalMediaPath))
	}

	// --- quirks (data-driven corrections keyed by arch/model) ---
	applyQuirks(b, p, env)
	vllmPDEnv(p, env)

	argv := append(append(b.args(), templateArgs...), p.ExtraArgs...)

	return ResolvedLaunch{
		Entrypoint: entrypoint,
		Argv:       argv,
		Env:        env,
		EnvLayers:  proto.LaunchEnvLayers{Template: cloneEnv(p.TemplateEnv)},
	}
}

func stripEquivalentModelIdentity(args []string, modelPath string) []string {
	out := make([]string, 0, len(args))
	for index := 0; index < len(args); index++ {
		token := args[index]
		if token == "--model" || token == "--model-path" {
			if index+1 < len(args) && args[index+1] == modelPath {
				index++
				continue
			}
		}
		if strings.HasPrefix(token, "--model=") && strings.TrimPrefix(token, "--model=") == modelPath {
			continue
		}
		if strings.HasPrefix(token, "--model-path=") && strings.TrimPrefix(token, "--model-path=") == modelPath {
			continue
		}
		out = append(out, token)
	}
	return out
}

// PDConnectorFlags are the flags vllmKVTransferConfig composes: the
// connector config itself, and the KV offload flags, which next to it would
// replace it (measured: --kv-offloading-size alone drops NIXL).
func (vLLMEngine) PDConnectorFlags() []string {
	return []string{"--kv-transfer-config", "--kv-offloading-size", "--kv-offloading-backend"}
}

// vllmKVTransferConfig is the single assembly point of --kv-transfer-config.
// Without PD it is the disk tier alone. On a PD instance it is the mode's
// connector in the role's connector role; when KV offload is also on (only
// ever on the prefill role, see PDRoleLaunchConfig) the two become one
// MultiConnector, because a second flag would replace the first: measured on
// vLLM 0.25.1, --kv-offloading-size next to a NIXL config silently drops NIXL,
// so the CPU tier joins the composition too. Settings the engine only reads at
// the top level (the KV load failure policy) stay at the top level.
func vllmKVTransferConfig(p Params) (string, bool) {
	q := p.Quick
	if p.PD == nil {
		if q.KVDiskOffload {
			return diskKVTransferConfig(q), true
		}
		return "", false
	}
	mode := p.PD.mode()
	policy := mode.EffectiveKVLoadFailure(p.PD.KVLoadFailure)
	role := mode.ConnectorRole(p.PD.Role)
	pd := map[string]any{"kv_connector": mode.Connector, "kv_role": role}
	if mode.ConnectorPortKey != "" && p.PD.SideChannelPort > 0 {
		pd[mode.ConnectorPortKey] = strconv.Itoa(p.PD.SideChannelPort)
	}
	if mode.ConnectorParallelism {
		sides := map[string]any{}
		for _, r := range mode.Roles {
			sides[r] = map[string]any{"tp_size": max(p.PD.RoleTP[r], 1), "dp_size": 1}
		}
		pd["kv_connector_extra_config"] = sides
	}
	if len(mode.ConnectorExtra) > 0 || mode.Transport.MooncakeBootstrap() {
		extra, _ := pd["kv_connector_extra_config"].(map[string]any)
		if extra == nil {
			extra = map[string]any{}
		}
		for key, value := range mode.ConnectorExtra {
			extra[key] = value
		}
		// TransportMooncake follows the network the group shares; the RDMA
		// transport states its protocol in ConnectorExtra.
		if mode.Transport == pdmode.TransportMooncake {
			extra["mooncake_protocol"] = MooncakeProtocol(p.ComputeNetwork)
		}
		if hcas := hcaDevices(p.ComputeNetwork.HCAs); len(hcas) > 0 && extra["mooncake_protocol"] == "rdma" {
			extra["device_name"] = strings.Join(hcas, ",")
		}
		pd["kv_connector_extra_config"] = extra
	}
	// No engine_id: vLLM gives each engine process a fresh one, and the
	// gateway reads it from the prefill's bootstrap server
	// (pdmode.Mode.SynthesizedHandoff).
	var offload map[string]any
	switch {
	case q.KVDiskOffload:
		offload = diskKVTransferObject(q)
	case q.KVOffloadingSize > 0:
		offload = map[string]any{
			"kv_connector": "OffloadingConnector", "kv_role": "kv_both",
			"kv_connector_extra_config": map[string]any{"cpu_bytes_to_use": KVCPUTierBytes(q)},
		}
	}
	config := pd
	if offload != nil {
		config = map[string]any{
			"kv_connector": "MultiConnector", "kv_role": role,
			"kv_connector_extra_config": map[string]any{"connectors": []map[string]any{pd, offload}},
		}
	}
	if policy != "" {
		config["kv_load_failure_policy"] = policy
	}
	raw, _ := json.Marshal(config)
	return string(raw), true
}

func diskKVTransferConfig(q LaunchConfig) string {
	raw, _ := json.Marshal(diskKVTransferObject(q))
	return string(raw)
}

func diskKVTransferObject(q LaunchConfig) map[string]any {
	// The thread counts are folded into one composite flag, so an unset one has
	// no "emit nothing" spelling of its own: the dictionary's default is what
	// the connector is given.
	readThreads, writeThreads := q.KVOffloadReadThreads, q.KVOffloadWriteThreads
	if readThreads <= 0 {
		readThreads = defaultInt(EngineVLLM, "kv_offload_read_threads")
	}
	if writeThreads <= 0 {
		writeThreads = defaultInt(EngineVLLM, "kv_offload_write_threads")
	}
	extra := map[string]any{
		"spec_name":        "TieringOffloadingSpec",
		"cpu_bytes_to_use": KVCPUTierBytes(q),
		"secondary_tiers": []map[string]any{{
			"type": "fs", "root_dir": filepath.Clean(q.SharedKVDir),
			"n_read_threads": readThreads, "n_write_threads": writeThreads,
		}},
	}
	if q.KVOffloadBlockSize > 0 {
		extra["block_size"] = q.KVOffloadBlockSize
	}
	return map[string]any{
		"kv_connector": "OffloadingConnector", "kv_role": "kv_both",
		"kv_connector_extra_config": extra,
	}
}

func speculativeConfig(q LaunchConfig) string {
	config := map[string]any{}
	if q.SpeculativeAlgorithm != "" {
		config["method"] = q.SpeculativeAlgorithm
	}
	if q.SpeculativeDraftModel != "" {
		config["model"] = q.SpeculativeDraftModel
	}
	if q.SpeculativeTokens > 0 {
		config["num_speculative_tokens"] = q.SpeculativeTokens
	}
	raw, _ := json.Marshal(config)
	return string(raw)
}
