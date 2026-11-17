package proto

import "time"

// PartitionCapability is the server-derived, per-GPU partition fact exposed
// through the API. Status has exactly two values: ready or unavailable.
type PartitionCapability struct {
	Backend     string `json:"backend"`
	DisplayName string `json:"display_name"`
	Vendor      string `json:"vendor"`
	Maturity    string `json:"maturity"`
	Status      string `json:"status"`
	// Reason is the English fallback of ReasonCode, for scripts and older
	// clients; clients render ReasonCode with ReasonParams instead.
	Reason string `json:"reason,omitempty"`
	// ReasonCode says why an unavailable capability is not ready
	// (messages.ErrorPartition*); empty when Status is ready.
	ReasonCode         string                 `json:"reason_code,omitempty"`
	ReasonParams       *PartitionReasonParams `json:"reason_params,omitempty"`
	Compatibility      string                 `json:"compatibility_summary,omitempty"`
	CompatibilityNotes []string               `json:"compatibility_notes,omitempty"`
	ProbeStatus        string                 `json:"probe_status,omitempty"`
	// ProbeCheckedAt is absent until a probe result arrived: a zero time on
	// the wire rendered as "checked 0001-01-01".
	ProbeCheckedAt *time.Time `json:"probe_checked_at,omitempty"`
}

// PartitionReasonParams are the facts a capability's ReasonCode names. Only
// the fields that code uses are set.
type PartitionReasonParams struct {
	// Runtime is the node's container runtime driver and SupportedRuntimes
	// the drivers the backend is limited to.
	Runtime           string   `json:"runtime,omitempty"`
	SupportedRuntimes []string `json:"supported_runtimes,omitempty"`
	// MissingAssets are the files no single reported asset root holds.
	MissingAssets []string `json:"missing_assets,omitempty"`
	// GPUIndex is the card whose probe result has not arrived.
	GPUIndex *int `json:"gpu_index,omitempty"`
	// ProbeDetail is the agent's own probe output (PartitionProbeResult.Detail):
	// evidence shown beside the reason, never the reason itself.
	ProbeDetail string `json:"probe_detail,omitempty"`
}
