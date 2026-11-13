import { useState } from "react";
import { Alert, Button, Card, DatePicker, Form, Popconfirm, Select, Space, Switch, Table, Tag, Typography, message } from "antd";
import { DownloadOutlined } from "@ant-design/icons";
import dayjs, { type Dayjs } from "dayjs";
import { useTranslation } from "react-i18next";
import { api, usePolling } from "../../api";
import { apiErrorText } from "../../apiError";
import { useIsAdmin } from "../../components/useIsAdmin";
import { formatBytes } from "../../format";
import { nodeDisplayName } from "../../nodeDisplay";
import { diagnosticsBundleDownloadURL, type DiagnosticsBundle, type DiagnosticsBundleItem } from "../../diagnosticsApi";
import { HelpTip } from "../../ui-kit/HelpTip";

const { Paragraph } = Typography;

// The windows offered besides a custom range, in hours.
const WINDOW_HOURS = [1, 6, 24];

function when(iso?: string): string {
  return iso ? dayjs(iso).format("YYYY-MM-DD HH:mm:ss") : "—";
}

function stateColor(state: string) {
  return state === "ready" || state === "collected" ? "success" : state === "running" ? "processing" : state === "failed" ? "error" : "default";
}

/**
 * DiagnosticsBundleCard collects the installation's logs, debug sessions and
 * the related records into one archive to download and send to support.
 */
export default function DiagnosticsBundleCard() {
  const { t } = useTranslation("settings");
  const { t: tCommon } = useTranslation("common");
  const isAdmin = useIsAdmin();
  const bundles = usePolling(() => api.diagnosticsBundles(), 5000);
  const deployments = usePolling(() => api.deployments(), 60000);
  const nodes = usePolling(() => api.nodes(), 60000);
  const [windowChoice, setWindowChoice] = useState<string>("1");
  const [range, setRange] = useState<[Dayjs, Dayjs] | null>(null);
  const [deploymentIDs, setDeploymentIDs] = useState<number[]>([]);
  const [nodeUUIDs, setNodeUUIDs] = useState<string[]>([]);
  const [doctor, setDoctor] = useState(true);
  const [busy, setBusy] = useState(false);

  const limits = bundles.data?.limits;
  const running = (bundles.data?.bundles ?? []).some((b) => b.state === "running");

  const start = async () => {
    let from: string | undefined;
    let to: string | undefined;
    if (windowChoice === "custom") {
      if (!range) {
        message.error(t("diagnostics.bundle.rangeRequired"));
        return;
      }
      [from, to] = [range[0].toISOString(), range[1].toISOString()];
    } else {
      const end = dayjs();
      [from, to] = [end.subtract(Number(windowChoice), "hour").toISOString(), end.toISOString()];
    }
    setBusy(true);
    try {
      await api.startDiagnosticsBundle({ from: from.replace(/\.\d+Z$/, "Z"), to: to.replace(/\.\d+Z$/, "Z"), deployment_ids: deploymentIDs, node_uuids: nodeUUIDs, doctor });
      message.success(t("diagnostics.bundle.started"));
      await bundles.refresh();
    } catch (error) {
      message.error(apiErrorText(tCommon, error));
    } finally {
      setBusy(false);
    }
  };

  const itemReason = (item: DiagnosticsBundleItem) =>
    item.reason ? t(`diagnostics.bundle.itemReason.${item.reason}`, { defaultValue: item.reason }) : item.truncated ? t("diagnostics.bundle.truncated") : "";

  return (
    <Card title={t("diagnostics.bundle.title")}>
      <Paragraph type="secondary">{t("diagnostics.bundle.description", { days: limits?.keep_days ?? 7 })} <HelpTip content={t("diagnostics.bundle.descriptionHelp")} /></Paragraph>
      {bundles.error && <Alert type="error" showIcon message={apiErrorText(tCommon, bundles.error)} style={{ marginBottom: 12 }} />}
      <Form layout="vertical" disabled={!isAdmin}>
        <Space wrap size={16} align="start">
          <Form.Item label={t("diagnostics.bundle.window")}>
            <Select
              aria-label={t("diagnostics.bundle.window")}
              style={{ width: 180 }}
              value={windowChoice}
              onChange={setWindowChoice}
              options={[
                ...WINDOW_HOURS.map((h) => ({ value: String(h), label: t("diagnostics.bundle.lastHours", { count: h }) })),
                { value: "custom", label: t("diagnostics.bundle.custom") },
              ]}
            />
          </Form.Item>
          {windowChoice === "custom" && (
            <Form.Item label={t("diagnostics.bundle.range")}>
              <DatePicker.RangePicker showTime value={range} onChange={(v) => setRange(v && v[0] && v[1] ? [v[0], v[1]] : null)} />
            </Form.Item>
          )}
          <Form.Item label={t("diagnostics.bundle.doctor")} extra={t("diagnostics.bundle.doctorHelp")}>
            <Switch aria-label={t("diagnostics.bundle.doctor")} checked={doctor} onChange={setDoctor} />
          </Form.Item>
        </Space>
        <Form.Item label={t("diagnostics.bundle.deployments")} extra={t("diagnostics.bundle.deploymentsHelp")}>
          <Select
            mode="multiple"
            aria-label={t("diagnostics.bundle.deployments")}
            value={deploymentIDs}
            onChange={setDeploymentIDs}
            optionFilterProp="label"
            options={(deployments.data?.items ?? []).map((d) => ({ value: d.id, label: d.name }))}
          />
        </Form.Item>
        <Form.Item label={t("diagnostics.bundle.nodes")} extra={t("diagnostics.bundle.nodesHelp", { max: limits?.max_nodes ?? 64 })}>
          <Select
            mode="multiple"
            aria-label={t("diagnostics.bundle.nodes")}
            value={nodeUUIDs}
            onChange={setNodeUUIDs}
            optionFilterProp="label"
            options={(nodes.data?.items ?? []).map((n) => ({ value: n.uuid, label: nodeDisplayName(n) }))}
          />
        </Form.Item>
        <Popconfirm title={t("diagnostics.bundle.startConfirm")} onConfirm={start} disabled={!isAdmin || running}>
          <Button type="primary" loading={busy || running}>{running ? t("diagnostics.bundle.running") : t("diagnostics.bundle.start")}</Button>
        </Popconfirm>
      </Form>
      <Table<DiagnosticsBundle>
        style={{ marginTop: 16 }}
        size="small"
        rowKey="id"
        loading={bundles.loading && !bundles.data}
        dataSource={bundles.data?.bundles ?? []}
        locale={{ emptyText: t("diagnostics.bundle.empty") }}
        pagination={false}
        expandable={{
          expandedRowRender: (b) => (
            <Table<DiagnosticsBundleItem>
              size="small"
              rowKey="name"
              pagination={false}
              dataSource={b.items}
              columns={[
                { title: t("diagnostics.bundle.item"), dataIndex: "name" },
                { title: t("diagnostics.bundle.state"), render: (_, it) => <Tag color={stateColor(it.state)}>{t(`diagnostics.bundle.itemState.${it.state}`)}</Tag> },
                { title: t("diagnostics.bundle.size"), render: (_, it) => formatBytes(it.bytes) },
                { title: t("diagnostics.bundle.note"), render: (_, it) => itemReason(it) },
              ]}
            />
          ),
        }}
        columns={[
          { title: t("diagnostics.bundle.createdAt"), render: (_, b) => `${when(b.created_at)} · ${b.requested_by || "—"}` },
          { title: t("diagnostics.bundle.window"), render: (_, b) => `${when(b.from)} – ${when(b.to)}` },
          { title: t("diagnostics.bundle.state"), render: (_, b) => <Tag color={stateColor(b.state)}>{t(`diagnostics.bundle.bundleState.${b.state}`)}</Tag> },
          { title: t("diagnostics.bundle.size"), render: (_, b) => (b.state === "ready" ? formatBytes(b.size_bytes) : b.error || "—") },
          {
            title: "",
            render: (_, b) =>
              b.state === "ready" ? (
                <Button icon={<DownloadOutlined />} href={diagnosticsBundleDownloadURL(b.id)} aria-label={t("diagnostics.bundle.download")}>
                  {t("diagnostics.bundle.download")}
                </Button>
              ) : null,
          },
        ]}
      />
    </Card>
  );
}
