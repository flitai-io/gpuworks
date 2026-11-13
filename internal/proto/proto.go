// Package proto defines the wire format shared by server (hub) and agent (conn).
//
// Transport: agent dials wss://server/api/v1/agent/connect, the WebSocket
// connection is adapted to an io.ReadWriteCloser and a yamux session is
// layered on top. Both ends are then peers over multiplexed streams.
//
// Every yamux stream begins with a one-line JSON stream header (StreamHeader).
// The ctrl stream then carries newline-delimited Envelope messages (JSON Lines).
package proto

import (
	"encoding/json"
	"net/url"
	"strings"
	"time"
)

// Protocol version range this build understands. Hello negotiates the overlap.
const (
	ProtoMin = 1
	ProtoMax = 1
)

// Stream kinds (first-frame stream header).
const (
	StreamCtrl             = "ctrl"               // agent opens; connection-lifetime; bidirectional JSON Lines, incl. metric/log batches
	StreamLogtail          = "logtail"            // server opens on demand
	StreamTunnel           = "tunnel"             // server opens per data-plane request
	StreamPeerRelaySend    = "peer.relay.send"    // server opens; source RPC on data, result on ctrl
	StreamPeerRelayReceive = "peer.relay.receive" // server opens; target-owned receiver only
	StreamModelRead        = "model.read"         // server opens; agent streams a tar archive of a model dir
	StreamModelWrite       = "model.write"        // server opens; agent receives a tar archive into a model dir
	StreamImageRead        = "image.read"         // server opens; agent streams docker save output
	StreamImageWrite       = "image.write"        // server opens; agent receives docker load input
	StreamTerminal         = "terminal"           // server opens; interactive pty, see terminal.go
	// StreamLargeFileRead is a bounded single-file read (header Dst = absolute
	// path on the node). It exists because the server occasionally needs a file
	// that already exists on a node — a tokenizer.json the model directory
	// holds — and neither fs.read_file (a general-purpose reader capped at
	// 512 KiB) nor model.read (a whole-directory tar) is that operation.
	StreamLargeFileRead = "file.large_read"
	// StreamAgentUpgrade carries a replacement agent binary from the server
	// (header Dst = artifact name, Ref = its sha256 hex, Size = its length).
	// The agent answers with one AgentUpgradeStatus JSON line once the binary
	// is verified, then installs it and execs. Pushing over the existing
	// connection is what lets a server_dial node, which cannot reach the
	// server's /dl/ URL, upgrade at all.
	StreamAgentUpgrade = "agent.upgrade"
	// StreamDiagnosticsRead carries a node's part of a diagnostics collection:
	// the agent answers with a tar of its own log (agent.log, agent.log.1)
	// and of the debug session files header Ref names (comma-separated
	// session ids). The agent chooses the files; the server names no path.
	StreamDiagnosticsRead = "diagnostics.read"
	// StreamTelemetry is opened by the agent on the data lane and carries its
	// metric and log batches as JSON Lines envelopes, so the node's bulkiest
	// traffic does not queue ahead of heartbeats and RPC responses on ctrl.
	// Header Ref names the batch type, TMetrics or TLogs: one stream each,
	// so the server holding logs back never holds metrics back. Only used
	// against a server whose hello advertises CapTelemetryStream.
	StreamTelemetry = "telemetry"
)

// LargeFileReadMaxBytes bounds one StreamLargeFileRead transfer. It matches the
// tokenizer loader's own limit, which is the largest single file the server has
// a reason to pull off a node; the agent refuses an oversized file before any
// byte leaves the machine.
const LargeFileReadMaxBytes = 64 << 20

const (
	LaneCtrl = "ctrl"
	LaneData = "data"
)

// LaneFor maps a stream kind to the lane that carries it. Unknown kinds, such
// as one from a newer peer, ride ctrl; every kind this build declares is
// classified explicitly and lane_test.go fails on an unclassified one.
func LaneFor(kind string) string {
	switch kind {
	case StreamPeerRelaySend, StreamPeerRelayReceive, StreamLogtail, StreamTunnel, StreamModelRead, StreamModelWrite, StreamImageRead, StreamImageWrite, StreamLargeFileRead, StreamAgentUpgrade,
		StreamStorageUpload, StreamStorageDownload, StreamDiagnosticsRead, StreamTelemetry:
		return LaneData
	default:
		return LaneCtrl
	}
}

// Terminal stream targets (StreamHeader.Target).
const (
	TermTargetHost      = "host"      // pty on the agent host
	TermTargetContainer = "container" // pty inside a running container (Ref = container id)
)

// Agent capabilities relevant to terminals, advertised in Hello.Caps.
//
// The two are separate because they became available in different releases and
// depend on different things: the host pty needs only a linux agent, while
// attaching to a running container also needs a runtime that supports it. The
// server refuses a target whose capability is absent rather than opening a
// session that can only fail.
const (
	CapWebterm          = "webterm"           // host pty
	CapWebtermContainer = "webterm.container" // exec into a running container
	// CapWebtermContainerUdocker is intentionally separate from the generic
	// container capability. udocker opens a controlled shell by starting a fresh
	// proot invocation with the tracked workload context; it is not an attach to
	// the model process.
	CapWebtermContainerUdocker = "webterm.container.udocker"
	CapBench                   = "bench" // embedded Go benchmark engine
	// Benchmark engine features newer than CapBench. An older engine ignores
	// the search fields they read: warmup_requests (CapBenchWarmup),
	// request_rates and the other open-loop fields (CapBenchArrival), and the
	// min_interactivity SLA floor (CapBenchInteractivity).
	CapBenchWarmup        = "bench.warmup"
	CapBenchArrival       = "bench.arrival"
	CapBenchInteractivity = "bench.interactivity"
	// CapBenchEvidence supports versioned request/session facts and the
	// min_session_completion constraint. Older engines ignore that floor.
	CapBenchEvidence = "bench.evidence"
	// CapBenchRequestTimeout applies the search document's request_timeout_s.
	// Older engines bound every request by a fixed five minutes.
	CapBenchRequestTimeout = "bench.request_timeout"
	// CapUpgradePush: the agent accepts StreamAgentUpgrade. Older agents only
	// upgrade by pulling /dl/ after the agent.upgrade RPC.
	CapUpgradePush = "upgrade.push"
	// CapKubeBinaryUpgrade: an agent of a Kubernetes DaemonSet that runs from
	// a binary on the node's state directory, so its upgrade replaces the
	// binary like any agent's. Without it the DaemonSet's image is changed.
	CapKubeBinaryUpgrade = "upgrade.kube_binary"
	// CapImagePullCandidates: image.pull accepts the whole candidate list and
	// the agent walks it itself.
	CapImagePullCandidates = "image.pull.candidates"
	// CapNetworkUpdate accepts network-only state.update without a full replay.
	CapNetworkUpdate = "state.update.network"
	// CapConnectionDiagnostics serves a lightweight connection snapshot over ctrl.
	CapConnectionDiagnostics = "node.connection_diagnostics"
	// CapPeerRelayV1 accepts transfer-scoped peer TLS relay streams on data.
	CapPeerRelayV1 = "peer.relay.v1"
)

// Server capabilities, advertised in the server's Hello.Caps.
const (
	// CapTelemetryStream: the server accepts agent-opened StreamTelemetry
	// streams on the data lane.
	CapTelemetryStream = "telemetry.stream"
)

// StreamHeader is the first line written on every yamux stream.
type StreamHeader struct {
	Kind string `json:"kind"`
	// Tunnel-only: destination address on the agent host, e.g. 127.0.0.1:38000.
	Dst string `json:"dst,omitempty"`
	// Logtail: instance/container identifier.
	// Terminal: container id when Target is TermTargetContainer.
	// Peer relay receive: active transfer ID.
	Ref string `json:"ref,omitempty"`
	// Terminal: host | container. Peer relay receive: model | image.
	Target string `json:"target,omitempty"`
	// Terminal-only: initial window size. Carried in the header so the shell
	// starts at the right size instead of drawing one frame at 80x24 before the
	// first resize frame lands.
	Rows uint16 `json:"rows,omitempty"`
	Cols uint16 `json:"cols,omitempty"`
	// Terminal-only: seconds to wait after SIGTERM before SIGKILL when reaping
	// the pty's process group. Server-configured (platform settings), so the
	// agent needs no settings of its own.
	GraceSec int `json:"grace_sec,omitempty"`
	// Upgrade: byte length of the binary that follows the header.
	// Storage upload: byte length of the body, -1 when the client sent none.
	Size int64 `json:"size,omitempty"`
	// Storage files (StreamStorageUpload, StreamStorageDownload): the storage
	// and the clean path relative to its root.
	StorageID uint   `json:"storage_id,omitempty"`
	Rel       string `json:"rel,omitempty"`
	// Storage upload: the staging path relative to the storage root
	// (StorageUploadStagingRel), the archive format to unpack the body from
	// into Rel ("" = store the body as the file Rel), and whether an existing
	// Rel may be replaced.
	Staging   string `json:"staging,omitempty"`
	Extract   string `json:"extract,omitempty"`
	Overwrite bool   `json:"overwrite,omitempty"`
	// Storage download: the archive format a directory is packed into.
	Format string `json:"format,omitempty"`
}

// Control-message envelope types (the "t" field).
const (
	THello              = "hello"                // handshake (both directions)
	THeartbeat          = "heartbeat"            // application-level liveness
	TRPCReq             = "rpc.req"              // request/response short actions
	TRPCResp            = "rpc.resp"             //
	TStateSync          = "state.sync"           // full desired-state replay (server→agent)
	TStateUpdate        = "state.update"         // incremental desired-state (server→agent)
	TStatusNode         = "status.node"          // agent→server node status (hardware/health)
	TAgentUpgradeStatus = "agent.upgrade.status" // agent→server asynchronous upgrade result
	TStatusInstance     = "status.instance"      // agent→server instance status
	TStatusBenchmark    = "status.benchmark"     // agent→server benchmark progress
	TRegistered         = "registered"           // server→agent: confirmed node identity
	TReject             = "reject"               // server→agent: handshake rejected before close
	TDataAck            = "data.ack"             // server→agent: data lane accepted, start yamux
)

// Handshake rejection reasons. These are the stable wire values carried in
// Reject.Reason on both the ctrl handshake and the data-lane handshake.
const (
	RejectBadToken      = "bad_token"      // node token missing or unrecognised
	RejectProtoMismatch = "proto_mismatch" // no overlapping protocol version
	RejectStaleSession  = "stale_session"  // data lane SessionID does not match the node's current ctrl session
)

// Envelope is one control-plane message on the ctrl stream.
type Envelope struct {
	T  string          `json:"t"`
	ID uint64          `json:"id,omitempty"`
	D  json.RawMessage `json:"d,omitempty"`
}

// Hello is exchanged first on the ctrl stream in both directions.
type Hello struct {
	ProtoMin int    `json:"proto_min"`
	ProtoMax int    `json:"proto_max"`
	Version  string `json:"version"`
	// Caps are the sender's capabilities: the agent's in its hello, the
	// server's in its reply.
	Caps []string `json:"caps,omitempty"`
	// RuntimeAssetDigests are validated SHA-addressed catalog assets already
	// present in the agent cache. The server omits their content from desired
	// state while retaining the immutable identity and mount contract.
	RuntimeAssetDigests []string `json:"runtime_asset_digests,omitempty"`

	// Agent → server identity. NodeToken is the required identity. The older
	// bootstrap/UUID fields remain as deprecated wire fields for older binaries,
	// but current servers do not authenticate them.
	BootstrapToken string `json:"bootstrap_token,omitempty"`
	NodeUUID       string `json:"node_uuid,omitempty"`
	NodeToken      string `json:"node_token,omitempty"`
	Fingerprint    string `json:"fingerprint,omitempty"`
	Hostname       string `json:"hostname,omitempty"`
	ConnectMode    string `json:"connect_mode,omitempty"` // agent_dial | server_dial
	Smoke          bool   `json:"smoke,omitempty"`        // validate auth/transport without registering a session
	// OwnModelsDir is the directory the agent keeps model files in when that
	// is not the storages' own directory: always for a non-root agent (it
	// cannot create /opt/gpuworks/...), and for a root agent outside the
	// standard /opt/gpuworks layout (a custom --state install must not have
	// its models relocated to the global default). The server records it as
	// the node's own directory for the default local storage when the node
	// has none; a directory an operator set is never replaced.
	OwnModelsDir string `json:"own_models_dir,omitempty"`
}

// Reject explains why a connection is being closed. It is sent before the
// socket is closed so agents can log an actionable reason instead of only EOF.
type Reject struct {
	Reason string `json:"reason"` // bad_token | proto_mismatch | stale_session
	Detail string `json:"detail,omitempty"`
}

// Registered is the server's reply granting a node identity (bootstrap only).
type Registered struct {
	NodeUUID  string `json:"node_uuid"`
	NodeToken string `json:"node_token"`
	Proto     int    `json:"proto"` // negotiated protocol version
	SessionID string `json:"session_id,omitempty"`
}

// DataHello is the first line an agent writes on a data-lane WebSocket, before
// layering yamux. The server validates NodeToken with the same per-node token
// check as the ctrl handshake and requires SessionID to equal the node's
// current ctrl SessionID, so a stale data lane can never attach to a newer ctrl
// session. On success the server replies with a TDataAck envelope; on failure
// it replies with a TReject envelope (RejectStaleSession / RejectBadToken).
type DataHello struct {
	NodeToken string `json:"node_token"`
	SessionID string `json:"session_id"`
}

// Heartbeat is the application-level liveness ping (agent→server).
type Heartbeat struct {
	UnixMS int64 `json:"unix_ms"`
}

// GPUInfo is one detected GPU reported in NodeStatus. It carries the facts
// the probe read; the architecture family is not one of them: the server
// derives it from Vendor, ArchID and ComputeCapability
// (accelerator.GPUArch), and ignores the arch_family key older agents send.
type GPUInfo struct {
	// Index is the device index the vendor CLI gives the card, or -1 when
	// the CLI does not list it: a card its driver could not bring up, one
	// passed through to a virtual machine, one with no driver. Such a card
	// has no index to be addressed by, which is why anything lasting (an
	// instance's placement, this card's stored row) keys on PCIeBDF.
	Index             int     `json:"index"`
	UUID              string  `json:"uuid"`
	Name              string  `json:"name"`
	MemoryTotalBytes  int64   `json:"memory_total_bytes"`
	ComputeCapability string  `json:"compute_capability"` // e.g. "8.9"
	Vendor            string  `json:"vendor,omitempty"`   // nvidia | ascend | ppu | amd | hygon | apple
	ArchID            string  `json:"arch_id,omitempty"`  // e.g. sm_90, 910B4, 910C, K100AI, ZW810E, an Apple SoC name
	PCIeBDF           string  `json:"pcie_bdf,omitempty"`
	NUMANode          int     `json:"numa_node"`
	DriverVersion     string  `json:"driver_version,omitempty"`
	PowerLimitWatts   float64 `json:"power_limit_watts,omitempty"`
	MIGEnabled        bool    `json:"mig_enabled,omitempty"` // legacy compatibility
	// Healthy is whether the card is in a state to serve. A current agent
	// fills it from the level and the ownership beside it
	// (accelerator.CardHealthy), so it adds nothing to them; it stays on
	// the wire because an agent predating health levels states only this,
	// and the server turns that into a level when it stores the card.
	Healthy       bool   `json:"healthy"`
	HealthStatus  string `json:"health_status,omitempty"`
	HealthCode    string `json:"health_code,omitempty"`
	HealthMessage string `json:"health_message,omitempty"`
	// ForeignProcesses are compute processes on this card that none of
	// this platform's instances account for: somebody started a workload
	// on the node by hand. The card is not given work while they run,
	// because its memory is already spoken for by something the platform
	// cannot see or plan around.
	ForeignProcesses []CardProcess `json:"foreign_processes,omitempty"`
	// Ownership says whether this node may use the card
	// (accelerator.Card*): empty when it is the node's own, which is also
	// what an agent predating the field reports for every card it lists.
	// A card owned elsewhere is reported so an operator sees the hardware
	// that is physically there, and is never scheduled.
	Ownership string `json:"ownership,omitempty"`
	// Checks are the state of each card check on this card
	// (CapCardChecks). They are reported, never stored with the card: the
	// server keeps them in memory.
	Checks []CardCheck `json:"checks,omitempty"`
}

// CardProcess is a compute process the vendor tool reports on a card.
type CardProcess struct {
	PID int `json:"pid"`
	// Name is the process as the vendor tool names it, which is the
	// executable rather than a command line.
	Name string `json:"name"`
	// MemoryBytes is what it holds on the card, 0 when the tool does not
	// say.
	MemoryBytes int64 `json:"memory_bytes,omitempty"`
}

// AssetReport is an agent-observed filesystem fact. It deliberately contains
// no Ready flag: readiness is derived by the server from the backend's current
// RequiredAssets list, so changing policy does not require an agent upgrade.
type AssetReport struct {
	Backend string   `json:"backend"`
	Root    string   `json:"root"`
	Files   []string `json:"files"`
}

const (
	PartitionProbePassed  = "passed"
	PartitionProbeFailed  = "failed"
	PartitionProbeSkipped = "skipped"
)

// PartitionProbeResult is the result of running the real partition injection
// path inside a short-lived container. Unlike AssetReport, this is behavioral
// evidence: a backend is ready only after a passed result has been reported.
type PartitionProbeResult struct {
	Backend   string    `json:"backend"`
	GPUIndex  *int      `json:"gpu_index,omitempty"` // nil is a legacy backend-wide result; 0 is a valid GPU index
	Status    string    `json:"status"`              // passed | failed | skipped
	Detail    string    `json:"detail,omitempty"`
	CheckedAt time.Time `json:"checked_at"`
}

// PartitionCapability is the server-derived, per-GPU partition fact exposed
// through the API. Status has exactly two values: ready or unavailable.
type PartitionCapability struct {
	Backend            string   `json:"backend"`
	DisplayName        string   `json:"display_name"`
	Vendor             string   `json:"vendor"`
	Maturity           string   `json:"maturity"`
	Status             string   `json:"status"`
	Reason             string   `json:"reason,omitempty"`
	Compatibility      string   `json:"compatibility_summary,omitempty"`
	CompatibilityNotes []string `json:"compatibility_notes,omitempty"`
	ProbeStatus        string   `json:"probe_status,omitempty"`
	// ProbeCheckedAt is absent until a probe result arrived: a zero time on
	// the wire rendered as "checked 0001-01-01".
	ProbeCheckedAt *time.Time `json:"probe_checked_at,omitempty"`
}

// Peer-memory states (SysInfo.PeerMemory).
const (
	PeerMemoryLoaded  = "loaded"
	PeerMemoryMissing = "missing"
)

// SysInfo is the host summary reported in NodeStatus.
type SysInfo struct {
	Hostname         string `json:"hostname"`
	OS               string `json:"os"`
	Kernel           string `json:"kernel"`
	Arch             string `json:"arch"`
	CPUModel         string `json:"cpu_model"`
	CPUCores         int    `json:"cpu_cores"`
	MemTotalKB       int64  `json:"mem_total_kb"`
	MemAvailKB       int64  `json:"mem_avail_kb"`
	DiskTotalKB      int64  `json:"disk_total_kb"`
	DiskAvailKB      int64  `json:"disk_avail_kb"`
	GPUVendor        string `json:"gpu_vendor,omitempty"`
	GPUDriver        string `json:"gpu_driver,omitempty"`
	NvidiaDriver     string `json:"nvidia_driver,omitempty"`
	Framework        string `json:"framework,omitempty"` // cuda | cann | hggc | rocm | dtk | metal
	FrameworkVersion string `json:"framework_version,omitempty"`
	ModelsRoot       string `json:"models_root,omitempty"`
	RuntimeDriver    string `json:"runtime_driver,omitempty"`
	RuntimePlugin    string `json:"runtime_plugin,omitempty"`
	// RuntimeGPUBlocked is the code for why the selected container runtime
	// cannot give a container this node's accelerators, or is empty when
	// it can (accelerator.RuntimeGPUBlocked*). The agent decides it,
	// because only the agent can see whether the vendor's container plugin
	// is installed; the server cannot tell from RuntimePlugin, which is a
	// display string. A code rather than a sentence, so the operator reads
	// it in their own language.
	RuntimeGPUBlocked string `json:"runtime_gpu_blocked,omitempty"`
	// PeerMemory is whether the host's RDMA NICs can register this node's
	// cards' memory: PeerMemoryLoaded or PeerMemoryMissing for the vendor's
	// peer-memory module, empty when the host has no RDMA NIC or the vendor
	// has no such module (and from an agent that predates the field). PD
	// modes that move the KV over RDMA run only where it is loaded.
	PeerMemory string `json:"peer_memory,omitempty"`
	// KubeUnschedulable is a Kubernetes node's own cordon (spec.unschedulable),
	// as its agent reads it; always false on other nodes.
	KubeUnschedulable bool `json:"kube_unschedulable,omitempty"`
	// RuntimeFallbackFrom is the runtime auto-detection passed over, and
	// RuntimeFallbackReason the code saying why: it cannot give a container
	// this node's accelerators (accelerator.RuntimeGPUBlocked*), its daemon
	// is installed but did not answer at startup
	// (RuntimeFallbackDaemonNotReady), or accelerator.RuntimeGPUReady once it
	// can serve and the agent has yet to switch back. Both empty when nothing
	// was passed over.
	RuntimeFallbackFrom   string   `json:"runtime_fallback_from,omitempty"`
	RuntimeFallbackReason string   `json:"runtime_fallback_reason,omitempty"`
	Runtimes              []string `json:"runtimes,omitempty"`
	// AcceleratorVendors is every vendor whose cards are in the host, the
	// one the node manages (GPUVendor) included, so the operator can choose
	// another (AcceleratorPreference). Empty when the host's PCI bus cannot
	// be read, as in a container.
	AcceleratorVendors []AcceleratorPresence `json:"accelerator_vendors,omitempty"`
	// Keep this field on the wire even when it is an empty, non-nil slice.
	// That lets the server distinguish a current agent that observed no files
	// ([]) from an older agent that did not know the field at all (null after
	// decoding). This is the same compatibility pattern used by image_refs.
	PartitionAssets []AssetReport `json:"partition_assets"`
	// Keep this field on the wire even when empty. nil after decoding therefore
	// identifies an older agent that predates behavioral partition probes.
	PartitionProbes         []PartitionProbeResult `json:"partition_probes"`
	PartitionSafetyProtocol int                    `json:"partition_safety_protocol,omitempty"`
}

// ImagePresence is the directory-driven image catalog report from an agent.
// SizeBytes is retained for legacy reports and transfer bookkeeping; it does
// not establish a runtime measurement. Zero means the legacy value is unknown.
type ImagePresence struct {
	// RuntimeSizeBytes is a separate runtime observation, never archive/progress bytes.
	// Nil means unknown; zero is a measured empty image.
	RuntimeSizeBytes *int64 `json:"runtime_size_bytes,omitempty"`
	ImageRef         string `json:"image_ref"`
	Present          bool   `json:"present"`
	SizeBytes        int64  `json:"size_bytes,omitempty"`
	// ImageID is the runtime's immutable identity. Empty means this report
	// did not observe one, so a digest already stored is left as it is.
	ImageID string `json:"image_id,omitempty"`
}

// ImageNodeStatus is an explicit image distribution progress update.
type ImageNodeStatus struct {
	ImageRef         string `json:"image_ref"`
	State            string `json:"state"`
	SizeBytes        int64  `json:"size_bytes,omitempty"`
	TransferredBytes int64  `json:"transferred_bytes,omitempty"`
	RateBPS          int64  `json:"rate_bps,omitempty"`
	TransferID       string `json:"transfer_id,omitempty"`
	SourceNodeUUID   string `json:"source_node_uuid,omitempty"`
	Source           string `json:"source,omitempty"`
	Message          string `json:"message,omitempty"`
	ImageID          string `json:"image_id,omitempty"`
}

// Path usage kinds that measure a storage: its directory or mount point on the
// node, and an nfs_cache storage's node cache.
const (
	PathKindStorage      = "storage"
	PathKindStorageCache = "storage_cache"
)

// PathUsage reports statfs for a managed path without recursively scanning it.
type PathUsage struct {
	Kind string `json:"kind"` // models | udocker | docker | storage | storage_cache
	// StorageID names the storage a storage or storage_cache entry measures.
	StorageID      uint   `json:"storage_id,omitempty"`
	Path           string `json:"path"`
	TotalBytes     int64  `json:"total_bytes"`
	AvailableBytes int64  `json:"available_bytes"`
}

// NodeStatus is the periodic node-level report (status.node).
type NodeStatus struct {
	Sys           SysInfo         `json:"sys"`
	GPUs          []GPUInfo       `json:"gpus"`
	GPUProbeOK    *bool           `json:"gpu_probe_ok,omitempty"`
	GPUProbeError string          `json:"gpu_probe_error,omitempty"`
	TopoRaw       string          `json:"topo_raw,omitempty"` // vendor topology CLI output
	Images        []ImagePresence `json:"images,omitempty"`
	PathUsage     []PathUsage     `json:"path_usage,omitempty"`
	// Network is nil from agents that predate network inventory reporting, so
	// the server keeps the previous inventory instead of clearing it.
	Network *NetInventory `json:"network,omitempty"`
	// RuntimeProxy is the proxy the container runtime itself pulls through,
	// for a runtime whose pulls run in a daemon (dockerd). Nil when the
	// runtime pulls in the agent's own process, or from an older agent.
	RuntimeProxy *RuntimeProxyStatus `json:"runtime_proxy,omitempty"`
}

// RuntimeProxyStatus is what an agent observes about the proxy its container
// runtime daemon uses, and what it can do about it.
type RuntimeProxyStatus struct {
	// HTTPProxy, HTTPSProxy and NoProxy are what the daemon reports it uses
	// (docker info). A daemon masks proxy credentials, so a password in them
	// reads as "xxxxx".
	HTTPProxy  string `json:"http_proxy"`
	HTTPSProxy string `json:"https_proxy"`
	NoProxy    string `json:"no_proxy"`
	// Configured is the proxy the daemon starts with: the docker unit's
	// environment as systemd has it loaded, every drop-in included, masked
	// like HTTPProxy. ConfiguredKnown is false when systemd could not be
	// asked. A daemon reporting something else has not been restarted since
	// its configuration changed.
	ConfiguredHTTPProxy  string `json:"configured_http_proxy,omitempty"`
	ConfiguredHTTPSProxy string `json:"configured_https_proxy,omitempty"`
	ConfiguredKnown      bool   `json:"configured_known,omitempty"`
	// Written is the proxy the agent last wrote to the daemon's systemd
	// drop-in (empty when it wrote none), so a mismatch with the daemon's own
	// report means the daemon has not been restarted since.
	WrittenHTTPProxy  string `json:"written_http_proxy,omitempty"`
	WrittenHTTPSProxy string `json:"written_https_proxy,omitempty"`
	WrittenNoProxy    string `json:"written_no_proxy,omitempty"`
	// LiveRestore is the daemon's live-restore: containers survive a daemon
	// restart.
	LiveRestore bool `json:"live_restore"`
	// DaemonJSONProxy: /etc/docker/daemon.json sets "proxies", which the
	// daemon prefers over the systemd environment the agent writes.
	DaemonJSONProxy bool `json:"daemon_json_proxy"`
	// Systemd: the daemon runs as a systemd unit the agent can configure.
	Systemd bool `json:"systemd"`
	// Privilege is how the agent can change the daemon: "root", "sudo"
	// (passwordless sudo) or "none".
	Privilege string `json:"privilege"`
	// Error is why the agent could not observe or configure the daemon.
	Error string `json:"error,omitempty"`
}

// MaskProxyCredentials spells a proxy URL the way the Docker daemon reports
// it in `docker info`: any user information becomes "xxxxx:xxxxx".
func MaskProxyCredentials(proxy string) string {
	proxy = strings.TrimSpace(proxy)
	parsed, err := url.Parse(proxy)
	if err != nil || parsed.User == nil {
		return proxy
	}
	parsed.User = url.UserPassword("xxxxx", "xxxxx")
	return parsed.String()
}

// Runtime proxy privileges (RuntimeProxyStatus.Privilege).
const (
	RuntimePrivilegeRoot = "root"
	RuntimePrivilegeSudo = "sudo"
	RuntimePrivilegeNone = "none"
)

// RuntimeFallbackDaemonNotReady is the fallback reason for a runtime whose
// CLI and local socket are present but whose daemon did not answer while the
// agent started: dockerd still starting at boot, or too busy (pulling a
// large image) to answer in time. The agent rechecks it every minute and
// switches back once it answers and no instance would be disturbed.
const RuntimeFallbackDaemonNotReady = "daemon_not_ready"

// PeerRelayReady is consumed by the coordinator before forwarding TLS bytes.
type PeerRelayReady struct {
	Error string `json:"error,omitempty"`
}

// PeerRelayWaitClosedParams waits for a target data stream's attachment release.
type PeerRelayWaitClosedParams struct {
	Kind       string `json:"kind"`
	TransferID string `json:"transfer_id"`
}
