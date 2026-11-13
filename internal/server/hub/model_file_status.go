package hub

import (
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// Session replacement and status application share the same ordering, as
// instance reports do. A delayed old connection cannot finish a new copy, nor
// recreate the copy observations its replacement dropped.
func (h *Hub) applyModelFileStatus(session *Session, status proto.ModelFileStatus) error {
	if session == nil {
		return nil
	}
	_, err := session.applyCurrent(func(nodeID uint) error {
		// Recorded before the store publishes the progress event, so a reader
		// woken by that event sees this frame.
		h.copies.Record(nodeID, status, h.now())
		return h.store.UpdateModelFileStatus(nodeID, status)
	})
	return err
}

// ModelCopyObservation is the latest progress frame of the copy the row now
// intends, if its node is connected and reported one recently.
func (h *Hub) ModelCopyObservation(file *store.ModelFile) (proto.ModelFileStatus, bool) {
	return h.copies.Observe(store.ModelFileKey(file.ID), file.State, file.TransferID, h.now())
}
