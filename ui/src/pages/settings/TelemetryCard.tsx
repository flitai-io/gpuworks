import { useCallback, useEffect, useState } from "react";
import { Alert, Button, Card, Descriptions, Form, Input, InputNumber, Space, Switch, Tag, Typography, message } from "antd";
import { DeleteOutlined, PlusOutlined } from "@ant-design/icons";
import dayjs from "dayjs";
import { useTranslation } from "react-i18next";
import { api } from "../../api";
import { apiErrorText } from "../../apiError";
import { useIsAdmin } from "../../components/useIsAdmin";
import { OutboundHostAccess, useOutboundHostAccess } from "../../components/OutboundHostAccess";
import { resetTraceLinkCache } from "../monitor/useTraceLink";
import type { TelemetryExport, TelemetryExportTest, TelemetryExportUpdate } from "../../telemetryApi";

const { Text, Paragraph } = Typography;

/** TELEMETRY_ANCHOR is the fragment links to this card use (/settings#telemetry). */
export const TELEMETRY_ANCHOR = "telemetry";

// A header row being edited. A stored header shows no value: left empty it
// keeps the stored one (sent as null).
interface HeaderRow {
  key: number;
  name: string;
  value: string;
  stored: boolean;
}

interface Draft {
  enabled: boolean;
  endpoint: string;
  headers: HeaderRow[];
  // Percent, 0..100.
  percent: number;
  traceLink: string;
}

let nextKey = 1;

function draftOf(exp: TelemetryExport): Draft {
  return {
    enabled: exp.enabled,
    endpoint: exp.endpoint,
    headers: exp.headers.map((h) => ({ key: nextKey++, name: h.name, value: "", stored: true })),
    percent: Math.round(exp.sample_ratio * 10000) / 100,
    traceLink: exp.trace_link,
  };
}

function updateOf(d: Draft): TelemetryExportUpdate {
  return {
    enabled: d.enabled,
    endpoint: d.endpoint.trim(),
    headers: d.headers
      .filter((h) => h.name.trim() !== "")
      .map((h) => ({ name: h.name.trim(), value: h.stored && h.value === "" ? null : h.value })),
    sample_ratio: d.percent / 100,
    trace_link: d.traceLink.trim(),
  };
}

function when(ms: number | null): string {
  return ms == null ? "—" : dayjs(ms).format("YYYY-MM-DD HH:mm:ss");
}

/**
 * TelemetryCard configures where the gateway's request traces go (an
 * OpenTelemetry collector over OTLP/HTTP), sends a test span with the draft,
 * and shows what the running export did.
 */
export default function TelemetryCard() {
  const { t } = useTranslation("settings");
  const { t: tCommon } = useTranslation("common");
  const isAdmin = useIsAdmin();
  const [current, setCurrent] = useState<TelemetryExport>();
  const [draft, setDraft] = useState<Draft>();
  const outboundAccess = useOutboundHostAccess([draft?.endpoint ?? ""], isAdmin && !!draft);
  const [loadError, setLoadError] = useState("");
  const [saving, setSaving] = useState(false);
  const [testing, setTesting] = useState(false);
  const [test, setTest] = useState<TelemetryExportTest | { error: string; ok: false; trace_id: ""; traces_url: "" }>();

  const load = useCallback(
    () =>
      api
        .telemetryExport()
        .then((exp) => {
          setCurrent(exp);
          setDraft(draftOf(exp));
          setLoadError("");
        })
        .catch((error) => setLoadError(apiErrorText(tCommon, error))),
    [tCommon],
  );
  useEffect(() => {
    void load();
  }, [load]);

  const edit = (change: Partial<Draft>) => setDraft((d) => (d ? { ...d, ...change } : d));
  const editHeader = (key: number, change: Partial<HeaderRow>) =>
    setDraft((d) => (d ? { ...d, headers: d.headers.map((h) => (h.key === key ? { ...h, ...change } : h)) } : d));

  const save = async () => {
    if (!draft) return;
    setSaving(true);
    try {
      await outboundAccess.authorize();
      const saved = await api.updateTelemetryExport(updateOf(draft));
      setCurrent(saved);
      setDraft(draftOf(saved));
      resetTraceLinkCache(); // request details read the new trace link
      message.success(t("telemetry.saved"));
    } catch (error) {
      message.error(t("error.save", { detail: apiErrorText(tCommon, error) }));
    } finally {
      setSaving(false);
    }
  };

  const runTest = async () => {
    if (!draft) return;
    setTesting(true);
    setTest(undefined);
    try {
      await outboundAccess.authorize();
      setTest(await api.testTelemetryExport(updateOf(draft)));
    } catch (error) {
      setTest({ ok: false, error: apiErrorText(tCommon, error), trace_id: "", traces_url: "" });
    } finally {
      setTesting(false);
    }
  };

  const status = current?.status;
  const editable = isAdmin && !!draft;
  let state = <Tag>{t("telemetry.state.off")}</Tag>;
  if (status?.setting_error) state = <Tag color="error">{t("telemetry.state.broken")}</Tag>;
  else if (status?.active && status.last_error && (status.last_export_at ?? 0) < (status.last_error_at ?? 0)) state = <Tag color="warning">{t("telemetry.state.failing")}</Tag>;
  else if (status?.active) state = <Tag color="success">{t("telemetry.state.on")}</Tag>;

  return (
    <div id={TELEMETRY_ANCHOR}>
      <Card
        title={t("telemetry.title")}
        extra={
          <Space>
            {current && state}
            <Button type="primary" loading={saving} disabled={!editable} onClick={() => void save()}>
              {t("common.save")}
            </Button>
          </Space>
        }
      >
        <Space direction="vertical" size={8} style={{ width: "100%", maxWidth: 820 }}>
          <Text type="secondary">{t("telemetry.description")}</Text>
          {loadError && (
            <Alert type="error" showIcon message={loadError} action={<Button size="small" onClick={() => void load()}>{tCommon("actions.retry")}</Button>} />
          )}
          {!isAdmin && <Alert type="info" showIcon message={t("telemetry.adminOnly")} />}
          {status?.setting_error && <Alert type="error" showIcon message={t("telemetry.settingError")} description={status.setting_error} />}
          {draft && (
            <Form layout="vertical" style={{ marginTop: 8 }}>
              <Form.Item label={t("telemetry.enabled")} style={{ marginBottom: 12 }}>
                <Switch checked={draft.enabled} disabled={!editable} onChange={(enabled) => edit({ enabled })} aria-label={t("telemetry.enabled")} />
              </Form.Item>
              <Form.Item label={t("telemetry.endpoint")} extra={t("telemetry.endpointHelp")} style={{ marginBottom: 12 }}>
                <Input
                  value={draft.endpoint}
                  disabled={!editable}
                  placeholder="http://otel-collector:4318"
                  aria-label={t("telemetry.endpoint")}
                  onChange={(e) => edit({ endpoint: e.target.value })}
                />
              </Form.Item>
              <OutboundHostAccess access={outboundAccess} />
              <Form.Item label={t("telemetry.headers")} extra={t("telemetry.headersHelp")} style={{ marginBottom: 12 }}>
                <Space direction="vertical" size={6} style={{ width: "100%" }}>
                  {draft.headers.map((h, i) => (
                    <Space.Compact key={h.key} style={{ width: "100%" }}>
                      <Input
                        value={h.name}
                        disabled={!editable}
                        placeholder="Authorization"
                        aria-label={t("telemetry.headerName", { n: i + 1 })}
                        style={{ width: "35%" }}
                        onChange={(e) => editHeader(h.key, { name: e.target.value })}
                      />
                      <Input.Password
                        value={h.value}
                        disabled={!editable}
                        placeholder={h.stored ? t("telemetry.headerKept") : t("telemetry.headerValue")}
                        aria-label={t("telemetry.headerValueOf", { n: i + 1 })}
                        visibilityToggle={!h.stored || h.value !== ""}
                        onChange={(e) => editHeader(h.key, { value: e.target.value })}
                      />
                      <Button
                        icon={<DeleteOutlined />}
                        disabled={!editable}
                        aria-label={t("telemetry.removeHeader", { n: i + 1 })}
                        onClick={() => edit({ headers: draft.headers.filter((x) => x.key !== h.key) })}
                      />
                    </Space.Compact>
                  ))}
                  <Button
                    size="small"
                    icon={<PlusOutlined />}
                    disabled={!editable}
                    onClick={() => edit({ headers: [...draft.headers, { key: nextKey++, name: "", value: "", stored: false }] })}
                  >
                    {t("telemetry.addHeader")}
                  </Button>
                </Space>
              </Form.Item>
              <Form.Item label={t("telemetry.sampleRatio")} extra={t("telemetry.sampleRatioHelp")} style={{ marginBottom: 12 }}>
                <InputNumber
                  min={0}
                  max={100}
                  step={1}
                  value={draft.percent}
                  disabled={!editable}
                  suffix="%"
                  aria-label={t("telemetry.sampleRatio")}
                  style={{ width: 140 }}
                  onChange={(v) => edit({ percent: v == null ? 100 : Math.min(100, Math.max(0, v)) })}
                />
              </Form.Item>
              <Form.Item label={t("telemetry.traceLink")} extra={t("telemetry.traceLinkHelp")} style={{ marginBottom: 12 }}>
                <Input
                  value={draft.traceLink}
                  disabled={!editable}
                  placeholder="https://grafana.example.com/explore?left=...{trace_id}..."
                  aria-label={t("telemetry.traceLink")}
                  onChange={(e) => edit({ traceLink: e.target.value })}
                />
              </Form.Item>
              <Space wrap>
                <Button loading={testing} disabled={!editable || draft.endpoint.trim() === ""} onClick={() => void runTest()}>
                  {t("telemetry.test")}
                </Button>
                <Text type="secondary" style={{ fontSize: 12 }}>
                  {t("telemetry.testHelp")}
                </Text>
              </Space>
              {test && (
                <Alert
                  style={{ marginTop: 8 }}
                  type={test.ok ? "success" : "error"}
                  showIcon
                  message={test.ok ? t("telemetry.testOK") : t("telemetry.testFailed")}
                  description={
                    test.ok ? (
                      <span>
                        {t("telemetry.testTrace")} <Text code copyable>{test.trace_id}</Text>
                      </span>
                    ) : (
                      test.error
                    )
                  }
                />
              )}
            </Form>
          )}
          {status && (
            <Descriptions size="small" column={1} bordered style={{ marginTop: 8 }} title={<Text strong>{t("telemetry.status")}</Text>}>
              <Descriptions.Item label={t("telemetry.sendingTo")}>{status.active ? <Text code>{status.traces_url}</Text> : t("telemetry.notSending")}</Descriptions.Item>
              <Descriptions.Item label={t("telemetry.counts")}>
                {t("telemetry.countsText", { exported: status.exported, dropped: status.dropped, failed: status.failed, queued: status.queued })}
              </Descriptions.Item>
              <Descriptions.Item label={t("telemetry.lastExport")}>{when(status.last_export_at)}</Descriptions.Item>
              <Descriptions.Item label={t("telemetry.lastError")}>
                {status.last_error ? (
                  <Paragraph style={{ margin: 0 }}>
                    <Text type="secondary">{when(status.last_error_at)}</Text> {status.last_error}
                  </Paragraph>
                ) : (
                  "—"
                )}
              </Descriptions.Item>
            </Descriptions>
          )}
        </Space>
      </Card>
    </div>
  );
}
