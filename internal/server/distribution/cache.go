package distribution

import (
	"context"
	"net/url"
	"sync"
	"sync/atomic"
	"time"
)

// A server checks its channel when the sign-in page opens and when the
// console asks; the sources are asked at most once per CheckTTL for a channel
// (a failed check is retried after FailedCheckTTL), and a manual check
// always asks (ADR 0040).
const (
	CheckTTL       = time.Hour
	FailedCheckTTL = 5 * time.Minute
)

// CheckCache keeps the last probe of a channel's sources. The zero value is
// ready to use.
type CheckCache struct {
	mu      sync.Mutex
	channel string
	at      time.Time
	fetched *Fetched
	err     error
	// prefetching is set while a Prefetch probe runs, so however many callers
	// ask, at most one runs in the background.
	prefetching atomic.Bool
}

// fresh reports whether the cached probe of channel can still be served. The
// caller holds c.mu.
func (c *CheckCache) fresh(channel string, now time.Time) bool {
	ttl := CheckTTL
	if c.err != nil {
		ttl = FailedCheckTTL
	}
	return c.channel == channel && !c.at.IsZero() && now.Sub(c.at) < ttl
}

// Probe returns the cached probe of channel when it is fresh, and asks the
// sources otherwise (refresh always asks). It answers with the first index
// that verifies, so a slow source never holds a check up; the sources it did
// not wait for are marked ErrNotAwaited. Concurrent callers wait for one
// probe instead of each starting their own. It also returns when the probe
// was made.
func (c *CheckCache) Probe(ctx context.Context, client *Client, channel string, refresh bool, report func() url.Values) (*Fetched, error, time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now().UTC()
	if !refresh && c.fresh(channel, now) {
		return c.fetched, c.err, c.at
	}
	var described url.Values
	if report != nil {
		described = report()
	}
	fetched, err := client.FetchIndexReporting(ctx, channel, described)
	if err != nil && ctx.Err() != nil {
		// The caller went away; that says nothing about the sources.
		return nil, err, now
	}
	c.channel, c.at, c.fetched, c.err = channel, now, fetched, err
	return fetched, err, now
}

// Prefetch starts a probe of channel in the background, bounded by timeout,
// unless the cache is fresh or a probe is already running, and returns at
// once.
func (c *CheckCache) Prefetch(client *Client, channel string, timeout time.Duration, report func() url.Values) {
	if !c.prefetching.CompareAndSwap(false, true) {
		return
	}
	if !c.mu.TryLock() {
		// A probe holds the cache; it will be fresh when it is done.
		c.prefetching.Store(false)
		return
	}
	stale := !c.fresh(channel, time.Now().UTC())
	c.mu.Unlock()
	if !stale {
		c.prefetching.Store(false)
		return
	}
	go func() {
		defer c.prefetching.Store(false)
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		_, _, _ = c.Probe(ctx, client, channel, false, report) // Prefetch has no result consumer; Probe retains non-cancellation outcomes.
	}()
}

// Cached returns the last probe of channel, however old, without asking
// anyone; ok is false when there is none. It serves the installations that
// have switched automatic checking off.
func (c *CheckCache) Cached(channel string) (fetched *Fetched, err error, at time.Time, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cachedLocked(channel)
}

// TryCached returns the same snapshot as Cached when it is immediately
// available. An optional display must not wait for a source probe in progress.
func (c *CheckCache) TryCached(channel string) (fetched *Fetched, err error, at time.Time, ok bool) {
	if !c.mu.TryLock() {
		return nil, nil, time.Time{}, false
	}
	defer c.mu.Unlock()
	return c.cachedLocked(channel)
}

func (c *CheckCache) cachedLocked(channel string) (fetched *Fetched, err error, at time.Time, ok bool) {
	if c.channel != channel || c.at.IsZero() {
		return nil, nil, time.Time{}, false
	}
	return c.fetched, c.err, c.at, true
}
