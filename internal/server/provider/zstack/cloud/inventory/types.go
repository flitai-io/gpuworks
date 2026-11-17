// Package inventory defines Cloud resource data shared by discovery, onboarding and claims.
package inventory

// ResourceCapacity describes a reported capacity; nil values are unknown.
type ResourceCapacity struct {
	Total     *int64 `json:"total"`
	Available *int64 `json:"available"`
	Reserved  *int64 `json:"reserved"`
	Unit      string `json:"unit"`
}

// CloudGPU identifies a discovered device and its observed Cloud state.
type CloudGPU struct {
	Model       string `json:"-"`
	Description string `json:"-"`
	ExternalID  string `json:"external_id"`
	PCIAddress  string `json:"pci_address"`
	State       string `json:"state"`
	Status      string `json:"status"`
	Allocation  string `json:"allocation"`
	Mode        string `json:"mode"`
	VMUUID      string `json:"vm_uuid,omitempty"`
}

// CloudResource describes a discovered host and its installation-time snapshot.
type CloudResource struct {
	Type              string           `json:"type"`
	ExternalID        string           `json:"external_id"`
	Name              string           `json:"name"`
	ManagementAddress string           `json:"management_address"`
	Hypervisor        string           `json:"hypervisor"`
	State             string           `json:"state"`
	Status            string           `json:"status"`
	CPU               ResourceCapacity `json:"cpu"`
	Memory            ResourceCapacity `json:"memory"`
	GPUs              []CloudGPU       `json:"gpus"`
	// SSH access is persisted with the provider instance; snapshots track resources.
	SSHUser string `json:"-"`
	SSHPort int    `json:"-"`
}

// GPUDevice keeps the host identity alongside the shared GPU inventory.
type GPUDevice struct {
	HostUUID string
	GPU      CloudGPU
}

type CPUReservation struct {
	Value                 int64
	HostOverrideAvailable bool
}
