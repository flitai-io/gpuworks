// The token usage table's rows: the API answers one row per key x user x
// deployment x source; the page groups them by key, deployment or model.
import type { UsageCounts, UsageRow } from "../../analyticsApi";
import { ratio } from "./format";

export type UsageGroupBy = "key" | "deployment" | "model";

export const COUNT_FIELDS: (keyof UsageCounts)[] = [
  "requests", "no_usage", "prompt_tokens", "completion_tokens", "reasoning_tokens", "reasoning_reported", "cached_tokens", "cache_reported", "cache_reported_prompt",
];

export function zeroCounts(): UsageCounts {
  return {
    requests: 0, no_usage: 0, prompt_tokens: 0, completion_tokens: 0, reasoning_tokens: 0, reasoning_reported: 0, cached_tokens: 0, cache_reported: 0, cache_reported_prompt: 0,
  };
}

export function addCounts(into: UsageCounts, add: UsageCounts): UsageCounts {
  for (const f of COUNT_FIELDS) into[f] += add[f];
  return into;
}

/** Cached tokens over the prompt tokens of the requests that reported a cache figure; null when none did. */
export function hitRate(c: UsageCounts): number | null {
  return ratio(c.cached_tokens, c.cache_reported_prompt);
}

/** Input that did not hit the cache. Requests that reported no cache figure count here too: see uncachedUnreported. */
export function uncachedTokens(c: UsageCounts): number {
  return Math.max(0, c.prompt_tokens - c.cached_tokens);
}

/** Input tokens of requests that reported no cache figure: neither a hit nor a known miss. */
export function uncachedUnreported(c: UsageCounts): number {
  return Math.max(0, c.prompt_tokens - c.cache_reported_prompt);
}

export interface KeyInfo {
  id: number;
  name: string;
  prefix: string;
  revoked: boolean;
  deleted: boolean;
  /** A Playground request: no key, told apart by user. */
  playground: boolean;
  /** A batch created in the console: no key, told apart by user. */
  batch?: boolean;
  /** A server-origin evaluation's requests: no key, told apart by user. */
  evaluation?: boolean;
  userName: string;
}

export interface DeploymentInfo {
  id: number;
  name: string;
  served: string;
  deleted: boolean;
}

export interface UsageNode {
  id: string;
  kind: UsageGroupBy;
  /** What the node is when it is the second level: a key under a deployment or model, a deployment under a key. */
  level: "top" | "child";
  counts: UsageCounts;
  firstMs: number;
  lastMs: number;
  key?: KeyInfo;
  deployment?: DeploymentInfo;
  model?: string;
  /** The deployments a key row used / the number of keys a deployment or model row has. */
  deployments?: DeploymentInfo[];
  keyCount?: number;
  children?: UsageNode[];
}

/** keyIdentity says which requests are "one key": the key, or for keyless Playground or console batch requests the user. */
export function keyIdentity(r: UsageRow): string {
  if (r.api_key_id) return `k${r.api_key_id}`;
  if (r.source === "playground") return `pg${r.user_id}`;
  if (r.source === "batch") return `b${r.user_id}`;
  if (r.source === "evaluation") return `ev${r.user_id}`;
  return "nokey";
}

export function keyInfoOf(r: UsageRow): KeyInfo {
  return {
    id: r.api_key_id,
    name: r.api_key_name,
    prefix: r.api_key_prefix,
    revoked: r.api_key_revoked,
    deleted: r.api_key_deleted,
    playground: !r.api_key_id && r.source === "playground",
    batch: !r.api_key_id && r.source === "batch",
    evaluation: !r.api_key_id && r.source === "evaluation",
    userName: r.user_name,
  };
}

export function deploymentInfoOf(r: UsageRow): DeploymentInfo {
  return { id: r.deployment_id, name: r.deployment_name, served: r.served_name, deleted: r.deployment_deleted };
}

interface Bucket {
  counts: UsageCounts;
  firstMs: number;
  lastMs: number;
  rows: UsageRow[];
}

function groupRows(rows: UsageRow[], by: (r: UsageRow) => string): Map<string, Bucket> {
  const out = new Map<string, Bucket>();
  for (const r of rows) {
    const id = by(r);
    let b = out.get(id);
    if (!b) {
      b = { counts: zeroCounts(), firstMs: Infinity, lastMs: 0, rows: [] };
      out.set(id, b);
    }
    addCounts(b.counts, r);
    if (r.first_ms) b.firstMs = Math.min(b.firstMs, r.first_ms);
    b.lastMs = Math.max(b.lastMs, r.last_ms);
    b.rows.push(r);
  }
  return out;
}

const finite = (ms: number) => (Number.isFinite(ms) ? ms : 0);

function keyNodes(rows: UsageRow[], idPrefix: string, level: UsageNode["level"]): UsageNode[] {
  return [...groupRows(rows, keyIdentity)].map(([id, b]) => ({
    id: `${idPrefix}${id}`,
    kind: "key" as const,
    level,
    counts: b.counts,
    firstMs: finite(b.firstMs),
    lastMs: b.lastMs,
    key: keyInfoOf(b.rows[0]),
  }));
}

function deploymentNodes(rows: UsageRow[], idPrefix: string, level: UsageNode["level"]): UsageNode[] {
  return [...groupRows(rows, (r) => String(r.deployment_id))].map(([id, b]) => ({
    id: `${idPrefix}d${id}`,
    kind: "deployment" as const,
    level,
    counts: b.counts,
    firstMs: finite(b.firstMs),
    lastMs: b.lastMs,
    deployment: deploymentInfoOf(b.rows[0]),
  }));
}

/** buildUsageTree groups the API's rows: by key (children: the deployments it used), or by deployment or model (children: the keys that used it). */
export function buildUsageTree(rows: UsageRow[], by: UsageGroupBy): UsageNode[] {
  if (by === "key") {
    return [...groupRows(rows, keyIdentity)].map(([id, b]) => {
      const deployments = deploymentNodes(b.rows, `k:${id}:`, "child");
      return {
        id: `k:${id}`,
        kind: "key" as const,
        level: "top" as const,
        counts: b.counts,
        firstMs: finite(b.firstMs),
        lastMs: b.lastMs,
        key: keyInfoOf(b.rows[0]),
        deployments: deployments.map((d) => d.deployment!),
        children: deployments,
      };
    });
  }
  if (by === "deployment") {
    return [...groupRows(rows, (r) => String(r.deployment_id))].map(([id, b]) => {
      const keys = keyNodes(b.rows, `d:${id}:`, "child");
      return {
        id: `d:${id}`,
        kind: "deployment" as const,
        level: "top" as const,
        counts: b.counts,
        firstMs: finite(b.firstMs),
        lastMs: b.lastMs,
        deployment: deploymentInfoOf(b.rows[0]),
        keyCount: keys.length,
        children: keys,
      };
    });
  }
  return [...groupRows(rows, (r) => r.served_name || `#${r.deployment_id}`)].map(([name, b]) => {
    const keys = keyNodes(b.rows, `m:${name}:`, "child");
    return {
      id: `m:${name}`,
      kind: "model" as const,
      level: "top" as const,
      counts: b.counts,
      firstMs: finite(b.firstMs),
      lastMs: b.lastMs,
      model: name,
      deployments: [...groupRows(b.rows, (r) => String(r.deployment_id))].map(([, d]) => deploymentInfoOf(d.rows[0])),
      keyCount: keys.length,
      children: keys,
    };
  });
}

export interface KeyOption {
  /** The filter value: the key's id. */
  id: number;
  info: KeyInfo;
}

/** keyOptions are the keys in rows, once each, for the key filter. */
export function keyOptions(rows: UsageRow[]): KeyOption[] {
  const seen = new Map<number, KeyOption>();
  for (const r of rows) {
    if (r.api_key_id && !seen.has(r.api_key_id)) seen.set(r.api_key_id, { id: r.api_key_id, info: keyInfoOf(r) });
  }
  return [...seen.values()].sort((a, b) => a.info.name.localeCompare(b.info.name));
}

/** hasKeylessRows tells whether any row is a request without a key (to offer "no key" in the filter). */
export function hasKeylessRows(rows: UsageRow[]): boolean {
  return rows.some((r) => !r.api_key_id);
}
