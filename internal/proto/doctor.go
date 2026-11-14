package proto

// DoctorReport is the result of the node.doctor RPC: a structured self-check
// of the node the server renders in the reader's locale. Report is the same
// content rendered as English text (what `gpuworks-agent doctor` prints); an
// agent that predates the structured form returns only Report.
type DoctorReport struct {
	Report       string              `json:"report"`
	Host         *DoctorHost         `json:"host,omitempty"`
	Runtimes     []DoctorRuntime     `json:"runtimes,omitempty"`
	Accelerators *DoctorAccelerators `json:"accelerators,omitempty"`
	// Findings are the items that need attention, most severe first. Every
	// finding names the command its evidence came from and, where one exists,
	// the command a person runs to recheck it.
	Findings []DoctorFinding `json:"findings,omitempty"`
	// Facts are statements without a verdict (runtime plugin, device
	// injection, vendor diagnostics), in report order.
	Facts []DoctorFact `json:"facts,omitempty"`
}

// DoctorHost is the host overview.
type DoctorHost struct {
	Hostname      string       `json:"hostname"`
	OS            string       `json:"os"`
	Arch          string       `json:"arch"`
	Kernel        string       `json:"kernel"`
	CPUModel      string       `json:"cpu_model"`
	CPUCores      int          `json:"cpu_cores"`
	MemTotalBytes int64        `json:"mem_total_bytes"`
	MemAvailBytes int64        `json:"mem_avail_bytes"`
	Disks         []DoctorDisk `json:"disks"`
}

// DoctorDisk is the capacity of one filesystem path the node depends on.
type DoctorDisk struct {
	// Kind is the path's role: root, models, docker, udocker, storage.
	Kind       string `json:"kind"`
	Path       string `json:"path"`
	TotalBytes int64  `json:"total_bytes"`
	AvailBytes int64  `json:"avail_bytes"`
	// Mount describes the mount holding the path. An agent that predates it,
	// or one that could not read the mount table, sends none.
	Mount *DoctorMount `json:"mount,omitempty"`
}

// DoctorMount is the mount a doctor path lives on, as /proc/self/mountinfo
// states it.
type DoctorMount struct {
	// Point is the mount point; FSType the filesystem type (ext4, nfs4,
	// overlay); Source the device or remote export it was mounted from.
	Point  string `json:"point"`
	FSType string `json:"fs_type"`
	Source string `json:"source,omitempty"`
	// Options are the mount and superblock options in mountinfo's order.
	Options string `json:"options,omitempty"`
	// Network is set for a network filesystem (NFS, CIFS, CephFS, ...).
	Network bool `json:"network,omitempty"`
	// NoExec and ReadOnly mirror the mount flags of the same name.
	NoExec   bool `json:"noexec,omitempty"`
	ReadOnly bool `json:"read_only,omitempty"`
	// Ephemeral is set when the mount is a container's own layer: the node
	// runs in a container and the path is not on a volume, so the data is
	// lost when the container is recreated.
	Ephemeral bool `json:"ephemeral,omitempty"`
}

// Doctor runtime status values.
const (
	DoctorRuntimeOK          = "ok"
	DoctorRuntimeNotFound    = "not_found"
	DoctorRuntimeQueryFailed = "query_failed"
)

// DoctorRuntime is one container runtime or tool the node may use.
type DoctorRuntime struct {
	Name     string `json:"name"`
	Status   string `json:"status"`
	Version  string `json:"version,omitempty"`
	Selected bool   `json:"selected,omitempty"`
	// Command is what was run to read the version.
	Command string `json:"command"`
}

// DoctorAccelerators is the accelerator inventory as the vendor tool reports it.
type DoctorAccelerators struct {
	Vendor string      `json:"vendor"`
	Driver string      `json:"driver,omitempty"`
	GPUs   []DoctorGPU `json:"gpus"`
}

// DoctorGPU is one accelerator in the inventory.
type DoctorGPU struct {
	Index       int    `json:"index"`
	Name        string `json:"name"`
	MemoryBytes int64  `json:"memory_bytes"`
	PCIeBDF     string `json:"pcie_bdf,omitempty"`
	NUMANode    int    `json:"numa_node"`
	// Ownership is empty for a card this node may use, otherwise why not
	// (accelerator.Card*). A card lent to a virtual machine is not faulty
	// and carries no health finding, so without this it reads as a
	// perfectly good card that simply cannot be used.
	Ownership    string `json:"ownership,omitempty"`
	HealthStatus string `json:"health_status"`
	HealthCode   string `json:"health_code,omitempty"`
}

// Doctor finding levels.
const (
	DoctorLevelWarning = "warning"
	DoctorLevelError   = "error"
)

// Doctor finding codes that are not accelerator health codes (those use the
// accelerator health vocabulary).
const (
	DoctorDiskLow                = "disk_low"
	DoctorRuntimeUnavailable     = "runtime_unavailable"
	DoctorAcceleratorProbeFailed = "accelerator_probe_failed"
	DoctorCUDAInitFailed         = "cuda_init_failed"
	// DoctorAcceleratorUnrecognized: this host has a PCI function of a
	// vendor whose accelerators the platform supports, that none of that
	// vendor's rules recognize — typically a new product line at a class
	// nobody announced. The evidence carries the ids needed to add the
	// rule, which is why this is reported rather than dropped: the
	// alternative is a card that is simply missing from its node.
	DoctorAcceleratorUnrecognized = "accelerator_unrecognized"
	DoctorMultiGPUCheckFailed     = "multi_gpu_check_failed"
	// DoctorRuntimeProvisionFailed: the selected runtime could not be set up
	// on this node (evidence: the error of its last attempt).
	DoctorRuntimeProvisionFailed = "runtime_provision_failed"
	// DoctorRuntimeStateUntraversable: a directory above the agent's state
	// directory is closed to the dedicated user the runtime launches workloads
	// as, so every launch fails with "permission denied". Evidence: the
	// blocking directory, its mode and owner, the identity, and the fix.
	DoctorRuntimeStateUntraversable = "runtime_state_untraversable"
	// DoctorPathNoExec and DoctorPathReadOnly: a path the agent or runtime
	// executes from, or writes to, is on a mount that forbids it.
	DoctorPathNoExec   = "path_noexec"
	DoctorPathReadOnly = "path_read_only"
	// DoctorPathEphemeral: a path that must persist is on a container's own
	// layer.
	DoctorPathEphemeral = "path_ephemeral"
	// DoctorMacOSUnsupported: the Mac runs a macOS older than nodes support.
	// DoctorMacOSIdleSleep: the Mac sleeps when idle on its current power
	// source; with no instance running it then drops off the cluster.
	DoctorMacOSUnsupported = "macos_unsupported"
	DoctorMacOSIdleSleep   = "macos_idle_sleep"
	// DoctorPeerMemoryMissing: the host has RDMA NICs but the vendor's
	// peer-memory module is not loaded, so the NICs cannot register the
	// cards' memory and PD over RDMA cannot move KV between nodes. Evidence:
	// the module and the command that loads it.
	DoctorPeerMemoryMissing = "peer_memory_missing"
	// DoctorUCXNetlinkHang: the host kernel answers an IPv6 route dump in a
	// way that hangs UCX 1.20-1.22 (bundled with NIXL), so PD over NIXL cannot
	// start here. Evidence: the kernel release and the commands that stop it.
	DoctorUCXNetlinkHang = "ucx_netlink_hang"
)

// DoctorFinding is one item that needs attention.
type DoctorFinding struct {
	Code  string `json:"code"`
	Level string `json:"level"`
	// Targets are the accelerators an accelerator health finding covers, one
	// entry per card with that card's own evidence and commands.
	Targets []DoctorTarget `json:"targets,omitempty"`
	// Disk is the path a disk_low or path_* finding is about.
	Disk *DoctorDisk `json:"disk,omitempty"`
	// Probe is the command (or call) the evidence came from; Evidence is what
	// it returned; Recheck is the command a person runs to see the same state.
	Probe    string        `json:"probe,omitempty"`
	Evidence []DoctorField `json:"evidence,omitempty"`
	Recheck  string        `json:"recheck,omitempty"`
}

// DoctorTarget is one accelerator a finding covers.
type DoctorTarget struct {
	Index    int           `json:"index"`
	PCIeBDF  string        `json:"pcie_bdf,omitempty"`
	Evidence []DoctorField `json:"evidence,omitempty"`
	Probe    string        `json:"probe,omitempty"`
	Recheck  string        `json:"recheck,omitempty"`
	Actions  []string      `json:"actions,omitempty"`
	// Processes are the compute processes the vendor tool lists on the card;
	// a recovery action must wait for them to stop.
	Processes []DoctorProcess `json:"processes,omitempty"`
}

// DoctorField is one value read from a command, named as the command names it.
type DoctorField struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// DoctorProcess is a compute process on an accelerator.
type DoctorProcess struct {
	PID  int    `json:"pid"`
	Name string `json:"name"`
}

// Doctor fact codes.
const (
	DoctorFactRuntimeDetails  = "runtime_details"
	DoctorFactRuntimePlugin   = "runtime_plugin"
	DoctorFactDeviceAccess    = "device_access"
	DoctorFactDeviceInjection = "device_injection"
	DoctorFactFabricManager   = "fabric_manager"
	DoctorFactCUDARuntime     = "cuda_runtime"
	DoctorFactPeerAccess      = "peer_access"
	DoctorFactNCCL            = "nccl"
	// DoctorFactCardRuntime: what asking the host runtime about each of the
	// node's dies on demand found (passed, failed, not checked and why).
	DoctorFactCardRuntime = "card_runtime"
	// DoctorFactRuntimeIdentity: the identity the runtime's workloads run as
	// and why. DoctorFactNetworkFilesystem: a path on a network filesystem
	// (a supported setup, stated, not judged).
	DoctorFactRuntimeIdentity   = "runtime_identity"
	DoctorFactNetworkFilesystem = "network_filesystem"
	// macOS nodes: how much memory Metal lets the GPU keep resident, the power
	// source and sleep settings, and the application firewall's state.
	DoctorFactMetalWorkingSet = "metal_working_set"
	DoctorFactPower           = "power"
	DoctorFactMacOSFirewall   = "macos_firewall"
)

// DoctorFact is a statement without a verdict. Value is the raw result; the
// client labels it by Code. A fact whose statement the client must word in
// the reader's language carries it typed as well (Identity, Disks); Value then
// holds the English wording for the CLI and for clients that predate the field.
type DoctorFact struct {
	Code  string `json:"code"`
	Value string `json:"value"`
	// Identity is set on a runtime_identity fact.
	Identity *DoctorRunIdentity `json:"identity,omitempty"`
	// Disks are the paths a network_filesystem fact covers, all on one mount.
	Disks []DoctorDisk `json:"disks,omitempty"`
}

// DoctorRunIdentity is whom a runtime's workloads run as.
type DoctorRunIdentity struct {
	// Runtime is the runtime as operators know it (runveil).
	Runtime string `json:"runtime"`
	// Pending reports a runtime that sets itself up on first use and has not
	// yet; the fields below are then empty.
	Pending bool `json:"pending,omitempty"`
	// User is the dedicated account; empty means the agent's own user.
	User string `json:"user,omitempty"`
	UID  int    `json:"uid"`
	// Fallback is why the dedicated account is not used, as the system
	// reported it; empty when there was no fallback.
	Fallback string `json:"fallback,omitempty"`
}
