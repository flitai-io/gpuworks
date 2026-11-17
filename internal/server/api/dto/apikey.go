package dto

import "time"

// APIKey is the API view of a key (never includes the secret).
type APIKey struct {
	Name     string     `json:"name"`
	Prefix   string     `json:"prefix"`
	LastUsed *time.Time `json:"last_used,omitempty"`
	Created  time.Time  `json:"created_at"`
	// Revoked is when the key was revoked; a revoked key never authenticates
	// and is listed so its past usage keeps a name.
	Revoked *time.Time `json:"revoked_at,omitempty"`
	// AllDeployments: the key may call every deployment. Otherwise only
	// Deployments, which may have become empty when they were all deleted.
	AllDeployments bool               `json:"all_deployments"`
	Deployments    []APIKeyDeployment `json:"deployments"`
	// Priority is the key's request priority binding.
	Priority APIKeyPriority `json:"priority"`
}

// APIKeyDeployment is a deployment a restricted API key may call.
type APIKeyDeployment struct {
	ID         uint   `json:"id"`
	Name       string `json:"name"`
	ServedName string `json:"served_name"`
}
