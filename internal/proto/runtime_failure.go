package proto

// RuntimeFailure describes a rejected accelerator injection. The accompanying
// InstanceStatus.Message remains the human-readable fallback for old clients.
// It is a snapshot of host facts, not an instruction to change the host.
type RuntimeFailure struct {
	Code            string `json:"code"`
	Vendor          string `json:"vendor,omitempty"`
	OS              string `json:"os,omitempty"`
	Arch            string `json:"arch,omitempty"`
	Runtime         string `json:"runtime,omitempty"`
	RequiredOS      string `json:"required_os,omitempty"`
	RequiredArch    string `json:"required_arch,omitempty"`
	RequiredRuntime string `json:"required_runtime,omitempty"`
	Resource        string `json:"resource,omitempty"`
}

const (
	ErrorAcceleratorDeviceSelectionRequired = "ACCELERATOR_DEVICE_SELECTION_REQUIRED"
	ErrorAcceleratorRuntimeUnsupported      = "ACCELERATOR_RUNTIME_UNSUPPORTED"
	ErrorAcceleratorRuntimeUnreported       = "ACCELERATOR_RUNTIME_UNREPORTED"
	ErrorAcceleratorDeviceUnavailable       = "ACCELERATOR_DEVICE_UNAVAILABLE"
	ErrorAcceleratorInventoryUnavailable    = "ACCELERATOR_INVENTORY_UNAVAILABLE"
	ErrorAcceleratorResourceUnavailable     = "ACCELERATOR_RESOURCE_UNAVAILABLE"
)
