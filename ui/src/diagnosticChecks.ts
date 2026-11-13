import type { ProductMessage } from "./apiError";

/** A bounded explanation of the checks actually made for one request. */
export interface DiagnosticCheck {
  id: string;
  status: string;
  message: ProductMessage;
  blocked_by?: string[];
}

export interface DiagnosticReport {
  checks: DiagnosticCheck[];
  truncated?: boolean;
}

/** Ignore malformed optional diagnostics while retaining the ordinary error. */
export function diagnosticReport(value: unknown): DiagnosticReport | undefined {
  if (!value || typeof value !== "object" || !("checks" in value) || !Array.isArray(value.checks)) return undefined;
  const checks = value.checks.filter((item): item is DiagnosticCheck => {
    if (!item || typeof item !== "object") return false;
    const { id, status, message, blocked_by } = item;
    return typeof id === "string" && typeof status === "string" &&
      message && typeof message === "object" && typeof message.code === "string" && typeof message.message === "string" &&
      (message.params === undefined || (message.params !== null && typeof message.params === "object" && !Array.isArray(message.params))) &&
      (blocked_by === undefined || (Array.isArray(blocked_by) && blocked_by.every((id: unknown) => typeof id === "string")));
  }).slice(0, 128);
  return { checks, ...(("truncated" in value && value.truncated === true) || value.checks.length > checks.length ? { truncated: true } : {}) };
}
