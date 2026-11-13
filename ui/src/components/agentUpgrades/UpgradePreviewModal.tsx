import { Alert, Button, Checkbox, Modal, Space, Spin, Table, Typography } from "antd";
import { useEffect, useState } from "react";
import { useTranslation } from "react-i18next";
import { Badge } from "../ui/badge";
import ServerBuildSummary from "../ServerBuildSummary";
import type { AgentUpgradeCandidate, AgentUpgradePreview } from "../../agentUpgradesTypes";

export default function UpgradePreviewModal({ open, preview, loading, busy, error, onClose, onRefresh, onStart }: {
  open: boolean;
  preview: AgentUpgradePreview | null;
  loading: boolean;
  busy: boolean;
  error: string;
  onClose: () => void;
  onRefresh: () => void;
  onStart: () => void;
}) {
  const { t } = useTranslation("nodes");
  const [acknowledged, setAcknowledged] = useState(false);
  useEffect(() => setAcknowledged(false), [open, preview]);
  const eligible = preview?.counts.eligible ?? 0;
  return <Modal open={open} title={t("batchUpgrade.previewTitle")} onCancel={() => !busy && onClose()} destroyOnHidden
    afterClose={() => setAcknowledged(false)} footer={<Space>
      <Button disabled={busy} onClick={onClose}>{t("batchUpgrade.close")}</Button>
      <Button disabled={loading || busy} onClick={() => { setAcknowledged(false); onRefresh(); }}>{t("batchUpgrade.refreshPreview")}</Button>
      <Button type="primary" aria-label={t("batchUpgrade.start")} loading={busy} disabled={loading || !preview || !acknowledged || eligible === 0 || !!preview.blocked_by_item} onClick={onStart}>
        {t("batchUpgrade.start")}
      </Button>
    </Space>} width={850}>
    <Space direction="vertical" style={{ width: "100%" }}>
      <Typography.Paragraph>{t("batchUpgrade.scope")}</Typography.Paragraph>
      {loading && <Spin aria-label={t("batchUpgrade.loadingPreview")} />}
      {error && <Alert type="error" showIcon message={error} />}
      {preview && <>
        <Typography.Text strong>{t("batchUpgrade.targetHeading")}</Typography.Text>
        <ServerBuildSummary version={preview.target_version} build={preview.target_build} />
        <Typography.Paragraph>{t("batchUpgrade.targetSource")} <Typography.Link href="/settings#updates" target="_blank" rel="noopener noreferrer">{t("batchUpgrade.versionDetails")}</Typography.Link></Typography.Paragraph>
        <Typography.Text>{t("batchUpgrade.previewCounts", { total: preview.node_uuids.length, eligible, skipped: preview.node_uuids.length - eligible })}</Typography.Text>
        {preview.blocked_by_item && <Alert type="warning" showIcon message={t(preview.blocked_by_item.reason === "unconfirmed" ? "batchUpgrade.blocked" : "batchUpgrade.activeBlock", { name: preview.blocked_by_item.name || preview.blocked_by_item.node_uuid })} />}
        {!eligible && <Alert type="info" showIcon message={t("batchUpgrade.noEligible")} />}
        <Table<AgentUpgradeCandidate> size="small" rowKey="node_uuid" dataSource={preview.nodes} pagination={{ pageSize: 10, showSizeChanger: false }} columns={[
          { title: t("batchUpgrade.columns.node"), key: "node", render: (_, node) => <Space direction="vertical" size={0}><Typography.Text>{node.name || node.node_uuid}</Typography.Text><Typography.Text type="secondary">{node.node_uuid}</Typography.Text></Space> },
          { title: t("fleet.versionChange"), key: "version", render: (_, node) => node.eligibility === "eligible"
            ? <div className="flex flex-col items-start gap-1"><span>{node.from_version} → {preview.target_version}</span>
                <Badge size="sm" status="neutral">{t(node.downgrade ? "fleet.status.downgrade" : node.version_change === "upgrade" ? "fleet.willUpgrade" : "fleet.replaceBuild")}</Badge></div>
            : node.from_version || "—" },
          { title: t("batchUpgrade.columns.platform"), key: "platform", render: (_, node) => `${node.os || "—"}/${node.arch || "—"}` },
          { title: t("batchUpgrade.columns.result"), key: "eligibility", render: (_, node) => t(`batchUpgrade.eligibility.${node.eligibility}`, { defaultValue: t("batchUpgrade.eligibility.unavailable") }) },
          { title: t("batchUpgrade.columns.reason"), key: "reason", render: (_, node) => node.reason ? t(`batchUpgrade.reason.${node.reason}`, { defaultValue: node.reason }) : "—" },
        ]} />
      </>}
      <Alert type="warning" showIcon message={t("batchUpgrade.disruption")} description={t("batchUpgrade.versionWarning")} />
      <Checkbox checked={acknowledged} disabled={busy || loading} onChange={(event) => setAcknowledged(event.target.checked)}>{t("batchUpgrade.acknowledge")}</Checkbox>
    </Space>
  </Modal>;
}
