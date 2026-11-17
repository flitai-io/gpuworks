import enPlayground from "../../i18n/locales/en/playground.json";
import enCommon from "../../i18n/locales/en/common.json";
import zhCommon from "../../i18n/locales/zh/common.json";
import zhPlayground from "../../i18n/locales/zh/playground.json";
import { isGenericAPIErrorCode } from "../../apiError";

export const MAX_ERROR_BYTES = 16 * 1024;
export const MAX_ERROR_TEXT = 2048;

function requestText() {
  const language = globalThis.document?.documentElement?.lang ?? "";
  return (language.toLowerCase().startsWith("zh") ? zhPlayground : enPlayground).request;
}

// Errors can contain Pydantic's complete input_value, including uploaded audio.
// Never display that diagnostic payload, even for old persisted conversations.
export function safeErrorText(raw: string): string {
  const text = raw.slice(0, MAX_ERROR_BYTES)
    .split(/\binput_value\s*=|\binput\s*:|["']input["']\s*:/i, 1)[0]
    .replace(/data:[^\s"'<>]*|[A-Za-z0-9+/=_-]{128,}/g, "…");
  return text.length > MAX_ERROR_TEXT ? `${text.slice(0, MAX_ERROR_TEXT - 1)}…` : text.trim();
}

// gatewayErrorText renders a gateway error code in the page's language: a
// specific code from its template and params, a generic one (a status
// category) as the category followed by the server's English detail. Empty
// when the code has no text or a param its template names is missing, so the
// caller falls back to the server's message.
function gatewayErrorText(code: string, params: Record<string, unknown> | undefined, message: string): string {
  const common = (globalThis.document?.documentElement?.lang ?? "").toLowerCase().startsWith("zh") ? zhCommon : enCommon;
  const template = (common.apiError as Record<string, string>)[code];
  if (!template) return "";
  if (isGenericAPIErrorCode(code)) {
    return message.trim() ? common.apiErrorDetail.replace("{{category}}", template).replace("{{detail}}", message.trim()) : template;
  }
  let missing = false;
  const text = template.replace(/{{(\w+)}}/g, (_, key: string) => {
    const value = params?.[key];
    if (value === undefined || value === null) missing = true;
    return String(value ?? "");
  });
  return missing ? "" : text;
}

export function playgroundErrorText(status: number, raw: string | undefined): string {
  const fallback = `Request failed (${status})`;
  if (raw === undefined || raw.length > MAX_ERROR_BYTES) return `${fallback}. ${requestText().errorTooLarge}`;
  try {
    const parsed = JSON.parse(raw) as { error?: { message?: unknown; code?: string; params?: Record<string, unknown> } | string; message?: unknown; detail?: unknown };
    if (parsed.error && typeof parsed.error === "object" && typeof parsed.error.code === "string") {
      const text = gatewayErrorText(parsed.error.code, parsed.error.params, typeof parsed.error.message === "string" ? parsed.error.message : "");
      if (text) return safeErrorText(text);
    }
    if (typeof parsed.error === "string" && parsed.error) return safeErrorText(parsed.error);
    if (parsed.error && typeof parsed.error === "object" && typeof parsed.error.message === "string") return safeErrorText(parsed.error.message);
    if (typeof parsed.message === "string" && parsed.message) return safeErrorText(parsed.message);
    if (status === 404 && parsed.detail === "Not Found") return requestText().routeMissing;
    if (typeof parsed.detail === "string") return safeErrorText(parsed.detail);
    if (Array.isArray(parsed.detail)) {
      // Pydantic's input/ctx/loc may contain user data. Only its human message
      // is needed to explain validation; bound both item count and final text.
      const messages = parsed.detail.slice(0, 4).flatMap((entry) =>
        entry && typeof entry === "object" && typeof entry.msg === "string" ? [entry.msg] : []);
      if (messages.length) return safeErrorText(messages.join("; "));
    }
    return fallback;
  } catch {
    return raw.trim().startsWith("<") ? fallback : safeErrorText(raw) || fallback;
  }
}

// Stop consuming an upstream error once it exceeds the diagnostic budget.
// Cancellation releases the connection, including chunked errors without a length.
export async function readErrorBody(response: Response): Promise<string | undefined> {
  if (!response.body) return "";
  const reader = response.body.getReader();
  const decoder = new TextDecoder();
  let size = 0;
  let text = "";
  try {
    if (Number(response.headers.get("Content-Length")) > MAX_ERROR_BYTES) return undefined;
    while (true) {
      const { done, value } = await reader.read();
      if (done) return text + decoder.decode();
      size += value.byteLength;
      if (size > MAX_ERROR_BYTES) return undefined;
      text += decoder.decode(value, { stream: true });
    }
  } finally {
    try { await reader.cancel(); } catch { /* Preserve the original read/abort error. */ }
    reader.releaseLock();
  }
}
