import { acceleratorMessageParams } from "./acceleratorMessages";
import { formatBytes } from "./format";
import { notifySessionExpired } from "./session";
import { diagnosticReport, type DiagnosticReport } from "./diagnosticChecks";

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

export function apiErrorText(t: APIErrorTranslator, error: unknown): string {
  if (!(error instanceof APIError)) return error instanceof Error ? error.message : String(error ?? "");
  const params = { ...error.params };
  if (error.code === "PULL_SETTING_INVALID") {
    const field = String(params.field ?? "");
    params.field = t(`pullSettingField.${field}`, { defaultValue: field });
    params.detail = pullSettingErrorDetail(t, error);
  }
  if (error.code === "ACCELERATOR_FEATURE_UNSUPPORTED") {
    return t("acceleratorFeature.unsupported", { ...acceleratorMessageParams(t, params), defaultValue: error.fallback || error.code });
  }
  if (error.code === "SERVER_STORAGE_FULL" && typeof params.path === "string" && typeof params.available_bytes === "number" && typeof params.total_bytes === "number") {
    return t("serverStorageFull.at", { path: params.path, available: formatBytes(params.available_bytes), total: formatBytes(params.total_bytes), defaultValue: error.fallback || error.code });
  }
  return t(`apiError.${error.code}`, {
    ...params,
    defaultValue: error.fallback || error.code,
  });
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
  if (payload?.code) return new APIError(payload.code, payload.params, detail, response.status, diagnosticReport(payload.diagnostics), response.headers.get("X-Request-ID") || undefined);
  return new Error(detail);
}
