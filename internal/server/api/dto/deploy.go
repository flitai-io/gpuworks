package dto

import (
	"time"

	"gpuworks.ai/gpuworks/internal/server/inferenceapi"
	"gpuworks.ai/gpuworks/pkg/localization"
)

// Model is the API view of a model.
type Model struct {
	ID       uint   `json:"id"`
	Source   string `json:"source"`
	SourceID string `json:"source_id"`
	// SourceIDEditable is set when PATCH /models/{id} may change SourceID: a
	// local or URL model, whose source id is a label chosen at registration.
	SourceIDEditable bool `json:"source_id_editable,omitempty"`
	// SourceURL is the HTTP directory a Source="url" model is served from. It
	// is absent for every other source, which has no directory of its own.
	SourceURL         string                   `json:"source_url,omitempty"`
	Revision          string                   `json:"revision"`
	ArtifactSubfolder string                   `json:"artifact_subfolder,omitempty"`
	Name              string                   `json:"name"`
	Arch              string                   `json:"arch"`
	QuantLabel        string                   `json:"quant_label"`
	WeightGiB         float64                  `json:"weight_gib"`
	Operations        []inferenceapi.Operation `json:"operations"`
	InputModalities   []string                 `json:"input_modalities"`
	README            string                   `json:"readme,omitempty"`
	LogoPath          string                   `json:"logo_path,omitempty"`
	OriginCreatedAt   *time.Time               `json:"origin_created_at,omitempty"`
	OriginUpdatedAt   *time.Time               `json:"origin_updated_at,omitempty"`
	ContextLength     int64                    `json:"context_length,omitempty"`
	Precision         string                   `json:"precision,omitempty"`
	CreatedAt         time.Time                `json:"created_at"`
	// Presence per node (model_files aggregation).
	Nodes []ModelNodePresence `json:"nodes,omitempty"`
	// Storages lists the model's copies in shared storages.
	Storages []ModelStoragePresence `json:"storages,omitempty"`
	// MetadataNotice is set on a registration's answer only, when the
	// management node read no metadata: it could not reach any address the
	// rules give it (MODEL_METADATA_UNREACHABLE), or the rules forbid it
	// every address (MODEL_METADATA_FORBIDDEN). The model is registered
	// either way.
	MetadataNotice *ProductMessage `json:"metadata_notice,omitempty"`
}

// Deployment is the API view of a deployment.
type Deployment struct {
	Operations               []DeploymentOperation `json:"operations"`
	ID                       uint                  `json:"id"`
	TemplateID               uint                  `json:"template_id,omitempty"`
	TemplateSpecSource       string                `json:"template_spec_source,omitempty"`
	TemplateSpecName         string                `json:"template_spec_name,omitempty"`
	TemplateScenario         string                `json:"template_scenario,omitempty"`
	TemplateDeviated         bool                  `json:"template_deviated"`
	CatalogSelectionSnapshot any                   `json:"catalog_selection_snapshot,omitempty"`
	MatchedBy                string                `json:"matched_by,omitempty"`
	Name                     string                `json:"name"`
	Engine                   string                `json:"engine"`
	Image                    string                `json:"image,omitempty"`
	ModelID                  uint                  `json:"model_id"`
	ModelName                string                `json:"model_name"`
	// NodeUUID/NodeName/NodeUUIDs/NodeNames are the placement target - what was
	// asked for, not where the replicas are. Read RuntimeNodes for the location.
	NodeUUID string `json:"node_uuid"`
	// NodeName is what a person calls the node. The uuid stays authoritative
	// for addressing; this exists so clients never have to show a raw uuid.
	NodeName  string   `json:"node_name,omitempty"`
	NodeUUIDs []string `json:"node_uuids,omitempty"`
	// NodeNames name NodeUUIDs, rank by rank.
	NodeNames []string `json:"node_names,omitempty"`
	// RuntimeNodes are the nodes the instances are on now, with how many each
	// holds, from the instance rows the GPU page attributes cards from.
	RuntimeNodes []DeploymentRuntimeNode `json:"runtime_nodes,omitempty"`
	// RuntimeNodeNames name RuntimeNodes in the same order.
	RuntimeNodeNames []string `json:"runtime_node_names,omitempty"`
	// NodeRequirements is where the deployment may run, as DeployRequest
	// states it: always present, {} when it names nothing. A deployment
	// pinned to nodes names them under gpuworks.ai/node.
	NodeRequirements map[string][]string `json:"node_requirements"`
	// NodeRequirementLabels show each value of NodeRequirements the way the
	// node pages show that label, with how many nodes carry it now; a value
	// no node carries (a deleted node or network) has nodes 0. Only a single
	// deployment's read carries them.
	NodeRequirementLabels []RequirementLabel `json:"node_requirement_labels,omitempty"`
	// PlacementStrategy is spread or binpack.
	PlacementStrategy string `json:"placement_strategy"`
	ServedName        string `json:"served_name"`
	// QuickParams is the deployment's complete launch configuration
	// (deploy.LaunchConfig) and ExtraArgs the arguments the parameter dictionary
	// does not describe. They are what the instance is started from, so read
	// APIs expose them.
	QuickParams  map[string]any `json:"quick_params,omitempty"`
	ExtraArgs    string         `json:"extra_args,omitempty"`
	DesiredState string         `json:"desired_state"`
	RuntimeState string         `json:"runtime_state"`
	// State is the one state to show for the deployment: RuntimeState, except
	// that a deployment the gateway refuses every request of because the
	// replicas it needs sleep reads asleep (awake again once it is woken), and
	// one whose sleep order is still being carried out reads sleeping.
	// RuntimeState keeps what the runtime reports, so a sleeping deployment is
	// still "running" there: the engine runs, the gateway refuses the request.
	//
	// The judgement is the gateway's own (internal/server/proxy/route_select.go
	// and pd_route.go): a replica is asleep when planning.InstanceAsleep says so,
	// and a deployment is asleep only when every role the gateway needs has
	// none awake. A partly slept deployment therefore keeps its running state
	// and says how many of its replicas sleep in AsleepReplicas.
	State string `json:"state"`
	// AsleepReplicas counts the replicas the gateway refuses because they
	// sleep, over every role: with ReadyReplicas it accounts for the replicas
	// the gateway reaches at all. Against Replicas it says how much of the
	// deployment sleeps, so a partly slept one shows its running replicas
	// instead of reading as a broken replica.
	AsleepReplicas int `json:"asleep_replicas,omitempty"`
	// SleepLevel is the deepest sleep level an engine of the deployment
	// reports: 0 while every replica is awake, and while a sleep order is
	// still being carried out (the intent is there, the engine has not slept
	// yet).
	SleepLevel       int    `json:"sleep_level,omitempty"`
	Sleepable        bool   `json:"sleepable"`
	PartitionBackend string `json:"partition_backend,omitempty"`
	// VRAMLimitBytes is the dGPU memory limit each replica runs under per
	// card; absent for a deployment on whole cards. A change replaces the
	// instances, so an instance's own vram_limit_bytes may trail it.
	VRAMLimitBytes int64 `json:"vram_limit_bytes,omitempty"`
	// Replicas is how many replicas the deployment has, and it is the
	// denominator of the sleep count: AsleepReplicas never exceeds it. A PD
	// deployment has one replica per role unit, so this totals the roles the
	// way the list row does rather than echoing the stored field, which PD
	// creation forces to 1 and which would print "2/1 replicas".
	Replicas int `json:"replicas"`
	// MaxReplicas is how many replicas the deployment may be scaled to.
	MaxReplicas   int    `json:"max_replicas"`
	RoutingPolicy string `json:"routing_policy"`
	// StartTimeoutSec is the deployment's own instance health budget; absent
	// when it states none and the launch resolves an automatic budget.
	StartTimeoutSec int `json:"start_timeout_sec,omitempty"`
	// ReadyReplicas counts the replicas the gateway would serve a request from
	// now: running, on an online node, every rank of a multi-node one up, and
	// awake. A PD deployment counts the same replicas the list row does, one
	// per role, so it totals pd.roles[].ready rather than omitting the field:
	// the drawer prints it beside AsleepReplicas, and a missing half of that
	// pair left the sleep count without the count it adds to.
	//
	// ReadyReplicas and AsleepReplicas are two counts of one set of replicas,
	// judged the way the gateway judges them, so they never contradict each
	// other; what they do not count is a replica the gateway cannot reach for
	// another reason (stopped, failed, its node gone).
	ReadyReplicas *int `json:"ready_replicas,omitempty"`
	// Health is the deployment's health check: on or off, its threshold and
	// what the recent checks found.
	Health DeploymentHealth `json:"health"`
	// TokenizeEnabled says whether the gateway forwards /v1/tokenize and
	// /v1/detokenize to the deployment's engine.
	TokenizeEnabled bool `json:"tokenize_enabled"`
	// NodeReplacement is how long a node may stay offline before its
	// replicas move (after_sec 0 never moves them).
	NodeReplacement NodeReplacement `json:"node_replacement"`
	// YieldPolicy is how the deployment shares GPUs with others and the
	// parameters that go with it.
	YieldPolicy YieldPolicy `json:"yield_policy"`
	// SleepHold says how its asleep replicas are held, derived from each
	// replica's own hold (Instance.SleepHeld): SleepHoldAll when an
	// operator's or a schedule's sleep holds every one (a request wakes
	// none), SleepHoldSome when it holds some (a request wakes the others),
	// SleepHoldNone when the platform put them all to sleep (a request
	// wakes them); absent with none asleep. SleepHeld is SleepHoldAll: the
	// deployment has replicas asleep and a request wakes none of them.
	SleepHold string `json:"sleep_hold,omitempty"`
	SleepHeld bool   `json:"sleep_held"`
	// SleepWakesOnRequest is what an operator's sleep of the deployment does
	// unless its order says otherwise (planning.OperatorSleepWakesOnRequest):
	// true when a request wakes what it puts to sleep, false when the sleep is
	// held. A client offers it as the default of the sleep dialog.
	SleepWakesOnRequest bool `json:"sleep_wakes_on_request"`
	// WakeWaitingSince is when the oldest wake of the deployment that waits
	// for room started waiting; absent when none waits. A waiting wake gives
	// up after WakeWaitLimitSec.
	WakeWaitingSince *time.Time `json:"wake_waiting_since,omitempty"`
	WakeWaitLimitSec int        `json:"wake_wait_limit_sec"`
	// YieldPolicyEligible says whether the deployment may hold a yield policy
	// other than never (planning.YieldPolicyRefusal), and
	// ScheduleCapabilities what its schedule may order.
	YieldPolicyEligible  Eligibility          `json:"yield_policy_eligible"`
	ScheduleCapabilities ScheduleCapabilities `json:"schedule_capabilities"`
	// PlacementMode is "auto" when the platform picks a multi-node or PD
	// deployment's nodes whenever a unit starts, "manual" when they are the
	// operator's; absent for a single-node deployment, whose draft says it.
	PlacementMode string              `json:"placement_mode,omitempty"`
	DistGroup     string              `json:"dist_group,omitempty"`
	DistRole      string              `json:"dist_role,omitempty"`
	NNodes        int                 `json:"nnodes,omitempty"`
	NodeRank      int                 `json:"node_rank"`
	MasterAddr    string              `json:"master_addr,omitempty"`
	MasterPort    int                 `json:"master_port,omitempty"`
	CreatedAt     time.Time           `json:"created_at"`
	ModelProgress []ModelNodePresence `json:"model_progress,omitempty"`
	Instances     []Instance          `json:"instances,omitempty"`
	// PD is set on a PD disaggregated deployment: its mode, policies and each
	// role's desired, ready and asleep counts, with the state derived from them
	// and from which roles the gateway needs (a prefill role its mode serves
	// without does not make the deployment unready or asleep when empty).
	PD *DeploymentPD `json:"pd,omitempty"`
	// RetryExhausted is set on an ordinary deployment one of whose replicas
	// has no instance because the controller gave up rebuilding it. A PD
	// deployment reports this per unit instead (DeploymentPDUnit).
	RetryExhausted *DeploymentRetryExhausted `json:"retry_exhausted,omitempty"`
	// Units lists a multi-node deployment's replicas: each one's leader and
	// follower nodes and its spent retry budget, the shape a PD role's units
	// take. PUT /deployments/{id}/replicas replaces the list.
	Units []DeploymentPDUnit `json:"units,omitempty"`
	// RestartRequestedAt is set while a rolling restart replaces the
	// deployment's replicas one at a time: when it was asked for.
	RestartRequestedAt *time.Time `json:"restart_requested_at,omitempty"`
	// PlacementStatus is set while replicas of an ordinary single-node
	// deployment have no instance.
	PlacementStatus *DeploymentPlacementStatus `json:"placement_status,omitempty"`
}

// DeploymentPlacementStatus is how many replicas wait for a node and the
// controller's latest reason. The reason is what the controller recorded the
// last time it found no room (written at most once per pending timeout); the
// live picture of every node is GET /deployments/{id}/candidates.
type DeploymentPlacementStatus struct {
	Unscheduled int `json:"unscheduled"`
	// Reason is the scheduler reason of the latest refusal - pool_empty or
	// pool_no_fit, whose params carry the counts and the closest nodes.
	Reason *ScheduleReason `json:"reason,omitempty"`
	At     *time.Time      `json:"at,omitempty"`
}

// DeploymentRetryExhausted names the replica of an ordinary deployment that
// spent its retry budget: POST /deployments/{id}/units/retry with
// {"role":"","index":Index} gives it back.
type DeploymentRetryExhausted struct {
	Index          int    `json:"index"`
	FailedAttempts int    `json:"failed_attempts"`
	LastFailure    string `json:"last_failure,omitempty"`
}

// InstanceReplica names the replica an instance fills: its PD role (empty
// outside PD) and its position among the role's replicas.
type InstanceReplica struct {
	Role  string `json:"role,omitempty"`
	Index int    `json:"index"`
}

// DeploymentPD is the PD section of a deployment (ADR 0028).
type DeploymentPD struct {
	Mode          string `json:"mode"`
	Fallback      string `json:"fallback"`
	KVLoadFailure string `json:"kv_load_failure"`
	// State is not_ready, partial or running; Short says some role has fewer
	// ready instances than desired.
	State string             `json:"state"`
	Short bool               `json:"short"`
	Roles []DeploymentPDRole `json:"roles"`
}

// DeploymentPDRole is one role: its TP, its units and how many are ready.
type DeploymentPDRole struct {
	Role    string `json:"role"`
	TP      int    `json:"tp"`
	PP      int    `json:"pp,omitempty"`
	Desired int    `json:"desired"`
	// Ready counts the role's units the gateway would route to now: running,
	// on online nodes, every rank of a multi-node unit up, and awake.
	Ready int `json:"ready"`
	// Asleep counts the role's units the gateway refuses for sleep, which is
	// why Ready can be short of Desired with nothing down. It is the
	// deployment's asleep_replicas split by role.
	Asleep int `json:"asleep,omitempty"`
	// MayBeEmpty says the mode runs with no instance of the role: it may be
	// scaled to zero and sleep on its own.
	MayBeEmpty bool               `json:"may_be_empty"`
	Units      []DeploymentPDUnit `json:"units"`
}

// DeploymentPDUnit is one desired instance of a role: its leader's node and
// GPUs, and the follower ranks of a multi-node instance in rank order.
type DeploymentPDUnit struct {
	NodeUUID  string             `json:"node_uuid"`
	NodeName  string             `json:"node_name,omitempty"`
	GPUs      []int              `json:"gpus,omitempty"`
	Followers []DeploymentPDRank `json:"followers,omitempty"`
	// FailedAttempts is how many launches of the unit failed (its spent
	// retry budget); LastFailure is the newest one's message.
	FailedAttempts int    `json:"failed_attempts,omitempty"`
	LastFailure    string `json:"last_failure,omitempty"`
	// RetryExhausted: the unit has no instance and the controller gave up
	// rebuilding it; POST /deployments/{id}/units/retry gives the budget back.
	RetryExhausted bool `json:"retry_exhausted,omitempty"`
}

// DeploymentPDRank is one follower rank of a multi-node PD instance.
type DeploymentPDRank struct {
	NodeUUID string `json:"node_uuid"`
	NodeName string `json:"node_name,omitempty"`
	GPUs     []int  `json:"gpus,omitempty"`
}

// DeploymentOperation is the contract projection needed to open Playground.
type DeploymentOperation struct {
	Type     string `json:"type"`
	Endpoint string `json:"endpoint"`
	Renderer string `json:"renderer"`
}

type RoutingPolicyOption struct {
	Value       string            `json:"value"`
	Label       localization.Text `json:"label"`
	Description localization.Text `json:"description"`
}

// A deployment's SleepHold: how its asleep replicas are held.
const (
	SleepHoldAll  = "all"
	SleepHoldSome = "some"
	SleepHoldNone = "none"
)

// Instance is the API view of a running replica.
type Instance struct {
	StartupEstimate       *StartupEstimate `json:"startup_estimate,omitempty"`
	StartTimeoutSec       int              `json:"start_timeout_sec,omitempty"`
	StartTimeoutSource    string           `json:"start_timeout_source,omitempty"`
	StartTimeoutConflict  bool             `json:"start_timeout_conflict,omitempty"`
	EngineStartTimeoutSec int              `json:"engine_start_timeout_sec,omitempty"`
	RuntimeFailure        *ProductMessage  `json:"runtime_failure,omitempty"`
	PreStartArgv          []string         `json:"pre_start_argv,omitempty"`
	PreStartScript        string           `json:"pre_start_script,omitempty"`
	UUID                  string           `json:"uuid"`
	DeploymentID          uint             `json:"deployment_id"`
	NodeUUID              string           `json:"node_uuid"`
	State                 string           `json:"state"`
	GPUIndexes            []int            `json:"gpu_indexes"`
	// VRAMLimitBytes is the dGPU memory limit the instance runs under on each
	// of its GPUs; absent for an instance that holds its GPUs whole.
	VRAMLimitBytes int64              `json:"vram_limit_bytes,omitempty"`
	Ports          []int              `json:"ports"`
	APIPort        int                `json:"api_port"`
	Image          string             `json:"image"`
	ImageDigestID  string             `json:"image_digest_id,omitempty"`
	Message        string             `json:"message"`
	ContainerID    string             `json:"container_id,omitempty"`
	Phase          string             `json:"phase,omitempty"`
	Progress       float64            `json:"progress,omitempty"`
	BytesDone      int64              `json:"bytes_done,omitempty"`
	BytesTotal     int64              `json:"bytes_total,omitempty"`
	RateBPS        int64              `json:"rate_bps,omitempty"`
	ModelProgress  *ModelNodePresence `json:"model_progress,omitempty"`
	SleepLevel     int                `json:"sleep_level"`
	// DesiredSleep is the level the instance is ordered to (0 awake); it
	// differs from SleepLevel while the order is being carried out.
	DesiredSleep int `json:"desired_sleep"`
	// Asleep says the gateway sends the instance nothing because of sleep:
	// ordered to sleep, or still reporting a sleep level.
	Asleep bool `json:"asleep"`
	// SleepHeld says an operator's or a schedule's sleep holds the
	// instance: a request does not wake it. Its deployment's other
	// instances may be held or not on their own.
	SleepHeld bool `json:"sleep_held"`
	// Replica is the replica the instance fills (as its leader or a
	// follower of its generation); absent when it fills none.
	Replica *InstanceReplica `json:"replica,omitempty"`
	// CanSleepAlone and CanWakeAlone say whether the instance takes a sleep
	// or wake order of its own now (POST /instances/{uuid}/sleep|wake).
	CanSleepAlone bool   `json:"can_sleep_alone"`
	CanWakeAlone  bool   `json:"can_wake_alone"`
	DistGroup     string `json:"dist_group,omitempty"`
	DistRole      string `json:"dist_role,omitempty"`
	NNodes        int    `json:"nnodes,omitempty"`
	NodeRank      int    `json:"node_rank"`
	Headless      bool   `json:"headless,omitempty"`
	DistAttempt   int    `json:"dist_attempt,omitempty"`
	// Role is the instance's PD role; empty outside PD deployments.
	Role        string            `json:"role,omitempty"`
	Schedule    any               `json:"schedule,omitempty"` // {explain, breakdown} from scheduling
	Measured    any               `json:"measured,omitempty"` // vllm-log measured breakdown
	Entrypoint  []string          `json:"entrypoint,omitempty"`
	Argv        []string          `json:"argv,omitempty"`
	Env         map[string]string `json:"env,omitempty"`
	DockerArgs  []string          `json:"docker_args,omitempty"`
	ShmBytes    int64             `json:"shm_bytes,omitempty"`
	Mounts      []EvaluateMount   `json:"mounts,omitempty"`
	CommandMode string            `json:"command_mode,omitempty"`
	ModelMount  string            `json:"model_mount_path,omitempty"`
	// ModelFiles are the model files the instance reads, as recorded when it
	// launched; empty for an instance from before they were recorded.
	ModelFiles []InstanceModelFile `json:"model_files,omitempty"`
}

// InstanceModelFile is one model file an instance reads: the storage holding
// it (absent once that storage is gone) and the host path it mounts.
type InstanceModelFile struct {
	ModelID     uint   `json:"model_id"`
	StorageID   uint   `json:"storage_id,omitempty"`
	StorageName string `json:"storage_name,omitempty"`
	StorageType string `json:"storage_type,omitempty"`
	Path        string `json:"path"`
}

// InstanceEvent is a timeline entry.
//
// ID is the pagination cursor: pass the id of the last entry of a page as
// `before` to read the entries before it. Data carries the raw payload of rows
// written before events had codes; a coded row describes itself with Params.
type InstanceEvent struct {
	ID        uint           `json:"id"`
	Kind      string         `json:"kind"`
	Code      string         `json:"code,omitempty"`
	Params    map[string]any `json:"params,omitempty"`
	Message   string         `json:"message,omitempty"`
	Data      string         `json:"data,omitempty"`
	CreatedAt time.Time      `json:"created_at"`
}

// APIKey is the API view of a key (never includes the secret).
type APIKey struct {
	Name     string     `json:"name"`
	Prefix   string     `json:"prefix"`
	LastUsed *time.Time `json:"last_used,omitempty"`
	Created  time.Time  `json:"created_at"`
	// Revoked is when the key was revoked; a revoked key never authenticates
	// and is listed so its past usage keeps a name.
	Revoked *time.Time `json:"revoked_at,omitempty"`
	// AllDeployments: the key may call every deployment. Otherwise only
	// Deployments, which may have become empty when they were all deleted.
	AllDeployments bool               `json:"all_deployments"`
	Deployments    []APIKeyDeployment `json:"deployments"`
}

// APIKeyDeployment is a deployment a restricted API key may call.
type APIKeyDeployment struct {
	ID         uint   `json:"id"`
	Name       string `json:"name"`
	ServedName string `json:"served_name"`
}

// DeployRequest is the create-deployment body.
type DeployRequest struct {
	ModelSource        *ModelSource `json:"model_source,omitempty"`
	ModelID            uint         `json:"model_id"`
	TemplateID         uint         `json:"template_id,omitempty"`
	TemplateSpecSource string       `json:"template_spec_source,omitempty"`
	TemplateSpecName   string       `json:"template_spec_name,omitempty"`
	Scenario           string       `json:"scenario,omitempty"`
	// PlacementMode is "auto" (the server picks the node) or "manual". It is
	// a request-only word: a manual placement is stored as a node
	// requirement naming NodeUUID.
	PlacementMode string `json:"placement_mode,omitempty"`
	// NodeRequirements narrows the nodes the deployment may run on: for each
	// key (a node label, or a gpuworks.ai/ system label), the values a node
	// may carry. Values of a key are alternatives; every key must hold.
	NodeRequirements map[string][]string `json:"node_requirements,omitempty"`
	// PlacementStrategy is "spread" (the default) or "binpack".
	PlacementStrategy  string         `json:"placement_strategy,omitempty"`
	TargetVendor       string         `json:"target_vendor,omitempty"`
	NodeUUID           string         `json:"node_uuid"`
	NodeUUIDs          []string       `json:"node_uuids,omitempty"`
	Name               string         `json:"name"`
	ServedName         string         `json:"served_name"`
	Engine             string         `json:"engine"`
	Image              string         `json:"image,omitempty"`
	ContainerOverrides map[string]any `json:"container_overrides,omitempty"`
	QuickParams        map[string]any `json:"quick_params"`
	// QuickParamsUnset is a request-only tombstone. It removes named engine
	// dictionary keys from template/user launch layers without persisting a
	// second launch configuration.
	QuickParamsUnset []string `json:"quick_params_unset,omitempty"`
	ExtraArgs        string   `json:"extra_args"`
	Sleepable        bool     `json:"sleepable"`
	// YieldPolicy: the never policy when absent (PUT .../yield-policy checks).
	YieldPolicy   *YieldPolicyRequest `json:"yield_policy,omitempty"`
	Replicas      int                 `json:"replicas,omitempty"`
	RoutingPolicy string              `json:"routing_policy,omitempty"`
	Placement     map[string]any      `json:"placement"`
	VRAMLimitGiB  float64             `json:"vram_limit_gib"` // >0 = dGPU memory limit (shareable GPU)
	// StartTimeoutSec is how long a started instance may take to answer its
	// health check before the start counts as failed. Zero (or absent) states
	// none: each launch resolves an automatic budget, with the cluster default
	// as its floor and the managed engine deadline taken into account.
	StartTimeoutSec int `json:"start_timeout_sec,omitempty"`
	// HealthCheck turns the server's health check of the deployment on or
	// off; absent, it is on when the model serves chat or embeddings.
	HealthCheck *bool `json:"health_check,omitempty"`
	// HealthCheckThresholdSec is how long a check waits for its reply,
	// 1–600 seconds; 0 (or absent) is 60.
	HealthCheckThresholdSec int `json:"health_check_threshold_sec,omitempty"`
	// TokenizeEnabled opens or closes the gateway's /v1/tokenize and
	// /v1/detokenize for the deployment; absent, they are open.
	TokenizeEnabled  *bool  `json:"tokenize_enabled,omitempty"`
	PartitionBackend string `json:"partition_backend,omitempty"`
	DistGroup        string `json:"dist_group,omitempty"`
	DistRole         string `json:"dist_role,omitempty"`
	NNodes           int    `json:"nnodes,omitempty"`
	NodeRank         int    `json:"node_rank,omitempty"`
	MasterAddr       string `json:"master_addr,omitempty"`
	MasterPort       int    `json:"master_port,omitempty"`
}

// ModelSource is what a deploy request says about the model's files: the
// storage a node saves them into when it has to fetch them (zero follows the
// default order; it does not limit where nodes read the model from, a
// "storage" node requirement does), and which of the template's public weight
// sources to run (huggingface, modelscope). Where a node gets the files from
// is not the request's to say: it copies them from a node of its sync group or
// downloads them, whichever applies when it starts.
type ModelSource struct {
	StorageID uint   `json:"storage_id,omitempty"`
	Provider  string `json:"provider,omitempty"`
}

// EvaluateRequest previews an assembled command line for the deploy form. It
// is the create body itself: embedding keeps the two requests one field list,
// so an input that reaches the create (sleepable)
// can never go missing from the preview.
type EvaluateRequest struct {
	DeployRequest
	// DeploymentID previews the draft as an edit of that deployment: the GPU
	// memory its own instances hold now is left out of the plan, since the
	// edit replaces them. Zero previews a new deployment.
	DeploymentID uint `json:"deployment_id,omitempty"`
}

// DeploymentCandidates is GET /deployments/{id}/candidates: every node of
// the cluster as the deployment's next replica sees it, read live. Counts
// cover the whole cluster and add up to Nodes; Items is one page of it, in the
// order replicas try nodes (candidates by rank, then unmet, then offline),
// filtered by ?status=.
type DeploymentCandidates struct {
	Nodes int `json:"nodes"`
	// Counts are by status: room, busy, unmet, offline.
	Counts map[string]int `json:"counts"`
	// UnmetByKey counts unmet nodes by the requirement they fail first.
	UnmetByKey map[string]int        `json:"unmet_by_key"`
	Strategy   string                `json:"placement_strategy"`
	Total      int                   `json:"total"`
	Offset     int                   `json:"offset"`
	Limit      int                   `json:"limit"`
	Items      []DeploymentCandidate `json:"items"`
}

// DeploymentCandidate is one node in DeploymentCandidates.
type DeploymentCandidate struct {
	UUID   string `json:"uuid"`
	Name   string `json:"name"`
	Status string `json:"status"`
	// Rank is the order replicas try candidates in (1 is first); omitted for
	// a node that is not a candidate.
	Rank int `json:"rank,omitempty"`
	// Detail says which requirement an unmet node fails.
	Detail string `json:"detail,omitempty"`
	// Reason is the structured refusal of an unmet node whose accelerators
	// cannot run the deployment, rendered by the client from its code.
	Reason   *ScheduleReason `json:"reason,omitempty"`
	Replicas int             `json:"replicas"`
	FreeGPUs int             `json:"free_gpus"`
	GPUs     int             `json:"gpus"`
	// Model is present, fetching or absent.
	Model string `json:"model"`
}

// RequirementLabel is one value of a deployment's node requirements as a
// person reads it (planning.RequirementLabel). Display is the text to show;
// DisplayCode, when present, names a client translation as SystemLabel's.
type RequirementLabel struct {
	Key         string `json:"key"`
	Value       string `json:"value"`
	Display     string `json:"display"`
	DisplayCode string `json:"display_code,omitempty"`
	Nodes       int    `json:"nodes"`
}

// DeploymentRequirementsRequest replaces a single-node deployment's node
// requirements and placement strategy (PUT /deployments/{id}/requirements).
// They bound replicas launched from then on; running ones stay where they
// are. A vendor left unstated is kept; it cannot change.
type DeploymentRequirementsRequest struct {
	NodeRequirements map[string][]string `json:"node_requirements"`
	// PlacementStrategy is spread or binpack; unstated keeps the current one.
	PlacementStrategy string `json:"placement_strategy,omitempty"`
}
