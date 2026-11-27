// Package messages defines language-neutral product messages shared by the
// server's domain services and API layer. Human-readable text belongs to the
// client locale resources; the wire contract contains only a stable code and
// interpolation parameters.
package messages

// Warning is a non-blocking, language-neutral product warning.
type Warning struct {
	Code   string         `json:"code"`
	Params map[string]any `json:"params,omitempty"`
}

// Warning codes used by deployment evaluation.
const (
	WarningModelNotFound                   = "MODEL_NOT_FOUND"
	WarningImageOverrideIncompatible       = "IMAGE_OVERRIDE_INCOMPATIBLE"
	WarningImageEngineDefaultIncompatible  = "IMAGE_ENGINE_DEFAULT_INCOMPATIBLE"
	WarningImageOverrideUnverified         = "IMAGE_OVERRIDE_UNVERIFIED"
	WarningImageDigestUnverified           = "IMAGE_DIGEST_UNVERIFIED"
	WarningImageCatalogNoMatch             = "IMAGE_CATALOG_NO_MATCH"
	WarningImageCatalogUnavailable         = "IMAGE_CATALOG_UNAVAILABLE"
	WarningPlanningEngineUnavailable       = "PLANNING_ENGINE_UNAVAILABLE"
	WarningPlanningGPUArchCaution          = "PLANNING_GPU_ARCH_CAUTION"
	WarningPlanningModelWeightUnknown      = "PLANNING_MODEL_WEIGHT_UNKNOWN"
	WarningImagePartitionProfileUnbuilt    = "IMAGE_PARTITION_PROFILE_UNBUILT"
	WarningImagePartitionProfileUnverified = "IMAGE_PARTITION_PROFILE_UNVERIFIED"
	WarningImageCatalogNoMatchDomestic     = "IMAGE_CATALOG_NO_MATCH_DOMESTIC"
	WarningImageCatalogUnavailableDomestic = "IMAGE_CATALOG_UNAVAILABLE_DOMESTIC"
	WarningImageDriverCompatibility        = "IMAGE_DRIVER_COMPATIBILITY"
	// WarningTemplateImagePinnedByRuntimePatch: the request runs a template
	// spec that patches its own runtime - a pre-start script or assets
	// pinned to the engine sources of its own image ({image}) - on another
	// image ({requested}). The catalog records that such a step exists but
	// not what it does, so the platform states the risk rather than deciding:
	// the patch may refuse the other image's sources and fail the start.
	WarningTemplateImagePinnedByRuntimePatch = "TEMPLATE_IMAGE_PINNED_BY_RUNTIME_PATCH"
	// WarningExtraArgsEngineUnverified: a residual user flag is documented by
	// another engine's dictionary, not this target's. It remains passthrough;
	// dictionary absence is not evidence that the target CLI refuses it.
	// Params: flag, engine, documented_engine.
	WarningExtraArgsEngineUnverified = "EXTRA_ARGS_ENGINE_UNVERIFIED"
	// WarningComputeNetworkFallback: some ranks have a compute network of
	// their own, but not all share it, so the group runs over the management
	// network.
	WarningComputeNetworkFallback = "COMPUTE_NETWORK_FALLBACK"
	// WarningPlanningKVBelowContext is an uncertain KV capacity prediction,
	// not a startup/resource refusal. Params: kv_tokens, max_model_len.
	WarningPlanningKVBelowContext = "PLANNING_KV_BELOW_CONTEXT"
	// WarningDGPUStartupMemory flags a tested configuration that may fail
	// Decode graph capture. Params: quota_gib, utilization, max_utilization.
	WarningDGPUStartupMemory = "DGPU_STARTUP_MEMORY"
	// WarningManualDraftUnavailable: a multi-node or PD draft has no manual
	// command script that runs every rank's own engine command, because ranks
	// of one kind differ in something no platform variable carries (the RDMA
	// device names of two nodes, say). The command cannot be edited from the
	// form; no params.
	WarningManualDraftUnavailable = "MANUAL_DRAFT_UNAVAILABLE"
	// WarningDockerArgPrivileged: a docker arg runs the container privileged
	// (every device, the host's kernel interfaces). It is allowed; the warning
	// says what it does. Params: arg (the token as written), option.
	WarningDockerArgPrivileged = "DOCKER_ARG_PRIVILEGED"
	// WarningDockerArgSharedPID: a docker arg (--pid) puts the container in
	// the host's or another container's process namespace, whose processes it
	// can then read and signal. Allowed and warned the same way. Params: arg,
	// option.
	WarningDockerArgSharedPID = "DOCKER_ARG_SHARED_PID"
)

// WarningKubeEnginePortsOutside: an edit of a Kubernetes entry's engine_ports
// left the ports some running instances hold outside the new range. The edit
// is saved and those instances keep their ports; only ports allocated later
// come from the new range. Params: range ("START-END"), instances (instance_uuid,
// deployment and ports, the ports outside the range, of each).
const WarningKubeEnginePortsOutside = "KUBE_ENGINE_PORTS_OUTSIDE"

// Launch warnings: the platform cannot prove a launch will work, or knows it
// depends on host conditions it cannot see. Evaluate reports them; create and
// the controller never fail on them (the engine log and instance state carry a
// real failure).
const (
	// WarningAscendV1CompatMultiNode: the node-local V1 rendezvous of the
	// vLLM-Ascend rc2 A3 compatibility script cannot coordinate ranks on
	// several nodes. Params: nnodes, runtime, image.
	WarningAscendV1CompatMultiNode = "ASCEND_V1_COMPAT_MULTI_NODE"
	// WarningAscendV1CompatDataParallel: the V1 rendezvous with a data-parallel
	// degree above one has not been verified on real hardware. Params: dp,
	// runtime, image.
	WarningAscendV1CompatDataParallel = "ASCEND_V1_COMPAT_DATA_PARALLEL"
	// WarningAscendCompatManualShell: a manual shell command runs next to the
	// compatibility script, so the platform cannot tell which model runner the
	// script patched for. Params: image, runtime.
	WarningAscendCompatManualShell = "ASCEND_COMPAT_MANUAL_SHELL"
	// WarningEngramSharedMemoryRuntime: Engram shared host memory on a runtime
	// that uses the host /dev/shm and ulimits directly. Params: runtime, flag,
	// shm_gib.
	WarningEngramSharedMemoryRuntime = "ENGRAM_SHARED_MEMORY_RUNTIME"
	// WarningEngramConfigUncertain: an Engram configuration whose shared-memory
	// use cannot be proven, on a runtime that uses the host /dev/shm. Params:
	// runtime, flag, shm_gib.
	WarningEngramConfigUncertain = "ENGRAM_CONFIG_UNCERTAIN"
	// WarningEngramManualUnchecked: a manual shell command mentions Engram but
	// cannot be read statically, so its shared-memory settings are not derived.
	// Params: flag.
	WarningEngramManualUnchecked = "ENGRAM_MANUAL_UNCHECKED"
	// WarningManualCommandPortUnstated: a manual shell command of a launch
	// that serves the API neither states the port flag the platform rewrites
	// nor references the API port variable, so the engine listens on its own
	// default port and the instance never becomes healthy unless that default
	// happens to be the assigned port. Params: flag, variable.
	WarningManualCommandPortUnstated = "MANUAL_COMMAND_PORT_UNSTATED"
	// WarningManualCommandPortTaken: a port a manual shell command states is
	// already taken on the node the preview serves the API from, so the
	// launch runs on the free port the platform assigns instead, as it does
	// for any stated port. Params: port, node, flag, variable.
	WarningManualCommandPortTaken = "MANUAL_COMMAND_PORT_TAKEN"
	// WarningAscendSleepExpandableSegmentsDisabled: the template's allocator
	// setting turns expandable segments on, which Ascend sleep mode cannot
	// run with, so the platform turned it off; NPU memory may fragment more.
	// Params: variable, template_value, value.
	WarningAscendSleepExpandableSegmentsDisabled = "ASCEND_SLEEP_EXPANDABLE_SEGMENTS_DISABLED"
)

// NewWarning constructs a warning while keeping empty parameter maps off the
// wire. It is intentionally small so domain packages do not need API helpers.
func NewWarning(code string, params map[string]any) Warning {
	if len(params) == 0 {
		params = nil
	}
	return Warning{Code: code, Params: params}
}

// Issue is a product error or warning in wire form where the two travel in
// lists: the embedded Warning is the stable code and the params that
// interpolate into the client's localized text; Error is the English text, the
// key an error response carries it under.
type Issue struct {
	Warning
	Error string `json:"error"`
}
