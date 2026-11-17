import { useTranslation } from "react-i18next";
import { Badge } from "./components/ui/badge";
import type { BadgeStatus } from "./components/ui/badge-colors";

/** The five check statuses (ADR 0067, internal/proto/check.go). */
export const checkStatuses = ["pass", "warn", "fail", "skip", "na"] as const;
export type CheckStatus = (typeof checkStatuses)[number];

export function isCheckStatus(value: unknown): value is CheckStatus {
  return typeof value === "string" && (checkStatuses as readonly string[]).includes(value);
}

/** skip and na are neutral: a check that did not run, or does not apply, is
 * never drawn as passed. An unknown value is drawn as skip. */
export function checkStatusTone(status: string): BadgeStatus {
  switch (status) {
    case "pass": return "success";
    case "warn": return "warning";
    case "fail": return "danger";
    default: return "neutral";
  }
}

/** The worst status in a list, in the order fail > warn > skip > pass > na;
 * an empty list is na. */
export function worstCheckStatus(statuses: string[]): CheckStatus {
  const rank: Record<CheckStatus, number> = { fail: 4, warn: 3, skip: 2, pass: 1, na: 0 };
  let worst: CheckStatus = "na";
  for (const raw of statuses) {
    const status: CheckStatus = isCheckStatus(raw) ? raw : "skip";
    if (rank[status] > rank[worst]) worst = status;
  }
  return worst;
}

export function CheckStatusBadge({ status }: { status: string }) {
  const { t } = useTranslation("common");
  const known: CheckStatus = isCheckStatus(status) ? status : "skip";
  return <Badge status={checkStatusTone(known)}>{t(`checkStatus.${known}`)}</Badge>;
}
