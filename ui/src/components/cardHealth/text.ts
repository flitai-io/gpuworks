import type { TFunction } from "i18next";
import type { CheckStatus } from "../../api";

// Wording for card checks. Every value here comes from the server (check ids,
// statuses, codes); this only names them in the reader's language.

export function checkName(t: TFunction, id: string): string {
  return t(`cardHealth.check.${id}`, { ns: "nodes", defaultValue: id });
}

export function checkHelp(t: TFunction, id: string): string {
  return t(`cardHealth.checkHelp.${id}`, { ns: "nodes", defaultValue: "" });
}

export function statusName(t: TFunction, status: CheckStatus | "stale"): string {
  return t(`cardHealth.state.${status}`, { ns: "nodes", defaultValue: status });
}

/** The codes a card check reports beyond the accelerator health codes. */
const checkCodes = new Set(["FOREIGN_PROCESSES", "PEER_MEMORY_MISSING"]);

/** What a check found, or why it was skipped, in words; the code itself when this client does not know it. */
export function codeText(t: TFunction, status: CheckStatus, code?: string): string {
  if (!code) return "";
  if (status === "skip") return t(`cardHealth.skip.${code}`, { ns: "nodes", defaultValue: code });
  if (checkCodes.has(code)) return t(`cardHealth.reason.${code}`, { ns: "nodes" });
  return t(`gpuHealth.code.${code}.label`, { ns: "common", defaultValue: code });
}

export function periodText(t: TFunction, seconds: number): string {
  const minutes = Math.round(seconds / 60);
  return minutes <= 1 ? t("cardHealth.period.minute", { ns: "nodes" }) : t("cardHealth.period.minutes", { ns: "nodes", count: minutes });
}

export function durationText(t: TFunction, seconds: number): string {
  if (seconds < 60) return t("cardHealth.duration.seconds", { ns: "nodes", count: Math.max(0, Math.round(seconds)) });
  if (seconds < 3600) return t("cardHealth.duration.minutes", { ns: "nodes", count: Math.floor(seconds / 60) });
  if (seconds < 86400) return t("cardHealth.duration.hours", { ns: "nodes", count: Math.floor(seconds / 3600) });
  return t("cardHealth.duration.days", { ns: "nodes", count: Math.floor(seconds / 86400) });
}

export function cardName(t: TFunction, index: number | undefined, fallback: string): string {
  return index !== undefined && index >= 0 ? t("cardHealth.card", { ns: "nodes", index }) : fallback;
}

export function localTime(iso?: string): string {
  if (!iso) return "-";
  const d = new Date(iso);
  return Number.isNaN(d.getTime()) ? iso : d.toLocaleString();
}
