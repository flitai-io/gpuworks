package api

import "gpuworks.ai/gpuworks/internal/server/messages"

// Peer transfer observation errors; the codes are owned by messages.
const (
	ErrorPeerTransferObservationUnavailable = messages.ErrorPeerTransferObservationUnavailable
	ErrorPeerTransferQueryInvalid           = messages.ErrorPeerTransferQueryInvalid
	ErrorPeerTransferCanceled               = messages.ErrorPeerTransferCanceled
)
