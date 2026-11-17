package proxy

import (
	"math/rand/v2"
	"sync"
	"time"
)

type candidate struct {
	instanceID   uint
	instanceUUID string
	nodeUUID     string
	port         int
	modelName    string
	uuidHash     uint64
	// kvHost and kvPort are a PD prefill instance's KV endpoint (its
	// bootstrap server on concurrent modes), read from its schedule record.
	kvHost string
	kvPort int
	// routedAt is one more than the routing generation (routingGeneration)
	// the route snapshot holding this candidate was built at, 0 for a
	// candidate read another way: while the generation has not moved, the
	// candidate's row is still what it was read as (instanceRowsUnchanged).
	routedAt uint64
}

type instanceState struct {
	inflight, failStreak int
	cooldownUntil        time.Time
}
type balancer struct {
	mu     sync.Mutex
	states map[string]*instanceState
	now    func() time.Time
	randN  func(int) int
	load   *engineLoad
}
type attemptOutcome int

const (
	outcomeOK attemptOutcome = iota
	outcomeUpstreamUnavailable
	outcomeConnFailure
	outcomeCanceled
)

type lease struct {
	c         candidate
	once      sync.Once
	releaseFn func(attemptOutcome)
}

func newBalancer(load *engineLoad) *balancer {
	return &balancer{states: make(map[string]*instanceState), now: time.Now, randN: rand.IntN, load: load}
}

func (b *balancer) state(uuid string) *instanceState {
	if b.states == nil {
		b.states = make(map[string]*instanceState)
	}
	s := b.states[uuid]
	if s == nil {
		s = &instanceState{}
		b.states[uuid] = s
	}
	return s
}

// inflight is the number of leases an instance currently holds.
func (b *balancer) inflight(uuid string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	if s, ok := b.states[uuid]; ok {
		return s.inflight
	}
	return 0
}

func (b *balancer) gc(active map[string]bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for uuid, state := range b.states {
		if !active[uuid] && state.inflight == 0 {
			delete(b.states, uuid)
		}
	}
}

func (b *balancer) pick(candidates []candidate, excluded map[string]bool, online func(string) bool) *lease {
	b.mu.Lock()
	defer b.mu.Unlock()
	nowFn := b.now
	if nowFn == nil {
		nowFn = time.Now
	}
	randFn := b.randN
	if randFn == nil {
		randFn = rand.IntN
	}
	now := nowFn()
	eligible := make([]candidate, 0, len(candidates))
	for _, c := range candidates {
		if !excluded[c.instanceUUID] && (online == nil || online(c.nodeUUID)) {
			eligible = append(eligible, c)
		}
	}
	if len(eligible) == 0 {
		return nil
	}
	pool := eligible[:0]
	for _, c := range eligible {
		if !now.Before(b.state(c.instanceUUID).cooldownUntil) {
			pool = append(pool, c)
		}
	}
	if len(pool) == 0 {
		pool = eligible
	}
	best := make([]candidate, 0, len(pool))
	bestScore := 0.0
	for _, c := range pool {
		s := b.state(c.instanceUUID)
		score := float64(s.inflight)
		if b.load != nil {
			if g, ok := b.load.gauge(c.instanceUUID); ok {
				score += g.waiting
				if g.kv >= kvSaturatedFraction {
					score += kvSaturatedPenalty
				}
			}
		}
		if len(best) == 0 || score < bestScore {
			best = best[:0]
			best = append(best, c)
			bestScore = score
		} else if score == bestScore {
			best = append(best, c)
		}
	}
	c := best[0]
	if len(best) > 1 {
		c = best[randFn(len(best))]
	}
	s := b.state(c.instanceUUID)
	s.inflight++
	l := &lease{c: c}
	l.releaseFn = func(outcome attemptOutcome) { b.release(l, outcome) }
	return l
}

func (b *balancer) pickFailover(candidates []candidate, excluded map[string]bool, online func(string) bool) *lease {
	b.mu.Lock()
	defer b.mu.Unlock()
	nowFn := b.now
	if nowFn == nil {
		nowFn = time.Now
	}
	now := nowFn()
	var fallback *candidate
	for i := range candidates {
		c := &candidates[i]
		if excluded[c.instanceUUID] || (online != nil && !online(c.nodeUUID)) {
			continue
		}
		if fallback == nil {
			fallback = c
		}
		if now.Before(b.state(c.instanceUUID).cooldownUntil) {
			continue
		}
		s := b.state(c.instanceUUID)
		s.inflight++
		l := &lease{c: *c}
		l.releaseFn = func(o attemptOutcome) { b.release(l, o) }
		return l
	}
	if fallback == nil {
		return nil
	}
	s := b.state(fallback.instanceUUID)
	s.inflight++
	l := &lease{c: *fallback}
	l.releaseFn = func(o attemptOutcome) { b.release(l, o) }
	return l
}

func (b *balancer) release(l *lease, outcome attemptOutcome) {
	if l == nil {
		return
	}
	l.once.Do(func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		s := b.state(l.c.instanceUUID)
		if s.inflight > 0 {
			s.inflight--
		}
		switch outcome {
		case outcomeOK:
			s.failStreak = 0
			s.cooldownUntil = time.Time{}
		case outcomeConnFailure:
			s.failStreak++
			d := cooldownBase
			for i := 1; i < s.failStreak; i++ {
				d *= 2
				if d >= cooldownMax {
					d = cooldownMax
					break
				}
			}
			nowFn := b.now
			if nowFn == nil {
				nowFn = time.Now
			}
			s.cooldownUntil = nowFn().Add(d)
		case outcomeUpstreamUnavailable:
			s.failStreak++
			if s.failStreak >= 3 {
				nowFn := b.now
				if nowFn == nil {
					nowFn = time.Now
				}
				s.cooldownUntil = nowFn().Add(cooldownBase)
			}
		}
	})
}

func (l *lease) release(outcome attemptOutcome) {
	if l != nil && l.releaseFn != nil {
		l.releaseFn(outcome)
	}
}
