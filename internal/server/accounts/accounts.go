// Package accounts holds the rules for accounts reached through external
// identities (ADR 0085): which extension owns an identity source, what a sign-in
// through a source does (find the linked account, or create one when the site
// provisions accounts), and what an identity connector may do to the accounts
// of its sources. Administrators are out of a connector's reach; the store
// enforces that in the transaction that writes. The package keeps no state
// beyond the source owners its Registry records while the extensions start;
// reads and writes go through the narrow Store it declares.
package accounts

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/serverconfig"
)

// TicketTTL is how long a sign-in ticket can be redeemed (ADR 0085 decision 3).
const TicketTTL = 60 * time.Second

// MaxPage bounds one page of Accounts.List.
const MaxPage = 500

// Profile is what an identity connector says about a user. It names no role:
// an external identity never makes anyone an administrator.
type Profile struct {
	Username    string
	DisplayName string
	Email       string
}

// Account is one account linked through a source, as its connector sees it.
type Account struct {
	Subject  string
	UserID   uint
	Username string
	// Protected says the account is an administrator: the connector cannot
	// disable, enable, rename or sign it out.
	Protected bool
	Disabled  bool
}

// User is an account as the rules need it.
type User struct {
	ID       uint
	Username string
	// Admin: role admin, the local administrators among them.
	Admin    bool
	Disabled bool
}

// ErrNotLinked: no account is linked to the external identity.
var ErrNotLinked = errors.New("no account is linked to the external identity")

// Store is what the rules read and write. LinkedAccount answers ErrNotLinked
// for an identity without a link; every other refusal is a
// *messages.ProductError carrying the code a connector reads
// (EXTERNAL_IDENTITY_NOT_FOUND, EXTERNAL_IDENTITY_TAKEN,
// EXTERNAL_IDENTITY_INVALID, IDENTITY_PROTECTED_USER or
// IDENTITY_USERNAME_TAKEN). A disabled account is refused by SignIn, not by
// the Store. The writes through a source refuse an administrator in the
// transaction that writes, so an account promoted a moment earlier is never
// written as an ordinary one.
type Store interface {
	LinkedAccount(source, subject string) (u User, linkID uint, err error)
	CreateExternalUser(username, source, subject string) (u User, linkID uint, err error)
	RenameLinkedAccount(source, subject, username string) (User, error)
	DisableLinkedAccount(source, subject string) error
	EnableLinkedAccount(source, subject string) error
	RevokeLinkedAccountSessions(source, subject string) (int, error)
	// AccountsBySource pages through the accounts linked through source in
	// link order: those after the link id after, at most limit; next is the
	// after of the next page, 0 after the last.
	AccountsBySource(source string, after uint, limit int) (accounts []Account, next uint, err error)
	IssueSignInTicket(userID, linkID uint, source, handle string, ttl time.Duration) (string, error)
}

var sourceName = regexp.MustCompile(`^[a-z][a-z0-9_]{1,31}$`)

// Registry records which extension owns each identity source
// (user_identities.source) and hands each extension its Accounts. Sources are
// declared while the extensions start, before any request; afterwards the
// registry is only read.
type Registry struct {
	st        Store
	provision string
	owner     map[string]string
}

// NewRegistry is the registry of a server whose site policy for unlinked
// identities is provision (server.yaml auth.external.provision; empty is
// serverconfig.ProvisionAuto).
func NewRegistry(st Store, provision string) *Registry {
	if provision == "" {
		provision = serverconfig.ProvisionAuto
	}
	return &Registry{st: st, provision: provision, owner: map[string]string{}}
}

// Declare claims sources for owner. A name belongs to one owner; claiming one
// another owner claimed is IDENTITY_SOURCE_CONFLICT, and nothing is claimed.
func (r *Registry) Declare(owner string, sources ...string) error {
	for _, s := range sources {
		if !sourceName.MatchString(s) {
			return fmt.Errorf("extension %s: identity source %q does not match [a-z][a-z0-9_]{1,31}", owner, s)
		}
		if prev, ok := r.owner[s]; ok && prev != owner {
			return messages.NewProductError(messages.ErrorIdentitySourceConflict,
				fmt.Sprintf("identity source %s belongs to extension %s", s, prev), map[string]any{"source": s})
		}
	}
	for _, s := range sources {
		r.owner[s] = owner
	}
	return nil
}

// Owns reports whether owner declared source.
func (r *Registry) Owns(owner, source string) bool { return r.owner[source] == owner }

// For is owner's view of the accounts of its sources.
func (r *Registry) For(owner string) *Accounts { return &Accounts{owner: owner, r: r} }

// Accounts is one extension's view of the accounts linked through the identity
// sources it declared. Every operation takes a source and refuses one the
// extension does not own (IDENTITY_SOURCE_CONFLICT); none can make an
// administrator, and none can disable, enable, rename or sign one out
// (IDENTITY_PROTECTED_USER). It is safe for concurrent use; its methods do I/O
// and must not be called from a hook.
type Accounts struct {
	owner string
	r     *Registry
}

func (a *Accounts) owns(source string) error {
	if !a.r.Owns(a.owner, source) {
		return messages.NewProductError(messages.ErrorIdentitySourceConflict,
			fmt.Sprintf("extension %s does not own identity source %s", a.owner, source), map[string]any{"source": source})
	}
	return nil
}

func validProfile(p Profile) error {
	if u := strings.TrimSpace(p.Username); u == "" || u != p.Username || utf8.RuneCountInString(u) > 64 {
		return messages.NewProductError(messages.ErrorExternalIdentityInvalid, "a profile needs a username of 1-64 characters without surrounding spaces", nil)
	}
	if utf8.RuneCountInString(p.DisplayName) > 128 || utf8.RuneCountInString(p.Email) > 254 {
		return messages.NewProductError(messages.ErrorExternalIdentityInvalid, "the profile's display name or email is too long", nil)
	}
	return nil
}

// SignIn is a verified sign-in through source: the core finds the account
// linked to subject, or creates one when the site provisions accounts
// (auth.external.provision: auto), and answers a one-time ticket bound to
// handle, the browser binding the core gave the connector's start route. The
// connector then sends the browser to /api/v1/auth/complete?ticket=<ticket>.
// An account is never found by username or email, and a sign-in never renames
// one: the username changes only through Upsert.
func (a *Accounts) SignIn(source, subject string, p Profile, handle string) (string, error) {
	if err := a.owns(source); err != nil {
		return "", err
	}
	if !validHandle(handle) {
		return "", messages.NewProductError(messages.ErrorIdentityTicketInvalid, "the sign-in did not start at the core's sign-in route", nil)
	}
	if err := validProfile(p); err != nil {
		return "", err
	}
	u, link, err := a.r.st.LinkedAccount(source, subject)
	if errors.Is(err, ErrNotLinked) {
		if a.r.provision == serverconfig.ProvisionExisting {
			return "", messages.NewProductError(messages.ErrorIdentityNotProvisioned,
				"no account is linked to this identity, and this server creates none", map[string]any{"source": source})
		}
		u, link, err = a.r.st.CreateExternalUser(p.Username, source, subject)
	}
	if err != nil {
		return "", err
	}
	if u.Disabled {
		return "", messages.NewProductError(messages.ErrorUserDisabled, "this account is disabled", nil)
	}
	return a.r.st.IssueSignInTicket(u.ID, link, source, handle, TicketTTL)
}

// Upsert is a directory sync of one member: it creates the account linked to
// subject (whatever the provisioning policy says: the directory is the site's
// own) or renames the linked account to p.Username. Renaming an administrator
// is IDENTITY_PROTECTED_USER.
func (a *Accounts) Upsert(source, subject string, p Profile) (Account, error) {
	if err := a.owns(source); err != nil {
		return Account{}, err
	}
	if err := validProfile(p); err != nil {
		return Account{}, err
	}
	u, _, err := a.r.st.LinkedAccount(source, subject)
	switch {
	case errors.Is(err, ErrNotLinked):
		u, _, err = a.r.st.CreateExternalUser(p.Username, source, subject)
	case err == nil && u.Username != p.Username:
		u, err = a.r.st.RenameLinkedAccount(source, subject, p.Username)
	}
	if err != nil {
		return Account{}, err
	}
	return Account{Subject: subject, UserID: u.ID, Username: u.Username, Protected: u.Admin, Disabled: u.Disabled}, nil
}

// Disable disables the account linked to subject (ADR 0085 decision 7).
func (a *Accounts) Disable(source, subject string) error {
	if err := a.owns(source); err != nil {
		return err
	}
	return a.r.st.DisableLinkedAccount(source, subject)
}

// Enable enables the account linked to subject again.
func (a *Accounts) Enable(source, subject string) error {
	if err := a.owns(source); err != nil {
		return err
	}
	return a.r.st.EnableLinkedAccount(source, subject)
}

// RevokeSessions ends every management session of the account linked to
// subject, as a sign-out at the identity source asks. It answers how many
// ended.
func (a *Accounts) RevokeSessions(source, subject string) (int, error) {
	if err := a.owns(source); err != nil {
		return 0, err
	}
	return a.r.st.RevokeLinkedAccountSessions(source, subject)
}

// List pages through the accounts linked through source, for a full
// directory sync's reconciliation. cursor is 0 for the first page and then
// the next List answered; next is 0 after the last page.
func (a *Accounts) List(source string, cursor uint, limit int) (accounts []Account, next uint, err error) {
	if err := a.owns(source); err != nil {
		return nil, 0, err
	}
	if limit <= 0 || limit > MaxPage {
		return nil, 0, fmt.Errorf("accounts: limit %d is outside 1-%d", limit, MaxPage)
	}
	return a.r.st.AccountsBySource(source, cursor, limit)
}

// validHandle: a handle the core minted is 64 lowercase hex characters.
func validHandle(h string) bool {
	if len(h) != 64 {
		return false
	}
	for i := 0; i < len(h); i++ {
		c := h[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
