package dto

// DebugSessionStart opens a debug session. Zero duration or size takes the
// server's default (DebugSessionLimits).
type DebugSessionStart struct {
	DurationSeconds int      `json:"duration_seconds,omitempty"`
	MaxBytes        int64    `json:"max_bytes,omitempty"`
	DeploymentIDs   []uint   `json:"deployment_ids,omitempty"`
	NodeUUIDs       []string `json:"node_uuids,omitempty"`
}

// DebugSessionState is the open debug session, or the last one (nil before
// the first), with the bounds a new one may take.
type DebugSessionState struct {
	Session *DebugSession      `json:"session"`
	Limits  DebugSessionLimits `json:"limits"`
}

// DebugSessionLimits are the server's debug session bounds.
type DebugSessionLimits struct {
	DefaultDurationSeconds int   `json:"default_duration_seconds"`
	MinDurationSeconds     int   `json:"min_duration_seconds"`
	MaxDurationSeconds     int   `json:"max_duration_seconds"`
	DefaultMaxBytes        int64 `json:"default_max_bytes"`
	MaxMaxBytes            int64 `json:"max_max_bytes"`
	NodeMaxBytes           int64 `json:"node_max_bytes"`
	RequestMaxBytes        int64 `json:"request_max_bytes"`
	KeepDays               int   `json:"keep_days"`
}

// DebugSession is a debug session as it stands.
type DebugSession struct {
	ID            string             `json:"id"`
	StartedBy     string             `json:"started_by"`
	StartedAt     string             `json:"started_at"`
	Until         string             `json:"until"`
	MaxBytes      int64              `json:"max_bytes"`
	DeploymentIDs []uint             `json:"deployment_ids"`
	Nodes         []DebugSessionNode `json:"nodes"`
	Open          bool               `json:"open"`
	// Ended is why a closed session ended: deadline, size_limit or stopped.
	Ended          string `json:"ended,omitempty"`
	EndedAt        string `json:"ended_at,omitempty"`
	ServerLogBytes int64  `json:"server_log_bytes"`
	RequestBytes   int64  `json:"request_bytes"`
	Requests       int64  `json:"requests"`
}

// DebugSessionNode is one node's part: collecting, skipped (offline, or an
// agent without debug sessions: reason offline / unsupported) or failed.
type DebugSessionNode struct {
	UUID   string `json:"uuid"`
	Name   string `json:"name"`
	State  string `json:"state"`
	Reason string `json:"reason,omitempty"`
}

// DiagnosticsBundleStart begins a diagnostics collection. From and To are
// RFC 3339; both empty is the last hour. Doctor (default true) runs each
// collected node's diagnosis. With no node named, the nodes of the chosen
// instances (and deployments' instances) are collected.
type DiagnosticsBundleStart struct {
	From          string   `json:"from,omitempty"`
	To            string   `json:"to,omitempty"`
	DeploymentIDs []uint   `json:"deployment_ids,omitempty"`
	InstanceUUIDs []string `json:"instance_uuids,omitempty"`
	NodeUUIDs     []string `json:"node_uuids,omitempty"`
	Doctor        *bool    `json:"doctor,omitempty"`
}

// DiagnosticsBundles lists the kept collections, newest first, with the
// bounds a new one may take.
type DiagnosticsBundles struct {
	Bundles []DiagnosticsBundle      `json:"bundles"`
	Limits  DiagnosticsBundlesLimits `json:"limits"`
}

// DiagnosticsBundlesLimits are the server's collection bounds.
type DiagnosticsBundlesLimits struct {
	DefaultWindowSeconds int `json:"default_window_seconds"`
	MaxWindowSeconds     int `json:"max_window_seconds"`
	MaxNodes             int `json:"max_nodes"`
	MaxInstances         int `json:"max_instances"`
	KeepDays             int `json:"keep_days"`
}

// DiagnosticsBundle is a collection: running, ready (downloadable) or
// failed.
type DiagnosticsBundle struct {
	ID            string                  `json:"id"`
	RequestedBy   string                  `json:"requested_by"`
	CreatedAt     string                  `json:"created_at"`
	FinishedAt    string                  `json:"finished_at,omitempty"`
	From          string                  `json:"from"`
	To            string                  `json:"to"`
	DeploymentIDs []uint                  `json:"deployment_ids"`
	InstanceUUIDs []string                `json:"instance_uuids"`
	NodeUUIDs     []string                `json:"node_uuids"`
	Doctor        bool                    `json:"doctor"`
	State         string                  `json:"state"`
	Error         string                  `json:"error,omitempty"`
	SizeBytes     int64                   `json:"size_bytes"`
	Items         []DiagnosticsBundleItem `json:"items"`
}

// DiagnosticsBundleItem is one part of a collection, as a check (ADR 0067).
// ID names the kind of part (server_log, node_agent_log, ...), Path its place
// in the archive. Status is pass (collected whole), warn (cut at its size
// bound), skip (not read: the node is offline or its agent predates the read)
// or fail; Code and Params render the reason for every status but pass, and
// Message is the English fallback. Evidence is the failed read's own error
// text.
type DiagnosticsBundleItem struct {
	ID       string                       `json:"id"`
	Path     string                       `json:"path"`
	Status   string                       `json:"status"`
	Code     string                       `json:"code,omitempty"`
	Params   *DiagnosticsBundleItemParams `json:"params,omitempty"`
	Message  string                       `json:"message,omitempty"`
	Evidence string                       `json:"evidence,omitempty"`
	Bytes    int64                        `json:"bytes"`
}

// DiagnosticsBundleItemParams are the values the text of an item's code
// interpolates: the part's path and, for a node's part, the node.
type DiagnosticsBundleItemParams struct {
	Path string `json:"path"`
	Node string `json:"node,omitempty"`
}
