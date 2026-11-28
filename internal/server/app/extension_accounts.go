package app

import (
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"gpuworks.ai/gpuworks/internal/server/accounts"
	"gpuworks.ai/gpuworks/internal/server/api"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// SignInEntry is a sign-in option a module offers on the sign-in page:
// a button that sends the browser through the core's
// /api/v1/auth/sign-in/start to Start, one of the module's AccessPublic GET
// routes. Label is shown as is. Icon is reserved: it is passed through to the
// sign-in options, and the console does not show it.
type SignInEntry struct {
	Source string
	Label  string
	Icon   string
	Start  string
}

// Accounts is the module's handle on the accounts of the identity sources it
// declared (package accounts). It may be called at any time; its operations
// refuse sources the module did not declare while Init ran.
func (h *Host) Accounts() *accounts.Accounts { return h.reg.accounts.For(h.name) }

// DeclareIdentitySources claims identity source names (user_identities.source)
// for this module. A name belongs to one module; claiming one another module
// claimed is IDENTITY_SOURCE_CONFLICT.
func (h *Host) DeclareIdentitySources(sources ...string) error {
	if h.reg.closed {
		return ErrHostClosed
	}
	return h.reg.accounts.Declare(h.name, sources...)
}

// OfferSignIn puts a sign-in entry on the sign-in page. Its source must be one
// the module declared, Start one of its own AccessPublic GET routes, and the
// entry follows the feature of that route: it is left out while the feature is
// locked or hidden. A source has at most one entry.
func (h *Host) OfferSignIn(e SignInEntry) error {
	if h.reg.closed {
		return ErrHostClosed
	}
	if !h.reg.accounts.Owns(h.name, e.Source) {
		return messages.NewProductError(messages.ErrorIdentitySourceConflict,
			fmt.Sprintf("extension %s did not declare identity source %s", h.name, e.Source), map[string]any{"source": e.Source})
	}
	if strings.TrimSpace(e.Label) == "" || utf8.RuneCountInString(e.Label) > 64 {
		return fmt.Errorf("extension %s: sign-in entry %s needs a label of 1-64 characters", h.name, e.Source)
	}
	if !strings.HasPrefix(e.Start, "/x/"+h.name+"/") && e.Start != "/x/"+h.name {
		return fmt.Errorf("extension %s: sign-in entry %s starts at %q, outside /x/%s", h.name, e.Source, e.Start, h.name)
	}
	for _, prev := range h.reg.signIns {
		if prev.Source == e.Source {
			return fmt.Errorf("extension %s: identity source %s offers a sign-in twice", h.name, e.Source)
		}
	}
	h.reg.signIns = append(h.reg.signIns, api.SignInOption{Source: e.Source, Label: e.Label, Icon: e.Icon, Start: e.Start, Module: h.name})
	return nil
}

// accountStore adapts the server store to accounts.Store: the store's rows
// become accounts.User and accounts.Account, a missing link on a read is
// accounts.ErrNotLinked, and the store's refusals become the product codes a
// connector reads.
type accountStore struct{ st *store.Store }

func accountUser(u *store.User) accounts.User {
	return accounts.User{ID: u.ID, Username: u.Username, Admin: u.IsAdmin(), Disabled: u.DisabledAt != nil}
}

func (s accountStore) LinkedAccount(source, subject string) (accounts.User, uint, error) {
	u, link, err := s.st.LinkedAccount(source, subject)
	if errors.Is(err, store.ErrNotFound) {
		return accounts.User{}, 0, accounts.ErrNotLinked
	}
	if err != nil {
		return accounts.User{}, 0, err
	}
	return accountUser(u), link.ID, nil
}

func (s accountStore) CreateExternalUser(username, source, subject string) (accounts.User, uint, error) {
	u, link, err := s.st.CreateExternalUser(username, source, subject)
	if err != nil {
		return accounts.User{}, 0, accountError(err, username)
	}
	return accountUser(u), link.ID, nil
}

func (s accountStore) RenameLinkedAccount(source, subject, username string) (accounts.User, error) {
	u, err := s.st.RenameLinkedAccount(source, subject, username)
	if err != nil {
		return accounts.User{}, accountError(err, username)
	}
	return accountUser(u), nil
}

func (s accountStore) DisableLinkedAccount(source, subject string) error {
	_, err := s.st.DisableLinkedAccount(source, subject)
	return accountError(err, "")
}

func (s accountStore) EnableLinkedAccount(source, subject string) error {
	_, err := s.st.EnableLinkedAccount(source, subject)
	return accountError(err, "")
}

func (s accountStore) RevokeLinkedAccountSessions(source, subject string) (int, error) {
	n, err := s.st.RevokeLinkedAccountSessions(source, subject)
	return n, accountError(err, "")
}

func (s accountStore) AccountsBySource(source string, after uint, limit int) ([]accounts.Account, uint, error) {
	rows, next, err := s.st.AccountsBySource(source, after, limit)
	if err != nil {
		return nil, 0, accountError(err, "")
	}
	out := make([]accounts.Account, len(rows))
	for i, r := range rows {
		out[i] = accounts.Account{Subject: r.Subject, UserID: r.UserID, Username: r.Username, Protected: r.Protected, Disabled: r.Disabled}
	}
	return out, next, nil
}

func (s accountStore) IssueSignInTicket(userID, linkID uint, source, handle string, ttl time.Duration) (string, error) {
	return s.st.IssueSignInTicket(userID, linkID, source, handle, ttl)
}

// accountError turns the store's account errors into the product errors a
// connector reads; other errors pass through.
func accountError(err error, username string) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, store.ErrNotFound):
		return messages.WrapProductError(messages.ErrorExternalIdentityNotFound, "no account is linked to this identity", nil, err)
	case errors.Is(err, store.ErrProtectedAccount):
		return messages.WrapProductError(messages.ErrorIdentityProtectedUser,
			"an identity connector cannot disable, enable, rename or sign out an administrator", nil, err)
	case errors.Is(err, store.ErrUsernameTaken):
		return messages.WrapProductError(messages.ErrorIdentityUsernameTaken, "another account already has this username",
			map[string]any{"username": username}, err)
	case errors.Is(err, store.ErrIdentityTaken):
		return messages.WrapProductError(messages.ErrorExternalIdentityTaken, "the external identity is already linked", nil, err)
	case errors.Is(err, store.ErrInvalidIdentity):
		return messages.WrapProductError(messages.ErrorExternalIdentityInvalid, "the source or subject is not a valid external identity", nil, err)
	}
	return err
}
