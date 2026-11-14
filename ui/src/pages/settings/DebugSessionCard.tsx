import { useState } from "react";
import { Alert, Button, Card, Descriptions, Form, InputNumber, Popconfirm, Select, Space, Table, Tag, Typography, message } from "antd";
import dayjs from "dayjs";
import { useTranslation } from "react-i18next";
import { api, usePolling } from "../../api";
import { apiErrorText } from "../../apiError";
import { useIsAdmin } from "../../components/useIsAdmin";
import { formatBytes } from "../../format";
import { nodeDisplayName } from "../../nodeDisplay";
import type { DebugSession, DebugSessionNode } from "../../diagnosticsApi";
import { HelpTip } from "../../ui-kit/HelpTip";

const { Paragraph, Text } = Typography;

const GiB = 1 << 30;

/** DIAGNOSTICS_ANCHOR is the fragment links to the diagnostics tab use (/settings#diagnostics). */
export const DIAGNOSTICS_ANCHOR = "diagnostics";

function when(iso?: string): string {
  return iso ? dayjs(iso).format("YYYY-MM-DD HH:mm:ss") : "—";
}

function nodeStateTag(t: (k: string) => string, node: DebugSessionNode) {
  const color = node.state === "collecting" ? "processing" : node.state === "failed" ? "error" : "default";
  return <Tag color={color}>{t(`diagnostics.debug.nodeState.${node.state}`)}</Tag>;
}

/**
 * DebugSessionCard opens a time- and size-bounded debug session: the server's
 * and the chosen nodes' debug logs, and the chosen deployments' raw requests,
 * written to their own files for a diagnostics collection to pick up.
 */
export default function DebugSessionCard() {
  const { t } = useTranslation("settings");
  const { t: tCommon } = useTranslation("common");
  const isAdmin = useIsAdmin();
  const state = usePolling(() => api.debugSession(), 5000);
  const deployments = usePolling(() => api.deployments(), 60000);
  const nodes = usePolling(() => api.nodes(), 60000);
  const [minutes, setMinutes] = useState<number | null>(null);
  const [gib, setGib] = useState<number | null>(null);
  const [deploymentIDs, setDeploymentIDs] = useState<number[]>([]);
  const [nodeUUIDs, setNodeUUIDs] = useState<string[]>([]);
  const [busy, setBusy] = useState(false);

  const limits = state.data?.limits;
  const session = state.data?.session ?? null;
  const open = !!session?.open;

  const start = async () => {
    if (!limits) return;
    setBusy(true);
    try {
      await api.startDebugSession({
        duration_seconds: Math.round((minutes ?? limits.default_duration_seconds / 60) * 60),
        max_bytes: Math.round((gib ?? limits.default_max_bytes / GiB) * GiB),
        deployment_ids: deploymentIDs,
        node_uuids: nodeUUIDs,
      });
      message.success(t("diagnostics.debug.started"));
      await state.refresh();
    } catch (error) {
      message.error(apiErrorText(tCommon, error));
    } finally {
      setBusy(false);
    }
  };

  const stop = async () => {
    setBusy(true);
    try {
      await api.stopDebugSession();
      message.success(t("diagnostics.debug.stopped"));
      await state.refresh();
    } catch (error) {
      message.error(apiErrorText(tCommon, error));
    } finally {
      setBusy(false);
    }
  };

  const summary = (s: DebugSession) => (
    <Descriptions size="small" column={2} bordered>
      <Descriptions.Item label={t("diagnostics.debug.startedAt")}>{when(s.started_at)} · {s.started_by || "—"}</Descriptions.Item>
      <Descriptions.Item label={s.open ? t("diagnostics.debug.until") : t("diagnostics.debug.endedAt")}>
        {s.open ? when(s.until) : `${when(s.ended_at)} · ${t(`diagnostics.debug.ended.${s.ended ?? "stopped"}`)}`}
      </Descriptions.Item>
      <Descriptions.Item label={t("diagnostics.debug.used")}>
        {formatBytes(s.server_log_bytes + s.request_bytes)} / {formatBytes(s.max_bytes)}
      </Descriptions.Item>
      <Descriptions.Item label={t("diagnostics.debug.requests")}>{s.requests}</Descriptions.Item>
    </Descriptions>
  );

  return (
    <div id={DIAGNOSTICS_ANCHOR}>
      <Card title={t("diagnostics.debug.title")} extra={open ? <Tag color="processing">{t("diagnostics.debug.collecting")}</Tag> : null}>
        <Paragraph type="secondary">{t("diagnostics.debug.description")} <HelpTip content={t("diagnostics.debug.descriptionHelp")} /></Paragraph>
        {state.error && <Alert type="error" showIcon message={apiErrorText(tCommon, state.error)} style={{ marginBottom: 12 }} />}
        {!isAdmin && <Alert type="info" showIcon message={t("diagnostics.adminOnly")} style={{ marginBottom: 12 }} />}
        {session && (
          <Space direction="vertical" size={12} style={{ width: "100%", marginBottom: 16 }}>
            {!open && <Text strong>{t("diagnostics.debug.last")}</Text>}
            {summary(session)}
            {session.nodes.length > 0 && (
              <Table<DebugSessionNode>
                size="small"
                rowKey="uuid"
                pagination={false}
                dataSource={session.nodes}
                columns={[
                  { title: t("diagnostics.debug.node"), dataIndex: "name" },
                  { title: t("diagnostics.debug.nodeStateTitle"), render: (_, n) => nodeStateTag(t, n) },
                  { title: t("diagnostics.debug.reason"), render: (_, n) => (n.reason ? t(`diagnostics.debug.nodeReason.${n.reason}`, { defaultValue: n.reason }) : "—") },
                ]}
              />
            )}
          </Space>
        )}
        {open ? (
          isAdmin && (
            <Popconfirm title={t("diagnostics.debug.stopConfirm")} onConfirm={stop} okButtonProps={{ loading: busy }}>
              <Button danger loading={busy}>{t("diagnostics.debug.stop")}</Button>
            </Popconfirm>
          )
        ) : (
          <Form layout="vertical" disabled={!isAdmin || !limits}>
            <Alert type="warning" showIcon message={t("diagnostics.debug.contentWarning", { days: limits?.keep_days ?? 7 })} style={{ marginBottom: 12 }} />
            <Space wrap size={16}>
              <Form.Item
                label={t("diagnostics.debug.duration")}
                extra={limits && t("diagnostics.debug.durationHelp", { max: limits.max_duration_seconds / 60 })}
              >
                <InputNumber
                  aria-label={t("diagnostics.debug.duration")}
                  min={limits ? limits.min_duration_seconds / 60 : 1}
                  max={limits ? limits.max_duration_seconds / 60 : 60}
                  value={minutes ?? (limits ? limits.default_duration_seconds / 60 : null)}
                  onChange={setMinutes}
                  suffix={t("diagnostics.debug.minutes")}
                  style={{ width: 160 }}
                />
              </Form.Item>
              <Form.Item label={t("diagnostics.debug.size")} extra={limits && t("diagnostics.debug.sizeHelp", { max: limits.max_max_bytes / GiB, node: formatBytes(limits.node_max_bytes), request: formatBytes(limits.request_max_bytes) })}>
                <InputNumber
                  aria-label={t("diagnostics.debug.size")}
                  min={1}
                  max={limits ? limits.max_max_bytes / GiB : 50}
                  value={gib ?? (limits ? limits.default_max_bytes / GiB : null)}
                  onChange={setGib}
                  suffix="GiB"
                  style={{ width: 160 }}
                />
              </Form.Item>
            </Space>
            <Form.Item label={t("diagnostics.debug.deployments")} extra={t("diagnostics.debug.deploymentsHelp")}>
              <Select
                mode="multiple"
                aria-label={t("diagnostics.debug.deployments")}
                value={deploymentIDs}
                onChange={setDeploymentIDs}
                optionFilterProp="label"
                options={(deployments.data?.items ?? []).map((d) => ({ value: d.id, label: d.name }))}
              />
            </Form.Item>
            <Form.Item label={t("diagnostics.debug.nodes")} extra={t("diagnostics.debug.nodesHelp")}>
              <Select
                mode="multiple"
                aria-label={t("diagnostics.debug.nodes")}
                value={nodeUUIDs}
                onChange={setNodeUUIDs}
                optionFilterProp="label"
                options={(nodes.data?.items ?? []).map((n) => ({ value: n.uuid, label: nodeDisplayName(n) }))}
              />
            </Form.Item>
            <Popconfirm title={t("diagnostics.debug.startConfirm")} onConfirm={start} okButtonProps={{ loading: busy }} disabled={!isAdmin || !limits}>
              <Button type="primary" loading={busy}>{t("diagnostics.debug.start")}</Button>
            </Popconfirm>
          </Form>
        )}
      </Card>
    </div>
  );
}
