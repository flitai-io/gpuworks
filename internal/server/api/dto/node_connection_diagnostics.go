package dto

import (
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
)

// NodeConnectionCheck describes a bounded observation, not end-to-end health.
type NodeConnectionCheck struct {
	Stage      string     `json:"stage"`
	Outcome    string     `json:"outcome"`
	Vantage    string     `json:"vantage"`
	Target     string     `json:"target,omitempty"`
	Source     string     `json:"source,omitempty"`
	ObservedAt *time.Time `json:"observed_at,omitempty"`
	HTTPStatus int        `json:"http_status,omitempty"`
}

// NodeConnectionExposure is the connector's last recorded observation.
// It deliberately carries its original time; it is not a fresh public probe.
type NodeConnectionExposure struct {
	URL        string    `json:"url"`
	State      string    `json:"state"`
	ObservedAt time.Time `json:"observed_at"`
}

type NodeConnectionDiagnostics struct {
	NodeUUID          string                  `json:"node_uuid"`
	CheckedAt         time.Time               `json:"checked_at"`
	ConnectMode       string                  `json:"connect_mode"`
	SavedServerURL    string                  `json:"saved_server_url"`
	LastSeenAt        *time.Time              `json:"last_seen_at"`
	RecoveryPhase     string                  `json:"recovery_phase"`
	RecoveryAttempts  int                     `json:"recovery_attempts"`
	LastProbeAt       *time.Time              `json:"last_probe_at"`
	LastRecoveryError string                  `json:"last_recovery_error"`
	LastRecoveryCode  string                  `json:"last_recovery_code,omitempty"`
	LastRejectReason  string                  `json:"last_reject_reason,omitempty"`
	LastRejectAt      *time.Time              `json:"last_reject_at,omitempty"`
	Exposure          *NodeConnectionExposure `json:"exposure"`
	AgentReport       *proto.DoctorReport     `json:"agent_report,omitempty"`
	Checks            []NodeConnectionCheck   `json:"checks"`
	Guidance          []string                `json:"guidance"`
}
