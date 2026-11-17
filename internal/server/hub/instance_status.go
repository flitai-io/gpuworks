package hub

import (
	"gpuworks.ai/gpuworks/internal/proto"
)

// applyInstanceStatus persists a report only from the node's current session
// (applyCurrent). The node-bound store method then rejects a valid current
// session reporting another node's instance.
func (h *Hub) applyInstanceStatus(session *Session, status proto.InstanceStatus) (bool, error) {
	if session == nil {
		return false, nil
	}
	return session.applyCurrent(func(nodeID uint) error {
		return h.store.UpdateInstanceStatusFromNode(nodeID, status)
	})
}
