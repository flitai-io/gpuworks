package proto

const TPeerTransferPhase = "peer.transfer.phase"

// PeerTransferPhase reports only an observed source-side transport milestone.
// Connected means pinned TLS succeeded, not that the target accepted the token.
type PeerTransferPhase struct {
	TransferID string `json:"transfer_id"`
	Transport  string `json:"transport"`
	Phase      string `json:"phase"`
}
