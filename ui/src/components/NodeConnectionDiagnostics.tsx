import { Alert, Button, Card, Descriptions, Space, Typography } from "antd";
import { useEffect, useRef, useState } from "react";
import { useTranslation } from "react-i18next";
import type { NodeDoctorReport } from "../api";
import { formatTime } from "../i18n/datetime";
import { nodeConnectionApi, type NodeConnectionCheck, type NodeConnectionDiagnostics as Report } from "../nodeConnectionApi";
import { recoveryLabel } from "../nodes/recoveryPhase";
import { recoveryErrorText } from "../nodes/recoveryError";
import { useIsAdmin } from "./useIsAdmin";

const outcomes = new Set(["runtime_snapshot", "offline", "unsupported", "credentials_unavailable", "credential_error", "unavailable", "not_checked", "not_applicable", "invalid_address", "unverified_identity", "ssh_failed", "cancelled", "timeout", "connected", "command_failed", "invalid_response", "snapshot", "tool_missing", "http_error", "http_ok", "dns", "connect", "tls", "transport"]);
const exposureStates = new Set(["ready", "starting", "awaiting_route", "degraded", "disabled", "failed"]);
const guidance = new Set(["snapshot_scope", "http_scope", "test_from_node", "manual_address_update", "addresses_differ", "configuration_differs", "server_dial_scope"]);
const checkStages = new Set(["agent_control", "ssh", "configuration", "node_http"]);
const reasons = new Set(["bad_token", "proto_mismatch", "stale_session", "dns", "connect", "tls", "ws_upgrade", "timeout", "cancelled", "transport", "invalid_configuration", "none"]);
const rejectReasons = new Set(["bad_token", "proto_mismatch", "stale_session"]);
const runtimeFields = ["source", "checked_at", "server_url", "connect_mode", "server_source", "token_present", "control", "data_lane", "outcome", "reason", "retry_at", "proxy", "proxy_url"] as const;
const runtimeEnums: Record<string, ReadonlySet<string>> = {
  source: new Set(["agent_runtime"]),
  connect_mode: new Set(["agent_dial", "server_dial"]),
  server_source: new Set(["flag", "environment", "state_file", "missing", "agent_runtime", "unknown"]),
  token_present: new Set(["true", "false"]),
  control: new Set(["probing", "connected", "not_connected", "smoke_verified", "not_applicable", "unknown"]),
  data_lane: new Set(["unverified", "attached", "detached", "disabled", "not_negotiated", "pending", "not_applicable", "unknown"]),
  outcome: new Set(["ok", "failed", "invalid_configuration", "not_applicable", "probing", "unknown"]),
  reason: reasons,
  proxy: new Set(["direct", "environment_proxy", "unknown"]),
};
const runtimeStages = ["configuration", "dial", "dns", "connect", "proxy_connect", "tls", "ws_upgrade", "yamux", "hello", "wait_registered", "server_hello", "session"] as const;
const phases = new Set(["ok", "failed", "timeout", "cancelled", "in_progress", "not_observed", "not_checked", "not_applicable"]);

function fact(report: NodeDoctorReport, name: string): string {
  const matches = Array.isArray(report.facts) ? report.facts.filter((item) => item?.code === `connection.${name}`) : [];
  const value = matches.length === 1 ? matches[0].value : "";
  return typeof value === "string" && value.length <= 2048 && !/[\u0000-\u001f\u007f-\u009f]/u.test(value) ? value : "";
}

function displayURL(value: string, originOnly = false): string {
  try {
    const url = new URL(value);
    if (!["http:", "https:"].includes(url.protocol) || url.username || url.password || url.search || url.hash || /[\u0000-\u0020\u007f-\u009f]/u.test(value)) return "";
    if (originOnly && value !== url.origin) return "";
    return value;
  } catch { return ""; }
}

function displayTime(value: string): string {
  return /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d{1,9})?(?:Z|[+-]\d{2}:\d{2})$/u.test(value) && Number.isFinite(Date.parse(value)) ? formatTime(value) : "";
}

function RuntimeSnapshot({ report }: { report: NodeDoctorReport }) {
  const { t } = useTranslation("nodes");
  if (fact(report, "source") !== "agent_runtime" || !displayTime(fact(report, "checked_at"))) return null;
  const unknown = t("connectionDiagnostics.unknown");
  const text = (name: typeof runtimeFields[number]) => {
    const value = fact(report, name);
    if (runtimeEnums[name]) return runtimeEnums[name].has(value) ? t(`connectionDiagnostics.runtime.value.${name}.${value}`) : unknown;
    if (name === "checked_at" || name === "retry_at") return displayTime(value) || unknown;
    return displayURL(value, name === "proxy_url") || unknown;
  };
  return <>
    <Descriptions column={1} size="small" bordered>
      {runtimeFields.map((name) => <Descriptions.Item key={name} label={t(`connectionDiagnostics.runtime.field.${name}`)}>{text(name)}</Descriptions.Item>)}
    </Descriptions>
    {runtimeStages.map((stage) => {
      const phase = fact(report, `phase.${stage}`);
      const elapsed = fact(report, `elapsed_ms.${stage}`);
      const observed = fact(report, `observed_at.${stage}`);
      if (!phase && !elapsed && !observed) return null;
      return <Descriptions key={stage} column={1} size="small" bordered title={t(`connectionDiagnostics.runtime.stage.${stage}`)}>
        <Descriptions.Item label={t("connectionDiagnostics.result")}>{phases.has(phase) ? t(`connectionDiagnostics.runtime.phase.${phase}`) : unknown}</Descriptions.Item>
        <Descriptions.Item label={t("connectionDiagnostics.runtime.elapsed")}>{/^\d+$/u.test(elapsed) && Number(elapsed) <= 86400000 ? t("connectionDiagnostics.runtime.milliseconds", { count: Number(elapsed) }) : unknown}</Descriptions.Item>
        <Descriptions.Item label={t("connectionDiagnostics.observedAt")}>{displayTime(observed) || unknown}</Descriptions.Item>
      </Descriptions>;
    })}
  </>;
}

function ConnectionCheck({ check }: { check: NodeConnectionCheck }) {
  const { t } = useTranslation("nodes");
  if (!checkStages.has(check.stage)) return null;
  const location = ["control_plane", "node"].includes(check.vantage) ? check.vantage : "unknown";
  const source = check.source && ["state_file", "legacy_arguments"].includes(check.source) ? check.source : "";
  const status = Number.isInteger(check.http_status) && check.http_status! >= 100 && check.http_status! <= 599 ? check.http_status : undefined;
  return <Descriptions column={1} size="small" bordered title={t(`connectionDiagnostics.stage.${check.stage}`)}>
    <Descriptions.Item label={t("connectionDiagnostics.result")}>{t(`connectionDiagnostics.outcome.${outcomes.has(check.outcome) ? check.outcome : "unknown"}`)}{status ? ` · HTTP ${status}` : ""}</Descriptions.Item>
    <Descriptions.Item label={t("connectionDiagnostics.vantage")}>{t(`connectionDiagnostics.location.${location}`)}</Descriptions.Item>
    {check.stage !== "agent_control" && <Descriptions.Item label={t("connectionDiagnostics.target")}>{displayURL(check.target ?? "") || t("connectionDiagnostics.unknown")}</Descriptions.Item>}
    {source && <Descriptions.Item label={t("connectionDiagnostics.source")}>{t(`connectionDiagnostics.sourceName.${source}`)}</Descriptions.Item>}
    <Descriptions.Item label={t("connectionDiagnostics.observedAt")}>{displayTime(check.observed_at ?? "") || t("connectionDiagnostics.unknown")}</Descriptions.Item>
  </Descriptions>;
}

export default function NodeConnectionDiagnostics({ nodeUUID }: { nodeUUID: string }) {
  const { t } = useTranslation("nodes");
  const admin = useIsAdmin();
  const [report, setReport] = useState<Report | null>(null);
  const [error, setError] = useState(false);
  const [loading, setLoading] = useState(false);
  const active = useRef<AbortController | null>(null);
  const generation = useRef(0);
  const currentNode = useRef(nodeUUID);
  currentNode.current = nodeUUID;

  useEffect(() => {
    generation.current++;
    active.current?.abort();
    setReport(null);
    setError(false);
    setLoading(false);
    return () => { generation.current++; active.current?.abort(); };
  }, [nodeUUID]);

  const run = async () => {
    const current = ++generation.current;
    active.current?.abort();
    const controller = new AbortController();
    active.current = controller;
    setLoading(true);
    setError(false);
    setReport(null);
    try {
      const next = await nodeConnectionApi.diagnose(nodeUUID, controller.signal);
      if (current === generation.current && currentNode.current === nodeUUID) {
        if (next.node_uuid !== nodeUUID) throw new Error("mismatched diagnostic node");
        setReport(next);
      }
    } catch {
      if (current === generation.current && currentNode.current === nodeUUID) setError(true);
    } finally {
      if (current === generation.current && currentNode.current === nodeUUID) setLoading(false);
    }
  };

  if (!admin) return null;
  return <Card size="small" title={t("connectionDiagnostics.title")} extra={<Button aria-label={t("connectionDiagnostics.run")} loading={loading} onClick={run}>{t("connectionDiagnostics.run")}</Button>}>
    <Space direction="vertical" size={12} style={{ width: "100%", overflowWrap: "anywhere" }}>
      <Typography.Text type="secondary">{t("connectionDiagnostics.scope")}</Typography.Text>
      {!report && !loading && !error && <Typography.Text>{t("connectionDiagnostics.empty")}</Typography.Text>}
      {loading && <Typography.Text role="status">{t("connectionDiagnostics.loading")}</Typography.Text>}
      {error && <Alert type="error" showIcon message={t("connectionDiagnostics.failed")} />}
      {report && <>
        <section aria-label={t("connectionDiagnostics.currentTitle")} style={{ width: "100%" }}>
          <Typography.Title level={5}>{t("connectionDiagnostics.currentTitle")}</Typography.Title>
          <Space direction="vertical" size={12} style={{ width: "100%" }}>
            <Descriptions column={1} size="small" bordered><Descriptions.Item label={t("connectionDiagnostics.checkedAt")}>{displayTime(report.checked_at) || t("connectionDiagnostics.unknown")}</Descriptions.Item></Descriptions>
            {report.checks.filter((check) => check.stage === "agent_control").map((check) => <ConnectionCheck key={check.stage} check={check} />)}
            {report.agent_report && <RuntimeSnapshot report={report.agent_report} />}
            <Typography.Text type="secondary">{t("connectionDiagnostics.runtime.scope")}</Typography.Text>
          </Space>
        </section>
        <section aria-label={t("connectionDiagnostics.historyTitle")} style={{ width: "100%" }}>
          <Typography.Title level={5}>{t("connectionDiagnostics.historyTitle")}</Typography.Title>
          <Descriptions column={1} size="small" bordered>
            <Descriptions.Item label={t("connectionDiagnostics.savedAddress")}>{displayURL(report.saved_server_url) || t("connectionDiagnostics.unknown")}</Descriptions.Item>
            <Descriptions.Item label={t("connectionDiagnostics.lastSeen")}>{report.last_seen_at ? formatTime(report.last_seen_at) : t("connectionDiagnostics.unknown")}</Descriptions.Item>
            <Descriptions.Item label={t("connectionDiagnostics.recoveryPhase")}>{recoveryLabel[report.recovery_phase] ? t(recoveryLabel[report.recovery_phase].key) : t("connectionDiagnostics.unknown")}</Descriptions.Item>
            <Descriptions.Item label={t("connectionDiagnostics.lastRecovery")}>{report.last_probe_at ? formatTime(report.last_probe_at) : t("connectionDiagnostics.unknown")} · {t("connectionDiagnostics.attempts", { count: report.recovery_attempts })}</Descriptions.Item>
            {report.last_recovery_error && <Descriptions.Item label={t("connectionDiagnostics.recoveryError")}><span style={{ whiteSpace: "pre-line" }}>{recoveryErrorText(t, report.last_recovery_code, report.last_recovery_error)}</span></Descriptions.Item>}
            {report.last_reject_reason && rejectReasons.has(report.last_reject_reason) && <Descriptions.Item label={t("connectionDiagnostics.lastReject")}>{t(`connectionDiagnostics.runtime.value.reason.${report.last_reject_reason}`)}{report.last_reject_at ? ` · ${displayTime(report.last_reject_at)}` : ""}</Descriptions.Item>}
            <Descriptions.Item label={t("connectionDiagnostics.exposure")}>
              {report.exposure ? <Space direction="vertical" size={0}><span>{displayURL(report.exposure.url) || t("connectionDiagnostics.unknown")}</span><span>{t(`connectionDiagnostics.exposureState.${exposureStates.has(report.exposure.state) ? report.exposure.state : "unknown"}`)}</span><Typography.Text type="secondary">{t("connectionDiagnostics.lastObserved", { time: displayTime(report.exposure.observed_at) || t("connectionDiagnostics.unknown") })}</Typography.Text></Space> : t("connectionDiagnostics.unknown")}
            </Descriptions.Item>
          </Descriptions>
        </section>
        <section aria-label={t("connectionDiagnostics.sshTitle")} style={{ width: "100%" }}>
          <Typography.Title level={5}>{t("connectionDiagnostics.sshTitle")}</Typography.Title>
          <Space direction="vertical" size={12} style={{ width: "100%" }}>{report.checks.filter((check) => check.stage !== "agent_control").map((check) => <ConnectionCheck key={check.stage} check={check} />)}</Space>
        </section>
        <section aria-label={t("connectionDiagnostics.cliTitle")} style={{ width: "100%" }}>
          <Typography.Title level={5}>{t("connectionDiagnostics.cliTitle")}</Typography.Title>
          <Typography.Paragraph>{t("connectionDiagnostics.cliScope")}</Typography.Paragraph>
          <Typography.Paragraph code style={{ whiteSpace: "pre-wrap", overflowWrap: "anywhere" }}>{t("connectionDiagnostics.cliCommand")}</Typography.Paragraph>
        </section>
        {report.guidance.filter((code) => guidance.has(code)).map((code) => <Alert key={code} type="info" showIcon message={t(`connectionDiagnostics.guidance.${code}`)} />)}
      </>}
    </Space>
  </Card>;
}
