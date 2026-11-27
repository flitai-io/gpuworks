import type { CheckStatus } from "../checkStatus";

/** One value the agent read from the vendor tool, named as the tool names it. An empty name is a sentence from an older agent. */
export interface GPUHealthField {
  name: string;
  value: string;
}

// Card health checks: GET /nodes/{uuid}/health-checks lays a node's card
// checks out as a table, the server deciding the columns, each cell's status,
// code and staleness, the worst status per card and whether a recheck is
// offered.
export interface CardHealthColumn {
  id: string;
  period_seconds: number;
}
export interface CardHealthCell {
  check: string;
  status: CheckStatus;
  /** What a warning or failure found, or why a skipped check did not run. */
  code?: string;
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
  worst: CheckStatus;
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
  /** The server's explanation when checks are unavailable. */
  notice?: { code: string; level: "info" | "warning" };
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
  status: "warn" | "fail";
  code?: string;
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
  status: CheckStatus;
  code?: string;
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

/** Vendor commands for a card's health finding, derived by the server. */
export interface GPUHealthGuide {
  probe: string;
  recheck?: string;
  actions?: string[];
}
