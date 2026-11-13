package dto

// NodeGPUIsolation says a Kubernetes node's NVIDIA instance pods are kept to
// their own card by CUDA_VISIBLE_DEVICES alone: they see every /dev/nvidiaN
// of the node (soft isolation). It is present only while the agent says so,
// and the server derives Fixes from it; a client shows the text for Level,
// Reason and each fix Code in its own language and the commands as they are.
type NodeGPUIsolation struct {
	// Level is soft.
	Level string `json:"level"`
	// Reason is why: cdi_names_missing (NVIDIA's container toolkit resolves
	// CDI names and no CDI spec names the card, typically because HAMi or
	// another device plugin replaced NVIDIA's).
	Reason string `json:"reason"`
	// Toolkit is gpu_operator or host when the agent could tell whose
	// toolkit it is; the fixes offered are the ones that apply to it.
	Toolkit string `json:"toolkit,omitempty"`
	// Cards is how many of the node's NVIDIA cards no CDI spec names.
	Cards int                   `json:"cards,omitempty"`
	Fixes []NodeGPUIsolationFix `json:"fixes"`
}

// NodeGPUIsolationFix is one way back to strict isolation.
type NodeGPUIsolationFix struct {
	// Code names the fix and when it applies (gpu_operator_device_plugin,
	// gpu_operator_toolkit_legacy_append, gpu_operator_toolkit_legacy_create,
	// host_toolkit_legacy).
	Code string `json:"code"`
	// Commands are run in order, as written, where the fix's text says.
	Commands []string `json:"commands"`
}
