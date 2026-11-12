package prepobs

import "time"

// PeerRelayPolicy is the effective policy of this server process.
type PeerRelayPolicy struct {
	RateLimitBytesPerSecond int64
	BurstBytes              int64
	QuantumBytes            int64
	MaxActive               int
	MaxPerNode              int
	NoProgressTimeout       time.Duration
	ConfirmationTimeout     time.Duration
}

// DefaultPeerRelayPolicy is the shared safe starting policy for one server
// process. File parsing and a lazily constructed hub use the same defaults.
func DefaultPeerRelayPolicy() PeerRelayPolicy {
	return PeerRelayPolicy{
		RateLimitBytesPerSecond: 64 << 20,
		BurstBytes:              64 << 10,
		QuantumBytes:            16 << 10,
		MaxActive:               2,
		MaxPerNode:              1,
		NoProgressTimeout:       2 * time.Minute,
		ConfirmationTimeout:     5 * time.Minute,
	}
}

type PeerTransferTotals struct {
	Started             uint64
	PeerConfirmed       uint64
	Failed              uint64
	Canceled            uint64
	Busy                uint64
	Unconfirmed         uint64
	ProbeStarted        uint64
	ProbeOK             uint64
	ProbeFailed         uint64
	SourceToTargetBytes int64
	TargetToSourceBytes int64
	RateWaitNS          int64
}
