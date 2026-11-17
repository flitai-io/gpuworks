package hub

import (
	"encoding/json"

	"gpuworks.ai/gpuworks/internal/proto"
)

// queuedNodeStatus carries receipt order within one live session. This is
// internal queue metadata, not a new agent protocol or persisted schema.
type queuedNodeStatus struct {
	status        proto.NodeStatus
	session       *Session
	imageRevision uint64
}

// applyQueuedNodeStatus persists a status report from the node's current
// session only (applyCurrent): a status write also marks the node online, so
// one from an ended session would leave a node without a session reading as
// online until the stale sweep. deregister fences the session before it
// writes offline, so that write is always the later one.
//
// The report's image inventory applies only if no image operation report was
// received after the one it was collected behind, and every one received
// before it has been persisted (they persist on the ordered worker, possibly
// after this report). Otherwise the inventory is older than an operation
// result and is dropped; the non-image facts still apply, and the next
// inventory (a minute later) reports fresh presence.
//
// Lock order: imageStatusMu, then reportMu (inside applyCurrent), then hub.mu
// (a read inside session()). fenceReports takes only reportMu.
func (h *Hub) applyQueuedNodeStatus(nodeUUID string, queued queuedNodeStatus) error {
	s := queued.session
	if s == nil {
		return nil
	}
	s.imageStatusMu.Lock()
	defer s.imageStatusMu.Unlock()
	if s.imageStatusRevision.Load() != queued.imageRevision || s.imageStatusApplied != queued.imageRevision {
		queued.status.Images = nil
	}
	_, err := s.applyCurrent(func(uint) error { return h.store.UpdateNodeStatus(nodeUUID, queued.status) })
	return err
}

// handleImageStatus counts the operation report on receipt, so an inventory
// received after it knows it is not newer, and persists it on the ordered
// worker. Receipt order is all the store's stale-progress guard needs: a late
// frame of a transfer the relay already completed directly names a transfer
// the row no longer has, and the store refuses it. A frame without a transfer
// can now persist after a direct write the server made meanwhile (InspectImage);
// the frames after it, persisted in receipt order, still end on the
// operation's own result. A job a closing session drops leaves its inventories
// without images, which no longer matters once the session is gone.
func (s *Session) handleImageStatus(raw json.RawMessage) {
	var st proto.ImageNodeStatus
	if err := json.Unmarshal(raw, &st); err != nil {
		return
	}
	revision := s.imageStatusRevision.Add(1)
	s.submitReport(func() {
		s.imageStatusMu.Lock()
		defer s.imageStatusMu.Unlock()
		_, _ = s.applyCurrent(func(nodeID uint) error {
			image, err := s.hub.store.ImageByRef(st.ImageRef)
			if err != nil {
				return err
			}
			return s.hub.store.ApplyImageNodeStatus(image.ID, nodeID, st)
		})
		s.imageStatusApplied = revision
	})
}
