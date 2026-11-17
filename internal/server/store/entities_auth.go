package store

import "time"

// User is a platform account. A single admin is bootstrapped at startup.
type User struct {
	ID           uint   `gorm:"primaryKey"`
	Username     string `gorm:"uniqueIndex;size:64"`
	PasswordHash string `gorm:"size:256"` // argon2id
	Role         string `gorm:"size:16"`  // admin | user
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

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
