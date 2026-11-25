package app

// businessRefusalHeader names, on a refused request, why this member did not
// serve it. The value is one of businessRefusalCodes and nothing else: the
// refusal is answered before authentication, so it carries no address, no
// identity and no text from the request. It is a diagnostic for operators
// and collectors, documented in docs/explanation/control-plane-cluster.md;
// the codes may change between releases.
const businessRefusalHeader = "X-Gpuworks-Refusal"

// Why a member refuses business.
const (
	refusalNoGeneration       = "no_business_generation"    // no business generation is installed
	refusalGenerationChanged  = "generation_changed"        // the generation was withdrawn under the request
	refusalClusterUpgrade     = "cluster_upgrade"           // business is closed for a cluster upgrade
	refusalNoHandler          = "no_business_handler"       // the member has no business handler or no store
	refusalSessionsUnroutable = "sessions_not_routable"     // the member cannot accept session calls
	refusalTargetNotCurrent   = "target_not_current"        // the member's target version is not current
	refusalProofNotEligible   = "proof_not_eligible"        // the member handler found the proof not eligible
	refusalProofMissing       = "proof_missing"             // no material proof has succeeded yet
	refusalProofExpired       = "proof_expired"             // the last proof's admission ran out
	refusalProofStopped       = "proof_stopped"             // the proof worker was stopped
	refusalProofRunCanceled   = "proof_run_canceled"        // the proof worker's run was cancelled
	refusalProofCanceled      = "proof_canceled"            // the proof in progress was cancelled
	refusalMemberRevoked      = "member_revoked"            // the database refused this member's admission: it was revoked
	refusalIdentityConflict   = "cluster_identity_conflict" // the database is another cluster's, or its protocol is not this program's
	refusalProofHeadroom      = "proof_headroom"            // readiness only: the proof ends too soon to advertise
	refusalProofEpochChanged  = "proof_epoch_changed"       // readiness only: admission changed under the probe
	refusalProofOwnerChanged  = "proof_owner_changed"       // readiness only: another proof worker took over
	refusalRenewalUnavailable = "renewal_unavailable"       // readiness only: no renewal restored the headroom
	refusalRequestCanceled    = "request_canceled"          // readiness only: the probe was cancelled
	refusalRequestDeadline    = "request_deadline"          // readiness only: the probe ran out of time
	refusalNoKnownLeader      = "no_known_leader"           // no leader was observed in the last few seconds
	refusalNotTheLeader       = "not_the_leader"            // a forwarded request reached a member that does not lead
	refusalLeaderUnreachable  = "leader_unreachable"        // the request could not be forwarded to the leader
	refusalPeerUnidentified   = "forward_peer_unidentified" // a forwarded request without a member certificate
	refusalForwardNotReady    = "forward_not_ready"         // the member route is not set up on this member
	refusalPeerUndecided      = "forward_peer_undecided"    // no answer about the forwarding member's admission
	refusalPeerNotAdmitted    = "forward_peer_not_admitted" // the forwarding member is not admitted (403)
	refusalOther              = "other"                     // a reason this list does not name
)

// businessRefusalCodes is the one list of the values the header can carry.
var businessRefusalCodes = []string{
	refusalNoGeneration, refusalGenerationChanged, refusalClusterUpgrade, refusalNoHandler,
	refusalSessionsUnroutable, refusalTargetNotCurrent,
	refusalProofNotEligible, refusalProofMissing, refusalProofExpired, refusalProofStopped,
	refusalProofRunCanceled, refusalProofCanceled, refusalMemberRevoked, refusalIdentityConflict, refusalProofHeadroom, refusalProofEpochChanged,
	refusalProofOwnerChanged, refusalRenewalUnavailable, refusalRequestCanceled, refusalRequestDeadline,
	refusalNoKnownLeader, refusalNotTheLeader, refusalLeaderUnreachable,
	refusalPeerUnidentified, refusalForwardNotReady, refusalPeerUndecided, refusalPeerNotAdmitted,
	refusalOther,
}
