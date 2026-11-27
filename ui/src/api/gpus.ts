import type { ProductMessage } from "../apiError";
import type { ListPage } from "./http";
import type { GPUHealthField, GPUHealthGuide } from "./cardHealth";

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
  /** An operator enabled scheduling; this persists until disabled, across reports and restarts. */
  scheduling_allowed?: boolean;
  /** What an operator may do to the card now, decided by the server. */
  scheduling_actions?: CardScheduling[];
  /** The extension source that holds the card out of scheduling; absent when none does. */
  scheduling_held_by?: string;
  /** Why an extension holds the card out of scheduling; it cannot be made schedulable meanwhile. */
  scheduling_hold?: ProductMessage;
  health_status?: string;
  health_code?: string;
  health_message?: string;
  health_evidence?: GPUHealthField[];
  health_guide?: GPUHealthGuide;
}

/** The facts a capability's reason_code names; probe_detail is the agent's own probe output. */
export interface PartitionReasonParams {
  runtime?: string;
  supported_runtimes?: string[];
  missing_assets?: string[];
  gpu_index?: number;
  probe_detail?: string;
}

export interface PartitionCapability {
  backend: string;
  display_name: string;
  vendor: string;
  maturity: "release" | "experimental" | string;
  status: "ready" | "unavailable";
  /** English fallback of reason_code; render reason_code instead. */
  reason?: string;
  /** Why an unavailable capability is not ready (PARTITION_*). */
  reason_code?: string;
  reason_params?: PartitionReasonParams;
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
  /** An operator enabled scheduling; this persists until disabled, across reports and restarts. */
  scheduling_allowed?: boolean;
  /** What an operator may do to the card now, decided by the server. */
  scheduling_actions?: CardScheduling[];
  /** The extension source that holds the card out of scheduling; absent when none does. */
  scheduling_held_by?: string;
  /** Why an extension holds the card out of scheduling; it cannot be made schedulable meanwhile. */
  scheduling_hold?: ProductMessage;
  allocation_state: "available" | "partial" | "allocated" | "unusable" | "unknown";
  allocated_bytes: number;
  /** The residue sleeping instances keep. What is left of the total after
   * this, allocated_bytes and available_bytes is withheld for another
   * reason (processes outside the platform, an unreachable node). */
  sleeping_bytes: number;
  /** The memory of processes outside the platform that the free figure
   * leaves out; 0 when none is counted (holders of unknown memory too).
   * What is withheld beyond it on an online node's card is the headroom the
   * deployment policy keeps beside them. */
  foreign_bytes: number;
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
