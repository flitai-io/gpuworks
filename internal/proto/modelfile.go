package proto

// ModelFileTask is the desired presence of a model on this node.
type ModelFileTask struct {
	Key       string `json:"key"` // stable id (model_id-node)
	ModelID   uint   `json:"model_id"`
	Source    string `json:"source"`    // huggingface | modelscope | url
	SourceID  string `json:"source_id"` // repo id, e.g. Qwen/Qwen3-8B
	Revision  string `json:"revision"`
	Subfolder string `json:"subfolder,omitempty"`
	// Path and RelPath say where the model lives, and exactly one of them is
	// set. Path is an absolute directory on the node's host (a node's own row:
	// a local storage, or a node cache). RelPath is relative to the task's
	// storage root on that node (a shared storage's row, or a row not yet
	// written), and must name something below it.
	Path    string `json:"path,omitempty"`
	RelPath string `json:"rel_path,omitempty"`
	// TargetPath is Path or RelPath under the name servers and agents used
	// before the two were split, and in the same either-or form: an agent
	// told it apart by whether it was absolute. The server still fills it so
	// an agent that predates Path/RelPath keeps working, and an agent still
	// reads it when neither new field is set (a server that predates them).
	// Remove it, with agent/app.legacyTaskTarget, once no agent or server
	// older than the release that introduced Path/RelPath is supported.
	TargetPath   string `json:"target_path"`
	DesiredState string `json:"desired_state"`  // present | absent
	RateLimitKBs int    `json:"rate_limit_kbs"` // 0 = unlimited
	// Candidates are the addresses a download is fetched from, in the order
	// the pull source rules give (planning.PlanModelFetch): rule endpoints
	// first, the source's own site last unless a rule forbids it. An agent
	// walks them itself and no longer applies NetworkConfig.Rewrites. A
	// server that predates them sends none; the agent then falls back to
	// Endpoint and the rewrites.
	Candidates []ModelFetchCandidate `json:"candidates,omitempty"`
	// Endpoint is the first candidate (for an HTTP directory, the directory),
	// for agents that predate Candidates. Every download goes through the
	// node's proxy (NetworkConfig); a task names none of its own.
	Endpoint    string `json:"endpoint,omitempty"`
	Acquisition string `json:"acquisition,omitempty"`
	TransferID  string `json:"transfer_id,omitempty"`
	// StorageID names the storage RelPath is relative to. Zero means the
	// agent's models root (servers that predate storages).
	StorageID uint `json:"storage_id,omitempty"`
	// Cache marks a task that fills the node's cache directory of an nfs_cache
	// storage rather than writing to the storage itself.
	Cache bool `json:"cache,omitempty"`
	// SourcePath is the absolute directory on this node a copy reads from
	// (acquisition copy).
	SourcePath string `json:"source_path,omitempty"`
	// Staging is the storage-relative directory a write to a shared storage is
	// assembled in before it is renamed into place.
	Staging string `json:"staging,omitempty"`
}

// ModelFetchCandidate is one address a model download may be fetched from.
// Base replaces the source's site in every request URL; Upstream marks the
// site itself rather than an address a rule offered in its place.
type ModelFetchCandidate struct {
	Base     string `json:"base"`
	Upstream bool   `json:"upstream,omitempty"`
}

// ModelFileState values.
const (
	MFPending     = "pending"
	MFDownloading = "downloading"
	MFCanceled    = "canceled"
	MFSyncing     = "syncing"
	MFPresent     = "present"
	MFRegistered  = "registered"
	MFFailed      = "failed"
	// MFDeleting is a cache removal whose durable agent acknowledgement is pending.
	MFDeleting         = "deleting"
	MFInsufficientDisk = "insufficient_disk_space"
	MFDownloadRequired = "download_required"
	MFAbsent           = "absent"
	// MFMissing is a shared model file that a rescan no longer finds.
	MFMissing = "missing"
)

// ModelFileStateReady reports whether a model file in this state holds the
// complete model: downloaded, copied or synced (present), or adopted where it
// already was (registered).
func ModelFileStateReady(state string) bool {
	return state == MFPresent || state == MFRegistered
}

// ModelFileStatus is reported by the agent (status.modelfile).
type ModelFileStatus struct {
	Preparation       *ModelPreparation `json:"preparation,omitempty"`
	Key               string            `json:"key"`
	State             string            `json:"state"`
	Revision          string            `json:"revision,omitempty"` // resolved commit/revision used by this download
	BytesTotal        int64             `json:"bytes_total"`
	BytesDone         int64             `json:"bytes_done"`
	TransferBytesDone int64             `json:"transfer_bytes_done,omitempty"`
	RateBytesPerS     int64             `json:"rate_bytes_per_s"`
	Path              string            `json:"path,omitempty"`
	Message           string            `json:"message,omitempty"`
	TransferID        string            `json:"transfer_id,omitempty"`
	Acquisition       string            `json:"acquisition,omitempty"`
}

// ModelPreparation is a transient observation of a same-node copy. Attempt
// identifies one source snapshot; neither a phase nor 100% makes files ready.
type ModelPreparation struct {
	RateStable  bool   `json:"rate_stable,omitempty"`
	Attempt     string `json:"attempt"`
	Phase       string `json:"phase"`
	TotalKnown  bool   `json:"total_known"`
	IdleSeconds int64  `json:"idle_seconds"`
	Stalled     bool   `json:"stalled,omitempty"`
}

const (
	ModelPhaseChecking   = "checking"
	ModelPhaseCopying    = "copying"
	ModelPhaseVerifying  = "verifying"
	ModelPhaseInstalling = "installing"
)

const (
	ModelAcquireAuto       = "auto"
	ModelAcquirePeer       = "peer"
	ModelAcquireDownload   = "download"
	ModelAcquireRegistered = "registered"
	// ModelAcquireImported is a shared model file recorded by a storage scan.
	ModelAcquireImported = "imported"
	// ModelAcquireCopy copies an existing directory on the same node
	// (ModelFileTask.SourcePath), e.g. filling an nfs_cache node cache.
	ModelAcquireCopy = "copy"
)

type ModelSyncListenParams struct {
	TransferID string `json:"transfer_id"`
	Key        string `json:"key"`
	TargetPath string `json:"target_path"`
	Token      string `json:"token"`
	// BindIP is the target's address on the network the server chose for
	// this copy; the receiver listens on it and advertises only it.
	BindIP            string `json:"bind_ip,omitempty"`
	ExpectedFileBytes int64  `json:"expected_file_bytes,omitempty"`
	// StorageID names the storage TargetPath is relative to; zero is the
	// agent's models root.
	StorageID uint `json:"storage_id,omitempty"`
}

type ModelSyncListenResult struct {
	Endpoints   []string `json:"endpoints,omitempty"`
	Fingerprint string   `json:"fingerprint,omitempty"`
	InProgress  bool     `json:"in_progress,omitempty"`
	TransferID  string   `json:"transfer_id,omitempty"`
}

type ModelSyncSendParams struct {
	ReportPhase bool `json:"report_phase,omitempty"`
	// AllowRelay permits negotiation only when TCP dialing fails before TLS.
	AllowRelay   bool     `json:"allow_relay,omitempty"`
	TransferID   string   `json:"transfer_id"`
	SourcePath   string   `json:"source_path"`
	Token        string   `json:"token"`
	Endpoints    []string `json:"endpoints"`
	Fingerprint  string   `json:"fingerprint"`
	RateLimitKBs int      `json:"rate_limit_kib_s,omitempty"`
	// SourceIP is the source's address on the chosen network; the sender
	// dials from it.
	SourceIP string `json:"source_ip,omitempty"`
}

type ModelSyncStopParams struct {
	TransferID string `json:"transfer_id"`
	Key        string `json:"key,omitempty"`
}

type ModelSyncTransferResult struct {
	// DirectDialError means every TCP candidate failed before the archive began.
	// It is emitted only for a caller that explicitly set AllowRelay.
	DirectDialError string `json:"direct_dial_error,omitempty"`
	Bytes           int64  `json:"bytes"`
	SHA256          string `json:"sha256"`
}

// ModelSyncCopyRequest is the server-side coordination contract. Only control
// metadata uses ctrl; archives use direct TLS or an authorized data-lane relay.
type ModelSyncCopyRequest struct {
	AllowRelay     bool   `json:"allow_relay,omitempty"`
	TransferID     string `json:"transfer_id"`
	Key            string `json:"key"`
	SourceNodeUUID string `json:"source_node_uuid"`
	SourcePath     string `json:"source_path"`
	TargetNodeUUID string `json:"target_node_uuid"`
	TargetPath     string `json:"target_path"`
	// TargetStorageID names the storage TargetPath is relative to on the target.
	TargetStorageID uint `json:"target_storage_id,omitempty"`
	// SourceIP and TargetIP are each end's address on the network chosen for
	// this copy (network.Service.StoragePath).
	SourceIP          string `json:"source_ip,omitempty"`
	TargetIP          string `json:"target_ip,omitempty"`
	ExpectedFileBytes int64  `json:"expected_file_bytes,omitempty"`
	RateLimitKBs      int    `json:"rate_limit_kib_s,omitempty"`
}

type ModelSyncCopyResult struct {
	Transport  string `json:"transport,omitempty"`
	TransferID string `json:"transfer_id"`
	InProgress bool   `json:"in_progress,omitempty"`
	Bytes      int64  `json:"bytes,omitempty"`
	SHA256     string `json:"sha256,omitempty"`
}

type ModelCopyStopParams struct {
	Key        string `json:"key"`
	TransferID string `json:"transfer_id"`
}
type ModelCopyStopResult struct {
	Stopped bool `json:"stopped"`
}
