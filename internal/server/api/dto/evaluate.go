package dto

import (
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/messages"
)

// This file holds the evaluate response contract: the deploy form's feedback
// object and the projections it renders. The create/deployment DTOs stay in
// deploy.go.

// FieldError is one request error attributed to a form field: the stable
// ProductError code the client translates, with the interpolation params and
// the field path that names the control the error belongs next to. The English
// fallback text stays on the 400 path; an errors[] entry carries only the code
// and params, because the deploy form renders the message in the client locale.
type FieldError struct {
	Field  string         `json:"field"`
	Code   string         `json:"code"`
	Params map[string]any `json:"params,omitempty"`
}

// ProductMessage is a product error reported inside an answer rather than as
// it: Code is rendered by the client locale with Params, Message is the
// English fallback.
type ProductMessage struct {
	Code    string         `json:"code"`
	Params  map[string]any `json:"params,omitempty"`
	Message string         `json:"message"`
}

// EvictionSuggestion identifies an active deployment whose sleep could free
// enough GPU memory for an evaluated launch.
type EvictionSuggestion struct {
	InstanceUUID string  `json:"instance_uuid"`
	DeploymentID uint    `json:"deployment_id"`
	Deployment   string  `json:"deployment"`
	GPUIndexes   []int   `json:"gpu_indexes"`
	FreesGiB     float64 `json:"frees_gib"`
}

// EvaluateResponse is the deploy-form feedback (argv preview + VRAM breakdown +
// schedule plan).
type EvaluateResponse struct {
	// GPUCandidates describes every card's resource availability for a manual
	// single-node draft. Unavailability is informational, not a selection ban.
	GPUCandidates *GPUCandidates `json:"gpu_candidates,omitempty"`

	StartupEstimate       *StartupEstimate `json:"startup_estimate,omitempty"`
	StartTimeoutSec       int              `json:"start_timeout_sec,omitempty"`
	StartTimeoutSource    string           `json:"start_timeout_source,omitempty"`
	StartTimeoutConflict  bool             `json:"start_timeout_conflict,omitempty"`
	EngineStartTimeoutSec int              `json:"engine_start_timeout_sec,omitempty"`
	// SchemaVersion versions the evaluate response contract. Clients that
	// render fixtures assert it, so a response shape they depend on cannot
	// drift from what the server publishes without failing a test.
	SchemaVersion int `json:"schema_version,omitempty"`
	// Errors are the request errors attributed to form fields. A response with
	// errors has ok=false and stays HTTP 200: evaluation is the interactive
	// endpoint, and the form keeps the draft while showing each error beside
	// its control. Codes match the 400 path create would answer with.
	Errors      []FieldError      `json:"errors,omitempty"`
	Diagnostics *DiagnosticReport `json:"diagnostics,omitempty"`
	RequestID   string            `json:"request_id,omitempty"`
	// AutoPlaced says the server picked the nodes of this multi-node or PD
	// request (placement_mode "auto"): the nodes the preview shows are where
	// it would go now, and the create picks again.
	AutoPlaced bool `json:"auto_placed,omitempty"`
	// One breakdown/plan describes one replica. Repeated replicas compete for
	// shared capacity and are admitted separately at scheduling time.
	Replicas           int                   `json:"replicas"`
	PlanScope          string                `json:"plan_scope"`
	PreStartArgv       []string              `json:"pre_start_argv,omitempty"`
	PreStartScript     string                `json:"pre_start_script,omitempty"`
	OK                 bool                  `json:"ok"`
	Skipped            bool                  `json:"skipped,omitempty"`
	SkipReason         string                `json:"skip_reason,omitempty"`
	TemplateSpecSource string                `json:"template_spec_source,omitempty"`
	TemplateSpecName   string                `json:"template_spec_name,omitempty"`
	TemplateMatch      string                `json:"template_match,omitempty"`
	MatchedBy          string                `json:"matched_by,omitempty"`
	TemplateDeviated   bool                  `json:"template_deviated"`
	Entrypoint         []string              `json:"entrypoint,omitempty"`
	Argv               []string              `json:"argv"`
	Command            []string              `json:"command,omitempty"`
	Env                map[string]string     `json:"env,omitempty"`
	EnvLayers          *EnvLayers            `json:"env_layers,omitempty"`
	CommandMode        string                `json:"command_mode,omitempty"`
	ModelMount         string                `json:"model_mount_path,omitempty"`
	DockerArgs         []string              `json:"docker_args,omitempty"`
	ShmBytes           int64                 `json:"shm_bytes,omitempty"`
	Mounts             []EvaluateMount       `json:"mounts,omitempty"`
	RankCommands       []EvaluateRankCommand `json:"rank_commands,omitempty"`
	Image              string                `json:"image"`
	Warnings           []messages.Warning    `json:"warnings"`
	Breakdown          any                   `json:"breakdown,omitempty"` // estimate.Breakdown
	Eviction           []EvictionSuggestion  `json:"eviction,omitempty"`
	Plan               any                   `json:"plan,omitempty"` // scheduler.Result (chosen GPUs + explain)
	SelectedNode       *PlacementNode        `json:"selected_node,omitempty"`
	Candidates         []PlacementCandidate  `json:"candidates,omitempty"`
	Acquisition        *AcquisitionResult    `json:"acquisition,omitempty"`
	// WeightSources lists the template spec's declared weight sources with
	// the suggested and registered facts, so the form renders a choice instead
	// of resolving one.
	WeightSources []WeightSourceOption `json:"weight_sources,omitempty"`
	// Preflight is the inline preflight summary for a multi-node draft.
	Preflight *PreflightSummary `json:"preflight,omitempty"`
	// Parallelism is the split this draft launches with; absent until the
	// degree is stated or a single-node schedule picked one.
	Parallelism *EvaluateParallelism `json:"parallelism,omitempty"`
	// EligibleNodes is every node judged against this draft's engine,
	// template predicate, label selector and vendor, eligible or not.
	EligibleNodes []EligibleNode `json:"eligible_nodes,omitempty"`
	// PoolPartitionCapability is the dGPU backend the eligible nodes offer to an
	// automatic single-node draft: the first eligible node's ready one, absent
	// when none has one or the draft names its node. An automatic draft gates
	// its memory quota on it, having no node of its own yet.
	PoolPartitionCapability *proto.PartitionCapability `json:"pool_partition_capability,omitempty"`
	// FormValues is what the form should display: the effective launch
	// configuration (dictionary defaults included, template projection merged)
	// plus the extra-args text after moved flags left it. The client sets its
	// fields from this instead of deriving any of it itself.
	FormValues map[string]any `json:"form_values,omitempty"`
	// Defaults carries the constants the form used to hardcode, so the client
	// reads them from the response instead of its own literals.
	Defaults *EvaluateDefaults `json:"defaults,omitempty"`
	// EffectiveParams is the complete launch configuration this request
	// materializes to: what the form should display, what the estimate used and
	// what a create would store are one object.
	EffectiveParams map[string]any `json:"effective_params,omitempty"`
	// Moved lists dictionary flags lifted out of extra_args into fields.
	Moved []deploy.MovedFlag `json:"moved,omitempty"`
	// TemplateParams and TemplateResidualArgs are what the selected catalog spec
	// contributed, split the same way: values the dictionary owns and the
	// argument tokens it does not.
	TemplateParams       map[string]any `json:"template_params,omitempty"`
	TemplateResidualArgs []string       `json:"template_residual_args,omitempty"`
	// EngineDecidesParamKeys are the dictionary keys whose default the
	// effective launch leaves to the engine - its entrypoint or engine release
	// may refuse the flag - so the form shows them as automatic until the
	// deployer turns them on or off.
	EngineDecidesParamKeys []string `json:"engine_decides_param_keys,omitempty"`
	// PD previews a PD disaggregated draft instance by instance; the command
	// fields above then describe the first decode instance.
	PD *EvaluatePD `json:"pd,omitempty"`
	// YieldPolicyEligible says whether the draft may be created with a yield
	// policy other than never (planning.DraftYieldPolicyRefusal), the check
	// the create makes; absent until the draft's engine is known.
	YieldPolicyEligible *Eligibility `json:"yield_policy_eligible,omitempty"`
}

type GPUCandidates struct {
	NodeUUID string         `json:"node_uuid"`
	Items    []GPUCandidate `json:"items"`
}

type GPUCandidate struct {
	Index     int             `json:"index"`
	Available bool            `json:"available"`
	Reason    *ScheduleReason `json:"reason,omitempty"`
}

// EvaluatePD is the preview of a PD draft (ADR 0028).
type EvaluatePD struct {
	Mode          string           `json:"mode"`
	Fallback      string           `json:"fallback"`
	KVLoadFailure string           `json:"kv_load_failure"`
	Roles         []EvaluatePDRole `json:"roles"`
}

// EvaluatePDRole is one role's preview.
type EvaluatePDRole struct {
	Role  string           `json:"role"`
	TP    int              `json:"tp"`
	PP    int              `json:"pp,omitempty"`
	Units []EvaluatePDUnit `json:"units"`
}

// EvaluatePDUnit is one instance's preview: whether it fits its node, the
// GPUs the schedule chose, the side channel address, and its command.
type EvaluatePDUnit struct {
	NodeUUID   string            `json:"node_uuid"`
	NodeName   string            `json:"node_name,omitempty"`
	Address    string            `json:"address,omitempty"`
	GPUs       []int             `json:"gpus,omitempty"`
	Fits       bool              `json:"fits"`
	Reason     any               `json:"reason,omitempty"`
	Breakdown  any               `json:"breakdown,omitempty"`
	Entrypoint []string          `json:"entrypoint,omitempty"`
	Argv       []string          `json:"argv"`
	Command    []string          `json:"command,omitempty"`
	Env        map[string]string `json:"env,omitempty"`
	DockerArgs []string          `json:"docker_args,omitempty"`
	ShmBytes   int64             `json:"shm_bytes,omitempty"`
	Mounts     []EvaluateMount   `json:"mounts,omitempty"`
	// Followers previews the follower ranks of a multi-node instance, in
	// rank order; each is headless and has no followers of its own.
	Followers []EvaluatePDUnit `json:"followers,omitempty"`
}

// EvaluateParallelism is a deployment's parallelism as it takes effect: TP
// and PP over the whole deployment, and the GPUs each node runs.
type EvaluateParallelism struct {
	TP          int `json:"tp"`
	PP          int `json:"pp"`
	Nodes       int `json:"nodes"`
	GPUsPerNode int `json:"gpus_per_node"`
}

// EvaluateDefaults carries the deploy form's former hardcoded constants: the
// container path the weights mount at, the port a distributed launch's master
// listens on, and the engine a deployment names none.
type EvaluateDefaults struct {
	ModelMount string `json:"model_mount"`
	MasterPort int    `json:"master_port"`
	Engine     string `json:"engine"`
}

// WeightSourceOption is one weight source the selected template spec
// declares, with the two facts the form needs to render a choice: whether the
// server suggests it and whether a matching model is already registered.
type WeightSourceOption struct {
	Provider   string `json:"provider"`
	RepoID     string `json:"repo_id"`
	Revision   string `json:"revision,omitempty"`
	Subfolder  string `json:"subfolder,omitempty"`
	Suggested  bool   `json:"suggested"`
	Registered bool   `json:"registered"`
}

// PreflightCheck is one structural preflight judgement an evaluation answers
// inline: the request-level rules (node count, duplicates, port) and each
// named node's presence, online state and routable address. Deep network
// probing stays with the preflight RPC.
type PreflightCheck struct {
	Code    string         `json:"code,omitempty"`
	Params  map[string]any `json:"params,omitempty"`
	Message string         `json:"message,omitempty"`
}

// PreflightSummary is the inline preflight answer for a multi-node draft.
type PreflightSummary struct {
	OK     bool             `json:"ok"`
	Checks []PreflightCheck `json:"checks,omitempty"`
}

// EligibleNode is one node judged against the request the form is drafting:
// whether the node could host it and, when it could not, the reasons in the
// same schedule-reason vocabulary the plan explains itself with. The node
// dropdown renders this list instead of running predicates of its own.
type EligibleNode struct {
	// RequirementsMatch is the independent result of explicit node conditions
	// (labels, networks and node selections), before vendor/chip/state checks.
	// Omitted when none were stated or the requirements could not be decoded.
	RequirementsMatch *bool            `json:"requirements_match,omitempty"`
	UUID              string           `json:"uuid"`
	Name              string           `json:"name"`
	Eligible          bool             `json:"eligible"`
	Reasons           []ScheduleReason `json:"reasons,omitempty"`
}

// ScheduleReason is the wire form of one scheduler explanation: the stable code
// the client translates, the parameters it interpolates, and the legacy
// message/score an older row may still carry. The scheduler's own Reason type
// has the same field names, and this package keeps its own copy so the DTO
// layer does not depend on the scheduler it describes.
type ScheduleReason struct {
	Code    string         `json:"code,omitempty"`
	Params  map[string]any `json:"params,omitempty"`
	Message string         `json:"message,omitempty"`
	Score   int            `json:"score,omitempty"`
}

// PlacementNode identifies the node selected by server-side automatic
// placement. Reasons are the scheduler's actual reasons for the chosen GPU
// plan, rather than a client-side reconstruction.
type PlacementNode struct {
	UUID    string           `json:"uuid"`
	Name    string           `json:"name,omitempty"`
	Reasons []ScheduleReason `json:"reasons,omitempty"`
}

// PlacementCandidate records every node considered by automatic placement.
// Plan is populated for feasible candidates; Reason explains the first hard
// rejection for nodes that never reached a viable plan.
type PlacementCandidate struct {
	NodeUUID string         `json:"node_uuid"`
	NodeName string         `json:"node_name,omitempty"`
	OK       bool           `json:"ok"`
	Reason   ScheduleReason `json:"reason,omitempty"`
	Plan     any            `json:"plan,omitempty"`
}

// AcquisitionResult is the stable wire contract for deployment acquisition
// candidates. Keeping this typed prevents accidental exposure of internal
// resolver fields.
type AcquisitionResult struct {
	// Present has one candidate per storage that holds the model and the
	// target nodes can read; Targets are the storages a node may save the
	// model into when it has to fetch it; Providers are the template's public
	// weight sources, which say which weights run.
	Present   []AcquisitionCandidate `json:"present,omitempty"`
	Targets   []AcquisitionCandidate `json:"targets,omitempty"`
	Providers []AcquisitionCandidate `json:"providers,omitempty"`
	Image     AcquisitionImage       `json:"image"`
	// Nodes is, for each node the request names (a manual placement or a
	// multi-node draft, in rank order), how that node will get the image and
	// the weights if it started now. It is a forecast, not a choice: the node
	// decides again when it starts.
	Nodes []AcquisitionNode `json:"nodes,omitempty"`
	// Components is, for each template component other than the primary (a
	// separately published text encoder, a speculative draft), how the
	// target nodes would get it if the deployment were created now. It
	// appears once the target nodes are known.
	Components []AcquisitionComponent `json:"components,omitempty"`
}

// AcquisitionComponent is one template component's forecast. Plan is "ready"
// (every target node reads it already), "shared_download" (the create
// downloads it once into the shared storage Storage, through one writer node,
// and the target nodes read that copy; FollowsPrimary says the request named
// no storage and Storage is the one the primary sits on) or "node_download"
// (each target node without it downloads it into its own storage). Nodes
// counts the target nodes, ReadyNodes those that read it now.
type AcquisitionComponent struct {
	Name           string `json:"name"`
	Role           string `json:"role"`
	Model          string `json:"model"`
	SizeBytes      int64  `json:"size_bytes,omitempty"`
	Plan           string `json:"plan"`
	Nodes          int    `json:"nodes"`
	ReadyNodes     int    `json:"ready_nodes"`
	StorageID      uint   `json:"storage_id,omitempty"`
	Storage        string `json:"storage,omitempty"`
	FollowsPrimary bool   `json:"follows_primary,omitempty"`
}

// AcquisitionNode is one node's forecast. ImagePlan is "present" (the node
// holds the image), "copy" (a node of its sync group, ImageCopyFrom, copies it
// over) or "pull" (from ImagePullRefs: the first ImagePullFixed of them, a pull
// source rule's endpoints, in that order, then the rest; ImagePullRanked says
// the node orders the rest, the image's own copies, by how fast it reaches
// their registries). WeightsPlan is "present", "copy" (from WeightsCopyFrom),
// "storage" (read or cached from the shared storage WeightsStorage) or
// "download" (from WeightsProvider's model, at WeightsFetchHosts in the order
// the node tries them - the site rules' addresses, then the site itself
// unless a rule forbids it; WeightsFetchError when the rules forbid every
// address).
type AcquisitionNode struct {
	NodeUUID          string          `json:"node_uuid"`
	NodeName          string          `json:"node_name"`
	ImageCached       *bool           `json:"image_cached,omitempty"`
	ImageCacheReason  string          `json:"image_cache_reason,omitempty"`
	ImagePlan         string          `json:"image_plan"`
	ImageCopyFrom     string          `json:"image_copy_from,omitempty"`
	ImagePullRefs     []string        `json:"image_pull_refs,omitempty"`
	ImagePullRanked   bool            `json:"image_pull_ranked,omitempty"`
	ImagePullFixed    int             `json:"image_pull_fixed,omitempty"`
	ImagePullError    *ProductMessage `json:"image_pull_error,omitempty"`
	WeightsPresent    bool            `json:"weights_present"`
	WeightsPlan       string          `json:"weights_plan"`
	WeightsCopyFrom   string          `json:"weights_copy_from,omitempty"`
	WeightsStorage    string          `json:"weights_storage,omitempty"`
	WeightsProvider   string          `json:"weights_provider,omitempty"`
	WeightsFetchHosts []string        `json:"weights_fetch_hosts,omitempty"`
	WeightsFetchError *ProductMessage `json:"weights_fetch_error,omitempty"`
}
type AcquisitionCandidate struct {
	Key         string `json:"key"`
	Label       string `json:"label"`
	Reason      string `json:"reason"`
	Recommended bool   `json:"recommended"`
	// DisplayName names the candidate for a person when Label is an
	// identifier. Clients show it in preference to Label.
	DisplayName string `json:"display_name,omitempty"`
	// Storage fields appear on present and target candidates.
	StorageID   uint   `json:"storage_id,omitempty"`
	StorageType string `json:"storage_type,omitempty"`
	SizeBytes   int64  `json:"size_bytes,omitempty"`
	CacheBytes  int64  `json:"cache_bytes,omitempty"`
	OnNode      bool   `json:"on_node,omitempty"`
	Unavailable string `json:"unavailable,omitempty"`
	Default     bool   `json:"default,omitempty"`
}
type AcquisitionImage struct {
	// Image is the canonical reference the deployment runs.
	Image       string        `json:"image,omitempty"`
	Cached      *bool         `json:"cached,omitempty"`
	CacheReason string        `json:"cache_reason,omitempty"`
	Options     []ImageOption `json:"options,omitempty"`
}

// ImageOption is a selectable canonical runtime image. Acquisition of the
// selected image is represented independently by AcquisitionImage.
type ImageOption struct {
	Image      string `json:"image"`
	Reason     string `json:"reason"`
	Compatible bool   `json:"compatible"`
}

type EnvLayers struct {
	System   map[string]string `json:"system,omitempty"`
	Template map[string]string `json:"template,omitempty"`
	User     map[string]string `json:"user,omitempty"`
}

type EvaluateMount struct {
	Source   string `json:"source,omitempty"`
	Volume   string `json:"volume,omitempty"`
	Target   string `json:"target"`
	ReadOnly bool   `json:"read_only,omitempty"`
	SizeHint string `json:"size_hint,omitempty"`
	Purpose  string `json:"purpose,omitempty"`
}

// EvaluateRankCommand is the exact engine launch contract for one ordered
// node in a distributed deployment preview.
type EvaluateRankCommand struct {
	PreStartArgv   []string          `json:"pre_start_argv,omitempty"`
	PreStartScript string            `json:"pre_start_script,omitempty"`
	NodeUUID       string            `json:"node_uuid"`
	NodeName       string            `json:"node_name,omitempty"`
	RankAddress    string            `json:"rank_address,omitempty"`
	NodeRank       int               `json:"node_rank"`
	Role           string            `json:"role"`
	Headless       bool              `json:"headless"`
	Image          string            `json:"image"`
	Entrypoint     []string          `json:"entrypoint,omitempty"`
	Argv           []string          `json:"argv"`
	Command        []string          `json:"command,omitempty"`
	Env            map[string]string `json:"env,omitempty"`
	CommandMode    string            `json:"command_mode,omitempty"`
	ModelMount     string            `json:"model_mount_path,omitempty"`
	DockerArgs     []string          `json:"docker_args,omitempty"`
	ShmBytes       int64             `json:"shm_bytes,omitempty"`
	Mounts         []EvaluateMount   `json:"mounts,omitempty"`
	EnvLayers      *EnvLayers        `json:"env_layers,omitempty"`
}
