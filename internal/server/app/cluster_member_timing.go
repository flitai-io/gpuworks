package app

// ClusterMemberTiming is the storage tier another member of the cluster
// runs, as that member states it, or why it could not be read.
type ClusterMemberTiming struct {
	ID      uint64
	Address string
	// Tier is the tier the member started with, as storagetier writes it.
	Tier string
	// Recorded is the tier the member's installation records now, which
	// it runs from its next start.
	Recorded string
	// Unread says why the member's timing is not known; empty when it is.
	Unread string
}
