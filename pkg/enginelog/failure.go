package enginelog

import (
	"regexp"
	"strconv"
	"strings"
)

// Engine failure codes: the known failures an engine's output can show
// (ADR 0067). They are the one vocabulary of engine-log classification: the
// agent reports them with an instance's runtime failure, and the server's
// assistant diagnoses by them. UPPER_SNAKE like every failure code.
const (
	CodeGLIBCTooOld             = "GLIBC_TOO_OLD"
	CodeDGPUMemoryLimit         = "DGPU_MEMORY_LIMIT_EXCEEDED"
	CodeKVCacheInsufficient     = "KV_CACHE_INSUFFICIENT"
	CodeCUDAOOM                 = "CUDA_OOM"
	CodeSHMInsufficient         = "SHM_INSUFFICIENT"
	CodeNCCLFailure             = "NCCL_FAILURE"
	CodeDiskFull                = "DISK_FULL"
	CodePortInUse               = "PORT_IN_USE"
	CodeImagePullDenied         = "IMAGE_PULL_DENIED"
	CodeDtypeUnsupported        = "DTYPE_UNSUPPORTED"
	CodeGPUArchUnsupported      = "GPU_ARCH_UNSUPPORTED"
	CodeTrustRemoteCodeRequired = "TRUST_REMOTE_CODE_REQUIRED"
	CodeModelArchUnsupported    = "MODEL_ARCH_UNSUPPORTED"
	CodeModelFilesMissing       = "MODEL_FILES_MISSING"
	CodeHFDownloadFailed        = "HF_DOWNLOAD_FAILED"
	// CodeNoOutput: the engine exited without printing anything, which a
	// full disk also produces. Classify reports it; Match, reading lines
	// someone else kept, cannot tell nothing from not kept.
	CodeNoOutput = "ENGINE_NO_OUTPUT"
)

// MaxEvidence bounds the lines kept per cause: the ones nearest the failure,
// which is where the cause usually prints.
const MaxEvidence = 3

// Cause is one known failure an engine's output shows. The typed fields are
// the parameters of the classifications that read them.
type Cause struct {
	Code     string
	Evidence []Evidence
	// GLIBC is the missing glibc symbol version (CodeGLIBCTooOld).
	GLIBC string
	// Device, NeededBytes and LimitBytes are the CUDA device, the memory
	// the engine would have held and its limit (CodeDGPUMemoryLimit).
	Device      int
	NeededBytes int64
	LimitBytes  int64
}

// Evidence is one line that matched. Index is its position in the lines the
// classification was given, so a caller can point back into its own view.
type Evidence struct {
	Index int
	Line  string
}

// Lines returns the evidence lines alone.
func (c Cause) Lines() []string {
	out := make([]string, len(c.Evidence))
	for i, e := range c.Evidence {
		out[i] = e.Line
	}
	return out
}

// Fallback is the English sentence for the cause, for logs and the failure
// message old clients read. Clients render the code in their own locale.
func (c Cause) Fallback() string {
	switch c.Code {
	case CodeGLIBCTooOld:
		return "container glibc too old: missing " + c.GLIBC + "; Ascend dGPU images require glibc 2.33 or newer"
	case CodeDGPUMemoryLimit:
		const gib = 1 << 30
		return "dGPU memory limit reached on GPU " + strconv.Itoa(c.Device) + ": the engine needed " +
			strconv.FormatFloat(float64(c.NeededBytes)/gib, 'f', 1, 64) + " GiB, its limit is " +
			strconv.FormatFloat(float64(c.LimitBytes)/gib, 'f', 1, 64) + " GiB"
	}
	return strings.ToLower(strings.ReplaceAll(c.Code, "_", " "))
}

type signature struct {
	code string
	// match fires on any line it matches unless exclude also matches it.
	match   *regexp.Regexp
	exclude *regexp.Regexp
	// params reads the cause's parameters from the newest matching line.
	params func(match []string, c *Cause)
	// hypothesis is what an LLM is asked to verify, in English: it is a
	// prompt, not interface text, and the model answers in the user's
	// language whatever the prompt's.
	hypothesis string
}

func re(pattern string) *regexp.Regexp { return regexp.MustCompile(`(?i)` + pattern) }

// signatures is the known-failure table, the only one. Order is the order
// causes are reported in, most specific first; a line may match several (an
// OOM during NCCL setup is both).
var signatures = []signature{
	{
		code:  CodeGLIBCTooOld,
		match: regexp.MustCompile("(?i)(?:version [`']?)?(GLIBC_[0-9]+(?:\\.[0-9]+)+)(?:[`']? not found|[^\\n]*required)"),
		params: func(m []string, c *Cause) {
			c.GLIBC = strings.ToUpper(m[1])
		},
		hypothesis: "The container's glibc is older than a library injected into it needs (the dynamic loader names the missing GLIBC_x.y). Ascend dGPU images require glibc 2.33 or newer; fixes are a newer base image or running without the dGPU limit.",
	},
	{
		// The line HAMi-core (the NVIDIA dGPU interposer) prints when it
		// refuses an allocation that would take the instance past its memory
		// limit: "Device <cuda index> OOM <bytes it would hold> / <limit>".
		// The engine's own report of the refusal is a generic error raised far
		// from it, so without this the failure names a symptom, not the limit.
		code:  CodeDGPUMemoryLimit,
		match: regexp.MustCompile(`HAMI-core ERROR[^\n]*?Device (\d+) OOM (\d+) / (\d+)`),
		params: func(m []string, c *Cause) {
			c.Device, _ = strconv.Atoi(m[1])
			c.NeededBytes, _ = strconv.ParseInt(m[2], 10, 64)
			c.LimitBytes, _ = strconv.ParseInt(m[3], 10, 64)
		},
		hypothesis: "The engine ran into the instance's dGPU memory limit: the interposer refused an allocation past it. Compare the limit with the weights per GPU plus KV cache; fixes are a larger dGPU memory limit, a lower gpu_memory_utilization or max_model_len, or a whole GPU.",
	},
	{
		code:       CodeKVCacheInsufficient,
		match:      re(`larger than the maximum number of tokens that can be stored in KV cache|KV cache is needed, which is larger than the available KV cache memory|No available memory for the cache blocks|Insufficient memory for KV cache|increase --mem-fraction-static|not enough memory.*kv cache`),
		hypothesis: "The KV cache does not fit what is left after the weights: max_model_len (or the requested sequence length) needs more KV cache than the GPU memory budget leaves. Check the weight size, max_model_len and gpu_memory_utilization; fixes are a smaller --max-model-len, a higher memory utilization, or a larger tensor-parallel size.",
	},
	{
		code:       CodeCUDAOOM,
		match:      re(`CUDA out of memory|OutOfMemoryError|CUDA error: out of memory|NPU out of memory|out of memory.*(GPU|device)`),
		hypothesis: "The process ran out of accelerator memory. Check whether the weights alone fit one GPU's share (weight size / tensor-parallel size), whether another process holds memory on the same GPU, and whether max_model_len or gpu_memory_utilization leave too little headroom.",
	},
	{
		code:       CodeSHMInsufficient,
		match:      re(`failed to extend /dev/shm|creating shared memory segment|/dev/shm has only|No space left on device.*shm|shm_open|unable to (allocate|open) shared memory|Bus error|shared memory.*(insufficient|too small|exhausted)`),
		hypothesis: "Shared memory (/dev/shm) is too small for the engine's inter-process or NCCL traffic. Check the container's shm size; fixes are a larger --shm-size / ipc=host, or disabling NCCL shared-memory transport.",
	},
	{
		code:       CodeNCCLFailure,
		match:      re(`NCCL error|ncclInternalError|ncclSystemError|ncclUnhandledCudaError|ncclRemoteError|ncclInvalidUsage|Watchdog caught collective operation timeout|ProcessGroupNCCL.*(timeout|timed out|error)|NCCL WARN|HCCL function error|hccl api failed|Communication_Error`),
		hypothesis: "Collective communication (NCCL, or HCCL on Ascend) between accelerators or nodes failed. Check the topology and P2P support, the network interface it picked (NCCL_SOCKET_IFNAME / HCCL_SOCKET_IFNAME, IB/RoCE devices), firewalls between nodes, and whether every rank started. A shared-memory error underneath points at /dev/shm instead.",
	},
	{
		code:       CodeDiskFull,
		match:      re(`No space left on device|disk quota exceeded`),
		exclude:    re(`shm`),
		hypothesis: "A disk filled up (model cache, image layers or container writable layer). Check free space on the node's model storage and container runtime directories.",
	},
	{
		code:       CodePortInUse,
		match:      re(`address already in use|EADDRINUSE|port is already allocated`),
		hypothesis: "The serving port is already taken by another process or container on the node. Check what listens on the instance's port.",
	},
	{
		code:       CodeImagePullDenied,
		match:      re(`pull access denied|unauthorized: authentication required|manifest unknown|repository does not exist|failed to resolve reference|denied: requested access|ErrImagePull`),
		hypothesis: "The container image could not be pulled: wrong name/tag, a private registry without credentials, or an unreachable registry. Check the image reference and registry credentials on the node.",
	},
	{
		code:       CodeDtypeUnsupported,
		match:      re(`Bfloat16 is only supported on GPUs with compute capability|bfloat16 is not supported|does not support (bf16|bfloat16|fp8)|fp8 .*not supported`),
		hypothesis: "The GPU does not support the requested dtype (for example bfloat16 below compute capability 8.0). Fix by serving with --dtype float16 or using a supported GPU.",
	},
	{
		code:       CodeGPUArchUnsupported,
		match:      re(`no kernel image is available for execution on the device|is not compatible with the current PyTorch installation|sm_[0-9]+ is not compatible|requires compute capability|unsupported (gpu|compute capability)`),
		hypothesis: "The image's kernels were not built for this GPU architecture. Check the GPU's compute capability against the image/engine build; fixes are an image built for that architecture or a different GPU.",
	},
	{
		code: CodeTrustRemoteCodeRequired,
		// Engines also print this option in configuration dumps and ignored-option
		// warnings. Only a request for permission is evidence of a refusal.
		match:      re("requires you to execute the configuration file in that repo|(?:please (?:pass|set)|make sure (?:that )?you set|you need to (?:pass|set)) (?:the (?:argument|option) )?[`'\"]?trust_remote_code\\s*=\\s*true\\b"),
		hypothesis: "The model ships custom code and the engine refused to load it without --trust-remote-code. Check whether the model's repository is trusted before enabling it.",
	},
	{
		code:       CodeModelArchUnsupported,
		match:      re(`Model architectures? \[.*\] (are|is) not supported|are not supported for now|Unsupported architecture|Unknown model architecture|does not recognize this architecture`),
		hypothesis: "The engine version does not support this model architecture. Check the architecture in config.json against the engine version; fixes are a newer engine image or another engine.",
	},
	{
		code:       CodeModelFilesMissing,
		match:      re(`does not appear to have a file named|Can't load (the )?(config|tokenizer)|No such file or directory: .*(config\.json|tokenizer|safetensors|\.bin)|Incorrect path_or_model_id|Invalid repository ID or local directory specified|Could not detect config format|is not a local folder and is not a valid model identifier|Unrecognized model in`),
		hypothesis: "The model directory the engine was pointed at is missing files (config.json, tokenizer or weights) or the path is wrong. Check the model path in the launch command and whether the download completed.",
	},
	{
		code:       CodeHFDownloadFailed,
		match:      re(`Repository Not Found|GatedRepoError|gated repo|401 Client Error|403 Client Error|LocalEntryNotFoundError|couldn't connect to 'https://huggingface\.co'|huggingface\.co.*(timed out|Max retries|Connection)`),
		hypothesis: "Downloading from the model hub failed: a wrong repository id, a gated model without an accepted license/token, or no network route to the hub. Check the repository id, token and the node's network/proxy.",
	},
}

// noOutputHypothesis is CodeNoOutput's: it has no line to match.
const noOutputHypothesis = "The engine exited without printing anything. Check the node's free disk space (a full disk produces exactly this), the image's entrypoint, and whether the runtime could start the process at all."

// Codes lists every failure code the classifications report, in table order,
// then CodeNoOutput.
func Codes() []string {
	out := make([]string, 0, len(signatures)+1)
	for _, s := range signatures {
		out = append(out, s.code)
	}
	return append(out, CodeNoOutput)
}

// Hypothesis is what an LLM is asked to verify for code; empty for a code
// that is not an engine failure code.
func Hypothesis(code string) string {
	if code == CodeNoOutput {
		return noOutputHypothesis
	}
	for _, s := range signatures {
		if s.code == code {
			return s.hypothesis
		}
	}
	return ""
}

// Match runs the known-failure table over lines (oldest first). Each code is
// reported once, with the matching lines nearest the end and the parameters
// read from the newest.
func Match(lines []string) []Cause {
	var out []Cause
	for _, s := range signatures {
		var ev []Evidence
		for i, line := range lines {
			if !s.match.MatchString(line) || (s.exclude != nil && s.exclude.MatchString(line)) {
				continue
			}
			ev = append(ev, Evidence{Index: i, Line: line})
		}
		if len(ev) == 0 {
			continue
		}
		if len(ev) > MaxEvidence {
			ev = ev[len(ev)-MaxEvidence:]
		}
		c := Cause{Code: s.code, Evidence: ev}
		if s.params != nil {
			s.params(s.match.FindStringSubmatch(ev[len(ev)-1].Line), &c)
		}
		out = append(out, c)
	}
	return out
}

// Classify reads the output a stopped container left: the known failures its
// lines show, or CodeNoOutput when it printed nothing. Terminal control
// sequences are removed first.
func Classify(logText string) []Cause {
	if strings.TrimSpace(logText) == "" {
		return []Cause{{Code: CodeNoOutput}}
	}
	return Match(strings.Split(ansiRE.ReplaceAllString(logText, ""), "\n"))
}
