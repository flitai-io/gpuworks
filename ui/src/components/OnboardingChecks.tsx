import { useTranslation } from "react-i18next";
import type { CheckResult } from "../api";
import { productMessageText, type APIErrorTranslator } from "../apiError";
import { CheckStatusBadge } from "../checkStatus";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "./ui/table";

/** What a person reads of one onboarding check: the text of its code in
 * this language (the English fallback when the code is unknown), or, for a
 * bare pass without a code, what the check observed. */
export function onboardingCheckText(t: APIErrorTranslator, check: CheckResult): string {
  if (check.code) return productMessageText(t, { code: check.code, params: check.params, message: check.message ?? "" });
  return check.message || check.evidence || "";
}

/** The check's name in this language; an id this client does not know shows
 * as itself. */
export function onboardingCheckName(t: APIErrorTranslator, id: string): string {
  return t(`onboardingCheck.${id}`, { defaultValue: id });
}

export interface OnboardingCheckCounts {
  pass: number;
  warn: number;
  fail: number;
  skip: number;
}

/** Counts per status; na is left out (it is neither a pass nor a problem)
 * and an unknown status counts as skip. */
export function countOnboardingChecks(checks: CheckResult[]): OnboardingCheckCounts {
  const counts: OnboardingCheckCounts = { pass: 0, warn: 0, fail: 0, skip: 0 };
  for (const check of checks) {
    if (check.status === "na") continue;
    if (check.status === "pass" || check.status === "warn" || check.status === "fail") counts[check.status] += 1;
    else counts.skip += 1;
  }
  return counts;
}

function CheckDetail({ check }: { check: CheckResult }) {
  const { t } = useTranslation("common");
  const text = onboardingCheckText(t, check);
  // A bare pass shows what it observed as its text; anything else keeps the
  // command's or provider's output one click away.
  const evidence = check.code && check.evidence ? check.evidence : "";
  return (
    <>
      {text || "—"}
      {evidence && (
        <details>
          <summary style={{ cursor: "pointer" }}>{t("onboardingCheckEvidence")}</summary>
          <pre className="m-0 whitespace-pre-wrap break-all font-mono text-label">{evidence}</pre>
        </details>
      )}
    </>
  );
}

/** One table for the checks of a precheck, an SSH install or a provider
 * install: name, status (all five distinct), and the text of the outcome. */
export default function OnboardingChecks({ checks, labels }: {
  checks: CheckResult[];
  labels: { item: string; status: string; detail: string };
}) {
  const { t } = useTranslation("common");
  return (
    <Table size="sm">
      <TableHeader>
        <TableRow>
          <TableHead style={{ width: 150 }}>{labels.item}</TableHead>
          <TableHead style={{ width: 100 }}>{labels.status}</TableHead>
          <TableHead>{labels.detail}</TableHead>
        </TableRow>
      </TableHeader>
      <TableBody>
        {checks.map((check, index) => (
          <TableRow key={`${check.id}:${index}`}>
            <TableCell>{onboardingCheckName(t, check.id)}</TableCell>
            <TableCell><CheckStatusBadge status={check.status} /></TableCell>
            <TableCell><CheckDetail check={check} /></TableCell>
          </TableRow>
        ))}
      </TableBody>
    </Table>
  );
}
