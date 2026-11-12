import type { ServerBuildInfo } from "./serverBuildInfo";
import type { BenchmarkPointEvidence } from "./benchmarkEvidenceTypes";
export type * from "./benchmarkEvidenceTypes";
// API client + shared types. Types mirror internal/server/api/dto.
import type { Eligibility, ScheduleCapabilities } from "./deploymentCapabilities";
import type { YieldPolicy } from "./yieldPolicyApi";
import { useEffect, useRef, useState } from "react";
import type {
  NetworkAttachBatchResult, NetworkAttachmentRequest, NetworkDetail, NetworkHCARequest, NetworkList,
  NetworkOverrideRequest, NetworkRequest, NetworkRole, NetworkTestEstimate, NetworkTestRequest, NetworkTestRun, NodeNetworks,
} from "./networkTypes";
export type * from "./networkTypes";
import type {
  ModelLocationRef, ModelStoragePresence, Storage, StorageControlPlane, StorageImportItem, StorageImportResult, StorageModelFile,
  StorageProbeRequest, StorageProbeResult, StorageRequest, StorageScanResult, StorageSettings, StorageNodeOverrideRequest,
  StorageDownloadFormat, StorageFileList,
} from "./storageTypes";
export type * from "./storageTypes";
import type { AgentUpgradeCreate, AgentUpgradeItem, AgentUpgradePage, AgentUpgradePreview, AgentUpgradeRun } from "./agentUpgradesTypes";
export type * from "./agentUpgradesTypes";
import i18n from "./i18n";
import { APIError, apiErrorText, apiResponseError, ProxyErrorPage, type ProductMessage } from "./apiError";
import type { DiagnosticReport } from "./diagnosticChecks";
import { readSSE } from "./sse";
import { isSessionExpired, subscribeSessionChanges } from "./session";
import type { LocalizedText } from "./i18n/localizedText";
import type { InferenceAPIOperation } from "./inferenceApi";
import {
  monitorDistributionsURL,
  monitorLatencyURL,
  type MonitorLatency,
  monitorRequestsURL,
  monitorRequestLookupURL,
  type MonitorRequestLookup,
  type MonitorRequestLookupQuery,
  type MonitorDistributions,
  type MonitorRequests,
  type MonitorRequestsQuery,
} from "./monitorRequestsApi";
import type { TelemetryExport, TelemetryExportTest, TelemetryExportUpdate } from "./telemetryApi";
import type { DebugSessionStart, DebugSessionState, DiagnosticsBundle, DiagnosticsBundleStart, DiagnosticsBundles } from "./diagnosticsApi";

/** What an operator sets one card to (PUT /nodes/{uuid}/gpus/scheduling). */
export type CardScheduling = "stopped" | "schedulable";

export interface GPU {
  /** The vendor CLI's device index, or -1 when it does not list the card. */
  index: number;
  uuid: string;
  name: string;
  compute_capability: string;
  arch_family: string;
  vendor?: string;
  arch_id?: string;
  memory_total_bytes: number;
  memory_capacity_known?: boolean; // Present on current servers; absent on older API responses.
  power_limit_watts?: number;
  pcie_bdf?: string;
  numa_node: number;
  partition_capabilities?: PartitionCapability[];
  mig_enabled?: boolean;
  /** Empty when this node owns the card; otherwise why it cannot use it. */
  ownership?: "" | "passthrough" | "no_driver" | "held_elsewhere";
  /**
   * Whether work may be placed on this card, as the server decided
   * (accelerator.CardSchedulable). A card lent to a virtual machine is
   * not faulty and still takes no work; a card held by somebody else's
   * process is healthy and takes none either. Why not is in
   * `health_status` / `health_code` / `ownership` / `foreign_processes`.
   */
  schedulable: boolean;
  /** Compute processes on the card that the platform does not account for. */
  foreign_processes?: { pid: number; name: string; memory_bytes?: number }[];
  /** An operator took the card out of scheduling; only they put it back. */
  scheduling_stopped?: boolean;
  /** An operator let work onto a card the platform judged unusable, until the next report judges it afresh. */
  scheduling_allowed?: boolean;
  /** What an operator may do to the card now, decided by the server. */
  scheduling_actions?: CardScheduling[];
  health_status?: string;
  health_code?: string;
  health_message?: string;
  health_evidence?: GPUHealthField[];
  health_guide?: GPUHealthGuide;
}

export interface PartitionCapability {
  backend: string;
  display_name: string;
  vendor: string;
  maturity: "release" | "experimental" | string;
  status: "ready" | "unavailable";
  reason?: string;
  compatibility_summary?: string;
  compatibility_notes?: string[];
  probe_status?: "passed" | "failed" | "skipped" | string;
  probe_checked_at?: string;
}

// --- GPU topology ---------------------------------------------------------
export interface TopoLink {
  raw: string;
  class: number;
  nvlink_bonds: number;
}
export interface TopoGPU {
  index: number;
  cpu_affinity: string;
  numa_node: number;
}
export interface Topology {
  gpus: TopoGPU[];
  matrix: TopoLink[][]; // matrix[i][j] link between GPU i and j
  fast_cliques: number[][] | null;
  nvlink_cliques: number[][] | null;
  numa_groups: Record<string, number[]> | null; // numa node -> gpu indexes
}
export interface TopoHighlight {
  gpus: number[];
  links: [number, number][];
  numa: number[];
}

// SystemLabel is one generated node label. value is what a selector matches
// on; display is the text to show, and display_code names a translation to
// prefer when the value is platform vocabulary rather than someone's name.
export interface SystemLabel {
  key: string;
  value: string;
  display: string;
  display_code?: string;
}

export interface RequirementLabel extends SystemLabel {
  nodes: number;
}

export type NodeScheduling = "schedulable" | "cordoned" | "draining";

// NodeReplica is one instance on a node and whether a drain moves it: a
// "pool" replica is replaced on another node, a "pinned" one waits (its
// deployment names only this node), a "named" multi-node or PD rank moves
// only when evicted.
export interface NodeReplica {
  instance: string;
  deployment_id: number;
  deployment: string;
  state: string;
  kind: "pool" | "pinned" | "named";
  drain_reason?: ScheduleMessage;
  drain_reason_at?: string;
  drain_message?: string;
}

export interface NodeReplicas {
  drain_offline?: boolean;
  scheduling: NodeScheduling;
  items: NodeReplica[];
}

/** One vendor whose cards are in a node's host (Node.accelerator_vendors). */
export interface AcceleratorPresence {
  vendor: string;
  cards: number;
  model?: string;
}

export interface Node {
  uuid: string;
  // scheduling is whether the node takes new replicas: cordoned takes none,
  // draining also moves the replicas a pool placed there.
  scheduling?: NodeScheduling;
  // scheduling_by is who set scheduling (absent when nobody holds the node,
  // or the author was not recorded): the platform lifts only its own.
  scheduling_by?: "operator" | "platform";
  // cordon_reason is why the platform holds the node now; absent unless
  // scheduling_by is platform.
  cordon_reason?: "runtime_gpu_blocked" | "card_needs_host_reboot" | "kube_unschedulable";
  name?: string;
  token?: string;
  /** Installs this node's agent; returned with the token, and the way back
   *  for a node the platform cannot restore over SSH. */
  install_command?: string;
  /** Whether the agent is on the other end; see nodes/connectionState. */
  state: "online" | "offline" | "connecting";
  hostname: string;
  os: string;
  kernel: string;
  arch: string;
  cpu_model: string;
  cpu_cores: number;
  mem_total_gib: number;
  disk_total_gib: number;
  gpu_count: number;
  /** Why the last accelerator probe failed, and when; absent once one succeeds. */
  gpu_probe_error?: string;
  /**
   * Why this node's container runtime cannot give a container its
   * accelerators. While set, the platform keeps the node cordoned.
   */
  runtime_gpu_blocked?: string;
  /**
   * Display name of the vendor container plugin this node's vendor and
   * runtime need ("NVIDIA Container Toolkit"); empty when none is needed.
   */
  runtime_gpu_plugin?: string;
  /**
   * The runtime the agent passed over for runveil because it cannot give a
   * container this node's accelerators ("docker" | "nerdctl"), why
   * (gpu_plugin_missing | gpu_runtime_unconfigured | gpu_ready), and the
   * plugin that runtime needs. Absent when nothing was passed over.
   */
  runtime_fallback_from?: string;
  runtime_fallback_reason?: string;
  runtime_fallback_plugin?: string;
  /**
   * When the current streak of failed vendor-tool probes started; kept
   * across consecutive failures and absent once a probe succeeds.
   */
  gpu_probe_failed_at?: string;
  gpu_vendor?: string;
  gpu_driver?: string;
  nvidia_driver?: string;
  framework?: string;
  framework_version?: string;
  path_usage?: Array<{
    kind: string;
    /** Names the storage a storage or storage_cache entry measures. */
    storage_id?: number;
    path: string;
    total_bytes: number;
    available_bytes: number;
  }>;
  runtime_driver?: string;
  runtime_preference?: string;
  /** The runtime preferences the operator may set, in the order to offer; absent when the node has no choice (a Kubernetes node). */
  runtime_choices?: string[];
  /** The operator's choice of which vendor's cards the node manages: auto or a vendor; absent is auto. */
  accelerator_preference?: string;
  /** Every vendor whose cards the agent saw on the host's bus, the managed one included. */
  accelerator_vendors?: AcceleratorPresence[];
  /** "<Name> <version>" of the installed vendor container plugin; empty when it is not installed. */
  runtime_plugin?: string;
  runtimes?: string[];
  partition_capabilities?: PartitionCapability[];
  labels?: Record<string, string>;
  // The labels the platform generates from the node's own facts, kept apart
  // from the written ones because they cannot be edited. A key appears once
  // per value the node carries for it.
  system_labels?: SystemLabel[];
  // The address the node's agent reaches the server from: its address on the
  // management network.
  management_address?: string;
  server_url?: string;
  connect_mode?: string;
  server_dial_address?: string;
  server_dial_listen?: string;
  agent_version?: string;
  caps?: string[];
  last_remote_addr?: string;
  last_reject_reason?: string;
  last_reject_at?: string;
  last_seen_at?: string;
  recovery_phase?:
    | ""
    | "probing"
    | "artifact_check"
    | "uploading"
    | "verifying"
    | "replacing"
    | "restarting"
    | "reinstalling"
    | "reconnecting"
    | "unreachable"
    | "recovery_blocked"
    | "stopped";
  recovery_attempts?: number;
  last_probe_at?: string;
  last_recovery_error?: string;
  has_ssh_credential: boolean;
  created_at: string;
  gpus: GPU[];
  topology?: Topology;
  /** Resolved compute network and its bootstrap IP (multi-node rank address). */
  compute_network?: string;
  compute_address?: string;
}

export interface AgentRejectEvent {
  token_prefix: string;
  reason: string;
  remote: string;
  at: string;
}

// How the server logs in to a node over SSH. platform_key is the server's own
// key pair (see PlatformSSHKey); it needs nothing from the operator once the
// node authorized its public key.
export type SSHAuthMode = "platform_key" | "password" | "private_key" | "keyboard_interactive";

export interface SSHCredentialInfo {
  host: string;
  port: number;
  user: string;
  has_password: boolean;
  has_private_key: boolean;
  auth_mode: SSHAuthMode;
  probe?: SSHProbeResult;
  // Authorizing the platform key with the password or key just saved.
  platform_key?: CheckResult;
}

export interface PlatformSSHKey {
  public_key: string;
  fingerprint: string;
  created_at: string;
  authorize_command: string;
}

export interface SSHProbeResult {
  reachable: boolean;
  agent_running: boolean;
  agent_installed: boolean;
  // Server an active gpuworks-agent unit on the host was installed for, when
  // it is not this node's server.
  foreign_server_url?: string;
  detail?: string;
}

export interface RecoveryOutcome {
  node_uuid: string;
  phase?: string;
  action: string;
  probe: SSHProbeResult;
  recovered: boolean;
  skipped_reason?: string;
  error?: string;
}

export interface SSHPrecheckRequest {
  target: string;
  auth_mode?: SSHAuthMode;
  password?: string;
  private_key_pem?: string;
  server_url?: string;
  // Directory the agent will be installed into; the precheck inspects the
  // filesystem behind it. Omitted means the install default.
  install_dir?: string;
  connect_mode?: "agent_dial" | "server_dial";
  // server_dial only: where the agent will listen and the address the server
  // dials it at; the precheck probes that address from the server.
  server_dial_listen?: string;
  host_address?: string;
}

export interface CheckResult {
  name: string;
  status: "ok" | "warn" | "error";
  detail?: string;
}

export interface SSHPrecheckReport {
  target: string;
  user: string;
  host: string;
  port: number;
  checks: CheckResult[];
}

export interface SSHInstallRequest {
  target: string;
  auth_mode?: SSHAuthMode;
  name?: string;
  password?: string;
  private_key_pem?: string;
  server_url?: string;
  install_dir?: string;
  connect_mode?: "agent_dial" | "server_dial";
  udocker_bundle?: string;
  server_dial_listen?: string;
  // Start a Linux agent under nohup instead of the gpuworks-agent systemd
  // unit; omitted means systemd. macOS always uses launchd.
  no_systemd?: boolean;
  // Where the server reaches the host for server_dial (default: the SSH host).
  host_address?: string;
  // Confirms replacing a gpuworks agent already on the host; without it the
  // server answers 409 NODE_INSTALL_HOST_ALREADY_NODE or NODE_INSTALL_EXISTING_AGENT(_UNKNOWN/_SELF).
  replace_existing_agent?: boolean;
}

export interface SSHInstallReport {
  node_uuid: string;
  target: string;
  user: string;
  host: string;
  port: number;
  arch: string;
  binary_name: string;
  install_dir: string;
  server_url?: string;
  server_dial_address?: string;
  auth_used: SSHAuthMode | "instance_password" | "";
  platform_key_authorized: boolean;
  checks: CheckResult[];
}

/** One value the agent read from the vendor tool, named as the tool names it. An empty name is a sentence from an older agent. */
export interface GPUHealthField {
  name: string;
  value: string;
}

// Card health checks: GET /nodes/{uuid}/health-checks lays a node's card
// checks out as a table, the server deciding the columns, each cell's state
// and staleness, the worst state per card and whether a recheck is offered.
export type CardCheckState = "pass" | "warn" | "fail" | "skip" | "pending" | "na";
export interface CardHealthColumn {
  id: string;
  period_seconds: number;
}
export interface CardHealthCell {
  check: string;
  state: CardCheckState;
  reason?: string;
  checked_at?: string;
  duration_ms?: number;
  stale: boolean;
}
export interface CardHealthCard {
  ident: string;
  uuid?: string;
  pcie_bdf?: string;
  name?: string;
  index: number;
  worst: "pass" | "warn" | "fail";
  can_recheck: boolean;
  cells: CardHealthCell[];
}
export interface CardHealthChecks {
  node_uuid: string;
  online: boolean;
  reports: boolean;
  stale: boolean;
  received_at?: string;
  checks: CardHealthColumn[];
  cards: CardHealthCard[];
  summary: Record<string, number>;
}
// A stretch in which a check warned or failed (GET /nodes/{uuid}/health-records).
export interface CardHealthRecord {
  id: number;
  card_ident: string;
  card_index?: number;
  card_name?: string;
  pcie_bdf?: string;
  removed: boolean;
  check: string;
  level: "warn" | "fail";
  reason?: string;
  started_at: string;
  ended_at?: string;
  last_seen_at: string;
  duration_seconds: number;
  count: number;
  flapping: boolean;
  ongoing: boolean;
  first_evidence: GPUHealthField[];
  latest_evidence: GPUHealthField[];
}
export interface CardHealthRecords {
  records: CardHealthRecord[];
  total: number;
  page: number;
  page_size: number;
  retention_days: number;
}
export interface CardCheckRun {
  state: CardCheckState;
  reason?: string;
  at: string;
  duration_ms?: number;
  probe?: string;
  evidence: GPUHealthField[];
}
export interface CardCheckDetail {
  ident: string;
  check: string;
  last?: CardCheckRun;
  last_ran?: CardCheckRun;
  last_problem?: CardCheckRun;
  agent_started_at?: string;
}
export type CardRef = { pcie_bdf?: string; uuid?: string };
const cardQuery = (card?: CardRef) => (card?.pcie_bdf ? `pcie_bdf=${encodeURIComponent(card.pcie_bdf)}` : card?.uuid ? `uuid=${encodeURIComponent(card.uuid)}` : "");

/** Vendor commands for a card's health finding, derived by the server. */
export interface GPUHealthGuide {
  probe: string;
  recheck?: string;
  actions?: string[];
}

/** The mount a doctor path lives on; absent from agents that predate it. */
export interface DoctorMount {
  point: string;
  fs_type: string;
  source?: string;
  options?: string;
  network?: boolean;
  noexec?: boolean;
  read_only?: boolean;
  ephemeral?: boolean;
}

export interface DoctorDisk {
  kind: string;
  path: string;
  total_bytes: number;
  avail_bytes: number;
  mount?: DoctorMount;
}

export interface DoctorField {
  name: string;
  value: string;
}

export interface DoctorTarget {
  index: number;
  pcie_bdf?: string;
  evidence?: DoctorField[];
  probe?: string;
  recheck?: string;
  actions?: string[];
  processes?: { pid: number; name: string }[];
}

export interface DoctorFinding {
  code: string;
  level: "warning" | "error";
  targets?: DoctorTarget[];
  disk?: DoctorDisk;
  probe?: string;
  evidence?: DoctorField[];
  recheck?: string;
}

/** The node.doctor result. Agents that predate the structured form send only `report`. */
export interface NodeDoctorReport {
  report: string;
  host?: {
    hostname: string;
    os: string;
    arch: string;
    kernel: string;
    cpu_model: string;
    cpu_cores: number;
    mem_total_bytes: number;
    mem_avail_bytes: number;
    disks: DoctorDisk[];
  };
  runtimes?: { name: string; status: "ok" | "not_found" | "query_failed"; version?: string; selected?: boolean; command: string }[];
  accelerators?: {
    vendor: string;
    driver?: string;
    gpus: { index: number; name: string; memory_bytes: number; pcie_bdf?: string; numa_node: number; health_status: string; health_code?: string }[];
  };
  findings?: DoctorFinding[];
  facts?: DoctorFact[];
}

/** Whom a runtime's workloads run as; set on a runtime_identity fact. */
export interface DoctorRunIdentity {
  runtime: string;
  pending?: boolean;
  /** The dedicated account; absent means the agent's own user. */
  user?: string;
  uid: number;
  /** Why the dedicated account is not used, as the system reported it. */
  fallback?: string;
}

/**
 * A statement without a verdict. value is the agent's English wording; a fact
 * the console words itself carries the statement typed too.
 */
export interface DoctorFact {
  code: string;
  value: string;
  identity?: DoctorRunIdentity;
  /** The paths a network_filesystem fact covers, all on one mount. */
  disks?: DoctorDisk[];
}

export interface FSEntry {
  name: string;
  is_dir: boolean;
  size: number;
}

// FSListResponse is GET /nodes/{uuid}/fs: the directory listed, with its
// entries as a List alongside.
export interface FSListResponse extends List<FSEntry> {
  path: string;
}

export interface EngineParamOption {
  value: string;
  label: LocalizedText;
}

export interface RoutingPolicyOption {
  value: "least_request" | "prefix_affinity" | "failover";
  label: LocalizedText;
  description: LocalizedText;
}

export interface EngineParamSpec {
  key: string;
  flag: string;
  // The engine's negative form of a boolean parameter, when it has one
  // (vLLM's --no-enable-prefix-caching, SGLang's --disable-radix-cache). The
  // server owns both polarities; the form only submits the value.
  flag_off?: string;
  // Accepted spellings mapped to the canonical value, so a catalog that writes
  // `--dtype half` and a form that submits `float16` are one value. The server
  // resolves them; the form never rewrites a value of its own.
  aliases?: Record<string, string>;
  label: LocalizedText;
  type: string;
  control?:
    | "segmented"
    | "slider"
    | "switch"
    | "number"
    | "select"
    | "combobox"
    | "text"
    | "json"
    | "path"
    | "tier"
    | string;
  group?:
    | "model"
    | "multimodal"
    | "performance"
    | "speculative"
    | "performance_advanced"
    | string;
  // Where the deploy form shows the parameter ("quick" surface, "expert" fold,
  // "hidden" never) and the order inside that surface. The dictionary owns the
  // layout; the form holds no key lists of its own.
  layout?: "quick" | "expert" | "hidden" | string;
  order?: number;
  enum?: string[];
  options?: EngineParamOption[];
  // The dictionary's default for this key. A boolean without one is
  // three-state on the form: "auto" leaves it unstated, so no flag is sent and
  // the engine keeps its own default.
  default?: unknown;
  min?: number;
  max?: number;
  step?: number;
  placeholder?: LocalizedText;
  help?: LocalizedText;
  engines?: string[];
  advanced?: boolean;
  automatic?: boolean;
  /** Dictionary key whose enabled tier makes this parameter effective. */
  depends_on?: string;
  multimodal_only?: boolean;
}

export interface EngineProfile {
  name: string;
  default_image: string;
  health_path: string;
  shm_bytes: number;
  sleep: { supported: boolean; levels?: string[] };
  metric_prefix: string;
  param_dict?: Record<string, string>;
  param_specs?: EngineParamSpec[];
  quick_form_keys?: string[];
  // The lowest kernel release on which this engine's KV disk tier starts;
  // absent when the engine states no floor. Clients compare it against the
  // kernel a node reports instead of carrying a release number of their own.
  kv_disk_min_kernel?: string;
  // The PD modes this engine can run, with each mode's rules (ADR 0028's
  // pdmode catalog); empty or absent when the engine has none. The deploy
  // form and the scale dialog read modes, policy options, defaults and
  // limits from here (pages/deploy/deployPd.ts) instead of restating them.
  pd_modes?: PdModeProfile[];
  multi_node_supported?: boolean;
}

// WakeRefusal is one group of sleepers a wake could not put back: the cards
// that cannot take them, how far short they are and who holds the memory.
// held_outside says a process outside the platform holds a card, which no
// sleep order of ours can free.
export interface WakeRefusal {
  instances: string[];
  node?: string;
  gpu_indexes?: number[];
  occupants?: string[];
  shortfall_gib?: string;
  held_outside?: boolean;
}

// WakeResult is a wake order's answer; a partial wake lists what it woke and
// the sleepers whose cards could not take them back. A wake that waits for
// room (wait or yield) answers 202 with status "waiting", what it woke at
// once, what waits (waiting) and the deployments put to sleep to make room
// (yielded); 200 "waking" when everything fit.
export interface WakeResult {
  status: string;
  woken?: string[];
  refused?: WakeRefusal[];
  waiting?: string[];
  yielded?: string[];
  // yielded_held are the yielded occupants that never yield by policy: a
  // request does not wake them. A request wakes the other yielded ones.
  yielded_held?: string[];
}

// wakeOnRequestQuery is a sleep order's wake_on_request parameter; none
// leaves the choice to the server's default.
function wakeOnRequestQuery(wakeOnRequest?: boolean): string {
  return wakeOnRequest === undefined ? "" : `&wake_on_request=${wakeOnRequest}`;
}

// WakeOptions are the ways a wake order may proceed: role limits it to one
// PD role, partial wakes what fits, wait leaves the rest waiting for room,
// yield first puts the occupants of the needed cards to sleep (at yieldLevel,
// 1 unless the server must fall back to 2).
export interface WakeOptions {
  role?: string;
  partial?: boolean;
  wait?: boolean;
  yield?: boolean;
  yieldLevel?: 1 | 2;
}

// PdModeProfile is one PD mode as the server's catalog states it.
export interface PdModeProfile {
  name: string;
  // Accelerator vendors the mode runs on.
  vendors: string[];
  roles: PdRoleName[];
  // Roles a running deployment may shrink to no instances.
  scalable_to_zero: PdRoleName[];
  // Direction rule between the two roles' TP; the server enforces it.
  tp_rule: string;
  max_units_per_role: number;
  // Allowed policy values, the default first. kv_load_failures is empty when
  // the mode's engine has no such setting.
  fallbacks: string[];
  kv_load_failures: string[];
  unverified?: boolean;
  // The first request after a start compiles kernels and takes minutes.
  first_request_compiles?: boolean;
}

export interface Health {
  status: string;
  version: string;
  nodes: number;
  sessions: number;
}

export interface ModelNodePresence {
  model_id?: number;
  model_name?: string;
  preparation?: { phase: string; reason?: string; total_known: boolean; idle_seconds: number; eta_seconds?: number; retryable?: boolean };

  node_uuid: string;
  // Present so a client never has to render a raw uuid; the uuid remains the
  // identifier.
  node_name?: string;
  source_node_name?: string;
  state: string;
  path: string;
  revision?: string;
  progress: number;
  size_bytes: number;
  downloaded: number;
  transferred_bytes: number;
  rate_bps: number;
  rate_limit_kib_s?: number;
  last_rate_limit_kib_s?: number;
  acquisition: "auto" | "peer" | "download" | "registered" | string;
  transfer_id?: string;
  source_node_uuid?: string;
  message?: string;
  /** The storage the files are in; cache marks an nfs_cache storage's copy on this node. */
  storage_id?: number;
  storage_name?: string;
  storage_type?: string;
  cache?: boolean;
  last_used_at?: string;
  /** These files may be deleted from the node (not in a read-only local storage); absent means not. */
  deletable?: boolean;
}

export interface Model {
  id: number;
  source: string;
  source_id: string;
  /** Set when PATCH /models/{id} may change source_id (a local or URL model). */
  source_id_editable?: boolean;
  /** The HTTP directory a source="url" model is served from; absent for every other source. */
  source_url?: string;
  revision: string;
  artifact_subfolder?: string;
  name: string;
  arch: string;
  quant_label: string;
  weight_gib: number;
  operations?: InferenceAPIOperation[];
  input_modalities?: Array<"text" | "image" | "audio" | "video" | string>;
  readme?: string;
  logo_path?: string;
  origin_created_at?: string;
  origin_updated_at?: string;
  context_length?: number;
  precision?: string;
  created_at: string;
  nodes?: ModelNodePresence[];
  /** The model's single copy in each shared storage that holds it. */
  storages?: ModelStoragePresence[];
  /** On a registration's answer only: why the management node read no metadata. */
  metadata_notice?: ProductMessage;
}

export interface LocalModelPreview {
  node_uuid: string;
  path: string;
  source_id: string;
  name: string;
  arch?: string;
  precision?: string;
  context_length?: number;
  weight_bytes: number;
  operations?: InferenceAPIOperation[];
  input_modalities?: string[];
  modalities?: string[];
  config_raw?: string;
  readme?: string;
  readme_truncated?: boolean;
}

export interface URLModelPreview {
  source_url: string;
  source_id: string;
  name: string;
  arch?: string;
  precision?: string;
  context_length?: number;
  weight_bytes: number;
  files_count: number;
  input_modalities?: string[];
}

export interface ImageNode {
  node_uuid: string;
  /** The node's display name and state, so the image page needs no node list. */
  node_name: string;
  node_state: string;
  state: string;
  size_bytes?: number;
  transferred_bytes?: number;
  rate_bps?: number;
  transfer_id?: string;
  source_node_uuid?: string;
  source_node_name?: string;
  source?: "peer" | "offline" | "quay" | "huaweicloud" | "direct" | string;
  rate_limit_kib_s?: number;
  last_rate_limit_kib_s?: number;
  message?: string;
  /** The image ID the node's runtime reported after the last pull ("sha256:…"); omitted when unknown. */
  image_digest_id?: string;
  /** When this row last changed (RFC 3339), so a rolling tag shows how old the node's copy is. */
  updated_at: string;
}

export interface ImageCatalogEntry {
  id: number;
  series_id?: number;
  purpose?: "inference" | string;
  name: string;
  engine: string;
  engine_version?: string;
  framework: string;
  framework_version?: string;
  driver_requirement?: string;
  arch?: string;
  image_ref: string;
  source: string;
  deprecated: boolean;
  enabled: boolean;
  notes?: string;
  size_bytes?: number;
  local_runtime_size_bytes?: number;
  local_size_known_nodes?: number;
  /** Deployments, live instances and models that name this image; a deprecated system image stays while this is above zero. */
  reference_count: number;
  /** Nodes holding this version and nodes in the fleet: every response carries them. */
  present_nodes: number;
  total_nodes: number;
  /**
   * One row per node. Only the series detail (GET /images/{series_id}) and the
   * responses of per-version writes carry it; the catalog-wide version list
   * (GET /images/versions) never does.
   */
  nodes?: ImageNode[];
  deprecated_since?: string;
  registry_sources?: string[];
  rolling_tag?: boolean;
  upstream_gone?: boolean;
  effective_enabled?: boolean;
}

/**
 * One engine an image catalog entry may declare (GET /images/engines), with
 * its product name. The server's order is the order the image form offers.
 */
export interface ImageEngineOption {
  name: string;
  label: string;
}

/**
 * An image's OCI config as one node's runtime reports it. It is read on
 * request and shown as a runtime fact; the catalog does not store it.
 */
export interface ImageInspection {
  image_ref: string;
  image_id?: string;
  entrypoint?: string[];
  cmd?: string[];
  env?: Record<string, string>;
  volumes?: string[];
}

/** What the server could learn about a remote image archive without downloading it. */
export interface ImageArchivePreview {
  url: string;
  size_bytes: number;
  repo_tags: string[];
  previewable: boolean;
}

export interface ImageNodeSyncResult {
  state: string;
  transfer_id?: string;
  message?: string;
}

/** Extra fields the "url" image source needs; ignored by every other source. */
export interface ImageFetchOptions {
  archive_url?: string;
  relay?: boolean;
  source_node?: string;
  rate_limit_kib_s?: number;
  /** Pull again even when the node already holds the image (a rolling tag moved upstream). */
  force?: boolean;
}

/**
 * imageFetchBody builds the sync request. No source means "pull through the
 * pull settings": the body is empty (or only {force:true}). A manual copy from
 * a node names source_node; an archive URL import names source "url". The
 * client never names a registry.
 */
export function imageFetchBody(source?: string, fetch?: ImageFetchOptions) {
  if (fetch?.source_node) return { source_node: fetch.source_node, rate_limit_kib_s: fetch.rate_limit_kib_s ?? 0 };
  const force = fetch?.force ? { force: true } : {};
  if (source !== "url") return { ...force };
  return {
    source,
    archive_url: fetch?.archive_url || "",
    relay: Boolean(fetch?.relay),
    ...force,
  };
}

export interface ImageSeries {
  id: number;
  name: string;
  engine?: string;
  purpose: "inference" | string;
  framework?: string;
  common_args?: Array<{ flag: string; value?: string; description?: string }>;
  common_env?: Array<{ key: string; value?: string; description?: string }>;
  source: string;
  notes?: string;
  enabled: boolean;
  version_count: number;
  present_nodes: number;
  total_nodes: number;
  versions?: ImageCatalogEntry[];
}

/** The accelerator vendors a Kubernetes cluster's nodes can have. */
export type KubeVendor = "nvidia" | "ascend" | "hygon" | "ppu";

/** A Kubernetes cluster whose GPU nodes join through one DaemonSet (dto.KubeCluster). */
export interface KubeCluster {
  id: number;
  uuid: string;
  name: string;
  namespace: string;
  server_url: string;
  /** The entry's own image; empty runs the published image from agent_image_source. */
  agent_image: string;
  /** The registry the published agent image is pulled from (dto.KubeAgentImageSource id). */
  agent_image_source: string;
  /** The image the entry's manifest names. */
  effective_agent_image: string;
  node_selector: string;
  /** Accelerator vendor of the cluster's nodes: nvidia, ascend, hygon or ppu. */
  vendor: KubeVendor;
  /** The agent's directory on the host. */
  state_dir: string;
  /**
   * Empty for any vendor but nvidia (devices come through the agent's device plugin); for nvidia, empty uses the
   * cluster's "nvidia" RuntimeClass when it has one and the node's default runtime otherwise.
   */
  runtime_class: string;
  /** A docker-registry Secret in the namespace the agent image is pulled with; empty pulls without one. */
  image_pull_secret: string;
  /** Empty means the agents find the socket themselves. */
  containerd_socket: string;
  /** API server address the agents use instead of the "kubernetes" Service; empty uses the Service. */
  api_server: string;
  engine_ports: string;
  /** Seconds a borrowed card may sit idle before it goes back; -1 keeps it. */
  idle_return_seconds: number;
  /** Empty until the first node enrolled. */
  cluster_uid: string;
  revoked: boolean;
  node_count: number;
  created_at: string;
  /** Carries the enrollment token; administrators only. */
  apply_command?: string;
  /** No node joined for longer than the dictionary's stalled_after_seconds: worth troubleshooting. */
  stalled: boolean;
  /** The cloud node pools a cloud entry holds, ascending; empty for an entry made by hand. */
  pool_ids: string[];
  /** Whether the server can apply the manifest to the source cluster, or delete it from there. */
  can_apply: boolean;
  can_remove: boolean;
  /** Why not: no_source (made by hand), private_api (the cloud says the API server has no public address), revoked (apply only). */
  operation_blocked?: "no_source" | "private_api" | "revoked";
  /** The cloud account cluster the entry was made from; gpuworks can then apply the manifest itself. */
  source?: KubeClusterSource;
}

/** What the cluster form takes from the server (dto.KubeDefaults); every value is a hint, a field left empty takes it. */
export interface KubeDefaults {
  vendor: KubeVendor;
  namespace: string;
  state_dir: string;
  engine_ports: string;
  idle_return_seconds: number;
  /** The idle_return_seconds value that keeps borrowed cards. */
  idle_return_keep: number;
  agent_image: string;
  stalled_after_seconds: number;
  /** The vendors a cluster can be connected for, the default first. */
  vendors: KubeVendorInfo[];
  /** The registry an entry made by hand pulls the published agent image from. */
  agent_image_source: string;
  /** The registries the agent image is published to, the default first. */
  agent_image_sources: KubeAgentImageSource[];
}

/** A registry the agent image is published to (dto.KubeAgentImageSource). */
export interface KubeAgentImageSource {
  id: string;
  /** The image there, at the server's runtime revision. */
  image: string;
}

export interface KubeVendorInfo {
  id: KubeVendor;
  /** The cluster may name a RuntimeClass for this vendor; for the others the server ignores one. */
  runtime_class: boolean;
}

/** Where a cluster entry came from (dto.KubeClusterSource). */
export interface KubeClusterSource {
  provider_id: number;
  /** Empty when the account is no longer listed. */
  provider_name: string;
  provider_kind: string;
  cluster_id: string;
}

/** What an apply or removal did to each object of the manifest (dto.KubeClusterOperation). */
export interface KubeClusterOperation {
  dry_run: boolean;
  results: KubeObjectResult[];
  /** Present when the operation failed: KUBE_CLUSTER_OPERATION_FAILED. */
  code?: string;
  error?: string;
  params?: { step: "kubeconfig" | "api_server"; reason: string };
}

export type KubeObjectAction = "create" | "update" | "delete" | "absent" | "kept" | "failed" | "not_tried" | "dependent";

export interface KubeObjectResult {
  kind: string;
  name: string;
  namespace?: string;
  action: KubeObjectAction;
  reason?: string;
  error?: string;
}

/** Creates a cluster; an omitted optional field takes the server's default (dto.KubeClusterRequest). */
export interface KubeClusterRequest {
  name: string;
  server_url: string;
  namespace?: string;
  agent_image?: string;
  /** Omitted: Docker Hub for an entry made by hand, the registry its region reaches for one from a cloud cluster. */
  agent_image_source?: string;
  node_selector?: string;
  /** Omitted means nvidia. */
  vendor?: KubeVendor;
  /** The agent's directory on the host; omitted means /opt/gpuworks. */
  state_dir?: string;
  /** NVIDIA only; omitted lets the agents decide on each node. */
  runtime_class?: string;
  /** Omitted pulls the agent image without credentials. */
  image_pull_secret?: string;
  /** Omitted means the agents detect it (containerd, k3s, RKE2, microk8s). */
  containerd_socket?: string;
  /** "https://host:port"; omitted means the cluster's "kubernetes" Service. */
  api_server?: string;
  engine_ports?: string;
  idle_return_seconds?: number;
  /** The cloud account cluster the entry is made from; both or neither. */
  provider_id?: number;
  provider_cluster_id?: string;
  /** The cluster's GPU node pools the entry holds (required with a source, one vendor). */
  provider_pool_ids?: string[];
}

// Every collection endpoint answers an envelope, never a bare array
// (docs/reference/api-conventions.md). List is a whole, unpaged collection:
// total equals items.length.
export interface List<T> {
  items: T[];
  total: number;
}

// CursorPage is one page of a cursor-paged collection (the event timelines):
// the next page is asked for with a cursor taken from the last item, and no
// total is counted.
export interface CursorPage<T> {
  items: T[];
}

// One page of a server-paged list (GET /deployments, GET /nodes with page
// parameters). total counts every row the filters match.
export interface ListPage<T> extends List<T> {
  page: number;
  page_size: number;
}

/** Where a model's files are, summed: GET /models/{id} lists each location. */
export interface ModelPresenceSummary {
  ready_nodes: number;
  cached_nodes: number;
  ready_storages: number;
  in_flight: number;
  /** Mean progress of the transfers in flight, 0–100. */
  in_flight_percent: number;
  failed: number;
}

/** A model's own fields, without the per-location presence and README a list row leaves out. */
export type ModelCore = Omit<Model, "nodes" | "storages" | "readme">;

/** A model as a list row: no README and no per-location presence, which GET /models/{id} carries. */
export type ModelListItem = ModelCore & { presence: ModelPresenceSummary };

/** GET /inference-templates/weight-models: the registered models serving a spec's weight sources. */
export interface TemplateWeightModels {
  /** Every model serving at least one source, newest first, each once. */
  models: ModelListItem[];
  /** The spec's sources in order; reuse_model_id is the model a choice of that source reuses. */
  sources: Array<WeightSource & { reuse_model_id?: number }>;
}

/** One page of GET /models?page=…, with the summary of every model. */
export interface ModelPage extends ListPage<ModelListItem> {
  summary: { registered: number; ready_files: number; sources: number; weight_gib: number };
}

export interface ListQuery {
  page: number;
  page_size: number;
  /** Matched as a case-insensitive substring of the row's names. */
  q?: string;
  /** Alternatives; empty matches every state. */
  state?: string[];
  /** key=value pairs: values of one key are alternatives, keys must all match. */
  label?: string[];
  /** A column key, ascending; "-" before it for descending. */
  sort?: string;
  /** A list's own column filters (source, vendor, allocation_state…): each lists alternatives. */
  extra?: Record<string, string[]>;
}

export function listQueryString(query: ListQuery): string {
  const params = new URLSearchParams({ page: String(query.page), page_size: String(query.page_size) });
  const q = query.q?.trim();
  if (q) params.set("q", q);
  if (query.state?.length) params.set("state", query.state.join(","));
  for (const pair of query.label ?? []) params.append("label", pair);
  if (query.sort) params.set("sort", query.sort);
  for (const [name, values] of Object.entries(query.extra ?? {})) if (values.length) params.set(name, values.join(","));
  return params.toString();
}

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

export type DeploymentPage = ListPage<DeploymentListItem>;


/** A node page row omits topology and path usage (GET /nodes/{uuid} has them). */
export interface NodePage extends ListPage<Node> {
  /** Every node, whatever the filters; total counts the matches. */
  all: number;
  /** Every user label key=value across all nodes, whatever the filters. */
  labels: Array<{ key: string; values: string[] }>;
}

/** What GET /nodes/facets says about the nodes its filters match. */
export interface NodeFacets {
  total: number;
  labels: Array<{ key: string; values: Array<{ value: string; nodes: number }> }>;
  /** Generated labels except the node identity, with the server's display text. */
  system_labels: Array<{ key: string; values: Array<{ value: string; display: string; display_code?: string; nodes: number }> }>;
  /**
   * Distinct accelerator models, each with how many matched nodes have one;
   * model is the card's gpuworks.ai/gpu-model label value.
   */
  gpus: NodeGPUCount[];
}

/** One reported accelerator identity aggregated by GET /nodes/facets. */
export interface NodeGPUCount {
  vendor: string;
  name: string;
  cc: string;
  arch_id?: string;
  model: string;
  nodes: number;
  cards: number;
  memory_bytes: number;
}

/** The filters of GET /nodes/facets: a list query without the page. */
export type NodeFilter = Omit<ListQuery, "page" | "page_size">;

export function nodeFilterString(filter: NodeFilter): string {
  const params = new URLSearchParams();
  const q = filter.q?.trim();
  if (q) params.set("q", q);
  if (filter.state?.length) params.set("state", filter.state.join(","));
  for (const pair of filter.label ?? []) params.append("label", pair);
  return params.toString();
}

export interface CapacityGroup {
  key: string;
  unlabeled?: boolean;
  nodes: number;
  gpus: number;
  vram_gib: number;
  unusable_gpus: number;
}

/** The fleet's cards counted once each, in the scheduling overview's states. */
export interface SchedulingCardSummary {
  nodes: number;
  online_nodes: number;
  cards: number;
  awake: number;
  asleep: number;
  free: number;
  held_outside: number;
  unhealthy: number;
  offline: number;
  contention: { waiting_wakes: number; held_requests: number; yields_last_hour: number };
}

export interface DashboardSummary {
  current_metrics?: {
    utilization_pct: number | null;
    temperature_c: number | null;
    power_watts: number | null;
    memory_used_bytes: number | null;
    /** VRAM used and owned by the online nodes' cards, summed over the cards with both samples fresh; null when none has. */
    vram_used_bytes?: number | null;
    vram_total_bytes?: number | null;
  };
  nodes: {
    total: number;
    online: number;
    offline: number;
    connecting: number;
    gpus: number;
    unusable_gpus: number;
    vram_gib: number;
    memory_gib: number;
  };
  /** Every deployment falls in exactly one bucket; other is the ones in no named bucket. */
  deployments: { total: number; running: number; warming: number; sleeping?: number; pending?: number; other?: number; sleepable: number };
  /** The scheduling overview's card summary; null when it cannot be built. */
  /** allocated = cards a deployment holds (awake + asleep), counted by the server. */
  cards?: (SchedulingCardSummary & { allocated: number }) | null;
  tasks?: { batches: { active: number; waiting: number } | null; benchmarks: number };
  by_vendor: CapacityGroup[];
  label_keys: string[];
  label_key: string;
  by_label: CapacityGroup[];
}

export type InferenceTemplatePage = ListPage<InferenceTemplate>;
export interface InferenceTemplateIndex {
  hardware?: DetectedTemplateChip[];
  items: InferenceTemplateIndexItem[];
}
export interface InferenceTemplateOrganizations {
  items: Array<{ organization: string; models: number }>;
}
export interface InferenceTemplateIndexItem {
  id: number;
  model_id: string;
  display_title?: string;
  source: "upstream" | "user" | string;
  engines: string[];
  scenarios: string[];
  profiles: InferenceProfileSummary[];
}
export interface InferenceProfileSummary {
  detected_compatible?: boolean;
  source?: "upstream" | "user" | string;
  name: string;
  display_name_zh_cn?: string;
  display_name_en?: string;
  scenario?: string;
  engine: string;
  image?: string;
  quantization?: string;
  weight_bytes?: number;
  weight_repo_ids?: string[];
  gpus: GPUPredicate;
}
export interface InferenceTemplate {
  detected_support?: { preferred_engine: string; warnings: Array<{ reason: string; quantization?: string; alternative_available: boolean }> };
  id: number;
  model_id: string;
  display_title?: string;
  parameter_count?: string;
  architecture?: string;
  input_modalities?: Array<"text" | "image" | "audio" | "video" | string>;
  readme_zh_cn?: string;
  readme_en?: string;
  readme_zh_cn_content?: string;
  readme_en_content?: string;
  context_length?: number;
  release_date?: string;
  source: "upstream" | "user" | string;
  unsupported?: InferenceTemplateUnsupported[];
  specs: InferenceSpec[];
}
export interface InferenceSpec {
  id: number;
  source: "upstream" | "user" | string;
  name: string;
  /** Human-written names; the page shows the one for its locale (specDisplayName). */
  display_name_zh_cn?: string;
  display_name_en?: string;
  scenario?: string;
  engine: string;
  image: string;
  /** Further references the template offers for pulling `image`, preferred first. */
  image_sources?: string[];
  quantization?: string;
  weight_sources: WeightSource[];
  weight_bytes?: number;
  runtime_weight_bytes?: number;
  /** /dev/shm size in bytes the spec's engine needs; absent when the spec states none. */
  shm_bytes?: number;
  /** The spec's container starts with unlimited locked memory (--ulimit memlock=-1:-1); absent when false. */
  memlock_unlimited?: boolean;
  gpus: GPUPredicate;
  args?: string[];
  env?: Record<string, string>;
  pre_start_argv?: string[];
  pre_start_script?: string;
  assets?: Array<{
    file: string;
    sha256: string;
    size_bytes?: number;
    unpack_to?: string;
  }>;
  entrypoint?: string[];
  // Keys whose default this spec's launch leaves to the engine.
  engine_decides_param_keys?: string[];
  placement?: { language: string; resolver: string };
  api?: import("./inferenceApi").InferenceAPIContract;
  components?: Array<{
    name: string;
    role: string;
    model_id?: string;
    mount_path: string;
    weight_bytes: number;
    weights: { quantization?: string; sources: WeightSource[] };
  }>;
}
export interface WeightSource {
  provider: string;
  repo_id: string;
  revision?: string;
  subfolder?: string;
}
export interface GPUPredicate {
  vendor: string;
  cc?: string;
  name?: string;
  arch_id?: string;
}
export interface InferenceTemplateUnsupported {
  gpus: GPUPredicate;
  quantization?: string;
  reason: string;
}
export interface InferenceTemplateSummary {
  deployable: number;
  models: number;
}
export interface InferenceTemplateChip {
  vendor: string;
  name: string;
  cc?: number;
  category: string;
  architecture: string;
  arch_id?: string;
}
export interface DetectedTemplateChip {
  vendor: string;
  name: string;
  cc?: string;
  arch_id?: string;
}
export interface AuthUser {
  id: number;
  username: string;
  role: string;
  /** The length a new password must have; /auth/me only. */
  password_policy?: { min_length: number; max_length: number };
}

/** One built-in (or administrator-configured) community repository card. */
export interface CommunityRepo {
  access_host?: string;
  repo: string;
  registry: "docker-hub" | "quay" | "oci" | string;
  engine: string;
  framework: string;
  /** Further repository paths shown on the same card; each lists its own tags. */
  siblings?: string[];
}

export interface CommunityTag {
  tag: string;
  engine?: string;
  engine_version?: string;
  framework?: string;
  framework_version?: string;
  arch?: string;
  rolling_tag?: boolean;
  upstream_gone?: boolean;
  /** When the registry last saw the tag move (RFC 3339); absent for plain OCI registries. */
  last_updated?: string;
}

/** Every tag the upstream registry lists for one repository, unfiltered. */
export interface CommunityRepoBrowse {
  repo: string;
  registry: string;
  engine: string;
  framework: string;
  siblings?: string[];
  tags: CommunityTag[];
}

/** A manifest item the import left alone because a community or custom row already holds its name. */
export interface ImageImportConflict {
  kind: string;
  name: string;
  reason: string;
}

/** One distribution source's answer to an update check (ADR 0040). */
export interface UpdateSourceStatus {
  url: string;
  ok: boolean;
  latency_ms: number;
  error?: string;
}

/** An official catalog in force next to the published one. */
export interface CatalogUpdate {
  current: string;
  latest?: string;
  update_available: boolean;
  /** True when this server can read the catalog; check_error explains a failed check. */
  compatible: boolean;
  /** Present when the catalog could not be checked; other updates remain usable. */
  check_error?: ProductMessage;
}

/** GET /api/v1/updates: what the distribution channel offers next to what this server runs. */
export interface UpdateStatus {
  /** nightly (滚动预览版) or release (正式发布版). */
  channel: string;
  /** The console can switch the channel (the server keeps it in server.yaml). */
  channel_editable: boolean;
  /**
   * Whether the server checks the channel by itself and tells it the fleet's
   * size and makeup (node count, GPU models, agent versions) while doing so.
   * False: it asks only on "check for updates", and says nothing then.
   */
  auto_check: boolean;
  checked_at: string;
  /** False when no source served a valid index; sources say why. */
  available: boolean;
  // Why no source served the channel's index when available is false:
  // the channel is reachable but empty, a source served an index that did
  // not verify, or none could be reached.
  unavailable_reason?: "unpublished" | "invalid" | "unreachable";
  source?: string;
  sources: UpdateSourceStatus[];
  server: {
    current_build?: ServerBuildInfo;
    current: string;
    latest?: string;
    built_at?: string;
    update_available: boolean;
    installable: boolean;
    /** True when the console can upgrade this server in place. */
    self_upgrade: boolean;
    /** Why it cannot: no_service_manager, not_installed, not_installed_binary or not_writable. */
    self_upgrade_blocker?: string;
    /** The installation directory the server runs from, when it was installed. */
    install_dir?: string;
  };
  image_catalog: CatalogUpdate;
  template_catalog: CatalogUpdate;
}

/** GET /api/v1/usage-report: the user experience improvement program. */
export interface UsageReportStatus {
  /** usage_report in server.yaml; absent means on. */
  enabled: boolean;
  /** The console can switch it (the server keeps it in server.yaml). */
  editable: boolean;
  /** The UTC day (YYYY-MM-DD) of the last report delivered. */
  last_day?: string;
  /** The latest delivery attempt since the server started. */
  last_attempt?: { at: string; day: string; ok: boolean; error?: string };
  next_due_at?: string;
}

/** One of the publisher's messages for the header's notice bell, with its own text per locale. */
export interface NoticeItem {
  id: string;
  severity: "critical" | "important" | "info";
  kind: "security" | "release" | "event";
  title?: Record<string, string>;
  body?: Record<string, string>;
  url?: string;
  published_at: string;
  read: boolean;
}

/** GET /api/v1/notices. */
export interface NoticeList {
  notices: NoticeItem[];
  unread: number;
}

/** A console upgrade that has swapped the binary and is restarting into `to`. */
export interface ServerUpgradeStarted {
  from: string;
  to: string;
  backup: string;
}

/** The result of applying an official inference template catalog. */
export interface OfficialInferenceCatalogResult {
  models: number;
  package_digest: string;
  version: string;
}

/** What "check for updates" and a manifest upload changed in the system catalog. */
export interface ImageCatalogSyncResult {
  catalog_version: string;
  created: number;
  updated: number;
  deleted: number;
  deprecated: number;
  skipped: ImageImportConflict[];
  series: ImageSeries[];
}

export interface StorageNodeUsage {
  node_uuid: string;
  hostname: string;
  capacity_known: boolean;
  total_bytes: number;
  available_bytes: number;
  used_bytes: number;
  model_bytes: number;
  image_bytes: number;
  other_bytes: number;
  path_usage?: Array<{
    kind: string;
    /** Names the storage a storage or storage_cache entry measures. */
    storage_id?: number;
    path: string;
    total_bytes: number;
    available_bytes: number;
  }>;
}

/** The compute network a multi-node rank started on, recorded at launch. */
export interface InstanceComputeNetwork {
  name: string;
  management: boolean;
  type: "host_ethernet" | "host_rdma" | "accelerator_rdma" | "accelerator_hccs";
  interface: string;
  ip: string;
  link_layer?: "infiniband" | "roce" | "hccs";
}

// --- VRAM breakdown -------------------------------------------------------
export interface Breakdown {
  weight_bytes: number;
  per_dtype?: Record<string, number>;
  kv_bytes_per_token: number;
  kv_total_bytes: number;
  // One figure, not a decomposition: gpuworks does not model an engine's
  // internals, so the reserve is a single conservative constant.
  reserve_bytes: number;
  mm_encoder_bytes: number;
  per_gpu_bytes: number;
  startup_fixed_bytes?: number;
  total_bytes: number;
  actual_total_bytes?: number;
  grade: string;
  tensor_parallel: number;
  max_model_len: number;
  source?: "estimate" | "measured";
  gpu_memory_utilization?: number;
  capacity_bytes?: number;
  kv_pool_bytes?: number;
  estimated_kv_tokens?: number;
  kv_estimate_confidence?: string;
  kv_estimate_reason?: string;
  fits: boolean;
  max_concurrency?: number;
  vram_limit_bytes?: number;
}
export interface Measured {
  ModelLoadingSeconds?: number;
  CompilationSeconds?: number;
  GraphCaptureSeconds?: number;
  StartupSeconds?: number;
  ModelWeightsGiB: number;
  KVCacheGiB: number;
  KVCacheTokens: number;
  GraphCaptureGiB: number;
  MaxConcurrency: number;
  NumGPUBlocks: number;
  Found: boolean;
}

export interface RuntimeMount {
  source?: string;
  volume?: string;
  target: string;
  read_only?: boolean;
  size_hint?: string;
  purpose?: string;
}

export interface StartupEstimate {
  weight_bytes: number;
  method: string;
  estimated_seconds?: number;
  reference_mib_per_sec?: number;
  initialization_seconds?: number;
}

export interface Instance {
  pre_start_argv?: string[];
  pre_start_script?: string;
  start_timeout_sec?: number;
  start_timeout_source?: string;
  start_timeout_conflict?: boolean;
  engine_start_timeout_sec?: number;
  startup_estimate?: StartupEstimate;
  uuid: string;
  deployment_id: number;
  node_uuid: string;
  state: string;
  gpu_indexes: number[];
  // The dGPU memory limit on each of its GPUs; absent for whole cards.
  vram_limit_bytes?: number;
  ports: number[];
  api_port: number;
  image: string;
  image_digest_id?: string;
  message: string;
  runtime_failure?: { code: string; params?: Record<string, unknown>; message: string };
  container_id?: string;
  phase?: string;
  progress?: number;
  bytes_done?: number;
  bytes_total?: number;
  rate_bps?: number;
  model_progress?: {
    state: string;
    progress: number;
    size_bytes: number;
    downloaded: number;
    transferred_bytes: number;
    rate_bps: number;
    message?: string;
  };
  sleep_level: number;
  // The level the instance is ordered to (0 awake); it differs from
  // sleep_level while the order is being carried out.
  desired_sleep?: number;
  // The server's verdicts about the instance, which the UI shows as they are:
  // asleep (the gateway sends it nothing because of sleep), the replica it
  // fills (absent when it fills none), and whether it takes a sleep or wake
  // order of its own.
  asleep?: boolean;
  // sleep_held: an operator's or a schedule's sleep holds this instance, so
  // a request does not wake it.
  sleep_held?: boolean;
  replica?: { role?: string; index: number };
  can_sleep_alone?: boolean;
  can_wake_alone?: boolean;
  // A rank of a multi-node replica: rank 0 leads and serves, the rest are
  // headless followers (dist_role, normalized by the server for old rows).
  // dist_group + dist_attempt identify the generation the ranks were
  // launched as; nnodes is the replica's rank count.
  node_rank?: number;
  headless?: boolean;
  nnodes?: number;
  dist_role?: "leader" | "follower" | string;
  dist_group?: string;
  dist_attempt?: number;
  schedule?: { explain?: any; breakdown?: Breakdown; network?: { compute?: InstanceComputeNetwork }; unit?: { key?: string } };
  measured?: Measured;
  entrypoint?: string[];
  argv?: string[];
  env?: Record<string, string>;
  docker_args?: string[];
  /** Size of the container's /dev/shm tmpfs, in bytes. */
  shm_bytes?: number;
  mounts?: RuntimeMount[];
  command_mode?: string;
  model_mount_path?: string;
  // The model files the instance reads, as recorded when it launched: the
  // storage each is in (absent once that storage is gone) and the host path.
  model_files?: InstanceModelFile[];
  // role is the instance's PD role; absent outside PD deployments.
  role?: PdRoleName | string;
}

export interface InstanceModelFile {
  model_id: number;
  storage_id?: number;
  storage_name?: string;
  storage_type?: string;
  path: string;
}

export interface LaunchFactsIntent {
  deployment_id: number;
  name: string;
  engine: string;
  image?: string;
  served_name?: string;
  desired_state: string;
  replicas: number;
  env: Record<string, string>;
  sleepable: boolean;
  desired_sleep: number;
  vram_limit_bytes: number;
  sm_limit_pct: number;
  partition_backend?: string;
  nnodes?: number;
  node_rank?: number;
}

export interface LaunchFactsMount {
  source?: string;
  volume?: string;
  target: string;
  read_only?: boolean;
  size_hint?: string;
  purpose?: string;
}

export interface LaunchFactsResolved {
  command_mode?: string;
  entrypoint?: string[];
  argv: string[];
  pre_start_argv?: string[];
  pre_start_script?: string;
  env?: Record<string, string>;
  docker_args?: string[];
  mounts?: LaunchFactsMount[];
  model_mount: string;
  health_path?: string;
  placement_variables?: Record<string, unknown>;
}

export interface RuntimeObservationMount {
  type?: string;
  source?: string;
  destination: string;
  read_only?: boolean;
}

export interface RuntimeContainerObservation {
  status: "observed" | "unknown" | string;
  reason?: string;
  observed_at_unix_ms?: number;
  container_id?: string;
  image_id?: string;
  entrypoint: string[];
  cmd: string[];
  path?: string;
  args: string[];
  env: Record<string, string>;
  mounts: RuntimeObservationMount[];
  devices: string[];
  runtime_name?: string;
  network_mode?: string;
}

export interface RuntimeImageObservation {
  status: "observed" | "unknown" | string;
  reason?: string;
  observed_at_unix_ms?: number;
  image_id?: string;
  entrypoint: string[];
  cmd: string[];
  env: Record<string, string>;
  volumes: string[];
}

export interface RuntimeProcessObservation {
  status: "observed" | "unknown" | string;
  reason?: string;
  observed_at_unix_ms?: number;
  pid?: number;
  identity?: string;
  argv?: string[];
  env?: Record<string, string>;
}

export interface InstanceLaunchFacts {
  intent: {
    status: "available" | "unknown" | string;
    value?: LaunchFactsIntent;
  };
  resolved: {
    status: "available" | "unknown" | string;
    value?: LaunchFactsResolved;
  };
  runtime_observation_relation: {
    status: "current" | "historical" | "unverified" | "unknown" | string;
    reason?: string;
  };
  runtime_observation: {
    runtime: string;
    container: RuntimeContainerObservation;
    image: RuntimeImageObservation;
    process: RuntimeProcessObservation;
  };
  runtime_observation_received_at?: string;
}

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
  operations?: Array<
    Pick<
      import("./inferenceApi").InferenceAPIOperation,
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

export interface InstanceEvent {
  /** Pagination cursor: pass the last entry's id back as `before`. */
  id: number;
  kind: string;
  code?: string;
  params?: Record<string, unknown>;
  message?: string;
  data?: string;
  created_at: string;
}

/** A platform API token: authenticates the management API and MCP as its user. */
export interface PlatformToken {
  id: number;
  name: string;
  prefix: string;
  scope: "read" | "write";
  created_at: string;
  expires_at: string | null;
  last_used_at: string | null;
}

/** Whether the MCP service serves and how to reach it. */
export interface MCPStatus {
  enabled: boolean;
  path: string;
  protocol_version: string;
  tools: string[];
}

export interface ApiKey {
  name: string;
  prefix: string;
  last_used?: string;
  created_at: string;
  /** When the key was revoked; a revoked key no longer authenticates. */
  revoked_at?: string;
  /** The key may call every deployment; otherwise only `deployments`. */
  all_deployments: boolean;
  /** Empty when all_deployments, or when every granted deployment is gone. */
  deployments: ApiKeyDeployment[];
}

/** A deployment a restricted API key may call. */
export interface ApiKeyDeployment {
  id: number;
  name: string;
  served_name: string;
}

/** Which deployments an API key may call (POST /apikeys, PUT /apikeys/{prefix}/scope). */
export interface ApiKeyScope {
  all_deployments: boolean;
  deployment_ids: number[];
}

/** How long each kind of data is kept (GET/PUT /api/v1/data-retention). */
export interface DataRetention {
  metrics_hours: number;
  logs_hours: number;
  request_log_days: number;
  request_log_max_mb: number;
  usage_hourly_days: number;
  usage_daily_days: number;
  stats_hourly_days: number;
  stats_daily_days: number;
  quality: boolean;
  gpu: boolean;
  gpu_time: boolean;
  time_zone: string;
}

/** The experience environment beside this server: a separate console holding only simulated data. */
export interface ExperienceStatus {
  enabled: boolean;
  ready: boolean;
  port: number;
  user: string;
  password?: string;
  error?: string;
}

export interface DataRetentionView {
  value: DataRetention;
  /** Bounds of every numeric value, by field name. */
  limits: Record<string, { min: number; max: number }>;
  server_time_zone: string;
}

export interface DataRetentionStore {
  kind: "metrics" | "logs" | "requests" | "usage" | "quality" | "gpu" | "gpu_time" | "scheduling";
  file: string;
  file_bytes: number | null;
  hourly_rows: number | null;
  daily_rows: number | null;
  earliest_ms: number | null;
}

export interface DataRetentionStatus {
  stores: DataRetentionStore[];
  live_since_ms: number;
  backfill: { usage_done: boolean; usage_through_ms: number; quality_done: boolean; quality_through_ms: number };
  retain: { at_ms: number; error?: string };
  rollup: { at_ms: number; error?: string };
  usage_flush: { at_ms: number; rows: number; ms: number; error?: string };
}

export type ProviderCategory = "public_cloud" | "private_cloud";

export interface CloudProvider {
  id: number;
  name: string;
  kind: string;
  // The platform the kind connects, which the server derives; the platform type column shows it.
  category: ProviderCategory;
  state: string;
  instance_count: number;
  created_at: string;
  updated_at: string;
}

export interface ProviderEditConfig {
  id: number;
  name: string;
  kind: string;
  endpoint: string;
  public_key: string;
  extra: Record<string, string>;
}

export interface ProviderCapabilities {
  can_list_instances: boolean;
  can_power_control: boolean;
  can_install_agent: boolean;
  can_create: boolean;
  can_delete: boolean;
  can_inject_user_data: boolean;
  has_pricing: boolean;
}

export interface ProviderCredentialField {
  name: string;
  label: LocalizedText;
  secret: boolean;
  required: boolean;
  placeholder?: LocalizedText;
  default_value?: string;
  // Editable creation hint that must be changed before submission.
  default_template?: string;
  extra?: boolean;
}

export interface ProviderKindInfo {
  kind: string;
  label: LocalizedText;
  homepage?: string;
  capabilities: ProviderCapabilities;
  credential_fields: ProviderCredentialField[];
  default_server_dial_listen?: string;
  server_dial_supported: boolean;
  // The provider maps container ports: the server chooses the agent's port
  // and address when they are left empty.
  server_dial_auto_port?: boolean;
  /** The account's managed Kubernetes clusters can be listed and connected (GET /providers/{id}/clusters). */
  can_list_clusters?: boolean;
}

/** A managed Kubernetes cluster of a cloud account (dto.ProviderCluster), listed live. */
export interface ProviderCluster {
  external_id: string;
  name: string;
  region: string;
  version: string;
  /** The vendor's own word for the cluster's state. */
  state: string;
  running: boolean;
  /** The API server has a public address, so gpuworks can apply the manifest itself. */
  public_api: boolean;
  gpu_pools: ProviderClusterPool[];
  /** The cluster entry already made from this cluster. */
  entry?: { uuid: string; name: string };
  /** The registry an entry made from it pulls the agent image from unless the form names another. */
  agent_image_source: string;
}

/** A GPU node pool of a ProviderCluster. */
export interface ProviderClusterPool {
  id: string;
  name: string;
  gpu_model: string;
  /** GPUs per node. */
  gpu_count: number;
  nodes: number;
  vendor: KubeVendor;
  /** The cluster's entry holds this pool: the DaemonSet runs on its nodes. */
  in_entry: boolean;
}

export interface ProviderInstance {
  external_id: string;
  name: string;
  gpu_model: string;
  gpu_count: number;
  power: string;
  price_per_hr: number;
  region: string;
  public_ip: string;
  ssh_port: number;
  ssh_user: string;
  ssh_connections: SSHConnection[];
  kind: string;
  node_id: number;
  /** The instance is a node of this Kubernetes cluster of the account; it joins through the cluster. */
  managed_cluster?: string;
  /** What the instance allows now: power_on | power_off | install | delete. Empty for a cluster node. */
  actions: string[];
}

export interface SSHConnection {
  id: string;
  type: "direct" | "proxy" | "cloud_assistant" | string;
  label: LocalizedText;
  host: string;
  port: number;
  user: string;
  install_supported: boolean;
}

export interface ProviderInstallRequest {
  external_id: string;
  server_url?: string;
  ssh_key_pem?: string;
  password?: string;
  ssh_connection_id?: string;
  connect_mode?: "agent_dial" | "server_dial";
  server_dial_listen?: string;
  host_address?: string;
  server_dial_address?: string;
  // The operator agreed that opening a port for the agent restarts the
  // instance (RunPod answers INSTANCE_RESTART_REQUIRED until then).
  allow_instance_restart?: boolean;
  // The agent's install directory on the instance, absolute; omitted for
  // /opt/gpuworks.
  install_dir?: string;
  // Run the agent under nohup instead of the gpuworks-agent systemd unit.
  no_systemd?: boolean;
}

export interface ProviderInstallResponse {
  node_uuid?: string;
  status: string;
  connect_mode: "agent_dial" | "server_dial";
  server_dial_address?: string;
  server_url?: string;
  ssh_connection?: SSHConnection;
  ssh_attempts?: SSHAttempt[];
  checks?: CheckResult[];
  // The steps behind the summary checks: how the platform key reached the
  // instance, which SSH credential worked, and the driver's own checks.
  install_details?: CheckResult[];
  // Every item behind the one-line "precheck" row, passing ones included.
  precheck_details?: CheckResult[];
}

export interface SSHAttempt {
  connection_id: string;
  target: string;
  status: "selected" | "failed";
  detail?: string;
}

// --- AI helper -------------------------------------------------------------
export type AIPreset =
  "deepseek" | "bailian" | "siliconflow" | "openrouter" | "poe" | "custom";
export interface AIConfig {
  id: number;
  name: string;
  preset: string;
  base_url: string;
  model: string;
  is_default: boolean;
}
export interface AIConfigInput {
  name: string;
  preset: AIPreset;
  base_url: string;
  api_key: string;
  model: string;
  is_default: boolean;
}
/**
 * AIConfigPatch changes a stored service in place. Only the fields present
 * change; api_key is left out to keep the stored key, which the API never
 * sends back.
 */
export interface AIConfigPatch {
  name?: string;
  preset?: AIPreset;
  base_url?: string;
  api_key?: string;
  model?: string;
  is_default?: boolean;
}
/**
 * AIConfigDeleteResult answers a delete. promoted_id names the service that took
 * over as the default when the deleted one was in use, and is absent when
 * nothing changed hands: the console says which service the assistant moved to
 * instead of letting the data-egress notice change host unannounced.
 */
export interface AIConfigDeleteResult {
  status: string;
  promoted_id?: number;
}
/**
 * AIConfigTest asks whether an endpoint answers before it is saved. id names
 * the stored service an edit started from; with an empty api_key its stored
 * key is used, while base_url still matches the stored address.
 */
export interface AIConfigTest {
  id?: number;
  base_url: string;
  api_key?: string;
  model: string;
}
/**
 * AIConfigTestResult is what a connection test established. outcome is a
 * stable code the console words itself: ok, connection (DNS, dial or
 * timeout), blocked (refused by the outbound policy), auth (401/403), path (a
 * 404 that names no model: the wrong address), model (the endpoint does not
 * serve that model) or http (any other answer). detail is the endpoint's own
 * words, shortened and with the key removed.
 */
export interface AIConfigTestResult {
  outcome: "ok" | "connection" | "blocked" | "auth" | "path" | "model" | "http";
  status?: number;
  detail?: string;
  base_url: string;
  latency_ms: number;
}
/**
 * One assistant task about a subject named by reference (instance_uuid,
 * model_id, deployment_id or node_uuid); the server gathers its context.
 * Task "ask" is a free question. A follow-up carries the thread's earlier
 * turns in history.
 */
export interface AIAskRequest {
  task: "instance.diagnose" | "model.fit" | "deployment.unscheduled" | "node.health" | "ask";
  instance_uuid?: string;
  model_id?: number;
  deployment_id?: number;
  node_uuid?: string;
  question?: string;
  history?: { role: "user" | "assistant"; content: string }[];
  locale: string;
}
export interface AIStep {
  tool: string;
  args: string;
  ms: number;
  error?: string;
}
export interface AIAnswer {
  answer: string;
  steps: AIStep[];
  calls: number;
  usage: { prompt_tokens: number; completion_tokens: number };
}
/** A known failure the instance's logs match; the client renders code. */
export interface AIFinding {
  code: string;
  evidence: { line: string }[];
}
export interface AIFitRow {
  vendor: string;
  name: string;
  memory_bytes: number;
  nodes: number;
  cards: number;
  /** Broken cards (the UI's Error health level); cards counts the usable ones. */
  unusable: number;
  max_cards_per_node: number;
  dtype_supported: boolean;
  dtype_note?: string;
  /** Largest group one node links by NVLink or another fast interconnect; 1 = PCIe only, 0 = unknown. */
  fast_link_cards: number;
  fits: boolean;
  tp: number;
  per_gpu_bytes: number;
  usable_bytes: number;
  headroom_bytes: number;
}
/** One event of a streamed /ai/ask answer (Accept: text/event-stream). */
export type AIStreamEvent =
  | { type: "step"; id: number; tool: string; args: string; status: "running" | "done" | "error"; ms: number; error?: string }
  | { type: "text" | "reasoning"; delta: string }
  | { type: "done"; calls: number; usage: { prompt_tokens: number; completion_tokens: number }; first_text_ms: number; total_ms: number }
  | { type: "error"; error: string };
export interface AIModelFit {
  model_id: number;
  weight_bytes: number;
  dtype?: string;
  gpu_memory_utilization: number;
  rows: AIFitRow[];
}

export interface BenchmarkPoint {
  id: number;
  seq: number;
  // Closed loop: the concurrency held. Open loop (request_rate > 0): the
  // in-flight cap, 0 meaning unlimited.
  concurrency: number;
  target_isl: number;
  target_osl: number;
  // Open-loop arrival rate in req/s; 0 for a closed-loop point.
  request_rate?: number;
  state: string;
  success: number;
  errors: number;
  actual_isl: number;
  actual_osl: number;
  input_tokens: number;
  output_tokens: number;
  output_tput: number;
  total_tput: number;
  max_output_tput: number;
  max_total_tput: number;
  request_tput: number;
  avg_ttft: number;
  p50_ttft: number;
  p99_ttft: number;
  avg_tpot: number;
  p50_tpot: number;
  p99_tpot: number;
  avg_latency: number;
  p50_latency: number;
  p99_latency: number;
  long_tail?: BenchmarkLongTail;
  detail_path?: string;
  started_at?: string;
  ended_at?: string;
}
// BenchmarkLongTail is a point's long-tail facts: each percentile of TTFT,
// TPOT, end-to-end latency and inter-token latency in milliseconds, keyed by
// percentile ("50", "99"), and the cache and speculative-decoding facts.
export interface BenchmarkLongTail {
  ttft_ms: Record<string, number> | null;
  tpot_ms: Record<string, number> | null;
  latency_ms: Record<string, number> | null;
  itl_ms: Record<string, number> | null;
  approximate_tokens: boolean;
  kv_hit_rate?: number;
  cache_report_coverage?: number;
  peak_input_tokens: number;
  speculative?: { tokens_per_iteration: number; acceptance_rate: number };
  evidence?: BenchmarkPointEvidence;
}
export type BenchmarkPointSummary = Pick<
  BenchmarkPoint,
  | "seq"
  | "concurrency"
  | "target_isl"
  | "target_osl"
  | "request_rate"
  | "output_tput"
  | "request_tput"
  | "p99_ttft"
>;
export interface BenchmarkRun {
  id: number;
  name: string;
  target_kind: "instance" | "external";
  deployment_id?: number;
  instance_uuid?: string;
  base_url?: string;
  model: string;
  load_origin: string;
  num_gpus: number;
  dataset: {
    kind?: string;
    preset?: string;
    generator_version?: number;
    input_tokens?: number;
    mean_input_tokens?: number;
    first_turn_input_tokens?: number;
    subsequent_turn_input_tokens?: number;
    output_tokens?: number;
    turns?: number;
    peak_input_tokens?: number;
    required_context_tokens?: number;
    design_prefix_reuse_rate?: number;
    seed?: number;
    workload_fingerprint?: string;
    ignore_eos?: boolean;
    profile?: BenchmarkDatasetProfile;
    samples?: Array<{
      messages: Array<{ role: string; content: string }>;
      follow_ups?: Array<{ role: string; content: string }>;
      input_tokens?: number;
      output_tokens?: number;
      wait_millis?: number;
    }>;
  };
  mode: "grid" | "saturation" | "sla";
  tokenizer_ref?: string;
  target_snapshot?: BenchmarkTargetSnapshot;
  warnings?: string[];
  deployment_deleted?: boolean;
  instance_deleted?: boolean;
  search?: {
    concurrency?: number | number[];
    isl?: number[];
    osl?: number[];
    max_concurrency?: number;
    fixed_concurrency?: number[];
    sessions_per_point?: Record<string, number>;
    min_throughput_gain?: number;
    requests_per_point_multiplier?: number;
    max_avg_ttft?: number;
    max_p99_ttft?: number;
    max_avg_tpot?: number;
    max_p99_tpot?: number;
    max_avg_latency?: number;
    max_p99_latency?: number;
    max_rps?: number;
    max_tps?: number;
    min_rps?: number;
    min_tps?: number;
    min_interactivity?: number;
    min_session_completion?: number;
    // Retired per-user limits (throughput ÷ concurrency); only historical
    // runs carry them.
    max_per_user_total_tps?: number;
    min_per_user_total_tps?: number;
    max_per_user_output_tps?: number;
    min_per_user_output_tps?: number;
    objective?: "output_tps" | "total_tps" | "rps";
    request_rates?: number[];
    burstiness?: number;
    max_in_flight?: number;
    point_duration_s?: number;
    warmup_requests?: number;
    request_timeout_s?: number;
  };
  keep_detail: boolean;
  detail_cap_mb: number;
  state: "pending" | "running" | "done" | "failed" | "canceled";
  verdict?: BenchmarkVerdict;
  progress?: {
    point_index?: number;
    point_total?: number;
    point_count?: number;
    completed?: number;
    errors?: number;
    current_point?: {
      seq: number;
      concurrency: number;
      target_isl: number;
      target_osl: number;
      request_total: number;
      request_rate?: number;
      warmup?: boolean;
    };
    sample?: {
      output_tput: number;
      total_tput: number;
      request_tput: number;
      ttft_p50: number;
      tpot_p50: number;
      latency_p99: number;
      success: number;
      errors: number;
    };
  };
  error?: string;
  best_point?: BenchmarkPointSummary;
  points?: BenchmarkPoint[];
  created_at: string;
  started_at?: string;
  ended_at?: string;
}
export interface BenchmarkSnapshotGPU {
  index: number;
  name?: string;
  arch_id?: string;
  vendor?: string;
  uuid?: string;
  node_id?: number;
  node_name?: string;
}
export interface BenchmarkTargetSnapshot {
  target_kind: "instance" | "external";
  deployment_id?: number;
  deployment_name?: string;
  instance_uuid?: string;
  served_name?: string;
  model: string;
  model_source_id?: string;
  base_url?: string;
  load_origin: string;
  engine?: string;
  image?: string;
  command_mode?: string;
  entrypoint?: string[];
  argv?: string[];
  env?: Record<string, string>;
  docker_args?: string[];
  gpu_indexes?: number[];
  gpus?: BenchmarkSnapshotGPU[];
  num_gpus: number;
  tensor_parallel?: number;
  node_id?: number;
  node_name?: string;
}
export interface BenchmarkCurveSample {
  t: number;
  output_tput?: number | null;
  total_tput?: number | null;
  request_tput?: number | null;
  ttft_p50?: number | null;
  tpot_p50?: number | null;
  latency_p99?: number | null;
}
export interface BenchmarkCurveResponse {
  points: Array<{
    seq: number;
    concurrency: number;
    samples: BenchmarkCurveSample[];
  }>;
}
export interface BenchmarkTimelineRecord {
  version: number;
  run_id: number;
  point_seq: number;
  session_id: string;
  turn: number;
  planned_turns: number;
  attempt: number;
  finish_reason?: string;
  error_text?: string;
  canceled?: boolean;
  error_stage?: string;
  http_status?: number;
  input_source: string;
  output_source: string;
  output_budget: number;
  metrics: {
    started_at: string | null;
    first_output_at: string | null;
    first_content_at: string | null;
    last_output_at: string | null;
    finished_at: string | null;
    ttft_ms: number | null;
    tpot_ms: number | null;
    latency_ms: number | null;
    completion_ms: number | null;
    input_tokens: number;
    output_tokens: number;
  };
}
export interface BenchmarkRequestTimeline {
  status: "complete" | "disabled" | "unavailable" | "truncated";
  records: BenchmarkTimelineRecord[];
}
export interface BenchmarkViolation {
  reason?: "limit_exceeded" | "evidence_unavailable";
  metric: string;
  limit: number;
  actual: number;
}
export interface BenchmarkVerdict {
  kind:
    "saturated" | "truncated" | "sla_met" | "sla_none" | "grid_done" | "failed";
  best_point_seq?: number;
  saturation_concurrency?: number;
  headroom_pct?: number;
  sla_max_concurrency?: number;
  violated?: BenchmarkViolation[];
}
export interface BenchmarkMetaOption {
  value: string;
  label: LocalizedText;
  description?: LocalizedText;
  color?: string;
}
export interface BenchmarkModeDefaults {
  concurrency: number | number[];
  max_concurrency?: number;
  isl: number[];
  osl: number[];
  requests_per_point_multiplier: number;
  min_throughput_gain?: number;
  objective?: "output_tps" | "total_tps" | "rps";
}
export interface BenchmarkMeta {
  modes: BenchmarkMetaOption[];
  mode_defaults: Record<BenchmarkRun["mode"], BenchmarkModeDefaults>;
  datasets: BenchmarkMetaOption[];
  retired_datasets?: BenchmarkMetaOption[];
  agentic_presets: Array<{
    value: string;
    label: string;
    mean_input_tokens: number;
    first_turn_input_tokens: number;
    peak_input_tokens: number;
    required_context_tokens: number;
    recommended?: boolean;
  }>;
  agentic_defaults: {
    preset: string;
    turns: number;
    subsequent_turn_input_tokens: number;
    output_tokens: number;
    seed: number;
    concurrency: number[];
    sessions_per_point: Record<string, number>;
    sessions_per_concurrency: number;
    min_sessions_per_point: number;
  };
  dataset_profiles: Record<string, BenchmarkDatasetProfile>;
  sla_constraints: Array<{
    key: string;
    label: LocalizedText;
    unit?: string;
    direction: "min" | "max";
    min: number;
    max: number;
    step: number;
  }>;
  objectives: BenchmarkMetaOption[];
  statuses: BenchmarkMetaOption[];
  tokenizer_schemes: Record<string, string>;
  detail_cap_mb: { default: number; min: number; max: number; unit: string };
  load_defaults: BenchmarkLoadDefaults;
}
export interface BenchmarkLoadDefaults {
  warmup_requests: number;
  max_warmup_requests: number;
  burstiness: number;
  point_duration_s: number;
  min_point_duration_s: number;
  max_point_duration_s: number;
  min_open_loop_requests: number;
  max_request_rate: number;
  max_request_rates: number;
  max_burstiness: number;
  max_in_flight: number;
  request_timeout_s: number;
  min_request_timeout_s: number;
  max_request_timeout_s: number;
  ignore_eos: boolean;
  ignore_eos_datasets: string[];
}
export interface BenchmarkDatasetProfile {
  sample_count: number;
  max_input_tokens: number;
  max_output_tokens: number;
}
export interface NetworkConfig {
  http_proxy?: string;
  https_proxy?: string;
  /** Hosts and networks the proxy is not used for; also written into dockerd's proxy. */
  no_proxy?: string;
}

/**
 * What a docker node's agent observes about the proxy dockerd pulls through,
 * and what it can do about it. Absent for runtimes that apply the node proxy
 * on every pull and for offline nodes.
 */
export interface RuntimeProxy {
  http_proxy: string;
  https_proxy: string;
  no_proxy: string;
  /** What dockerd starts with, as systemd has the unit loaded (masked; empty when unknown). */
  configured_http_proxy: string;
  configured_https_proxy: string;
  /** What the platform wrote into the dockerd drop-in file. */
  written_http_proxy: string;
  written_https_proxy: string;
  written_no_proxy: string;
  /** dockerd keeps containers running across its own restart. */
  live_restore: boolean;
  /** /etc/docker/daemon.json also sets proxies, and those win over the drop-in. */
  daemon_json_proxy: boolean;
  systemd: boolean;
  privilege: "root" | "sudo" | "none" | string;
  /** dockerd already uses the proxy the platform set for the node; with none set, the proxy it is configured with. */
  applied: boolean;
  error?: string;
}

export interface NodeRuntime {
  driver: string;
  proxy?: RuntimeProxy;
}

export interface SchedulePlan {
  gpus: number[];
  tp: number;
  per_gpu_bytes: number;
  score: number;
  reasons: ScheduleReason[];
}

export interface ScheduleMessage {
  code?: string;
  params?: Record<string, unknown>;
  message?: string;
}

export type ScheduleMessageLike = ScheduleMessage | string;

export interface ScheduleReason extends ScheduleMessage {
  score?: number;
}

export interface ScheduleRejected {
  gpus?: number[];
  tp: number;
  reason: ScheduleMessageLike;
}

export interface ScheduleExplain {
  chosen?: SchedulePlan | null;
  rejected?: ScheduleRejected[];
  topo_highlight?: {
    gpus?: number[];
    links?: [number, number][];
    numa?: number[];
  };
  filters?: ScheduleReason[];
}
export interface ProductWarning {
  code: string;
  params?: Record<string, unknown>;
}

// One finding about a template document (POST /inference-templates/import
// and /evaluate): `error` is the server's English text, shown only for a code
// the console has no localized text for.
export interface InferenceTemplateIssue {
  code: string;
  error: string;
  params?: Record<string, unknown>;
}

// The import answer; `warnings` is absent when there are none.
// POST /deployments/{id}/catalog-template: a running deployment written as a
// flat catalog template (docs/how-to/save-deployment-as-template.md). The
// server tests the cheap operations before it writes the YAML; notes are the
// facts the YAML comments state, by stable code.
export interface CatalogTemplateCheck {
  operation: string;
  endpoint?: string;
  tested: boolean;
  passed: boolean;
  http_status?: number;
  message?: string;
}

export interface CatalogTemplateExport {
  model_id: string;
  spec_name: string;
  yaml: string;
  verified: boolean;
  saveable: boolean;
  checks: CatalogTemplateCheck[];
  notes: ProductWarning[];
  excluded_env: string[];
  evidence: string;
}

export interface InferenceTemplateImportResult {
  models: number;
  package_digest?: string;
  warnings?: InferenceTemplateIssue[];
}

// One selectable acquisition option. `key` is the opaque token the server
// validates on submit; the client never composes one itself.
export interface AcquisitionCandidate {
  key: string;
  label: string;
  // Present when `label` is an identifier: show this to the user and keep
  // submitting `label`.
  display_name?: string;
  reason: string;
  recommended?: boolean;
  // Storage fields appear on present and target candidates.
  storage_id?: number;
  storage_type?: string;
  size_bytes?: number;
  /** nfs_cache: bytes still to copy into the node cache before a start. */
  cache_bytes?: number;
  /** The files are already on the target node's disk. */
  on_node?: boolean;
  /** A reason code: the storage is listed but cannot serve the target nodes now. */
  unavailable?: string;
  default?: boolean;
}

export interface AcquisitionResult {
  /** One candidate per storage that holds the model and the target nodes can read. */
  present?: AcquisitionCandidate[];
  /** Writable storages a node may save the model into when it has to fetch it. */
  targets?: AcquisitionCandidate[];
  /** The template's public weight sources: which weights run. */
  providers?: AcquisitionCandidate[];
  image?: {
    image?: string;
    cached?: boolean;
    cache_reason?: string;
    options?: ImageOption[];
  };
  /**
   * A forecast per node the request names (a manual placement or a
   * multi-node draft, in rank order); absent for automatic placement.
   */
  nodes?: AcquisitionNodeFact[];
  /**
   * How each template component other than the primary (a separate text
   * encoder, a speculative draft) reaches the target nodes; present once the
   * nodes are known.
   */
  components?: AcquisitionComponent[];
}

/**
 * One template component's forecast. plan "ready": every target node reads it
 * already; "shared_download": the create downloads it once into the shared
 * storage `storage`, through one writer node (follows_primary: no storage was
 * chosen and it is the one the primary sits on); "node_download": each target
 * node without it downloads it into its own storage.
 */
export interface AcquisitionComponent {
  name: string;
  role: string;
  model: string;
  size_bytes?: number;
  plan: "ready" | "shared_download" | "node_download";
  nodes: number;
  ready_nodes: number;
  storage_id?: number;
  storage?: string;
  follows_primary?: boolean;
}

export type ImagePlan = "present" | "copy" | "pull";
export type WeightsPlan = "present" | "copy" | "storage" | "download";

/** How one node would get the image and the weights if it started now. */
export interface AcquisitionNodeFact {
  node_uuid: string;
  node_name: string;
  image_cached?: boolean;
  image_cache_reason?: string;
  image_plan: ImagePlan | string;
  image_copy_from?: string;
  image_pull_refs?: string[];
  /** The node reorders the image's own copies by how fast it reaches them. */
  image_pull_ranked?: boolean;
  /** The first image_pull_fixed refs are rule endpoints, tried in this order before the rest. */
  image_pull_fixed?: number;
  /** Why the node has nothing to pull from: a product error code, its params and the English fallback. */
  image_pull_error?: { code: string; params?: Record<string, unknown>; message: string };
  weights_present: boolean;
  weights_plan: WeightsPlan | string;
  weights_copy_from?: string;
  weights_storage?: string;
  weights_provider?: string;
  /** The hosts a download of the weights would try, in order (planned from the site rules). */
  weights_fetch_hosts?: string[];
  /** Why the node cannot download the weights (WEIGHTS_FETCH_FORBIDDEN: the rules forbid every address). */
  weights_fetch_error?: ProductMessage;
}

export interface ImageOption {
  image: string;
  reason: string;
  compatible: boolean;
}

/** A node selector: for each label key the values a node may carry (alternatives); keys all hold. Empty = every node. */
export type NodeRequirementsMap = Record<string, string[]>;

export interface PullSourceRule {
  id: number;
  position: number;
  /** "url" for a site rule (the match has an http(s) scheme), "image" for a registry rule. */
  kind: "image" | "url";
  match: string;
  endpoints: string[];
  allow_upstream: boolean;
  /** The management node reads the site through this rule too (model metadata); site rules only. */
  control_plane: boolean;
  note: string;
  node_requirements: NodeRequirementsMap;
  matched_nodes: number;
}

/** The addresses the console's browser may read a source's model metadata at; empty when the rules forbid them all. */
export interface MetadataSources {
  source: string;
  bases: string[];
}

/** A draft site rule and the node asked whether it reaches its addresses (nothing is saved). */
export interface PullSourceRuleTestRequest {
  match: string;
  endpoints: string[];
  allow_upstream: boolean;
  node_uuid: string;
  repo?: string;
}

export type SourceProbeStatus = "ok" | "auth_required" | "http_error" | "unreachable";

export interface SourceProbeOutcome {
  base: string;
  status: SourceProbeStatus;
  code?: number;
  latency_ms: number;
  error?: string;
}

/** The node's answer, address by address, in the order it would try them. */
export interface SourceRuleTestResult {
  source: string;
  repo: string;
  node_uuid: string;
  node_name: string;
  outcomes: SourceProbeOutcome[];
}

export interface PullSourceRuleRequest {
  match: string;
  endpoints: string[];
  allow_upstream: boolean;
  control_plane?: boolean;
  note: string;
  node_requirements: NodeRequirementsMap;
}

/** One ordered proxy entry; a node uses the first entry whose node requirements it meets. Addresses never carry credentials. */
export interface PullProxy {
  id: number;
  position: number;
  http_proxy: string;
  https_proxy: string;
  no_proxy: string;
  username: string;
  has_password: boolean;
  /** The management node goes out through this proxy too (model metadata, logos, catalog URLs, login tests). */
  control_plane: boolean;
  note: string;
  node_requirements: NodeRequirementsMap;
  matched_nodes: number;
}

/** password: omitted keeps the stored one on update, "" removes it, a value sets it. */
export interface PullProxyRequest {
  http_proxy: string;
  https_proxy: string;
  no_proxy: string;
  username: string;
  password?: string;
  control_plane: boolean;
  note: string;
  node_requirements: NodeRequirementsMap;
}

/** The proxy entry one node uses (null: none, the agent uses its own environment) and its container runtime's proxy status. */
export interface NodePullProxy {
  proxy: PullProxy | null;
  runtime?: NodeRuntime;
}

export interface SyncGroup {
  id: number;
  name: string;
  node_requirements: NodeRequirementsMap;
  /** 0 = unlimited. */
  rate_limit_kib_s: number;
  matched_nodes: number;
}

export interface SyncGroupRequest {
  name: string;
  node_requirements: NodeRequirementsMap;
  rate_limit_kib_s: number;
}

/**
 * Credential is one login per host. An empty username is a model site's token
 * (sent as a Bearer token over https to that exact host); otherwise it is a
 * registry login (user and password).
 */
export interface Credential {
  id: number;
  host: string;
  username: string;
  has_secret: boolean;
}

export interface CredentialRequest {
  host: string;
  username: string;
  /** On an update an empty secret keeps the stored one. */
  secret: string;
}

// One dictionary flag the server took out of extra_args and put in its field.
// The form follows that move instead of parsing arguments of its own: the
// launch configuration has a single typed representation (ADR 0020).
export interface MovedLaunchFlag {
  key: string;
  flag: string;
  value: unknown;
  source: "extra_args";
}

export interface EvaluateFieldError {
  // The form field the error belongs beside: a dictionary key, a wire field
  // name, or container_mount_rows for mount rows.
  field: string;
  code: string;
  params?: Record<string, unknown>;
}

export interface EvictionSuggestion {
  instance_uuid: string;
  deployment_id: number;
  deployment: string;
  gpu_indexes: number[];
  frees_gib: number;
}

export interface EligibleNode {
  // Explicit node conditions, independently of vendor/chip/state checks.
  requirements_match?: boolean;
  uuid: string;
  name: string;
  eligible: boolean;
  reasons?: ScheduleMessageLike[];
}

export interface WeightSourceOption {
  provider: string;
  repo_id: string;
  revision?: string;
  subfolder?: string;
  suggested: boolean;
  registered: boolean;
}

export interface PreflightCheck {
  code?: string;
  params?: Record<string, unknown>;
  message?: string;
}

export interface PreflightSummary {
  ok: boolean;
  checks?: PreflightCheck[];
}

export interface EvaluateResponse {
  gpu_candidates?: GPUCandidates;
  schema_version?: number;
  diagnostics?: DiagnosticReport;
  request_id?: string;
  // Request errors attributed to form fields. A response with errors has
  // ok=false and stays HTTP 200; the codes match the create 400 path.
  errors?: EvaluateFieldError[];
  // Every node judged against this draft, eligible or not.
  eligible_nodes?: EligibleNode[];
  // The dGPU backend the eligible nodes offer (the first ready one); absent
  // when none has one. An automatic draft gates its quota on it.
  pool_partition_capability?: PartitionCapability;
  // What the form should display: the effective launch configuration with the
  // dictionary defaults and template projection merged, plus extra_args after
  // the moved flags left it.
  form_values?: Record<string, unknown>;
  // The form's former hardcoded constants, answered by the server.
  defaults?: { model_mount: string; master_port: number; engine: string };
  weight_sources?: WeightSourceOption[];
  preflight?: PreflightSummary;
  /** The split the draft launches with (server-computed; absent until known). */
  parallelism?: { tp: number; pp: number; nodes: number; gpus_per_node: number };
  plan_scope?: "per_replica" | string;
  replicas?: number;
  pre_start_argv?: string[];
  pre_start_script?: string;
  acquisition?: AcquisitionResult;
  ok: boolean;
  // The server picked the nodes of this multi-node or PD request
  // (placement_mode "auto"): the nodes shown are where it would go now, and the
  // create picks again.
  auto_placed?: boolean;
  // The complete launch configuration after materialization: every dictionary
  // key the deployment will start with, defaults included.
  effective_params: Record<string, unknown>;
  // Dictionary flags the server moved out of extra_args into their fields.
  moved?: MovedLaunchFlag[];
  // The dictionary keys the selected catalog spec contributed, and the tokens
  // of that spec which are outside the dictionary and stay verbatim.
  template_params?: Record<string, unknown>;
  template_residual_args?: string[];
  // Keys whose default the effective launch leaves to the engine: the form
  // starts them at "automatic" and the deployer may still turn them on or off.
  engine_decides_param_keys?: string[];
  skipped?: boolean;
  skip_reason?: string;
  template_spec_source?: string;
  template_spec_name?: string;
  template_match?: "spec-name" | "automatic" | string;
  matched_by?: string;
  template_deviated: boolean;
  entrypoint?: string[];
  argv: string[];
  command?: string[];
  env?: Record<string, string>;
  command_mode?: string;
  model_mount_path?: string;
  docker_args?: string[];
  /** Size of the container's /dev/shm tmpfs, in bytes. */
  shm_bytes?: number;
  mounts?: RuntimeMount[];
  env_layers?: {
    system?: Record<string, string>;
    template?: Record<string, string>;
    user?: Record<string, string>;
  };
  rank_commands?: EvaluateRankCommand[];
  image: string;
  warnings: ProductWarning[];
  breakdown?: Breakdown;
  eviction?: EvictionSuggestion[];
  selected_node?: {
    uuid: string;
    name?: string;
    reasons?: ScheduleMessageLike[];
  };
  candidates?: Array<{
    node_uuid: string;
    node_name?: string;
    ok: boolean;
    reason?: ScheduleMessageLike;
    plan?: {
      ok: boolean;
      message: ScheduleMessageLike;
      plan?: SchedulePlan;
      explain?: ScheduleExplain;
    };
  }>;
  plan?: {
    ok: boolean;
    message: ScheduleMessageLike;
    plan?: SchedulePlan;
    explain?: ScheduleExplain;
  };
  // PD previews a PD disaggregated draft instance by instance (ADR 0028); the
  // command fields above then describe the first decode instance.
  pd?: EvaluatePd;
  // Whether the draft may be created with a yield policy other than never,
  // the create's own check; absent until the draft's engine is known.
  yield_policy_eligible?: Eligibility;
}

export interface GPUCandidates {
  node_uuid: string;
  items: { index: number; available: boolean; reason?: ScheduleReason }[];
}

// PdRoleName is the fixed pair every PD mode declares today (ADR 0028); the
// mode catalog is the only place new role names could come from.
export type PdRoleName = "prefill" | "decode";

export interface EvaluatePdUnit {
  node_uuid: string;
  node_name?: string;
  address?: string;
  gpus?: number[];
  fits: boolean;
  reason?: ScheduleMessageLike;
  breakdown?: Breakdown;
  entrypoint?: string[];
  argv?: string[];
  command?: string[];
  env?: Record<string, string>;
  docker_args?: string[];
  /** Size of the container's /dev/shm tmpfs, in bytes. */
  shm_bytes?: number;
  mounts?: RuntimeMount[];
  // The unit's headless ranks on other nodes, in rank order - same shape as
  // a unit itself (node_uuid, node_name, address, gpus, fits, reason, argv,
  // command, env, docker_args, mounts).
  followers?: EvaluatePdUnit[];
}

export interface EvaluatePdRole {
  role: PdRoleName;
  tp: number;
  // Pipeline-parallel degree; absent when the role runs PP 1.
  pp?: number;
  units: EvaluatePdUnit[];
}

export interface EvaluatePd {
  mode: string;
  fallback: string;
  kv_load_failure: string;
  // Decode first, then prefill (ADR 0028's role order).
  roles: EvaluatePdRole[];
}

export interface EvaluateRankCommand {
  pre_start_argv?: string[];
  pre_start_script?: string;
  node_uuid: string;
  node_name?: string;
  rank_address?: string;
  node_rank: number;
  role: "standalone" | "leader" | "follower";
  headless: boolean;
  image: string;
  entrypoint?: string[];
  argv: string[];
  command?: string[];
  env?: Record<string, string>;
  command_mode?: string;
  model_mount_path?: string;
  docker_args?: string[];
  /** Size of the container's /dev/shm tmpfs, in bytes. */
  shm_bytes?: number;
  mounts?: RuntimeMount[];
  env_layers?: {
    system?: Record<string, string>;
    user?: Record<string, string>;
  };
}

export interface DistributedPreflightPair {
  source_uuid: string;
  target_uuid: string;
  target: string;
  reachable: boolean;
  rtt_ms_p50: number;
  rtt_ms_p99: number;
  gbps?: number;
  bandwidth_tested: boolean;
  bytes_sent?: number;
  bytes_received?: number;
  duration_seconds?: number;
  error?: string;
  bandwidth_error?: string;
}

export interface DistributedPreflightResponse {
  ok: boolean;
  pairs: DistributedPreflightPair[];
  warnings?: string[];
}

// --- Metrics & logs --------------------------------------------------------
// MetricPoint is one sample as [unix ms, value]; GET /metrics/query and
// GET /monitor/series both answer series of this shape.
export type MetricPoint = [number, number];
export interface MetricSeries {
  labels: Record<string, string>;
  points: MetricPoint[];
}

export interface GPUOccupant {
  instance_uuid: string;
  deployment_id: number;
  deployment_name?: string;
  state: string;
  // 0 awake; a sleeper holds only its residue.
  sleep_level?: number;
  limit_bytes?: number;
  sm_limit_pct?: number;
}

export interface GPULoad {
  utilization_pct?: number;
  memory_used_bytes?: number;
  temperature_c?: number;
  power_watts?: number;
  fan_speed_pct?: number;
  pcie_rx_bytes_per_sec?: number;
  pcie_tx_bytes_per_sec?: number;
  updated_at?: string;
}

export interface PhysicalGPU {
  index: number;
  uuid: string;
  name: string;
  vendor: string;
  arch_family?: string;
  arch_id?: string;
  compute_capability?: string;
  memory_total_bytes: number;
  memory_capacity_known?: boolean; // Present on current servers; absent on older API responses.
  power_limit_watts?: number;
  pcie_bdf?: string;
  numa_node: number;
  partition_capabilities?: PartitionCapability[];
  health_status?: string;
  health_code?: string;
  health_message?: string;
  health_evidence?: GPUHealthField[];
  health_guide?: GPUHealthGuide;

  node_uuid: string;
  node_name: string;
  node_state: string;
  driver_version?: string;
  /** The server's verdict on whether work may be placed here. Read this
   * rather than deriving one from the health level: a card can be
   * perfectly well and still take no work. */
  schedulable: boolean;
  /** Empty for a card this node may use, otherwise why not. */
  ownership?: "" | "passthrough" | "no_driver" | "held_elsewhere";
  /** Compute processes on the card that no instance of ours accounts for. */
  foreign_processes?: { pid: number; name: string; memory_bytes?: number }[];
  /** An operator took the card out of scheduling; only they put it back. */
  scheduling_stopped?: boolean;
  /** An operator let work onto a card the platform judged unusable, until the next report judges it afresh. */
  scheduling_allowed?: boolean;
  /** What an operator may do to the card now, decided by the server. */
  scheduling_actions?: CardScheduling[];
  allocation_state: "available" | "partial" | "allocated" | "unusable" | "unknown";
  allocated_bytes: number;
  available_bytes: number;
  occupants: GPUOccupant[];
  load: GPULoad;
}

/** One page of GET /gpus?page=…, with the overview and filter values of every GPU. */
export interface GPUPage extends ListPage<PhysicalGPU> {
  overview: GPUListResponse["overview"];
  facets: { nodes: { uuid: string; name: string }[]; models: string[]; vendors: string[] };
}

export interface GPUListResponse {
  overview: {
    total: number;
    health: { ok: number; warning: number; error: number };
    allocation: { allocated: number; allocatable: number; unusable: number; unknown: number };
  };
  items: PhysicalGPU[];
}

export interface DGPU {
  instance_uuid: string;
  deployment_id: number;
  deployment_name?: string;
  state: string;
  // 0 awake; a sleeper holds only its residue.
  sleep_level?: number;
  node_uuid: string;
  node_name: string;
  gpu_index: number;
  gpu_uuid?: string;
  gpu_name?: string;
  memory_limit_bytes: number;
  /** The deployment's limit when the slice still runs under another; it takes it at its next restart. */
  pending_limit_bytes?: number;
  sm_limit_pct?: number;
  memory_used_bytes?: number;
  sm_util_pct?: number;
  updated_at?: string;
}

/** An instance a dGPU quota change left at its limit until it restarts, and why. */
export interface DGPUQuotaPending {
  instance_uuid: string;
  reason: "backend" | "asleep" | "starting" | "in_use" | "no_usage";
  used_bytes?: number;
}

/** The answer to a dGPU quota change: what took the new limit at once and what waits for a restart. */
export interface DGPUQuotaResult {
  deployment: Deployment;
  live: string[];
  pending: DGPUQuotaPending[];
}

export interface DGPUListResponse {
  items: DGPU[];
}

export interface LogEntry {
  id: number;
  instance: string;
  node: string;
  level: string;
  source: string;
  time_ms: number;
  line: string;
}

export interface LogsFilter {
  q?: string;
  instance?: string;
  // instances is the service page's merged log stream: a comma-separated
  // list on the wire (max 64), mutually exclusive with `instance` - the
  // server rejects both being set at once.
  instances?: string[];
  node?: string;
  level?: string;
  source?: string;
  from?: number;
  to?: number;
  cursor_ts?: number;
  cursor_id?: number;
  limit?: number;
  with_total?: boolean;
}

// LogsPage is one newest-first page of GET /logs/search; total,
// total_capped and truncated are omitted when zero/false.
export interface LogsPage extends CursorPage<LogEntry> {
  total?: number;
  total_capped?: boolean;
  truncated?: boolean;
}

export interface LogsBucket {
  start_ms: number;
  counts: Record<string, number>;
}

export interface LogsWindow {
  items: LogEntry[];
  // Index of the anchor row in items, or -1 when the anchor is gone from
  // that partition (pruned by retention, or ts from another day). The
  // surrounding rows are still real, so compare against the index for
  // highlighting and never use it to index items without a >= 0 check.
  match_index: number;
}

// --- Monitor (/api/v1/monitor) ---------------------------------------------
// Shapes mirror the wire contract exactly (all snake_case; points are
// [t, v] pairs, the same MetricSeries shape GET /metrics/query answers).

// A signal reports a fleet fact and passes no verdict on it.
export interface MonitorSignal {
  key: string;
  collected: boolean;
  value?: number;
  denominator?: number;
  // error_rate carries {"5xx", "429", "asleep_503"} (each a percent);
  // ready_replicas carries {"not_ready": string[]}; ttft/tpot carry
  // {"upper_bound": true} - value/denominator (p95/p99) are each the max,
  // across the window, of the per-tick quantile gauge, not a single
  // quantile over the whole window (monitor.Window's doc comment) - so they
  // read as a conservative upper bound, not an exact percentile. Any
  // collected signal may additionally carry {"no_recent_requests": true}
  // when the window has samples but nothing happened in the last 5
  // minutes - value/denominator stay real numbers (0 for a rate) except
  // for ttft/tpot, where the server omits them (null) since a quantile has
  // no meaningful zero.
  detail?: Record<string, unknown>;
  spark?: Array<[number, number]>;
}

export type MonitorSeries = MetricSeries;

export interface MonitorReplicaCount {
  ready: number;
  desired: number;
}

export interface MonitorService {
  deployment_id: number;
  name: string;
  engine: string;
  placement: string;
  replicas: MonitorReplicaCount;
  state: "running" | "starting" | "stopped" | "failed" | "asleep" | "offline";
  request_rate: number | null;
  ttft_p95_ms: number | null;
  running: number | null;
  waiting: number | null;
  kv_usage_pct: number | null;
  kv_hit_rate_pct: number | null;
  prompt_tok_s: number | null;
  generation_tok_s: number | null;
  // The deployment's health check.
  health: DeploymentHealth;
}

export interface MonitorVramNode {
  node_uuid: string;
  name: string;
  gpu_count: number;
  total_bytes: number;
  allocated_bytes: number;
  // Null when the node has no fresh nvidia-smi memory.used sample
  // (dto.MonitorVRAMNode.UsedBytes/UsedAsOf are *int64).
  used_bytes: number | null;
  used_as_of: number | null;
}

export interface MonitorEvent {
  time_ms: number;
  kind:
    | "instance_running"
    | "instance_failed"
    | "instance_stopped"
    | "sleep"
    | "wake"
    | "node_offline"
    | "node_online";
  // Always present on the wire (no omitempty) - "" / 0 for a node-level
  // event, which has no owning deployment or instance.
  deployment_id: number;
  instance_uuid: string;
  label: string;
}

export interface MonitorOverview {
  as_of: number;
  stale: boolean;
  signals: MonitorSignal[];
  services: MonitorService[];
  vram_by_node: MonitorVramNode[];
  cluster_tokens: { prompt: MonitorSeries; generation: MonitorSeries };
  events: MonitorEvent[];
}

export interface MonitorDeploymentSummary {
  id: number;
  name: string;
  engine: string;
  placement: string;
  replicas: MonitorReplicaCount;
}

export interface MonitorAlignment {
  proxy_requests: number;
  engine_completed: number;
  // client_errors is the client's own fault (4xx plus 429); server_errors is
  // the server's fault (5xx plus the asleep-instance 503) - see
  // dto.MonitorAlignment's doc comment. Both are disjoint from each other
  // and from engine_completed; lost is max(0, proxy_requests -
  // engine_completed - client_errors - server_errors) - in_flight is NOT
  // subtracted, so requests still streaming legitimately show up in lost.
  client_errors: number;
  server_errors: number;
  lost: number;
  // in_flight is the proxy's in-flight gauge at the end of the window.
  // discrepancy_persistent is true when lost exceeds the window's maximum
  // in-flight count - i.e. more requests are missing than could have been
  // in flight at any point, so the gap cannot be explained by streaming
  // requests. The UI shows the red "lost" sentence only when it is true.
  in_flight: number;
  discrepancy_persistent: boolean;
}

export interface MonitorTtftBreakdown {
  queue_ms: number;
  prefill_ms: number;
  first_token_ms: number;
  total_ms: number;
  // True when queue_ms/prefill_ms/first_token_ms needed adjusting (scaled
  // down, and/or first_token_ms clamped to 0) to actually sum to total_ms -
  // see dto.MonitorTTFTBreakdown's doc comment. The UI marks the caption
  // "approx." rather than presenting the bar as an exact decomposition.
  approximate: boolean;
}

export interface MonitorReplicaGPU {
  index: number;
  util_pct: number | null;
  mem_bw_pct: number | null;
  temperature_c: number | null;
  power_w: number | null;
}

export interface MonitorReplica {
  instance_uuid: string;
  state: string;
  node_name: string;
  gpu_indexes: number[];
  running: number | null;
  waiting: number | null;
  kv_usage_pct: number | null;
  kv_hit_rate_pct: number | null;
  prompt_tok_s: number | null;
  generation_tok_s: number | null;
  gpus: MonitorReplicaGPU[];
  // Set for a PD deployment's replica (ADR 0028); absent for an ordinary
  // deployment, which never renders a role badge.
  role?: PdRoleName | string;
}

// MonitorDeploymentPd is the service monitor page's PD summary (ADR 0028):
// present only when the deployment is a PD deployment. Every field is null,
// not omitted, when the window has no samples to compute it from - same
// null-means-not-collected contract as the rest of this file, so the UI
// renders a dash rather than a fabricated 0.
export interface MonitorDeploymentPd {
  effectiveness_pct: number | null;
  fallback_rate_pct: number | null;
  fallbacks: Record<string, number>;
  prefill_stage_avg_ms: number | null;
  prefill_waiting_avg: number | null;
  avg_prompt_tokens: number | null;
  // The prefill stage's p95 over the same window.
  prefill_stage_p95_ms: number | null;
  // The decode instances' KV transfer, per transfer. exposed is false when
  // the PD mode's engine exports no transfer metrics (vllm-ascend's
  // Mooncake connector); every value is then null.
  kv_transfer: {
    exposed: boolean;
    avg_ms: number | null;
    p95_ms: number | null;
    avg_mb: number | null;
    mb_per_s: number | null;
    failed: number | null;
    expired: number | null;
  };
}

export interface MonitorDeploymentCharts {
  latency: MonitorSeries[];
  tpot: MonitorSeries[];
  concurrency: MonitorSeries[];
  kv: MonitorSeries[];
  throughput: MonitorSeries[];
  stage_time: MonitorSeries[];
  preemptions: MonitorSeries[];
}

export type MonitorSignalSourceKey =
  | "ttft"
  | "e2e"
  | "prompt_tokens"
  | "generation_tokens"
  | "running"
  | "tpot"
  | "queue_time"
  | "prefill_time"
  | "decode_time";
export type MonitorSignalSource = "engine" | "gateway";

// engineReportsNoMetrics: every signal with data came from the gateway, so
// the engine itself reports nothing (Strata has no /metrics, or a callable
// template declares no mapping).
export function engineReportsNoMetrics(sources: MonitorDeployment["sources"]): boolean {
  const values = Object.values(sources ?? {});
  return values.length > 0 && values.every((source) => source === "gateway");
}

// One stored series from the metric catalog GET /api/v1/monitor/metrics. An
// inference histogram contributes its quantile gauges (quantiles: true) and
// its _sum/_count.
export type MonitorMetricSource = "gpu" | "dgpu" | "node" | "network" | "engine" | "gateway" | "kv_disk" | "benchmark";
export interface MonitorMetricSeriesInfo {
  series: string;
  name: string;
  source: MonitorMetricSource;
  kind: "counter" | "gauge" | "histogram";
  unit: string;
  quantiles: boolean;
  labels?: string[];
  description?: string;
}

export interface MonitorDeployment {
  as_of: number;
  stale: boolean;
  deployment: MonitorDeploymentSummary;
  alignment: MonitorAlignment | null;
  ttft_breakdown: MonitorTtftBreakdown | null;
  charts: MonitorDeploymentCharts;
  replicas: MonitorReplica[];
  // The full set of instance uuids the merged log stream should query - not
  // necessarily identical to `replicas` (e.g. a replica that has since
  // stopped or failed can still have logs worth showing).
  log_instances: string[];
  // Set only for a PD deployment (ADR 0028); an ordinary deployment never
  // carries this and the page never renders the PD card for it.
  pd?: MonitorDeploymentPd;
  // Where each signal the gateway can stand in for came from: the engine's
  // own series, or the gateway's when the engine reports none. A signal with
  // no data from either is absent. The page never labels the source; it uses
  // this to explain what only an engine can fill.
  sources?: Partial<Record<MonitorSignalSourceKey, MonitorSignalSource>>;
  // The deployment's health check.
  health: DeploymentHealth;
}

export type { MonitorRange } from "./monitorRange";
import { rangeParams, type MonitorRange } from "./monitorRange";

/** getJSON reads a JSON answer, an error answer as a localized Error. */
export async function getJSON<T>(path: string, signal?: AbortSignal): Promise<T> {
  const res = await fetch(path, { signal, credentials: "include" });
  if (!res.ok)
    throw await localizedResponseError(res, `${path}: ${res.status}`);
  return res.json();
}

async function getText(path: string, signal?: AbortSignal): Promise<string> {
  const res = await fetch(path, { signal, credentials: "include" });
  if (!res.ok)
    throw await localizedResponseError(res, `${path}: ${res.status}`);
  return res.text();
}

/** An error response as an Error in the reader's language: a product error's text, or the proxy-page explanation. */
async function kubeClusterOperation(uuid: string, op: "apply" | "remove", dryRun: boolean): Promise<KubeClusterOperation> {
  const res = await fetch(`/api/v1/kube/clusters/${encodeURIComponent(uuid)}/${op}`, {
    method: "POST",
    credentials: "include",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ dry_run: dryRun }),
  });
  if (res.status === 424) {
    // The cluster or the cloud refused: the body still says what happened to each object.
    const body = (await res.clone().json().catch(() => null)) as KubeClusterOperation | null;
    if (body && Array.isArray(body.results)) return body;
  }
  if (!res.ok) throw await localizedResponseError(res, `${op}: ${res.status}`);
  return (await res.json()) as KubeClusterOperation;
}

export async function localizedResponseError(
  response: Response,
  fallback: string,
): Promise<Error> {
  const error = await apiResponseError(response, fallback);
  if (error instanceof APIError) {
    error.message = apiErrorText((key, options) => i18n.t(key, options), error);
  } else if (error instanceof ProxyErrorPage) {
    error.message = i18n.t("httpError.proxyPage", { status: error.status, defaultValue: error.message });
  }
  return error;
}

/** A storage-files query string: each present value encoded with encodeURIComponent, empty ones left out. */
export function storageFilesQuery(values: Record<string, string | number | boolean | undefined>): string {
  return Object.entries(values)
    .filter(([, value]) => value !== undefined && value !== "")
    .map(([key, value]) => `${key}=${encodeURIComponent(String(value))}`)
    .join("&");
}

/** The link that downloads a storage file as is, or a directory packed as format (zip by default). */
export function storageFileDownloadURL(id: number, node: string | undefined, path: string, format?: StorageDownloadFormat): string {
  return `/api/v1/storages/${id}/files/download?${storageFilesQuery({ node_uuid: node, path, format })}`;
}

/** Where an upload is sent: path is the target file, or the directory an archive is unpacked into. */
export function storageFileUploadURL(id: number, node: string | undefined, path: string, extract: string, overwrite: boolean): string {
  return `/api/v1/storages/${id}/files/upload?${storageFilesQuery({ node_uuid: node, path, extract, overwrite: overwrite ? "true" : undefined })}`;
}

function wakeQuery(opts: WakeOptions): string {
  const query = new URLSearchParams();
  if (opts.role) query.set("role", opts.role);
  if (opts.partial) query.set("partial", "true");
  if (opts.wait) query.set("wait", "true");
  if (opts.yield) {
    query.set("yield", "true");
    if (opts.yieldLevel) query.set("yield_level", String(opts.yieldLevel));
  }
  const qs = query.toString();
  return qs ? `?${qs}` : "";
}

export async function sendJSON<T>(
  method: string,
  path: string,
  body?: unknown,
  signal?: AbortSignal,
): Promise<T> {
  const res = await fetch(path, {
    method,
    credentials: "include",
    headers:
      body !== undefined ? { "Content-Type": "application/json" } : undefined,
    body: body !== undefined ? JSON.stringify(body) : undefined,
    signal,
  });
  if (!res.ok) {
    throw await localizedResponseError(res, `${path}: ${res.status}`);
  }
  const text = await res.text();
  return text ? (JSON.parse(text) as T) : (undefined as T);
}

// editDeployment saves a draft edit; with dryRun it answers the plan (changes
// with their layers, the ways to apply) and writes nothing.
function editDeployment(id: number, body: DeploymentEditBody, opts: { dryRun: true }): Promise<DeploymentEditPlan>;
function editDeployment(id: number, body: DeploymentEditBody, opts?: { dryRun?: false }): Promise<DeploymentEditResult>;
function editDeployment(id: number, body: DeploymentEditBody, opts: { dryRun?: boolean } = {}) {
  return opts.dryRun
    ? sendJSON<DeploymentEditPlan>("PUT", `/api/v1/deployments/${id}?dry_run=1`, body)
    : sendJSON<DeploymentEditResult>("PUT", `/api/v1/deployments/${id}`, body);
}

// The inference catalog import and the remote sync take the same two shapes: a
// flat YAML document (JSON is YAML too), or a .gpuworks-catalog.tar.gz package,
// which a template with assets or documentation needs. The server tells them
// apart by the gzip magic; the content type only labels the upload.
export const inferenceCatalogFileAccept = ".yaml,.yml,.json,.tgz,.gz,application/yaml,text/yaml,application/json,application/gzip,application/x-gzip";

export function isInferenceCatalogPackage(file: File): boolean {
  return /\.(tgz|gz)$/i.test(file.name) || file.type === "application/gzip" || file.type === "application/x-gzip";
}

async function sendFile<T>(path: string, file: File): Promise<T> {
  const res = await fetch(path, {
    method: "POST",
    credentials: "include",
    headers: { "Content-Type": isInferenceCatalogPackage(file) ? "application/gzip" : "application/yaml" },
    body: file,
  });
  if (!res.ok) {
    throw await localizedResponseError(res, `${path}: ${res.status}`);
  }
  return res.json() as Promise<T>;
}


function eventPageQuery(page?: { limit?: number; before?: number }): string {
  const params = new URLSearchParams();
  if (page?.limit) params.set("limit", String(page.limit));
  if (page?.before) params.set("before", String(page.before));
  const query = params.toString();
  return query ? `?${query}` : "";
}

export interface MonitorSeriesParams {
  metric: string;
  by?: "node" | "deployment" | "instance" | "gpu";
  fn?: "avg" | "sum" | "max" | "rate";
  range?: MonitorRange;
  step?: string;
}

// monitorSeriesURL is exported so MetricExplorer can print the exact request
// URL it issued (plan §7 spec), without duplicating the query-building logic.
export function monitorSeriesURL(params: MonitorSeriesParams): string {
  const q = new URLSearchParams({ metric: params.metric });
  if (params.by) q.set("by", params.by);
  if (params.fn) q.set("fn", params.fn);
  if (params.range) for (const [k, v] of Object.entries(rangeParams(params.range))) q.set(k, v);
  if (params.step) q.set("step", params.step);
  return `/api/v1/monitor/series?${q.toString()}`;
}

// ServerListen is the control plane's listen configuration (server.yaml).
// Agent and gateway are empty when they inherit the console addresses.
export interface ServerListen {
  console: string[];
  agent: string[];
  gateway: string[];
  running: { addr: string; functions: string[] }[];
  restart_pending: boolean;
  config_path: string;
  editable: boolean;
  overridden: boolean;
  restart_mode: "self" | "command";
  restart_command?: string;
}

// SecuritySettings are the platform-wide security switches; only an
// administrator can change them.
/**
 * Branding is how the console presents itself. An empty field means the
 * built-in default, rendered in the viewer's language.
 */
export interface Branding {
  product_name: string;
  console_title: string;
  login_description: string;
  /** Base64 data: URL (PNG, JPEG, WebP or SVG), or "" for the built-in mark. */
  logo: string;
}

export interface SecuritySettings {
  /** Lets the playground HTML preview make network requests; off by default. */
  playground_preview_network: boolean;
  /**
   * Accepts /v1 requests that present no API key, anonymously; off by default.
   * A request that does present a key is still verified either way.
   */
  inference_open_access: boolean;
}

export interface ServerListenUpdate {
  console: string[];
  agent: string[];
  gateway: string[];
}

export const api = {
  health: () => getJSON<Health>("/api/v1/healthz"),
  engines: (targetVendor?: string) => getJSON<List<EngineProfile>>(`/api/v1/engines${targetVendor ? `?target_vendor=${encodeURIComponent(targetVendor)}` : ""}`),
  nodes: () => getJSON<List<Node>>("/api/v1/nodes"),
  nodesPage: (query: ListQuery) => getJSON<NodePage>(`/api/v1/nodes?${listQueryString(query)}`),
  nodeFacets: (filter: NodeFilter = {}) => getJSON<NodeFacets>(`/api/v1/nodes/facets?${nodeFilterString(filter)}`),
  dashboardSummary: (labelKey?: string) =>
    getJSON<DashboardSummary>(`/api/v1/dashboard/summary${labelKey ? `?label_key=${encodeURIComponent(labelKey)}` : ""}`),
  instances: () => getJSON<List<Instance>>("/api/v1/instances"),
  instanceLaunchFacts: (uuid: string) =>
    getJSON<InstanceLaunchFacts>(
      `/api/v1/instances/${encodeURIComponent(uuid)}/launch-facts`,
    ),
  agentRejects: () => getJSON<List<AgentRejectEvent>>("/api/v1/agent/rejects"),
  agentUpgradeErrors: () =>
    getJSON<Record<string, string>>("/api/v1/agent/upgrade-errors"),
  agentUpgradePreview: (body: { scope: "all" } | { node_uuids: string[] }) =>
    sendJSON<AgentUpgradePreview>("POST", "/api/v1/agent-upgrades/preview", body),
  createAgentUpgrade: (body: AgentUpgradeCreate) =>
    sendJSON<AgentUpgradeRun>("POST", "/api/v1/agent-upgrades", body),
  agentUpgradeRuns: (page = 1, pageSize = 20) =>
    getJSON<AgentUpgradePage<AgentUpgradeRun>>(`/api/v1/agent-upgrades?page=${page}&page_size=${pageSize}`),
  agentUpgradeRun: (id: number) => getJSON<AgentUpgradeRun>(`/api/v1/agent-upgrades/${id}`),
  agentUpgradeItems: (id: number, page = 1, pageSize = 20, state = "") =>
    getJSON<AgentUpgradePage<AgentUpgradeItem>>(`/api/v1/agent-upgrades/${id}/items?page=${page}&page_size=${pageSize}${state ? `&state=${encodeURIComponent(state)}` : ""}`),
  stopAgentUpgrade: (id: number) => sendJSON<AgentUpgradeRun>("POST", `/api/v1/agent-upgrades/${id}/stop`),
  continueAgentUpgradePreview: (id: number) => sendJSON<AgentUpgradePreview>("POST", `/api/v1/agent-upgrades/${id}/continue-preview`),
  releaseAgentUpgrade: (id: number, uuid: string, note: string) =>
    sendJSON<void>("POST", `/api/v1/agent-upgrades/${id}/items/${encodeURIComponent(uuid)}/release`, { note }),
  createNode: (
    input?:
      | string
      | {
          name?: string;
          connect_mode?: "agent_dial" | "server_dial";
          server_dial_listen?: string;
          host_address?: string;
          server_url?: string;
        },
  ) => {
    const body = typeof input === "string" ? { name: input } : (input ?? {});
    return sendJSON<{
      node: Node;
      token: string;
      install_command: string;
      server_url?: string;
      server_dial_address?: string;
    }>("POST", "/api/v1/nodes", body);
  },
  node: (uuid: string) => getJSON<Node>(`/api/v1/nodes/${uuid}`),
  gpus: () => getJSON<GPUListResponse>("/api/v1/gpus"),
  gpusPage: (query: ListQuery) => getJSON<GPUPage>(`/api/v1/gpus?${listQueryString(query)}`),
  dgpus: () => getJSON<DGPUListResponse>("/api/v1/dgpus"),
  nodeDoctor: (uuid: string) =>
    sendJSON<NodeDoctorReport>("POST", `/api/v1/nodes/${uuid}/doctor`),
  updateNodeAccelerator: (uuid: string, accelerator: string) =>
    sendJSON<Node>("PATCH", `/api/v1/nodes/${uuid}/accelerator`, { accelerator }),
  updateNodeRuntime: (
    uuid: string,
    runtime: "auto" | "docker" | "nerdctl" | "udocker",
  ) => sendJSON<Node>("PATCH", `/api/v1/nodes/${uuid}/runtime`, { runtime }),
  nodeFSList: (uuid: string, path: string) =>
    getJSON<FSListResponse>(
      `/api/v1/nodes/${uuid}/fs?path=${encodeURIComponent(path)}`,
    ),
  setNodeScheduling: (uuid: string, scheduling: NodeScheduling) =>
    sendJSON<Node>("PUT", `/api/v1/nodes/${encodeURIComponent(uuid)}/scheduling`, { scheduling }),
  nodeHealthChecks: (nodeUUID: string, card?: CardRef) => {
    const q = cardQuery(card);
    return getJSON<CardHealthChecks>(`/api/v1/nodes/${encodeURIComponent(nodeUUID)}/health-checks${q ? `?${q}` : ""}`);
  },
  nodeHealthRecords: (nodeUUID: string, opts: { card?: CardRef; check?: string; page: number; pageSize?: number }) => {
    const parts = [cardQuery(opts.card), opts.check ? `check=${encodeURIComponent(opts.check)}` : "", `page=${opts.page}`, `page_size=${opts.pageSize ?? 50}`].filter(Boolean);
    return getJSON<CardHealthRecords>(`/api/v1/nodes/${encodeURIComponent(nodeUUID)}/health-records?${parts.join("&")}`);
  },
  cardCheckDetail: (nodeUUID: string, card: CardRef, check: string) =>
    getJSON<CardCheckDetail>(`/api/v1/nodes/${encodeURIComponent(nodeUUID)}/health-checks/detail?${cardQuery(card)}&check=${encodeURIComponent(check)}`),
  recheckCard: (nodeUUID: string, card: CardRef) =>
    sendJSON<CardCheckDetail>("POST", `/api/v1/nodes/${encodeURIComponent(nodeUUID)}/gpus/recheck`, card.pcie_bdf ? { pcie_bdf: card.pcie_bdf } : { uuid: card.uuid }),
  setCardScheduling: (nodeUUID: string, card: { pcie_bdf?: string; uuid?: string }, scheduling: CardScheduling) =>
    sendJSON<Node>("PUT", `/api/v1/nodes/${encodeURIComponent(nodeUUID)}/gpus/scheduling`, {
      ...(card.pcie_bdf ? { pcie_bdf: card.pcie_bdf } : { uuid: card.uuid }),
      scheduling,
    }),
  nodeReplicas: (uuid: string) =>
    getJSON<NodeReplicas>(`/api/v1/nodes/${encodeURIComponent(uuid)}/replicas`),
  // evictNode takes every replica off a cordoned or draining node at once,
  // without waiting for replacements.
  evictNode: (uuid: string) =>
    sendJSON<{ evicted: string[] }>("POST", `/api/v1/nodes/${encodeURIComponent(uuid)}/evict`),
  updateNodeLabels: (uuid: string, labels: Record<string, string>) =>
    sendJSON<Node>("PUT", `/api/v1/nodes/${uuid}/labels`, { labels }),
  renameNode: (uuid: string, name: string) =>
    sendJSON<Node>("PATCH", `/api/v1/nodes/${uuid}/name`, { name }),
  deleteNode: (uuid: string, force = false) =>
    sendJSON<{ status: "deleted"; agent_uninstalled: boolean }>("DELETE", `/api/v1/nodes/${encodeURIComponent(uuid)}${force ? "?force=true" : ""}`),
  upgradeNode: (uuid: string) =>
    sendJSON<{ status: string; run_id?: number }>("POST", `/api/v1/nodes/${uuid}/upgrade`),
  precheckNode: (body: SSHPrecheckRequest) =>
    sendJSON<SSHPrecheckReport>("POST", "/api/v1/nodes/precheck", body),
  installNode: (body: SSHInstallRequest) =>
    sendJSON<SSHInstallReport>("POST", "/api/v1/nodes/install", body),
  nodePullProxy: (uuid: string) =>
    getJSON<NodePullProxy>(`/api/v1/nodes/${uuid}/pull-proxy`),
  nodeNetworks: (uuid: string) =>
    getJSON<NodeNetworks>(`/api/v1/nodes/${uuid}/networks`),
  updateManagementRoles: (nodeUUIDs: string[], allow: NetworkRole[], deny: NetworkRole[]) =>
    sendJSON<{ updated: number }>("PATCH", "/api/v1/nodes/management-roles", { node_uuids: nodeUUIDs, allow, deny }),
  setManagementRoles: (uuid: string, rolesOff: NetworkRole[]) =>
    sendJSON<NodeNetworks>("PUT", `/api/v1/nodes/${uuid}/management-roles`, { roles_off: rolesOff }),
  networks: () => getJSON<NetworkList>("/api/v1/networks"),
  network: (id: number) => getJSON<NetworkDetail>(`/api/v1/networks/${id}`),
  createNetwork: (body: NetworkRequest) =>
    sendJSON<NetworkDetail>("POST", "/api/v1/networks", body),
  updateNetwork: (id: number, body: NetworkRequest) =>
    sendJSON<NetworkDetail>("PUT", `/api/v1/networks/${id}`, body),
  deleteNetwork: (id: number) =>
    sendJSON<{ status: string }>("DELETE", `/api/v1/networks/${id}`),
  attachNetwork: (id: number, body: NetworkAttachmentRequest) =>
    sendJSON<NetworkDetail>("POST", `/api/v1/networks/${id}/attachments`, body),
  attachNetworkNodes: (id: number, nodeUUIDs: string[]) =>
    sendJSON<NetworkAttachBatchResult>("POST", `/api/v1/networks/${id}/attachments/batch`, { node_uuids: nodeUUIDs }),
  editNetworkHCAs: (id: number, body: NetworkHCARequest) =>
    sendJSON<NetworkDetail>("POST", `/api/v1/networks/${id}/hcas`, body),
  updateNetworkAttachment: (id: number, attachmentID: number, body: NetworkOverrideRequest) =>
    sendJSON<NetworkDetail>("PUT", `/api/v1/networks/${id}/attachments/${attachmentID}`, body),
  detachNetwork: (id: number, attachmentID: number) =>
    sendJSON<NetworkDetail>("DELETE", `/api/v1/networks/${id}/attachments/${attachmentID}`),
  startNetworkTest: (id: number, body: NetworkTestRequest) =>
    sendJSON<NetworkTestRun>("POST", `/api/v1/networks/${id}/tests`, body),
  estimateNetworkTest: (id: number, body: NetworkTestRequest) =>
    sendJSON<NetworkTestEstimate>("POST", `/api/v1/networks/${id}/tests/estimate`, body),
  /** Restarts the node's container runtime daemon so its proxy takes effect (admin). */
  restartNodeRuntime: (uuid: string) =>
    sendJSON<NodeRuntime>("POST", `/api/v1/nodes/${uuid}/runtime/restart`),
  pullSourceRules: () => getJSON<List<PullSourceRule>>("/api/v1/pull/source-rules"),
  createPullSourceRule: (body: PullSourceRuleRequest) =>
    sendJSON<PullSourceRule>("POST", "/api/v1/pull/source-rules", body),
  updatePullSourceRule: (id: number, body: PullSourceRuleRequest) =>
    sendJSON<PullSourceRule>("PUT", `/api/v1/pull/source-rules/${id}`, body),
  deletePullSourceRule: (id: number) =>
    sendJSON<void>("DELETE", `/api/v1/pull/source-rules/${id}`),
  /** ids lists every rule exactly once, in the order they are tried. */
  orderPullSourceRules: (ids: number[]) =>
    sendJSON<List<PullSourceRule>>("PUT", "/api/v1/pull/source-rules/order", { ids }),
  /** The management node's candidate bases for reading a source's metadata in the browser. */
  metadataSources: (source: "huggingface" | "modelscope") =>
    getJSON<MetadataSources>(`/api/v1/pull/metadata-sources?source=${encodeURIComponent(source)}`),
  testPullSourceRule: (body: PullSourceRuleTestRequest) =>
    sendJSON<SourceRuleTestResult>("POST", "/api/v1/pull/source-rules/test", body),
  pullProxies: () => getJSON<List<PullProxy>>("/api/v1/pull/proxies"),
  createPullProxy: (body: PullProxyRequest) =>
    sendJSON<PullProxy>("POST", "/api/v1/pull/proxies", body),
  updatePullProxy: (id: number, body: PullProxyRequest) =>
    sendJSON<PullProxy>("PUT", `/api/v1/pull/proxies/${id}`, body),
  deletePullProxy: (id: number) =>
    sendJSON<void>("DELETE", `/api/v1/pull/proxies/${id}`),
  /** ids lists every proxy entry exactly once, in the new order. */
  orderPullProxies: (ids: number[]) =>
    sendJSON<List<PullProxy>>("PUT", "/api/v1/pull/proxies/order", { ids }),
  syncGroups: () => getJSON<List<SyncGroup>>("/api/v1/pull/sync-groups"),
  createSyncGroup: (body: SyncGroupRequest) =>
    sendJSON<SyncGroup>("POST", "/api/v1/pull/sync-groups", body),
  updateSyncGroup: (id: number, body: SyncGroupRequest) =>
    sendJSON<SyncGroup>("PUT", `/api/v1/pull/sync-groups/${id}`, body),
  deleteSyncGroup: (id: number) =>
    sendJSON<void>("DELETE", `/api/v1/pull/sync-groups/${id}`),
  credentials: () => getJSON<List<Credential>>("/api/v1/pull/credentials"),
  createCredential: (body: CredentialRequest) =>
    sendJSON<Credential>("POST", "/api/v1/pull/credentials", body),
  updateCredential: (id: number, body: CredentialRequest) =>
    sendJSON<Credential>("PUT", `/api/v1/pull/credentials/${id}`, body),
  deleteCredential: (id: number) =>
    sendJSON<void>("DELETE", `/api/v1/pull/credentials/${id}`),
  testCredential: (id: number) =>
    sendJSON<{ ok: boolean; message?: string }>("POST", `/api/v1/pull/credentials/${id}/test`),
  nodeSSH: (uuid: string) =>
    getJSON<SSHCredentialInfo>(`/api/v1/nodes/${uuid}/ssh`),
  putNodeSSH: (
    uuid: string,
    body: {
      host: string;
      port: number;
      user: string;
      auth_mode: SSHAuthMode;
      password?: string;
      private_key_pem?: string;
      probe?: boolean;
    },
  ) => sendJSON<SSHCredentialInfo>("PUT", `/api/v1/nodes/${uuid}/ssh`, body),
  platformSSHKey: () => getJSON<PlatformSSHKey>("/api/v1/ssh-key"),
  rotatePlatformSSHKey: () => sendJSON<PlatformSSHKey>("POST", "/api/v1/ssh-key/rotate"),
  importPlatformSSHKey: (privateKeyPEM: string) =>
    sendJSON<PlatformSSHKey>("PUT", "/api/v1/ssh-key", { private_key_pem: privateKeyPEM }),
  deleteNodeSSH: (uuid: string) =>
    sendJSON<{ status: string }>("DELETE", `/api/v1/nodes/${uuid}/ssh`),
  probeNodeRecovery: (uuid: string) =>
    sendJSON<RecoveryOutcome>("POST", `/api/v1/nodes/${uuid}/recovery/probe`),

  me: () => getJSON<AuthUser>("/api/v1/auth/me"),
  /** Replaces the signed-in user's password and ends all of their sessions. */
  changePassword: (currentPassword: string, newPassword: string) =>
    sendJSON<{ status: string }>("POST", "/api/v1/auth/password", { current_password: currentPassword, new_password: newPassword }),
  models: () => getJSON<List<Model>>("/api/v1/models"),
  modelsPage: (query: ListQuery) => getJSON<ModelPage>(`/api/v1/models?${listQueryString(query)}`),
  model: (id: number) => getJSON<Model>(`/api/v1/models/${id}`),
  templateWeightModels: (templateID: number, specSource: string, specName: string) =>
    getJSON<TemplateWeightModels>(
      `/api/v1/inference-templates/weight-models?${new URLSearchParams({ template_id: String(templateID), spec_source: specSource, spec_name: specName })}`,
    ),
  inferenceTemplates: (query: ListQuery) =>
    getJSON<InferenceTemplatePage>(`/api/v1/inference-templates?${listQueryString(query)}`),
  inferenceTemplateIndex: (scope?: { vendor: string; node_uuid?: string }) =>
    getJSON<InferenceTemplateIndex>(`/api/v1/inference-templates/index${scope ? `?${new URLSearchParams({ compatibility: "detected", vendor: scope.vendor, ...(scope.node_uuid ? { node_uuid: scope.node_uuid } : {}) })}` : ""}`),
  inferenceTemplateOrganizations: (query = "") =>
    getJSON<InferenceTemplateOrganizations>(
      `/api/v1/inference-templates/organizations${query}`,
    ),
  inferenceTemplate: (modelID: string) =>
    getJSON<InferenceTemplate>(
      `/api/v1/inference-templates/model/${modelID.split("/").map(encodeURIComponent).join("/")}`,
    ),
  // The flat YAML the template editor shows. The server writes it from the
  // types the import endpoint reads, so saving it unchanged is lossless;
  // specSource keeps only the specs of that source.
  inferenceTemplateYAML: (modelID: string, specSource?: string) =>
    getText(
      `/api/v1/inference-templates/yaml?${new URLSearchParams({ model_id: modelID, ...(specSource ? { spec_source: specSource } : {}) })}`,
    ),
  inferenceTemplateChips: () =>
    getJSON<List<InferenceTemplateChip>>("/api/v1/inference-templates/chips"),
  inferenceTemplateSchema: () =>
    getJSON<unknown>("/api/v1/inference-templates/schema"),
  detectedInferenceTemplateChips: () =>
    getJSON<List<DetectedTemplateChip>>("/api/v1/inference-templates/my-chips"),
  inferenceTemplateSummary: (query = "") =>
    getJSON<InferenceTemplateSummary>(
      `/api/v1/inference-templates/summary${query}`,
    ),
  cloneInferenceTemplate: (
    sourceModelID: string,
    modelID?: string,
    displayTitle?: string,
  ) =>
    sendJSON<InferenceTemplate>("POST", "/api/v1/inference-templates/clone", {
      source_model_id: sourceModelID,
      model_id: modelID,
      display_title: displayTitle,
    }),
  // The official catalog comes from the distribution channel; the request has no body.
  syncInferenceTemplates: () =>
    sendJSON<OfficialInferenceCatalogResult>("POST", "/api/v1/inference-templates/sync"),
  // An official catalog package uploaded for a server that cannot reach the channel.
  uploadOfficialInferenceCatalog: (file: File) =>
    sendFile<OfficialInferenceCatalogResult>("/api/v1/inference-templates/official", file),
  // The sign-in page asks the server to start its update check, so the header
  // has the answer once an administrator signs in. It needs no session and
  // the server answers nothing about the result.
  prefetchUpdates: async (): Promise<void> => {
    await fetch("/api/v1/updates/prefetch", { method: "POST" });
  },
  // The server reuses its last check for up to an hour; refresh asks the
  // distribution channel again.
  updates: (refresh = false) =>
    sendJSON<UpdateStatus>("GET", refresh ? "/api/v1/updates?refresh=true" : "/api/v1/updates"),
  setUpdateChannel: (channel: string) =>
    sendJSON<{ channel: string }>("PUT", "/api/v1/updates/channel", { channel }),
  setUpdateAutoCheck: (enabled: boolean) =>
    sendJSON<{ enabled: boolean }>("PUT", "/api/v1/updates/auto-check", { enabled }),
  usageReport: () => sendJSON<UsageReportStatus>("GET", "/api/v1/usage-report"),
  setUsageReport: (enabled: boolean) =>
    sendJSON<UsageReportStatus>("PUT", "/api/v1/usage-report/enabled", { enabled }),
  // The next report exactly as the server would send it, kept as text so the
  // console shows its bytes rather than a re-serialization.
  usageReportPreview: async (): Promise<string> => {
    const res = await fetch("/api/v1/usage-report/preview", { credentials: "include" });
    if (!res.ok) throw await localizedResponseError(res, `/api/v1/usage-report/preview: ${res.status}`);
    return res.text();
  },
  notices: () => sendJSON<NoticeList>("GET", "/api/v1/notices"),
  readNotices: (ids: string[]) => sendJSON<void>("POST", "/api/v1/notices/read", { ids }),
  // The server downloads its release from the channel, swaps the binary and restarts.
  upgradeServer: () => sendJSON<ServerUpgradeStarted>("POST", "/api/v1/updates/server"),
  // The same with a release archive uploaded for a server that cannot reach the channel.
  uploadServerArchive: (file: File) =>
    sendFile<ServerUpgradeStarted>("/api/v1/updates/server/upload", file),
  importInferenceTemplates: (file: File, overwrite = false) =>
    sendFile<InferenceTemplateImportResult>(
      `/api/v1/inference-templates/import${overwrite ? "?overwrite=true" : ""}`,
      file,
    ),
  deleteInferenceTemplateSpec: (modelID: string, spec: string) =>
    sendJSON<{ deleted: boolean; model_deleted: boolean }>(
      "DELETE",
      `/api/v1/inference-templates/spec?model_id=${encodeURIComponent(modelID)}&spec=${encodeURIComponent(spec)}`,
    ),
  deleteModel: (id: number) => sendJSON<void>("DELETE", `/api/v1/models/${id}`),
  createModel: (body: {
    source: string;
    source_id: string;
    revision: string;
    name: string;
    readme?: string;
    config_raw?: string;
    weight_bytes?: number;
    logo_data?: string;
    browser_metadata?: boolean;
  }) => sendJSON<Model>("POST", "/api/v1/models", body),
  quickRegisterModel: (body: {
    source: string;
    source_id: string;
    revision?: string;
    name?: string;
    weight_bytes?: number;
    quantization?: string;
    architecture?: string;
    context_length?: number;
    artifact_subfolder?: string;
  }) => sendJSON<Model>("POST", "/api/v1/models/quick-register", body),
  refreshModelMetadata: (
    id: number,
    metadata?: {
      readme?: string;
      config_raw?: string;
      weight_bytes?: number;
      logo_data?: string;
      browser_metadata: true;
    },
  ) =>
    sendJSON<Model>("POST", `/api/v1/models/${id}/metadata/refresh`, metadata),
  updateModelSourceID: (id: number, sourceID: string) =>
    sendJSON<Model>("PATCH", `/api/v1/models/${id}`, { source_id: sourceID }),
  updateModelInferenceConfig: (
    id: number,
    operations: Pick<InferenceAPIOperation, "type">[],
    inputModalities: string[],
  ) =>
    sendJSON<Model>("PATCH", `/api/v1/models/${id}/inference-config`, {
      operations,
      input_modalities: inputModalities,
    }),
  previewLocalModel: (nodeUuid: string, path: string) =>
    sendJSON<LocalModelPreview>("POST", "/api/v1/models/local/preview", {
      node_uuid: nodeUuid,
      path,
    }),
  registerLocalModel: (body: {
    node_uuid: string;
    path: string;
    source_id?: string;
    name?: string;
    storage_id?: number;
  }) => sendJSON<Model>("POST", "/api/v1/models/local", body),
  previewURLModel: (url: string) =>
    sendJSON<URLModelPreview>("POST", "/api/v1/models/preview-url", { url }),
  registerURLModel: (body: {
    source_url: string;
    source_id?: string;
    name?: string;
  }) => sendJSON<Model>("POST", "/api/v1/models/register-url", body),
  retryModelCache: (id: number, node: string, storageID: number) => sendJSON<{ status: string }>("POST", `/api/v1/models/${id}/nodes/${encodeURIComponent(node)}/cache/retry`, { storage_id: storageID }),
  downloadModel: (id: number, nodeUuid: string) =>
    sendJSON<void>("POST", `/api/v1/models/${id}/download`, {
      node_uuid: nodeUuid,
    }),
  /** Download into a storage: a local one on each chosen node, a shared one once (no node). */
  /** status is "present" when the storage (or every chosen node) already has the model. */
  downloadModelToStorage: (id: number, body: { storage_id: number; node_uuids?: string[] }) =>
    sendJSON<{ status?: string; writer?: string } | undefined>("POST", `/api/v1/models/${id}/download`, body),
  cancelModelDownload: (id: number, body: { storage_id?: number; node_uuid?: string }) =>
    sendJSON<{ state: string; message: string }>("POST", `/api/v1/models/${id}/download/cancel`, body),
  copyModel: (id: number, body: { source: ModelLocationRef; target: ModelLocationRef; rate_limit_kib_s?: number }) =>
    sendJSON<{ state?: string; transfer_id?: string; message?: string }>("POST", `/api/v1/models/${id}/copy`, body),
  /** Delete a model's copy in a shared storage; force overrides other gpuworks using it. */
  deleteModelInStorage: (id: number, storageId: number, force = false) =>
    sendJSON<void>("DELETE", `/api/v1/models/${id}/storages/${storageId}${force ? "?force=1" : ""}`),
  /** Delete a node's local model files or cache; storageId picks one when the node has several. */
  deleteModelOnNode: (id: number, nodeUuid: string, storageId?: number) =>
    sendJSON<void>("DELETE", `/api/v1/models/${id}/nodes/${nodeUuid}${storageId ? `?storage_id=${storageId}` : ""}`),
  syncModel: (id: number, nodeUuid: string, sourceNode?: string, rateLimitKiB?: number) =>
    sendJSON<{ state: string; transfer_id?: string; message?: string }>(
      "POST",
      `/api/v1/models/${id}/nodes/${nodeUuid}/sync`,
      sourceNode ? { source_node: sourceNode, rate_limit_kib_s: rateLimitKiB ?? 0 } : {},
    ),
  cancelModelSync: (id: number, nodeUuid: string) =>
    sendJSON<{ state: string; transfer_id?: string; message?: string }>(
      "POST",
      `/api/v1/models/${id}/nodes/${nodeUuid}/sync/cancel`,
    ),
  registerModelPath: (id: number, nodeUuid: string, path: string, storageId?: number) =>
    sendJSON<void>("POST", `/api/v1/models/${id}/register-path`, {
      node_uuid: nodeUuid,
      path,
      ...(storageId ? { storage_id: storageId } : {}),
    }),

  images: (source?: string) =>
    getJSON<List<ImageSeries>>(
      `/api/v1/images${source ? `?source=${encodeURIComponent(source)}` : ""}`,
    ),
  imageVersions: (source?: string) =>
    getJSON<List<ImageCatalogEntry>>(
      `/api/v1/images/versions${source ? `?source=${encodeURIComponent(source)}` : ""}`,
    ),
  imageEngines: () => getJSON<List<ImageEngineOption>>("/api/v1/images/engines"),
  communityRepos: () =>
    getJSON<List<CommunityRepo>>("/api/v1/images/community/repos"),
  browseCommunity: (repo: string) =>
    getJSON<CommunityRepoBrowse>(
      `/api/v1/images/community/browse?repo=${encodeURIComponent(repo)}`,
    ),
  image: (id: number) => getJSON<ImageSeries>(`/api/v1/images/${id}`),
  deleteImage: (id: number) => sendJSON<void>("DELETE", `/api/v1/images/${id}`),
  deleteImageSeries: (id: number) =>
    sendJSON<void>("DELETE", `/api/v1/image-series/${id}`),
  setImageSeriesEnabled: (id: number, enabled: boolean) =>
    sendJSON<ImageSeries>("PATCH", `/api/v1/images/${id}`, { enabled }),
  setImageVersionEnabled: (id: number, enabled: boolean) =>
    sendJSON<ImageCatalogEntry>("PATCH", `/api/v1/images/versions/${id}`, {
      enabled,
    }),
  updateImageVersion: (id: number, body: Record<string, unknown>) =>
    sendJSON<ImageCatalogEntry>("PATCH", `/api/v1/images/versions/${id}`, body),
  importImages: (series: unknown[]) =>
    sendJSON<List<ImageSeries>>("POST", "/api/v1/images/import", { series }),
  // A manifest upload is the manifest JSON itself; the server answers with
  // the same counts as a sync from the distribution channel.
  importImageCatalogFile: async (file: File) => {
    const res = await fetch("/api/v1/images/import", {
      method: "POST",
      credentials: "include",
      headers: { "Content-Type": "application/json" },
      body: await file.text(),
    });
    if (!res.ok) {
      throw await localizedResponseError(
        res,
        `image catalog import: ${res.status}`,
      );
    }
    return (await res.json()) as ImageCatalogSyncResult;
  },
  // The catalog comes from the distribution channel; the request has no body.
  syncImages: () =>
    sendJSON<ImageCatalogSyncResult>("POST", "/api/v1/images/sync"),
  previewImageArchiveURL: (url: string) =>
    sendJSON<ImageArchivePreview>(
      "POST",
      "/api/v1/images/preview-archive-url",
      { url },
    ),
  syncImage: (
    id: number,
    nodeUuid: string,
    source?: string,
    fetch?: ImageFetchOptions,
  ) =>
    sendJSON<ImageNodeSyncResult>(
      "POST",
      `/api/v1/images/${id}/nodes/${nodeUuid}/sync`,
      imageFetchBody(source, fetch),
    ),
  probeImage: (id: number, nodeUuid: string, sourceNode: string) =>
    sendJSON<{ state: string }>("POST", `/api/v1/images/${id}/nodes/${nodeUuid}/sync/probe`, { source_node: sourceNode }),
  retryImage: (
    id: number,
    nodeUuid: string,
    source?: string,
    fetch?: ImageFetchOptions,
  ) =>
    sendJSON<ImageNodeSyncResult>(
      "POST",
      `/api/v1/images/${id}/nodes/${nodeUuid}/sync/retry`,
      imageFetchBody(source, fetch),
    ),
  cancelImage: (id: number, nodeUuid: string) =>
    sendJSON<ImageNodeSyncResult>(
      "POST",
      `/api/v1/images/${id}/nodes/${nodeUuid}/cancel`,
    ),
  removeImage: (id: number, nodeUuid: string) =>
    sendJSON<ImageNodeSyncResult>(
      "DELETE",
      `/api/v1/images/${id}/nodes/${nodeUuid}`,
    ),
  inspectImage: (id: number, nodeUuid: string) =>
    sendJSON<ImageInspection>(
      "POST",
      `/api/v1/images/${id}/nodes/${nodeUuid}/inspect`,
      {},
    ),
  storages: () => getJSON<List<Storage>>("/api/v1/storages"),
  storage: (id: number, allNodes = false) =>
    getJSON<Storage>(`/api/v1/storages/${id}${allNodes ? "?all_nodes=1" : ""}`),
  storageUsage: () => getJSON<List<StorageNodeUsage>>("/api/v1/storages/usage"),
  /** The model files in a storage: the shared copy, node caches, or the node copies of a local storage. */
  storageModelFiles: (id: number) => getJSON<List<StorageModelFile>>(`/api/v1/storages/${id}/model-files`),
  createStorage: (body: StorageRequest) => sendJSON<Storage>("POST", "/api/v1/storages", body),
  updateStorage: (id: number, body: StorageRequest) => sendJSON<Storage>("PATCH", `/api/v1/storages/${id}`, body),
  updateStorageNodeOverrides: (id: number, body: StorageNodeOverrideRequest) =>
    sendJSON<Storage>("PATCH", `/api/v1/storages/${id}/node-overrides`, body),
  reconcileStorageCache: (id: number, nodeUUIDs: string[], mode: "trim" | "clear") =>
    sendJSON<unknown>("POST", `/api/v1/storages/${id}/cache/reconcile`, { node_uuids: nodeUUIDs, mode }),
  deleteStorage: (id: number) => sendJSON<void>("DELETE", `/api/v1/storages/${id}`),
  probeStorage: (body: StorageProbeRequest) =>
    sendJSON<StorageProbeResult>("POST", "/api/v1/storages/probe", body),
  storageControlPlanes: (id: number) =>
    getJSON<List<StorageControlPlane>>(`/api/v1/storages/${id}/control-planes`),
  scanStorage: (id: number, body: { cursor?: string; limit?: number }) =>
    sendJSON<StorageScanResult>("POST", `/api/v1/storages/${id}/scan`, body),
  importStorage: (id: number, items: StorageImportItem[]) =>
    sendJSON<StorageImportResult>("POST", `/api/v1/storages/${id}/import`, { items }),
  clearStorageCache: (id: number, nodeUuid?: string) =>
    sendJSON<{ removed: number; freed_bytes: number }>(
      "DELETE",
      `/api/v1/storages/${id}/cache${nodeUuid ? `?node_uuid=${encodeURIComponent(nodeUuid)}` : ""}`,
    ),
  storageSettings: () => getJSON<StorageSettings>("/api/v1/storages/settings"),
  updateStorageSettings: (body: { control_plane_name?: string; reserve_free_bytes?: number }) =>
    sendJSON<StorageSettings>("PATCH", "/api/v1/storages/settings", body),
  // Storage files (ADR 0053). node is required for a local storage and left
  // out for a shared one, where the server picks a node. Uploads go through
  // storageUpload.ts (XHR, for progress); downloads are plain links.
  storageFiles: (id: number, query: { node?: string; path: string; cursor?: string; limit?: number }, signal?: AbortSignal) =>
    getJSON<StorageFileList>(`/api/v1/storages/${id}/files?${storageFilesQuery({ node_uuid: query.node, path: query.path, cursor: query.cursor, limit: query.limit })}`, signal),
  storageFilesMkdir: (id: number, node: string | undefined, path: string) =>
    sendJSON<{ path: string }>("POST", `/api/v1/storages/${id}/files/mkdir`, { ...(node ? { node_uuid: node } : {}), path }),
  storageFilesRename: (id: number, node: string | undefined, from: string, to: string) =>
    sendJSON<{ path: string }>("POST", `/api/v1/storages/${id}/files/rename`, { ...(node ? { node_uuid: node } : {}), from, to }),
  deleteStorageFile: (id: number, node: string | undefined, path: string) =>
    sendJSON<void>("DELETE", `/api/v1/storages/${id}/files?${storageFilesQuery({ node_uuid: node, path })}`),

  deployments: () => getJSON<List<Deployment>>("/api/v1/deployments"),
  deploymentsPage: (query: ListQuery) => getJSON<DeploymentPage>(`/api/v1/deployments?${listQueryString(query)}`),
  routingPolicies: () => getJSON<List<RoutingPolicyOption>>("/api/v1/routing-policies"),
  deployment: (id: number) => getJSON<Deployment>(`/api/v1/deployments/${id}`),
  setDeploymentRouting: (id: number, routing_policy: "least_request" | "prefix_affinity" | "failover") =>
    sendJSON<Deployment>("PUT", `/api/v1/deployments/${id}/routing`, { routing_policy }),
  // threshold_sec 0 keeps the current threshold.
  setDeploymentHealthCheck: (id: number, enabled: boolean, threshold_sec = 0) =>
    sendJSON<Deployment>("PUT", `/api/v1/deployments/${id}/health-check`, { enabled, threshold_sec }),
  setDeploymentTokenize: (id: number, enabled: boolean) =>
    sendJSON<Deployment>("PUT", `/api/v1/deployments/${id}/tokenize`, { enabled }),
  deploymentDraft: (id: number) =>
    getJSON<DeploymentDraft>(`/api/v1/deployments/${id}/draft`),
  editDeployment,
  exportCatalogTemplate: (id: number, includeEnv: string[] = [], signal?: AbortSignal) =>
    sendJSON<CatalogTemplateExport>(
      "POST",
      `/api/v1/deployments/${id}/catalog-template`,
      includeEnv.length ? { include_env: includeEnv } : {},
      signal,
    ),
  cloneDeployment: (id: number, name?: string) =>
    sendJSON<Deployment>(
      "POST",
      `/api/v1/deployments/${id}/clone`,
      name ? { name } : {},
    ),
  createDeployment: (body: unknown) =>
    sendJSON<Deployment>("POST", "/api/v1/deployments", body),
  stopDeployment: (id: number) =>
    sendJSON<void>("POST", `/api/v1/deployments/${id}/stop`),
  // startDeployment brings a stopped deployment back from the configuration
  // it kept; each replica starts with its full retry budget.
  startDeployment: (id: number) =>
    sendJSON<Deployment>("POST", `/api/v1/deployments/${id}/start`),
  deleteDeployment: (id: number, force = false) =>
    sendJSON<void>(
      "DELETE",
      `/api/v1/deployments/${id}${force ? "?force=true" : ""}`,
    ),
  // sleepDeployment orders every live instance to sleep, or with role only a
  // PD deployment's instances of that role. wakeOnRequest says whether a
  // request wakes what it puts to sleep; absent, the server's default for the
  // deployment (sleep_wakes_on_request).
  sleepDeployment: (id: number, level: number, mode: "wait" | "abort" = "wait", role?: string, wakeOnRequest?: boolean) =>
    sendJSON<void>("POST", `/api/v1/deployments/${id}/sleep?level=${level}&mode=${mode}${role ? `&role=${encodeURIComponent(role)}` : ""}${wakeOnRequestQuery(wakeOnRequest)}`),
  // releaseSleepHold lets requests wake the deployment's held sleepers
  // without waking them; it answers the instances released.
  releaseSleepHold: (id: number) =>
    sendJSON<{ released: string[] }>("POST", `/api/v1/deployments/${id}/wake-on-request`),
  // wakeDeployment wakes the deployment (or one PD role) all or nothing;
  // partial wakes the sleepers whose cards can take them back and lists the
  // rest; wait leaves the rest waiting for room, and yield first puts the
  // occupants of the needed cards to sleep.
  wakeDeployment: (id: number, opts: WakeOptions = {}) =>
    sendJSON<WakeResult>("POST", `/api/v1/deployments/${id}/wake${wakeQuery(opts)}`),
  // endWakeWaiting cancels the deployment's waiting wakes; it answers with
  // the instances no longer waiting.
  endWakeWaiting: (id: number) =>
    sendJSON<List<string>>("DELETE", `/api/v1/deployments/${id}/wake-waiting`),
  // sleepInstance and wakeInstance order the single-node replica an instance
  // leads; the deployment's other replicas keep serving.
  sleepInstance: (uuid: string, level: number, mode: "wait" | "abort" = "wait", wakeOnRequest?: boolean) =>
    sendJSON<void>("POST", `/api/v1/instances/${encodeURIComponent(uuid)}/sleep?level=${level}&mode=${mode}${wakeOnRequestQuery(wakeOnRequest)}`),
  wakeInstance: (uuid: string, opts: WakeOptions = {}) =>
    sendJSON<WakeResult | void>("POST", `/api/v1/instances/${encodeURIComponent(uuid)}/wake${wakeQuery(opts)}`),
  // The wait before a lost node's replica moves.
  setNodeReplacement: (id: number, body: NodeReplacement) =>
    sendJSON<NodeReplacement>("PUT", `/api/v1/deployments/${id}/node-replacement`, body),
  // updateDeploymentPd rescales a PD deployment's role units and/or fallback
  // policy (ADR 0028); mode, TP and kv_load_failure are fixed once instances
  // start, so the request never carries them.
  updateDeploymentPd: (id: number, body: PdScaleRequest) =>
    sendJSON<Deployment>("PUT", `/api/v1/deployments/${id}/pd`, body),
  // setPlacementMode hands a multi-node or PD deployment's node choice to the
  // platform ("auto": a PD unit's pinned cards are dropped) or back to the
  // operator ("manual": the nodes it has stay). Nothing restarts.
  setPlacementMode: (id: number, mode: UnitsPlacementMode) =>
    sendJSON<Deployment>("PUT", `/api/v1/deployments/${id}/placement-mode`, { mode }),
  // restartDeployment replaces every replica of a deployment, whatever its
  // shape: "rolling" (the server's default) one at a time, each draining
  // while its replacement starts; "all" stops every instance at once. Every
  // replica gets its retry budget back.
  restartDeployment: (id: number, strategy: "rolling" | "all" = "rolling") =>
    sendJSON<Deployment>("POST", `/api/v1/deployments/${id}/restart`, { strategy }),
  // retryDeploymentUnit gives one unit its retry budget back (its failed
  // attempts are retired) so the controller rebuilds it; other units are not
  // touched. role is empty outside PD.
  retryDeploymentUnit: (id: number, unit: { role: string; index: number }) =>
    sendJSON<Deployment>("POST", `/api/v1/deployments/${id}/units/retry`, unit),
  // deploymentCandidates pages through the cluster as the deployment's next
  // replica sees it, optionally only nodes of one status.
  deploymentCandidates: (id: number, page: { status?: DeploymentCandidateStatus; offset?: number; limit?: number } = {}) => {
    const query = new URLSearchParams();
    if (page.status) query.set("status", page.status);
    if (page.offset) query.set("offset", String(page.offset));
    if (page.limit) query.set("limit", String(page.limit));
    const suffix = query.toString() ? `?${query}` : "";
    return getJSON<DeploymentCandidates>(`/api/v1/deployments/${id}/candidates${suffix}`);
  },
  // setDeploymentDGPUQuota sets the dGPU memory limit a deployment's replicas
  // run under, at once: running slices are held to it in place where that is
  // safe; the answer names the ones that take it at their next restart.
  setDeploymentDGPUQuota: (id: number, body: { vram_limit_gib: number }) =>
    sendJSON<DGPUQuotaResult>("PUT", `/api/v1/deployments/${id}/dgpu`, body),
  // setDeploymentRequirements replaces where a single-node deployment's
  // replicas may run from now on; running replicas stay where they are. The
  // vendor is kept when unstated and cannot change.
  setDeploymentRequirements: (id: number, body: { node_requirements: Record<string, string[]>; placement_strategy?: string }) =>
    sendJSON<Deployment>("PUT", `/api/v1/deployments/${id}/requirements`, body),
  // suggestUnit proposes the nodes of one more multi-node replica, given the
  // replicas listed so far; nothing is written until the list is saved.
  suggestUnit: (id: number, units: PdScaleUnitInput[]) =>
    sendJSON<UnitSuggestion>("POST", `/api/v1/deployments/${id}/units/suggest`, { units }),
  // scaleReplicas sets an ordinary single-node deployment's replica count
  // (remove names the instances whose replicas go when it shrinks; place_on
  // names nodes the added replicas start on first, at most as many as are
  // added), or lists a multi-node deployment's replicas.
  scaleReplicas: (id: number, body: { replicas: number; remove?: string[]; place_on?: string[] } | { units: PdScaleUnitInput[] }) =>
    sendJSON<Deployment>("PUT", `/api/v1/deployments/${id}/replicas`, body),
  // restartInstance replaces the replica an instance fills (a follower names
  // its whole generation); drain lets it finish its requests first.
  restartInstance: (uuid: string, drain = true) =>
    sendJSON<Deployment>("POST", `/api/v1/instances/${encodeURIComponent(uuid)}/restart${drain ? "" : "?drain=false"}`),

  evaluate: (body: unknown, signal?: AbortSignal) =>
    sendJSON<EvaluateResponse>("POST", "/api/v1/evaluate", body, signal),
  distributedPreflight: (body: {
    node_uuids: string[];
    port?: number;
    master_addr?: string;
    bandwidth?: boolean;
    seconds?: number;
  }) =>
    sendJSON<DistributedPreflightResponse>(
      "POST",
      "/api/v1/deployments/preflight",
      body,
    ),

  benchmarks: () => getJSON<List<BenchmarkRun>>("/api/v1/benchmarks"),
  benchmarkMeta: () => getJSON<BenchmarkMeta>("/api/v1/benchmarks/meta"),
  benchmark: (id: number) => getJSON<BenchmarkRun>(`/api/v1/benchmarks/${id}`),
  benchmarkCurve: (id: number) =>
    getJSON<BenchmarkCurveResponse>(`/api/v1/benchmarks/${id}/curve`),
  benchmarkPointTimeline: (id: number, seq: number, signal?: AbortSignal) =>
    getJSON<BenchmarkRequestTimeline>(`/api/v1/benchmarks/${id}/points/${seq}/timeline`, signal),
  createBenchmark: (body: unknown) =>
    sendJSON<BenchmarkRun>("POST", "/api/v1/benchmarks", body),
  benchmarkModels: (body: unknown) =>
    sendJSON<{ models: string[]; normalized_url: string; notice?: string }>(
      "POST",
      "/api/v1/benchmarks/models",
      body,
    ),
  importBenchmarkDataset: async (file: File) => {
    const res = await fetch("/api/v1/benchmarks/datasets/import", {
      method: "POST",
      credentials: "include",
      body: file,
    });
    if (!res.ok)
      throw await localizedResponseError(res, `dataset import: ${res.status}`);
    return (await res.json()) as {
      kind: "imported";
      profile?: BenchmarkDatasetProfile;
      samples: Array<{
        messages: Array<{ role: string; content: string }>;
        input_tokens?: number;
        output_tokens?: number;
        wait_millis?: number;
      }>;
    };
  },
  importBenchmarkTokenizer: async (file: File) => {
    const res = await fetch("/api/v1/benchmarks/tokenizers/import", {
      method: "POST",
      credentials: "include",
      body: file,
    });
    if (!res.ok)
      throw await localizedResponseError(
        res,
        `tokenizer import: ${res.status}`,
      );
    return (await res.json()) as { tokenizer_ref: string; name: string };
  },
  cancelBenchmark: (id: number) =>
    sendJSON<BenchmarkRun>("POST", `/api/v1/benchmarks/${id}/cancel`),
  deleteBenchmark: (id: number) =>
    sendJSON<void>("DELETE", `/api/v1/benchmarks/${id}`),
  // The timeline is newest-first and cursor paginated: `before` is the id of
  // the last entry of the page already held. The server folds repeated
  // milestones, so a page is milestones, never progress samples.
  instanceEvents: (uuid: string, page?: { limit?: number; before?: number }) =>
    getJSON<CursorPage<InstanceEvent>>(
      `/api/v1/instances/${uuid}/events${eventPageQuery(page)}`,
    ),
  deploymentEvents: (id: number, page?: { limit?: number; before?: number }) =>
    getJSON<CursorPage<InstanceEvent>>(
      `/api/v1/deployments/${id}/events${eventPageQuery(page)}`,
    ),

  apiKeys: () => getJSON<List<ApiKey>>("/api/v1/apikeys"),
  createApiKey: (name: string, scope?: ApiKeyScope) =>
    sendJSON<{ key: string; name: string }>("POST", "/api/v1/apikeys", { name, ...scope }),
  setApiKeyScope: (prefix: string, scope: ApiKeyScope) =>
    sendJSON<ApiKey>("PUT", `/api/v1/apikeys/${prefix}/scope`, scope),
  revokeApiKey: (prefix: string) =>
    sendJSON<void>("DELETE", `/api/v1/apikeys/${prefix}`),
  platformTokens: () => getJSON<List<PlatformToken>>("/api/v1/platform-tokens"),
  createPlatformToken: (body: { name: string; scope: PlatformToken["scope"]; expires_in_days?: number }) =>
    sendJSON<{ token: string; item: PlatformToken }>("POST", "/api/v1/platform-tokens", body),
  deletePlatformToken: (id: number) => sendJSON<void>("DELETE", `/api/v1/platform-tokens/${id}`),
  mcpStatus: () => getJSON<MCPStatus>("/api/v1/mcp/status"),
  putMCPSettings: (enabled: boolean) => sendJSON<MCPStatus>("PUT", "/api/v1/mcp/settings", { enabled }),

  kubeClusters: () => getJSON<List<KubeCluster>>("/api/v1/kube/clusters"),
  kubeDefaults: () => getJSON<KubeDefaults>("/api/v1/kube/defaults"),
  /** Replaces the node pools a cloud entry holds; the cluster follows when the manifest is applied again. */
  setKubeClusterPools: (uuid: string, poolIds: string[]) =>
    sendJSON<KubeCluster>("PUT", `/api/v1/kube/clusters/${encodeURIComponent(uuid)}/pools`, { provider_pool_ids: poolIds }),
  createKubeCluster: (body: KubeClusterRequest) => sendJSON<KubeCluster>("POST", "/api/v1/kube/clusters", body),
  kubeManifest: (uuid: string) => getText(`/api/v1/kube/clusters/${encodeURIComponent(uuid)}/manifest`),
  revokeKubeCluster: (uuid: string) => sendJSON<KubeCluster>("POST", `/api/v1/kube/clusters/${encodeURIComponent(uuid)}/revoke`),
  deleteKubeCluster: (uuid: string) => sendJSON<void>("DELETE", `/api/v1/kube/clusters/${encodeURIComponent(uuid)}`),
  /** Applies (or, with remove, deletes) an entry's manifest on its cluster; a failure still carries the results it got to. */
  kubeClusterOperation: (uuid: string, op: "apply" | "remove", dryRun: boolean) => kubeClusterOperation(uuid, op, dryRun),
  providerClusters: (id: number) => getJSON<List<ProviderCluster>>(`/api/v1/providers/${id}/clusters`),

  providerKinds: () => getJSON<List<ProviderKindInfo>>("/api/v1/providers/kinds"),
  providers: () => getJSON<List<CloudProvider>>("/api/v1/providers"),
  provider: (id: number) => getJSON<ProviderEditConfig>(`/api/v1/providers/${id}`),
  addProvider: (body: {
    kind: string;
    name: string;
    endpoint?: string;
    api_key?: string;
    public_key?: string;
    private_key?: string;
    extra?: Record<string, string>;
  }) =>
    sendJSON<{ id: number; kind: string; name: string }>(
      "POST",
      "/api/v1/providers",
      body,
    ),
  updateProvider: (
    id: number,
    body: {
      name: string;
      endpoint?: string;
      api_key?: string;
      public_key?: string;
      private_key?: string;
      extra?: Record<string, string>;
    },
  ) =>
    sendJSON<{ id: number; kind: string; name: string }>(
      "PUT",
      `/api/v1/providers/${id}`,
      body,
    ),
  deleteProvider: (id: number) =>
    sendJSON<void>("DELETE", `/api/v1/providers/${id}`),
  syncProvider: (id: number) =>
    sendJSON<{ count: number; instances: unknown[] }>(
      "POST",
      `/api/v1/providers/${id}/sync`,
    ),
  providerInstances: (id: number) =>
    getJSON<List<ProviderInstance>>(`/api/v1/providers/${id}/instances`),
  powerProviderInstance: (
    id: number,
    externalId: string,
    action: "on" | "off",
  ) =>
    sendJSON<void>(
      `POST`,
      `/api/v1/providers/${id}/instances/${externalId}/power`,
      { action },
    ),
  installAgent: (id: number, body: ProviderInstallRequest) =>
    sendJSON<ProviderInstallResponse>(
      "POST",
      `/api/v1/providers/${id}/install`,
      body,
    ),

  metricsQuery: (
    metric: string,
    params: {
      range?: string;
      start?: number;
      end?: number;
      step?: string;
      node?: string;
      gpu?: string;
      instance?: string;
      benchmark?: string;
      point?: string;
      iface?: string;
      hca?: string;
      role?: string;
    } = {},
  ) => {
    const q = new URLSearchParams({ metric });
    if (params.range) q.set("range", params.range);
    if (params.step) q.set("step", params.step);
    if (params.node) q.set("node", params.node);
    if (params.gpu) q.set("gpu", params.gpu);
    if (params.instance) q.set("instance", params.instance);
    if (params.start) q.set("start", String(params.start));
    if (params.end) q.set("end", String(params.end));
    if (params.benchmark) q.set("benchmark", params.benchmark);
    if (params.point) q.set("point", params.point);
    if (params.iface) q.set("iface", params.iface);
    if (params.hca) q.set("hca", params.hca);
    if (params.role) q.set("role", params.role);
    return getJSON<List<MetricSeries>>(`/api/v1/metrics/query?${q.toString()}`);
  },
  logsSearch: (f: LogsFilter = {}) => {
    const p = new URLSearchParams();
    if (f.q) p.set("q", f.q);
    if (f.instance) p.set("instance", f.instance);
    if (f.instances && f.instances.length)
      p.set("instances", f.instances.join(","));
    if (f.node) p.set("node", f.node);
    if (f.level) p.set("level", f.level);
    if (f.source !== undefined) p.set("source", f.source);
    if (f.from) p.set("from", String(f.from));
    if (f.to) p.set("to", String(f.to));
    if (f.cursor_ts) p.set("cursor_ts", String(f.cursor_ts));
    if (f.cursor_id) p.set("cursor_id", String(f.cursor_id));
    if (f.limit) p.set("limit", String(f.limit));
    if (f.with_total) p.set("with_total", "1");
    return getJSON<LogsPage>(`/api/v1/logs/search?${p.toString()}`);
  },
  logsContext: (
    instance: string,
    node: string,
    ts: number,
    id: number,
    before: number,
    after: number,
  ) =>
    getJSON<LogsWindow>(
      `/api/v1/logs/context?instance=${encodeURIComponent(instance)}&node=${encodeURIComponent(node)}&ts=${ts}&id=${id}&before=${before}&after=${after}`,
    ),
  logsHistogram: (f: LogsFilter = {}, bucketMS = 60_000) => {
    const p = new URLSearchParams();
    if (f.q) p.set("q", f.q);
    if (f.instance) p.set("instance", f.instance);
    if (f.instances && f.instances.length)
      p.set("instances", f.instances.join(","));
    if (f.node) p.set("node", f.node);
    if (f.level) p.set("level", f.level);
    if (f.source !== undefined) p.set("source", f.source);
    if (f.from) p.set("from", String(f.from));
    if (f.to) p.set("to", String(f.to));
    p.set("bucket", String(bucketMS));
    return getJSON<List<LogsBucket>>(`/api/v1/logs/histogram?${p.toString()}`);
  },

  settings: () => getJSON<Record<string, any>>("/api/v1/settings"),
  dataRetention: () => getJSON<DataRetentionView>("/api/v1/data-retention"),
  experience: () => getJSON<ExperienceStatus>("/api/v1/experience"),
  setExperienceEnabled: (enabled: boolean) => sendJSON<ExperienceStatus>("PUT", "/api/v1/experience/enabled", { enabled }),
  resetExperience: () => sendJSON<ExperienceStatus>("POST", "/api/v1/experience/reset"),
  rotateExperiencePassword: () => sendJSON<ExperienceStatus>("POST", "/api/v1/experience/password"),
  putDataRetention: (value: DataRetention) => sendJSON<DataRetentionView>("PUT", "/api/v1/data-retention", value),
  dataRetentionStatus: () => getJSON<DataRetentionStatus>("/api/v1/data-retention/status"),
  putSetting: (key: string, value: unknown) =>
    sendJSON<void>("PUT", `/api/v1/settings/${key}`, value),

  aiAvailable: () => getJSON<{ available: boolean }>("/api/v1/ai/available"),
  aiConfigs: () =>
    getJSON<List<AIConfig>>("/api/v1/ai/configs"),
  createAIConfig: (body: AIConfigInput) =>
    sendJSON<AIConfig>("POST", "/api/v1/ai/configs", body),
  updateAIConfig: (id: number, body: AIConfigPatch) =>
    sendJSON<AIConfig>("PATCH", `/api/v1/ai/configs/${id}`, body),
  testAIConfig: (body: AIConfigTest) =>
    sendJSON<AIConfigTestResult>("POST", "/api/v1/ai/configs/test", body),
  deleteAIConfig: (id: number) =>
    sendJSON<AIConfigDeleteResult>("DELETE", `/api/v1/ai/configs/${id}`),
  aiAsk: (body: AIAskRequest, signal?: AbortSignal) =>
    sendJSON<AIAnswer>("POST", "/api/v1/ai/ask", body, signal),
  /**
   * aiAskStream asks with the answer streamed: onEvent sees each tool call,
   * text and reasoning delta, then one done or error event. A failure before
   * the model is asked (no endpoint, unknown subject) rejects like aiAsk.
   */
  aiAskStream: async (body: AIAskRequest, onEvent: (event: AIStreamEvent) => void, signal?: AbortSignal) => {
    const path = "/api/v1/ai/ask";
    const res = await fetch(path, {
      method: "POST",
      credentials: "include",
      headers: { "Content-Type": "application/json", Accept: "text/event-stream" },
      body: JSON.stringify(body),
      signal,
    });
    if (!res.ok) throw await localizedResponseError(res, `${path}: ${res.status}`);
    if (!res.body) throw new Error(`${path}: no response body`);
    await readSSE(res.body, (type, data) => {
      if (!["step", "text", "reasoning", "done", "error"].includes(type)) return;
      onEvent({ type, ...JSON.parse(data) } as AIStreamEvent);
    });
  },
  aiInstanceFindings: (uuid: string, signal?: AbortSignal) =>
    getJSON<List<AIFinding>>(`/api/v1/ai/instances/${encodeURIComponent(uuid)}/findings`, signal),
  aiModelFit: (id: number, signal?: AbortSignal) =>
    getJSON<AIModelFit>(`/api/v1/ai/models/${id}/fit`, signal),

  monitorOverview: (range: MonitorRange) =>
    getJSON<MonitorOverview>(
      `/api/v1/monitor/overview?${new URLSearchParams(rangeParams(range))}`,
    ),
  monitorDeployment: (id: number, range: MonitorRange) =>
    getJSON<MonitorDeployment>(
      `/api/v1/monitor/deployments/${id}?${new URLSearchParams(rangeParams(range))}`,
    ),
  monitorRequests: (id: number, query: MonitorRequestsQuery, signal?: AbortSignal) =>
    getJSON<MonitorRequests>(monitorRequestsURL(id, query), signal),
  monitorRequestLookup: (query: MonitorRequestLookupQuery, signal?: AbortSignal) =>
    getJSON<MonitorRequestLookup>(monitorRequestLookupURL(query), signal),
  monitorLatency: (id: number, range: MonitorRange) => getJSON<MonitorLatency>(monitorLatencyURL(id, range)),
  monitorDistributions: (range: MonitorRange, deploymentId?: number) =>
    getJSON<MonitorDistributions>(monitorDistributionsURL(range, deploymentId)),
  monitorMetrics: () => getJSON<List<MonitorMetricSeriesInfo>>("/api/v1/monitor/metrics"),
  serverListen: () => getJSON<ServerListen>("/api/v1/server/listen"),
  updateServerListen: (body: ServerListenUpdate) => sendJSON<ServerListen>("PUT", "/api/v1/server/listen", body),
  restartServer: () => sendJSON<{ status: string }>("POST", "/api/v1/server/restart"),
  branding: () => getJSON<Branding>("/api/v1/branding"),
  updateBranding: (body: Partial<Branding>) => sendJSON<Branding>("PATCH", "/api/v1/branding", body),
  securitySettings: () => getJSON<SecuritySettings>("/api/v1/security/settings"),
  updateSecuritySettings: (body: Partial<SecuritySettings>) => sendJSON<SecuritySettings>("PATCH", "/api/v1/security/settings", body),
  telemetryExport: () => getJSON<TelemetryExport>("/api/v1/telemetry/export"),
  updateTelemetryExport: (body: TelemetryExportUpdate) => sendJSON<TelemetryExport>("PUT", "/api/v1/telemetry/export", body),
  testTelemetryExport: (body: TelemetryExportUpdate) => sendJSON<TelemetryExportTest>("POST", "/api/v1/telemetry/export/test", body),
  debugSession: () => getJSON<DebugSessionState>("/api/v1/diagnostics/debug-session"),
  startDebugSession: (body: DebugSessionStart) => sendJSON<DebugSessionState>("POST", "/api/v1/diagnostics/debug-session", body),
  stopDebugSession: () => sendJSON<DebugSessionState>("DELETE", "/api/v1/diagnostics/debug-session"),
  diagnosticsBundles: () => getJSON<DiagnosticsBundles>("/api/v1/diagnostics/bundles"),
  startDiagnosticsBundle: (body: DiagnosticsBundleStart) => sendJSON<DiagnosticsBundle>("POST", "/api/v1/diagnostics/bundles", body),
  monitorSeries: (params: MonitorSeriesParams) =>
    getJSON<List<MonitorSeries>>(monitorSeriesURL(params)),
};

// All hooks share one EventSource. Opening one connection per hook exhausts the
// browser's per-origin HTTP/1.1 connection pool and leaves ordinary API
// refreshes pending behind the long-lived SSE streams.
let sharedEventSource: EventSource | null = null;
let sharedEventSubscribers = 0;
let eventSessionCheck: Promise<void> | null = null;

function acquireEventSource(): EventSource {
  if (!sharedEventSource) {
    sharedEventSource = new EventSource("/api/v1/events");
    sharedEventSource.onerror = () => {
      // EventSource hides HTTP statuses, so distinguish authentication expiry
      // from an ordinary connection failure through a management request.
      if (eventSessionCheck) return;
      eventSessionCheck = fetch("/api/v1/auth/me", { credentials: "include" })
        .then(async (response) => { if (!response.ok) await apiResponseError(response, ""); })
        .catch(() => { /* A network failure does not establish session expiry. */ })
        .finally(() => { eventSessionCheck = null; });
    };
  }
  sharedEventSubscribers += 1;
  return sharedEventSource;
}

function releaseEventSource(source: EventSource) {
  sharedEventSubscribers = Math.max(0, sharedEventSubscribers - 1);
  if (sharedEventSubscribers === 0 && sharedEventSource === source) {
    source.close();
    sharedEventSource = null;
  }
}

// useEvents subscribes to the shared SSE stream and invokes onChange whenever
// a matching topic event arrives (events carry no payload; callers refetch).
export function useEvents(topic: string, onChange: () => void) {
  const cb = useRef(onChange);
  cb.current = onChange;
  useEffect(() => {
    const handler = () => cb.current();
    return subscribeEvents(topic, handler, handler);
  }, [topic]);
}

export interface ChangeEvent {
  key?: string;
  reason?: string;
}

export function useEventPayload(
  topic: string,
  onEvent: (event: ChangeEvent) => void,
) {
  const cb = useRef(onEvent);
  cb.current = onEvent;
  useEffect(() => {
    const handler = (raw: Event) => {
      try {
        cb.current(
          JSON.parse((raw as MessageEvent<string>).data) as ChangeEvent,
        );
      } catch {
        cb.current({});
      }
    };
    return subscribeEvents(topic, handler, () => cb.current({ reason: "session-restored" }));
  }, [topic]);
}

function subscribeEvents(topic: string, handler: EventListener, restored: () => void): () => void {
  let source: EventSource | null = null;
  const stop = () => {
    if (!source) return;
    source.removeEventListener(topic, handler);
    releaseEventSource(source);
    source = null;
  };
  const start = () => {
    if (source) return;
    source = acquireEventSource();
    source.addEventListener(topic, handler);
  };
  if (!isSessionExpired()) start();
  const unsubscribe = subscribeSessionChanges((change) => {
    if (change === "expired") stop();
    else { start(); restored(); }
  });
  return () => { unsubscribe(); stop(); };
}

// usePolling refetches on an interval as an SSE fallback and for freshness.
// Keep the implicit dependency list referentially stable. A default `[]`
// parameter is recreated on every render, which makes the effect below run
// again whenever a request updates state and can turn polling into a tight
// request loop.
const EMPTY_POLLING_DEPS: readonly unknown[] = [];

export function usePolling<T>(
  fetcher: () => Promise<T>,
  intervalMs: number,
  deps?: unknown[],
) {
  const [data, setData] = useState<T | null>(null);
  const [error, setError] = useState<Error | null>(null);
  const [loading, setLoading] = useState(true);
  const [updatedAt, setUpdatedAt] = useState<Date | null>(null);
  const fRef = useRef(fetcher);
  fRef.current = fetcher;
  const aliveRef = useRef(true);
  const inFlightRef = useRef<Promise<void> | null>(null);
  const trailingRef = useRef(false);
  const load = async (foreground: boolean) => {
    if (foreground) setLoading(true);
    if (inFlightRef.current) {
      trailingRef.current = true;
      if (foreground) {
        return inFlightRef.current.finally(() => {
          if (aliveRef.current) setLoading(false);
        });
      }
      return inFlightRef.current;
    }
    const task = (async () => {
      do {
        trailingRef.current = false;
        try {
          const d = await fRef.current();
          if (aliveRef.current) {
            setData(d);
            setError(null);
            setUpdatedAt(new Date());
          }
        } catch (e) {
          if (aliveRef.current) setError(e as Error);
        }
      } while (trailingRef.current && aliveRef.current);
    })().finally(() => {
      inFlightRef.current = null;
      if (aliveRef.current && foreground) setLoading(false);
    });
    inFlightRef.current = task;
    return task;
  };
  useEffect(() => {
    aliveRef.current = true;
    void load(true);
    const visibleTick = () => {
      if (!document.hidden) void load(false);
    };
    const id =
      intervalMs > 0 ? setInterval(visibleTick, intervalMs) : undefined;
    if (intervalMs > 0)
      document.addEventListener("visibilitychange", visibleTick);
    return () => {
      aliveRef.current = false;
      if (id != null) clearInterval(id);
      if (intervalMs > 0)
        document.removeEventListener("visibilitychange", visibleTick);
    };
  }, deps ?? EMPTY_POLLING_DEPS);
  const refetch = async () => {
    await load(true);
  };
  const refresh = async () => {
    await load(false);
  };
  return { data, error, loading, updatedAt, refetch, refresh };
}
