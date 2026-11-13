// Package dto holds the API-facing shapes. Entities never cross the api
// boundary; store rows are mapped to these explicitly.
package dto

import (
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
)

// GPUHealthField is one value the agent read from the vendor tool when it
// judged the card, named as the tool names it. A field with no name is a
// sentence from an agent that predates structured evidence.
type GPUHealthField struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// GPUHealthGuide holds the vendor commands for a card's health finding: Probe
// reproduces the evidence fields, Recheck is the vendor tool's readable view
// of the same state, Actions are the recovery commands the tool itself names.
type GPUHealthGuide struct {
	Probe   string   `json:"probe"`
	Recheck string   `json:"recheck,omitempty"`
	Actions []string `json:"actions,omitempty"`
}

// GPU is the API view of a store.GPU.
type GPU struct {
	Index                 int                         `json:"index"`
	UUID                  string                      `json:"uuid"`
	Name                  string                      `json:"name"`
	ComputeCapability     string                      `json:"compute_capability"`
	ArchFamily            string                      `json:"arch_family"`
	Vendor                string                      `json:"vendor,omitempty"`
	ArchID                string                      `json:"arch_id,omitempty"`
	MemoryTotalBytes      int64                       `json:"memory_total_bytes"`
	MemoryCapacityKnown   bool                        `json:"memory_capacity_known"`
	PowerLimitWatts       float64                     `json:"power_limit_watts,omitempty"`
	PCIeBDF               string                      `json:"pcie_bdf,omitempty"`
	NUMANode              int                         `json:"numa_node"`
	PartitionCapabilities []proto.PartitionCapability `json:"partition_capabilities,omitempty"`
	MIGEnabled            bool                        `json:"mig_enabled,omitempty"`
	// Ownership is empty for a card this node may use, otherwise why not
	// (accelerator.Card*): it is reported so an operator sees the hardware
	// that is physically there.
	Ownership string `json:"ownership,omitempty"`
	// Schedulable is whether work may be placed on this card, as the
	// server decided it. A client reads it rather than deriving its own
	// rule from the fields that explain why.
	Schedulable bool `json:"schedulable"`
	// ForeignProcesses are compute processes on this card that none of the
	// platform's instances account for, so it is not offered for work
	// while they run.
	ForeignProcesses []proto.CardProcess `json:"foreign_processes,omitempty"`
	// SchedulingStopped: an operator took the card out of scheduling; only
	// they put it back. SchedulingAllowed: an operator let work onto it
	// although the platform judged it unusable, until a report judges it
	// afresh (about a minute). SchedulingActions are what an operator may do
	// to it now (stopped, schedulable), decided by the server.
	SchedulingStopped bool             `json:"scheduling_stopped,omitempty"`
	SchedulingAllowed bool             `json:"scheduling_allowed,omitempty"`
	SchedulingActions []string         `json:"scheduling_actions,omitempty"`
	HealthStatus      string           `json:"health_status,omitempty"`
	HealthCode        string           `json:"health_code,omitempty"`
	HealthMessage     string           `json:"health_message,omitempty"`
	HealthEvidence    []GPUHealthField `json:"health_evidence,omitempty"`
	HealthGuide       *GPUHealthGuide  `json:"health_guide,omitempty"`
}

// Node is the API view of a store.Node.
type Node struct {
	UUID string `json:"uuid"`
	// Scheduling is schedulable, cordoned or draining.
	Scheduling string `json:"scheduling"`
	// SchedulingBy is who set Scheduling: operator or platform; absent on a
	// schedulable node, or a cordon set before the author was recorded. The
	// platform lifts only its own.
	SchedulingBy string `json:"scheduling_by,omitempty"`
	// CordonReason is why the platform holds the node out of scheduling
	// now (runtime_gpu_blocked, card_needs_host_reboot, kube_unschedulable); absent unless
	// SchedulingBy is platform. A client renders the code in its language.
	CordonReason string `json:"cordon_reason,omitempty"`
	Name         string `json:"name,omitempty"`
	Token        string `json:"token,omitempty"`
	// InstallCommand installs this node's agent, and is also how a node
	// whose agent the platform cannot restore itself is brought back. It
	// carries the token, so it is returned only alongside it.
	InstallCommand string  `json:"install_command,omitempty"`
	State          string  `json:"state"`
	Hostname       string  `json:"hostname"`
	OS             string  `json:"os"`
	Kernel         string  `json:"kernel"`
	Arch           string  `json:"arch"`
	CPUModel       string  `json:"cpu_model"`
	CPUCores       int     `json:"cpu_cores"`
	MemTotalGiB    float64 `json:"mem_total_gib"`
	DiskTotalGiB   float64 `json:"disk_total_gib"`
	GPUCount       int     `json:"gpu_count"`
	// GPUProbeError is why the last accelerator probe failed and
	// GPUProbeFailedAt since when probes have been failing, so a reader
	// knows the cards shown are as the last successful probe left them and
	// for how long. Absent once a probe succeeds.
	GPUProbeError     string     `json:"gpu_probe_error,omitempty"`
	GPUProbeFailedAt  *time.Time `json:"gpu_probe_failed_at,omitempty"`
	GPUVendor         string     `json:"gpu_vendor,omitempty"`
	GPUDriver         string     `json:"gpu_driver,omitempty"`
	NvidiaDriver      string     `json:"nvidia_driver,omitempty"`
	Framework         string     `json:"framework,omitempty"`
	FrameworkVersion  string     `json:"framework_version,omitempty"`
	PathUsage         any        `json:"path_usage,omitempty"`
	RuntimeDriver     string     `json:"runtime_driver,omitempty"`
	RuntimePreference string     `json:"runtime_preference,omitempty"`
	// RuntimeChoices are the runtime preferences the operator may set on
	// this node (PATCH /nodes/{uuid}/runtime), in the order to offer them;
	// absent when the node has no choice to make (a Kubernetes node runs
	// its instances as pods of its cluster). A Mac offers only auto.
	RuntimeChoices []string `json:"runtime_choices,omitempty"`
	// RuntimePlugin is the installed vendor container plugin, for display
	// ("NVIDIA Container Toolkit 1.17.4"); absent when none is installed.
	RuntimePlugin string `json:"runtime_plugin,omitempty"`
	// RuntimeGPUPlugin names the vendor container plugin this node's
	// accelerators need under its runtime (accelerator.RuntimeGPUPlugin);
	// absent when they need none. It is what gpu_plugin_missing is missing.
	RuntimeGPUPlugin string `json:"runtime_gpu_plugin,omitempty"`
	// RuntimeGPUBlocked is the code for why this node's container runtime
	// cannot give a container its accelerators: the vendor plugin is not
	// installed (gpu_plugin_missing), or the daemon that has it was never
	// configured to use it (gpu_runtime_unconfigured). While it is set the
	// platform keeps the node cordoned, and turning scheduling back on is
	// shown as going against it. A client renders the code in its own
	// language, naming the plugin from RuntimeGPUPlugin.
	RuntimeGPUBlocked string `json:"runtime_gpu_blocked,omitempty"`
	// RuntimeFallbackFrom is the runtime the agent passed over (docker |
	// nerdctl), and RuntimeFallbackReason why: gpu_plugin_missing or
	// gpu_runtime_unconfigured when it cannot give a container this node's
	// cards, daemon_not_ready when its daemon did not answer while the agent
	// started, or gpu_ready once it can serve and the agent switches back when
	// the node is idle. RuntimeFallbackPlugin names the plugin that runtime needs
	// (accelerator.RuntimeGPUPlugin). All empty when nothing was passed over.
	RuntimeFallbackFrom   string `json:"runtime_fallback_from,omitempty"`
	RuntimeFallbackReason string `json:"runtime_fallback_reason,omitempty"`
	RuntimeFallbackPlugin string `json:"runtime_fallback_plugin,omitempty"`
	// AcceleratorPreference is the operator's choice of which vendor's
	// cards the node manages: auto, a vendor, or absent when never chosen
	// (which is automatic). AcceleratorVendors is every vendor whose cards
	// the agent last saw on the host's bus, the managed one (GPUVendor)
	// included, and on a Kubernetes node only the vendors that run there;
	// a client offers the choice only when it lists more than one.
	AcceleratorPreference string                      `json:"accelerator_preference,omitempty"`
	AcceleratorVendors    []proto.AcceleratorPresence `json:"accelerator_vendors,omitempty"`
	Runtimes              []string                    `json:"runtimes,omitempty"`
	PartitionCapabilities []proto.PartitionCapability `json:"partition_capabilities,omitempty"`
	Labels                map[string]string           `json:"labels"`
	// SystemLabels are the labels the platform generates from the node's own
	// facts (planning.SystemLabels). They are kept apart from Labels, which
	// an operator writes, because only one of the two can be edited.
	SystemLabels []SystemLabel `json:"system_labels,omitempty"`
	// ManagementAddress is the address the node's agent reaches the server
	// from: its address on the management network.
	ManagementAddress string `json:"management_address,omitempty"`
	// ComputeNetwork / ComputeAddress name the network the node would use
	// for compute traffic on its own (its compute network, else the
	// management network) and its address there.
	ComputeNetwork     string        `json:"compute_network,omitempty"`
	ComputeAddress     string        `json:"compute_address,omitempty"`
	ServerURL          string        `json:"server_url,omitempty"`
	ConnectMode        string        `json:"connect_mode,omitempty"`
	ServerDialAddress  string        `json:"server_dial_address,omitempty"`
	ServerDialListen   string        `json:"server_dial_listen,omitempty"`
	AgentVersionChange string        `json:"agent_version_change,omitempty"`
	AgentEligibility   string        `json:"agent_eligibility,omitempty"`
	AgentVersion       string        `json:"agent_version,omitempty"`
	Caps               []string      `json:"caps,omitempty"`
	LastRemoteAddr     string        `json:"last_remote_addr,omitempty"`
	LastRejectReason   string        `json:"last_reject_reason,omitempty"`
	LastRejectAt       *time.Time    `json:"last_reject_at,omitempty"`
	LastSeenAt         *time.Time    `json:"last_seen_at,omitempty"`
	RecoveryPhase      string        `json:"recovery_phase,omitempty"`
	RecoveryAttempts   int           `json:"recovery_attempts,omitempty"`
	LastProbeAt        *time.Time    `json:"last_probe_at,omitempty"`
	LastRecoveryError  string        `json:"last_recovery_error,omitempty"`
	HasSSHCredential   bool          `json:"has_ssh_credential"`
	CreatedAt          time.Time     `json:"created_at"`
	GPUs               []GPU         `json:"gpus"`
	Topology           *NodeTopology `json:"topology,omitempty"`
	// GPUIsolation is set while the node's NVIDIA instance pods are kept to
	// their own card by CUDA alone (soft isolation); absent when strict, and
	// while the node is offline.
	GPUIsolation *NodeGPUIsolation `json:"gpu_isolation,omitempty"`
}

// SystemLabel is one generated node label: a key in the reserved
// gpuworks.ai namespace, one of the node's values for it, and how to show
// that value. A key appears once per value a node carries.
//
// Display is the text to show when nothing translates the value - a node or
// network name, a GPU model, a CPU architecture. DisplayCode names an
// existing client translation for the values that are platform vocabulary
// (a vendor, the management network) and is absent for the rest.
type SystemLabel struct {
	Key         string `json:"key"`
	Value       string `json:"value"`
	Display     string `json:"display"`
	DisplayCode string `json:"display_code,omitempty"`
}

// Health is the /healthz payload.
type Health struct {
	Status   string `json:"status"`
	Version  string `json:"version"`
	Nodes    int    `json:"nodes"`
	Sessions int    `json:"sessions"`
}

// CardSchedulingRequest sets whether one card of a node takes new work: the
// card is named by its PCI address (pcie_bdf), or by its UUID when it has
// none; scheduling is stopped or schedulable.
type CardSchedulingRequest struct {
	PCIeBDF    string `json:"pcie_bdf,omitempty"`
	UUID       string `json:"uuid,omitempty"`
	Scheduling string `json:"scheduling"`
}

// NodeSchedulingRequest sets whether a node takes new replicas
// (PUT /nodes/{uuid}/scheduling): schedulable, cordoned (nothing new is
// launched there) or draining (cordoned, and the replicas a pool placed
// there are replaced on other nodes before they are retired).
type NodeSchedulingRequest struct {
	Scheduling string `json:"scheduling"`
}

// NodeReplicas is what still runs on a node, for following a drain
// (GET /nodes/{uuid}/replicas).
type NodeReplicas struct {
	DrainOffline bool          `json:"drain_offline,omitempty"`
	Scheduling   string        `json:"scheduling"`
	Items        []NodeReplica `json:"items"`
}

// NodeReplica is one instance on a node and whether a drain moves it: "pool"
// replicas are replaced on another node; a "pinned" deployment names only
// this node, so its replica waits until the node is schedulable or the
// deployment's node requirements change; a "named" one is a multi-node or PD
// rank that names its node: a drain starts its unit on other nodes first and
// moves it once that runs, or it waits when no node fits.
type NodeReplica struct {
	Instance     string `json:"instance"`
	DeploymentID uint   `json:"deployment_id"`
	Deployment   string `json:"deployment"`
	State        string `json:"state"`
	Kind         string `json:"kind"`
	// Latest scheduler feedback and failed attempt are observations, not a
	// separate persisted drain state. Only populated while awaiting replacement.
	DrainReason   *ScheduleReason `json:"drain_reason,omitempty"`
	DrainReasonAt *time.Time      `json:"drain_reason_at,omitempty"`
	DrainMessage  string          `json:"drain_message,omitempty"`
}

// NodeEviction is what POST /nodes/{uuid}/evict took off the node: the
// leaders it set draining, the replicas they fill rebuilt wherever they may
// run.
type NodeEviction struct {
	Evicted []string `json:"evicted"`
}
