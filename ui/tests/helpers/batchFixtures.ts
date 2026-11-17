import type { Batch, BatchDictionary, BatchModel } from "../../src/batchApi.ts";

const RETENTION_MS = 30 * 86_400_000;

/**
 * A batch as the management API returns it, overridden per test. The facts the
 * server derives follow the overrides as it derives them: cancellable while
 * validating or in progress, and the output kept 30 days (its default
 * retention) after a batch that wrote files ended.
 */
export function batch(overrides: Partial<Batch> = {}): Batch {
  const status = overrides.status ?? "in_progress";
  const ended = overrides.completed_at ?? overrides.cancelled_at ?? overrides.expired_at ?? overrides.failed_at;
  const hasFiles = !!(overrides.output_file_id || overrides.error_file_id);
  return {
    id: "batch_aaa111",
    endpoint: "/v1/chat/completions",
    status,
    cancellable: status === "validating" || status === "in_progress",
    input_file_id: "file-in1",
    completion_window: "24h",
    priority_class: "batch",
    request_counts: { total: 10, completed: 4, failed: 1 },
    metadata: {},
    user_id: 3,
    api_key_id: 8,
    created_at: "2026-10-28T09:00:00Z",
    expires_at: "2026-10-29T09:00:00Z",
    in_progress_at: "2026-10-28T09:00:05Z",
    ...(hasFiles && ended ? { output_expires_at: new Date(Date.parse(ended) + RETENTION_MS).toISOString() } : {}),
    ...overrides,
  };
}

export function page(items: Batch[], extra: { page?: number; page_size?: number; total?: number } = {}) {
  return { items, page: extra.page ?? 1, page_size: extra.page_size ?? 20, total: extra.total ?? items.length };
}

/** The chat endpoints a chat model serves, the default of a test's model. */
export const CHAT_ENDPOINTS = ["/v1/chat/completions", "/v1/completions"];

/** A dictionary model: a chat model unless the test names its endpoints. */
export function model(name: string, state: string, endpoints: string[] = CHAT_ENDPOINTS): BatchModel {
  return { name, state, ready: state === "running" || state === "asleep", endpoints };
}

/**
 * The batch dictionary as GET /api/v1/batches/dictionary answers it: the
 * server's endpoints, window, limits, statuses and retention, with the models
 * a test names (none by default).
 */
export function dictionary(models: BatchModel[] = [], overrides: Partial<BatchDictionary> = {}): BatchDictionary {
  return {
    endpoints: ["/v1/chat/completions", "/v1/completions", "/v1/embeddings"],
    completion_window_hours: { min: 1, max: 72, default: 24 },
    max_file_bytes: 200 * 1024 * 1024,
    max_file_lines: 50000,
    statuses: ["validating", "in_progress", "finalizing", "cancelling", "completed", "failed", "expired", "cancelled"],
    output_retention_days: 30,
    models,
    priority_class: "batch",
    ...overrides,
  };
}
