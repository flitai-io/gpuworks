// Client for usage analytics (GET /api/v1/analytics/*, docs/reference/api.md
// "Analytics"). The wire types mirror internal/server/api/dto/analytics.go.
import { getJSON } from "./api";

export type Granularity = "hour" | "day";
export type GpuGroupBy = "model" | "node" | "gpu";
export type GpuMetric = "util" | "power" | "temp" | "mem";
export type UsageSource = "api" | "playground" | "batch" | "evaluation";

/** The span a statistics answer covers, as the server resolved it. */
export interface AnalyticsRange {
  from_ms: number;
  to_ms: number;
  granularity: Granularity;
  time_zone: string;
  /** The oldest bucket the statistic still has at this granularity; 0 when none. */
  earliest_ms: number;
  /** Buckets before this were filled in once from the request log. */
  live_since_ms: number;
  /** Bucket starts (Unix ms); series align with them. */
  buckets: number[];
}

/** A usage total: tokens add up the requests that reported usage. */
export interface UsageCounts {
  requests: number;
  no_usage: number;
  prompt_tokens: number;
  completion_tokens: number;
  reasoning_tokens: number;
  reasoning_reported: number;
  cached_tokens: number;
  cache_reported: number;
  cache_reported_prompt: number;
}

/** One key x user x deployment x source over the range. */
export interface UsageRow extends UsageCounts {
  api_key_id: number;
  api_key_name: string;
  api_key_prefix: string;
  api_key_revoked: boolean;
  /** Deleted before keys were kept on revoking. */
  api_key_deleted: boolean;
  user_id: number;
  user_name: string;
  deployment_id: number;
  deployment_name: string;
  served_name: string;
  deployment_deleted: boolean;
  source: UsageSource;
  first_ms: number;
  last_ms: number;
}

export interface UsageAnalytics {
  range: AnalyticsRange;
  totals: UsageCounts;
  series: UsageCounts[];
  rows: UsageRow[];
}

/** Outcomes and latency percentiles; null without any sample. */
export interface QualityFigures {
  requests: number;
  completed: number;
  length: number;
  client_closed: number;
  interrupted: number;
  error: number;
  ttft_p50_ms: number | null;
  ttft_p95_ms: number | null;
  e2e_p50_ms: number | null;
  e2e_p95_ms: number | null;
  output_tokens_per_sec: number | null;
}

export interface QualityRow extends QualityFigures {
  deployment_id: number;
  deployment_name: string;
  served_name: string;
  deployment_deleted: boolean;
}

export interface QualityAnalytics {
  range: AnalyticsRange;
  totals: QualityFigures;
  series: QualityFigures[];
  rows: QualityRow[];
  /** The totals split by the priority class requests ran at; "" is unknown (recorded before classes). */
  classes: QualityClassRow[];
}

export interface QualityClassRow extends QualityFigures {
  priority_class: string;
}

/** A card's or a group's statistics; null without samples of that kind. */
export interface GPUFigures {
  util_avg: number | null;
  util_max: number | null;
  mem_used_avg_bytes: number | null;
  mem_used_max_bytes: number | null;
  mem_total_bytes: number | null;
  power_avg_watts: number | null;
  temp_avg_celsius: number | null;
  temp_max_celsius: number | null;
  util_samples: number;
  power_samples: number;
}

export interface GPUCardRow extends GPUFigures {
  node_uuid: string;
  node_name: string;
  /** The card's node is deleted; node_name is the name it left. */
  node_deleted: boolean;
  gpu_index: number;
  gpu_uuid: string;
  gpu_model: string;
  /** Deployments holding the card now. */
  deployments: string[];
  /** The card is in the inventory now, whether its node is online or not. */
  present: boolean;
}

export interface GPUHeatRow {
  key: string;
  label: string;
  /** The row's node is deleted; label is the name it left. */
  node_deleted: boolean;
  /** Aligned with the range's buckets; null where there were no samples. */
  values: (number | null)[];
}

/** A model's or a node's cards over the range, from their own statistics. */
export interface GPUGroupRow extends GPUFigures {
  /** The model, or the node's UUID. */
  key: string;
  label: string;
  /** A deleted node's row: label is the name it left, and it counts no cards now. */
  node_deleted: boolean;
  /** The group's cards and their nodes in the inventory now; a node's card models. */
  cards: number;
  nodes: number;
  models: string[];
  /** Deployments holding the group's cards now, with how many each. */
  deployments: { name: string; cards: number }[];
  /** Cards no deployment holds now. */
  unassigned: number;
}

export interface GPUAnalytics {
  range: AnalyticsRange;
  metric: GpuMetric;
  group_by: GpuGroupBy;
  totals: GPUFigures;
  /** The cards the filter covers in the inventory now, and of each model. */
  card_count: number;
  by_model: { model: string; cards: number }[];
  /** cards lists each card only when the filter covers a few hundred at most. */
  detail: boolean;
  cards: GPUCardRow[];
  /** The models or nodes (empty by card). */
  groups: GPUGroupRow[];
  heat: GPUHeatRow[];
  /** The heat map's rows in all; heat holds the highest and lowest when fewer. */
  heat_total: number;
}

export interface GPUTimeRow {
  deployment_id: number;
  deployment_name: string;
  served_name: string;
  deployment_deleted: boolean;
  gpu_model: string;
  gpu_hours: number;
  whole_gpu_hours: number;
  avg_instances: number;
  max_instances: number;
  /** The deployment's tokens over the range (all its models). */
  prompt_tokens: number;
  completion_tokens: number;
  gpu_hours_per_million_output: number | null;
  gpu_hours_per_million_tokens: number | null;
}

export interface GPUTimeSeries {
  deployment_id: number;
  deployment_name: string;
  gpu_hours: number[];
}

export interface GPUCapacity {
  gpu_model: string;
  total_gpu_hours: number;
  schedulable_gpu_hours: number;
  allocated_gpu_hours: number;
}

export interface GPUTimeAnalytics {
  range: AnalyticsRange;
  rows: GPUTimeRow[];
  series: GPUTimeSeries[];
  capacity: GPUCapacity[];
}

export interface KeyUsage {
  prefix: string;
  today: UsageCounts;
  last_7_days: UsageCounts;
}

export interface KeyUsageList {
  items: KeyUsage[];
  time_zone: string;
}

/** The parameters of a statistics query; absent ones are left out of the URL. */
export interface AnalyticsParams {
  from: number;
  to: number;
  granularity?: Granularity;
  deployment?: number;
  /** An API token id, or "none" for requests without one. */
  key?: string;
  source?: UsageSource;
  groupBy?: GpuGroupBy;
  metric?: GpuMetric;
  /** Node label filter, "key=value" each: values of one key are alternatives, keys all hold. */
  labels?: string[];
  model?: string;
}

const KEYS: [keyof AnalyticsParams, string][] = [
  ["from", "from"], ["to", "to"], ["granularity", "granularity"], ["deployment", "deployment"], ["key", "key"],
  ["source", "source"], ["groupBy", "group_by"], ["metric", "metric"], ["model", "model"],
];

/** analyticsQueryString is the query string of p: only the parameters that are set. */
export function analyticsQueryString(p: AnalyticsParams): string {
  const parts: string[] = [];
  for (const [field, name] of KEYS) {
    const value = p[field];
    if (value === undefined || value === "" || (value === 0 && field !== "from")) continue;
    parts.push(`${name}=${encodeURIComponent(String(value))}`);
  }
  for (const label of p.labels ?? []) parts.push(`label=${encodeURIComponent(label)}`);
  return parts.join("&");
}

export const analyticsApi = {
  usage: (p: AnalyticsParams, signal?: AbortSignal) => getJSON<UsageAnalytics>(`/api/v1/analytics/usage?${analyticsQueryString(p)}`, signal),
  quality: (p: AnalyticsParams, signal?: AbortSignal) => getJSON<QualityAnalytics>(`/api/v1/analytics/quality?${analyticsQueryString(p)}`, signal),
  gpus: (p: AnalyticsParams, signal?: AbortSignal) => getJSON<GPUAnalytics>(`/api/v1/analytics/gpus?${analyticsQueryString(p)}`, signal),
  gpuTime: (p: AnalyticsParams, signal?: AbortSignal) => getJSON<GPUTimeAnalytics>(`/api/v1/analytics/gpu-time?${analyticsQueryString(p)}`, signal),
  keyUsage: (signal?: AbortSignal) => getJSON<KeyUsageList>("/api/v1/analytics/key-usage", signal),
};
