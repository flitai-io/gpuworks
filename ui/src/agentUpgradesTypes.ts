import type { ServerBuildInfo } from "./serverBuildInfo";

export interface AgentUpgradeCounts {
  total: number;
  processed: number;
  succeeded: number;
  failed: number;
  skipped: number;
  cancelled: number;
  interrupted: number;
  unconfirmed: number;
  queued: number;
  active: number;
}

export interface AgentUpgradeBlocker {
  run_id: number;
  node_uuid: string;
  name?: string;
  reason: string;
}

export type AgentVersionChange = "upgrade" | "downgrade" | "different_build";

export interface AgentUpgradeCandidate {
  node_uuid: string;
  name: string;
  from_version: string;
  version_change?: AgentVersionChange;
  downgrade?: boolean;
  os: string;
  arch: string;
  transport: string;
  eligibility: string;
  reason?: string;
}

export interface AgentUpgradePreview {
  target_build?: ServerBuildInfo;
  target_version: string;
  manifest_fingerprint: string;
  node_uuids: string[];
  nodes: AgentUpgradeCandidate[];
  counts: Record<string, number>;
  blocked_by_item?: AgentUpgradeBlocker | null;
}

export interface AgentUpgradeCreate {
  node_uuids: string[];
  expected_target_version: string;
  expected_manifest_fingerprint: string;
  idempotency_key: string;
  source_run_id?: number;
}

export interface AgentUpgradeRun {
  id: number;
  target_version: string;
  state: string;
  counts: AgentUpgradeCounts;
  blocked_by_item?: AgentUpgradeBlocker | null;
  requested_by?: string;
  created_at?: string;
  source_run_id?: number;
  stop_requested_at?: string;
  stop_requested_by?: string;
  pause_reason?: string;
  interrupted_reason?: string;
}

export interface AgentUpgradeItem {
  run_id: number;
  node_uuid: string;
  name: string;
  from_version: string;
  target_version: string;
  current_version?: string;
  observed_version?: string;
  phase: string;
  reason?: string;
  error?: string;
  started_at?: string;
  deadline_at?: string;
  finished_at?: string;
  verified_at?: string;
  verified_by?: string;
  verification_code?: string;
  verification_note?: string;
}

export interface AgentUpgradePage<T> {
  items: T[];
  total: number;
  page: number;
  page_size: number;
  blocked_by_item?: AgentUpgradeBlocker | null;
}

export function agentUpgradeRunning(run: AgentUpgradeRun): boolean {
  return ["running", "stopping", "paused"].includes(run.state);
}
