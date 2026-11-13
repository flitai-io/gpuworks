package store

import (
	"encoding/json"
	"time"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/proto"
)

// Node states say whether the control plane has the node's agent on the
// other end, and nothing else: what its accelerators are and whether they
// can be used are the cards' own facts (their health), because a node whose
// driver is wedged is exactly the one an operator needs a terminal on.
//
// The column holds what the control plane itself knows: connecting until a
// node's agent has ever arrived, online while a session is up, offline once
// it is gone. Out-of-band recovery does not write it — Node.RecoveryPhase
// carries that — so what a client is shown is Node.ConnectionState, which
// folds the two together in one place.
const (
	NodeStateConnecting = "connecting"
	NodeStateOnline     = "online"
	NodeStateOffline    = "offline"
)

// Node is a compute host. Dynamic per-GPU metrics live in tsdb, not here.
type Node struct {
	ID   uint   `gorm:"primaryKey"`
	UUID string `gorm:"uniqueIndex;size:64"`
	// Name is the user-managed display name. Hostname is observed from the agent.
	Name string `gorm:"size:255"`
	// Token is intentionally retained in plaintext so administrators can copy it
	// again from the node details page. TokenHash is the authentication index.
	Token        string `gorm:"uniqueIndex;size:128;default:null"`
	TokenHash    string `gorm:"size:128;index"` // sha256 hex of the node token
	AgentVersion string `gorm:"size:64"`
	State        string `gorm:"size:16;index"`
	Hostname     string `gorm:"size:255"`
	Labels       string `gorm:"type:text"` // JSON map[string]string, user-defined (decoded by NodeLabels)
	// Scheduling is an operator's say over new replicas on the node
	// (NodeScheduling): empty while it takes them, cordoned or draining
	// while it does not. Nothing else records it.
	Scheduling NodeScheduling `gorm:"size:16;not null;default:''"`
	// SchedulingBy is who set Scheduling to its current value, so the
	// platform lifts only its own cordons (SchedulingSource). Empty on a
	// schedulable node.
	SchedulingBy SchedulingSource `gorm:"size:16;not null;default:''"`

	// Hardware summary (denormalized for list views; full detail via GPUs).
	OS               string `gorm:"size:32"`
	Kernel           string `gorm:"size:64"`
	Arch             string `gorm:"size:16"`
	CPUModel         string `gorm:"size:255"`
	CPUCores         int
	MemTotalKB       int64
	DiskTotalKB      int64
	GPUCount         int
	GPUVendor        string `gorm:"size:32;index"`
	GPUDriver        string `gorm:"size:64"`
	NvidiaDriver     string `gorm:"size:32"`
	Framework        string `gorm:"size:32;index"`
	FrameworkVersion string `gorm:"size:64"`
	PathUsage        string `gorm:"type:text"` // JSON []proto.PathUsage, latest statfs snapshot
	// PathUsageObservedAt records an actual disk snapshot; heartbeats do not refresh it.
	PathUsageObservedAt *time.Time `json:"-"`
	// GPUProbeError is why the node's last accelerator probe failed, as the
	// agent worded it, and GPUProbeFailedAt since when its probes have been
	// failing; both empty once a probe succeeds. The cards and their count stay as the last successful
	// probe left them, so this is the only record that what is shown may be
	// out of date. It does not change the node's connection state: a node
	// whose driver stopped answering is still reachable, and is exactly the
	// one an operator needs a terminal on.
	GPUProbeError    string `gorm:"size:512"`
	GPUProbeFailedAt *time.Time

	// Connectivity.
	ServerURL         string `gorm:"size:512"` // server external address used during enrollment
	ConnectMode       string `gorm:"size:16"`  // agent_dial (default) | server_dial
	ServerDialAddress string `gorm:"size:128"` // server-reachable host:port for reverse connections
	ServerDialListen  string `gorm:"size:128"` // agent-side listen address restored by out-of-band reinstall
	// InstallDir is where an SSH install put the agent (its binary and
	// state), so out-of-band restart and reinstall reach this node's agent on
	// a host that runs several; empty for nodes installed another way, which
	// recovery treats as the default /opt/gpuworks.
	InstallDir string `gorm:"size:512"`
	// KubeClusterID is the Kubernetes cluster (KubeCluster) this node joined
	// from, and KubeNodeName its node name there; zero and empty for a node
	// that joined any other way. Enrollment finds a returning node by the pair.
	KubeClusterID uint   `gorm:"index:idx_node_kube,priority:1"`
	KubeNodeName  string `gorm:"size:253;index:idx_node_kube,priority:2"`
	// KubeUnschedulable is the node's cordon in Kubernetes, as its agent
	// last reported it. While it is set the platform cordons the node too,
	// and lifts that once Kubernetes does (store.UpdateNodeStatus).
	KubeUnschedulable bool
	LastRemoteAddr    string `gorm:"size:128"` // address observed by the control plane
	RuntimeDriver     string `gorm:"size:16"`  // docker | nerdctl | udocker | host (detected)
	RuntimePreference string `gorm:"size:16"`  // auto | docker | nerdctl | udocker (desired)
	RuntimePlugin     string `gorm:"size:128"` // selected vendor runtime plugin display name/version
	// RuntimeGPUBlocked is the agent's code for why the selected container
	// runtime cannot give a container this node's accelerators
	// (accelerator.RuntimeGPUBlocked*), and empty when it can. While it is
	// set the platform keeps the node cordoned: the cards are fine and
	// every deployment would still fail at container start. A code, so
	// the operator reads it in their own language.
	RuntimeGPUBlocked string `gorm:"size:64"`
	// PeerMemory is whether the node's RDMA NICs can register its cards'
	// memory (proto.PeerMemoryLoaded / PeerMemoryMissing), empty when the
	// node has no RDMA NIC, its vendor has no peer-memory module, or its
	// agent predates the report. PD modes over RDMA (pdmode.Mode.PeerMemory)
	// place only on nodes where it is loaded.
	PeerMemory string `gorm:"size:16"`
	// RuntimeFallbackFrom is the runtime the agent's auto-detection passed
	// over because it cannot give a container this node's accelerators
	// (docker | nerdctl), and RuntimeFallbackReason the code saying why
	// (accelerator.RuntimeGPUBlocked*, or accelerator.RuntimeGPUReady once
	// it can and the agent has yet to switch back). Both empty when
	// nothing was passed over. The server cannot derive them: it does not
	// know which runtimes a node has, only the one it runs.
	RuntimeFallbackFrom   string `gorm:"size:16"`
	RuntimeFallbackReason string `gorm:"size:64"`
	// AcceleratorPreference is the operator's choice of which vendor's
	// cards the node manages on a host that has more than one: "" when
	// never chosen, auto, or a vendor (accelerator.HasPCICards). The agent
	// keeps its own copy and applies a differing one only when idle.
	AcceleratorPreference string `gorm:"size:16"`
	// AcceleratorVendors is JSON []proto.AcceleratorPresence, the agent's
	// latest report of every vendor whose cards are in the host, the
	// managed one included; read and written whole, never queried into.
	// Only the agent can see the host's PCI bus, and the cards rows hold
	// only the managed vendor's.
	AcceleratorVendors string `gorm:"type:text"`
	Runtimes           string `gorm:"type:text"` // JSON []string of container runtimes the agent detected
	PartitionAssets    string `gorm:"type:text"` // JSON []proto.AssetReport
	PartitionProbes    string `gorm:"type:text"` // JSON []proto.PartitionProbeResult
	NetworkInventory   string `gorm:"type:text"` // JSON proto.NetInventory, latest agent report
	ServingForward     string `gorm:"type:text"` // JSON store.ServingForwardStatus, the gateway's last direct-path outcome
	// ManagementRolesOff lists the roles (netplan roles) the management network
	// may not carry for this node; JSON []string, empty means all of them.
	ManagementRolesOff      string `gorm:"type:text;column:management_roles_off"`
	PartitionSafetyProtocol int

	LastRejectReason  string `gorm:"size:64"`
	LastRejectAt      *time.Time
	LastSeenAt        *time.Time
	RecoveryPhase     string `gorm:"size:16;index"`
	RecoveryAttempts  int
	LastProbeAt       *time.Time
	LastRecoveryError string `gorm:"type:text"` // free text: the last out-of-band recovery error
	CreatedAt         time.Time
	UpdatedAt         time.Time

	// HasSSHCredential is a derived read-model flag populated by Store queries.
	// The encrypted credential remains in its dedicated table and never enters a
	// node DTO.
	HasSSHCredential bool `gorm:"-"`

	// Topology: raw `nvidia-smi topo -m` + precomputed model as JSON.
	TopoRaw  string `gorm:"type:text"` // free text: the topology tool output as the agent reported it
	TopoJSON string `gorm:"type:text"` // JSON topoparse.Topology

	GPUs []GPU `gorm:"foreignKey:NodeID;constraint:OnDelete:CASCADE"`
}

// BeforeSave keys a card row written directly, which is what the fixtures
// do: the unique index needs Ident, and a row built by hand would otherwise
// land without it. store.newCardRow is the only writer of a card from a
// probe's report and fills it itself — gorm runs hooks on the statement's
// model, so an update written as Model(&GPU{}).Updates(&row) never reaches
// this.
func (g *GPU) BeforeSave(*gorm.DB) error {
	if g.Ident == "" {
		g.Ident = cardIdent(proto.GPUInfo{UUID: g.UUID, PCIeBDF: g.PCIeBDF, Index: g.Index})
	}
	g.HealthStatus = accelerator.NormalizeHealthLevel(g.HealthStatus)
	return nil
}

// CanTakeWork is whether work may be placed on this card: this node's own,
// at a health level that still serves, and not taken by something outside
// the platform. It is the one rule, asked of the card's own facts, so it
// answers for a card read from the database and for one built in memory
// alike.
//
// Why not is told by Ownership, HealthStatus and HealthCode, each of which
// names something an operator can act on; a boolean could not.
func (g GPU) CanTakeWork() bool {
	return cardTakesWork(g.Ownership, g.HealthStatus, g.HeldOutsidePlatform(), g.SchedulingStopped, g.SchedulingAllowedAt != nil)
}

// cardTakesWork is CanTakeWork from the columns, for readers that aggregate
// cards in SQL rather than load them. The operator's decision comes first
// (card_scheduling.go): a card they stopped takes nothing, a card they
// allowed takes work whatever the platform judged — but never one that is
// not this node's, which no decision changes.
func cardTakesWork(ownership, health string, held, stopped, allowed bool) bool {
	switch {
	case stopped:
		return false
	case allowed:
		return accelerator.CardUsable(ownership)
	}
	return accelerator.CardSchedulable(ownership, health, held)
}

// HeldOutsidePlatform is whether a process outside the platform is using
// this card.
func (g GPU) HeldOutsidePlatform() bool {
	return g.ForeignProcesses != ""
}

// Healthy is whether the card is in a state to serve, which is what the
// agent's Healthy flag meant: this node's own, at a level that still
// serves. It is answered from the card's own two facts rather than stored
// beside them, because a stored copy could only ever repeat them or
// contradict them.
func (g GPU) Healthy() bool {
	return accelerator.CardHealthy(g.Ownership, g.HealthStatus)
}

// ForeignUse is who outside the platform is using this card, for showing
// it. Whether the card can be used asks only whether this is empty, so a
// list that cannot be parsed costs a name on screen rather than a wrong
// verdict.
func (g GPU) ForeignUse() []proto.CardProcess {
	if g.ForeignProcesses == "" {
		return nil
	}
	var procs []proto.CardProcess
	if err := json.Unmarshal([]byte(g.ForeignProcesses), &procs); err != nil {
		// The count still says the card is held; this only means we
		// cannot say by what.
		return []proto.CardProcess{{Name: "unknown"}}
	}
	return procs
}

// ConnectionState is the node's connection as a client is shown it: the
// stored state, except that a node whose agent the platform is actively
// restoring reads as connecting rather than offline. Every list, filter,
// count and badge folds it here so they cannot disagree; code that decides
// what may run where reads State, which the fold never turns into online.
func (n *Node) ConnectionState() string {
	if n == nil {
		return ""
	}
	if n.State == NodeStateOffline && RecoveryUnderway(n.RecoveryPhase) {
		return NodeStateConnecting
	}
	return n.State
}

// OwnedGPUs are the cards this node may run work on: Node.GPUs without the
// ones owned elsewhere (passed through to a virtual machine, or held by no
// driver at all). Those are reported and stored so an operator sees the
// hardware that is physically in the machine, and they are listed in the
// node's detail for that reason — but anything that decides what can run
// where, or how much capacity exists, asks for these.
//
// Ownership is not health: a card given to a VM is not faulty, and saying
// so would send somebody looking for a fault that is not there. It is
// still reported with Healthy false, because "this card cannot serve" is
// equally true of it; the two statements do not conflict.
func (n *Node) OwnedGPUs() []GPU {
	if n == nil {
		return nil
	}
	owned := make([]GPU, 0, len(n.GPUs))
	for _, gpu := range n.GPUs {
		if accelerator.CardUsable(gpu.Ownership) {
			owned = append(owned, gpu)
		}
	}
	return owned
}

// A GPU index is the accelerator's own identity, never a position in this
// slice. A node whose cards are partly passed through elsewhere reports a
// sparse set (0, 3, 4, 6 on a four-card Ascend host), so indexing GPUs by a
// hardware index is both wrong on a mixed node and out of range on a sparse
// one. Every lookup of a scheduler-chosen GPU goes through here.
func (n *Node) GPUByIndex(index int) (GPU, bool) {
	if n == nil {
		return GPU{}, false
	}
	for _, gpu := range n.OwnedGPUs() {
		if gpu.Index == index {
			return gpu, true
		}
	}
	return GPU{}, false
}

// VendorOf is the canonical vendor of one of the node's cards
// (accelerator.GPUVendor over the card's row and the node's framework).
func (n *Node) VendorOf(g GPU) string {
	framework := ""
	if n != nil {
		framework = n.Framework
	}
	return accelerator.GPUVendor(g.Vendor, g.Name, g.ArchID, g.ComputeCapability, framework)
}

// ArchOf is the architecture of one of the node's cards: its family and
// most specific identity, derived from the stored facts (accelerator.GPUArch
// over the card's vendor, ArchID and compute capability). Neither is stored.
func (n *Node) ArchOf(g GPU) accelerator.Arch {
	return accelerator.GPUArch(n.VendorOf(g), g.ArchID, g.ComputeCapability)
}

// GPU is a physical accelerator on a node (real table, not a JSON view).
// Name is the normalized model name (accelerator.NormalizeGPUName). The
// architecture is stored as raw facts only: ComputeCapability for NVIDIA,
// ArchID for every other vendor (empty for NVIDIA, whose sm_XY follows from
// the capability); Node.ArchOf derives the rest.
type GPU struct {
	ID     uint `gorm:"primaryKey"`
	NodeID uint `gorm:"index:idx_node_card,unique,priority:1"`
	// Ident is the key this card is stored under: its hardware identity, or
	// its device index for a prober that reports no identity (store.cardIdent).
	// The device index cannot be the key — a card the vendor CLI does not
	// list has none, and the ones it does list are renumbered whenever that
	// set changes.
	Ident string `gorm:"size:96;index:idx_node_card,unique,priority:2"`
	// Index is the vendor CLI's device index, or -1 when it does not list
	// the card (passed through, no driver, or its driver could not bring it
	// up). It is a fact about the card, not a way to refer to it.
	Index int
	// Ownership is empty for a card this node may use, otherwise why not
	// (accelerator.Card*). A card owned elsewhere is stored so an operator
	// sees the hardware that is there, and is never scheduled.
	Ownership         string `gorm:"size:16"`
	UUID              string `gorm:"size:64;index"`
	Name              string `gorm:"size:128"`
	ComputeCapability string `gorm:"size:16"`
	Vendor            string `gorm:"size:32;index"`
	ArchID            string `gorm:"size:32"`
	MemoryTotal       int64
	PowerLimitWatts   float64
	PCIeBDF           string `gorm:"size:32"`
	NUMANode          int
	MIGEnabled        bool
	// ForeignProcesses is the JSON []proto.CardProcess of the compute
	// processes outside the platform that hold this card, for showing who
	// holds it; "" when nobody does (encodeCardProcesses), which is all
	// CanTakeWork asks of it, so the verdict never depends on parsing.
	// Read and written whole, never queried into.
	ForeignProcesses string `gorm:"type:text"`
	// HealthStatus is the level, normalized on the way in: an agent that
	// predates levels reports only a boolean, and store.newCardRow turns
	// it into one of OK, Warning or Error (accelerator.CardHealthLevel)
	// before it is stored. Nothing below this line re-derives it, so the
	// boolean is not kept beside it — Healthy() answers from this and the
	// ownership, which is what it always meant.
	HealthStatus  string `gorm:"size:32"`
	HealthCode    string `gorm:"size:64"`
	HealthMessage string `gorm:"type:text"` // the agent's health evidence: accelerator.FormatHealthEvidence lines; agents before the evidence format wrote one sentence (accelerator.ParseHealthEvidence reads both)

	// The operator's say over this card (card_scheduling.go). Reports never
	// write these: they are not facts of the card but decisions about it,
	// kept on its row (keyed by the card's identity, so they cannot drift
	// to another card) so that placing work reads no second table.
	//
	// SchedulingStopped: the operator took the card out of scheduling. It
	// holds until they put it back; nothing the platform sees lifts it.
	SchedulingStopped bool `gorm:"not null;default:false"`
	// SchedulingAllowedAt: when the operator let work onto a card the
	// platform had judged unusable (a fault, or a process outside the
	// platform). It holds until the first report at least
	// cardAllowanceWindow later, which judges the card afresh.
	SchedulingAllowedAt *time.Time

	CreatedAt time.Time
	UpdatedAt time.Time
}
