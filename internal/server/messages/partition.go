package messages

// Why a dGPU (partition) capability is unavailable on a card, derived by
// internal/server/partitioncap from backend policy and the node's reports.
// The same code is the capability's reason_code and the code of the error
// that refuses a deployment asking for that backend. Params are the typed
// proto.PartitionReasonParams; on an error they also carry backend (the
// backend's display name) and node.
const (
	// ErrorPartitionRuntimeMissing: the node reported no usable container
	// runtime, so no dGPU container can start there.
	ErrorPartitionRuntimeMissing = "PARTITION_RUNTIME_MISSING"
	// ErrorPartitionRuntimeUnsupported: the backend is limited to
	// supported_runtimes and the node runs runtime.
	ErrorPartitionRuntimeUnsupported = "PARTITION_RUNTIME_UNSUPPORTED"
	// ErrorPartitionAgentUpgradeRequired: the agent predates the dGPU
	// safety protocol or the per-card probe report.
	ErrorPartitionAgentUpgradeRequired = "PARTITION_AGENT_UPGRADE_REQUIRED"
	// ErrorPartitionAssetsMissing: no reported asset root holds every file
	// the backend needs; missing_assets names the absent ones.
	ErrorPartitionAssetsMissing = "PARTITION_ASSETS_MISSING"
	// ErrorPartitionProbePending: the behavioural probe has no result yet,
	// for gpu_index when none arrived at all.
	ErrorPartitionProbePending = "PARTITION_PROBE_PENDING"
	// ErrorPartitionProbeSkipped: the agent skipped the probe, so container
	// inference under the backend is unverified; probe_detail says why.
	ErrorPartitionProbeSkipped = "PARTITION_PROBE_SKIPPED"
	// ErrorPartitionProbeFailed: the probe ran and failed; probe_detail is
	// its output.
	ErrorPartitionProbeFailed = "PARTITION_PROBE_FAILED"
	// ErrorPartitionBackendUnknown: a request named a backend the registry
	// does not have.
	ErrorPartitionBackendUnknown = "PARTITION_BACKEND_UNKNOWN"
	// ErrorPartitionBackendNotOnNode: a request named a backend none of the
	// node's cards can use.
	ErrorPartitionBackendNotOnNode = "PARTITION_BACKEND_NOT_ON_NODE"
)
