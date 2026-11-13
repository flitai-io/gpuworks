import { Alert, Button, Descriptions, Drawer, Tag, Typography } from "antd";
import { ExportOutlined } from "@ant-design/icons";
import dayjs from "dayjs";
import { useTranslation } from "react-i18next";
import { Link } from "react-router-dom";
import { traceLinkFor, type MonitorLookupDeployment, type MonitorRequest } from "../../monitorRequestsApi";
import type { MonitorRequestPD } from "../../monitorRequestsApi";
import { callerName, replicaText, RequestDetailLines } from "./RequestRow";
import { formatMS } from "./requestShape";
import { HelpTip } from "../../ui-kit/HelpTip";
import { useTraceLink } from "./useTraceLink";

const { Text } = Typography;

// RequestDetailDrawer shows one request: its request id and trace id (to
// copy, and to open in the tracing backend when a trace link is set), who
// sent it, where it went and the times behind its row. deployment, when
// given, names the deployment and links to its service page, a deleted one
// marked so (a lookup spans deployments; a service page's own list does not
// need it).
export default function RequestDetailDrawer({
  request: r,
  replica,
  deployment,
  onClose,
}: {
  request: MonitorRequest | null;
  replica?: { n: number; node: string };
  deployment?: MonitorLookupDeployment;
  onClose: () => void;
}) {
  const { t } = useTranslation("monitoring");
  const template = useTraceLink(r != null);
  const link = r ? traceLinkFor(template, r.trace_id) : null;
  return (
    <Drawer open={r != null} onClose={onClose} title={t("requests.drawer.title")} width={520} destroyOnHidden>
      {r && (
        <>
          <Descriptions size="small" column={1} bordered labelStyle={{ width: 104, whiteSpace: "nowrap" }}>
            <Descriptions.Item label={t("requests.drawer.requestId")}>
              <Text code copyable={{ text: r.id }}>{r.id}</Text>
            </Descriptions.Item>
            <Descriptions.Item label={t("requests.drawer.traceId")}>
              {r.trace_id ? (
                <span style={{ display: "inline-flex", gap: 8, alignItems: "center", flexWrap: "wrap" }}>
                  <Text code copyable={{ text: r.trace_id }}>{r.trace_id}</Text>
                  {link && (
                    <Button size="small" type="link" icon={<ExportOutlined />} href={link} target="_blank" rel="noopener noreferrer">
                      {t("requests.drawer.openTrace")}
                    </Button>
                  )}
                </span>
              ) : (
                <Text type="secondary">{t("requests.drawer.noTrace")}</Text>
              )}
            </Descriptions.Item>
            {deployment !== undefined && (
              <Descriptions.Item label={t("requests.drawer.deployment")}>
                {deployment.name ? (
                  <>
                    <Link to={`/monitor/d/${r.deployment_id}`}>{deployment.name}</Link>
                    {deployment.deleted && <Tag style={{ marginLeft: 8 }}>{t("requests.drawer.deleted")}</Tag>}
                  </>
                ) : (
                  t("requests.drawer.deletedDeployment", { id: r.deployment_id })
                )}
              </Descriptions.Item>
            )}
            <Descriptions.Item label={t("requests.drawer.caller")}>{callerName(r, t)}</Descriptions.Item>
            <Descriptions.Item label={t("requests.drawer.received")}>{dayjs(r.received_ms).format("YYYY-MM-DD HH:mm:ss.SSS")}</Descriptions.Item>
            <Descriptions.Item label={r.pd ? t("requests.drawer.decodeReplica") : t("requests.drawer.replica")}>{replicaText(r, replica, t) || "—"}</Descriptions.Item>
          </Descriptions>
          {r.pd && <PDSection pd={r.pd} queueMS={r.engine_queue_ms} inStage={r.phase !== "done" && r.dispatch_ms == null} />}
          <Alert
            style={{ marginTop: 16 }}
            type="info"
            showIcon
            message={<span className="inline-flex items-center gap-1">{t("requests.drawer.observationTitle")}<HelpTip content={t("requests.drawer.observationScopeHelp")} /></span>}
            description={<>
              <Typography.Paragraph>{t("requests.drawer.streamingEvidence")}</Typography.Paragraph>
              <Link to="/settings#diagnostics">{t("requests.drawer.collectEvidence")}</Link>
            </>}
          />
          <div style={{ marginTop: 16 }}>
            <RequestDetailLines r={r} replica="" />
          </div>
        </>
      )}
    </Drawer>
  );
}

// PDSection is a PD request's prefill stage: which prefill instance took it,
// how long the gateway waited for it and what that instance said of it, the
// decode instance's wait for the KV, and a fallback when there was one.
function PDSection({ pd, queueMS, inStage }: { pd: MonitorRequestPD; queueMS: number | null; inStage: boolean }) {
  const { t } = useTranslation("monitoring");
  const time = (v: number | null) => (v == null ? "—" : formatMS(v));
  return (
    <Descriptions size="small" column={1} bordered title={t("requests.drawer.pd")} style={{ marginTop: 16 }} labelStyle={{ width: 140, whiteSpace: "nowrap" }}>
      <Descriptions.Item label={t("requests.drawer.pdInstance")}>
        {pd.prefill_instance_uuid ? <Text code>{pd.prefill_instance_uuid.slice(0, 8)}</Text> : "—"}
      </Descriptions.Item>
      <Descriptions.Item label={t("requests.drawer.pdStage")}>{inStage ? t("requests.drawer.pdInStage") : time(pd.prefill_ms)}</Descriptions.Item>
      <Descriptions.Item label={t("requests.drawer.pdQueue")}>{time(pd.prefill_queue_ms)}</Descriptions.Item>
      <Descriptions.Item label={t("requests.drawer.pdCompute")}>{time(pd.prefill_compute_ms)}</Descriptions.Item>
      <Descriptions.Item label={t("requests.drawer.pdKV")}>{time(queueMS)}</Descriptions.Item>
      {pd.fallback && <Descriptions.Item label={t("requests.drawer.pdFallback")}>{t(`requests.pdFallback.${pd.fallback}`, { defaultValue: pd.fallback })}</Descriptions.Item>}
    </Descriptions>
  );
}
