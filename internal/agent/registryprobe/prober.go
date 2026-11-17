package registryprobe

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// Defaults for the probe and its memory.
const (
	// ProbeTimeout bounds one /v2/ request.
	ProbeTimeout = 5 * time.Second
	// ResultTTL is how long a probe result is reused before the host is
	// probed again.
	ResultTTL = 30 * time.Minute
	// CooldownTTL is how long a host whose pull failed or stalled sorts last.
	CooldownTTL = 30 * time.Minute
)

type probeKey struct{ host, route string }

type probeEntry struct {
	result HostResult
	at     time.Time
}

// Prober probes registries and remembers the answers and the failures. It is
// safe for concurrent use and shared by every pull on a node.
type Prober struct {
	now       func() time.Time
	timeout   time.Duration
	ttl       time.Duration
	cooldown  time.Duration
	transport func(Route) http.RoundTripper

	offline bool

	mu     sync.Mutex
	probed map[probeKey]probeEntry
	cooled map[string]time.Time
}

// Option customizes a Prober; production code uses none.
type Option func(*Prober)

// WithClock replaces the clock used for latency, result age and cooldown.
func WithClock(now func() time.Time) Option { return func(p *Prober) { p.now = now } }

// WithTimeout replaces ProbeTimeout.
func WithTimeout(d time.Duration) Option { return func(p *Prober) { p.timeout = d } }

// WithTransport replaces the HTTP transport a probe goes through.
func WithTransport(f func(Route) http.RoundTripper) Option {
	return func(p *Prober) { p.transport = f }
}

// New returns a Prober with the package defaults.
func New(opts ...Option) *Prober {
	p := &Prober{
		now:       time.Now,
		timeout:   ProbeTimeout,
		ttl:       ResultTTL,
		cooldown:  CooldownTTL,
		transport: defaultTransport,
		probed:    map[probeKey]probeEntry{},
		cooled:    map[string]time.Time{},
	}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

// Cool marks a host whose pull failed or stalled: for CooldownTTL its
// candidates sort after every other candidate in their run.
func (p *Prober) Cool(host string) {
	host = NormalizeHost(host)
	if host == "" {
		return
	}
	p.mu.Lock()
	p.cooled[host] = p.now().Add(p.cooldown)
	p.mu.Unlock()
}

// Cooled reports whether host is cooling down.
func (p *Prober) Cooled(host string) bool {
	host = NormalizeHost(host)
	p.mu.Lock()
	defer p.mu.Unlock()
	until, ok := p.cooled[host]
	if !ok {
		return false
	}
	if !p.now().Before(until) {
		delete(p.cooled, host)
		return false
	}
	return true
}

// Probe answers for each host: a fresh result when the cache has none. Hosts
// are probed in parallel; ctx cancels the whole batch, and an unfinished
// probe reads as unreachable.
func (p *Prober) Probe(ctx context.Context, hosts []string, route Route) map[string]HostResult {
	out := make(map[string]HostResult, len(hosts))
	var stale []string
	p.mu.Lock()
	now := p.now()
	for _, host := range hosts {
		if e, ok := p.probed[probeKey{host, route.Key}]; ok && now.Sub(e.at) < p.ttl {
			out[host] = e.result
			continue
		}
		stale = append(stale, host)
	}
	p.mu.Unlock()

	if len(stale) > 0 {
		// Observe the transport's actual proxy resolution. Consulting Proxy a
		// second time could produce a different route or repeat its side effects.
		observedRoute := route
		observedRoute.Proxy = observeProxy(route.Proxy)
		client := &http.Client{
			Transport: p.transport(observedRoute),
			// Any answer proves the host is reachable; where it would
			// redirect to does not matter.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
		results := make([]HostResult, len(stale))
		var wg sync.WaitGroup
		for i, host := range stale {
			wg.Add(1)
			go func() {
				defer wg.Done()
				results[i] = p.probeOne(ctx, client, host)
			}()
		}
		wg.Wait()
		if closer, ok := client.Transport.(interface{ CloseIdleConnections() }); ok {
			closer.CloseIdleConnections()
		}
		p.mu.Lock()
		at := p.now()
		for i, host := range stale {
			out[host] = results[i]
			// A probe cut short by the caller says nothing about the host.
			if ctx.Err() == nil {
				p.probed[probeKey{host, route.Key}] = probeEntry{result: results[i], at: at}
			}
		}
		p.mu.Unlock()
	}
	for host, res := range out {
		res.Cooled = p.Cooled(host)
		out[host] = res
	}
	return out
}

func (p *Prober) probeOne(ctx context.Context, client *http.Client, host string) (res HostResult) {
	res = HostResult{Host: host}
	var path atomic.Int32
	defer func() {
		res.ObservedAt = p.now()
		res.Path = probePath(path.Load())
	}()
	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	ctx = context.WithValue(ctx, probePathKey{}, &path)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+probeHost(host)+"/v2/", nil)
	if err != nil {
		res.Failure = "invalid_target"
		return res
	}
	start := p.now()
	resp, err := client.Do(req)
	if err != nil {
		res.Failure = probeFailure(err)
		// A plain-HTTP registry answers a TLS handshake with an HTTP error:
		// that is still an answer.
		if errors.Is(err, http.ErrSchemeMismatch) {
			res.Reachable = true
			res.LatencyMS = p.now().Sub(start).Milliseconds()
			res.Failure = "http_on_https"
		}
		return res
	}
	_ = resp.Body.Close()
	res.Reachable = true
	res.HTTPStatus = resp.StatusCode
	res.LatencyMS = p.now().Sub(start).Milliseconds()
	return res
}

// errOffline is what an offline prober's every probe fails with.
var errOffline = errors.New("registry probing is off for this agent")

type offlineTransport struct{}

func (offlineTransport) RoundTrip(*http.Request) (*http.Response, error) { return nil, errOffline }

// Offline returns a Prober that reaches no registry: every host reads as
// unreachable at once, so ranked candidates keep the order the server gave.
// An agent running against an injected runtime (a test harness) uses it, so a
// hermetic test never touches the network.
func Offline() *Prober {
	p := New(WithTransport(func(Route) http.RoundTripper { return offlineTransport{} }))
	p.offline = true
	return p
}

// ReachesNetwork reports whether the prober's probes go to the network; only
// an Offline prober's do not.
func (p *Prober) ReachesNetwork() bool { return !p.offline }
