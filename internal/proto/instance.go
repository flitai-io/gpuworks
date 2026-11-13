package proto

import "encoding/json"

// LaunchEnvLayers retain provenance for preview/audit while Env is the single
// effective environment consumed by the agent.
type LaunchEnvLayers struct {
	System   map[string]string `json:"system,omitempty"`
	Template map[string]string `json:"template,omitempty"`
	User     map[string]string `json:"user,omitempty"`
}

// RequestedFeatures is the server's capability snapshot of a resolved launch.
// A nil pointer on an old launch means no snapshot was sent. CommandKnown=false
// means that the executable uses an unknown dialect, not that it requests no
// features; declared topology and resource facts remain represented below.
type RequestedFeatures struct {
	CommandKnown     bool `json:"command_known"`
	EngineSleep      bool `json:"engine_sleep,omitempty"`
	MultiNode        bool `json:"multi_node,omitempty"`
	PD               bool `json:"prefill_decode_disaggregation,omitempty"`
	KVDiskTier       bool `json:"kv_disk_tier,omitempty"`
	PipelineParallel bool `json:"pipeline_parallelism,omitempty"`
	DataParallel     bool `json:"data_parallelism,omitempty"`
}

// Names returns the stable capability identifiers represented by the snapshot.
func (f *RequestedFeatures) Names() []string {
	if f == nil {
		return nil
	}
	var out []string
	for _, entry := range []struct {
		name    string
		enabled bool
	}{{"engine_sleep", f.EngineSleep}, {"multi_node", f.MultiNode}, {"prefill_decode_disaggregation", f.PD}, {"kv_disk_tier", f.KVDiskTier}, {"pipeline_parallelism", f.PipelineParallel}, {"data_parallelism", f.DataParallel}} {
		if entry.enabled {
			out = append(out, entry.name)
		}
	}
	return out
}

// ResolvedLaunch is the one fully merged launch truth shared by preview,
// persistence and agent execution. It contains no instance identity or node
// allocation facts; those belong to InstanceDesiredState.
type ResolvedLaunch struct {
	StartupEstimate *StartupEstimate `json:"startup_estimate,omitempty"`
	// These are the resolved health budget, not a startup-time prediction.
	StartTimeoutSeconds       int                `json:"start_timeout_seconds,omitempty"`
	StartTimeoutSource        string             `json:"start_timeout_source,omitempty"`
	StartTimeoutConflict      bool               `json:"start_timeout_conflict,omitempty"`
	EngineStartTimeoutSeconds int                `json:"engine_start_timeout_seconds,omitempty"`
	RequestedFeatures         *RequestedFeatures `json:"requested_features,omitempty"`
	CommandMode               string             `json:"command_mode,omitempty"`
	Entrypoint                []string           `json:"entrypoint,omitempty"`
	Argv                      []string           `json:"argv"`
	PreStartArgv              []string           `json:"pre_start_argv,omitempty"`
	PreStartScript            string             `json:"pre_start_script,omitempty"`
	RuntimeAssets             []RuntimeAsset     `json:"runtime_assets,omitempty"`
	Env                       map[string]string  `json:"env,omitempty"`
	EnvLayers                 LaunchEnvLayers    `json:"env_layers,omitempty"`
	DockerArgs                []string           `json:"docker_args,omitempty"`
	// ShmBytes is the size of the container's /dev/shm tmpfs, not a memory
	// reservation or limit: pages count against the host only once written.
	// It is ineffective under --ipc=host, which shares the host's /dev/shm.
	ShmBytes           int64                  `json:"shm_bytes,omitempty"`
	Mounts             []MountSpec            `json:"mounts,omitempty"`
	ModelMount         string                 `json:"model_mount"`
	HealthPath         string                 `json:"health_path,omitempty"`
	APIContract        json.RawMessage        `json:"api_contract,omitempty"`
	PlacementVariables map[string]interface{} `json:"placement_variables,omitempty"`
}

// InstanceDesiredState is the desired state for one concrete inference instance. The
// embedded launch is resolved once by the server; the agent does not rebuild
// catalog, image, placement or command semantics.
type InstanceDesiredState struct {
	ResolvedLaunch `json:"launch"`

	UUID         string `json:"uuid"`
	DeploymentID uint   `json:"deployment_id"`
	Engine       string `json:"engine,omitempty"`
	SleepAPI     string `json:"sleep_api,omitempty"`
	LogDialect   string `json:"log_dialect,omitempty"`
	// MetricPath, MetricPrefix and MetricMap are how an agent that predates
	// Observation scrapes the instance; the server keeps filling them from the
	// same resolution so both generations of agent see one mapping.
	MetricPath   string            `json:"metric_path,omitempty"`
	MetricPrefix string            `json:"metric_prefix,omitempty"`
	MetricMap    map[string]string `json:"metric_map,omitempty"`
	// Observation is how the platform observes the instance. It is the one
	// part of the desired state the server may change while the instance runs
	// (StateUpdate.Observations): it decides what is collected, never how the
	// container runs.
	Observation  *InstanceObservation `json:"observation,omitempty"`
	DesiredState string               `json:"desired_state"` // running | stopped
	Image        string               `json:"image"`
	ImagePullRef string               `json:"image_pull_ref,omitempty"`
	// ImagePullRefs is the flat order an agent that predates
	// ImagePullCandidates tries; the server derives it from the candidates.
	ImagePullRefs []string `json:"image_pull_refs,omitempty"`
	// ImageSources are the references that name the same image as Image, in
	// the order they are preferred (template sources, catalog sources, Image
	// itself). They are what the launch recorded about the image; the server
	// turns them into ImagePullCandidates for the node each time it sends the
	// instance, so a change to the pull source rules reaches a replica that
	// starts later.
	ImageSources []string `json:"image_sources,omitempty"`
	// ImagePullCandidates is where this node pulls Image from, in order, with
	// the node's pull source rules applied (pullref.PlanCandidates).
	ImagePullCandidates []ImagePullCandidate `json:"image_pull_candidates,omitempty"`
	// Explicit and Proxy are written only by servers that predate pull
	// source rules, for a deployment-scoped source and network choice. A
	// current server never sets them; an agent still honors them for a
	// desired state an older server stored.
	ImagePullExplicit bool   `json:"image_pull_explicit,omitempty"`
	ImagePullProxy    string `json:"image_pull_proxy,omitempty"`
	GPUIndexes        []int  `json:"gpu_indexes"`
	// GPUIdents are the hardware identities (accelerator.CardIdent) of the
	// same cards, in the same order. The agent resolves them to the device
	// indexes the driver gives those cards right now, because an index only
	// numbers the cards the driver could initialize and shifts whenever that
	// set changes. Empty from a server that predates this field, and empty
	// per card for a prober that reports neither a PCI address nor a UUID;
	// the agent then uses GPUIndexes as it always did.
	GPUIdents   []string `json:"gpu_idents,omitempty"`
	HostNetwork bool     `json:"host_network"`
	Ports       []int    `json:"ports"`      // api port first
	ModelPath   string   `json:"model_path"` // host path to mount
	// ServedModelName is the name the launch tells the engine to answer under
	// (--served-model-name), decided by the server; empty when it sets none.
	// The agent checks the engine on the API port answers under it.
	ServedModelName string `json:"served_model_name,omitempty"`
	APIPort         int    `json:"api_port"`
	StartTimeout    int    `json:"start_timeout_seconds"`

	// Distributed process metadata. Headless ranks participate in the engine's
	// worker group but do not expose an HTTP endpoint of their own.
	DistGroup   string `json:"dist_group,omitempty"`
	DistRole    string `json:"dist_role,omitempty"` // leader | follower
	NNodes      int    `json:"nnodes,omitempty"`
	NodeRank    int    `json:"node_rank,omitempty"`
	Headless    bool   `json:"headless,omitempty"`
	DistAttempt int    `json:"dist_attempt,omitempty"`

	// Sleep: vllm sleep-mode reconciliation. DesiredSleep 0=awake, 1=L1
	// (weights offloaded to host RAM, KV cache discarded), 2=L2 (weights and KV
	// cache discarded; waking reloads the weights from the model directory).
	// Sleepable enables the dev-mode endpoints + prewarm. SleepMode is what the
	// engine does with requests it is still serving when told to sleep: "wait"
	// finishes them first, "abort" cuts them off; empty means wait.
	Sleepable    bool   `json:"sleepable,omitempty"`
	DesiredSleep int    `json:"desired_sleep,omitempty"`
	SleepMode    string `json:"sleep_mode,omitempty"`

	// dGPU: per-GPU memory limit for GPU-sharing. 0 = whole GPU.
	VRAMLimitBytes int64 `json:"vram_limit_bytes,omitempty"`
	// MemoryClaimBytes is the memory the instance was admitted with on each of
	// its GPUs when they are shared by memory (an Apple silicon Mac): the
	// scheduler's decision, kept so the ledger charges what was admitted even
	// after the template or the estimate changes. 0 = not shared by memory.
	// Only the server reads it.
	MemoryClaimBytes int64  `json:"memory_claim_bytes,omitempty"`
	SMLimitPct       int    `json:"sm_limit_pct,omitempty"`
	PartitionBackend string `json:"partition_backend,omitempty"`
}

// RuntimeAsset is an immutable, catalog-owned launch input. PackageDigest and
// ModelID are the persisted content reference. Content is populated only in a
// transient authenticated desired-state message when the agent misses its SHA
// cache; it must not be stored in deployment or instance JSON.
type RuntimeAsset struct {
	File          string `json:"file"`
	SHA256        string `json:"sha256"`
	SizeBytes     int64  `json:"size_bytes,omitempty"`
	UnpackTo      string `json:"unpack_to"`
	PackageDigest string `json:"package_digest,omitempty"`
	ModelID       string `json:"model_id,omitempty"`
	Content       []byte `json:"content,omitempty"`
}

type MountSpec struct {
	Source   string `json:"source,omitempty"`
	Volume   string `json:"volume,omitempty"`
	Target   string `json:"target"`
	ReadOnly bool   `json:"read_only,omitempty"`
	SizeHint string `json:"size_hint,omitempty"`
	Purpose  string `json:"purpose,omitempty"`
}

// MountPurposeComputeNetwork marks a read-only host file an instance needs
// to use its compute network (the accelerator cards' port configuration).
const MountPurposeComputeNetwork = "compute_network"

// MountPurposeKVDisk marks the managed mount backing a deployment's KV disk
// offload tier: the server states the container path and the capacity ceiling
// (in SizeHint) and leaves Source empty, and the agent prepares a host
// directory, fills Source in and prunes the directory to that ceiling.
//
// It is the first managed purpose whose directory the container writes to - the
// others publish read-only assets - so the engine process must run as the
// runtime mount owner's uid or as root to use it.
//
// The constant lives here because both sides need the same word and neither may
// import the other; the protocol is their only shared vocabulary.
const MountPurposeKVDisk = "kv-disk"

// PhaseSleep marks a status report about sleep/wake reconciliation. A report in
// this phase with a Message is a failed sleep or wake order; the server records
// it on the instance timeline so the reason is visible without the agent log.
const PhaseSleep = "sleep"

const (
	SleepLevelAwake = 0
	SleepLevelL1    = 1
	SleepLevelL2    = 2
	SleepModeWait   = "wait"
	SleepModeAbort  = "abort"
)

// InstanceState values (agent-reported runtime state).
//
// InstPendingRemoval is server-assigned rather than agent-reported: the
// controller marks an instance for teardown and the reconcile loop treats it as
// no longer schedulable while the agent stops and removes the container.
//
// InstDraining is server-assigned too: the server wants the instance gone once
// it has finished what it holds. It no longer fills its replica (a
// replacement may start beside it) and the gateway sends it nothing new, but
// the agent keeps it running until the controller retires it.
const (
	InstPending        = "pending"
	InstPulling        = "pulling"
	InstStarting       = "starting"
	InstOptimizing     = "optimizing"
	InstRunning        = "running"
	InstDraining       = "draining"
	InstRemoving       = "removing" // agent is still reclaiming a container or its partition state
	InstStopped        = "stopped"
	InstFailed         = "failed"
	InstPendingRemoval = "pending-removal"
)

// InstanceStates returns every instance state value in lifecycle order. It is
// the single enumeration of the set; callers validating or ranging over states
// must use it instead of rebuilding a list.
func InstanceStates() []string {
	return []string{
		InstPending,
		InstPulling,
		InstStarting,
		InstOptimizing,
		InstRunning,
		InstDraining,
		InstRemoving,
		InstStopped,
		InstFailed,
		InstPendingRemoval,
	}
}

// InstanceFinishedStates returns the states of an instance whose generation is
// over: it stopped, failed, or awaits removal by the controller. Such an
// instance no longer holds its image, and an agent report must not move it
// back. It is the one definition of the set for SQL predicates.
func InstanceFinishedStates() []string {
	return []string{InstStopped, InstFailed, InstPendingRemoval}
}

// InstanceFillsReplica reports whether an instance in this state occupies its
// replica: anything not finished (stopped, failed) and not on its way out
// (draining, pending removal, removing). It is the one definition of the set;
// the server's replica matching, sleep orders and the API derive from it.
func InstanceFillsReplica(state string) bool {
	switch state {
	case InstStopped, InstFailed, InstDraining, InstPendingRemoval, InstRemoving:
		return false
	}
	return true
}

// Instance lifecycle phases the agent reports while it works on an instance.
// These are wire values: the server stores them, events carry them and the UI
// renders them. The agent reports them, so they are named here once instead of
// being spelled out at every producer.
const (
	PhaseImageVerify      = "image_verify"
	PhaseImagePull        = "image_pull"
	PhaseImagePullRetry   = "image_pull_retry"
	PhaseImageExtract     = "image_extract"
	PhaseAssetPrepare     = "asset_prepare"
	PhaseContainerUnpack  = "container_unpack"
	PhaseContainerPrepare = "container_prepare"
	PhaseContainerLaunch  = "container_launch"
	PhaseHealthCheck      = "health_check"
)

// InstanceStatus is reported by the agent (status.instance).
type InstanceStatus struct {
	RuntimeFailure *RuntimeFailure `json:"runtime_failure,omitempty"`
	UUID           string          `json:"uuid"`
	State          string          `json:"state"`
	ContainerID    string          `json:"container_id,omitempty"`
	Ports          []int           `json:"ports,omitempty"`
	Message        string          `json:"message,omitempty"`  // error/detail
	Phase          string          `json:"phase,omitempty"`    // image_verify | image_pull | image_extract | container_unpack | container_prepare | container_launch | health_check | optimizing | sleep
	Progress       float64         `json:"progress,omitempty"` // 0..1 when known
	BytesDone      int64           `json:"bytes_done,omitempty"`
	BytesTotal     int64           `json:"bytes_total,omitempty"`
	RateBPS        int64           `json:"rate_bps,omitempty"`
	Measured       json.RawMessage `json:"measured,omitempty"`    // vllm log parse
	DirectAddr     string          `json:"direct_addr,omitempty"` // host:port for direct proxy
	SleepLevel     int             `json:"sleep_level,omitempty"` // 0 awake, 1 L1, 2 L2
	// SleepTarget is populated on a failed sleep/wake report. The server uses
	// it to roll the deployment intent back only when the failed order is still
	// the current intent.
	SleepTarget *int `json:"sleep_target,omitempty"`
	// SleepResidueBytes is the largest per-GPU memory the engine still held on
	// its cards right after a sleep it applied, read when the instance had
	// those cards to itself; 0 when it was not measured. The server's ledger
	// charges it instead of an estimate while the instance sleeps.
	SleepResidueBytes int64 `json:"sleep_residue_bytes,omitempty"`
	// L1HostBufferHeld says the engine completed an L1 sleep since its
	// container started (the agent's startup prewarm or an order). vLLM keeps
	// the pinned host memory an L1 sleep copies the weights into for the life
	// of the process, so a later L1 sleep of this instance takes no host
	// memory it does not already hold. False when not known.
	L1HostBufferHeld bool `json:"l1_host_buffer_held,omitempty"`
	// RuntimeAssetDigests acknowledges assets that were verified and published
	// into the local content-addressed cache before container creation.
	RuntimeAssetDigests []string `json:"runtime_asset_digests,omitempty"`
	// RuntimeObservation is an instance-scoped snapshot of what the runtime
	// actually created. It may be reported without State so observation refresh
	// does not manufacture a lifecycle transition.
	RuntimeObservation *InstanceRuntimeObservation `json:"runtime_observation,omitempty"`
	// OpenAPIPaths is set only when the agent parsed an OpenAPI document from
	// the engine. Nil means the document was not read. An empty slice means the
	// document listed no paths.
	OpenAPIPaths *[]string `json:"openapi_paths,omitempty"`
}

const (
	ObservationObserved = "observed"
	ObservationUnknown  = "unknown"
)

// Observation reasons explain why a runtime observation value is missing. They
// are wire values too: the server keeps the known ones stable across reports
// and the UI localizes them.
const (
	ObservationReasonUnsupported   = "unsupported"
	ObservationReasonNotCollected  = "not_collected"
	ObservationReasonInspectFailed = "inspect_failed"
	ObservationReasonTimeout       = "timeout"
	ObservationReasonPermission    = "permission_denied"
	ObservationReasonNotRunning    = "not_running"
	ObservationReasonNonLocalPID   = "non_local_pid"
)

// InstanceRuntimeObservation keeps container configuration, immutable OCI
// metadata, and live-process facts as separate layers. The first two can be
// known while process facts remain unavailable.
type InstanceRuntimeObservation struct {
	Runtime   string                      `json:"runtime"`
	Container RuntimeContainerObservation `json:"container"`
	Image     RuntimeImageObservation     `json:"image"`
	Process   RuntimeProcessObservation   `json:"process"`
}

type RuntimeContainerObservation struct {
	Status           string                    `json:"status"`
	Reason           string                    `json:"reason,omitempty"`
	ObservedAtUnixMS int64                     `json:"observed_at_unix_ms,omitempty"`
	ContainerID      string                    `json:"container_id,omitempty"`
	ImageID          string                    `json:"image_id,omitempty"`
	Entrypoint       []string                  `json:"entrypoint"`
	Cmd              []string                  `json:"cmd"`
	Path             string                    `json:"path,omitempty"`
	Args             []string                  `json:"args"`
	Env              map[string]string         `json:"env"`
	Mounts           []RuntimeMountObservation `json:"mounts"`
	Devices          []string                  `json:"devices"`
	RuntimeName      string                    `json:"runtime_name,omitempty"`
	NetworkMode      string                    `json:"network_mode,omitempty"`
}

type RuntimeMountObservation struct {
	Type        string `json:"type,omitempty"`
	Source      string `json:"source,omitempty"`
	Destination string `json:"destination"`
	ReadOnly    bool   `json:"read_only,omitempty"`
}

type RuntimeImageObservation struct {
	Status           string            `json:"status"`
	Reason           string            `json:"reason,omitempty"`
	ObservedAtUnixMS int64             `json:"observed_at_unix_ms,omitempty"`
	ImageID          string            `json:"image_id,omitempty"`
	Entrypoint       []string          `json:"entrypoint"`
	Cmd              []string          `json:"cmd"`
	Env              map[string]string `json:"env"`
	Volumes          []string          `json:"volumes"`
}

type RuntimeProcessObservation struct {
	Status           string            `json:"status"`
	Reason           string            `json:"reason,omitempty"`
	ObservedAtUnixMS int64             `json:"observed_at_unix_ms,omitempty"`
	PID              int               `json:"pid,omitempty"`
	Identity         string            `json:"identity,omitempty"`
	Argv             []string          `json:"argv,omitempty"`
	Env              map[string]string `json:"env,omitempty"`
}

// InstanceObservation is the resolved observation of one instance (ADR 0039):
// where its engine's Prometheus endpoint is and how its families map onto the
// platform's inference metrics. The server resolves it from the engine's
// built-in mapping and the template's api.metrics; the agent only applies it.
type InstanceObservation struct {
	// MetricPath is the engine's metrics endpoint on its API port.
	MetricPath string `json:"metric_path,omitempty"`
	// MetricFormat is "json" for an endpoint that answers one JSON document,
	// whose rule sources are dotted key paths; empty is Prometheus text.
	MetricFormat string `json:"metric_format,omitempty"`
	// MetricRules is the mapping. When it is empty, MetricDetect offers
	// mappings chosen by the family prefix the scrape actually shows.
	MetricRules  []MetricRule    `json:"metric_rules,omitempty"`
	MetricDetect []MetricRuleSet `json:"metric_detect,omitempty"`
}

// MetricRule maps one exposition family (optionally `name{label="v"}`) to a
// stored platform series. Scale multiplies a counter or gauge; zero means one.
type MetricRule struct {
	Source string  `json:"source"`
	Series string  `json:"series"`
	Scale  float64 `json:"scale,omitempty"`
}

// MetricRuleSet is a mapping that applies when a scrape has a family named
// Prefix+":...".
type MetricRuleSet struct {
	Prefix string       `json:"prefix"`
	Rules  []MetricRule `json:"rules"`
}

// ImagePullCandidate is one reference a node tries for an image. The node may
// reorder a run of adjacent Rank candidates by how well it reaches their
// registries; candidates without Rank keep their place (they are endpoints of
// the operator's pull source rules, in the operator's order).
type ImagePullCandidate struct {
	Ref  string `json:"ref"`
	Rank bool   `json:"rank,omitempty"`
}
