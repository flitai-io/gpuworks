import type { TFunction } from "i18next";
import { APIError, apiErrorText } from "./apiError";

/** The type names a person reads: local storage, NFS, NFS with local cache, mounted shared directory. */
export function storageTypeLabel(t: TFunction, type?: string): string {
  if (!type) return "";
  return t(`storageType.${type}`, { ns: "common", defaultValue: type });
}

/** A node's state for a storage (ready, mounting, failed, unresponsive, unsupported, offline). */
export function storageStateLabel(t: TFunction, state: string): string {
  return t(`storageNodeState.${state}`, { ns: "common", defaultValue: state });
}

/** A reason code from the server (acquisition unavailable, location reasons) as text; unknown codes pass through. */
export function storageReasonText(t: TFunction, code?: string): string {
  if (!code) return "";
  // Reasons only the storage pages show are in the storage namespace.
  return t(`storageReason.${code}`, { ns: ["common", "storage"], defaultValue: code });
}

/** A reason shown in parts where there is room: what is wrong, what the agent does or why it leaves the mount alone, and the ways out. */
export interface StorageReasonParts {
  summary: string;
  reason: string;
  fixes: string[];
}

// The reasons of an unresponsive mount, each with its ways out in the order
// shown. "wait" is the one fix several of them share; a code listed in
// ownSummary has its own summary, the rest share "stuck".
const unresponsiveFixes = new Map<string, string[]>([
  ["unresponsive_retrying", []],
  ["stuck_will_remount", []],
  ["stuck_in_use", ["wait", "stuck_in_use"]],
  ["stuck_not_managed", ["wait", "stuck_not_managed"]],
  ["stuck_nested_mount", ["wait", "stuck_nested_mount"]],
  ["stuck_shared_server", ["wait", "stuck_shared_server"]],
  ["stuck_restart_needed", ["stuck_restart_needed"]],
]);
const ownSummary = new Set(["unresponsive_retrying", "stuck_restart_needed"]);

/**
 * The parts of an unresponsive-mount reason for a surface with room (a drawer
 * section); null for every other code, which has only storageReasonText.
 */
export function storageReasonParts(t: TFunction, code?: string): StorageReasonParts | null {
  const fixes = code ? unresponsiveFixes.get(code) : undefined;
  if (!code || !fixes) return null;
  const part = (key: string) => t(`storageReason.unresponsive.${key}`, { ns: "common" });
  return {
    summary: part(`summary.${ownSummary.has(code) ? code : "stuck"}`),
    reason: part(`reason.${code}`),
    fixes: fixes.map((fix) => part(`fix.${fix}`)),
  };
}

/**
 * A failed storage action as text: the server's error in the person's language,
 * and for a storage a deployment still uses, which deployments (#id).
 */
export function storageErrorText(t: TFunction, error: unknown): string {
  const base = apiErrorText((key, options) => t(key, { ns: "common", ...options }), error);
  if (error instanceof APIError && error.code === "STORAGE_IN_USE") {
    const ids = Array.isArray(error.params?.deployment_ids) ? (error.params.deployment_ids as number[]) : [];
    if (ids.length > 0) return `${base} (${ids.map((id) => `#${id}`).join(", ")})`;
  }
  return base;
}

/** Share of a volume in use, 0 to 100, for a progress bar. */
export function usedPercent(total?: number, available?: number): number {
  if (!total) return 0;
  return Math.max(0, Math.min(100, Math.round(((total - (available ?? 0)) / total) * 100)));
}
