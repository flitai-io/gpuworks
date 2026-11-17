// Client for the Batch API management endpoints (GET/POST /api/v1/batches,
// docs/reference/api.md "Batches"). The wire types mirror the server's batch
// DTO; times are RFC3339 strings and the ones a batch has not reached are
// absent.
import { getJSON, listQueryString, localizedResponseError, type ListPage, type ListQuery } from "./api";

/** The endpoints the sample file knows how to fill; the ones offered are the dictionary's. */
export type BatchEndpoint = "/v1/chat/completions" | "/v1/completions" | "/v1/embeddings";

export type BatchStatus =
  | "validating"
  | "in_progress"
  | "finalizing"
  | "completed"
  | "failed"
  | "expired"
  | "cancelling"
  | "cancelled";

export type BatchFileKind = "input" | "output" | "error";

/** Requests that reached a final result; requests still being retried are in neither count. */
export interface BatchRequestCounts {
  total: number;
  completed: number;
  failed: number;
}

export interface BatchFailure {
  code: string;
  message: string;
}

/**
 * A finding that does not refuse the batch: BATCH_MODEL_SLEEP_HELD {model} says the model the batch was given sleeps by an
 * operator's order, which requests do not wake; BATCH_FILE_MODELS_SLEEP_HELD {models, more} says so of models its file names.
 * BATCH_MODEL_WAITS {model, state} and BATCH_FILE_MODELS_WAIT {models, more, state} say a model is offline, failed or stopped
 * (state a BatchModelState): its requests wait until it serves.
 */
export interface BatchWarning {
  code: string;
  params?: Record<string, unknown>;
}

export interface Batch {
  id: string;
  endpoint: BatchEndpoint | string;
  status: BatchStatus;
  /** Whether a cancel would stop the batch now; the server decides (a finalizing batch, for one, ends as it is). */
  cancellable: boolean;
  input_file_id: string;
  /** Only on a single batch (GET, create, cancel), not on list rows. */
  input_filename?: string;
  output_file_id?: string;
  error_file_id?: string;
  completion_window: string;
  /** The served name every request is sent to, replacing its line's model; absent when each line names its own. */
  model?: string;
  /** The request priority class the batch's requests run at: the one it was created with, else the batch class. */
  priority_class: string;
  request_counts: BatchRequestCounts;
  metadata: Record<string, string>;
  failure?: BatchFailure;
  /** When the output and error files are deleted, the server's retention after the batch ended; absent while it runs or without files. */
  output_expires_at?: string;
  /** Non-blocking findings of creating the batch; only on the create response. */
  warnings?: BatchWarning[];
  /** Why requests of a running batch are being retried, per model; only on a single batch, absent when none waits. */
  waiting?: BatchModelWait[];
  user_id: number;
  api_key_id: number;
  created_at: string;
  expires_at: string;
  in_progress_at?: string;
  finalizing_at?: string;
  completed_at?: string;
  failed_at?: string;
  expired_at?: string;
  cancelling_at?: string;
  cancelled_at?: string;
}

/** What the create call takes besides the file. */
export interface BatchCreateOptions {
  /** The url the file's requests ask for; the server reads it from the file when absent. */
  endpoint?: BatchEndpoint;
  model?: string;
  completionWindowHours?: number;
  /** The request priority class the batch's requests run at; the dictionary's priority_class when absent. */
  priorityClass?: string;
  signal?: AbortSignal;
}

/**
 * What a served name is to a batch, as the server judges it (the same rule its
 * create check uses), in the order the dictionary lists them: running, asleep
 * with a sleep a request wakes, starting, asleep by an operator's or a
 * schedule's order (which requests do not wake), on a disconnected node,
 * failed, stopped.
 */
export type BatchModelState = "running" | "asleep" | "starting" | "asleep_held" | "offline" | "failed" | "stopped";

export interface BatchModel {
  name: string;
  state: BatchModelState | string;
  /** Server-derived default-choice eligibility, including automatic wake. */
  ready: boolean;
  /** The batch endpoints the model serves; a batch of another endpoint naming it is refused. */
  endpoints: string[];
}

/** Why a running batch's requests for one model are being retried, as the server last saw it. */
export interface BatchModelWait {
  model: string;
  /** The model's state now; absent when no deployment serves it any more. */
  state?: BatchModelState | string;
  /** The HTTP status of the last answer, 0 when none came. */
  status: number;
  /** The gateway's or the engine's words, English. */
  detail?: string;
  since: string;
  /** How many requests wait for a retry now. */
  requests: number;
}

/**
 * What a batch is offered with (GET /api/v1/batches/dictionary): the server
 * enforces every one of these, the page only shows them.
 */
export interface BatchDictionary {
  endpoints: string[];
  completion_window_hours: { min: number; max: number; default: number };
  max_file_bytes: number;
  max_file_lines: number;
  /** Every status, in the order a filter lists them. */
  statuses: BatchStatus[];
  output_retention_days: number;
  /** The served names of the deployments, in the order of their states and then by name. */
  models: BatchModel[];
  /** The request priority class a batch created without one runs at; a create may name any class. */
  priority_class: string;
}

/** Seconds-level cadence at which the server checkpoints a running batch's progress. */
export const BATCH_POLL_MS = 5000;

const TERMINAL: ReadonlySet<string> = new Set(["completed", "failed", "expired", "cancelled"]);

/** Whether a batch is still moving (validating, running, finalizing or being cancelled), so the page keeps polling it. */
export function isBatchActive(status: string): boolean {
  return !TERMINAL.has(status);
}

/** The authenticated link of one of a batch's JSON Lines files (the session cookie carries the login). */
export function batchFileURL(id: string, kind: BatchFileKind): string {
  return `/api/v1/batches/${encodeURIComponent(id)}/files/${kind}`;
}

export const batchApi = {
  dictionary: () => getJSON<BatchDictionary>("/api/v1/batches/dictionary"),
  list: (query: ListQuery) => getJSON<ListPage<Batch>>(`/api/v1/batches?${listQueryString(query)}`),
  get: (id: string) => getJSON<Batch>(`/api/v1/batches/${encodeURIComponent(id)}`),
  /**
   * Uploads the JSONL input and starts the batch (admin only). The batch's
   * endpoint is the url of the file's requests unless endpoint is given; model,
   * when given, replaces the model of every line; completionWindowHours is the
   * time the batch has (the dictionary's completion_window_hours bound it).
   */
  create: async (file: File, options: BatchCreateOptions = {}): Promise<Batch> => {
    const { endpoint, model, completionWindowHours, priorityClass, signal } = options;
    const body = new FormData();
    body.append("file", file, file.name);
    if (endpoint) body.append("endpoint", endpoint);
    if (model) body.append("model", model);
    if (priorityClass) body.append("priority_class", priorityClass);
    if (completionWindowHours !== undefined) body.append("completion_window", `${completionWindowHours}h`);
    const res = await fetch("/api/v1/batches", { method: "POST", credentials: "include", body, signal });
    if (!res.ok) throw await localizedResponseError(res, `/api/v1/batches: ${res.status}`);
    return res.json();
  },
  /** Asks the executor to stop the batch; it shows as cancelling until in-flight requests end (admin only). */
  cancel: async (id: string): Promise<Batch> => {
    const path = `/api/v1/batches/${encodeURIComponent(id)}/cancel`;
    const res = await fetch(path, { method: "POST", credentials: "include" });
    if (!res.ok) throw await localizedResponseError(res, `${path}: ${res.status}`);
    return res.json();
  },
};
