// Package healthcheck is the server's health check of deployments: once a
// minute, each deployment that has it turned on gets one minimal request
// through the gateway (a chat message "1" with max_tokens 1, an embedding of
// "1", or a one-question decision about "1"), and the reply's time and
// outcome say whether the service answers.
// It only reports: nothing is restarted or taken out of routing because of
// it. The gateway does not account a check as traffic (proxy.Proxy.Probe),
// and the results live in memory: a restarted server checks again within a
// minute.
package healthcheck

import (
	"context"
	"sync"
	"time"
)

// Status is a deployment's health as the check sees it.
type Status string

const (
	// StatusOK: the last check was answered within the threshold.
	StatusOK Status = "ok"
	// StatusTimeout: the last check had no reply within the threshold; it
	// gave up there.
	StatusTimeout Status = "timeout"
	// StatusFailed: the last check was answered with an error, or the
	// gateway could not reach a replica.
	StatusFailed Status = "failed"
	// StatusOff: the deployment does not have the check turned on.
	StatusOff Status = "off"
	// StatusNoReplica: no replica is running awake, so nothing was sent.
	StatusNoReplica Status = "no_replica"
	// StatusPending: turned on, not checked yet.
	StatusPending Status = "pending"
)

// Kind is what a check asks.
type Kind string

const (
	KindChat       Kind = "chat"
	KindEmbeddings Kind = "embeddings"
	// KindCompletions asks a deployment that serves text completion only.
	KindCompletions Kind = "completions"
	// KindDecision asks a deployment that serves System One decisions only.
	KindDecision Kind = "decision"
)

// Payload is the request body a check of kind sends, without its model,
// which the gateway fills in.
func Payload(kind Kind) map[string]any {
	if kind == KindEmbeddings {
		return map[string]any{"input": "1"}
	}
	if kind == KindCompletions {
		return map[string]any{"prompt": "1", "max_tokens": 1, "stream": false}
	}
	if kind == KindDecision {
		// One yes/no question: the smallest request the System One wire
		// format accepts (a state and one to sixteen questions).
		return map[string]any{
			"state":     "1",
			"questions": map[string]any{"q": map[string]any{"type": "noul", "instructions": "Is the state a number?"}},
		}
	}
	return map[string]any{
		"messages":   []map[string]any{{"role": "user", "content": "1"}},
		"max_tokens": 1,
		"stream":     false,
	}
}

// Target is one deployment to check.
type Target struct {
	DeploymentID uint
	Kind         Kind
	// Path is the endpoint the check posts to, e.g. /v1/chat/completions.
	Path      string
	Threshold time.Duration
	// Active is false for a deployment that is stopped: it has no replica
	// to check.
	Active bool
}

// Reply is what the gateway made of one check.
type Reply struct {
	// NoReplica: the gateway had no awake running replica to send it to;
	// nothing was sent.
	NoReplica bool
	// HTTPStatus is the reply's status; 0 when none came.
	HTTPStatus int
	// InstanceUUID is the replica that answered (or was last tried).
	InstanceUUID string
	// Message is the start of an error reply, or the error that kept one
	// from coming.
	Message string
}

// Source lists the deployments that have the check turned on.
type Source interface {
	Targets(ctx context.Context) ([]Target, error)
}

// Sender sends one check through the gateway to a deployment, unaccounted.
type Sender interface {
	Probe(ctx context.Context, deploymentID uint, path string, payload map[string]any) Reply
}

// Probe is one check's result.
type Probe struct {
	AtMS         int64
	Status       Status
	LatencyMS    int64
	HTTPStatus   int
	InstanceUUID string
	Message      string
}

// RecentSize is how many checks a deployment's health keeps.
const RecentSize = 10

// Health is a deployment's health as the checks found it.
type Health struct {
	Status    Status
	Threshold time.Duration
	// Recent are the last checks that ran, oldest first; the last one is the
	// latest.
	Recent []Probe
}

// Last is the latest check that ran, if any.
func (h Health) Last() (Probe, bool) {
	if len(h.Recent) == 0 {
		return Probe{}, false
	}
	return h.Recent[len(h.Recent)-1], true
}

// MeanOK is the mean time of the recent checks answered in time, and how
// many there were.
func (h Health) MeanOK() (ms int64, n int) {
	var sum int64
	for _, p := range h.Recent {
		if p.Status == StatusOK {
			sum += p.LatencyMS
			n++
		}
	}
	if n == 0 {
		return 0, 0
	}
	return sum / int64(n), n
}

// Checker runs the checks and keeps each deployment's recent results.
type Checker struct {
	src      Source
	send     Sender
	interval time.Duration
	// concurrency bounds the checks in flight at once.
	concurrency int
	now         func() time.Time

	mu       sync.Mutex
	health   map[uint]*Health
	inflight map[uint]bool
	wg       sync.WaitGroup
}

// Interval is how often each deployment is checked.
const Interval = time.Minute

// New builds a Checker that checks every Interval.
func New(src Source, send Sender) *Checker {
	return &Checker{src: src, send: send, interval: Interval, concurrency: 32, now: time.Now, health: map[uint]*Health{}, inflight: map[uint]bool{}}
}

// Run checks every interval until ctx ends, then waits for the checks in
// flight to give up.
func (c *Checker) Run(ctx context.Context) {
	defer c.wg.Wait()
	t := time.NewTicker(c.interval)
	defer t.Stop()
	c.Start(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.Start(ctx)
		}
	}
}

// Start begins one round: it lists the targets, records the ones it will
// not send to, and sends the rest in the background, skipping any whose
// previous check is still waiting. Wait waits for the round's checks.
func (c *Checker) Start(ctx context.Context) {
	targets, err := c.src.Targets(ctx)
	if err != nil {
		return
	}
	c.mu.Lock()
	listed := make(map[uint]bool, len(targets))
	for _, t := range targets {
		listed[t.DeploymentID] = true
	}
	// A deployment that turned the check off forgets its results.
	for id := range c.health {
		if !listed[id] {
			delete(c.health, id)
		}
	}
	var send []Target
	for _, t := range targets {
		h := c.health[t.DeploymentID]
		if h == nil {
			h = &Health{Status: StatusPending}
			c.health[t.DeploymentID] = h
		}
		h.Threshold = t.Threshold
		if !t.Active {
			h.Status = StatusNoReplica
			continue
		}
		if c.inflight[t.DeploymentID] {
			continue
		}
		c.inflight[t.DeploymentID] = true
		send = append(send, t)
	}
	c.mu.Unlock()

	sem := make(chan struct{}, max(c.concurrency, 1))
	for _, t := range send {
		c.wg.Add(1)
		go func() {
			defer c.wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				c.finish(t.DeploymentID, nil)
				return
			}
			defer func() { <-sem }()
			c.check(ctx, t)
		}()
	}
}

// Wait waits for the checks started so far.
func (c *Checker) Wait() { c.wg.Wait() }

func (c *Checker) check(ctx context.Context, t Target) {
	// A check never waits past the next round.
	threshold := t.Threshold
	if threshold <= 0 {
		threshold = c.interval
	}
	cctx, cancel := context.WithTimeout(ctx, threshold)
	defer cancel()
	started := c.now()
	reply := c.send.Probe(cctx, t.DeploymentID, t.Path, Payload(t.Kind))
	elapsed := c.now().Sub(started)
	if ctx.Err() != nil {
		// The server is stopping: the check was cut short, not the service.
		c.finish(t.DeploymentID, nil)
		return
	}
	p := Probe{AtMS: started.UnixMilli(), LatencyMS: elapsed.Milliseconds(), HTTPStatus: reply.HTTPStatus, InstanceUUID: reply.InstanceUUID, Message: reply.Message}
	switch {
	case reply.NoReplica:
		c.finish(t.DeploymentID, &Probe{Status: StatusNoReplica})
		return
	case cctx.Err() != nil || elapsed >= threshold:
		p.Status, p.LatencyMS = StatusTimeout, threshold.Milliseconds()
	case reply.HTTPStatus >= 200 && reply.HTTPStatus < 300:
		p.Status = StatusOK
	default:
		p.Status = StatusFailed
	}
	c.finish(t.DeploymentID, &p)
}

// finish records a check's result (nil for none) and lets the deployment be
// checked again. A result for a deployment no longer listed is dropped.
func (c *Checker) finish(id uint, p *Probe) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.inflight, id)
	h := c.health[id]
	if h == nil || p == nil {
		return
	}
	if p.Status == StatusNoReplica {
		h.Status = StatusNoReplica
		return
	}
	h.Status = p.Status
	h.Recent = append(h.Recent, *p)
	if len(h.Recent) > RecentSize {
		h.Recent = append([]Probe(nil), h.Recent[len(h.Recent)-RecentSize:]...)
	}
}

// Health is a deployment's health: StatusOff when it is not checked.
func (c *Checker) Health(id uint) Health {
	c.mu.Lock()
	defer c.mu.Unlock()
	h := c.health[id]
	if h == nil {
		return Health{Status: StatusOff}
	}
	out := *h
	out.Recent = append([]Probe(nil), h.Recent...)
	return out
}
