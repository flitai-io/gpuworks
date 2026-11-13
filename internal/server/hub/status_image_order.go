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
func (h *Hub) applyQueuedNodeStatus(nodeUUID string, queued queuedNodeStatus) error {
	s := queued.session
	if s == nil {
		return nil
	}
	s.imageStatusMu.Lock()
	defer s.imageStatusMu.Unlock()
	if s.imageStatusRevision.Load() != queued.imageRevision {
		// This also protects work already popped by the worker, not just entries
		// still pending. Preserve non-image facts from the original inventory.
		queued.status.Images = nil
	}
	_, err := s.applyCurrent(func(uint) error { return h.store.UpdateNodeStatus(nodeUUID, queued.status) })
	return err
}

// handleImageStatus stays synchronous: reports arrive only on change (rare),
// and the store's stale-progress guard depends on their ordering against the
// relay's direct completion write.
func (s *Session) handleImageStatus(raw json.RawMessage) {
	var st proto.ImageNodeStatus
	if err := json.Unmarshal(raw, &st); err != nil {
		return
	}
	// Status receipt only reads the atomic revision; slow persistence never
	// puts its mutex on the status/heartbeat/RPC read path.
	s.imageStatusMu.Lock()
	defer s.imageStatusMu.Unlock()
	if s.hub.session(s.NodeUUID) != s {
		return
	}
	s.imageStatusRevision.Add(1)
	_, _ = s.applyCurrent(func(nodeID uint) error {
		image, err := s.hub.store.ImageByRef(st.ImageRef)
		if err != nil {
			return err
		}
		return s.hub.store.ApplyImageNodeStatus(image.ID, nodeID, st)
	})
}
