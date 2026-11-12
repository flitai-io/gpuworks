package messages

// Peer transfer observation and outcome codes. PEER_RELAY_BUSY lives with its
// sentinel error in error.go.
const (
	ErrorPeerTransferObservationUnavailable = "PEER_TRANSFER_OBSERVATION_UNAVAILABLE"
	ErrorPeerTransferQueryInvalid           = "PEER_TRANSFER_QUERY_INVALID"
	// ErrorPeerTransferCanceled is carried by transfer_error_code on a canceled ledger row.
	ErrorPeerTransferCanceled = "PEER_TRANSFER_CANCELED"
)
