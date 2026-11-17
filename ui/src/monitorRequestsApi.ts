import { rangeParams, type MonitorRange } from "./monitorRange";

// Wire types of the per-request monitor views (dto/monitor_requests.go):
// GET /api/v1/monitor/deployments/{id}/requests and
// GET /api/v1/monitor/distributions.

export type MonitorRequestPhase = "waiting" | "thinking" | "answering" | "done";
export type MonitorRequestOutcome = "" | "ok" | "client_closed" | "upstream_error" | "truncated";
// How a finished request turned out, as history filters by it.
export type MonitorRequestResult = "completed" | "length" | "client_closed" | "interrupted" | "error";
export const MONITOR_REQUEST_RESULTS: MonitorRequestResult[] = ["completed", "length", "client_closed", "interrupted", "error"];

// One request as the gateway saw it. Times are milliseconds since the gateway
// received it; null is a moment that did not happen (yet) or was not
// observed, a null count one the engine did not report.
export interface MonitorRequest {
  id: string;
  // W3C trace id (32 hex digits): the caller's trace when it sent a
  // traceparent, else one the gateway started; "" for a request recorded
  // before trace ids were.
  trace_id: string;
  deployment_id: number;
  received_ms: number;
  phase: MonitorRequestPhase;
  // How long ago the request was received (live only).
  age_ms: number | null;
  instance_uuid: string;
  api_key_id: number;
  // Empty for no key (playground) or a deleted key.
  api_key_name: string;
  /** The priority class the gateway ran the request at; "" before classes or while in flight. */
  priority_class: string;
  user_id: number;
  source: "api" | "playground" | "batch" | "evaluation";
  stream: boolean;
  body_bytes: number;
  images: number;
  dispatch_ms: number | null;
  first_byte_ms: number | null;
  first_think_ms: number | null;
  first_answer_ms: number | null;
  last_chunk_ms: number | null;
  end_ms: number | null;
  events: number;
  think_events: number;
  max_gap_ms: number | null;
  // How a finished request ended; null/empty while in flight.
  status: number | null;
  outcome: MonitorRequestOutcome;
  finish_reason: string;
  // Empty while in flight.
  result: MonitorRequestResult | "";
  prompt_tokens: number | null;
  cached_tokens: number | null;
  completion_tokens: number | null;
  reasoning_tokens: number | null;
  // The engine's own account of the request's phases.
  engine_queue_ms: number | null;
  engine_prefill_ms: number | null;
  engine_decode_ms: number | null;
  // The time from receipt to the first generated output, reasoning
  // included; null for a reply that does not stream, or one still waiting.
  ttft_ms: number | null;
  // The time per output token after the first, and the prefill and decode
  // speeds, as the server works them out; null when unknown or in flight.
  tpot_ms: number | null;
  prefill_tok_s: number | null;
  decode_tok_s: number | null;
  // How many requests this one stands for in a sampled history.
  weight: number;
  // The request's time split into the phases it went through, in order:
  // the server's split, the row's bar. Adds up to the end-to-end time (or
  // the age, in flight).
  segments: MonitorRequestSegment[];
  // A PD disaggregated request's prefill stage; null for any other request.
  // In PD the engine figures above are the decode instance's: its queue
  // includes waiting for the KV, its prefill recomputes the last token.
  pd: MonitorRequestPD | null;
}

export interface MonitorRequestPD {
  // The prefill instance that took the prefill copy (in flight: is
  // computing it); "" when none did.
  prefill_instance_uuid: string;
  // How long the gateway waited for the stage; null when it did not wait
  // (concurrent handoff) or the stage is still running.
  prefill_ms: number | null;
  // The prefill instance's own account of its queue and prefill.
  prefill_queue_ms: number | null;
  prefill_compute_ms: number | null;
  // Why the request was decoded alone; "" when it was not.
  fallback: string;
}

export type SegmentKind = "gateway" | "pd_prefill" | "preprocess" | "queue" | "prefill" | "waiting" | "thinking" | "answering" | "tail";

// The part of a request's time a segment belongs to: the wait for the first
// token (adding up to ttft_ms), the decode after it, or the stream's end; ""
// for a reply that does not stream.
export type SegmentStage = "first_token" | "decode" | "tail" | "";

export interface MonitorRequestSegment {
  kind: SegmentKind;
  ms: number;
  stage: SegmentStage;
}

// GET /api/v1/monitor/deployments/{id}/latency: where the time of the
// deployment's whole replies went. The segments' means add up to the
// end-to-end mean.
export interface MonitorLatencyStat {
  mean_ms: number;
  p50_ms: number;
  p95_ms: number;
}

export interface MonitorLatencySegment {
  kind: SegmentKind;
  // Over every whole reply (0 for one without the phase).
  mean_ms: number;
  // The replies that had the phase; the percentiles are over those.
  requests: number;
  p50_ms: number;
  p95_ms: number;
}

export interface MonitorLatency {
  from_ms: number;
  to_ms: number;
  requests: number;
  end_to_end: MonitorLatencyStat | null;
  segments: MonitorLatencySegment[];
  speeds: {
    prefill_tok_s: number | null;
    thinking_tok_s: number | null;
    answering_tok_s: number | null;
    decode_tok_s: number | null;
  };
}

export function monitorLatencyURL(id: number, range: MonitorRange): string {
  return `/api/v1/monitor/deployments/${id}/latency?${new URLSearchParams(rangeParams(range))}`;
}

export interface MonitorRequestCounts {
  waiting: number;
  thinking: number;
  answering: number;
}

export interface MonitorRequests {
  view: "live" | "history";
  as_of: number;
  // Phase counts over every in-flight request (live only).
  counts: MonitorRequestCounts | null;
  requests: MonitorRequest[];
  // Pages history further back; "" when there is no more.
  next_cursor: string;
  // History only: the time range as applied (Unix ms), and every request
  // the gateway finished in it, before filters and the log's sampling.
  from_ms?: number;
  to_ms?: number;
  range_total?: number;
}

export interface MonitorGridCell {
  x: number;
  y: number;
  n: number;
}

export interface MonitorDistributions {
  from_ms: number;
  to_ms: number;
  // Token bucket i holds [token_buckets[i], token_buckets[i+1]), the last
  // one everything above.
  token_buckets: number[];
  // First-token bucket i holds below ttft_bounds_ms[i] (and from the
  // previous bound); the bucket after the last bound holds everything slower.
  ttft_bounds_ms: number[];
  // Requests by prompt bucket (x) and completion bucket (y).
  input_output: MonitorGridCell[];
  // Requests by uncached prompt bucket (x) and first-token bucket (y).
  uncached_ttft: MonitorGridCell[];
  coverage: { total: number; with_usage: number; with_cache: number };
}

export interface MonitorRequestsQuery {
  view: "live" | "history";
  range?: string;
  // History: an explicit range (Unix ms), given together; replaces range.
  from?: number;
  to?: number;
  limit?: number;
  key?: number;
  // A replica's instance uuid.
  instance?: string;
  // History filters: results, and end-to-end time bounds in milliseconds.
  results?: MonitorRequestResult[];
  minMs?: number;
  maxMs?: number;
  // Time-to-first-token bounds in milliseconds; either keeps only streamed
  // replies that had a first token.
  minTtftMs?: number;
  maxTtftMs?: number;
  cursor?: string;
}

export function monitorRequestsURL(id: number, q: MonitorRequestsQuery): string {
  const params = new URLSearchParams({ view: q.view });
  if (q.from && q.to) {
    params.set("from", String(q.from));
    params.set("to", String(q.to));
  } else if (q.range) {
    // A picked page range is the history's window too.
    for (const [k, v] of Object.entries(rangeParams(q.range as MonitorRange))) params.set(k, v);
  }
  if (q.limit) params.set("limit", String(q.limit));
  if (q.key) params.set("key", String(q.key));
  if (q.instance) params.set("instance", q.instance);
  if (q.results?.length) params.set("results", q.results.join(","));
  if (q.minMs) params.set("min_ms", String(Math.round(q.minMs)));
  if (q.maxMs) params.set("max_ms", String(Math.round(q.maxMs)));
  if (q.minTtftMs) params.set("min_ttft_ms", String(Math.round(q.minTtftMs)));
  if (q.maxTtftMs) params.set("max_ttft_ms", String(Math.round(q.maxTtftMs)));
  if (q.cursor) params.set("cursor", q.cursor);
  return `/api/v1/monitor/deployments/${id}/requests?${params}`;
}

export function monitorDistributionsURL(range: MonitorRange, deploymentId?: number): string {
  const params = new URLSearchParams(rangeParams(range));
  if (deploymentId) params.set("deployment", String(deploymentId));
  return `/api/v1/monitor/distributions?${params}`;
}

// GET /api/v1/monitor/requests/lookup: the finished requests of a trace id or
// a request id, across deployments, newest first.
export interface MonitorLookupDeployment {
  id: number;
  name: string;
  deleted: boolean;
}

export interface MonitorRequestLookup {
  as_of: number;
  // The window searched (Unix ms) and where it came from: the query's
  // from/to, the time a gateway-started trace id carries, or the last hour.
  from_ms: number;
  to_ms: number;
  window: "requested" | "trace_time" | "default";
  requests: MonitorRequest[];
  // The deployments of the requests by served name; a deleted one by the
  // name it left (deleted set), or an empty name when none was kept.
  deployments: MonitorLookupDeployment[];
}

export interface MonitorRequestLookupQuery {
  traceId?: string;
  requestId?: string;
  from?: number;
  to?: number;
}

// A trace id is 32 hex digits; anything else is looked up as a request id.
export const TRACE_ID_PATTERN = /^[0-9a-fA-F]{32}$/;

export function monitorRequestLookupURL(q: MonitorRequestLookupQuery): string {
  const params = new URLSearchParams();
  if (q.traceId) params.set("trace_id", q.traceId);
  if (q.requestId) params.set("request_id", q.requestId);
  if (q.from && q.to) {
    params.set("from", String(q.from));
    params.set("to", String(q.to));
  }
  return `/api/v1/monitor/requests/lookup?${params}`;
}

// traceLinkFor fills a trace link template's {trace_id}; null without a
// template or a trace id.
export function traceLinkFor(template: string, traceId: string): string | null {
  if (!template || !traceId) return null;
  return template.split("{trace_id}").join(encodeURIComponent(traceId));
}
