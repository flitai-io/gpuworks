package dto

import (
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
)

// GPUHealthOverview counts this node's cards by the level their probe
// found, which is what the fleet's health looks like. It is not another
// way of saying "schedulable": a card at Warning still serves, and a card
// that takes no work for a reason other than health (lent to a virtual
// machine, held by somebody else's process) is at OK.
type GPUHealthOverview struct {
	OK      int `json:"ok"`
	Warning int `json:"warning"`
	Error   int `json:"error"`
}

// GPUAllocationOverview partitions physical devices by AllocationState.
// Allocatable includes available and partial devices; Allocated includes only
// allocated devices, never partial, unusable, or unreachable reservations.
type GPUAllocationOverview struct {
	Allocated   int `json:"allocated"`
	Allocatable int `json:"allocatable"`
	// Unusable counts the cards that take no work at all, for whatever
	// reason — the same verdict PhysicalGPU.Schedulable carries.
	Unusable int `json:"unusable"`
	// Unknown counts the cards on nodes the control plane cannot reach,
	// whose every reading is as their last report left it. They are
	// neither allocatable nor known to be unusable.
	Unknown int `json:"unknown"`
}

type GPUOverview struct {
	Total      int                   `json:"total"`
	Health     GPUHealthOverview     `json:"health"`
	Allocation GPUAllocationOverview `json:"allocation"`
}

type GPUOccupant struct {
	InstanceUUID   string `json:"instance_uuid"`
	DeploymentID   uint   `json:"deployment_id"`
	DeploymentName string `json:"deployment_name,omitempty"`
	State          string `json:"state"`
	SleepLevel     int    `json:"sleep_level,omitempty"` // 0 awake; a sleeper holds only its residue
	LimitBytes     int64  `json:"limit_bytes,omitempty"`
	SMLimitPct     int    `json:"sm_limit_pct,omitempty"`
}

type GPULoad struct {
	UtilizationPct *float64   `json:"utilization_pct,omitempty"`
	MemoryUsed     *float64   `json:"memory_used_bytes,omitempty"`
	TemperatureC   *float64   `json:"temperature_c,omitempty"`
	PowerWatts     *float64   `json:"power_watts,omitempty"`
	FanSpeedPct    *float64   `json:"fan_speed_pct,omitempty"`
	PCIeRXBPS      *float64   `json:"pcie_rx_bytes_per_sec,omitempty"`
	PCIeTXBPS      *float64   `json:"pcie_tx_bytes_per_sec,omitempty"`
	UpdatedAt      *time.Time `json:"updated_at,omitempty"`
}

type PhysicalGPU struct {
	Index             int    `json:"index"`
	UUID              string `json:"uuid"`
	Name              string `json:"name"`
	Vendor            string `json:"vendor"`
	ArchFamily        string `json:"arch_family,omitempty"`
	ArchID            string `json:"arch_id,omitempty"`
	ComputeCapability string `json:"compute_capability,omitempty"`
	// MemoryTotalBytes is capacity in bytes; zero means the agent could not
	// determine the card's memory capacity.
	MemoryTotalBytes      int64                       `json:"memory_total_bytes"`
	MemoryCapacityKnown   bool                        `json:"memory_capacity_known"`
	PowerLimitWatts       float64                     `json:"power_limit_watts,omitempty"`
	PCIeBDF               string                      `json:"pcie_bdf,omitempty"`
	NUMANode              int                         `json:"numa_node"`
	PartitionCapabilities []proto.PartitionCapability `json:"partition_capabilities,omitempty"`
	// Schedulable is whether work may be placed on this card, as the
	// server decided it (accelerator.CardSchedulable). A client reads it
	// rather than deriving one of its own from the fields below, which
	// explain why rather than whether.
	Schedulable bool `json:"schedulable"`
	// Ownership is empty for a card this node may use, otherwise why not
	// (accelerator.Card*). It is one of the reasons Schedulable can be
	// false, and the client shows it instead of a health verdict: a card
	// lent to a virtual machine is not faulty.
	Ownership string `json:"ownership,omitempty"`
	// ForeignProcesses are compute processes on the card that none of the
	// platform's instances account for. The other reason Schedulable can
	// be false without anything ailing the card.
	ForeignProcesses []proto.CardProcess `json:"foreign_processes,omitempty"`
	// SchedulingStopped: an operator took the card out of scheduling; only
	// they put it back. SchedulingAllowed: an operator enabled scheduling;
	// it persists until disabled, regardless of time or reports.
	// SchedulingActions are what an operator may do
	// to it now (stopped, schedulable), decided by the server.
	SchedulingStopped bool     `json:"scheduling_stopped,omitempty"`
	SchedulingAllowed bool     `json:"scheduling_allowed,omitempty"`
	SchedulingActions []string `json:"scheduling_actions,omitempty"`
	// SchedulingHeldBy names the extension source that holds the card out
	// of scheduling (a card holder); absent when none does. SchedulingHold
	// says why, as a product message a client renders in its language,
	// with Message as the English fallback. While it is held the card is
	// not Schedulable and SchedulingActions never offers schedulable.
	SchedulingHeldBy string          `json:"scheduling_held_by,omitempty"`
	SchedulingHold   *ProductMessage `json:"scheduling_hold,omitempty"`
	// HealthStatus is the level, normalized when the card was stored, so
	// it is always one of OK, Warning or Error.
	HealthStatus   string           `json:"health_status,omitempty"`
	HealthCode     string           `json:"health_code,omitempty"`
	HealthMessage  string           `json:"health_message,omitempty"`
	HealthEvidence []GPUHealthField `json:"health_evidence,omitempty"`
	HealthGuide    *GPUHealthGuide  `json:"health_guide,omitempty"`
	NodeUUID       string           `json:"node_uuid"`
	NodeName       string           `json:"node_name"`
	NodeState      string           `json:"node_state"`
	DriverVersion  string           `json:"driver_version,omitempty"`
	// AllocationState is what is being asked of this card right now:
	// unknown when its node is not connected and every reading is as the
	// last report left it; unusable when it takes no work at all (its
	// health, its ownership, or a process outside the platform); otherwise
	// how much of it is taken — available, partial or allocated.
	//
	// Why a card is unusable is in Schedulable's neighbours — HealthStatus,
	// Ownership and ForeignProcesses — rather than in this one word. There
	// used to be a second column, work_state, saying normal/abnormal/
	// unknown; it answered this same question from the same verdict and
	// the two could only ever agree.
	AllocationState string `json:"allocation_state"`
	AllocatedBytes  int64  `json:"allocated_bytes"`
	// SleepingBytes is the residue sleeping instances keep on the card. What
	// is left of the total after AllocatedBytes, SleepingBytes and
	// AvailableBytes is withheld for another reason (processes outside the
	// platform and the headroom beside them, or a node nobody can reach), so
	// a client need not call all of it a sleeper's.
	SleepingBytes int64 `json:"sleeping_bytes"`
	// ForeignBytes is the memory of processes outside the platform that the
	// free figure leaves out (planning.ForeignHold); zero when none is
	// counted, which includes holders whose memory is not known. What is
	// withheld beyond it on a card of an online node is the headroom the
	// deployment policy keeps beside them, so the two are told apart: one is
	// a reading of somebody's use, the other memory nobody uses.
	ForeignBytes   int64         `json:"foreign_bytes"`
	AvailableBytes int64         `json:"available_bytes"`
	Occupants      []GPUOccupant `json:"occupants"`
	Load           GPULoad       `json:"load"`
}

type GPUListResponse struct {
	Overview GPUOverview   `json:"overview"`
	Items    []PhysicalGPU `json:"items"`
}

// GPUPage is one page of GET /api/v1/gpus?page=…: the matching GPUs, the
// overview of every GPU, and the values the column filters offer.
type GPUPage struct {
	ListPage[PhysicalGPU]
	Overview GPUOverview `json:"overview"`
	Facets   GPUFacets   `json:"facets"`
}

// GPUFacets are the distinct nodes, models and vendors of every GPU.
type GPUFacets struct {
	Nodes   []GPUFacetNode `json:"nodes"`
	Models  []string       `json:"models"`
	Vendors []string       `json:"vendors"`
}

type GPUFacetNode struct {
	UUID string `json:"uuid"`
	Name string `json:"name"`
}

type DGPU struct {
	InstanceUUID   string `json:"instance_uuid"`
	DeploymentID   uint   `json:"deployment_id"`
	DeploymentName string `json:"deployment_name,omitempty"`
	State          string `json:"state"`
	SleepLevel     int    `json:"sleep_level,omitempty"` // 0 awake; a sleeper holds only its residue
	NodeUUID       string `json:"node_uuid"`
	NodeName       string `json:"node_name"`
	GPUIndex       int    `json:"gpu_index"`
	GPUUUID        string `json:"gpu_uuid,omitempty"`
	GPUName        string `json:"gpu_name,omitempty"`
	MemoryLimit    int64  `json:"memory_limit_bytes"`
	// PendingLimit is the deployment's memory limit when the slice runs
	// under another one: a change it could not take in place (its backend,
	// or a lower limit than it holds) reaches it at its next restart.
	PendingLimit int64 `json:"pending_limit_bytes,omitempty"`
	SMLimitPct   int   `json:"sm_limit_pct,omitempty"`
	// MemoryUsed and SMUtilPct are the slice's own readings, as its
	// partition backend's interposer accounts them; absent until a report
	// carries one.
	MemoryUsed *float64   `json:"memory_used_bytes,omitempty"`
	SMUtilPct  *float64   `json:"sm_util_pct,omitempty"`
	UpdatedAt  *time.Time `json:"updated_at,omitempty"`
	// MemoryUsedUnsupported and SMUtilUnsupported say the slice's backend
	// keeps no such reading (accelerator.SliceUsage), so its absence is not
	// a report still to come.
	MemoryUsedUnsupported bool `json:"memory_used_unsupported,omitempty"`
	SMUtilUnsupported     bool `json:"sm_util_unsupported,omitempty"`
}

type DGPUListResponse struct {
	Items []DGPU `json:"items"`
}
