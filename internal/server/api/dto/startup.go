package dto

import "gpuworks.ai/gpuworks/internal/proto"

// StartupEstimate exposes a rough formula and its assumptions separately from
// the actual startup budget and observed engine timings.
type StartupEstimate struct {
	WeightBytes           int64    `json:"weight_bytes"`
	Method                string   `json:"method"`
	EstimatedSeconds      *float64 `json:"estimated_seconds,omitempty"`
	ReferenceMiBPerSec    int      `json:"reference_mib_per_sec,omitempty"`
	InitializationSeconds int      `json:"initialization_seconds,omitempty"`
}

func StartupEstimateOf(in *proto.StartupEstimate) *StartupEstimate {
	if in == nil {
		return nil
	}
	return &StartupEstimate{WeightBytes: in.WeightBytes, Method: in.Method, EstimatedSeconds: in.EstimatedSeconds, ReferenceMiBPerSec: in.ReferenceMiBPerSec, InitializationSeconds: in.InitializationSeconds}
}
