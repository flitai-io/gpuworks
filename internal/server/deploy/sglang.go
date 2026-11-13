// sglang.go is the SGLang engine adapter: registry identity, engine profile and
// the deterministic `sglang.launch_server` argv assembly.

package deploy

import (
	"net"
	"strconv"
	"strings"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/pdmode"
)

type sglangEngine struct{ BaseEngine }

// sglangReserveBase is what SGLang holds back per GPU besides its weights and
// KV cache. Unlike vLLM's measured peak, SGLang carves its pool out of a
// fraction of the card, so the number is flat: neither the architecture nor the
// capture-graph switch moves it. Measured on real hardware; the measurements
// and the harness are in scripts/estimate-calib/.
const sglangReserveBase int64 = 205 << 20

func (sglangEngine) Name() string { return EngineSGLang }

// TPFlags: SGLang accepts the same three spellings as vLLM and injects
// --tp-size.
func (sglangEngine) TPFlags() []string { return sglangTensorParallelFlags }

// SplitCommand: an SGLang launch encodes everything in the entrypoint
// ("python -m sglang.launch_server ...") and leaves Argv empty, which is why
// the parameter vector is the entrypoint tail alone. The shape is the same one
// parameter applicability reads, so a catalog entrypoint and the adapter's own
// command are recognized in exactly one place.
func (sglangEngine) SplitCommand(launch ResolvedLaunch) ([]string, []string) {
	entry := launch.Entrypoint
	if isSGLangLLMEntrypoint(entry) {
		return entry[:3], append([]string(nil), entry[3:]...)
	}
	return launch.Entrypoint, launch.Argv
}

func (sglangEngine) Profile() Profile {
	dict := paramDictForEngine(EngineSGLang)
	return Profile{
		Name: EngineSGLang, DefaultImage: "lmsysorg/sglang:latest", HealthPath: "/health", ShmBytes: 16 << 30,
		MetricPath: "/metrics",
		Sleep:      SleepCaps{Supported: false}, MetricPrefix: EngineSGLang,
		ReserveBase: sglangReserveBase,
		// The mapping itself lives in internal/inferencemetrics (engines.go);
		// MetricMap is its stored-series view for the engines API.
		MetricMap: EngineMetricMap(EngineSGLang),
		ParamDict: dict, ParamDefinitions: structuredParams(EngineSGLang, dict),
		QuickFormKeys: []string{
			"enable_metrics", "log_level",
			"max_model_len", "dtype", "quantization", "gpu_memory_utilization",
			"max_num_seqs", "max_num_batched_tokens", "enable_prefix_caching",
			"data_parallel_size",
			"enable_cache_report",
			"kv_cache_dtype", "reasoning_parser", "tool_call_parser",
			"enable_auto_tool_choice", "trust_remote_code", "enforce_eager",
		},
	}
}

func (sglangEngine) Build(p Params) ResolvedLaunch {
	if p.TensorParallel <= 0 {
		p.TensorParallel = 1
	}
	if p.APIPort <= 0 && !p.Headless {
		p.APIPort = 8000
	}
	if p.Host == "" {
		p.Host = "127.0.0.1"
	}
	blockers, templateArgs := residualLayers(p.TemplateArgs, p.ExtraArgs)
	userFlags := blockers
	// Normalize supported aliases so user arguments retain highest priority.
	for _, flag := range sglangTensorParallelFlags {
		if userFlags[flag] {
			userFlags[sglangTensorParallelFlags[0]] = true
		}
	}
	for _, flag := range sglangPipelineParallelFlags {
		if userFlags[flag] {
			userFlags[sglangPipelineParallelFlags[0]] = true
		}
	}
	if userFlags["--nccl-init-addr"] {
		userFlags["--dist-init-addr"] = true
	}
	b := newBuilder(EngineSGLang, userFlags)
	b.injectKV("--model-path", p.ModelPath)
	// TP and PP are the deployment's own degrees; SGLang divides the TP×PP
	// ranks by nnodes to get each node's local ranks.
	b.injectKV(sglangTensorParallelFlags[0], strconv.Itoa(p.TensorParallel))
	if p.PipelineParallel > 1 {
		b.injectKV(sglangPipelineParallelFlags[0], strconv.Itoa(p.PipelineParallel))
	}
	// Non-zero SGLang ranks start an official dummy health server. Port 0 lets
	// the OS choose an unpublished port while gpuworks keeps the rank headless.
	host := p.Host
	if p.PD != nil && p.PD.Role == pdmode.RolePrefill {
		// SGLang's bootstrap server binds the engine host, and decoders on
		// other nodes dial it; the API port becomes reachable on the host
		// network too, the same exposure multi-node inference already has.
		host = "0.0.0.0"
	}
	b.injectKV("--host", host)
	b.injectKV("--port", strconv.Itoa(p.APIPort))
	if servedName := p.EngineServedName(); servedName != "" && !p.Headless {
		b.injectKV("--served-model-name", servedName)
	}
	if p.NNodes > 1 {
		b.injectKV("--nnodes", strconv.Itoa(p.NNodes))
		b.injectKV("--node-rank", strconv.Itoa(p.NodeRank))
		if p.MasterAddr != "" && p.MasterPort > 0 {
			b.injectKV("--dist-init-addr", net.JoinHostPort(p.MasterAddr, strconv.Itoa(p.MasterPort)))
		}
	}
	q := p.Quick
	if q.LogLevel != "" {
		b.injectDictionary("log_level", q.LogLevel)
	}
	if q.EnableMetrics {
		b.injectDictionary("enable_metrics", true)
	}
	if q.MaxModelLen > 0 {
		b.injectDictionary("max_model_len", q.MaxModelLen)
	}
	if q.GPUMemUtil > 0 {
		b.injectDictionary("gpu_memory_utilization", q.GPUMemUtil)
	}
	if q.MaxNumBatchedTokens > 0 {
		b.injectDictionary("max_num_batched_tokens", q.MaxNumBatchedTokens)
	}
	if q.MaxNumSeqs > 0 {
		b.injectDictionary("max_num_seqs", q.MaxNumSeqs)
	}
	if q.KVCacheDtype != "" && q.KVCacheDtype != "auto" {
		b.injectDictionary("kv_cache_dtype", q.KVCacheDtype)
	}
	if q.Dtype != "" && q.Dtype != "auto" {
		b.injectDictionary("dtype", q.Dtype)
	}
	if q.Quantization != "" {
		b.injectDictionary("quantization", q.Quantization)
	}
	// The dictionary owns both polarities: SGLang has no positive radix-cache
	// flag, so enabling it emits nothing, and enforce_eager is spelled by its
	// own negative-sounding flag.
	b.injectDictionaryBool("enable_prefix_caching", q.EnablePrefixCaching)
	b.injectDictionaryBool("enable_cache_report", q.EnableCacheReport)
	if q.EnforceEager {
		b.injectDictionary("enforce_eager", true)
	}
	if q.SpeculativeAlgorithm != "" {
		b.injectDictionary("speculative_algorithm", q.SpeculativeAlgorithm)
	}
	if q.SpeculativeDraftModel != "" {
		b.injectDictionary("speculative_draft_model", q.SpeculativeDraftModel)
	}
	if q.SpeculativeNumSteps > 0 {
		b.injectDictionary("speculative_num_steps", q.SpeculativeNumSteps)
	}
	if q.SpeculativeEagleTopK > 0 {
		b.injectDictionary("speculative_eagle_top_k", q.SpeculativeEagleTopK)
	}
	if q.SpeculativeTokens > 0 {
		b.injectDictionary("speculative_tokens", q.SpeculativeTokens)
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
	if q.TrustRemote {
		b.injectDictionary("trust_remote_code", true)
	}
	if q.DP > 0 {
		b.injectDictionary("data_parallel_size", q.DP)
	}
	if q.LoRA {
		b.injectDictionary("lora", true)
	}
	env := map[string]string{"HF_HUB_OFFLINE": "1", "TRANSFORMERS_OFFLINE": "1"}
	sglangPD(b, p, env)
	entry := append([]string{"python", "-m", "sglang.launch_server"}, b.args()...)
	entry = append(entry, templateArgs...)
	entry = append(entry, p.ExtraArgs...)
	return ResolvedLaunch{Entrypoint: entry, Env: env, EnvLayers: proto.LaunchEnvLayers{Template: cloneEnv(p.TemplateEnv)}}
}

// PDConnectorFlags are the disaggregation flags sglangPD composes.
func (sglangEngine) PDConnectorFlags() []string {
	return []string{"--disaggregation-mode", "--disaggregation-transfer-backend", "--disaggregation-bootstrap-port"}
}

// sglangPD injects a PD instance's disaggregation role, transfer backend and,
// on the prefill side, the bootstrap port decoders pair with it on (ADR 0028),
// plus the flags and environment the mode declares. Under NIXL the transfer
// backend is UCX, so the network device rules are the same as vLLM's.
func sglangPD(b *builder, p Params, env map[string]string) {
	if p.PD == nil {
		return
	}
	mode := p.PD.mode()
	b.injectKV("--disaggregation-mode", mode.ConnectorRole(p.PD.Role))
	b.injectKV("--disaggregation-transfer-backend", mode.Connector)
	if p.PD.Role == pdmode.RolePrefill && p.PD.SideChannelPort > 0 {
		b.injectKV("--disaggregation-bootstrap-port", strconv.Itoa(p.PD.SideChannelPort))
	}
	for _, flag := range mode.EngineArgs {
		b.injectFlag(flag)
	}
	// Mooncake over RDMA registers the cards with the HCAs the group's host
	// RDMA network names; left to itself it picks among every HCA on the host.
	if mode.Transport == pdmode.TransportMooncakeRDMA {
		if hcas := hcaDevices(p.ComputeNetwork.HCAs); len(hcas) > 0 {
			b.injectKV("--disaggregation-ib-device", strings.Join(hcas, ","))
		}
	}
	for key, value := range mode.Env {
		env[key] = value
	}
	// SGLang's prefill registers with its own bootstrap server at its --host
	// (0.0.0.0 on a prefill instance), and a decoder queries it at the
	// instance's PD address, both over HTTP. A container runtime
	// that injects a proxy (Docker's client proxies, for one) would carry
	// those calls to the proxy, which answers 502 for a host it cannot reach:
	// measured on a PPU host whose no_proxy did not list the node address.
	// Stating the variables keeps the runtime from injecting its own; the
	// engine reads its weights offline, so nothing else it does needs a proxy.
	pdNoProxy(p, env)
	for key, value := range kvTransferNetworkEnv(p) {
		env[key] = value
	}
}
