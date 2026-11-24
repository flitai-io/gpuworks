package proto

import "context"

// RPCDebugStart opens a debug session on the agent: until UntilMS, or until
// its file holds MaxBytes, every agent log record at debug and above is also
// written to <state>/debug/<SessionID>.log; the agent's own log keeps its
// level. RPCDebugStop ends the session early. An agent that predates them
// answers an unknown-RPC error, which the server records as not collected.
const (
	RPCDebugStart = "debug.start"
	RPCDebugStop  = "debug.stop"
)

type DebugStartParams struct {
	SessionID string `json:"session_id"`
	UntilMS   int64  `json:"until_ms"`
	MaxBytes  int64  `json:"max_bytes"`
}

type DebugStopParams struct {
	SessionID string `json:"session_id"`
}

// DebugSessionResult is the agent's session after a start or stop.
type DebugSessionResult struct {
	SessionID    string `json:"session_id"`
	Path         string `json:"path"`
	Open         bool   `json:"open"`
	WrittenBytes int64  `json:"written_bytes"`
	Ended        string `json:"ended,omitempty"`
}

// TunnelTrace joins one captured gateway attempt to its agent-side stream.
type TunnelTrace struct {
	RequestID string `json:"request_id"`
	Attempt   int    `json:"attempt"`
}

type tunnelTraceKey struct{}

func WithTunnelTrace(ctx context.Context, trace *TunnelTrace) context.Context {
	return context.WithValue(ctx, tunnelTraceKey{}, trace)
}

func TunnelTraceFrom(ctx context.Context) *TunnelTrace {
	trace, _ := ctx.Value(tunnelTraceKey{}).(*TunnelTrace)
	return trace
}
