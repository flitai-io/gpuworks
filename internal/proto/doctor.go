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
	// Findings are the check results that need attention: fail, then warn,
	// then skip. Every
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

// Doctor check ids (ADR 0067: which check this is, lower_snake). A finding's
// Code says what the check found, or, for a skipped check, why it did not run.
const (
	DoctorCheckDiskFree              = "disk_free"
	DoctorCheckMounts                = "mounts"
	DoctorCheckPathExec              = "path_exec"
	DoctorCheckPathWrite             = "path_write"
	DoctorCheckPathPersist           = "path_persist"
	DoctorCheckRuntimeAvailable      = "runtime_available"
	DoctorCheckRuntimeProvision      = "runtime_provision"
	DoctorCheckRuntimeStateAccess    = "runtime_state_access"
	DoctorCheckAcceleratorInventory  = "accelerator_inventory"
	DoctorCheckAcceleratorRecognized = "accelerator_recognized"
	DoctorCheckAcceleratorHealth     = "accelerator_health"
	DoctorCheckCardRuntime           = "card_runtime"
	DoctorCheckCUDAInit              = "cuda_init"
	DoctorCheckPeerAccess            = "peer_access"
	DoctorCheckNCCL                  = "nccl"
	DoctorCheckPeerMemory            = "peer_memory"
	DoctorCheckMacOSVersion          = "macos_version"
	DoctorCheckMacOSSleep            = "macos_sleep"
	DoctorCheckUCXNetlink            = "ucx_netlink"
)

// Doctor finding codes that are not accelerator health codes (those use the
// accelerator health vocabulary). UPPER_SNAKE (ADR 0067); an agent that
// predates that sent them in lower_snake, which NormalizeDoctorReport folds.
const (
	DoctorDiskLow                = "DISK_LOW"
	DoctorRuntimeUnavailable     = "RUNTIME_UNAVAILABLE"
	DoctorAcceleratorProbeFailed = "ACCELERATOR_PROBE_FAILED"
	DoctorCUDAInitFailed         = "CUDA_INIT_FAILED"
	// DoctorAcceleratorUnrecognized: this host has a PCI function of a
	// vendor whose accelerators the platform supports, that none of that
	// vendor's rules recognize — typically a new product line at a class
	// nobody announced. The evidence carries the ids needed to add the
	// rule, which is why this is reported rather than dropped: the
	// alternative is a card that is simply missing from its node.
	DoctorAcceleratorUnrecognized = "ACCELERATOR_UNRECOGNIZED"
	DoctorMultiGPUCheckFailed     = "MULTI_GPU_CHECK_FAILED"
	// DoctorRuntimeProvisionFailed: the selected runtime could not be set up
	// on this node (evidence: the error of its last attempt).
	DoctorRuntimeProvisionFailed = "RUNTIME_PROVISION_FAILED"
	// DoctorRuntimeStateUntraversable: a directory above the agent's state
	// directory is closed to the dedicated user the runtime launches workloads
	// as, so every launch fails with "permission denied". Evidence: the
	// blocking directory, its mode and owner, the identity, and the fix.
	DoctorRuntimeStateUntraversable = "RUNTIME_STATE_UNTRAVERSABLE"
	// DoctorPathNoExec and DoctorPathReadOnly: a path the agent or runtime
	// executes from, or writes to, is on a mount that forbids it.
	DoctorPathNoExec   = "PATH_NOEXEC"
	DoctorPathReadOnly = "PATH_READ_ONLY"
	// DoctorPathEphemeral: a path that must persist is on a container's own
	// layer.
	DoctorPathEphemeral = "PATH_EPHEMERAL"
	// DoctorMacOSUnsupported: the Mac runs a macOS older than nodes support.
	// DoctorMacOSIdleSleep: the Mac sleeps when idle on its current power
	// source; with no instance running it then drops off the cluster.
	DoctorMacOSUnsupported = "MACOS_UNSUPPORTED"
	DoctorMacOSIdleSleep   = "MACOS_IDLE_SLEEP"
	// DoctorPeerMemoryMissing: the host has RDMA NICs but the vendor's
	// peer-memory module is not loaded, so the NICs cannot register the
	// cards' memory and PD over RDMA cannot move KV between nodes. Evidence:
	// the module and the command that loads it. The peer-memory card check
	// warns with the same code.
	DoctorPeerMemoryMissing = "PEER_MEMORY_MISSING"
	// DoctorUCXNetlinkHang: the host kernel answers an IPv6 route dump in a
	// way that hangs UCX 1.20-1.22 (bundled with NIXL), so PD over NIXL cannot
	// start here. Evidence: the kernel release and the commands that stop it.
	DoctorUCXNetlinkHang = "UCX_NETLINK_HANG"
)

// Why a doctor check was skipped (a finding with Status CheckSkip). The
// card_runtime check also skips cards for the card checks' own reasons
// (CardCheckSkip*): one finding per reason, naming its cards as targets.
const (
	// DoctorSkipMountTableUnreadable: /proc/self/mountinfo could not be
	// read, so no path's mount was judged (evidence: the error).
	DoctorSkipMountTableUnreadable = "MOUNT_TABLE_UNREADABLE"
	// DoctorSkipCUDANotPassed: the check needs the CUDA driver, and the
	// CUDA check did not pass.
	DoctorSkipCUDANotPassed = "CUDA_CHECK_NOT_PASSED"
	// DoctorSkipPrerequisiteMissing: the check needs something the host
	// does not have (a library, a Python package, a working driver), as the
	// check's own output states in its evidence.
	DoctorSkipPrerequisiteMissing = "CHECK_PREREQUISITE_MISSING"
	// DoctorSkipToolFailed: the command the check reads failed or returned
	// nothing it could read (evidence: the command's error).
	DoctorSkipToolFailed = "CHECK_TOOL_FAILED"
	// DoctorSkipCancelled and DoctorSkipBudgetExhausted: the diagnosis was
	// cancelled, or ran out of its time budget, before it reached the
	// targets.
	DoctorSkipCancelled       = "CHECK_CANCELLED"
	DoctorSkipBudgetExhausted = "CHECK_BUDGET_EXHAUSTED"
	// DoctorSkipCardUnaddressable: the vendor tool gave the card no device
	// number to ask its runtime about.
	DoctorSkipCardUnaddressable = "CARD_UNADDRESSABLE"
)

// DoctorFinding is one check result that needs attention: a check that
// failed or warned, or one that should have run and did not (skip). A check
// that passed or does not apply is not listed.
type DoctorFinding struct {
	// ID is the check (DoctorCheck*); an agent that predates it sends none.
	ID string `json:"id,omitempty"`
	// Status is CheckFail, CheckWarn or CheckSkip.
	Status string `json:"status"`
	// Code is what the check found or, for a skip, why it did not run.
	Code string `json:"code"`
	// Level is what an agent that predates Status sent instead ("warning",
	// "error"). NormalizeDoctorReport folds it into Status and clears it;
	// nothing else reads it.
	Level string `json:"level,omitempty"`
	// Targets are the accelerators an accelerator health finding covers, one
	// entry per card with that card's own evidence and commands.
	Targets []DoctorTarget `json:"targets,omitempty"`
	// Disk is the path a DISK_LOW or PATH_* finding is about.
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
