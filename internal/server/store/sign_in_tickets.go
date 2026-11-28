package store

import (
	"crypto/subtle"
	"errors"
	"time"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/server/auth"
)

// ErrSignInTicketInvalid: the sign-in ticket does not exist, expired, was
// used, or belongs to another identity source or another browser
// (IDENTITY_TICKET_INVALID). The cases are not told apart: a caller learns
// nothing about a ticket it does not hold.
var ErrSignInTicketInvalid = errors.New("the sign-in ticket is not valid")

// IssueSignInTicket makes a one-time sign-in ticket for the user linked
// through identityID, bound to source and handle, valid for ttl (ADR 0085
// decision 3). The store keeps only its hash. Expired tickets are swept in
// the same transaction, so the table holds the sign-ins of the last ttl.
func (s *Store) IssueSignInTicket(userID, identityID uint, source, handle string, ttl time.Duration) (string, error) {
	if ttl <= 0 {
		return "", errors.New("sign-in ticket lifetime must be positive")
	}
	raw := auth.NewToken(32)
	if raw == "" {
		return "", errors.New("could not generate a sign-in ticket")
	}
	now := time.Now().UTC()
	err := s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("expires_at <= ?", now).Delete(&SignInTicket{}).Error; err != nil {
			return err
		}
		return tx.Create(&SignInTicket{TokenHash: HashToken(raw), UserID: userID, IdentityID: identityID,
			Source: source, Handle: handle, CreatedAt: now, ExpiresAt: now.Add(ttl)}).Error
	})
	if err != nil {
		return "", err
	}
	return raw, nil
}

// RedeemSignInTicket uses a ticket once: it is deleted whether or not it
// matches, so a ticket tried in the wrong browser cannot be tried again.
// It answers the ticket's account when the ticket is unexpired, was issued
// for source and bound to handle, and the account is enabled; a disabled
// account is ErrUserDisabled, anything else ErrSignInTicketInvalid. A redeemed
// ticket records the sign-in on its identity link. Any other error is the
// database's, and the transaction rolled back: the ticket is not spent.
func (s *Store) RedeemSignInTicket(ticket, source, handle string) (*User, error) {
	if ticket == "" {
		return nil, ErrSignInTicketInvalid
	}
	var user User
	// outcome is decided inside the transaction, which commits in every
	// case once the ticket was found: its deletion must stand.
	var outcome error
	err := s.db.Transaction(func(tx *gorm.DB) error {
		var t SignInTicket
		if err := tx.Where("token_hash = ?", HashToken(ticket)).First(&t).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				outcome = ErrSignInTicketInvalid
				return nil
			}
			return err
		}
		if err := tx.Where("token_hash = ?", t.TokenHash).Delete(&SignInTicket{}).Error; err != nil {
			return err
		}
		now := time.Now().UTC()
		if !now.Before(t.ExpiresAt) || t.Source != source ||
			subtle.ConstantTimeCompare([]byte(t.Handle), []byte(handle)) != 1 {
			outcome = ErrSignInTicketInvalid
			return nil
		}
		if err := tx.First(&user, t.UserID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				outcome = ErrSignInTicketInvalid
				return nil
			}
			return err
		}
		if user.DisabledAt != nil {
			outcome = ErrUserDisabled
			return nil
		}
		return tx.Model(&UserIdentity{}).Where("id = ? AND user_id = ?", t.IdentityID, t.UserID).Update("last_sign_in_at", now).Error
	})
	if err != nil {
		return nil, err
	}
	if outcome != nil {
		return nil, outcome
	}
	return &user, nil
}
