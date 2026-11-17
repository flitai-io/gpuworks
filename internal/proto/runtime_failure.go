package proto

import "strings"

// RuntimeFailure is the single structured reason an instance failed at run
// time (ADR 0067): the code says what happened, the typed fields are its
// parameters, and the client renders the text in its own locale. The
// accompanying InstanceStatus.Message stays the English fallback for old
// clients, logs and scripts; nothing matches on it.
//
// An agent attaches one to every failed report (and to the removing report of
// a failure whose cleanup is still pending); the server attaches one to every
// failure it decides itself. It is persisted whole in instances.runtime_failure
// (ADR 0032). It is a snapshot of what was observed, not an instruction to
// change the host. Only facts that cannot be derived from the instance row or
// from the code have a field.
type RuntimeFailure struct {
	Code string `json:"code"`

	// Accelerator facts of a refused injection (ErrorAccelerator* codes).
	Vendor          string `json:"vendor,omitempty"`
	OS              string `json:"os,omitempty"`
	Arch            string `json:"arch,omitempty"`
	Runtime         string `json:"runtime,omitempty"`
	RequiredOS      string `json:"required_os,omitempty"`
	RequiredArch    string `json:"required_arch,omitempty"`
	RequiredRuntime string `json:"required_runtime,omitempty"`
	// Resource names the device, path or card identity the failure is about.
	Resource string `json:"resource,omitempty"`
	// Feature is the first accelerator feature the target does not support
	// (accelerator.Feature value).
	Feature string `json:"feature,omitempty"`

	// Phase is the lifecycle phase the instance was in when it failed: a
	// Phase* value, PhaseServing once it had been running, or PhaseSleep.
	Phase string `json:"phase,omitempty"`
	// ExitCode is the container's exit status when it had exited.
	ExitCode *int `json:"exit_code,omitempty"`
	// OOMKilled says the runtime (or the engine's own out-of-memory error at
	// exit status 137) reported the container killed for memory.
	OOMKilled bool `json:"oom_killed,omitempty"`
	// Port is the host port a port failure is about.
	Port int `json:"port,omitempty"`
	// Detail is the text of an error from outside the platform's vocabulary
	// (a runtime CLI's stderr, a filesystem error, an engine's own exception),
	// shown verbatim inside the localized sentence. Bounded like Evidence.
	Detail string `json:"detail,omitempty"`
	// Rank and Node name the rank of a multi-node unit whose failure ended
	// the generation, when the failure is reported on the unit's leader.
	Rank *int   `json:"rank,omitempty"`
	Node string `json:"node,omitempty"`

	// Causes are the known engine failures the container's last output
	// shows (pkg/enginelog classifications), most specific first, at most
	// MaxFailureCauses.
	Causes []FailureCause `json:"causes,omitempty"`
	// Evidence is the line the engine's failure began with (its root cause),
	// at most MaxFailureEvidence lines of MaxFailureEvidenceChars.
	Evidence []string `json:"evidence,omitempty"`
}

// FailureCause is one known engine failure a container's output showed. Code
// is a pkg/enginelog failure code; the typed fields are the parameters the
// classifications that read them fill.
type FailureCause struct {
	Code     string   `json:"code"`
	Evidence []string `json:"evidence,omitempty"`
	// GLIBC is the glibc symbol version the dynamic loader could not find.
	GLIBC string `json:"glibc,omitempty"`
	// Device, NeededBytes and LimitBytes describe a dGPU memory limit
	// refusal: the CUDA device, what the engine would have held, its limit.
	Device      *int  `json:"device,omitempty"`
	NeededBytes int64 `json:"needed_bytes,omitempty"`
	LimitBytes  int64 `json:"limit_bytes,omitempty"`
}

// Bounds of a RuntimeFailure's evidence. A report crosses the control stream
// and is stored on the instance row, so its size must not follow the engine's
// output: four causes of three lines and three root-cause lines, each at most
// 300 bytes, keep it under 5 KiB.
const (
	MaxFailureCauses        = 4
	MaxFailureEvidence      = 3
	MaxFailureEvidenceChars = 300
)

// Instance runtime failure codes (ADR 0067). The agent and the server share
// them; every one has en/zh text under apiError.<CODE> in the console.
const (
	ErrorAcceleratorDeviceSelectionRequired = "ACCELERATOR_DEVICE_SELECTION_REQUIRED"
	ErrorAcceleratorRuntimeUnsupported      = "ACCELERATOR_RUNTIME_UNSUPPORTED"
	ErrorAcceleratorRuntimeUnreported       = "ACCELERATOR_RUNTIME_UNREPORTED"
	ErrorAcceleratorDeviceUnavailable       = "ACCELERATOR_DEVICE_UNAVAILABLE"
	ErrorAcceleratorInventoryUnavailable    = "ACCELERATOR_INVENTORY_UNAVAILABLE"
	ErrorAcceleratorResourceUnavailable     = "ACCELERATOR_RESOURCE_UNAVAILABLE"
	// ErrorAcceleratorFeatureUnsupported: the node's accelerator does not
	// support a feature the launch requests (Vendor, Feature).
	ErrorAcceleratorFeatureUnsupported = "ACCELERATOR_FEATURE_UNSUPPORTED"
	// ErrorLaunchFeaturesUnknown: the vendor needs to know which features a
	// custom command requests and the server could not tell (Vendor).
	ErrorLaunchFeaturesUnknown = "LAUNCH_FEATURES_UNKNOWN"
	// ErrorAcceleratorSMLimitUnsupported: a compute/SM limit was requested
	// on a vendor whose partition backend (if any) enforces memory only
	// (Vendor).
	ErrorAcceleratorSMLimitUnsupported = "ACCELERATOR_SM_LIMIT_UNSUPPORTED"
	// ErrorAcceleratorPartitionContract: a vendor that needs an explicit
	// device selection was given a partition backend that is not its own
	// (Vendor).
	ErrorAcceleratorPartitionContract = "ACCELERATOR_PARTITION_CONTRACT_INVALID"
	// ErrorLaunchFeaturesContradictory: the desired state's feature snapshot
	// contradicts its own topology or mounts.
	ErrorLaunchFeaturesContradictory = "LAUNCH_FEATURES_CONTRADICTORY"
	// ErrorAcceleratorInjectionUnavailable: the accelerator runtime could not
	// be injected into the container (Detail).
	ErrorAcceleratorInjectionUnavailable = "ACCELERATOR_INJECTION_UNAVAILABLE"
	// ErrorAcceleratorCardMissing: a card the instance is placed on is no
	// longer present on the node (Resource is its identity).
	ErrorAcceleratorCardMissing = "ACCELERATOR_CARD_MISSING"
	// ErrorDistributedNetworkUnavailable: no network interface could be
	// chosen for a multi-node engine (Detail).
	ErrorDistributedNetworkUnavailable = "DISTRIBUTED_NETWORK_UNAVAILABLE"
	// ErrorModelPathUnavailable: a model directory is missing or does not
	// answer (Resource is the path, Detail the error).
	ErrorModelPathUnavailable = "MODEL_PATH_UNAVAILABLE"
	// ErrorEnginePortRefused: a host port the server allocated was already
	// taken as the launch began (Port). The agent checks the ports before it
	// pulls an image or creates a container, so such an attempt started
	// nothing: the server retries it at once with other ports and does not
	// charge it to the retry budget (docs/adr/0029).
	ErrorEnginePortRefused = "ENGINE_PORT_REFUSED"
	// ErrorEnginePortInUse: another engine answered on the instance's API
	// port after its container started (Port, Detail is what it serves).
	// That attempt did start a container and is charged as any failure is.
	ErrorEnginePortInUse = "ENGINE_PORT_IN_USE"
	// ErrorImagePullFailed: no image source delivered the image (Detail).
	ErrorImagePullFailed = "IMAGE_PULL_FAILED"
	// ErrorContainerRuntimeUnavailable: the node's container runtime could
	// not be set up (Runtime, Detail).
	ErrorContainerRuntimeUnavailable = "CONTAINER_RUNTIME_UNAVAILABLE"
	// ErrorRuntimeAssetPrepareFailed, ErrorPreStartPrepareFailed and
	// ErrorKVDiskPrepareFailed: a host-side preparation of the container's
	// mounts failed (Detail).
	ErrorRuntimeAssetPrepareFailed = "RUNTIME_ASSET_PREPARE_FAILED"
	ErrorPreStartPrepareFailed     = "PRE_START_PREPARE_FAILED"
	ErrorKVDiskPrepareFailed       = "KV_DISK_PREPARE_FAILED"
	// ErrorDGPUPartitionFailed: the dGPU limit could not be applied (Detail).
	ErrorDGPUPartitionFailed = "DGPU_PARTITION_FAILED"
	// ErrorContainerRunFailed: the runtime refused to create or start the
	// container (Detail).
	ErrorContainerRunFailed = "CONTAINER_RUN_FAILED"
	// ErrorContainerDisappeared: the container was removed behind the agent.
	ErrorContainerDisappeared = "CONTAINER_DISAPPEARED"
	// ErrorContainerExited: the container exited (ExitCode, Causes, Evidence).
	ErrorContainerExited = "CONTAINER_EXITED"
	// ErrorContainerOOMKilled: the container was killed for memory (ExitCode).
	ErrorContainerOOMKilled = "CONTAINER_OOM_KILLED"
	// ErrorContainerUnhealthy: the runtime's health check failed a headless
	// worker that is still running.
	ErrorContainerUnhealthy = "CONTAINER_UNHEALTHY"
	// ErrorEngineHealthEndpointInvalid: the health endpoint the desired state
	// names cannot be requested.
	ErrorEngineHealthEndpointInvalid = "ENGINE_HEALTH_ENDPOINT_INVALID"
	// ErrorEngineHealthTimeout: the engine did not become healthy within its
	// start timeout (Detail is the last health result, if any).
	ErrorEngineHealthTimeout = "ENGINE_HEALTH_TIMEOUT"
	// ErrorWorkerReadinessTimeout: a headless worker did not stay up long
	// enough within its start timeout.
	ErrorWorkerReadinessTimeout = "WORKER_READINESS_TIMEOUT"
	// ErrorEngineUnresponsive: a running engine stopped answering its health
	// check (Detail is the last probe error).
	ErrorEngineUnresponsive = "ENGINE_UNRESPONSIVE"
	// ErrorCallableInitFailed: a callable serve adapter reported its own
	// initialization failure (Detail).
	ErrorCallableInitFailed = "CALLABLE_INIT_FAILED"
	// ErrorSleepPrewarmFailed: the startup sleep/wake prewarm failed (Detail).
	ErrorSleepPrewarmFailed = "SLEEP_PREWARM_FAILED"
	// ErrorSleepStateUncertain: a sleep or wake order left the engine in a
	// state the agent cannot know (Detail).
	ErrorSleepStateUncertain = "SLEEP_STATE_UNCERTAIN"

	// Failures the server decides itself.

	// ErrorServingPathUnavailable: the server cannot reach the node's serving
	// path, so the instance was never started (Detail).
	ErrorServingPathUnavailable = "SERVING_PATH_UNAVAILABLE"
	// ErrorUnitRankGone: a rank of a multi-node unit lost its row (Rank, Node).
	ErrorUnitRankGone = "UNIT_RANK_GONE"
	// ErrorUnitRankNodeOffline: a rank's node stayed offline beyond the
	// reconnect grace (Rank, Node).
	ErrorUnitRankNodeOffline = "UNIT_RANK_NODE_OFFLINE"
	// ErrorUnitRankFailed: a rank failed without a structured reason, from
	// an agent that predates them (Rank, Node, Detail is its message). A rank
	// with a structured reason passes its own code on, with Rank and Node.
	ErrorUnitRankFailed = "UNIT_RANK_FAILED"
)

// PhaseServing marks a failure of an instance that had been running.
const PhaseServing = "serving"

// Bounded returns f with its evidence, causes and detail cut to the wire
// bounds. The agent bounds what it reports; the server bounds again what it
// stores, since an agent is not trusted to.
func (f RuntimeFailure) Bounded() RuntimeFailure {
	f.Detail = clipFailureText(f.Detail)
	f.Evidence = boundEvidence(f.Evidence)
	if len(f.Causes) > MaxFailureCauses {
		f.Causes = f.Causes[:MaxFailureCauses]
	}
	if f.Causes != nil {
		causes := make([]FailureCause, len(f.Causes))
		for i, c := range f.Causes {
			c.Evidence = boundEvidence(c.Evidence)
			causes[i] = c
		}
		f.Causes = causes
	}
	return f
}

// Normalized returns f with its codes in UPPER_SNAKE, for the server's read
// boundary (older agents and older rows).
func (f RuntimeFailure) Normalized() RuntimeFailure {
	f.Code = NormalizeCode(f.Code)
	if f.Causes != nil {
		causes := make([]FailureCause, len(f.Causes))
		for i, c := range f.Causes {
			c.Code = NormalizeCode(c.Code)
			causes[i] = c
		}
		f.Causes = causes
	}
	return f
}

func boundEvidence(lines []string) []string {
	if len(lines) == 0 {
		return nil
	}
	if len(lines) > MaxFailureEvidence {
		lines = lines[len(lines)-MaxFailureEvidence:]
	}
	out := make([]string, len(lines))
	for i, line := range lines {
		out[i] = clipFailureText(line)
	}
	return out
}

// clipFailureText cuts s to MaxFailureEvidenceChars bytes on a rune boundary.
func clipFailureText(s string) string {
	s = strings.TrimSpace(s)
	if len(s) <= MaxFailureEvidenceChars {
		return s
	}
	cut := MaxFailureEvidenceChars - len("…")
	for cut > 0 && s[cut]&0xC0 == 0x80 {
		cut--
	}
	return s[:cut] + "…"
}
