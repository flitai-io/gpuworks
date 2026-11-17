package messages

// Diagnostics codes: debug sessions and diagnostics collections (package
// diagnostics), refused before anything starts.
const (
	// ErrorDebugSessionInvalid: a debug session request is out of range;
	// {field} is the setting (duration or max_bytes), {max} its upper bound
	// in that setting's unit (seconds or bytes).
	ErrorDebugSessionInvalid = "DEBUG_SESSION_INVALID"
	// ErrorDebugSessionOpen: a debug session ({session}) is already open;
	// it is stopped before another starts.
	ErrorDebugSessionOpen = "DEBUG_SESSION_OPEN"
	// ErrorDiagnosticsScopeUnknown: a debug session or collection names a deployment or node
	// ({id}) that does not exist.
	ErrorDiagnosticsScopeUnknown = "DIAGNOSTICS_SCOPE_UNKNOWN"
	// ErrorDiagnosticsBundleInvalid: a diagnostics collection request is out
	// of range; {field} is window (at most {max} seconds) or scope (at most
	// {max} nodes and instances).
	ErrorDiagnosticsBundleInvalid = "DIAGNOSTICS_BUNDLE_INVALID"
	// ErrorDiagnosticsBundleRunning: a collection ({bundle}) is being built;
	// one runs at a time.
	ErrorDiagnosticsBundleRunning = "DIAGNOSTICS_BUNDLE_RUNNING"
	// ErrorDiagnosticsUnavailable: this server runs without diagnostics.
	ErrorDiagnosticsUnavailable = "DIAGNOSTICS_UNAVAILABLE"
	// ErrorDiagnosticsBundleNotFound: no collection {bundle} is kept (or it is
	// not ready to download).
	ErrorDiagnosticsBundleNotFound = "DIAGNOSTICS_BUNDLE_NOT_FOUND"
)

// Diagnostics collection items (ADR 0067): why a part of a collection was
// not collected whole. {path} is the part's place in the archive, {node} the
// node a node part belongs to; a failed read's own error text is the item's
// evidence.
const (
	// CheckDiagnosticsItemTruncated (warn): the part reached its size bound
	// and only its start was kept.
	CheckDiagnosticsItemTruncated = "DIAGNOSTICS_ITEM_TRUNCATED"
	// CheckDiagnosticsNodeOffline (skip): the node was offline, so nothing
	// was read from it.
	CheckDiagnosticsNodeOffline = "DIAGNOSTICS_NODE_OFFLINE"
	// CheckDiagnosticsAgentUnsupported (skip): the node's agent predates the
	// diagnostics read.
	CheckDiagnosticsAgentUnsupported = "DIAGNOSTICS_AGENT_UNSUPPORTED"
	// CheckDiagnosticsItemReadFailed (fail): reading the part from its source
	// (a log, a node, the records) failed.
	CheckDiagnosticsItemReadFailed = "DIAGNOSTICS_ITEM_READ_FAILED"
	// CheckDiagnosticsItemStageFailed (fail): the part could not be written
	// to the server's work directory.
	CheckDiagnosticsItemStageFailed = "DIAGNOSTICS_ITEM_STAGE_FAILED"
)
