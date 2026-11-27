package api

// Refusals of a server upgrade that Agent upgrade work still holds the slot
// for. They live here rather than in errors.go to keep that file within the
// repository's length limit; the English fallbacks stay at the call site.
const (
	// ErrorServerUpgradeWaitsForAgent answers a server upgrade asked for
	// while one node's Agent upgrade still holds the slot; params name the
	// node and at most how many minutes that lasts.
	ErrorServerUpgradeWaitsForAgent = "SERVER_UPGRADE_WAITS_FOR_AGENT"
	// ErrorServerUpgradeWaitsForAgentBatch answers a server upgrade asked for
	// while an Agent upgrade batch still has nodes to dispatch; params name
	// the batch and how many nodes are queued.
	ErrorServerUpgradeWaitsForAgentBatch = "SERVER_UPGRADE_WAITS_FOR_AGENT_BATCH"
)
