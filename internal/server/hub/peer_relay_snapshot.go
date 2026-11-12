package hub

import (
	"sort"

	"gpuworks.ai/gpuworks/internal/server/prepobs"
)

func (h *Hub) PeerTransferSnapshot() prepobs.PeerTransferSnapshot {
	return h.peerTransferSnapshot("", false)
}
func (h *Hub) PeerTransferSnapshotFor(id string) prepobs.PeerTransferSnapshot {
	return h.peerTransferSnapshot(id, true)
}
func (h *Hub) peerTransferSnapshot(id string, filtered bool) prepobs.PeerTransferSnapshot {
	m := h.peerManager()
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	s := prepobs.PeerTransferSnapshot{ObservedAt: now, StartedAt: m.started, Policy: m.policy, Totals: m.totals, MaxActive: m.policy.MaxActive, ActiveCount: len(m.leases), Active: []prepobs.PeerTransferObservation{}}
	for _, l := range m.leases {
		if l.waiting > 0 {
			s.WaitingCount++
			s.Totals.RateWaitNS += now.Sub(l.waitStart).Nanoseconds()
		}
	}
	appendEntry := func(e *peerTransfer) {
		if e == nil {
			return
		}
		o := e.observation
		if e.lease != nil && e.lease.waiting > 0 {
			o.RateWaitNS += now.Sub(e.lease.waitStart).Nanoseconds()
		}
		var bytes int64
		for _, sample := range e.samples {
			if now.Unix()-sample.second < 5 && now.Unix() >= sample.second {
				bytes += sample.bytes
			}
		}
		o.ServerRateBytesPerSecond = float64(bytes) / 5
		s.Active = append(s.Active, o)
	}
	if filtered {
		appendEntry(m.entries[id])
	} else {
		for _, e := range m.entries {
			appendEntry(e)
		}
		sort.Slice(s.Active, func(i, j int) bool { return s.Active[i].TransferID < s.Active[j].TransferID })
	}
	return s
}
