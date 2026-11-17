package dto

import "time"

// CardHealthChecks is a node's card checks laid out as a table: GET
// /nodes/{uuid}/health-checks. The server decides everything here, the columns
// included; a client only shows it.
type CardHealthChecks struct {
	NodeUUID string `json:"node_uuid"`
	Online   bool   `json:"online"`
	// Reports is false for an agent too old to report card checks.
	Reports bool `json:"reports"`
	// Stale: the last report is too old to say what the checks say now.
	Stale      bool               `json:"stale"`
	ReceivedAt *time.Time         `json:"received_at,omitempty"`
	Checks     []CardHealthColumn `json:"checks"`
	Cards      []CardHealthCard   `json:"cards"`
	// Summary counts the cells in each check status.
	Summary map[string]int `json:"summary"`
}

// CardHealthColumn is one check shown as a column, with how often it runs.
type CardHealthColumn struct {
	ID            string `json:"id"`
	PeriodSeconds int    `json:"period_seconds"`
}

// CardHealthCard is one card's row.
type CardHealthCard struct {
	Ident   string `json:"ident"`
	UUID    string `json:"uuid,omitempty"`
	PCIeBDF string `json:"pcie_bdf,omitempty"`
	Name    string `json:"name,omitempty"`
	Index   int    `json:"index"`
	// Worst is the most serious check status of the cells: fail, warn,
	// skip, pass, na in that order, so a card whose checks did not all run
	// never sums up as passed.
	Worst string `json:"worst"`
	// CanRecheck says the runtime check can be run on the card now.
	CanRecheck bool             `json:"can_recheck"`
	Cells      []CardHealthCell `json:"cells"`
}

// CardHealthCell is one check on one card.
type CardHealthCell struct {
	Check string `json:"check"`
	// Status is pass, warn, fail, skip or na.
	Status string `json:"status"`
	// Code is what a warning or failure found, or why a skipped check did
	// not run (CARD_CHECK_NOT_RUN_YET before its first run).
	Code       string     `json:"code,omitempty"`
	CheckedAt  *time.Time `json:"checked_at,omitempty"`
	DurationMS int32      `json:"duration_ms,omitempty"`
	Stale      bool       `json:"stale"`
}

// CardHealthRecords is a page of failure records: GET
// /nodes/{uuid}/health-records.
type CardHealthRecords struct {
	Records       []CardHealthRecord `json:"records"`
	Total         int64              `json:"total"`
	Page          int                `json:"page"`
	PageSize      int                `json:"page_size"`
	RetentionDays int                `json:"retention_days"`
}

// CardHealthRecord is one stretch in which a check warned or failed.
type CardHealthRecord struct {
	ID        uint   `json:"id"`
	CardIdent string `json:"card_ident"`
	// CardIndex, CardName and PCIeBDF name a card still on the node;
	// Removed says it no longer is.
	CardIndex *int   `json:"card_index,omitempty"`
	CardName  string `json:"card_name,omitempty"`
	PCIeBDF   string `json:"pcie_bdf,omitempty"`
	Removed   bool   `json:"removed"`
	Check     string `json:"check"`
	// Status is warn or fail; Code what the check found.
	Status     string     `json:"status"`
	Code       string     `json:"code,omitempty"`
	StartedAt  time.Time  `json:"started_at"`
	EndedAt    *time.Time `json:"ended_at,omitempty"`
	LastSeenAt time.Time  `json:"last_seen_at"`
	// DurationSeconds is how long it lasted, or has lasted so far.
	DurationSeconds int64            `json:"duration_seconds"`
	Count           int              `json:"count"`
	Flapping        bool             `json:"flapping"`
	Ongoing         bool             `json:"ongoing"`
	FirstEvidence   []GPUHealthField `json:"first_evidence"`
	LatestEvidence  []GPUHealthField `json:"latest_evidence"`
}

// CardCheckDetail is the last run of one check on one card, as the agent
// holds it.
type CardCheckDetail struct {
	Ident string `json:"ident"`
	Check string `json:"check"`
	// Last is absent when the check has not run since the agent started.
	Last *CardCheckRun `json:"last,omitempty"`
	// LastRan is the last run that did run, when Last was skipped.
	LastRan *CardCheckRun `json:"last_ran,omitempty"`
	// LastProblem is the last run that warned or failed, when it is an
	// earlier one.
	LastProblem    *CardCheckRun `json:"last_problem,omitempty"`
	AgentStartedAt *time.Time    `json:"agent_started_at,omitempty"`
}

// CardCheckRun is one run of a check.
type CardCheckRun struct {
	Status     string           `json:"status"`
	Code       string           `json:"code,omitempty"`
	At         time.Time        `json:"at"`
	DurationMS int64            `json:"duration_ms,omitempty"`
	Probe      string           `json:"probe,omitempty"`
	Evidence   []GPUHealthField `json:"evidence"`
}

// CardRecheckRequest names the card to run the runtime check on now.
type CardRecheckRequest struct {
	PCIeBDF string `json:"pcie_bdf,omitempty"`
	UUID    string `json:"uuid,omitempty"`
}
