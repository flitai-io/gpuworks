import { Progress, Space, Typography } from "antd";
import { useTranslation } from "react-i18next";
import type { AgentUpgradeCounts } from "../../agentUpgradesTypes";

const resultCounts = ["succeeded", "failed", "skipped", "cancelled", "interrupted", "unconfirmed", "queued", "active"] as const;

export default function UpgradeSummary({ counts }: { counts: AgentUpgradeCounts }) {
  const { t } = useTranslation("nodes");
  const percent = counts.total ? Math.min(100, Math.round(counts.processed / counts.total * 100)) : 0;
  const issues = counts.failed + counts.interrupted + counts.unconfirmed;
  return <Space direction="vertical" style={{ width: "100%" }} size={4}>
    <Typography.Text strong>{t("batchUpgrade.processed", { processed: counts.processed, total: counts.total })}</Typography.Text>
    <Progress percent={percent} size="small" showInfo={false} status={issues ? "exception" : "normal"} />
    <Typography.Text>{resultCounts.map((key) => t(`batchUpgrade.result.${key}`, { value: counts[key] })).join(" · ")}</Typography.Text>
    {issues > 0 && <Typography.Text type="warning">{t("batchUpgrade.processingNotSuccess")}</Typography.Text>}
  </Space>;
}
