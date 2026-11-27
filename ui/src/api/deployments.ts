import type { Eligibility, ScheduleCapabilities } from "../deploymentCapabilities";
import type { YieldPolicy } from "../yieldPolicyApi";
import type { ListPage } from "./http";
import type { RequirementLabel } from "./nodes";
import type { PdRoleName } from "./engines";
import type { ModelNodePresence } from "./models";
import type { ProductWarning, ScheduleMessage } from "./scheduling";
import type { Instance } from "./instances";

// The server's health check of a deployment: a minimal request through the
// gateway every minute. It only reports.
export type HealthStatus = "ok" | "timeout" | "failed" | "off" | "no_replica" | "pending";

export interface HealthProbe {
  at_ms: number;
  status: "ok" | "timeout" | "failed";
  // A timeout's latency is the threshold.
  latency_ms: number;
  http_status?: number;
  // The replica that answered.
  instance_uuid?: string;
  // The start of an error reply, or why none came.
  message?: string;
}

export interface DeploymentHealth {
  status: HealthStatus;
  threshold_sec: number;
  last: HealthProbe | null;
  // The mean of the recent checks answered within the threshold, and how
  // many there were.
  recent_mean_ms: number | null;
  recent_ok: number;
  // Up to 10, oldest first.
  recent: HealthProbe[];
}

/** A deployment as a list row: GET /deployments/{id} carries the rest. */
export type DeploymentListItem = Pick<
  Deployment,
  | "id"
  | "name"
  | "template_id"
  | "template_deviated"
  | "engine"
  | "image"
  | "model_id"
  | "model_name"
  | "served_name"
  | "node_uuid"
  | "node_name"
  | "node_names"
  | "runtime_nodes"
  | "runtime_node_names"
  | "desired_state"
  | "runtime_state"
  | "state"
  | "asleep_replicas"
  | "sleep_level"
  | "sleepable"
  | "replicas"
  | "ready_replicas"
  | "health"
  | "operations"
  | "created_at"
> & {
  // preparation is the first of the deployment's model_progress still being
  // prepared while it is active (dto.DeploymentListItem).
  preparation?: ModelNodePresence;
};

export type DeploymentPage = ListPage<DeploymentListItem> & {
  /** Runtime nodes across all deployments, independent of filters and paging. */
  node_options?: Array<{ uuid: string; name: string }>;
};

/**
 * One node a deployment's instances are on now: where a replica actually runs,
 * which is the placement target only until something moves it.
 */
export interface DeploymentRuntimeNode {
  node_uuid: string;
  node_name?: string;
  /**
   * How many of the deployment's instance rows are on the node, of any state. A
   * spread deployment has one node per replica here; the count is what a table
   * folds into "N nodes" and expands again.
   */
  instances: number;
}

export interface Deployment {
  warnings?: ProductWarning[];
  operations?: Array<
    Pick<
      import("../inferenceApi").InferenceAPIOperation,
      "type" | "endpoint" | "renderer"
    >
  >;
  id: number;
  template_id?: number;
  template_spec_source?: string;
  template_spec_name?: string;
  template_scenario?: string;
  template_deviated: boolean;
  name: string;
  model_id: number;
  model_name: string;
  engine: string;
  image?: string;
  node_uuid: string;
  node_name?: string;
  node_uuids?: string[];
  /** Display names of node_uuids, rank by rank. */
  node_names?: string[];
  /**
   * Where the deployment's instances run now, one entry per node, read from the
   * instance rows. node_name/node_names above are the placement target — the
   * node, or rank by rank the nodes, the deployment was *asked* to run on — so
   * the two differ whenever automatic placement picks the node later, a retried
   * replica is relaunched elsewhere, or a spread deployment runs on several
   * nodes at once. Empty before anything is placed.
   */
  runtime_nodes?: DeploymentRuntimeNode[];
  /**
   * Names of runtime_nodes, in the same order. Prefer runtime_nodes where the
   * uuid or the per-node instance count is needed.
   */
  runtime_node_names?: string[];
  partition_backend?: string;
  /** The dGPU memory limit each replica runs under per card; absent on whole cards. */
  vram_limit_bytes?: number;
  served_name: string;
  desired_state: string;
  runtime_state: string;
  /** The state to show: runtime_state, except that a deployment the gateway
   *  refuses every request of because the replicas it needs sleep reads
   *  asleep, and one whose sleep order its engines have not carried out yet
   *  reads sleeping. A partly slept deployment keeps running and says so in
   *  asleep_replicas. */
  state?: string;
  /** Replicas the gateway refuses because they sleep, over every role: with
   *  ready_replicas it accounts for the replicas the gateway reaches at all. */
  asleep_replicas?: number;
  /** The deepest sleep level an engine reports: 0 awake, and while a sleep
   *  order is still being carried out. */
  sleep_level?: number;
  sleepable: boolean;
  replicas: number;
  // How many replicas the deployment may be scaled to.
  max_replicas?: number;
  // nnodes is how many ranks each replica spans; absent or 1 for single-node.
  nnodes?: number;
  routing_policy?: "least_request" | "prefix_affinity" | "failover" | string;
  // start_timeout_sec is the deployment's own instance health budget; absent
  // when it follows the cluster default (settings, deployment group).
  start_timeout_sec?: number;
  // ready_replicas counts the replicas the gateway would serve a request from
  // now (running, on an online node, every rank up, awake). A PD deployment
  // totals its roles' ready units, the same replicas its replicas field
  // totals, so the pair means one thing on both. With asleep_replicas it is
  // two counts of one set of replicas, so the two can never contradict each
  // other.
  ready_replicas?: number;
  // health is the deployment's health check (dto/deployment_health.go).
  health: DeploymentHealth;
  // tokenize_enabled says whether the gateway forwards /v1/tokenize and
  // /v1/detokenize to the deployment's engine.
  tokenize_enabled: boolean;
  // priority_scheduling says whether the deployment asked for its engine's
  // per-request priority scheduling (applied when the launch allows it).
  priority_scheduling: boolean;
  created_at: string;
  // quick_params is the deployment's complete launch configuration, keyed by
  // the parameter dictionary's keys. Panels read the values the deployment was
  // created with from here rather than re-deriving them from the command.
  quick_params?: Record<string, unknown>;
  model_progress?: ModelNodePresence[];
  instances?: Instance[];
  // PD is set on a PD disaggregated deployment: its mode, policies and each
  // role's desired/ready counts, with state derived from them (ADR 0028).
  pd?: DeploymentPd;
  // retry_exhausted names the replica of an ordinary deployment that the
  // controller gave up rebuilding; retryDeploymentUnit({role: "", index})
  // gives it back. A PD deployment reports this per unit instead.
  retry_exhausted?: { index: number; failed_attempts: number; last_failure?: string };
  // restart_requested_at is set while a rolling restart replaces the
  // replicas one at a time.
  restart_requested_at?: string;
  // units lists a multi-node deployment's replicas (leader and follower
  // nodes, spent retry budget); scaleReplicas({units}) replaces the list.
  units?: DeploymentPdUnit[];
  // node_requirements is where the deployment may run: for each key (a node
  // label or a gpuworks.ai/ system label) the values a node may carry. A
  // deployment pinned to nodes names them under gpuworks.ai/node.
  node_requirements?: Record<string, string[]>;
  // node_requirement_labels show each requirement value the way the node
  // pages show it, with how many nodes carry it now (0: a deleted node or
  // network, a label nothing has). Only a single deployment's read has them.
  node_requirement_labels?: RequirementLabel[];
  placement_strategy?: "spread" | "binpack" | string;
  // placement_mode says who picks a multi-node or PD deployment's nodes: the
  // platform ("auto", whenever a unit starts) or the operator ("manual").
  // Absent for a single-node deployment, whose requirements say it.
  placement_mode?: UnitsPlacementMode;
  // placement_status is set while replicas of an ordinary deployment wait
  // for a node: how many, and the controller's latest reason.
  placement_status?: DeploymentPlacementStatus;
  // node_replacement moves a replica off a node that stays offline for
  // after_sec (0 never moves it). wake_waiting_since is set while some wake
  // of the deployment waits for room.
  node_replacement?: NodeReplacement;
  wake_waiting_since?: string;
  // wake_wait_limit_sec is how long a wake waits for room before it gives up.
  wake_wait_limit_sec: number;
  // yield_policy is how the deployment shares its GPUs with the others on
  // them; absent counts as never.
  yield_policy?: YieldPolicy;
  // sleep_hold is how the server reads its asleep replicas: every one held
  // by an operator's or a schedule's sleep (all, a request wakes none), some
  // (a request wakes the others), or none; absent with none asleep.
  // sleep_held is sleep_hold all.
  sleep_hold?: SleepHold;
  sleep_held?: boolean;
  // sleep_wakes_on_request is what an operator's sleep does unless it says
  // otherwise: true when a request wakes what it puts to sleep (the
  // deployment shares its cards by yield policy), false when it is held.
  sleep_wakes_on_request?: boolean;
  // yield_policy_eligible says whether the deployment may hold a policy other
  // than never, and schedule_capabilities what its schedule may order: the
  // server's decisions, which the pages render and do not re-derive.
  yield_policy_eligible: Eligibility;
  schedule_capabilities: ScheduleCapabilities;
  // Whether the health check can be turned on: the server's decision, shown
  // as is.
  health_checkable: boolean;
}



export type SleepHold = "all" | "some" | "none";

export interface NodeReplacement {
  after_sec: number;
}

/** Who places a multi-node or PD deployment's units (PUT /deployments/{id}/placement-mode). */
export type UnitsPlacementMode = "auto" | "manual";

// DeploymentDraft is GET /deployments/{id}/draft: the deployment read back as
// the create request that would make it (the deploy form fills itself from
// it), the revision an edit sends back, and the request fields an edit cannot
// change for this deployment's shape (the server's list, never a literal here).
export interface DeploymentDraft {
  deployment: Record<string, unknown>;
  revision: string;
  locked: string[];
}

// live applies to the deployment at once; restart is read by a launch, so a
// running instance keeps the old value until it is replaced.
export type DeploymentChangeLayer = "live" | "restart";

export interface DeploymentChange {
  field: string;
  layer: DeploymentChangeLayer;
  // from and to are always present: a string value as itself, any other value
  // as its JSON text, an absent value as "".
  from: string;
  to: string;
  // masked: the value is withheld (an environment variable's value); from and
  // to are "" then.
  masked?: boolean;
}

export type DeploymentEditApply = "rolling" | "all" | "none";

// DeploymentEditPlan is what an edit does: PUT /deployments/{id}?dry_run=1
// answers it without writing, and the edit itself returns it.
export interface DeploymentEditPlan {
  changes: DeploymentChange[];
  restart: boolean;
  apply_options: DeploymentEditApply[];
  default_apply: DeploymentEditApply;
  can_start: boolean;
  can_wake: boolean;
  // apply_note names why the apply options are narrower than the state
  // alone allows: "partition_change" (dGPU on/off or another backend).
  apply_note?: "partition_change";
  revision: string;
}

export interface DeploymentEditBody {
  deployment: Record<string, unknown>;
  base_revision: string;
  apply?: DeploymentEditApply;
  start?: boolean;
  wake?: boolean;
}

export interface DeploymentEditResult {
  plan: DeploymentEditPlan;
  deployment: Deployment;
}


export interface DeploymentPlacementStatus {
  unscheduled: number;
  // reason is pool_empty or pool_no_fit; its params carry the counts and
  // `closest`, the best candidates with their refusals.
  reason?: ScheduleMessage;
  at?: string;
}

export type DeploymentCandidateStatus = "room" | "busy" | "unmet" | "cordoned" | "offline";

export interface DeploymentCandidate {
  uuid: string;
  name: string;
  status: DeploymentCandidateStatus;
  rank?: number;
  detail?: string;
  // reason is set instead of detail when the node's accelerators cannot run
  // the deployment (unmet key "accelerator").
  reason?: ScheduleMessage;
  replicas: number;
  free_gpus: number;
  gpus: number;
  model: "present" | "fetching" | "absent";
}

// DeploymentCandidates is one page of the cluster as a deployment's next
// replica sees it; counts cover every node.
export interface DeploymentCandidates {
  nodes: number;
  counts: Record<DeploymentCandidateStatus, number>;
  unmet_by_key: Record<string, number>;
  placement_strategy: string;
  total: number;
  offset: number;
  limit: number;
  items: DeploymentCandidate[];
}

// DeploymentPdFollower is one headless rank of a multi-node unit: another
// node (plus the GPUs it runs, when pinned) alongside the unit's own node,
// in rank order. It carries no TP/PP of its own - the role's tp/pp cover the
// whole unit.
export interface DeploymentPdFollower {
  node_uuid: string;
  node_name?: string;
  gpus?: number[];
}

export interface DeploymentPdUnit {
  node_uuid: string;
  node_name?: string;
  gpus?: number[];
  followers?: DeploymentPdFollower[];
  // The unit's spent retry budget (ADR 0029): failed launches, the newest
  // failure's message, and whether the controller gave up rebuilding it.
  failed_attempts?: number;
  last_failure?: string;
  retry_exhausted?: boolean;
}

export interface DeploymentPdRole {
  role: PdRoleName;
  tp: number;
  // Pipeline-parallel degree; absent/omitted when the role runs PP 1 (no
  // multi-node unit has needed a wire value yet).
  pp?: number;
  desired: number;
  // ready counts the role's units the gateway would route to now: running, on
  // online nodes, every rank up, and awake. asleep says why ready may be short
  // of desired with nothing down — those units sleep.
  ready: number;
  asleep?: number;
  // The mode runs with no instance of the role: it may scale to zero and
  // sleep on its own.
  may_be_empty?: boolean;
  units: DeploymentPdUnit[];
}

export interface DeploymentPd {
  mode: string;
  fallback: string;
  kv_load_failure: string;
  // not_ready: no role has a ready instance yet; partial: some but not every
  // role the gateway needs has one (a role the mode serves without does not
  // count); running: every one of them does.
  state: "not_ready" | "partial" | "running";
  // short: some role has fewer ready instances than desired.
  short: boolean;
  roles: DeploymentPdRole[];
}

// PdScaleFollowerInput is one headless rank on the wire for PUT
// /deployments/{id}/pd - node_uuid plus an optional GPU pin, no node_name
// (an input, not a display DTO).
export interface PdScaleFollowerInput {
  node_uuid: string;
  gpus?: number[];
}

// PdScaleUnitInput is one role's unit on the wire for PUT
// /deployments/{id}/pd - node_uuid plus an optional GPU pin and followers,
// the same shape buildPdPlacement (pages/deploy/deployPd.ts) projects for
// the deploy draft. TP and PP travel with neither: a role's TP/PP are fixed
// once instances start (PD_IMMUTABLE) and the endpoint only ever changes
// units.
// UnitSuggestion is a proposed multi-node replica: leader then followers on
// nodes sharing a compute network and GPU architecture, each rank
// trial-planned; unit is null with a reason (unit_none, unit_no_fit) when
// nothing fits.
export interface UnitSuggestion {
  unit: PdScaleUnitInput | null;
  network?: { id: number; name: string };
  arch?: string;
  nodes?: { uuid: string; name: string; free_gpus: number; model: string }[];
  reason?: ScheduleMessage;
}

export interface PdScaleUnitInput {
  node_uuid: string;
  gpus?: number[];
  followers?: PdScaleFollowerInput[];
}

// PdScaleRoleUnit is a unit of PUT /deployments/{id}/pd. On an automatically
// placed deployment a new unit may leave node_uuid out (the platform picks its
// node; a stated node is its first placement) and states no gpus.
export type PdScaleRoleUnit = Omit<PdScaleUnitInput, "node_uuid"> & { node_uuid?: string };

export interface PdScaleRequest {
  roles?: Partial<Record<PdRoleName, { units: PdScaleRoleUnit[] }>>;
  fallback?: string;
}
