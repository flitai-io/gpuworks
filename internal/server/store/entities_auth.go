package store

import "time"

// User is a platform account. A single admin is bootstrapped at startup.
type User struct {
	ID           uint   `gorm:"primaryKey"`
	Username     string `gorm:"uniqueIndex;size:64"`
	PasswordHash string `gorm:"size:256"` // argon2id
	Role         string `gorm:"size:16"`  // admin | user
	// LocalAdmin marks the break-glass floor of the plugin host (ADR 0071,
	// decision 11). It counts only while Role is admin and the request comes
	// from a login session (auth.Claims.LocalAdmin): a demoted user loses it
	// without a write, and a platform token never carries it. Migration 225
	// set it for every administrator; EnsureAdmin and reset-password set it
	// for theirs; users created later start without it.
	LocalAdmin bool `gorm:"not null;default:false"`
	// DisabledAt is when the account was disabled (ADR 0085): empty while the
	// user is enabled. A disabled user cannot sign in, and their platform
	// tokens and inference API keys are refused with USER_DISABLED. Sessions
	// are deleted when the account is disabled; their data and keys are kept,
	// so enabling the account again restores everything.
	DisabledAt *time.Time
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// UserIdentity links a user to an identity in an outside identity source
// (ADR 0085): one row per (source, subject). Subject is the identity source's
// own immutable id, never a username or an email. The source is named by the
// extension (a plugin package or a compiled module) that owns it.
type UserIdentity struct {
	ID     uint `gorm:"primaryKey"`
	UserID uint `gorm:"index;not null"`
	// Source is the extension's source name, [a-z][a-z0-9_]{1,31}.
	// idx_user_identities_source orders one source's links by id, for the
	// connector's paged reads (Store.AccountsBySource).
	Source string `gorm:"size:32;not null;uniqueIndex:idx_user_identities_source_subject,priority:1;index:idx_user_identities_source"`
	// Subject is the identity source's id for this user.
	Subject string `gorm:"size:256;not null;uniqueIndex:idx_user_identities_source_subject,priority:2"`
	// CreatedAt is when the link was made; LastSignInAt the last sign-in
	// through it (empty before the first).
	CreatedAt    time.Time
	LastSignInAt *time.Time
}

func (UserIdentity) TableName() string { return "user_identities" }

// IsLocalAdmin reports whether the account holds the break-glass floor of
// ADR 0071 decision 11: the stored flag counts only while the role is admin,
// so a demoted user loses it without a write.
func (u User) IsLocalAdmin() bool { return u.LocalAdmin && u.Role == "admin" }

// IsAdmin reports whether the account is an administrator; the local
// administrators are among them. An administrator is out of an identity
// connector's reach (ADR 0085 decision 4).
func (u User) IsAdmin() bool { return u.Role == "admin" }

// SignInTicket is a one-time sign-in ticket an identity connector obtained for
// a user (ADR 0085 decision 3): the core redeems it at /api/v1/auth/complete in
// the browser that started the sign-in, then deletes it. Only the ticket's
// hash is kept. Rows live 60 seconds; every issue sweeps the expired ones, so
// the table holds the sign-ins of the last minute.
type SignInTicket struct {
	// TokenHash is HashToken of the ticket the connector was handed.
	TokenHash string `gorm:"primaryKey;size:64"`
	UserID    uint   `gorm:"not null"`
	// IdentityID is the user_identities row the sign-in went through; its
	// last_sign_in_at is set when the ticket is redeemed.
	IdentityID uint `gorm:"not null"`
	// Source is the identity source the ticket was issued for, and Handle
	// the browser binding's handle it is bound to.
	Source    string    `gorm:"size:32;not null"`
	Handle    string    `gorm:"size:64;not null"`
	CreatedAt time.Time `gorm:"not null"`
	ExpiresAt time.Time `gorm:"not null;index"`
}

func (SignInTicket) TableName() string { return "sign_in_tickets" }

// APIKey authenticates data-plane (/v1) and API access. The prefix is stored in
// clear for O(1) lookup; the full key is verified against an argon2id hash.
// A revoked key stays, so its usage keeps a name; it never authenticates
// again and its prefix is never reused.
//
// Restricted false lets the key reach every deployment. True limits it to the
// deployments granted in api_key_deployments; a restricted key whose grants
// are all gone (the deployments were deleted) reaches none.
type APIKey struct {
	ID         uint   `gorm:"primaryKey"`
	Name       string `gorm:"size:128"`
	Prefix     string `gorm:"uniqueIndex;size:16"` // first chars, clear
	Hash       string `gorm:"size:256"`            // argon2id of full key
	UserID     uint   `gorm:"index"`
	CreatedAt  time.Time
	LastUsed   *time.Time
	RevokedAt  *time.Time
	Restricted bool
	// PriorityClassID is the priority class the key's requests run at (nil:
	// the default class), PriorityCeilingClassID the highest class a request
	// may ask for (nil: the key's own class), and PriorityPolicy how a
	// request may choose (package priority; empty is the default policy).
	PriorityClassID        *uint  `gorm:"index"`
	PriorityCeilingClassID *uint  `gorm:"index"`
	PriorityPolicy         string `gorm:"size:16;not null;default:''"`
}

// APIKeyDeployment grants a restricted API key one deployment. Rows go with
// the deployment when it is deleted, so a later deployment that happens to
// reuse its ID is never granted by accident.
type APIKeyDeployment struct {
	APIKeyID     uint `gorm:"primaryKey;autoIncrement:false"`
	DeploymentID uint `gorm:"primaryKey;autoIncrement:false;index"`
}

// ManagementSession stores only the hash of a browser or management credential.
// Deleting the row revokes every JWT or Cookie bound to that login.
type ManagementSession struct {
	TokenHash         string `gorm:"primaryKey;size:64"`
	UserID            uint   `gorm:"index"`
	CreatedAt         time.Time
	ExpiresAt         time.Time `gorm:"index"`
	AbsoluteExpiresAt time.Time `gorm:"index"`
}
