package dto

import "time"

// PeerTransferSnapshot describes this server process's observed peer attempts.
// It does not decide whether the destination resource is ready.
type PeerTransferSnapshot struct {
	ObservedAt       time.Time                 `json:"observed_at"`
	ServerStartedAt  *time.Time                `json:"server_started_at,omitempty"`
	Scope            string                    `json:"scope"`
	Policy           PeerRelayPolicy           `json:"policy"`
	ActiveRelayCount int                       `json:"active_relay_count"`
	RateWaitingCount int                       `json:"rate_waiting_count"`
	Totals           PeerTransferTotals        `json:"totals"`
	Active           []PeerTransferObservation `json:"active"`
}

type PeerRelayPolicy struct {
	RateLimitBPS               int64   `json:"rate_limit_bps"`
	BurstBytes                 int64   `json:"burst_bytes"`
	MaxActive                  int     `json:"max_active"`
	MaxPerNode                 int     `json:"max_per_node"`
	NoProgressTimeoutSeconds   float64 `json:"no_progress_timeout_seconds"`
	ConfirmationTimeoutSeconds float64 `json:"confirmation_timeout_seconds"`
}

type PeerTransferTotals struct {
	Started             uint64  `json:"started"`
	PeerConfirmed       uint64  `json:"peer_confirmed"`
	Failed              uint64  `json:"failed"`
	Canceled            uint64  `json:"canceled"`
	Busy                uint64  `json:"busy"`
	Unconfirmed         uint64  `json:"unconfirmed"`
	ProbeStarted        uint64  `json:"probe_started"`
	ProbeOK             uint64  `json:"probe_ok"`
	ProbeFailed         uint64  `json:"probe_failed"`
	SourceToTargetBytes int64   `json:"source_to_target_bytes"`
	TargetToSourceBytes int64   `json:"target_to_source_bytes"`
	RateWaitMS          float64 `json:"rate_wait_ms"`
}

type PeerTransferObservation struct {
	TransferID            string     `json:"transfer_id"`
	Kind                  string     `json:"kind"`
	Operation             string     `json:"operation"`
	SourceNodeUUID        string     `json:"source_node_uuid"`
	TargetNodeUUID        string     `json:"target_node_uuid"`
	ResourceKey           string     `json:"resource_key,omitempty"`
	Transport             string     `json:"transport,omitempty"`
	Phase                 string     `json:"phase"`
	Reason                string     `json:"reason,omitempty"`
	FallbackReason        string     `json:"fallback_reason,omitempty"`
	StartedAt             *time.Time `json:"started_at,omitempty"`
	UpdatedAt             *time.Time `json:"updated_at,omitempty"`
	LastProgressAt        *time.Time `json:"last_progress_at,omitempty"`
	SourceToTargetBytes   int64      `json:"source_to_target_bytes"`
	TargetToSourceBytes   int64      `json:"target_to_source_bytes"`
	EffectiveRateLimitBPS int64      `json:"effective_rate_limit_bps"`
	ServerRateBPS         float64    `json:"server_rate_bps"`
	RateWindowSeconds     int        `json:"rate_window_seconds"`
	RateWaitMS            float64    `json:"rate_wait_ms"`
}
