package store

import (
	"errors"
	"fmt"
	"regexp"
	"time"

	"gorm.io/gorm"
)

// Errors of the account and external-identity operations (ADR 0085).
var (
	// ErrUserDisabled: the account is disabled; sign-in, platform tokens and
	// inference API keys of this user are refused (USER_DISABLED).
	ErrUserDisabled = errors.New("user is disabled")
	// ErrLastLocalAdmin: disabling this account would leave no enabled local
	// administrator, the floor of ADR 0071 decision 11.
	ErrLastLocalAdmin = errors.New("the last enabled local administrator cannot be disabled")
	// ErrIdentityTaken: the (source, subject) pair is already linked, to this
	// user or another one.
	ErrIdentityTaken = errors.New("the external identity is already linked")
)

// identitySourcePattern is the source-name rule: one word the extension owns,
// the same shape as module and plugin names.
var identitySourcePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{1,31}$`)

// maxIdentitySubject bounds a subject: an id from an identity source, never a
// free-form description.
const maxIdentitySubject = 256

// UserByID is one account, disabled or not.
func (s *Store) UserByID(id uint) (*User, error) {
	var u User
	if err := s.reader().First(&u, id).Error; err != nil {
		return nil, sessionLookupError(err)
	}
	return &u, nil
}

// ListUsers is every account in id order, disabled ones included: an
// administrator manages the accounts the platform holds.
func (s *Store) ListUsers() ([]User, error) {
	var out []User
	if err := s.reader().Order("id").Find(&out).Error; err != nil {
		return nil, err
	}
	return out, nil
}

// FindUserByIdentity answers the enabled account linked to (source, subject):
// ErrNotFound when no account is linked, and ErrUserDisabled, with no account,
// when the linked one is disabled, so a sign-in through an identity source
// cannot skip the disabled check (ADR 0085 decisions 5 and 7). The pair is the
// only key, never a username or an email. An administrator who needs the
// account whatever its state reads it with ExternalIdentities and UserByID.
func (s *Store) FindUserByIdentity(source, subject string) (*User, error) {
	if !identitySourcePattern.MatchString(source) || subject == "" || len(subject) > maxIdentitySubject {
		return nil, ErrNotFound
	}
	var identity UserIdentity
	if err := s.reader().Where("source = ? AND subject = ?", source, subject).First(&identity).Error; err != nil {
		return nil, sessionLookupError(err)
	}
	u, err := s.UserByID(identity.UserID)
	if err != nil {
		return nil, err
	}
	if u.DisabledAt != nil {
		return nil, ErrUserDisabled
	}
	return u, nil
}

// ExternalIdentities is the accounts' links to outside identity sources, in
// creation order.
func (s *Store) ExternalIdentities(userID uint) ([]UserIdentity, error) {
	var out []UserIdentity
	if err := s.reader().Where("user_id = ?", userID).Order("id").Find(&out).Error; err != nil {
		return nil, err
	}
	return out, nil
}

// LinkIdentity links an existing account to (source, subject). Linking never
// creates an account and never changes its role: an identity connector cannot
// make anyone an administrator (ADR 0085 decision 5).
func (s *Store) LinkIdentity(userID uint, source, subject string) (*UserIdentity, error) {
	if !identitySourcePattern.MatchString(source) {
		return nil, fmt.Errorf("identity source %q: %w", source, ErrInvalidIdentity)
	}
	if subject == "" || len(subject) > maxIdentitySubject {
		return nil, fmt.Errorf("identity subject length %d: %w", len(subject), ErrInvalidIdentity)
	}
	var out UserIdentity
	err := s.db.Transaction(func(tx *gorm.DB) error {
		var u User
		if err := tx.First(&u, userID).Error; err != nil {
			return sessionLookupError(err)
		}
		var existing UserIdentity
		err := tx.Where("source = ? AND subject = ?", source, subject).First(&existing).Error
		if err == nil {
			return ErrIdentityTaken
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		out = UserIdentity{UserID: userID, Source: source, Subject: subject, CreatedAt: time.Now().UTC()}
		return identityWriteError(tx.Create(&out).Error)
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// identityWriteError maps the unique (source, subject) index to
// ErrIdentityTaken: two links of the same pair racing past the read above
// meet it on insert. It reads SQLite's typed extended result code, like
// syncGroupWriteError, and never the driver's English text.
func identityWriteError(err error) error {
	var coded interface{ Code() int }
	if errors.As(err, &coded) && coded.Code() == 2067 { // SQLITE_CONSTRAINT_UNIQUE
		return ErrIdentityTaken
	}
	return err
}

// ErrInvalidIdentity: the source name or the subject does not have the shape
// of an external identity.
var ErrInvalidIdentity = errors.New("invalid external identity")

// UnlinkIdentity removes one of the account's links and answers the link it
// removed. A link of another account is ErrNotFound, so an administrator
// cannot take one away by guessing its id.
func (s *Store) UnlinkIdentity(userID, identityID uint) (*UserIdentity, error) {
	var removed UserIdentity
	err := s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("id = ? AND user_id = ?", identityID, userID).First(&removed).Error; err != nil {
			return sessionLookupError(err)
		}
		return tx.Delete(&UserIdentity{}, removed.ID).Error
	})
	if err != nil {
		return nil, err
	}
	return &removed, nil
}

// DisableUser disables an account (ADR 0085 decision 7): its sessions are
// deleted at once, sign-in, platform tokens and inference API keys are refused
// with USER_DISABLED, and its data and keys are kept. Running workloads are
// not interrupted. Disabling an already disabled account changes nothing.
func (s *Store) DisableUser(id uint) (*User, error) {
	var out User
	var sessions []ManagementSession
	err := s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.First(&out, id).Error; err != nil {
			return sessionLookupError(err)
		}
		var err error
		sessions, err = disableUserTx(tx, &out)
		return err
	})
	if err != nil {
		return nil, err
	}
	s.userDisabled(id, sessions)
	return &out, nil
}

// disableUserTx disables u, read in tx, and answers the sessions it deleted.
// An account already disabled is left as it is.
func disableUserTx(tx *gorm.DB, u *User) ([]ManagementSession, error) {
	if u.DisabledAt != nil {
		return nil, nil
	}
	if u.IsLocalAdmin() {
		var remaining int64
		if err := tx.Model(&User{}).
			Where("role = ? AND local_admin = ? AND disabled_at IS NULL AND id <> ?", "admin", true, u.ID).
			Count(&remaining).Error; err != nil {
			return nil, err
		}
		if remaining == 0 {
			return nil, ErrLastLocalAdmin
		}
	}
	now := time.Now().UTC()
	if err := tx.Model(&User{}).Where("id = ?", u.ID).Update("disabled_at", now).Error; err != nil {
		return nil, err
	}
	u.DisabledAt = &now
	var sessions []ManagementSession
	if err := tx.Where("user_id = ?", u.ID).Find(&sessions).Error; err != nil {
		return nil, err
	}
	return sessions, tx.Where("user_id = ?", u.ID).Delete(&ManagementSession{}).Error
}

// userDisabled runs after the commit that disabled an account.
func (s *Store) userDisabled(id uint, sessions []ManagementSession) {
	// An open stream re-checks its credential on this topic and ends before
	// it forwards another change.
	for _, session := range sessions {
		s.publishManagementSession(session.TokenHash, "user-disabled")
	}
	// New requests in this process see the disablement at once; other
	// processes follow within their caches' TTLs (ADR 0085 decision 7).
	s.apiKeys.forgetUser(id)
	s.platformTokens.forgetUser(id)
}

// EnableUser restores a disabled account; its data and keys work again.
// Enabling an already enabled account changes nothing.
func (s *Store) EnableUser(id uint) (*User, error) {
	var out User
	err := s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.First(&out, id).Error; err != nil {
			return sessionLookupError(err)
		}
		return enableUserTx(tx, &out)
	})
	if err != nil {
		return nil, err
	}
	s.userEnabled(id)
	return &out, nil
}

// enableUserTx enables u, read in tx; an enabled account is left as it is.
func enableUserTx(tx *gorm.DB, u *User) error {
	if u.DisabledAt == nil {
		return nil
	}
	if err := tx.Model(&User{}).Where("id = ?", u.ID).Update("disabled_at", nil).Error; err != nil {
		return err
	}
	u.DisabledAt = nil
	return nil
}

// userEnabled runs after the commit that enabled an account.
func (s *Store) userEnabled(id uint) {
	s.apiKeys.forgetUser(id)
	s.platformTokens.forgetUser(id)
}
