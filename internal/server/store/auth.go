package store

import (
	"crypto/sha256"
	"errors"
	"strconv"
	"time"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/server/auth"
)

// EnsureAdmin creates the admin user if none exists, returning (username,
// generatedPassword, created). When created is false the password is "".
func (s *Store) EnsureAdmin(username, password string) (string, string, bool, error) {
	var count int64
	if err := s.db.Model(&User{}).Count(&count).Error; err != nil {
		return "", "", false, err
	}
	if count > 0 {
		return username, "", false, nil
	}
	if password == "" {
		var err error
		password, err = auth.NewPassword()
		if err != nil {
			return "", "", false, err
		}
	}
	if err := auth.ValidateAccountPassword(username, password); err != nil {
		return "", "", false, err
	}
	hash, err := auth.HashPassword(password)
	if err != nil {
		return "", "", false, err
	}
	u := &User{Username: username, PasswordHash: hash, Role: "admin"}
	if err := s.db.Create(u).Error; err != nil {
		return "", "", false, err
	}
	return username, password, true, nil
}

// VerifyUser checks a username/password pair.
func (s *Store) VerifyUser(username, password string) (*User, bool) {
	u, err := s.AuthenticateUser(username, password)
	return u, err == nil
}

// AuthenticateUser distinguishes rejected credentials from storage failures.
func (s *Store) AuthenticateUser(username, password string) (*User, error) {
	var u User
	if err := s.db.Where("username = ?", username).First(&u).Error; err != nil {
		return nil, sessionLookupError(err)
	}
	if !auth.VerifyPassword(password, u.PasswordHash) {
		return nil, ErrNotFound
	}
	return &u, nil
}

// SetUserPassword replaces a user's password hash.
func (s *Store) SetUserPassword(username, password string) error {
	if err := auth.ValidateAccountPassword(username, password); err != nil {
		return err
	}
	hash, err := auth.HashPassword(password)
	if err != nil {
		return err
	}
	var user User
	err = s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("username = ?", username).First(&user).Error; err != nil {
			return sessionLookupError(err)
		}
		if err := tx.Model(&user).Update("password_hash", hash).Error; err != nil {
			return err
		}
		return tx.Where("user_id = ?", user.ID).Delete(&ManagementSession{}).Error
	})
	if err != nil {
		return err
	}
	s.publishManagementSession(strconv.FormatUint(uint64(user.ID), 10), "password-changed")
	return nil
}

// CreateAPIKey generates and stores a new API key that reaches every
// deployment, returning the full key once.
func (s *Store) CreateAPIKey(name string, userID uint) (fullKey string, err error) {
	return s.CreateScopedAPIKey(name, userID, APIKeyScope{})
}

// ListAPIKeys returns key metadata (never the secret), revoked keys included.
func (s *Store) ListAPIKeys() ([]APIKey, error) {
	var ks []APIKey
	err := s.db.Order("id desc").Find(&ks).Error
	return ks, err
}

// RevokeAPIKey stops a key authenticating, keeping its row (and so its name
// in the usage it has). An unknown prefix returns ErrNotFound; an already
// revoked key remains a no-op.
func (s *Store) RevokeAPIKey(prefix string) error {
	changed := false
	err := s.db.Transaction(func(tx *gorm.DB) error {
		result := tx.Model(&APIKey{}).Where("prefix = ? AND revoked_at IS NULL", prefix).
			Update("revoked_at", time.Now().UTC())
		if result.Error != nil {
			return result.Error
		}
		changed = result.RowsAffected > 0
		if changed {
			return nil
		}
		// The UPDATE acquires SQLite's writer before this existence check, so
		// a concurrent create cannot turn an unmatched target into a live key
		// that we acknowledge without revoking.
		var existing APIKey
		err := tx.Select("id").Where("prefix = ?", prefix).First(&existing).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrNotFound
		}
		return err
	})
	// Forget the key even when the write errored: re-verifying against the
	// database is always safe, trusting a possibly revoked key is not.
	s.apiKeys.forget(prefix)
	if err == nil && changed {
		s.Bus.PublishChange("apikey", prefix, "revoked")
	}
	return err
}

// APIKeyIdentity is who presented a valid API key: the key, its owner and
// the deployments it may call (see Allows). Deployments is shared with the
// cache and must not be modified.
type APIKeyIdentity struct {
	KeyID       uint
	UserID      uint
	Restricted  bool
	Deployments map[uint]struct{}
}

// VerifyAPIKey validates a presented key (prefix lookup + argon2id verify) and
// touches LastUsed.
func (s *Store) VerifyAPIKey(presented string) bool {
	_, ok := s.ResolveAPIKey(presented)
	return ok
}

// ResolveAPIKey is VerifyAPIKey that also says which key and owner the
// secret belongs to; the gateway attributes each request to them.
func (s *Store) ResolveAPIKey(presented string) (APIKeyIdentity, bool) {
	digest := sha256.Sum256([]byte(presented))
	if cached, ok := s.apiKeys.lookup(digest); ok {
		s.touchAPIKey(cached.identity.KeyID)
		return cached.identity, true
	}
	call, leader := s.apiKeys.join(digest)
	if !leader {
		<-call.done
		if call.ok {
			s.touchAPIKey(call.identity.KeyID)
		}
		return call.identity, call.ok
	}
	defer s.apiKeys.finish(digest, call)
	generation := s.apiKeys.snapshot()
	var k APIKey
	if err := s.db.Where("prefix = ? AND revoked_at IS NULL", auth.PrefixOf(presented)).First(&k).Error; err != nil {
		// Not found and read errors both reject; neither is cached.
		return APIKeyIdentity{}, false
	}
	if !verifyAPIKeyHash(presented, k.Hash) {
		return APIKeyIdentity{}, false
	}
	identity := APIKeyIdentity{KeyID: k.ID, UserID: k.UserID, Restricted: k.Restricted}
	if k.Restricted {
		set, err := apiKeyDeploymentSet(s.db, k.ID)
		if err != nil {
			// Rejected and not cached, like a failed key read.
			return APIKeyIdentity{}, false
		}
		identity.Deployments = set
	}
	call.identity, call.ok = identity, true
	s.apiKeys.remember(generation, digest, identity, k.Prefix)
	s.touchAPIKey(k.ID)
	return identity, true
}

func (s *Store) touchAPIKey(id uint) {
	if now, due := s.apiKeys.dueLastUsed(id); due {
		s.db.Model(&APIKey{}).Where("id = ?", id).Update("last_used", now)
	}
}
