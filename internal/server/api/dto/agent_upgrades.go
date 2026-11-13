package dto

import "time"

// Agent upgrade DTOs keep run intent and observations separate from storage entities.
type AgentUpgradePreviewRequest struct {
	Scope     string   `json:"scope,omitempty"`
	NodeUUIDs []string `json:"node_uuids,omitempty"`
}

type AgentUpgradeCreateRequest struct {
	NodeUUIDs                   []string `json:"node_uuids"`
	ExpectedTargetVersion       string   `json:"expected_target_version"`
	ExpectedManifestFingerprint string   `json:"expected_manifest_fingerprint"`
	IdempotencyKey              string   `json:"idempotency_key"`
	SourceRunID                 uint     `json:"source_run_id,omitempty"`
}

type AgentUpgradeCandidate struct {
	NodeUUID      string `json:"node_uuid"`
	Name          string `json:"name"`
	VersionChange string `json:"version_change,omitempty"`
	Downgrade     bool   `json:"downgrade,omitempty"`
	FromVersion   string `json:"from_version"`
	OS            string `json:"os"`
	Arch          string `json:"arch"`
	Transport     string `json:"transport"`
	Eligibility   string `json:"eligibility"`
	Reason        string `json:"reason"`
}

type AgentFleetSummary struct {
	Target         string `json:"target"`
	Align          int    `json:"align"`
	Upgrade        int    `json:"upgrade"`
	Downgrade      int    `json:"downgrade"`
	DifferentBuild int    `json:"different_build"`
	Check          int    `json:"check"`
	Offline        int    `json:"offline,omitempty"`
	RunID          uint   `json:"run_id,omitempty"`
}

type AgentUpgradeBlocker struct {
	RunID    uint   `json:"run_id"`
	NodeUUID string `json:"node_uuid"`
	Name     string `json:"name"`
	Reason   string `json:"reason"`
}

type AgentUpgradePreview struct {
	TargetBuild         ServerBuildInfo         `json:"target_build"`
	TargetVersion       string                  `json:"target_version"`
	ManifestFingerprint string                  `json:"manifest_fingerprint"`
	NodeUUIDs           []string                `json:"node_uuids"`
	Nodes               []AgentUpgradeCandidate `json:"nodes"`
	Counts              map[string]int          `json:"counts"`
	BlockedByItem       *AgentUpgradeBlocker    `json:"blocked_by_item"`
}

type AgentUpgradeCounts struct {
	Total       int `json:"total"`
	Processed   int `json:"processed"`
	Succeeded   int `json:"succeeded"`
	Failed      int `json:"failed"`
	Skipped     int `json:"skipped"`
	Cancelled   int `json:"cancelled"`
	Interrupted int `json:"interrupted"`
	Unconfirmed int `json:"unconfirmed"`
	Queued      int `json:"queued"`
	Active      int `json:"active"`
}

type AgentUpgradeRun struct {
	ID                uint                 `json:"id"`
	RequestedBy       string               `json:"requested_by"`
	CreatedAt         time.Time            `json:"created_at"`
	TargetVersion     string               `json:"target_version"`
	SourceRunID       uint                 `json:"source_run_id,omitempty"`
	StopRequestedBy   string               `json:"stop_requested_by,omitempty"`
	StopRequestedAt   *time.Time           `json:"stop_requested_at,omitempty"`
	PauseReason       string               `json:"pause_reason,omitempty"`
	InterruptedReason string               `json:"interrupted_reason,omitempty"`
	State             string               `json:"state"`
	Counts            AgentUpgradeCounts   `json:"counts"`
	BlockedByItem     *AgentUpgradeBlocker `json:"blocked_by_item"`
}

type AgentUpgradeRunPage struct {
	Items         []AgentUpgradeRun    `json:"items"`
	Total         int64                `json:"total"`
	Page          int                  `json:"page"`
	PageSize      int                  `json:"page_size"`
	BlockedByItem *AgentUpgradeBlocker `json:"blocked_by_item"`
}

type AgentUpgradeItem struct {
	RunID            uint       `json:"run_id"`
	NodeUUID         string     `json:"node_uuid"`
	Name             string     `json:"name"`
	FromVersion      string     `json:"from_version"`
	TargetVersion    string     `json:"target_version"`
	CurrentVersion   string     `json:"current_version"`
	ObservedVersion  string     `json:"observed_version,omitempty"`
	OS               string     `json:"os"`
	Arch             string     `json:"arch"`
	Transport        string     `json:"transport"`
	Phase            string     `json:"phase"`
	Reason           string     `json:"reason,omitempty"`
	Error            string     `json:"error,omitempty"`
	StartedAt        *time.Time `json:"started_at,omitempty"`
	DeadlineAt       *time.Time `json:"deadline_at,omitempty"`
	FinishedAt       *time.Time `json:"finished_at,omitempty"`
	RegisteredAt     *time.Time `json:"registered_at,omitempty"`
	VerifiedAt       *time.Time `json:"verified_at,omitempty"`
	VerifiedBy       string     `json:"verified_by,omitempty"`
	VerificationCode string     `json:"verification_code,omitempty"`
	VerificationNote string     `json:"verification_note,omitempty"`
}

type AgentUpgradeItemPage struct {
	Items    []AgentUpgradeItem `json:"items"`
	Total    int64              `json:"total"`
	Page     int                `json:"page"`
	PageSize int                `json:"page_size"`
}
