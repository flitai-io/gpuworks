package messages

// Multi-node preflight check outcomes (ADR 0067): the codes of the checks the
// inline preflight of an evaluation reports (dto.PreflightCheck) and of the
// node pairs the network preflight probes. The refusals the request validator
// shares with the preflight RPC (ErrorPreflight* in deployment.go) are reused
// as the failing outcome of the request-level checks.
const (
	// CheckPreflightNodeCountOK: {count} nodes are selected, within the range
	// the preflight accepts.
	CheckPreflightNodeCountOK = "PREFLIGHT_NODE_COUNT_OK"
	// CheckPreflightNodeListOK: every selected node is named once, with no
	// empty or padded UUID.
	CheckPreflightNodeListOK = "PREFLIGHT_NODE_LIST_OK"
	// CheckPreflightPortOK: the rendezvous port {port} is in range.
	CheckPreflightPortOK = "PREFLIGHT_PORT_OK"
	// CheckPreflightNodeOnline: node {node} exists and is online.
	CheckPreflightNodeOnline = "PREFLIGHT_NODE_ONLINE"
	// CheckPreflightNodeNotFound: no node {node} is registered.
	CheckPreflightNodeNotFound = "PREFLIGHT_NODE_NOT_FOUND"
	// CheckPreflightNodeUnreadable: node {node} could not be read from the
	// control plane's records.
	CheckPreflightNodeUnreadable = "PREFLIGHT_NODE_UNREADABLE"
	// CheckPreflightSharedNetworkOK: the selected nodes share network
	// {network}.
	CheckPreflightSharedNetworkOK = "PREFLIGHT_SHARED_NETWORK_OK"
	// CheckPreflightSharedNetworkUnknown: whether the nodes share a network
	// could not be determined.
	CheckPreflightSharedNetworkUnknown = "PREFLIGHT_SHARED_NETWORK_UNKNOWN"
	// CheckPreflightBlocked: the check was not run because check {blocked_by}
	// failed first.
	CheckPreflightBlocked = "PREFLIGHT_BLOCKED"

	// CheckPreflightPairReachable: node {source} reached node {target} at
	// {address}.
	CheckPreflightPairReachable = "PREFLIGHT_PAIR_REACHABLE"
	// CheckPreflightPairUnreachable: node {source} could not reach node
	// {target} at {address}.
	CheckPreflightPairUnreachable = "PREFLIGHT_PAIR_UNREACHABLE"
	// CheckPreflightListenerFailed: node {target} did not open the probe
	// listener on port {port}, so no node could probe it.
	CheckPreflightListenerFailed = "PREFLIGHT_LISTENER_FAILED"
	// CheckPreflightProbeFailed: node {source} did not run or answer the probe
	// to node {target}.
	CheckPreflightProbeFailed = "PREFLIGHT_PROBE_FAILED"
	// CheckPreflightBandwidthNotMeasured: node {source} reached node {target}
	// but the requested bandwidth was not measured.
	CheckPreflightBandwidthNotMeasured = "PREFLIGHT_BANDWIDTH_NOT_MEASURED"
)
