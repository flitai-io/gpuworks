package store

import (
	"errors"
	"sync"
	"time"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/server/auth"
)

// A platform token is a 256-bit random secret, so it is stored as a SHA-256
// and checked with one indexed read - no argon2id, which once cost the /v1
// gateway 40 ms of CPU per request (apikey_cache.go). Still, every request
// that carries a token would read the token and its user, and an MCP tool
// call reads the API again with the same token; a token that verified is
// therefore remembered for a short while.
const (
	// platformTokenCacheTTL bounds how long a verified token and its user
	// are trusted without re-reading them. Revoking through this store takes
	// effect at once; the TTL covers changes made elsewhere (no API changes a
	// user's role today).
	platformTokenCacheTTL = time.Minute
	// platformTokenCacheMaxEntries bounds memory against token churn.
	platformTokenCacheMaxEntries = 4096
	// platformTokenLastUsedInterval is how often last_used_at is written per
	// token: it answers "is this token still used?", which minutes answer.
	platformTokenLastUsedInterval = time.Minute
)

type verifiedPlatformToken struct {
	token PlatformToken
	user  User
	until time.Time
}

type platformTokenCache struct {
	mu       sync.Mutex
	verified map[string]verifiedPlatformToken // by token hash
	lastUsed map[uint]time.Time
	// generation advances on every revoke; a verifier may only remember
	// what it read if none happened since, so a revoke that races a read
	// is never undone by the cache.
	generation uint64
}

func (c *platformTokenCache) lookup(hash string, now time.Time) (verifiedPlatformToken, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.verified[hash]
	if !ok || !now.Before(v.until) {
		return verifiedPlatformToken{}, false
	}
	return v, true
}

func (c *platformTokenCache) snapshot() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.generation
}

func (c *platformTokenCache) remember(generation uint64, hash string, v verifiedPlatformToken) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if generation != c.generation {
		return
	}
	if c.verified == nil || len(c.verified) >= platformTokenCacheMaxEntries {
		c.verified = map[string]verifiedPlatformToken{}
	}
	c.verified[hash] = v
}

func (c *platformTokenCache) forget(hash string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.generation++
	delete(c.verified, hash)
}

// usedAt keeps a remembered token's last use in step with the row.
func (c *platformTokenCache) usedAt(hash string, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if v, ok := c.verified[hash]; ok {
		v.token.LastUsedAt = &now
		c.verified[hash] = v
	}
}

// dueLastUsed reports whether last_used_at should be written now.
func (c *platformTokenCache) dueLastUsed(id uint, now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if now.Sub(c.lastUsed[id]) < platformTokenLastUsedInterval {
		return false
	}
	if c.lastUsed == nil {
		c.lastUsed = map[uint]time.Time{}
	}
	c.lastUsed[id] = now
	return true
}

// CreatePlatformToken stores a new token for a user and returns it in full,
// the only time it is ever shown.
func (s *Store) CreatePlatformToken(userID uint, name, scope string, expiresAt *time.Time) (string, *PlatformToken, error) {
	if scope != PlatformTokenRead && scope != PlatformTokenWrite {
		return "", nil, errors.New("scope must be read or write")
	}
	full, prefix := auth.NewPlatformToken()
	row := &PlatformToken{UserID: userID, Name: name, Prefix: prefix, Hash: auth.HashPlatformToken(full), Scope: scope, ExpiresAt: expiresAt}
	if err := s.db.Create(row).Error; err != nil {
		return "", nil, err
	}
	return full, row, nil
}

// ListPlatformTokens returns a user's tokens, newest first.
func (s *Store) ListPlatformTokens(userID uint) ([]PlatformToken, error) {
	var rows []PlatformToken
	err := s.db.Where("user_id = ?", userID).Order("id desc").Find(&rows).Error
	return rows, err
}

// DeletePlatformToken revokes one of a user's tokens. Another user's token
// reads as not found.
func (s *Store) DeletePlatformToken(userID, id uint) (*PlatformToken, error) {
	var row PlatformToken
	if err := s.db.Where("id = ? AND user_id = ?", id, userID).First(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	err := s.db.Delete(&row).Error
	// Forget the token even when the delete errored: re-reading is always
	// safe, trusting a possibly revoked token is not.
	s.platformTokens.forget(row.Hash)
	if err != nil {
		return nil, err
	}
	return &row, nil
}

// VerifyPlatformToken answers the token's row and its user, or false for an
// unknown, expired or orphaned token. A token that verified within
// platformTokenCacheTTL is answered from memory.
func (s *Store) VerifyPlatformToken(presented string) (*PlatformToken, *User, bool) {
	if !auth.IsPlatformToken(presented) {
		return nil, nil, false
	}
	hash := auth.HashPlatformToken(presented)
	now := time.Now().UTC()
	v, ok := s.platformTokens.lookup(hash, now)
	if !ok {
		generation := s.platformTokens.snapshot()
		if err := s.db.Where("hash = ?", hash).First(&v.token).Error; err != nil {
			return nil, nil, false
		}
		if err := s.db.First(&v.user, v.token.UserID).Error; err != nil {
			return nil, nil, false
		}
		v.until = now.Add(platformTokenCacheTTL)
		s.platformTokens.remember(generation, hash, v)
	}
	if v.token.ExpiresAt != nil && !now.Before(*v.token.ExpiresAt) {
		return nil, nil, false
	}
	if s.platformTokens.dueLastUsed(v.token.ID, now) {
		s.db.Model(&PlatformToken{}).Where("id = ?", v.token.ID).Update("last_used_at", now)
		v.token.LastUsedAt = &now
		s.platformTokens.usedAt(hash, now)
	}
	return &v.token, &v.user, true
}
