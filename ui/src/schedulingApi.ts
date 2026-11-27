// Client for scheduling analysis (GET /api/v1/scheduling/board, overview,
// timeline and history): who holds each GPU, what waits, the residency
// timeline and the cluster-wide scheduling history.
import { type CursorPage, type ListPage, type ScheduleMessage, getJSON, listQueryString } from "./api";
import type { ScheduleAction } from "./deploymentScheduleApi";
import type { YieldPolicyName } from "./yieldPolicyApi";
import type { QuickRangeKey, TimeRangeSelection } from "./timeRange";

// The scheduling center's board (GET /scheduling/board): who holds each GPU,
// what waits to be placed or woken, what the schedules do next and what the
// automation did last.
export interface BoardOccupant {
  deployment_id: number;
  deployment: string;
  yield_policy: YieldPolicyName;
  instance: string;
  state: "awake" | "asleep";
  sleep_level?: number;
  claim_bytes: number;
  residue_bytes?: number;
}

// The residency timeline (GET /scheduling/timeline): for the nodes of a board
// page, which deployment's instance held each GPU over a window or a custom
// range and in what state, and the stats of the deployments that share GPUs.
export type TimelineState = "awake" | "asleep" | "switching" | "starting";

export interface TimelineSegment {
  deployment_id: number;
  deployment: string;
  deployment_deleted?: boolean;
  instance: string;
  state: TimelineState;
  level?: number;
  from: string;
  to: string;
}

export interface TimelineGPU {
  index: number;
  segments: TimelineSegment[];
  /** Some of the card's history is not listed raw (set whenever summarized_before is). */
  truncated: boolean;
  /** Where the summaries end and the raw segments begin; absent when nothing was summarized. */
  summarized_before?: string;
  /** The time before summarized_before in at most 50 equal slices the server computed. */
  summary?: TimelineSummary[];
}

/** One slice of the time a card's raw segments no longer list. */
export interface TimelineSummary {
  kind: "summary";
  from: string;
  to: string;
  /** Times the card changed hands (the awake deployment became another one) in the slice. */
  switches: number;
  // The answer also carries dominant_deployment_id and dominant_deployment
  // (the deployment awake longest); they are kept for older clients and the
  // timeline does not read them.
  deployments: TimelineSummaryDeployment[];
}

/** What one deployment did with the card during a slice. */
export interface TimelineSummaryDeployment {
  deployment_id: number;
  deployment: string;
  deployment_deleted?: boolean;
  awake_seconds: number;
  asleep_seconds: number;
  switching_seconds: number;
  starting_seconds: number;
  /** The state it spent the most time in during the slice, as the server decided; the slice is drawn in its colour. */
  state: TimelineState;
  /** The sleep level (1 or 2) when state is asleep and the server knows it. */
  level?: number;
}

export interface TimelineNode {
  uuid: string;
  name: string;
  gpus: TimelineGPU[];
}

export interface TimelineDeployment {
  deployment_id: number;
  /** Its name; for a deleted deployment, the one it left behind. */
  deployment: string;
  /** Set when the deployment has been deleted since it shared cards in the window. */
  deployment_deleted?: boolean;
  /** Its yield policy; empty for a deleted deployment, whose policy is not kept. */
  yield_policy: YieldPolicyName | "";
  awake_seconds: number;
  /** awake_seconds over present_seconds, 0 to 1. */
  awake_share: number;
  /** The part of the window the deployment existed in, in seconds. */
  present_seconds: number;
  switches: number;
  yielded: number;
  yielded_to: number;
  wakes: number;
  held_requests: number;
  wait_p50_ms?: number;
  wait_p95_ms?: number;
  /** Where its live instances are now, by node; empty when none is placed. */
  cards: TimelineCards[];
}

/** The GPUs a deployment's instances hold on one node; node is empty for a node no longer known. */
export interface TimelineCards {
  node_id: number;
  /** Absent, as is the name, for a node no longer known. */
  node_uuid?: string;
  node: string;
  gpus: number[];
}

/** A window a timeline may be asked for; the server lists them. */
export interface TimelineWindowOption {
  key: string;
  seconds: number;
}

/** What a timeline's deployment figures count: node, only their replicas on the named nodes; fleet, all of them. The server's to say. */
export type TimelineScope = "node" | "fleet";

export interface SchedulingTimeline {
  scope: TimelineScope;
  /** The range answered: hours is its whole hours (0 under an hour); a custom range has key "custom" and custom true. */
  window: { from: string; to: string; hours: number; key: string; seconds: number; custom: boolean };
  /** The windows the picker offers, in order; the server's list, not the UI's. */
  windows: TimelineWindowOption[];
  nodes: TimelineNode[];
  deployments: TimelineDeployment[];
  /** False when the deployments' yields, yields to them and wakes are not known for the range (the activity keeps 24 hours): those figures are 0 and mean unknown. */
  switches_available: boolean;
}

/** One reason the scheduling activity counts, with the range's count. */
export interface ActivityReason {
  key: string;
  count: number;
}

/** One bucket: a count per reason, in the order of `reasons`. */
export interface ActivityBucket {
  start: string;
  counts: number[];
}

/** A card and how often it changed hands; node is empty for a node no longer known. */
export interface ActivityCard {
  node_uuid: string;
  node: string;
  gpu_index: number;
  switches: number;
}

// The scheduling activity (GET /scheduling/activity): the events per bucket,
// stacked by reason, and the cards that changed hands most. The server picks
// the bucket for the range and names the reasons.
export interface SchedulingActivity {
  from: string;
  to: string;
  bucket_seconds: number;
  reasons: ActivityReason[];
  buckets: ActivityBucket[];
  top_cards: ActivityCard[];
  beyond_retention: boolean;
}

// The activity's window parameter for each of the scheduling page's quick
// ranges (SCHEDULING_QUICK_RANGES); a custom range is asked for by since and
// until instead.
const ACTIVITY_WINDOW: Partial<Record<QuickRangeKey, string>> = { "15m": "15m", "1h": "1h", "6h": "6h", "1d": "24h", "1w": "7d" };

// rfc3339 is an instant as the API's times are written, to the second.
export function rfc3339(ms: number): string {
  return new Date(Math.floor(ms / 1000) * 1000).toISOString().replace(".000Z", "Z");
}

export interface BoardGPU {
  index: number;
  name: string;
  memory_bytes: number;
  awake_bytes: number;
  sleeping_bytes: number;
  free_bytes: number;
  /** The card's state as the overview gives it (one of its `states`). */
  state: string;
  held_outside: boolean;
  switching: boolean;
  occupants: BoardOccupant[];
}

export interface BoardNode {
  uuid: string;
  name: string;
  online: boolean;
  gpus: BoardGPU[];
}

// The scheduling center's overview (GET /scheduling/overview): every node and
// each card of the fleet in one compact response. A card is an array
// [index, model, memory_mib, awake_pct, sleeping_pct, state, switching, occupants?]:
// model is a position in `models` and state in `states` (the server derives
// it); switching is 1 while an instance on the card changes state; occupants,
// when present, are position*2 + 1 when asleep (position in `deployments`)
// for the deployments on the card.
export type OverviewCardWire = [number, number, number, number, number, number, 0 | 1, number[]?];

export interface OverviewCounts {
  cards: number;
  awake: number;
  asleep: number;
  free: number;
  held_outside: number;
  unhealthy: number;
  offline: number;
}

export interface OverviewContention {
  waiting_wakes: number;
  held_requests: number;
  yields_last_hour: number;
}

export interface OverviewNode {
  uuid: string;
  name: string;
  online: boolean;
  labels?: Record<string, string>;
  contention?: number;
  cards: OverviewCardWire[];
}

export interface OverviewDeployment {
  id: number;
  name: string;
  yield_policy: YieldPolicyName;
}

export interface SchedulingOverview {
  summary: OverviewCounts & { nodes: number; online_nodes: number; contention: OverviewContention };
  /** The card states the cards refer to by position. */
  states: string[];
  models: string[];
  deployments: OverviewDeployment[];
  nodes: OverviewNode[];
}

export interface BoardPending {
  deployment_id: number;
  deployment: string;
  reason: ScheduleMessage;
  at: string;
}

export interface BoardWaitingWake {
  deployment_id: number;
  deployment: string;
  instances: string[];
  since: string;
}

export interface BoardUpcoming {
  deployment_id: number;
  deployment: string;
  schedule_id: number;
  action: ScheduleAction;
  sleep_level?: number;
  replicas?: number;
  role?: string;
  at: string;
}

/** Who or what caused a scheduling history entry. */
export type SchedulingCategory = "auto" | "failure" | "manual";

/** One scheduling event of the cluster-wide history (GET /scheduling/history). */
export interface SchedulingHistoryEntry {
  /** Pagination cursor: pass the last entry's id back as `before`. */
  id: number;
  created_at: string;
  deployment_id: number;
  /** Absent when not even the deleted deployment's name is known. */
  deployment?: string;
  deployment_deleted?: boolean;
  instance?: string;
  kind: string;
  code: string;
  category: SchedulingCategory;
  params: Record<string, unknown>;
  /** The user who ordered a manual entry, when known. */
  actor?: string;
  /** The deployment the entry acted for (a yield gave it the GPUs), named. */
  target?: SchedulingHistoryDeployment;
  /** The nodes the entry concerns; absent when it names none the server can tell. */
  nodes?: SchedulingHistoryNode[];
  /**
   * Where the output that explains a failed attempt can be read (the
   * instance and a window in Unix ms, as the logs page takes them); the
   * server sets it on entries about an attempt that failed.
   */
  logs?: { instance: string; from: number; to: number };
}

/** A node a history entry concerns; uuid is absent for a node that is gone. */
export interface SchedulingHistoryNode {
  uuid?: string;
  name: string;
}

/** A deployment a history entry refers to; name is absent when unknown. */
export interface SchedulingHistoryDeployment {
  id: number;
  name?: string;
  deleted?: boolean;
}

export interface SchedulingHistoryQuery {
  limit?: number;
  before?: number;
  category?: SchedulingCategory[];
  deployment_id?: number;
  since?: string;
  until?: string;
}

export interface SchedulingBoard {
  nodes: ListPage<BoardNode>;
  pending: BoardPending[];
  waiting_wakes: BoardWaitingWake[];
  upcoming: BoardUpcoming[];
}

function schedulingHistoryQuery(query: SchedulingHistoryQuery): string {
  const params = new URLSearchParams();
  if (query.limit) params.set("limit", String(query.limit));
  if (query.before) params.set("before", String(query.before));
  if (query.category?.length) params.set("category", query.category.join(","));
  if (query.deployment_id) params.set("deployment_id", String(query.deployment_id));
  if (query.since) params.set("since", query.since);
  if (query.until) params.set("until", query.until);
  const text = params.toString();
  return text ? `?${text}` : "";
}

export const schedulingApi = {
  // The board is a page of nodes with their holders (page, page_size, q); or
  // one node's (node); or the fleet-wide lists alone (queues), which read no node.
  board: (query: { page: number; page_size: number; q?: string } | { node: string } | { queues: true }) => {
    if ("node" in query) return getJSON<SchedulingBoard>(`/api/v1/scheduling/board?node=${encodeURIComponent(query.node)}`);
    if ("queues" in query) return getJSON<SchedulingBoard>("/api/v1/scheduling/board?queues=1");
    return getJSON<SchedulingBoard>(`/api/v1/scheduling/board?${listQueryString(query)}`);
  },
  // Every node and card of the fleet in one compact response.
  overview: () => getJSON<SchedulingOverview>("/api/v1/scheduling/overview"),
  // The residency timeline of some nodes, or (sharing) the stats of the
  // deployments that share cards across the fleet and no lanes.
  // window is one of the answer's windows; without it the server answers its
  // default. A node's timeline may instead be asked for a custom range, since
  // and until as RFC 3339 times.
  timeline: (query: { nodes: string[]; window?: string } | { nodes: string[]; since: string; until: string } | { sharing: true; window?: string }) => {
    const params = new URLSearchParams();
    if ("sharing" in query) params.set("sharing", "1");
    else for (const node of query.nodes) params.append("node", node);
    if ("window" in query && query.window) params.set("window", query.window);
    if ("since" in query) {
      params.set("since", query.since);
      params.set("until", query.until);
    }
    return getJSON<SchedulingTimeline>(`/api/v1/scheduling/timeline?${params.toString()}`);
  },
  // The scheduling activity over a quick range or a custom one.
  activity: (range: TimeRangeSelection) => {
    const params = new URLSearchParams();
    const window = range.kind === "quick" ? ACTIVITY_WINDOW[range.key] : undefined;
    if (window) params.set("window", window);
    else if (range.kind === "custom") {
      params.set("since", rfc3339(range.startMS));
      params.set("until", rfc3339(range.endMS));
    }
    return getJSON<SchedulingActivity>(`/api/v1/scheduling/activity?${params.toString()}`);
  },
  // The history is newest-first and cursor paginated like a deployment's
  // timeline: `before` is the id of the last entry of the page already held.
  history: (query: SchedulingHistoryQuery) =>
    getJSON<CursorPage<SchedulingHistoryEntry>>(`/api/v1/scheduling/history${schedulingHistoryQuery(query)}`),
};
