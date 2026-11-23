import SettingsSaveButton from "./SettingsSaveButton";
import { useCallback, useEffect, useMemo, useState } from "react";
import { Alert, Button, Card, Form, InputNumber, Select, Skeleton, Space, Switch, Tag, Typography, message } from "antd";
import { useTranslation } from "react-i18next";
import { api, type DataRetention, type DataRetentionStatus, type DataRetentionStore, type DataRetentionView } from "../../api";
import { apiErrorText } from "../../apiError";
import { formatBytes } from "../../format";
import { formatTime } from "../../i18n/datetime";

const { Text, Paragraph } = Typography;

export const DATA_RETENTION_ANCHOR = "data-retention";

type NumericField = Exclude<keyof DataRetention, "quality" | "gpu" | "gpu_time" | "time_zone">;

/** The groups the page shows, each with the numeric values it edits and the stores it reports. */
const GROUPS: { key: string; fields: NumericField[]; stores: DataRetentionStore["kind"][] }[] = [
  { key: "metrics", fields: ["metrics_hours"], stores: ["metrics"] },
  { key: "logs", fields: ["logs_hours"], stores: ["logs"] },
  { key: "requests", fields: ["request_log_days", "request_log_max_mb"], stores: ["requests"] },
  { key: "usage", fields: ["usage_hourly_days", "usage_daily_days"], stores: ["usage"] },
  { key: "stats", fields: ["stats_hourly_days", "stats_daily_days"], stores: ["quality", "gpu", "gpu_time", "scheduling"] },
];

const SWITCHES: { field: "quality" | "gpu" | "gpu_time"; store: DataRetentionStore["kind"] }[] = [
  { field: "quality", store: "quality" },
  { field: "gpu", store: "gpu" },
  { field: "gpu_time", store: "gpu_time" },
];

function unitOf(field: NumericField): "hours" | "days" | "mb" {
  if (field.endsWith("_hours")) return "hours";
  if (field.endsWith("_mb")) return "mb";
  return "days";
}

/** shortened lists the values a draft keeps for less time (or less space) than the saved setting. */
export function shortened(saved: DataRetention, draft: DataRetention): NumericField[] {
  return (Object.keys(saved) as (keyof DataRetention)[]).filter(
    (k): k is NumericField => typeof saved[k] === "number" && (draft[k] as number) < (saved[k] as number),
  );
}

function timeZones(current: string[]): string[] {
  const all = (Intl as unknown as { supportedValuesOf?: (key: string) => string[] }).supportedValuesOf?.("timeZone") ?? [];
  return Array.from(new Set([...current.filter(Boolean), "UTC", ...all]));
}

// DataRetentionCard edits how long every kind of data is kept, and shows
// what each store holds now. The PUT replaces the whole setting; the server
// applies it at once and clears what a shorter retention no longer keeps in
// the background.
export default function DataRetentionCard() {
  const { t } = useTranslation("settings");
  const { t: tCommon } = useTranslation("common");
  const [view, setView] = useState<DataRetentionView | null>(null);
  const [draft, setDraft] = useState<DataRetention | null>(null);
  const [status, setStatus] = useState<DataRetentionStatus | null>(null);
  const [loadError, setLoadError] = useState("");
  const [saving, setSaving] = useState(false);

  const loadStatus = useCallback(() => {
    api.dataRetentionStatus().then(setStatus).catch(() => setStatus(null));
  }, []);
  useEffect(() => {
    api.dataRetention()
      .then((v) => {
        setView(v);
        setDraft(v.value);
        setLoadError("");
      })
      .catch((error) => setLoadError(apiErrorText(tCommon, error)));
    loadStatus();
  }, [tCommon, loadStatus]);

  const zones = useMemo(() => timeZones([view?.value.time_zone ?? "", view?.server_time_zone ?? ""]), [view]);
  const invalid = useMemo(() => {
    if (!draft || !view) return [] as NumericField[];
    return GROUPS.flatMap((g) => g.fields).filter((f) => {
      const limit = view.limits[f];
      const value = draft[f];
      return limit == null || value == null || !Number.isInteger(value) || value < limit.min || value > limit.max;
    });
  }, [draft, view]);
  const dirty = !!view && !!draft && JSON.stringify(view.value) !== JSON.stringify(draft);

  const store = (kind: DataRetentionStore["kind"]) => status?.stores.find((s) => s.kind === kind);

  const submit = async (value: DataRetention) => {
    setSaving(true);
    try {
      const next = await api.putDataRetention(value);
      setView(next);
      setDraft(next.value);
      message.success(t("dataRetention.saved"));
      loadStatus();
    } catch (error) {
      message.error(t("error.save", { detail: apiErrorText(tCommon, error) }));
    } finally {
      setSaving(false);
    }
  };
  const less = view && draft ? shortened(view.value, draft) : [];

  const field = (f: NumericField) => {
    const limit = view?.limits[f];
    const bad = invalid.includes(f);
    return (
      <Form.Item
        key={f}
        label={t(`dataRetention.fields.${f}`)}
        htmlFor={`retention-${f}`}
        validateStatus={bad ? "error" : undefined}
        extra={limit ? t(`dataRetention.range.${unitOf(f)}`, { min: limit.min, max: limit.max }) : undefined}
        style={{ marginBottom: 12, minWidth: 220 }}
      >
        <InputNumber
          id={`retention-${f}`}
          min={limit?.min}
          max={limit?.max}
          precision={0}
          value={draft?.[f]}
          onChange={(v) => draft && setDraft({ ...draft, [f]: v == null ? (null as unknown as number) : Number(v) })}
          suffix={t(`dataRetention.unit.${unitOf(f)}`)}
          style={{ width: 180 }}
        />
      </Form.Item>
    );
  };

  const holding = (s?: DataRetentionStore) => {
    if (!s) return null;
    const parts: string[] = [];
    if (s.file_bytes != null) parts.push(t("dataRetention.fileSize", { file: s.file, size: formatBytes(s.file_bytes) }));
    if (s.hourly_rows != null) parts.push(t("dataRetention.rows", { hourly: s.hourly_rows.toLocaleString(), daily: (s.daily_rows ?? 0).toLocaleString() }));
    if (s.earliest_ms) parts.push(t("dataRetention.earliest", { time: formatTime(new Date(s.earliest_ms).toISOString()) }));
    return <Text type="secondary" style={{ fontSize: 12 }}>{parts.join(" · ")}</Text>;
  };

  return (
    <Card
      id={DATA_RETENTION_ANCHOR}
      title={t("dataRetention.title")}
      extra={
        <Space>
          <Button size="small" disabled={!dirty || saving} onClick={() => view && setDraft(view.value)}>{t("dataRetention.reset")}</Button>
          <SettingsSaveButton loading={saving} disabled={!dirty || invalid.length > 0}
            onSave={() => { if (draft) return submit(draft); }}
            title={less.length ? t("dataRetention.shrinkTitle") : undefined}
            destructive={less.length > 0}
            description={less.length ? t("dataRetention.shrinkBody", { fields: less.map((f) => t(`dataRetention.fields.${f}`)).join(t("dataRetention.listSeparator")) }) : undefined}
            confirmText={less.length ? t("dataRetention.shrinkOk") : undefined}
          />
        </Space>
      }
    >
      <Space direction="vertical" size={12} style={{ width: "100%", maxWidth: 900 }}>
        <Text type="secondary">{t("dataRetention.description")}</Text>
        {loadError && <Alert type="error" showIcon message={loadError} />}
        {!draft && !loadError && <Skeleton active paragraph={{ rows: 6 }} />}
        {draft && (
          <>
            <Alert type="info" showIcon message={t("dataRetention.configNote")} />
            {GROUPS.map((g) => (
              <Card key={g.key} size="small" type="inner" title={
                <Space size={8}>
                  {t(`dataRetention.groups.${g.key}.title`)}
                  {g.key === "usage" && <Tag color="blue">{t("dataRetention.billing")}</Tag>}
                </Space>
              }>
                <Paragraph type="secondary" style={{ marginBottom: 8 }}>{t(`dataRetention.groups.${g.key}.description`)}</Paragraph>
                <Form layout="vertical">
                  <Space size={24} wrap align="start">{g.fields.map(field)}</Space>
                </Form>
                {g.key === "stats" ? (
                  <Space direction="vertical" size={8} style={{ width: "100%" }}>
                    {SWITCHES.map(({ field: f, store: kind }) => (
                      <Space key={f} size={12} wrap>
                        <Switch
                          size="small"
                          checked={draft[f]}
                          onChange={(on) => setDraft({ ...draft, [f]: on })}
                          aria-label={t(`dataRetention.switches.${f}`)}
                        />
                        <Text>{t(`dataRetention.switches.${f}`)}</Text>
                        {holding(store(kind))}
                      </Space>
                    ))}
                    {/* The scheduling activity is always counted: no switch. */}
                    <Space size={12} wrap>
                      <Text>{t("dataRetention.alwaysOn.scheduling")}</Text>
                      {holding(store("scheduling"))}
                    </Space>
                  </Space>
                ) : (
                  g.stores.map((kind) => <div key={kind}>{holding(store(kind))}</div>)
                )}
                {g.key === "requests" && <Text type="secondary" style={{ display: "block", fontSize: 12, marginTop: 4 }}>{t("dataRetention.requestsNote")}</Text>}
              </Card>
            ))}
            <Card size="small" type="inner" title={t("dataRetention.groups.zone.title")}>
              <Paragraph type="secondary" style={{ marginBottom: 8 }}>{t("dataRetention.groups.zone.description")}</Paragraph>
              <Select
                showSearch
                aria-label={t("dataRetention.groups.zone.title")}
                value={draft.time_zone}
                onChange={(zone) => setDraft({ ...draft, time_zone: zone })}
                options={zones.map((z) => ({ value: z, label: z === view?.server_time_zone ? t("dataRetention.serverZone", { zone: z }) : z }))}
                style={{ width: 320, maxWidth: "100%" }}
              />
            </Card>
            {status && (status.retain.error || status.rollup.error || status.usage_flush.error) && (
              <Alert type="warning" showIcon message={t("dataRetention.passFailed")} description={[status.retain.error, status.rollup.error, status.usage_flush.error].filter(Boolean).join("; ")} />
            )}
            {status && status.retain.at_ms > 0 && (
              <Text type="secondary" style={{ fontSize: 12 }}>{t("dataRetention.lastRetain", { time: formatTime(new Date(status.retain.at_ms).toISOString()) })}</Text>
            )}
          </>
        )}
      </Space>
    </Card>
  );
}
