package messages

// Why a node placement policy setting has no effect on a node (nodepolicy
// Applicability). The setting may still be written: the facts behind these
// change with the node's agent and partition backend, and an operator must be
// able to set or clear a knob before or after they do.
const (
	// NodePlacementPolicyNoPartitionBackend: the node has no ready dGPU
	// partition backend, so no partition is placed there at all. No params.
	NodePlacementPolicyNoPartitionBackend = "NODE_PLACEMENT_POLICY_NO_PARTITION_BACKEND"
	// NodePlacementPolicyAgentTooOld: the node's agent does not keep
	// processes outside the platform reported on cards the platform shares
	// with them, so nothing shares such a card. No params.
	NodePlacementPolicyAgentTooOld = "NODE_PLACEMENT_POLICY_AGENT_TOO_OLD"
)
