package prepobs

import "time"

// PeerTransferSpec identifies a real copy/probe owner. Observations never
// create operations or decide whether the destination resource is ready.
type PeerTransferSpec struct {
	TransferID     string
	Kind           string
	Operation      string
	SourceNodeUUID string
	TargetNodeUUID string
	ResourceKey    string
}

type PeerTransferObservation struct {
	PeerTransferSpec
	Transport                        string
	Phase                            string
	Reason                           string
	FallbackReason                   string
	StartedAt                        time.Time
	UpdatedAt                        time.Time
	LastProgressAt                   time.Time
	SourceToTargetBytes              int64
	TargetToSourceBytes              int64
	RateWaitNS                       int64
	EffectiveRateLimitBytesPerSecond int64
	ServerRateBytesPerSecond         float64
	RateWindowSeconds                int
}

// PeerTransferSnapshot is bounded and scoped to one server process. Byte
// counts describe the inner TLS stream, not archive payload or NIC wire bytes.
type PeerTransferSnapshot struct {
	ObservedAt   time.Time
	StartedAt    time.Time
	Active       []PeerTransferObservation
	Policy       PeerRelayPolicy
	Totals       PeerTransferTotals
	MaxActive    int
	ActiveCount  int
	WaitingCount int
}
