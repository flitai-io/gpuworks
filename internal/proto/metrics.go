package proto

// Metric and log batches are JSON Lines envelopes. Against a server that
// advertises CapTelemetryStream, an agent with a data lane sends them on its
// own StreamTelemetry streams there; otherwise (an older server, a connection
// with no data lane, or one whose data lane has not attached within 30 s of
// the handshake, until it does) they share the ctrl stream with heartbeats and
// RPC responses. Metric batches are best-effort; log batches
// wait in a bounded agent-side buffer while the node cannot send them, and a
// marker line reports any that buffer had to drop.

const (
	TMetrics = "metrics" // agent→server metric batch
	TLogs    = "logs"    // agent→server container-log batch
)

// MetricSample is one time-series point.
type MetricSample struct {
	Metric string            `json:"m"`
	Labels map[string]string `json:"l,omitempty"`
	Value  float64           `json:"v"`
	TimeMS int64             `json:"t"`
}

// MetricBatch is a batch of samples.
type MetricBatch struct {
	Samples []MetricSample `json:"samples"`
}

// LogLine is one container log line.
type LogLine struct {
	Instance string `json:"i"`
	TimeMS   int64  `json:"t"`
	Line     string `json:"line"`
	// Level is the severity the agent classified the line with ("" when the
	// line carried no recognizable stamp). Empty on batches from agents that
	// predate classification; the server stores it verbatim.
	Level string `json:"l,omitempty"`
	// Source separates instance logs (container output, the default) from
	// infrastructure logs: node (journald), agent, server. Empty means the
	// same as "instance" for old agents.
	Source string `json:"s,omitempty"`
}

// LogBatch is a batch of log lines.
type LogBatch struct {
	Lines []LogLine `json:"lines"`
}
