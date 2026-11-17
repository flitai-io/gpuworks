import { Alert, Space, Tag, Typography } from "antd";
import { useTranslation } from "react-i18next";
import type { PartitionCapability as Capability } from "../api";
import { formatTime } from "../i18n/datetime";
import { partitionReasonText } from "./partitionCapabilities";

export default function PartitionCapability({ capability, compact = false }: { capability?: Capability; compact?: boolean }) {
  const { t } = useTranslation("common");
  if (!capability) return <Typography.Text type="secondary">{t("partitionCapability.none")}</Typography.Text>;
  const ready = capability.status === "ready";
  const reason = partitionReasonText(t, capability);
  const probeDetail = capability.reason_params?.probe_detail;
  const probeStatusText = capability.probe_status
    ? t(`partitionCapability.probeStatus.${capability.probe_status}`, { defaultValue: capability.probe_status })
    : undefined;
  if (compact) {
    return <Space size={4} wrap>
      <Tag color={ready ? "green" : "default"}>{capability.display_name || capability.backend}</Tag>
      <Tag color={ready ? "success" : "error"}>{ready ? t("partitionCapability.available") : t("partitionCapability.unavailable")}</Tag>
      {capability.maturity === "experimental" && <Tag color="orange">{t("partitionCapability.experimental")}</Tag>}
	  {!!capability.compatibility_notes?.length && <Tag color="gold">{t("partitionCapability.compatibilityHint")}</Tag>}
    </Space>;
  }
  return <Alert
    type={ready ? (capability.maturity === "experimental" ? "warning" : "success") : "info"}
    showIcon
    message={<Space wrap>
      <Typography.Text strong>{capability.display_name || capability.backend}</Typography.Text>
      <Tag color={ready ? "success" : "error"}>{ready ? t("partitionCapability.ready") : t("partitionCapability.notReady")}</Tag>
      {capability.maturity === "experimental" && <Tag color="orange">{t("partitionCapability.experimentalBackend")}</Tag>}
	  {capability.probe_status && <Tag color={capability.probe_status === "passed" ? "green" : capability.probe_status === "failed" ? "red" : "default"}>{t("partitionCapability.probe", { status: probeStatusText })}</Tag>}
    </Space>}
    description={<Space direction="vertical" size={2}>
      {capability.compatibility_summary && <Typography.Text>{capability.compatibility_summary}</Typography.Text>}
	  {capability.probe_checked_at && <Typography.Text type="secondary">{t("partitionCapability.lastChecked", { time: formatTime(capability.probe_checked_at) })}</Typography.Text>}
	  {!!capability.compatibility_notes?.length && <Alert type="warning" showIcon message={t("partitionCapability.compatibilityHint")} description={<Space direction="vertical" size={0}>{capability.compatibility_notes.map((note) => <Typography.Text key={note}>{note}</Typography.Text>)}</Space>} />}
      {reason && <Typography.Text>{reason}{probeDetail && <> <code>{probeDetail}</code></>}</Typography.Text>}
    </Space>}
  />;
}
