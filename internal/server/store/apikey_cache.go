package store

import (
	"crypto/sha256"
	"sync"
	"time"

	"gpuworks.ai/gpuworks/internal/server/auth"
)

// API keys are 192-bit random secrets (auth.NewAPIKey). The argon2id hash
// protects the stored value if the database leaks; it is not there to slow down guessing a
// low-entropy input. Paying it on every /v1 request cost ~40ms of CPU and a
// 64 MiB allocation per call, which capped the whole gateway at a few hundred
// requests per second and let a burst of concurrent requests exhaust memory.
// A key that has passed the full check once is therefore remembered by the
// SHA-256 of what was presented, for a bounded time.
const (
	// apiKeyCacheTTL bounds how long a verified key is trusted without
	// re-reading the database. Deletions through this store invalidate
	// immediately; the TTL only covers changes made by another process.
	apiKeyCacheTTL = 5 * time.Minute
	// apiKeyCacheMaxEntries bounds memory. Only keys that verified are
	// cached, so the set is at most the number of live keys in use; the cap
	// guards against a pathological key churn rather than normal traffic.
	apiKeyCacheMaxEntries = 4096
	// apiKeyLastUsedInterval is how often last_used is persisted per key. The
	// column is an operator hint ("is this key still used?"), so minute
	// resolution is plenty and saves one SQLite write per request.
	apiKeyLastUsedInterval = time.Minute
)

// verifyAPIKeyHash is the slow check; tests replace it to count calls.
var verifyAPIKeyHash = auth.VerifyPassword

type verifiedAPIKey struct {
	identity APIKeyIdentity
	prefix   string
	expires  time.Time
}

// verifyCall is one slow check in progress; concurrent requests presenting
// the same secret wait for it instead of each running their own. Without this
// a restart in front of a busy client population would run one 64 MiB
// argon2id per in-flight request at once.
type verifyCall struct {
	done     chan struct{}
	identity APIKeyIdentity
	ok       bool
}

type apiKeyCache struct {
	mu       sync.Mutex
	verified map[[sha256.Size]byte]verifiedAPIKey
	inflight map[[sha256.Size]byte]*verifyCall
	lastUsed map[uint]time.Time
	// scopes caches APIKeyScopeByID by key ID, for requests sent on a key's
	// behalf without its secret (a batch's).
	scopes map[uint]verifiedAPIKey
	// generation advances on every key create/delete. A reader samples it
	// before its database read and may only publish what it read if nothing
	// changed meanwhile; otherwise a read that raced a delete could cache the
	// deleted key.
	generation uint64
	now        func() time.Time
}

func (c *apiKeyCache) snapshot() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.generation
}

func (c *apiKeyCache) clock() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now().UTC()
}

// lookup returns the cached key for a presented secret, if still fresh.
func (c *apiKeyCache) lookup(digest [sha256.Size]byte) (verifiedAPIKey, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.verified[digest]
	if !ok {
		return verifiedAPIKey{}, false
	}
	if !c.clock().Before(entry.expires) {
		delete(c.verified, digest)
		return verifiedAPIKey{}, false
	}
	return entry, true
}

func (c *apiKeyCache) remember(generation uint64, digest [sha256.Size]byte, identity APIKeyIdentity, prefix string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if generation != c.generation {
		return
	}
	if c.verified == nil {
		c.verified = make(map[[sha256.Size]byte]verifiedAPIKey)
	}
	if len(c.verified) >= apiKeyCacheMaxEntries {
		now := c.clock()
		for key, entry := range c.verified {
			if !now.Before(entry.expires) {
				delete(c.verified, key)
			}
		}
		if len(c.verified) >= apiKeyCacheMaxEntries {
			// Still full of live entries: start over rather than grow. The
			// next request per key pays one slow check and repopulates.
			clear(c.verified)
		}
	}
	c.verified[digest] = verifiedAPIKey{identity: identity, prefix: prefix, expires: c.clock().Add(apiKeyCacheTTL)}
}

// join returns the in-progress check for digest, and whether the caller is
// the one that must run it (and then call finish).
func (c *apiKeyCache) join(digest [sha256.Size]byte) (*verifyCall, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if call, ok := c.inflight[digest]; ok {
		return call, false
	}
	if c.inflight == nil {
		c.inflight = make(map[[sha256.Size]byte]*verifyCall)
	}
	call := &verifyCall{done: make(chan struct{})}
	c.inflight[digest] = call
	return call, true
}

func (c *apiKeyCache) finish(digest [sha256.Size]byte, call *verifyCall) {
	c.mu.Lock()
	// Invalidation may have replaced this call while its slow check was
	// running. Only the current call may remove its entry.
	if c.inflight[digest] == call {
		delete(c.inflight, digest)
	}
	c.mu.Unlock()
	close(call.done)
}

// forget drops every cached secret for a key prefix (the key's identity).
func (c *apiKeyCache) forget(prefix string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for digest, entry := range c.verified {
		if entry.prefix == prefix {
			delete(c.verified, digest)
			delete(c.lastUsed, entry.identity.KeyID)
		}
	}
	for id, entry := range c.scopes {
		if entry.prefix == prefix {
			delete(c.scopes, id)
		}
	}
	c.generation++
	// Existing requests can finish, but later requests must not join a
	// verification that read permissions before this change. The generation
	// guard also prevents those old results from repopulating the cache.
	clear(c.inflight)
}

// dueLastUsed reports whether key id's last_used should be written now, and
// if so records the write time so concurrent callers skip it.
func (c *apiKeyCache) dueLastUsed(id uint) (time.Time, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.clock()
	if last, ok := c.lastUsed[id]; ok && now.Sub(last) < apiKeyLastUsedInterval {
		return now, false
	}
	if c.lastUsed == nil {
		c.lastUsed = make(map[uint]time.Time)
	}
	c.lastUsed[id] = now
	return now, true
}

// changed records a key create: any verification that started before it must
// not be published.
func (c *apiKeyCache) changed() {
	c.mu.Lock()
	c.generation++
	clear(c.inflight)
	c.mu.Unlock()
}

func (c *apiKeyCache) lookupScope(id uint) (APIKeyIdentity, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.scopes[id]
	if !ok || !c.clock().Before(entry.expires) {
		return APIKeyIdentity{}, false
	}
	return entry.identity, true
}

func (c *apiKeyCache) rememberScope(generation uint64, identity APIKeyIdentity, prefix string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if generation != c.generation {
		return
	}
	if c.scopes == nil || len(c.scopes) >= apiKeyCacheMaxEntries {
		c.scopes = make(map[uint]verifiedAPIKey)
	}
	c.scopes[identity.KeyID] = verifiedAPIKey{identity: identity, prefix: prefix, expires: c.clock().Add(apiKeyCacheTTL)}
}
