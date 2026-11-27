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
	// slots is the replica's run slots, the concurrent-sequence cap its
	// launch states (deploy.RunSlots); 0 when it states none.
	slots int
	// prioritized: the replica was launched with the engine's priority
	// scheduling, so the gateway writes request priorities to it
	// (runsPriority) and its engine runs a higher class first.
	prioritized bool
}

type instanceState struct {
	inflight      inflightCounts
	failStreak    int
	cooldownUntil time.Time
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
	c candidate
	// priority is the value the lease was counted under (admission), and
	// is given back under.
	priority  int
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
		return s.inflight.total
	}
	return 0
}

func (b *balancer) gc(active map[string]bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for uuid, state := range b.states {
		if !active[uuid] && state.inflight.total == 0 {
			delete(b.states, uuid)
		}
	}
}

// pick leases the least loaded eligible candidate for adm. busy is true, and
// no lease is taken, when adm declares it would rather be refused and no
// eligible candidate has a free run slot for it (hasRoom); the candidates
// with one are then chosen from as from all.
func (b *balancer) pick(candidates []candidate, excluded map[string]bool, online func(string) bool, adm admission) (l *lease, busy bool) {
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
		return nil, false
	}
	if eligible = b.withRoom(eligible, adm); len(eligible) == 0 {
		return nil, true
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
		score := float64(s.inflight.total)
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
	return b.take(c, adm), false
}

// pickFailover leases the first eligible candidate in order that is not
// cooling down, else the first eligible one; for adm only among those with a
// free run slot, busy as for pick.
func (b *balancer) pickFailover(candidates []candidate, excluded map[string]bool, online func(string) bool, adm admission) (l *lease, busy bool) {
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
		if !b.hasRoom(*c, adm) {
			busy = true
			continue
		}
		if fallback == nil {
			fallback = c
		}
		if now.Before(b.state(c.instanceUUID).cooldownUntil) {
			continue
		}
		return b.take(*c, adm), false
	}
	if fallback == nil {
		return nil, busy
	}
	return b.take(*fallback, adm), false
}

func (b *balancer) release(l *lease, outcome attemptOutcome) {
	if l == nil {
		return
	}
	l.once.Do(func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		s := b.state(l.c.instanceUUID)
		s.inflight.remove(l.priority)
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
