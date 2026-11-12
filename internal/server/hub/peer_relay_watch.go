package hub

import "time"

func (l *peerRelayLease) watch() {
	defer close(l.done)
	for {
		m := l.manager
		m.mu.Lock()
		now := m.now()
		limit := m.policy.NoProgressTimeout
		reason := "no_progress_timeout"
		if l.confirming {
			limit = m.policy.ConfirmationTimeout
			reason = "confirmation_timeout"
		}
		remaining := limit - (now.Sub(l.clock) - l.paused)
		waiting := l.waiting > 0
		if waiting {
			remaining = limit
		}
		m.mu.Unlock()
		if !waiting && remaining <= 0 {
			l.timeout(reason)
			return
		}
		timer := time.NewTimer(remaining)
		select {
		case <-l.ctx.Done():
			timer.Stop()
			return
		case <-l.wake:
			timer.Stop()
		case <-timer.C:
		}
	}
}
func (l *peerRelayLease) sourceDrained() {
	m := l.manager
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	l.confirming = true
	l.clock = now
	l.paused = 0
	if l.waiting > 0 {
		l.pauseStart = now
	}
	if e := m.entries[l.id]; e != nil {
		e.observation.Phase = "confirming"
		e.observation.UpdatedAt = now
	}
	l.signalLocked()
}
