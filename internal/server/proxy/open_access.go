package proxy

import (
	"sync/atomic"
	"time"
)

// inferenceOpenAccessTTL bounds how long a read of the open-access switch is
// trusted. The switch is flipped by an administrator in the console, so a
// second of staleness costs nothing; the alternative is a settings read on
// every keyless request. The window is short in the direction that matters: a
// cached "closed" only refuses traffic, while a cached "open" keeps accepting
// keyless requests for up to this long after the switch was turned off.
const inferenceOpenAccessTTL = time.Second

// inferenceOpenAccessReading is one reading of the switch. A failed read is
// remembered as closed so a database that cannot answer cannot open the
// gateway, and it is re-read when the TTL passes rather than being pinned.
type inferenceOpenAccessReading struct {
	open   bool
	readAt time.Time
}

// inferenceOpenAccessCache is the last reading; the zero value reads as closed.
type inferenceOpenAccessCache struct {
	reading atomic.Pointer[inferenceOpenAccessReading]
}

// inferenceOpenAccess reports whether /v1 accepts a request that presents no
// API key. Every path that cannot establish that an administrator opened the
// gateway answers false: a failed read, a nil store, and the zero value before
// the first read all mean "a key is required".
func (p *Proxy) inferenceOpenAccess() bool {
	if p == nil || p.store == nil {
		return false
	}
	if r := p.openAccess.reading.Load(); r != nil && time.Since(r.readAt) < inferenceOpenAccessTTL {
		return r.open
	}
	open, err := p.store.InferenceOpenAccess()
	// An unreadable setting is closed, and closing it is also what gets cached:
	// the next request retries after the TTL, so a transient failure cannot
	// pin the gateway shut for longer than that.
	if err != nil {
		open = false
	}
	p.openAccess.reading.Store(&inferenceOpenAccessReading{open: open, readAt: time.Now()})
	return open
}
