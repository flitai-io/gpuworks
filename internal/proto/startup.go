package proto

// StartupEstimate retains the launch-time basic formula and its reference
// assumptions. It is not a node measurement or a health-check budget.
// An absent EstimatedSeconds means unknown, never zero seconds.
type StartupEstimate struct {
	WeightBytes           int64    `json:"weight_bytes"`
	Method                string   `json:"method"`
	EstimatedSeconds      *float64 `json:"estimated_seconds,omitempty"`
	ReferenceMiBPerSec    int      `json:"reference_mib_per_sec,omitempty"`
	InitializationSeconds int      `json:"initialization_seconds,omitempty"`
}
