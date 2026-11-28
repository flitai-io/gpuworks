package dto

import "time"

// User is an account as an administrator manages it. The password hash is
// never part of any DTO.
type User struct {
	ID         uint       `json:"id"`
	Username   string     `json:"username"`
	Role       string     `json:"role"`        // admin | user
	LocalAdmin bool       `json:"local_admin"` // the floor of ADR 0071 decision 11
	Disabled   bool       `json:"disabled"`
	DisabledAt *time.Time `json:"disabled_at"`
	CreatedAt  time.Time  `json:"created_at"`
	UpdatedAt  time.Time  `json:"updated_at"`
}

// ExternalIdentity is one link from an account to an outside identity source
// (ADR 0085).
type ExternalIdentity struct {
	ID           uint       `json:"id"`
	Source       string     `json:"source"`
	Subject      string     `json:"subject"`
	CreatedAt    time.Time  `json:"created_at"`
	LastSignInAt *time.Time `json:"last_sign_in_at"`
}

// ExternalIdentityRequest links an account to (source, subject).
type ExternalIdentityRequest struct {
	Source  string `json:"source"`
	Subject string `json:"subject"`
}

// SignInOptions is GET /api/v1/auth/sign-in-options (ADR 0085 decision 6):
// how this server signs people in, for the sign-in page to render as is.
type SignInOptions struct {
	// LocalPassword is server.yaml's auth.local_password: "all", or
	// "local_admin" when only local administrators use the password form.
	LocalPassword string `json:"local_password"`
	// Entries are the identity sources a user can sign in through now, in
	// the order the extensions offered them; empty for none.
	Entries []SignInEntry `json:"entries"`
}

// SignInEntry is one "sign in with ..." button: URL is the core's
// /api/v1/auth/sign-in/start for the source.
type SignInEntry struct {
	Source string `json:"source"`
	Label  string `json:"label"`
	Icon   string `json:"icon,omitempty"`
	URL    string `json:"url"`
}
