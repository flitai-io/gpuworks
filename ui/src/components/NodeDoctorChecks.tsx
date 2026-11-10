import { Alert, Descriptions, Space, Tag, Typography } from "antd";
import { useTranslation } from "react-i18next";
import type { ReactNode } from "react";
import type { DoctorFinding, NodeDoctorReport } from "../api";
import { formatTime } from "../i18n/datetime";

export default function NodeDoctorChecks({ report, renderFinding }: { report: NodeDoctorReport; renderFinding: (finding: DoctorFinding) => ReactNode }) {
  const { t } = useTranslation("nodes");
  return <Space direction="vertical" size={16} style={{ display: "flex" }}>
    <Alert type="info" showIcon message={t(`doctorChecks.summary.${report.summary}`, { defaultValue: t("doctorChecks.summary.unavailable") })} />
    <Typography.Text type="secondary">{t("doctorChecks.checkedAt", { time: formatTime(report.checked_at) })}</Typography.Text>
    {(report.checks ?? []).map((check, index) => <section key={`${check.stage}-${index}`} aria-label={check.finding ? t(`doctor.finding.${check.finding.code}.label`, { defaultValue: check.finding.code }) : t(`doctorChecks.stage.${check.stage}`)}>
      <Descriptions size="small" column={1} bordered title={check.finding ? t(`doctor.finding.${check.finding.code}.label`, { defaultValue: check.finding.code }) : t(`doctorChecks.stage.${check.stage}`)}>
        <Descriptions.Item label={t("doctorChecks.result")}>
          <Tag color={check.status === "passed" ? "green" : check.status === "failed" ? "red" : check.status === "warning" ? "orange" : check.status === "info" ? "blue" : "default"}>{t(`doctorChecks.status.${check.status}`)}</Tag>
        </Descriptions.Item>
        <Descriptions.Item label={t("doctorChecks.evidence")}>{check.finding ? renderFinding(check.finding) : t(`doctorChecks.reason.${check.reason}`, { defaultValue: t("doctorChecks.reason.not_observed") })}</Descriptions.Item>
        <Descriptions.Item label={t("doctorChecks.vantage")}>{t(`doctorChecks.vantageName.${check.vantage}`)}</Descriptions.Item>
        {check.target && <Descriptions.Item label={t("doctorChecks.target")}>{check.target}</Descriptions.Item>}
        {check.source && <Descriptions.Item label={t("doctorChecks.source")}>{t(`doctorChecks.sourceName.${check.source}`)}</Descriptions.Item>}
        <Descriptions.Item label={t("doctorChecks.observedAt")}>{check.observed_at ? formatTime(check.observed_at) : t("doctorChecks.unknown")}</Descriptions.Item>
      </Descriptions>
    </section>)}
  </Space>;
}
