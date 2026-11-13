import { useTranslation } from "react-i18next";
import type { ReactNode } from "react";
import type { DoctorFinding, NodeDoctorReport } from "../api";
import { formatTime } from "../i18n/datetime";
import { Badge } from "./ui/badge";
import { Notice } from "../ui-kit/Notice";

export default function NodeDoctorChecks({ report, renderFinding }: { report: NodeDoctorReport; renderFinding: (finding: DoctorFinding) => ReactNode }) {
  const { t } = useTranslation("nodes");
  return <div className="flex min-w-0 flex-col gap-4">
    <Notice title={t(`doctorChecks.summary.${report.summary}`, { defaultValue: t("doctorChecks.summary.unavailable") })} />
    <p className="text-fg-muted">{t("doctorChecks.checkedAt", { time: formatTime(report.checked_at) })}</p>
    {(report.checks ?? []).map((check, index) => <section key={`${check.stage}-${index}`} aria-label={check.finding ? t(`doctor.finding.${check.finding.code}.label`, { defaultValue: check.finding.code }) : t(`doctorChecks.stage.${check.stage}`)}>
      <h3 className="mb-3 text-body font-semibold">{check.finding ? t(`doctor.finding.${check.finding.code}.label`, { defaultValue: check.finding.code }) : t(`doctorChecks.stage.${check.stage}`)}</h3>
      <dl className="divide-y divide-border rounded-lg border border-border">
        <CheckField label={t("doctorChecks.result")}>
          <Badge status={check.status === "passed" ? "success" : check.status === "failed" ? "danger" : check.status === "warning" ? "warning" : check.status === "info" ? "info" : "neutral"}>{t(`doctorChecks.status.${check.status}`)}</Badge>
        </CheckField>
        <CheckField label={t("doctorChecks.evidence")}>{check.finding ? renderFinding(check.finding) : t(`doctorChecks.reason.${check.reason}`, { defaultValue: t("doctorChecks.reason.not_observed") })}</CheckField>
        <CheckField label={t("doctorChecks.vantage")}>{t(`doctorChecks.vantageName.${check.vantage}`)}</CheckField>
        {check.target && <CheckField label={t("doctorChecks.target")}>{check.target}</CheckField>}
        {check.source && <CheckField label={t("doctorChecks.source")}>{t(`doctorChecks.sourceName.${check.source}`)}</CheckField>}
        <CheckField label={t("doctorChecks.observedAt")}>{check.observed_at ? formatTime(check.observed_at) : t("doctorChecks.unknown")}</CheckField>
      </dl>
    </section>)}
  </div>;
}

function CheckField({ label, children }: { label: ReactNode; children: ReactNode }) {
  return <div className="grid grid-cols-[minmax(100px,1fr)_minmax(0,2fr)] gap-3 px-3 py-2 text-body">
    <dt className="text-fg-muted">{label}</dt>
    <dd className="min-w-0 break-words">{children}</dd>
  </div>;
}
