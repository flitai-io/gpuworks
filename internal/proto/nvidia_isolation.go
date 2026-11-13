package proto

// NVIDIAIsolation levels and reasons.
const (
	// NVIDIAIsolationSoft: pods see every /dev/nvidiaN of the node and
	// CUDA_VISIBLE_DEVICES confines them to the card they were assigned.
	NVIDIAIsolationSoft = "soft"
	// NVIDIAIsolationCDINamesMissing: NVIDIA's container toolkit resolves
	// NVIDIA_VISIBLE_DEVICES as CDI names, and no CDI spec on the node names
	// the card (typically because HAMi or another device plugin replaced
	// NVIDIA's, which is what writes the per-card names).
	NVIDIAIsolationCDINamesMissing = "cdi_names_missing"
)

// The container toolkit an NVIDIAIsolation was found under: the GPU
// Operator's install, or one installed on the host. The fix differs.
const (
	NVIDIAToolkitGPUOperator = "gpu_operator"
	NVIDIAToolkitHost        = "host"
)

// NVIDIAIsolation is what a Kubernetes node's agent observes about how
// strictly its NVIDIA instance pods are kept to their own card
// (NodeStatus.NVIDIAIsolation).
type NVIDIAIsolation struct {
	// Level is NVIDIAIsolationSoft.
	Level string `json:"level"`
	// Reason is NVIDIAIsolationCDINamesMissing.
	Reason string `json:"reason"`
	// Toolkit is NVIDIAToolkitGPUOperator or NVIDIAToolkitHost, whose
	// toolkit configuration the agent read; empty when it found none.
	Toolkit string `json:"toolkit,omitempty"`
	// Cards is how many of the node's NVIDIA cards no CDI spec names.
	Cards int `json:"cards,omitempty"`
}
