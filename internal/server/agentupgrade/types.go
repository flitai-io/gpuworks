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
	// UnconfirmedHold is how long an attempt that missed its deadline keeps
	// holding the slot; OldVersionSettle how long a node back on its old
	// version is watched before its attempt is settled (see runner.go).
	UnconfirmedHold  time.Duration
	OldVersionSettle time.Duration
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
	// Queued is how many nodes the batch still has to dispatch, set when a
	// server upgrade waits for a queue (Reason "active_run").
	Queued int
	// WaitMinutes is how long at most the attempt still holds the slot,
	// rounded up, set when a server upgrade waits for an attempt.
	WaitMinutes int
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
	// Blocker is what a refused server upgrade waits for, when that is why.
	Blocker *Blocker
}

func (e *Error) Error() string { return e.Detail }

// UpgradeErrorCode lets transport adapters preserve the stable reason without
// depending on this package's concrete error type.
func (e *Error) UpgradeErrorCode() string { return e.Code }

// ServerUpgradeWait is what a refused server upgrade waits for. A batch with
// nodes still to dispatch has queued > 0; otherwise it is one node's attempt,
// which stops holding the slot by itself within minutes at most. ok is false
// when the refusal has another cause.
func (e *Error) ServerUpgradeWait() (node string, run uint, queued, minutes int, ok bool) {
	if e.Blocker == nil {
		return "", 0, 0, 0, false
	}
	node = e.Blocker.Name
	if node == "" {
		node = e.Blocker.NodeUUID
	}
	return node, e.Blocker.RunID, e.Blocker.Queued, e.Blocker.WaitMinutes, true
}
