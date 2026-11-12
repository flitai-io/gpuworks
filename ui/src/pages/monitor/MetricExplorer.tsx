import { useCallback, useMemo, useState, type ReactNode } from "react";
import { Collapse, Select, Space, Typography } from "antd";
import { useTranslation } from "react-i18next";
import { api, monitorSeriesURL, usePolling, type MonitorMetricSource, type MonitorRange, type MonitorSeriesParams } from "../../api";
import TimeSeriesChart from "../../components/TimeSeriesChart";
import { explorerSeriesName } from "./monitorSeries";

// The explorer lists the server's metric catalog (GET /api/v1/monitor/metrics)
// grouped by what measures each series, in this order; it spells no metric
// names itself.
const SOURCE_ORDER: MonitorMetricSource[] = ["gpu", "dgpu", "node", "network", "engine", "gateway", "kv_disk", "benchmark", "peer_transfer"];
const DEFAULT_METRIC = "gpuworks_gpu_utilization";

const BY_OPTIONS: NonNullable<MonitorSeriesParams["by"]>[] = ["node", "deployment", "instance", "gpu"];
const FN_OPTIONS: NonNullable<MonitorSeriesParams["fn"]>[] = ["avg", "sum", "max", "rate"];

// The panel starts collapsed and its body - the catalog and series requests
// with it - mounts on first expand: a long range's series is the monitor
// page's slowest read, and most visits never open the explorer.
export default function MetricExplorer({ range }: { range: MonitorRange }) {
  const { t } = useTranslation("monitoring");
  return <Collapse items={[{ key: "explorer", label: t("metricExplorer.title"), children: <ExplorerBody range={range} /> }]} />;
}

function ExplorerBody({ range }: { range: MonitorRange }) {
  const { t } = useTranslation("monitoring");
  const [metric, setMetric] = useState(DEFAULT_METRIC);
  const [by, setBy] = useState<MonitorSeriesParams["by"]>(undefined);
  const [fn, setFn] = useState<NonNullable<MonitorSeriesParams["fn"]>>("avg");
  const vocabulary = usePolling(() => api.monitorMetrics(), 0, []);
  // A histogram family's own series is its quantile gauges (one per
  // "quantile" label). /monitor/series has no per-label filter, so selecting
  // one combines the quantiles under whatever by/fn is chosen -
  // metricExplorer.quantileFamilyNote says so next to the chart.
  const quantileFamilies = useMemo(
    () => new Set((vocabulary.data?.items ?? []).filter((entry) => entry.quantiles).map((entry) => entry.series)),
    [vocabulary.data],
  );

  // Selecting one of the quantile-family histograms defaults the
  // aggregation to max, not avg: avg would blend p50 into p99 into one
  // meaningless number, while max at least keeps the worst per-bucket value
  // dominant (still combining quantiles together - see
  // metricExplorer.quantileFamilyNote - but less misleading than avg).
  // Still user-overridable afterward; only the metric switch itself resets it.
  const handleMetricChange = (value: string) => {
    setMetric(value);
    if (quantileFamilies.has(value)) setFn("max");
  };

  const params = useMemo<MonitorSeriesParams>(() => ({ metric, by, fn, range }), [metric, by, fn, range]);
  const { data, error } = usePolling(() => api.monitorSeries(params), 0, [metric, by, fn, range]);
  const series = useMemo(() => data?.items ?? [], [data]);
  const url = monitorSeriesURL(params);
  const seriesName = useCallback((labels: Record<string, string>, index: number) => explorerSeriesName(t, labels, index), [t]);

  const metricOptions = useMemo(() => {
    const entries = vocabulary.data?.items ?? [];
    return SOURCE_ORDER.map((source) => ({
      label: t(`metricExplorer.groups.${source}`),
      options: entries
        .filter((entry) => entry.source === source)
        .map((entry) => ({ label: entry.series, value: entry.series, title: entry.description })),
    })).filter((group) => group.options.length > 0);
  }, [vocabulary.data, t]);

  return (
    <div>
      <Space wrap size={16} style={{ marginBottom: 12 }}>
        <Field label={t("metricExplorer.metric")}>
          <Select
            aria-label={t("metricExplorer.metric")}
            style={{ width: 280 }}
            value={metric}
            onChange={handleMetricChange}
            options={metricOptions}
            showSearch
            optionFilterProp="label"
          />
        </Field>
        <Field label={t("metricExplorer.by")}>
          <Select
            aria-label={t("metricExplorer.by")}
            style={{ width: 160 }}
            value={by ?? ""}
            onChange={(value) => setBy((value || undefined) as MonitorSeriesParams["by"])}
            options={[
              { value: "", label: t("metricExplorer.byOptions.none") },
              ...BY_OPTIONS.map((value) => ({ value, label: t(`metricExplorer.byOptions.${value}`) })),
            ]}
          />
        </Field>
        <Field label={t("metricExplorer.fn")}>
          <Select
            aria-label={t("metricExplorer.fn")}
            style={{ width: 120 }}
            value={fn}
            onChange={setFn}
            options={FN_OPTIONS.map((value) => ({ value, label: t(`metricExplorer.fnOptions.${value}`) }))}
          />
        </Field>
      </Space>
      <TimeSeriesChart series={series} height={220} seriesName={seriesName} />
      {quantileFamilies.has(metric) && (
        <Typography.Text type="secondary" style={{ fontSize: 12, display: "block", marginTop: 6 }}>
          {t("metricExplorer.quantileFamilyNote")}
        </Typography.Text>
      )}
      {error && (
        <Typography.Text type="danger" style={{ display: "block", marginTop: 6 }}>
          {error.message}
        </Typography.Text>
      )}
      <Typography.Text type="secondary" style={{ fontSize: 12, display: "block", marginTop: 6, wordBreak: "break-all" }}>
        {t("metricExplorer.requestUrl", { url })}
      </Typography.Text>
    </div>
  );
}

function Field({ label, children }: { label: string; children: ReactNode }) {
  return (
    <Space direction="vertical" size={2}>
      <Typography.Text type="secondary" style={{ fontSize: 12 }}>
        {label}
      </Typography.Text>
      {children}
    </Space>
  );
}
