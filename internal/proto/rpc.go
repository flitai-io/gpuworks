package proto

// RPC method names (short request/response actions).
const (
	RPCNodeDoctor = "node.doctor"
	// RPCNodeConnectionDiagnostics reads the receiving connection's own state.
	// Keep this distinct from doctor: old agents must refuse it, not run hardware checks.
	RPCNodeConnectionDiagnostics = "node.connection_diagnostics"
	RPCNodeSetRuntime            = "node.set_runtime"
	// RPCNodeSetAccelerator chooses which vendor's cards the node manages
	// ({"accelerator": "auto" | vendor}). The agent refuses while anything
	// runs on the node, and otherwise restarts itself to manage them.
	RPCNodeSetAccelerator  = "node.set_accelerator"
	RPCFSList              = "fs.list"
	RPCFSReadFile          = "fs.read_file"
	RPCFSStat              = "fs.stat"
	RPCNetProbe            = "net.probe"
	RPCNetBenchStart       = "net.bench.start"
	RPCNetBenchStop        = "net.bench.stop"
	RPCRDMABenchServe      = "rdma.bench.serve"
	RPCRDMABenchRun        = "rdma.bench.run"
	RPCAcceleratorPing     = "accelerator.ping"
	RPCRegisterPath        = "model.register_path"
	RPCModelEvict          = "model.evict"
	RPCCacheEvict          = "model.cache.evict"
	RPCModelCopyStop       = "model.copy.stop"
	RPCModelDownloadStop   = "model.download.stop"
	RPCModelSyncListen     = "model.sync.listen"
	RPCModelSyncSend       = "model.sync.send"
	RPCModelSyncStop       = "model.sync.stop"
	RPCModelSyncProbe      = "model.sync.probe"
	RPCPeerRelayWaitClosed = "peer.relay.wait_closed"
	RPCImageSyncListen     = "image.sync.listen"
	RPCImageSyncSend       = "image.sync.send"
	RPCImageSyncStop       = "image.sync.stop"
	RPCImageSyncProbe      = "image.sync.probe"
	RPCImagePull           = "image.pull"
	RPCImageLoadURL        = "image.load_url"
	RPCImageCancel         = "image.cancel"
	RPCImageRemove         = "image.remove"
	RPCImageInspect        = "image.inspect"
	// RPCRuntimeRestart restarts the node's container runtime daemon so a
	// changed proxy takes effect. Only dockerd under systemd supports it.
	RPCRuntimeRestart   = "runtime.restart"
	RPCImageExists      = "image.exists"
	RPCBenchProbe       = "bench.probe"
	RPCBenchCancel      = "bench.cancel"
	RPCPartitionReprobe = "partition.reprobe"
	RPCAgentUpgrade     = "agent.upgrade"
	// RPCKubeNodeExists asks a Kubernetes node's agent whether another node
	// of its cluster still exists there (KubeNodeExistsParams).
	RPCKubeNodeExists = "kube.node.exists"
)

// KubeNodeExistsParams is RPCKubeNodeExists's request.
type KubeNodeExistsParams struct {
	NodeName string `json:"node_name"`
}

// KubeNodeExistsResult is RPCKubeNodeExists's answer.
type KubeNodeExistsResult struct {
	Exists bool `json:"exists"`
}

type ImagePullParams struct {
	EffectiveRef string `json:"effective_ref"`
	CanonicalRef string `json:"canonical_ref"`
	// TransferID names this attempt, so a cancel or a late status belongs to the
	// transfer that is actually running rather than to whichever one shares the
	// reference.
	TransferID string `json:"transfer_id,omitempty"`
	// Candidates, when set, is the whole ordered list the agent tries itself
	// (falling back, ranking and cooling down exactly as for an instance's
	// image); EffectiveRef is then the first of them. Only an agent that
	// advertises CapImagePullCandidates receives it.
	Candidates []ImagePullCandidate `json:"candidates,omitempty"`
}

// RuntimeRestartResult reports a runtime.restart.
type RuntimeRestartResult struct {
	// Proxy is the runtime's proxy as observed after the restart.
	Proxy *RuntimeProxyStatus `json:"proxy,omitempty"`
}

// FSStatParams asks whether one path exists and what it is. It is the cheap
// half of adopting a node-local file: the caller learns a candidate is a
// regular file without transferring its bytes.
type FSStatParams struct {
	Path string `json:"path"`
}

// FSStatResult is the answer to FSStatParams. Regular is what callers gate on:
// a directory, a device or a symlink to either is not a file they can adopt.
type FSStatResult struct {
	Path      string `json:"path"`
	SizeBytes int64  `json:"size_bytes"`
	Regular   bool   `json:"regular"`
	Dir       bool   `json:"dir"`
}

type ImagePullResult struct {
	State        string `json:"state"`
	EffectiveRef string `json:"effective_ref"`
	CanonicalRef string `json:"canonical_ref"`
}

// ImageLoadURLParams asks an agent to download an image archive published
// over plain HTTP and load it under CanonicalRef. SizeBytes is what the server
// learned from a HEAD request; it is advisory (zero when unknown) and only
// drives the node's disk admission and progress reporting.
type ImageLoadURLParams struct {
	CanonicalRef string `json:"canonical_ref"`
	ArchiveURL   string `json:"archive_url"`
	SizeBytes    int64  `json:"size_bytes,omitempty"`
	RateLimitKBs int    `json:"rate_limit_kbs,omitempty"`
	// TransferID names this attempt; see ImagePullParams.
	TransferID string `json:"transfer_id,omitempty"`
}

// ImageCancelParams asks the node to stop one image operation. The transfer id
// is what makes that exact: the same image may have a stuck attempt and a
// queued retry at the same time.
type ImageCancelParams struct {
	CanonicalRef string `json:"canonical_ref"`
	TransferID   string `json:"transfer_id,omitempty"`
}

// ImageCancelResult reports whether anything was still running. Cancelling an
// operation that already finished is not an error: it is a no-op.
type ImageCancelResult struct {
	State     string `json:"state"`
	Cancelled bool   `json:"cancelled"`
}

// ImageRemoveParams asks a node to delete a local image, so a node that no
// longer needs it (or holds a copy an operator does not trust) can give the
// disk back.
type ImageRemoveParams struct {
	ImageRef string `json:"image_ref"`
}

// ImageRemoveResult reports the reference that is gone and the state the
// ledger row ends in.
type ImageRemoveResult struct {
	State    string `json:"state"`
	ImageRef string `json:"image_ref"`
}

// ImageLoadURLResult acknowledges the request. The download and load run in
// the background and report through status.image_node, exactly like image.pull.
type ImageLoadURLResult struct {
	State        string `json:"state"`
	CanonicalRef string `json:"canonical_ref"`
}

type ImageInspectParams struct {
	ImageRef string `json:"image_ref"`
}
type ImageInspectResult struct {
	ImageRef   string            `json:"image_ref"`
	ImageID    string            `json:"image_id,omitempty"`
	Entrypoint []string          `json:"entrypoint,omitempty"`
	Cmd        []string          `json:"cmd,omitempty"`
	Env        map[string]string `json:"env,omitempty"`
	Volumes    []string          `json:"volumes,omitempty"`
}

type ImageSyncListenParams struct {
	TransferID string `json:"transfer_id"`
	ImageRef   string `json:"image_ref"`
	Token      string `json:"token"`
	// BindIP is the target's address on the network the server chose for
	// this copy; the receiver listens on it and advertises only it.
	BindIP        string `json:"bind_ip,omitempty"`
	ExpectedBytes int64  `json:"expected_bytes,omitempty"`
}

type ImageSyncListenResult struct {
	Endpoints   []string `json:"endpoints,omitempty"`
	Fingerprint string   `json:"fingerprint,omitempty"`
	InProgress  bool     `json:"in_progress,omitempty"`
	TransferID  string   `json:"transfer_id,omitempty"`
}

type ImageSyncSendParams struct {
	ReportPhase bool `json:"report_phase,omitempty"`
	// AllowRelay permits negotiation only when TCP dialing fails before TLS.
	AllowRelay   bool     `json:"allow_relay,omitempty"`
	TransferID   string   `json:"transfer_id"`
	ImageRef     string   `json:"image_ref"`
	Token        string   `json:"token"`
	Endpoints    []string `json:"endpoints"`
	Fingerprint  string   `json:"fingerprint"`
	RateLimitKBs int      `json:"rate_limit_kib_s,omitempty"`
	// SourceIP is the source's address on the chosen network; the sender
	// dials from it.
	SourceIP string `json:"source_ip,omitempty"`
}

type ImageSyncStopParams struct {
	TransferID string `json:"transfer_id"`
}

type ImageSyncTransferResult struct {
	// DirectDialError is an opt-in negotiation result for pre-TLS TCP failure.
	DirectDialError string `json:"direct_dial_error,omitempty"`
	Bytes           int64  `json:"bytes"`
	SHA256          string `json:"sha256"`
}

type ImageSyncCopyRequest struct {
	AllowRelay     bool   `json:"allow_relay,omitempty"`
	TransferID     string `json:"transfer_id"`
	ImageRef       string `json:"image_ref"`
	SourceNodeUUID string `json:"source_node_uuid"`
	TargetNodeUUID string `json:"target_node_uuid"`
	// SourceIP and TargetIP are each end's address on the network chosen for
	// this copy (network.Service.StoragePath).
	SourceIP      string `json:"source_ip,omitempty"`
	TargetIP      string `json:"target_ip,omitempty"`
	ExpectedBytes int64  `json:"expected_bytes,omitempty"`
	RateLimitKBs  int    `json:"rate_limit_kib_s,omitempty"`
}

type ImageSyncCopyResult struct {
	Transport  string `json:"transport,omitempty"`
	TransferID string `json:"transfer_id"`
	InProgress bool   `json:"in_progress,omitempty"`
	Bytes      int64  `json:"bytes,omitempty"`
	SHA256     string `json:"sha256,omitempty"`
}
