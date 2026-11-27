import type { KindResource } from "./platform";
import type { ProductWarning } from "./scheduling";

/** The accelerator vendor of a cloud node pool (ProviderClusterPool.vendor). */
export type KubeVendor = "nvidia" | "ascend" | "hygon" | "ppu" | "amd" | "kunlunxin";

/** How many of an entry's nodes have one vendor's cards (dto.KubeVendorNodes); vendor "" is nodes that reported no cards. */
export interface KubeVendorNodes {
  vendor: string;
  nodes: number;
}

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
  /** The agent's directory on the host. */
  state_dir: string;
  /**
   * Applies to the instance pods of NVIDIA nodes only (other vendors ignore it); empty uses the cluster's "nvidia"
   * RuntimeClass when it has one and the node's default runtime otherwise.
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
  /** PriorityClass value of the hold and instance pods (larger first; ordinary pods are 0). */
  priority: number;
  /** Whether the scheduler may evict lower-priority pods for a hold pod. */
  allow_preemption: boolean;
  /** Empty until the first node enrolled. */
  cluster_uid: string;
  revoked: boolean;
  node_count: number;
  /** The joined nodes by the vendor of their cards, most first; each agent detects its own node's. */
  vendors: KubeVendorNodes[];
  created_at: string;
  /** Carries the enrollment token; administrators only. */
  apply_command?: string;
  /** Fetches and runs, with bash, the check that says node by node how the applied agents stand; carries the enrollment token, administrators only. */
  verify_command?: string;
  /** kubectl commands, in order, that uninstall the entry's agents and the pods they started from the cluster; administrators only, also for a revoked entry. */
  remove_commands?: string[];
  /** No node joined for longer than the dictionary's stalled_after_seconds: worth troubleshooting. */
  stalled: boolean;
  /** Where the entry stands, the one value the console shows and filters by. */
  state: KubeClusterState;
  /** The platform type the entry is listed under when connected on its own, and what it connects. */
  category: string;
  resources: KindResource[];
  /** The cloud node pools a cloud entry holds, ascending; empty for an entry made by hand. */
  pool_ids: string[];
  /** Other entries that use this namespace and may be of the same cluster (server-computed). */
  namespace_shared_with: string[];
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
  namespace: string;
  state_dir: string;
  engine_ports: string;
  idle_return_seconds: number;
  /** The idle_return_seconds value that keeps borrowed cards. */
  idle_return_keep: number;
  agent_image: string;
  stalled_after_seconds: number;
  /** The registry an entry made by hand pulls the published agent image from. */
  agent_image_source: string;
  /** The registries the agent image is published to, the default first. */
  agent_image_sources: KubeAgentImageSource[];
  /** Default PriorityClass value and its upper bound (the lower bound is 0). */
  priority: number;
  priority_max: number;
  allow_preemption: boolean;
  /** The codes each check script prints, in the order to explain them. */
  check_codes: KubeCheckCodes;
}

/** The codes the pre-check and the check after an apply print (dto.KubeCheckCodes). */
export interface KubeCheckCodes {
  precheck: string[];
  verify: string[];
}

/** The form fields a pre-check is rendered for (dto.KubePrecheckRequest); omitted ones take a new entry's defaults. */
export interface KubePrecheckRequest {
  server_url: string;
  namespace?: string;
  agent_image?: string;
  agent_image_source?: string;
  node_selector?: string;
  image_pull_secret?: string;
}

/** How the agents of an entry made from a cloud cluster stand, node by node (dto.KubeRollout); ask again after poll_after_seconds while not done. */
export interface KubeRollout {
  done: boolean;
  poll_after_seconds?: number;
  /** Agent pods the DaemonSet wants; null until counted or without a DaemonSet. */
  desired: number | null;
  rows: KubeCheckRow[];
}

/** One line of a check: a node and its pod (neither for the DaemonSet as a whole), a code of check_codes.verify, and Kubernetes' detail (dto.KubeCheckRow). */
export interface KubeCheckRow {
  node?: string;
  pod?: string;
  code: string;
  detail?: string;
}

/** The pre-check script and what it checks (dto.KubePrecheck). */
export interface KubePrecheck {
  script: string;
  file_name: string;
  /** The agent image every selected node pulls. */
  image: string;
  /** The host:port every selected node dials. */
  server: string;
}

/** A registry the agent image is published to (dto.KubeAgentImageSource). */
export interface KubeAgentImageSource {
  id: string;
  /** The image there, at the server's runtime revision. */
  image: string;
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

export type KubeObjectAction = "create" | "update" | "replace" | "delete" | "absent" | "failed" | "not_tried" | "dependent";

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
  /** Omitted: the dictionary's agent_image_source for an entry made by hand, the registry its region reaches for one from a cloud cluster. */
  agent_image_source?: string;
  node_selector?: string;
  /** The agent's directory on the host; omitted means /opt/gpuworks. */
  state_dir?: string;
  /** Applies to NVIDIA nodes' instance pods only; omitted lets the agents decide on each node. */
  runtime_class?: string;
  /** Omitted pulls the agent image without credentials. */
  image_pull_secret?: string;
  /** Omitted means the agents detect it (containerd, k3s, RKE2, microk8s). */
  containerd_socket?: string;
  /** "https://host:port"; omitted means the cluster's "kubernetes" Service. */
  api_server?: string;
  engine_ports?: string;
  idle_return_seconds?: number;
  /** Omitted take GET /kube/defaults. */
  priority?: number;
  allow_preemption?: boolean;
  /** The cloud account cluster the entry is made from; both or neither. */
  provider_id?: number;
  provider_cluster_id?: string;
  /** The cluster's GPU node pools the entry holds (required with a source; pools of any vendors). */
  provider_pool_ids?: string[];
}

/** Edits an entry (dto.KubeClusterUpdate): a field omitted keeps the entry's value, an empty string clears an optional one. The namespace, server URL and state directory are fixed at creation. */
export interface KubeClusterUpdate {
  name?: string;
  /** Refused for an entry made from a cloud cluster: its nodes are chosen by node pool. */
  node_selector?: string;
  agent_image?: string;
  agent_image_source?: string;
  image_pull_secret?: string;
  runtime_class?: string;
  containerd_socket?: string;
  api_server?: string;
  engine_ports?: string;
  idle_return_seconds?: number;
  priority?: number;
  allow_preemption?: boolean;
  /** Accepts that nodes a narrower node_selector leaves out still run instances (see KubeSelectionDrops); not a setting of the entry. */
  confirm_drop_instances?: boolean;
}

/** A node a narrower node selection would leave out while it still runs instances (dto.KubeSelectionDrop). */
export interface KubeSelectionDrop {
  node_uuid: string;
  node_name: string;
  /** The node's name in the Kubernetes cluster. */
  kube_node: string;
  /** The deployments whose instances run there; empty when none is on record. */
  deployments: string[];
}

/** The params of the 409 KUBE_SELECTION_DROPS_INSTANCES an edit or a pools change is refused with until confirmed. */
export interface KubeSelectionDrops {
  nodes: KubeSelectionDrop[];
  /** False when no agent of the cluster could be asked: the nodes only may be affected. */
  verified: boolean;
}

/** An instance a confirmed node selection change asked to stop because its node leaves the selection (dto.KubeStoppedInstance). */
export interface KubeStoppedInstance {
  instance_uuid: string;
  deployment: string;
  node_uuid: string;
}

/** The answer to a node pools change (dto.KubeClusterPools): the entry, and the instances asked to stop. */
export interface KubeClusterPools extends KubeCluster {
  stopped_instances?: KubeStoppedInstance[];
}

/** The answer to an edit (dto.KubeClusterEdit). */
export interface KubeClusterEdit {
  cluster: KubeCluster;
  /** The JSON names of the fields that changed, sorted. */
  changed: string[];
  /** The cluster still runs the old manifest until it is applied again. */
  reapply: boolean;
  /** For an entry the server cannot apply: the kubectl commands, in order, that bring the cluster in line. */
  commands: string[];
  /** The instances a confirmed narrower node selector asked to stop (always sent; an empty list when none). */
  stopped_instances?: KubeStoppedInstance[];
  /** What the edit allowed but the administrator should know, e.g. KUBE_ENGINE_PORTS_OUTSIDE (always sent; an empty list when none). */
  warnings?: ProductWarning[];
}

/** Where a cluster entry stands: revoked, stalled (no node joined for too long), pending (no node yet) or active. */
export type KubeClusterState = "revoked" | "stalled" | "pending" | "active";
