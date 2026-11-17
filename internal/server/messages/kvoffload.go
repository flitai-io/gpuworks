package messages

// KV offload backend product error codes (ADR 0069). Package deploy raises
// them when a deployment's kv_offload_backend selection cannot run as stated;
// params name the backend and what it collides with. Every one that judges a
// request field also carries {key}, the field the form marks.
const (
	// ErrorKVOffloadBackendUnknown: the engine has no selectable backend of
	// that id ({engine}, {backend}); also a stored row whose backend the
	// server no longer knows.
	ErrorKVOffloadBackendUnknown = "KV_OFFLOAD_BACKEND_UNKNOWN"
	// ErrorKVOffloadBackendConflictsNativeTier: the configuration states a
	// native KV offload parameter ({key}) the selected backend replaces.
	ErrorKVOffloadBackendConflictsNativeTier = "KV_OFFLOAD_BACKEND_CONFLICTS_NATIVE_TIER"
	// ErrorKVOffloadBackendConflictsArg: an argument layer states a flag
	// ({flag}) that would replace the backend's connector configuration.
	ErrorKVOffloadBackendConflictsArg = "KV_OFFLOAD_BACKEND_CONFLICTS_ARG"
	// ErrorKVOffloadBackendUnsupportedOnTarget: the backend does not run on
	// the target's accelerator vendor ({vendor}).
	ErrorKVOffloadBackendUnsupportedOnTarget = "KV_OFFLOAD_BACKEND_UNSUPPORTED_ON_TARGET"
	// ErrorKVOffloadBackendTopologyUnsupported: the backend does not run in
	// this launch topology ({nnodes} nodes, pipeline {pp}, data {dp}).
	ErrorKVOffloadBackendTopologyUnsupported = "KV_OFFLOAD_BACKEND_TOPOLOGY_UNSUPPORTED"
)
