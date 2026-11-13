package proto

// Metrics and log shipping over the ctrl stream: JSON envelopes sharing the
// stream, and the agent's write lock, with heartbeats and RPC responses. The
// agent drops a batch emitted while it has no bound connection.

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
