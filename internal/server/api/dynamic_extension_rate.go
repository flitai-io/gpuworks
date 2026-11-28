package api

import (
	"container/list"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
)

const (
	// publicLimiterCapacity bounds the client addresses the public route
	// limiter remembers; the least recently seen is forgotten first.
	publicLimiterCapacity = 10_000
	// Default rate of a public route that declares none, per client
	// address.
	defaultPublicPerMinute = 60
	defaultPublicBurst     = 30
)

// publicLimiter is a token bucket per public route and client address
// (an IPv6 address counts by its /64), in a map bounded by an LRU list.
type publicLimiter struct {
	mu      sync.Mutex
	limit   int
	buckets map[string]*list.Element
	order   *list.List
}

type publicBucket struct {
	key    string
	tokens float64
	at     time.Time
}

func newPublicLimiter(limit int) *publicLimiter {
	return &publicLimiter{limit: limit, buckets: map[string]*list.Element{}, order: list.New()}
}

// take spends one token of route's bucket for ip at now, or says how long
// until one is there.
func (l *publicLimiter) take(route *DynamicRoute, ip string, now time.Time) time.Duration {
	return l.takeKey(route.Plugin+" "+route.Method+" "+route.Pattern, route.RatePerMinute, route.RateBurst, ip, now)
}

// takeKey is take for any public route, named by key; a rate of zero is the
// default rate.
func (l *publicLimiter) takeKey(route string, perMinute, burst int, ip string, now time.Time) time.Duration {
	if perMinute <= 0 || burst <= 0 {
		perMinute, burst = defaultPublicPerMinute, defaultPublicBurst
	}
	rate := float64(perMinute) / 60 // tokens a second
	key := route + " " + clientGroup(ip)
	l.mu.Lock()
	defer l.mu.Unlock()
	var b *publicBucket
	if el, ok := l.buckets[key]; ok {
		l.order.MoveToFront(el)
		b = el.Value.(*publicBucket)
		b.tokens = min(float64(burst), b.tokens+now.Sub(b.at).Seconds()*rate)
		b.at = now
	} else {
		if l.order.Len() >= l.limit {
			oldest := l.order.Back()
			delete(l.buckets, oldest.Value.(*publicBucket).key)
			l.order.Remove(oldest)
		}
		b = &publicBucket{key: key, tokens: float64(burst), at: now}
		l.buckets[key] = l.order.PushFront(b)
	}
	if b.tokens < 1 {
		return time.Duration((1 - b.tokens) / rate * float64(time.Second))
	}
	b.tokens--
	return 0
}

// size is how many buckets the limiter holds.
func (l *publicLimiter) size() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.order.Len()
}

// PublicLimiterSize is how many client buckets the public plugin route
// limiter of h, a Router, holds now; 0 when h mounts no plugin routes.
// Scale cases check it stays bounded however many addresses call.
func PublicLimiterSize(h http.Handler) int {
	routes, ok := h.(chi.Routes)
	if !ok {
		return 0
	}
	n := 0
	_ = chi.Walk(routes, func(_, _ string, handler http.Handler, _ ...func(http.Handler) http.Handler) error {
		if d, ok := handler.(*dynamicEndpoint); ok {
			n = d.limits.size()
		}
		return nil
	})
	return n
}

// clientGroup is the address a client is limited by: an IPv4 address
// itself, an IPv6 address its /64.
func clientGroup(ip string) string {
	addr := net.ParseIP(ip)
	if addr == nil || addr.To4() != nil {
		return ip
	}
	return addr.Mask(net.CIDRMask(64, 128)).String() + "/64"
}
