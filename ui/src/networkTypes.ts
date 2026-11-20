// Wire types for /api/v1/networks and /api/v1/nodes/{uuid}/networks. Role and type values come from the server
// dictionary (role_options / type_options); the unions here only name them.

export type NetworkRole = "storage_sync" | "compute" | "serving";
export type NetworkType = "host_ethernet" | "host_rdma" | "accelerator_rdma" | "accelerator_hccs";
export type LinkLayer = "infiniband" | "roce" | "hccs";

export interface NetworkNodeFilters {
  roles?: string[];
  source?: string[];
}

export interface NetworkWarning {
  code: "conflict" | "override_missing" | "link_layer_mismatch" | "rdma_no_hca" | "bootstrap_missing" | "hca_unusable" | "hca_mismatch" | string;
  params?: Record<string, string>;
}

export interface NetworkAttachment {
  id: number;
  target: "node" | "label";
  node_uuid?: string;
  node_name?: string;
  label_key?: string;
  label_value?: string;
  interface?: string;
  ip?: string;
  hcas: string[];
  created_at: string;
}

export interface Network {
  id: number;
  name: string;
  description: string;
  roles: NetworkRole[];
  type: NetworkType;
  /** Host Ethernet: the data subnet; a network that uses rails: the bootstrap subnet (empty: inferred). */
  cidr: string;
  /** Host Ethernet only: an interface name glob. */
  interface: string;
  /** The management network: read-only, listed as id 0. */
  builtin: boolean;
  attachments: NetworkAttachment[];
  resolved_node_count: number;
  warning_node_count: number;
  created_at: string;
  updated_at: string;
}

export interface NetworkList {
  items: Network[];
  roles: NetworkRole[];
  types: NetworkType[];
  /** Per type, the roles a network of that type may carry. */
  type_roles: Record<string, NetworkRole[]>;
}

/** Why an RDMA port cannot carry RDMA traffic; absent when it can. */
export type RailGap = "inactive" | "no_ipv4" | "unknown";

/** One RDMA port a node uses on an RDMA network. */
export interface NetworkRail {
  name: string;
  /** Its interface's IPv4 address; empty for InfiniBand without IPoIB. */
  ip: string;
  netdev: string;
  link_layer: LinkLayer | "";
  state: string;
  speed_gbps: number;
  gap?: RailGap;
}

/** Whether rails or bootstrap are inferred or pinned on the node. */
export type EndpointSource = "auto" | "manual";

export interface NetworkNode {
  node_uuid: string;
  node_name: string;
  /** The node's written labels. */
  labels: Record<string, string>;
  roles: NetworkRole[];
  attachment_id: number;
  source: "node" | "label" | "management";
  label_key?: string;
  label_value?: string;
  /** The node's address on the network: the bootstrap address on RDMA networks. */
  interface: string;
  ip: string;
  hcas: NetworkRail[];
  link_layer?: LinkLayer;
  hca_source?: EndpointSource;
  bootstrap_source?: EndpointSource;
  tools: string[];
  warnings: NetworkWarning[];
  /** Management network only: the roles switched off on this node. */
  roles_off?: NetworkRole[];
}

export type NetworkTestStatus = "passed" | "partial" | "failed";
export type NetworkTestSide = "source" | "target";

/** One rail of an rdma_tool pair; a rail with no peer has only one side. */
export interface NetworkTestRail {
  rail: string;
  source_ip?: string;
  target_rail?: string;
  target_ip?: string;
  gbps: number;
  /** A card test's outcome: whether the target card answered, and how fast. */
  reachable?: boolean;
  rtt_ms?: number;
  code?: string;
  side?: NetworkTestSide;
  error?: string;
}

export interface NetworkTestPair {
  source_uuid: string;
  source_name: string;
  target_uuid: string;
  target_name: string;
  target?: string;
  status: NetworkTestStatus;
  passed: boolean;
  reachable: boolean;
  rtt_ms_p50: number;
  rtt_ms_p99: number;
  gbps: number;
  /** Mean of measured passing rails for RDMA bandwidth tests. */
  avg_rail_gbps?: number;
  /** Why the pair failed and which node it is about; runs stored before codes carry only error. */
  code?: string;
  side?: NetworkTestSide;
  error?: string;
  rails?: NetworkTestRail[];
  /** Card tests only. */
  card_shape?: CardShape;
}

export type NetworkTestKind = "tcp" | "rdma_tool" | "card_ping" | "card_matrix";
/**
 * How two nodes' cards reach each other in a card test, read by the server
 * from the run's results: every pair (full), exactly the same-named cards
 * (diagonal), some (mixed), none, or not probed at all (untested).
 */
export type CardShape = "full" | "diagonal" | "mixed" | "none" | "untested";

/** The size of a test request, computed by the server without starting it. */
export interface NetworkTestEstimate {
  kind: NetworkTestKind;
  pattern: NetworkTestPattern;
  node_count: number;
  pair_count: number;
  /** Card tests: a round lasts one probe's time limit at most, so seconds is the longest the run can take. */
  card?: { rounds: number; probes: number; seconds: number; max_seconds: number };
}
export type NetworkTestPattern = "mesh" | "ring";

export interface NetworkTestRun {
  id: number;
  status: "running" | "done" | "interrupted";
  kind: NetworkTestKind;
  pattern: NetworkTestPattern;
  scope: "all" | "nodes" | "label";
  /** The label a label-scoped run selected its nodes by; other scopes carry neither. */
  label_key?: string;
  label_value?: string;
  bandwidth: boolean;
  node_count: number;
  pair_count: number;
  passed_count: number;
  partial_count: number;
  /** Rails of an RDMA bandwidth run; of a card run, the card pairs pinged and those that answered. */
  rail_count: number;
  rail_passed_count: number;
  /** A finished card run: the shape most node pairs have. */
  card_shape?: CardShape;
  /** The nodes under test, fixed when the run starts; present while it is still running. */
  node_uuids: string[];
  pairs: NetworkTestPair[];
  started_at: string;
  finished_at?: string;
}

export interface NetworkTestRequest {
  kind?: NetworkTestKind;
  pattern?: NetworkTestPattern;
  scope: "all" | "nodes" | "label";
  node_uuids?: string[];
  label_key?: string;
  label_value?: string;
  bandwidth?: boolean;
  seconds?: number;
}

export interface NetworkDetail extends Network {
  nodes: NetworkNode[];
  role_options: NetworkRole[];
  type_options: NetworkType[];
  type_roles: Record<string, NetworkRole[]>;
  test_supported: boolean;
  latest_test: NetworkTestRun | null;
}

export interface NetworkRequest {
  name: string;
  description?: string;
  roles: NetworkRole[];
  type: NetworkType;
  cidr?: string;
  interface?: string;
}

export interface NetworkAttachmentRequest {
  node_uuid?: string;
  label_key?: string;
  label_value?: string;
  interface?: string;
  ip?: string;
  hcas?: string[];
}

export interface NetworkOverrideRequest {
  interface?: string;
  ip?: string;
  hcas?: string[];
}

export interface NetworkAttachBatchItem {
  node_uuid: string;
  status: "attached" | "already" | "failed";
  code?: string;
  params?: Record<string, unknown>;
  error?: string;
}

export interface NetworkAttachBatchResult {
  results: NetworkAttachBatchItem[];
  network: NetworkDetail;
}

export interface NetworkHCARequest {
  op: "add" | "remove" | "reset";
  hca?: string;
  node_uuid?: string;
}

export interface NetInterface {
  name: string;
  state: string;
  mtu: number;
  speed_mbps: number;
  mac: string;
  addrs: string[];
  /** Latest rates in bytes/s; absent when nothing arrived recently. */
  rx_bytes_per_sec?: number;
  tx_bytes_per_sec?: number;
}

export interface RDMAPort {
  device: string;
  port: number;
  state: string;
  link_layer: string;
  rate_gbps: number;
  netdev: string;
  /** Its interface's IPv4 address, and why it cannot carry RDMA traffic. */
  ip: string;
  gap?: RailGap;
  rx_bytes_per_sec?: number;
  tx_bytes_per_sec?: number;
}

/** One network port an accelerator card owns, including an A3/910C HCCS vNIC when reported. */
export interface AcceleratorPort {
  name: string;
  index: number;
  ip: string;
  netmask: string;
  /** UP | DOWN, empty when unknown. */
  state: string;
  speed_gbps: number;
  hccs_ip?: string;
  hccs_netmask?: string;
  hccs_state?: string;
  /** Why the card's port cannot carry traffic; absent when it can. */
  gap?: RailGap;
  hccs_gap?: RailGap;
}

export interface NodeLoadedNetwork {
  role: NetworkRole;
  network_id: number;
  network_name: string;
  network_type: NetworkType;
  attachment_id: number;
  source: "node" | "label" | "management";
  label_key?: string;
  label_value?: string;
  interface: string;
  ip: string;
  hcas: NetworkRail[];
  link_layer?: LinkLayer;
  hca_source?: EndpointSource;
  bootstrap_source?: EndpointSource;
  warnings: NetworkWarning[];
}

export interface ServingForward {
  address: string;
  mode?: "direct" | "unreachable";
  reason?: string;
  at?: string;
}

export interface NodeNetworks {
  networks: NodeLoadedNetwork[];
  serving_forward: ServingForward | null;
  management: NodeManagement;
  inventory_reported: boolean;
  interfaces: NetInterface[];
  rdma_ports: RDMAPort[];
  accelerator_ports: AcceleratorPort[];
}

/** A node's management network; interface and ip are empty until reported. */
export interface NodeManagement {
  interface: string;
  ip: string;
  roles_off: NetworkRole[];
  remote_addr?: string;
  behind_nat: boolean;
}
