package agentupgrade

import (
	"log/slog"
	"time"

	"gpuworks.ai/gpuworks/internal/server/store"
)

type Config struct {
	TargetVersion   string
	ArtifactDir     string
	BinaryAvailable func(name string) bool
	ReadBinary      func(name string) ([]byte, error)
	Transport       Transport
	Logger          *slog.Logger
	Now             func() time.Time
	TransferTimeout time.Duration
	ConfirmTimeout  time.Duration
	PollInterval    time.Duration
}

type Candidate struct {
	NodeUUID    string
	Name        string
	FromVersion string
	OS          string
	Arch        string
	Transport   string
	Eligibility string
	Reason      string
}

type Blocker struct {
	RunID    uint
	NodeUUID string
	Name     string
	Reason   string
}

type Preview struct {
	TargetVersion       string
	ManifestFingerprint string
	NodeUUIDs           []string
	Nodes               []Candidate
	Counts              map[string]int
	BlockedByItem       *Blocker
}

type CreateRequest struct {
	NodeUUIDs                   []string
	ExpectedTargetVersion       string
	ExpectedManifestFingerprint string
	IdempotencyKey              string
	SourceRunID                 uint
	RequestedBy                 string
}

type Counts struct {
	Total       int
	Processed   int
	Succeeded   int
	Failed      int
	Skipped     int
	Cancelled   int
	Interrupted int
	Unconfirmed int
	Queued      int
	Active      int
}

type RunView struct {
	Record        store.AgentUpgradeRun
	State         string
	Counts        Counts
	BlockedByItem *Blocker
}

type RunPage struct {
	Items         []RunView
	Total         int64
	Page          int
	PageSize      int
	BlockedByItem *Blocker
}
type ItemView struct {
	Record         store.AgentUpgradeItem
	CurrentVersion string
}

type ItemPage struct {
	Items    []ItemView
	Total    int64
	Page     int
	PageSize int
}

type Error struct {
	Code   string
	Detail string
}

func (e *Error) Error() string { return e.Detail }

// UpgradeErrorCode lets transport adapters preserve the stable reason without
// depending on this package's concrete error type.
func (e *Error) UpgradeErrorCode() string { return e.Code }
