package hub

// Status writes are fenced per session, never by hub.mu: register and every
// hub.session() lookup (the gateway's tunnel opens among them) would otherwise
// queue behind a fleet's status storm.

// applyCurrent runs write with the session's authenticated node ID only while
// s is still the node's registered session, and reports whether it applied.
// reportMu keeps the check and the write together without holding hub.mu
// across store I/O. register and deregister wait on the same mutex
// (fenceReports) once s left the table, so a write s validated finishes
// before the next session's first report or the node's offline mark.
func (s *Session) applyCurrent(write func(nodeID uint) error) (bool, error) {
	s.reportMu.Lock()
	defer s.reportMu.Unlock()
	if s.nodeID == 0 || s.hub.session(s.NodeUUID) != s {
		return false, nil
	}
	if err := write(s.nodeID); err != nil {
		return false, err
	}
	return true, nil
}

// fenceReports returns once no report write of s is in flight. Called after s
// left the session table, it guarantees none starts afterwards either.
func (s *Session) fenceReports() {
	s.reportMu.Lock()
	defer s.reportMu.Unlock()
}
