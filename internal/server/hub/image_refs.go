package hub

import "gpuworks.ai/gpuworks/internal/proto"

// RequestImageRefs hands the node the latest list of enabled image refs to
// send, without waiting for the agent. Each session holds only the latest
// list and sends it on its own coalescing worker (sync_worker.go), after the
// session's first replay, one write at a time: a node that stops reading holds
// up its own write and no other node's, and lists handed to it while a write
// is under way come to one more write of the newest. Waiting for the first
// replay also keeps the list out of the replay's bounded buffer, where a full
// buffer would drop it. A node that is not connected is skipped; its next
// session's full replay carries the refs.
func (h *Hub) RequestImageRefs(uuid string, refs []string) {
	s := h.session(uuid)
	if s == nil {
		return
	}
	s.imageRefsMu.Lock()
	s.imageRefs = refs
	s.imageRefsMu.Unlock()
	s.request(&s.imageRefsSync, s.pushImageRefs, "image refs push")
}

// pushImageRefs sends the latest refs the session was handed. A write error
// closes the session; the next one starts with a full replay that carries the
// refs.
func (s *Session) pushImageRefs() error {
	s.imageRefsMu.Lock()
	refs := s.imageRefs
	s.imageRefsMu.Unlock()
	return s.PushUpdate(proto.StateUpdate{ImageRefs: refs})
}
