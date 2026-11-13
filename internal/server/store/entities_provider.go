package store

import "time"

// Provider is a configured cloud account (credentials AES-GCM encrypted).
type Provider struct {
	ID          uint   `gorm:"primaryKey"`
	Name        string `gorm:"size:64"`       // user label
	Kind        string `gorm:"size:32;index"` // runpod | compshare | autodl | vast
	CredsCipher string `gorm:"type:text"`     // ciphertext (AES-GCM, base64) of JSON provider.Credentials
	State       string `gorm:"size:16"`       // valid | invalid
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// ProviderInstance is a vendor instance discovered under a provider account.
type ProviderInstance struct {
	ID             uint   `gorm:"primaryKey"`
	ProviderID     uint   `gorm:"index:idx_pi,unique,priority:1"`
	ExternalID     string `gorm:"size:128;index:idx_pi,unique,priority:2"`
	Name           string `gorm:"size:128"`
	GPUModel       string `gorm:"size:128"`
	GPUCount       int
	Power          string `gorm:"size:16"`
	PricePerHr     float64
	Region         string `gorm:"size:64"`
	PublicIP       string `gorm:"size:64"`
	SSHPort        int
	SSHUser        string `gorm:"size:32"`
	SSHConnections string `gorm:"type:text"` // JSON []provider.SSHConnection
	Kind           string `gorm:"size:16"`   // container | vm
	// ManagedCluster is the vendor's managed Kubernetes cluster the instance
	// is a node of (provider.Instance.Cluster); such an instance joins
	// through its cluster's DaemonSet, never by an agent installed on it.
	ManagedCluster string `gorm:"size:128"`
	NodeID         uint   `gorm:"index"` // linked gpuworks node once agent installed
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// ProviderBootstrapToken is a legacy provider-install record retained only for
// database cleanup/migration compatibility. Current provider installs create a
// pending Node and authenticate directly with its per-node token; this table
// is not consulted by the hub.
type ProviderBootstrapToken struct {
	ID         uint   `gorm:"primaryKey"`
	TokenHash  string `gorm:"uniqueIndex;size:128"`
	ProviderID uint   `gorm:"index"`
	ExternalID string `gorm:"size:128;index"`
	NodeID     uint   `gorm:"index"`
	ExpiresAt  time.Time
	UsedAt     *time.Time
	CreatedAt  time.Time
	UpdatedAt  time.Time
}
