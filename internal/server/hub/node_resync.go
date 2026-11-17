package hub

// RequestSync schedules a full desired-state replay to the node without making
// the caller wait for the agent: a setting saved for the whole fleet would
// otherwise replay node after node inside the request, about 10 ms each (11 s
// for 1000 nodes, measured). The replay runs on the session's own coalescing
// worker (sync_worker.go), so the replays of many nodes run beside each other
// under the fleet-wide replay slots, a stalled peer holds up only its own
// node, and any number of requests while a replay is pending or under way
// come to one more replay of the latest state.
//
// The replay is best effort, as the synchronous SyncNode it stands in for was
// at every caller that ignored its error: a node that is not connected, or
// that drops before its replay lands, receives the latest state as the
// initial replay of its next session.
func (h *Hub) RequestSync(uuid string) {
	if s := h.session(uuid); s != nil {
		s.request(&s.resync, s.pushInitialState, "full state sync")
	}
}
