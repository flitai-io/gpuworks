package hub

import (
	"context"
	"io"
	"math/bits"
	"time"
)

// Tokens and reservations are separate. Bytes granted to a blocked writer
// remain inside the burst allowance until that writer reports its actual n.
type peerTokenBucket struct {
	rate, burst, tokens int64
	remainder           uint64
	last                time.Time
}

func newPeerTokenBucket(rate, burst int64, now time.Time) peerTokenBucket {
	return peerTokenBucket{rate: rate, burst: burst, tokens: burst, last: now}
}
func (b *peerTokenBucket) refill(now time.Time, reserved int64) {
	elapsed := now.Sub(b.last)
	if elapsed <= 0 {
		return
	}
	b.last = now
	capacity := b.burst - reserved
	if capacity <= b.tokens {
		b.tokens = capacity
		b.remainder = 0
		return
	}
	hi, lo := bits.Mul64(uint64(b.rate), uint64(elapsed))
	lo, carry := bits.Add64(lo, b.remainder, 0)
	hi += carry
	if hi >= uint64(time.Second) {
		b.tokens = capacity
		b.remainder = 0
		return
	}
	n, rem := bits.Div64(hi, lo, uint64(time.Second))
	if n >= uint64(capacity-b.tokens) {
		b.tokens = capacity
		b.remainder = 0
	} else {
		b.tokens += int64(n)
		b.remainder = rem
	}
}
func (b *peerTokenBucket) delay(n int64) time.Duration {
	deficit := n - b.tokens
	if deficit <= 0 {
		return 0
	}
	ns := uint64(deficit)*uint64(time.Second) - b.remainder
	rate := uint64(b.rate)
	d := ns / rate
	if ns%rate != 0 {
		d++
	}
	if d == 0 {
		d = 1
	}
	return time.Duration(d)
}

type peerPermitRequest struct {
	n       int64
	ready   chan struct{}
	granted bool
	waiting bool
}
type peerRelayLease struct {
	manager            *peerRelayManager
	id, source, target string
	ctx                context.Context
	cancel             context.CancelCauseFunc
	rate               int64
	bucket             peerTokenBucket
	reserved           int64
	pending            []*peerPermitRequest
	waiting            int
	paused             time.Duration
	pauseStart         time.Time
	waitStart          time.Time
	clock              time.Time
	confirming         bool
	wake               chan struct{}
	done               chan struct{}
}

func (m *peerRelayManager) signalLocked() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}
func (l *peerRelayLease) signalLocked() {
	select {
	case l.wake <- struct{}{}:
	default:
	}
}
func (l *peerRelayLease) startWaitLocked(r *peerPermitRequest, now time.Time) {
	r.waiting = true
	if l.waiting == 0 {
		l.pauseStart = now
		l.waitStart = now
	}
	l.waiting++
	l.signalLocked()
}
func (l *peerRelayLease) endWaitLocked(r *peerPermitRequest, now time.Time) {
	if !r.waiting {
		return
	}
	r.waiting = false
	l.waiting--
	if l.waiting == 0 {
		l.paused += now.Sub(l.pauseStart)
		l.pauseStart = time.Time{}
		waited := now.Sub(l.waitStart).Nanoseconds()
		l.waitStart = time.Time{}
		m := l.manager
		m.totals.RateWaitNS += waited
		if e := m.entries[l.id]; e != nil {
			e.observation.RateWaitNS += waited
			e.observation.UpdatedAt = now
		}
	}
	l.signalLocked()
}
func (m *peerRelayManager) scheduleLocked(now time.Time) time.Duration {
	m.bucket.refill(now, m.reserved)
	delay := time.Duration(0)
	// One grant per transfer per turn; two pumps never receive two shares ahead
	// of another runnable transfer. No network operation holds this mutex.
	for len(m.order) > 0 {
		granted := false
		for checked := 0; checked < len(m.order); checked++ {
			if m.cursor >= len(m.order) {
				m.cursor = 0
			}
			l := m.order[m.cursor]
			m.cursor = (m.cursor + 1) % len(m.order)
			if len(l.pending) == 0 || l.ctx.Err() != nil {
				continue
			}
			r := l.pending[0]
			l.bucket.refill(now, l.reserved)
			if m.bucket.tokens < r.n || l.bucket.tokens < r.n {
				d := max(m.bucket.delay(r.n), l.bucket.delay(r.n))
				if delay == 0 || d < delay {
					delay = d
				}
				continue
			}
			m.bucket.tokens -= r.n
			l.bucket.tokens -= r.n
			m.reserved += r.n
			l.reserved += r.n
			l.pending = l.pending[1:]
			r.granted = true
			l.endWaitLocked(r, now)
			close(r.ready)
			granted = true
			break
		}
		if !granted {
			break
		}
	}
	return delay
}
func (m *peerRelayManager) hasPendingLocked() bool {
	for _, l := range m.order {
		if len(l.pending) > 0 && l.ctx.Err() == nil {
			return true
		}
	}
	return false
}
func (m *peerRelayManager) runScheduler() {
	for {
		m.mu.Lock()
		delay := m.scheduleLocked(m.now())
		if !m.hasPendingLocked() {
			m.running = false
			m.mu.Unlock()
			return
		}
		// Outstanding blocked writes can occupy all tokens; completion wakes this
		// loop. Avoid a busy loop while their reservations cannot be refilled.
		if delay <= 0 {
			delay = time.Millisecond
		}
		m.mu.Unlock()
		timer := time.NewTimer(delay)
		select {
		case <-timer.C:
		case <-m.wake:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
		}
	}
}
func (l *peerRelayLease) permit(n int64) (*peerPermitRequest, error) {
	if err := l.ctx.Err(); err != nil {
		return nil, context.Cause(l.ctx)
	}
	m := l.manager
	r := &peerPermitRequest{n: n, ready: make(chan struct{})}
	m.mu.Lock()
	l.pending = append(l.pending, r)
	m.scheduleLocked(m.now())
	if !r.granted {
		l.startWaitLocked(r, m.now())
	}
	if !m.running && m.hasPendingLocked() {
		m.running = true
		go m.runScheduler()
	}
	m.signalLocked()
	m.mu.Unlock()
	select {
	case <-r.ready:
		if l.ctx.Err() != nil {
			l.complete(r, 0, false)
			return nil, context.Cause(l.ctx)
		}
		return r, nil
	case <-l.ctx.Done():
		m.mu.Lock()
		if r.granted {
			m.mu.Unlock()
			l.complete(r, 0, false)
		} else {
			for i, v := range l.pending {
				if v == r {
					l.pending = append(l.pending[:i], l.pending[i+1:]...)
					break
				}
			}
			l.endWaitLocked(r, m.now())
			m.signalLocked()
			m.mu.Unlock()
		}
		return nil, context.Cause(l.ctx)
	}
}
func (l *peerRelayLease) complete(r *peerPermitRequest, written int64, forward bool) {
	m := l.manager
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	m.bucket.refill(now, m.reserved)
	l.bucket.refill(now, l.reserved)
	m.reserved -= r.n
	l.reserved -= r.n
	m.bucket.tokens += r.n - written
	l.bucket.tokens += r.n - written
	if written > 0 {
		if !l.confirming {
			l.clock = now
			l.paused = 0
			if l.waiting > 0 {
				l.pauseStart = now
			}
			l.signalLocked()
		}
		if forward {
			m.totals.SourceToTargetBytes += written
		} else {
			m.totals.TargetToSourceBytes += written
		}
		if e := m.entries[l.id]; e != nil {
			if !l.confirming {
				e.observation.Phase = "forwarding"
			}
			if forward {
				e.observation.SourceToTargetBytes += written
			} else {
				e.observation.TargetToSourceBytes += written
			}
			e.observation.LastProgressAt = now
			e.observation.UpdatedAt = now
			sec := now.Unix()
			sample := &e.samples[sec%int64(len(e.samples))]
			if sample.second != sec {
				*sample = peerRateSample{second: sec}
			}
			sample.bytes += written
		}
	}
	m.signalLocked()
}
func (l *peerRelayLease) copy(dst io.Writer, src io.Reader, forward bool) (int64, error) {
	buf := make([]byte, int(l.manager.policy.QuantumBytes))
	var total int64
	for {
		n, readErr := src.Read(buf)
		if n > 0 {
			offset := 0
			for offset < n {
				r, err := l.permit(int64(n - offset))
				if err != nil {
					return total, err
				}
				written, err := dst.Write(buf[offset:n])
				if written < 0 || written > n-offset {
					written = 0
					err = io.ErrShortWrite
				}
				l.complete(r, int64(written), forward)
				offset += written
				total += int64(written)
				if err != nil {
					return total, err
				}
				if written == 0 {
					return total, io.ErrShortWrite
				}
			}
		}
		if readErr != nil {
			if readErr == io.EOF {
				return total, nil
			}
			return total, readErr
		}
		if n == 0 {
			select {
			case <-l.ctx.Done():
				return total, context.Cause(l.ctx)
			default:
			}
		}
	}
}
