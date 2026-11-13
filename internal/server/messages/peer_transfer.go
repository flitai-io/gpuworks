package messages

// Peer transfer observation and outcome codes. PEER_RELAY_BUSY lives with its
// sentinel error in error.go.
const (
	ErrorPeerTransferObservationUnavailable = "PEER_TRANSFER_OBSERVATION_UNAVAILABLE"
	ErrorPeerTransferQueryInvalid           = "PEER_TRANSFER_QUERY_INVALID"
	// ErrorPeerTransferCanceled is carried by transfer_error_code on a canceled ledger row.
	ErrorPeerTransferCanceled = "PEER_TRANSFER_CANCELED"
	// ErrorImageRefetchFailed is carried by transfer_error_code on a present
	// image row whose forced re-fetch failed: the node keeps the image it had,
	// and message holds why the attempt failed.
	ErrorImageRefetchFailed = "IMAGE_REFETCH_FAILED"
)
