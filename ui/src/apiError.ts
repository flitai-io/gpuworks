import { acceleratorMessageParams, hostRequirement } from "./acceleratorMessages";
import { formatBytes } from "./format";
import { notifySessionExpired } from "./session";
import { CLUSTER_UPGRADE_CODE, notifyClusterUpgrade } from "./clusterUpgrade";
import { diagnosticReport, type DiagnosticReport } from "./diagnosticChecks";
import type { TFunction } from "i18next";
import { memoryContractHintNote, placementRefusalText } from "./pages/deploy/deploySchedule";

export interface APIErrorPayload {
  error?: string;
  message?: string;
  code?: string;
  params?: Record<string, unknown>;
  diagnostics?: DiagnosticReport;
}

export type APIErrorTranslator = (key: string, options: Record<string, unknown>) => string;

export class APIError extends Error {
  readonly code: string;
  readonly params?: Record<string, unknown>;
  readonly fallback: string;
  readonly status: number;
  readonly diagnostics?: DiagnosticReport;
  readonly requestId?: string;

  constructor(code: string, params: Record<string, unknown> | undefined, fallback: string, status: number, diagnostics?: DiagnosticReport, requestId?: string) {
    super(fallback || code);
    this.name = "APIError";
    this.code = code;
    this.params = params;
    this.fallback = fallback;
    this.status = status;
    this.diagnostics = diagnostics;
    this.requestId = requestId;
  }
}

/** The codes the server gives an error that has no code of its own: one per
 * HTTP status category (internal/server/messages/http.go, whose test keeps
 * this list equal to the server's). Such a code names only the category; the
 * English error text holds the detail. */
export const GENERIC_API_ERROR_CODES = [
  "BAD_REQUEST", "UNAUTHORIZED", "FORBIDDEN", "NOT_FOUND", "METHOD_NOT_ALLOWED", "REQUEST_TIMEOUT",
  "CONFLICT", "GONE", "REQUEST_TOO_LARGE", "UNSUPPORTED_MEDIA_TYPE", "RANGE_NOT_SATISFIABLE", "UNPROCESSABLE",
  "FAILED_DEPENDENCY", "TOO_MANY_REQUESTS", "INTERNAL_ERROR", "NOT_IMPLEMENTED", "BAD_GATEWAY",
  "SERVICE_UNAVAILABLE", "GATEWAY_TIMEOUT", "INSUFFICIENT_STORAGE",
] as const;

const genericCodes: ReadonlySet<string> = new Set(GENERIC_API_ERROR_CODES);

export function isGenericAPIErrorCode(code: string): boolean {
  return genericCodes.has(code);
}

export function apiErrorText(t: APIErrorTranslator, error: unknown): string {
  if (!(error instanceof APIError)) return error instanceof Error ? error.message : String(error ?? "");
  if (isGenericAPIErrorCode(error.code)) {
    // The category in the reader's language, then the server's own detail:
    // the category alone would hide what actually went wrong.
    const category = t(`apiError.${error.code}`, { defaultValue: error.code });
    const detail = error.fallback.trim();
    if (!detail) return category;
    return t("apiErrorDetail", { category, detail, defaultValue: `${category}: ${detail}` });
  }
  const params = { ...error.params };
  if (error.code === "IMAGE_ARCHIVE_LOAD_FAILED") {
    const reason = String(params.reason ?? "load_failed");
    const fallback = t("apiError.IMAGE_ARCHIVE_LOAD_FAILED", {});
    const fileReason = t(`images:archivePreviewReason.${reason}`, { defaultValue: fallback });
    return t(`images:archiveLoadReason.${reason}`, { defaultValue: fileReason });
  }
  if (error.code === "IMAGE_ARCHIVE_PREVIEW_FAILED") {
    const fallback = t("apiError.IMAGE_ARCHIVE_PREVIEW_FAILED", {});
    return t(`images:archivePreviewReason.${String(params.reason ?? "unreadable")}`, { defaultValue: fallback });
  }
  if (error.code === "SETTING_REMOVED") {
    const target = params.setting === "default_model_source" ? "modelSource"
      : params.route === "/api/v1/pull/source-rules" ? "sourceRules"
      : params.route === "/api/v1/pull/proxies" ? "proxies"
      : params.route === "/api/v1/pull/credentials" ? "credentials" : undefined;
    if (target) return t(`removedSetting.${target}`, { ...params, defaultValue: error.fallback || error.code });
  }
  if (error.code === "PULL_SETTING_INVALID") {
    const field = String(params.field ?? "");
    params.field = t(`pullSettingField.${field}`, { defaultValue: field });
    params.detail = pullSettingErrorDetail(t, error);
  }
  if (error.code === "SLURM_APPLY_FAILED") {
    const step = t(`slurmApplyStep.${String(params.step ?? "")}`, { defaultValue: String(params.step ?? "") });
    params.step = step;
    // A reason is a stable word the console may not know yet: the step says where it failed.
    const reason = String(params.reason ?? "");
    if (reason === "server_unreachable" && typeof params.url === "string" && params.url) {
      // The address the manager node could not reach, and why, when the server says.
      const kind = String(params.kind ?? "") || "other";
      params.kind = t(`slurmApplyKind.${kind}`, { defaultValue: kind });
      params.reason = t("slurmApplyReason.server_unreachable_at", { url: params.url, kind: params.kind, defaultValue: step });
    } else {
      params.reason = reason ? t(`slurmApplyReason.${reason}`, { defaultValue: step }) : t("slurmApplyReason.unknown", {});
    }
  }
  if (error.code === "SLURM_APPLY_NOT_APPLICABLE") {
    const reason = String(params.reason ?? "");
    params.reason = t(`slurmApplyNotApplicable.${reason}`, { defaultValue: reason });
  }
  if (error.code === "ACCELERATOR_FEATURE_UNSUPPORTED") {
    return t("acceleratorFeature.unsupported", { ...acceleratorMessageParams(t, params), defaultValue: error.fallback || error.code });
  }
  if (error.code === "ACCELERATOR_RUNTIME_UNSUPPORTED") params.required = hostRequirement(params);
  if (error.code === "UNIT_PLACEMENT_NONE") params.reasons = placementShortfall(t, params);
  if (error.code === "DISTRIBUTED_PARALLELISM_UNAVAILABLE") params.reason = distributedRefusal(t, params);
  if (error.code === "SERVER_STORAGE_FULL" && typeof params.path === "string" && typeof params.available_bytes === "number" && typeof params.total_bytes === "number") {
    return t("serverStorageFull.at", { path: params.path, available: formatBytes(params.available_bytes), total: formatBytes(params.total_bytes), defaultValue: error.fallback || error.code });
  }
  const text = t(`apiError.${error.code}`, {
    ...params,
    defaultValue: error.fallback || error.code,
  });
  if (error.code === "DEPLOYMENT_NEVER_FITS") {
    // The server decides whether the estimate alone is what refused the node.
    const tDeploy = (key: string, options: Record<string, unknown> = {}) => t(key, { ns: "deploy", ...options });
    return text + memoryContractHintNote(tDeploy as unknown as TFunction<"deploy">, params);
  }
  return text;
}

const placementChecks = ["unmet", "cordoned", "offline", "busy", "no_network", "ucx_netlink_hang"] as const;

// The first few nodes left out for a kernel that hangs UCX, as the server names them.
function ucxNetlinkNames(params: Record<string, unknown>): string {
  const names = params.ucx_netlink_nodes;
  return Array.isArray(names) ? names.filter((name): name is string => typeof name === "string").join(", ") : "";
}

/** The node counts of a placement refusal, joined with " · ": only the checks
 * that left nodes out, then the largest group sharing a network and GPU
 * architecture. busy names the GPUs each node lacks when the server sends
 * per_rank. */
export function placementShortfall(t: APIErrorTranslator, params: Record<string, unknown>): string {
  const parts = placementChecks
    .filter((check) => Number(params[check]) > 0)
    .map((check) => t(`placementShortfall.${check === "busy" && params.per_rank != null ? "busyPerRank" : check}`, { n: params[check], per_rank: params.per_rank, names: ucxNetlinkNames(params) }));
  if (params.largest != null) parts.push(t("placementShortfall.largest", { n: params.largest }));
  return parts.join(" · ");
}

/** Why the rank in params.node refused the recommended layouts: one of the
 * recommendation's own reasons, or a scheduler code worded as the deploy form
 * words a node's refusal. The reason's params travel in params.reason_params. */
function distributedRefusal(t: APIErrorTranslator, params: Record<string, unknown>): string {
  const reason = String(params.reason ?? "");
  const reasonParams = typeof params.reason_params === "object" && params.reason_params !== null
    ? params.reason_params as Record<string, unknown> : {};
  const own = t(`distributedRecommendationReason.${reason}`, { ...reasonParams, ns: "deploy", defaultValue: "" });
  if (own && own !== `distributedRecommendationReason.${reason}`) return own;
  const tDeploy = (key: string, options: Record<string, unknown> = {}) => t(key, { ns: "deploy", ...options });
  return placementRefusalText(tDeploy as unknown as TFunction<"deploy">, { code: reason, params: reasonParams });
}

const pullSettingReasons = new Set(["name_required", "name_duplicate", "max_bytes", "nonnegative", "invalid_text", "requirements_invalid"]);

/** Translate semantic validation reasons; old/new unknown reasons stay useful
 * in the current language while their raw diagnostic remains on the error. */
export function pullSettingErrorDetail(t: APIErrorTranslator, error: APIError): string {
  const fallback = t("pullSettingReason.invalid_value", { defaultValue: "Check this setting and try again" });
  const reason = String(error.params?.reason ?? "");
  if (!pullSettingReasons.has(reason)) return fallback;
  return t(`pullSettingReason.${reason}`, { ...error.params, defaultValue: fallback });
}

/** A product message an answer carries (dto.ProductMessage), in the reader's language. */
export interface ProductMessage {
  code: string;
  params?: Record<string, unknown>;
  message: string;
}

export function productMessageText(t: APIErrorTranslator, message: ProductMessage): string {
  return apiErrorText(t, new APIError(message.code, message.params, message.message, 200));
}

// ProxyErrorPage is an HTML page in place of a gpuworks JSON error: a reverse
// proxy, CDN or tunnel in front of the server answered, or replaced the
// server's own answer (Cloudflare does that to a 502 or 504), so the reason
// the server gave is gone.
export class ProxyErrorPage extends Error {
  readonly status: number;

  constructor(status: number, statusText: string) {
    super(`HTTP ${status}${statusText ? ` ${statusText}` : ""}: a reverse proxy or tunnel in front of gpuworks answered with its own error page; the server's reason is not available`);
    this.name = "ProxyErrorPage";
    this.status = status;
  }
}

function looksLikeHTML(response: Response, raw: string): boolean {
  return (response.headers.get("Content-Type") ?? "").toLowerCase().includes("text/html") || /^\s*<(!doctype|html)/i.test(raw);
}

export async function apiResponseError(response: Response, fallback: string): Promise<Error> {
  let raw = "";
  try {
    raw = await response.text();
  } catch {
    // Keep the request/status fallback when a response body cannot be read.
  }
  let payload: APIErrorPayload | undefined;
  if (raw && looksLikeHTML(response, raw)) return new ProxyErrorPage(response.status, response.statusText);
  if (raw) {
    try {
      payload = JSON.parse(raw) as APIErrorPayload;
    } catch {
      // Non-JSON responses remain raw lower-level errors.
    }
  }
  const detail = payload?.error || payload?.message || raw || fallback;
  if (response.status === 401 && payload?.code === "AUTHENTICATION_REQUIRED") notifySessionExpired();
  if (response.status === 503 && payload?.code === CLUSTER_UPGRADE_CODE) notifyClusterUpgrade();
  if (payload?.code) return new APIError(payload.code, payload.params, detail, response.status, diagnosticReport(payload.diagnostics), response.headers.get("X-Request-ID") || undefined);
  return new Error(detail);
}
