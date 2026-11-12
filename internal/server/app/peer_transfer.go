package app

import (
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/hub"
)

// Keep domain observations out of the REST handler's dependency surface.
type peerTransferStreams struct{ *hub.Hub }

func (s peerTransferStreams) PeerTransferSnapshotDTO() dto.PeerTransferSnapshot {
	return dto.PeerTransferSnapshotFrom(s.Hub.PeerTransferSnapshot(), "")
}
func (s peerTransferStreams) PeerTransferSnapshotForDTO(id string) dto.PeerTransferSnapshot {
	return dto.PeerTransferSnapshotFrom(s.Hub.PeerTransferSnapshotFor(id), id)
}
