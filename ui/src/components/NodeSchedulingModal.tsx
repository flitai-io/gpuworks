import { Alert, Button, Modal, Popconfirm, Space, Table, Tag, Tooltip, Typography, message } from "antd";
import { useCallback, useEffect, useState } from "react";
import { useTranslation } from "react-i18next";

import { api, type Node, type NodeReplica, type NodeReplicas, type NodeScheduling, type ScheduleMessage } from "../api";
import { apiErrorText } from "../apiError";
import { nodeDisplayName } from "../nodeDisplay";
import { formatDate } from "../i18n/datetime";
import { scheduleText } from "../pages/deploy/deploySchedule";
import { runtimeGpuBlockedReason } from "../nodes/runtimeGpuBlock";

const DRAIN_REFRESH_MS = 5000;

const schedulingColor: Record<NodeScheduling, string | undefined> = {
  schedulable: undefined,
  cordoned: "orange",
  draining: "volcano",
};

const kindColor: Record<NodeReplica["kind"], string> = {
  pool: "blue",
  pinned: "gold",
  named: "default",
};

// NodeSchedulingTag marks a node that takes no new replicas; a schedulable
// node shows nothing. The tag is the same whoever set it; hovering says who:
// the platform, with its reason and that it lifts the cordon itself once the
// reason is gone, or an operator, whose cordon the platform never lifts.
export function NodeSchedulingTag({ node }: { node: Node }) {
  const { t } = useTranslation("nodes");
  const scheduling = node.scheduling ?? "schedulable";
  if (scheduling === "schedulable") return null;
  const tag = <Tag color={schedulingColor[scheduling]}>{t(`scheduling.state.${scheduling}`)}</Tag>;
  if (scheduling === "draining") return <Tooltip title={t("scheduling.drainHint")}>{tag}</Tooltip>;
  const reason = cordonReasonText(t, node);
  if (!reason) return tag;
  return <Tooltip title={reason}>{tag}</Tooltip>;
}

function cordonReasonText(t: (key: string, options?: Record<string, unknown>) => string, node: Node): string {
  if (node.scheduling_by === "operator") return t("scheduling.by.operator");
  if (node.scheduling_by === "platform") {
    const why = node.cordon_reason === "card_needs_host_reboot" || node.cordon_reason === "kube_unschedulable"
      ? t(`scheduling.cordonReason.${node.cordon_reason}`)
      : runtimeGpuBlockedReason(t, node);
    return why ? `${why} ${t("scheduling.by.platformLifts")}` : t("scheduling.by.platformLifts");
  }
  // A cordon set before its author was recorded: say what is known.
  return runtimeGpuBlockedReason(t, node);
}

// NodeSchedulingModal cordons, drains or uncordons a node and follows a
// drain to empty: what still runs there and whether the drain moves it. A
// pinned replica stays until the operator changes placement or evicts it.
// Stopping a drain keeps the node cordoned rather than enabling placement.
export function NodeSchedulingModal({ node, onClose, onChanged }: { node: Node; onClose: () => void; onChanged: (node: Node) => void }) {
  const { t } = useTranslation("nodes");
  const { t: tCommon } = useTranslation("common");
  const { t: tDeploy } = useTranslation("deploy");
  const [scheduling, setScheduling] = useState<NodeScheduling>(node.scheduling ?? "schedulable");
  const [replicas, setReplicas] = useState<NodeReplicas | null>(null);
  const [loading, setLoading] = useState(true);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");

  const load = useCallback(async () => {
    setLoading(true);
    try {
      const next = await api.nodeReplicas(node.uuid);
      setReplicas(next);
      setScheduling(next.scheduling);
      setError("");
    } catch (err) {
      setError(apiErrorText(tCommon, err));
    } finally {
      setLoading(false);
    }
  }, [node.uuid, tCommon]);

  useEffect(() => {
    void load();
  }, [load]);

  // A drain moves replicas over minutes; follow it while the modal is open.
  useEffect(() => {
    if (scheduling !== "draining") return;
    const timer = window.setInterval(() => void load(), DRAIN_REFRESH_MS);
    return () => window.clearInterval(timer);
  }, [scheduling, load]);

  const change = async (next: NodeScheduling) => {
    setBusy(true);
    setError("");
    try {
      const updated = await api.setNodeScheduling(node.uuid, next);
      setScheduling(updated.scheduling ?? next);
      message.success(t(`scheduling.done.${next}`));
      onChanged(updated);
      await load();
    } catch (err) {
      setError(apiErrorText(tCommon, err));
    } finally {
      setBusy(false);
    }
  };

  const evict = async () => {
    setBusy(true);
    setError("");
    try {
      const result = await api.evictNode(node.uuid);
      message.success(t("scheduling.evicted", { count: result.evicted.length }));
      await load();
    } catch (err) {
      setError(apiErrorText(tCommon, err));
    } finally {
      setBusy(false);
    }
  };

  const items = replicas?.items ?? [];
  const stuck = items.filter((item) => item.kind === "pinned").length;
  const waiting = items.filter((item) => item.state !== "draining" && item.state !== "removing").length;
  const feedback = (item: NodeReplica) => {
    const reason = item.drain_reason;
    const first = reason?.params?.first_reason as ScheduleMessage | undefined;
    const rendered = reason ? scheduleText(tDeploy, first ? { ...reason, params: { ...reason.params, first: scheduleText(tDeploy, first) } } : reason) : "";
    if (item.state === "draining" || item.state === "removing") return <Typography.Text type="secondary">{t("scheduling.retiring")}</Typography.Text>;
    if (item.kind === "pinned") return <Typography.Text type="warning">{t("scheduling.pinnedReason")}</Typography.Text>;
    return <Space direction="vertical" size={0}>
      {rendered && <Typography.Text>{t("scheduling.lastFeedback", { reason: rendered })}</Typography.Text>}
      {item.drain_reason_at && <Typography.Text type="secondary">{formatDate(item.drain_reason_at)}</Typography.Text>}
      {item.drain_message && <Typography.Text type="danger">{t("scheduling.lastFailedAttempt", { message: item.drain_message })}</Typography.Text>}
      {!rendered && !item.drain_message && <Typography.Text type="secondary">{t("scheduling.awaitingReason")}</Typography.Text>}
    </Space>;
  };

  return (
    <Modal open title={t("scheduling.title", { node: nodeDisplayName(node) })} onCancel={onClose} footer={null} width={960}>
      <Space direction="vertical" style={{ width: "100%" }}>
        <Space wrap>
          <Typography.Text>{t("scheduling.current")}</Typography.Text>
          <Tag color={schedulingColor[scheduling]}>{t(`scheduling.state.${scheduling}`)}</Tag>
        </Space>
        <Typography.Paragraph type="secondary" style={{ margin: 0 }}>{t(`scheduling.help.${scheduling}`)}</Typography.Paragraph>
        <Space wrap>
          {scheduling !== "cordoned" && <Button loading={busy} onClick={() => void change("cordoned")}>{t(scheduling === "draining" ? "scheduling.action.stopDrain" : "scheduling.action.cordon")}</Button>}
          {scheduling !== "draining" && <Button loading={busy} onClick={() => void change("draining")}>{t("scheduling.action.drain")}</Button>}
          {scheduling !== "schedulable" && (
            // The platform cordons a node whose runtime cannot give a
            // container its accelerators, and will do so again on the next
            // report. The operator may know better, so this warns rather
            // than refuses.
            node.runtime_gpu_blocked ? (
              <Popconfirm
                title={t("scheduling.blocked.title")}
                description={<span style={{ whiteSpace: "pre-line", maxWidth: 420, display: "inline-block" }}>{t("scheduling.blocked.description", { reason: runtimeGpuBlockedReason(t, node) })}</span>}
                okText={t("scheduling.action.uncordonAnyway")}
                cancelText={t("action.cancel")}
                onConfirm={() => void change("schedulable")}
              >
                <Button type="primary" loading={busy}>{t("scheduling.action.uncordon")}</Button>
              </Popconfirm>
            ) : (
              <Button type="primary" loading={busy} onClick={() => void change("schedulable")}>{t("scheduling.action.uncordon")}</Button>
            )
          )}
          {scheduling !== "schedulable" && items.length > 0 && (
            <Popconfirm title={t("scheduling.action.evictConfirm")} onConfirm={() => void evict()} okText={t("scheduling.action.evict")} cancelText={t("action.cancel")}>
              <Button danger loading={busy}>{t("scheduling.action.evict")}</Button>
            </Popconfirm>
          )}
        </Space>
        {error && <Alert type="error" showIcon message={error} />}
        {scheduling === "draining" && replicas?.drain_offline && <Alert type="warning" showIcon message={t("scheduling.offline")} description={t("scheduling.offlineHint")} />}
        {scheduling === "draining" && waiting > 0 && <Alert type="warning" showIcon message={t("scheduling.waiting", { count: waiting })} description={t("scheduling.stopHint")} />}
        {scheduling === "draining" && replicas && !error && items.length === 0 && <Alert type="success" showIcon message={t("scheduling.completed")} />}
        {scheduling === "draining" && stuck > 0 && <Alert type="warning" showIcon message={t("scheduling.stuck", { count: stuck })} />}
        <Typography.Text strong>{t("scheduling.replicas", { count: items.length })}</Typography.Text>
        <Table<NodeReplica>
          rowKey="instance"
          size="small"
          loading={loading}
          dataSource={items}
          pagination={false}
          locale={{ emptyText: t("scheduling.empty") }}
          columns={[
            ...(scheduling === "draining" ? [{ title: t("scheduling.columns.feedback"), key: "drain_feedback", render: (_: unknown, item: NodeReplica) => feedback(item) }] : []),
            { title: t("scheduling.columns.deployment"), dataIndex: "deployment" },
            { title: t("scheduling.columns.instance"), dataIndex: "instance", render: (uuid: string) => <Typography.Text code>{uuid.slice(0, 12)}</Typography.Text> },
            { title: t("scheduling.columns.state"), dataIndex: "state", width: 110 },
            {
              title: t("scheduling.columns.drain"),
              dataIndex: "kind",
              render: (kind: NodeReplica["kind"]) => <Tag color={kindColor[kind]}>{t(`scheduling.kind.${kind}`)}</Tag>,
            },
          ]}
        />
      </Space>
    </Modal>
  );
}
