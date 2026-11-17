package proto

// Desired-state protocol for deployments/instances and model files.
//
// Control flow: on connect the server sends state.sync (full desired state; the
// agent clears local caches and reconciles). Thereafter state.update carries
// incremental changes (each object is a complete, idempotent body). The agent
// reconciles local reality (docker containers, download dirs) and reports back
// with status.instance / status.modelfile.

// Status envelope types reported by the agent.
const (
	TStatusModelFile = "status.modelfile"  // agent→server model download progress/state
	TStatusImageNode = "status.image_node" // agent→server image sync progress/state
)

// AgentUpgradeStatus is sent asynchronously because the initial
// agent.upgrade RPC intentionally returns before the download/smoke/exec
// sequence starts. Success is confirmed by a fresh handshake carrying the
// new version; failures use this event so the server does not rely on a
// duplicate rpc.resp with an already-released request ID. It is also the
// agent's one-line reply on a StreamAgentUpgrade stream ("verified" or
// "failed").
type AgentUpgradeStatus struct {
	State   string `json:"state"` // failed | verified
	Version string `json:"version,omitempty"`
	Error   string `json:"error,omitempty"`
	// Code is the RPCError code of a coded failure (ErrorAgentUpgradeInProgress).
	Code string `json:"code,omitempty"`
}

// DesiredState is the payload of a state.sync (full) message.
type DesiredState struct {
	Instances  []InstanceDesiredState `json:"instances"`
	ModelFiles []ModelFileTask        `json:"model_files"`
	Benchmarks []BenchmarkSpec        `json:"benchmarks,omitempty"`
	Network    NetworkConfig          `json:"network"`
	// RegistryAuth is the login for each private registry the node may pull
	// from. The agent keeps it in memory and hands it to a pull only for the
	// duration of that pull.
	RegistryAuth      []SourceAuth `json:"registry_auth,omitempty"`
	ImageRefs         []string     `json:"image_refs,omitempty"`
	ModelsRoot        string       `json:"models_root,omitempty"`
	RuntimePreference string       `json:"runtime_preference,omitempty"`
	// AcceleratorPreference is the operator's choice of which vendor's
	// cards the node manages (AcceleratorPreferenceAuto or a vendor), empty
	// when none was ever made. The agent applies a differing one only when
	// nothing runs on the node.
	AcceleratorPreference string `json:"accelerator_preference,omitempty"`
	// Storages is every storage the node should have usable. It stays on the
	// wire when empty: nil after decoding means a server that predates storages.
	Storages []StorageTask `json:"storages"`
	// OrphanCleanup enables the agent's startup sweep that removes
	// gpuworks-named containers absent from this desired state. The sweep is
	// disabled by default: name-prefix ownership is not cluster-scoped, so a
	// second gpuworks agent sharing the same container runtime (a test agent,
	// a leftover binary) would otherwise destroy this control plane's
	// instances. Operators opt in per cluster via the server setting.
	OrphanCleanup bool `json:"orphan_cleanup,omitempty"`
}

// StateUpdate is the payload of a state.update (incremental) message. Each list
// carries complete object bodies; *Removed carry UUIDs/keys to delete.
type StateUpdate struct {
	// Network replaces network policy only. Nil leaves it unchanged; an empty
	// object clears it. Send only to agents advertising CapNetworkUpdate.
	Network           *NetworkConfig         `json:"network,omitempty"`
	Instances         []InstanceDesiredState `json:"instances,omitempty"`
	RemovedInstances  []string               `json:"removed_instances,omitempty"`
	ModelFiles        []ModelFileTask        `json:"model_files,omitempty"`
	RemovedModelFiles []string               `json:"removed_model_files,omitempty"`
	Benchmarks        []BenchmarkSpec        `json:"benchmarks,omitempty"`
	RemovedBenchmarks []uint                 `json:"removed_benchmarks,omitempty"`
	// Keep image_refs on the wire even when it is an empty, non-nil slice: an
	// empty catalog update must clear the agent's previous observation set.
	ImageRefs  []string `json:"image_refs"`
	ModelsRoot string   `json:"models_root,omitempty"`
	// Observations replace how running instances are observed, in place: the
	// agent swaps the observation of an instance it already holds and leaves
	// its container, health and state alone; an instance it does not hold is
	// ignored (its stored desired state already carries the new observation
	// for the next launch or state.sync). An older agent ignores the field.
	Observations []ObservationUpdate `json:"observations,omitempty"`
}

// ObservationUpdate is one instance's new observation together with the
// legacy fields an agent that predates Observation reads.
type ObservationUpdate struct {
	UUID         string               `json:"uuid"`
	Observation  *InstanceObservation `json:"observation,omitempty"`
	MetricPath   string               `json:"metric_path,omitempty"`
	MetricPrefix string               `json:"metric_prefix,omitempty"`
	MetricMap    map[string]string    `json:"metric_map,omitempty"`
}

// SourceAuth is the login for one host the node fetches from: a registry
// (user name and password, written into a pull's docker auth) or a model site
// (Username empty: a token sent as a Bearer header). Secret is a password or
// token and must never be logged. The wire keeps the name it had when only
// registries had logins.
type SourceAuth struct {
	Host     string `json:"registry"`
	Username string `json:"username"`
	Secret   string `json:"secret"`
}
