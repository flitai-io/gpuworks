import { Typography } from "antd";
import { Alert } from "../pages/DeploymentControls";
import { Select, SelectContent, SelectTrigger, SelectItem } from "./ui/select";
import { useMemo, type ComponentProps } from "react";
import { api, usePolling, type Instance, type MetricPoint, type MetricSeries } from "../api";
import TimeSeriesChart, { autoValueFmt } from "./TimeSeriesChart";
import EmptyState from "./EmptyState";
import GatewayMetricsNotice, { useGatewayOnlyMetrics } from "./GatewayMetricsNotice";
import { RangeHeader, useMetricWindow } from "./MetricWindow";
import { rateSeries } from "../instanceMetricSeries";
import { MetricStat, percentScaled } from "./InferenceMetricsPanel";
import { resolveRange, stepString, type TimeRangeSelection } from "../timeRange";
import { useTranslation } from "react-i18next";

const LIVE_STATES = new Set(["running", "starting", "optimizing"]);

const METRICS = {
  running: "gpuworks_infer_requests_running",
  waiting: "gpuworks_infer_requests_waiting",
  cache: "gpuworks_infer_gpu_cache_usage_percent",
  promptTokens: "gpuworks_infer_prompt_tokens_total",
  generationTokens: "gpuworks_infer_generation_tokens_total",
} as const;

type MetricKey = keyof typeof METRICS;
type MetricData = Record<MetricKey, MetricSeries[]>;

const EMPTY_DATA: MetricData = {
  running: [],
  waiting: [],
  cache: [],
  promptTokens: [],
  generationTokens: [],
};

// One stable string per selection so polling restarts only when the window
// really changes; a custom range never polls (pollMs 0 in resolveRange).
function selectionKey(selection: TimeRangeSelection): string {
  return selection.kind === "quick" ? selection.key : `custom-${selection.startMS}-${selection.endMS}`;
}

export default function DeploymentMetricsPanel({ instances, deploymentId, onSelectInstance }: { instances: Instance[]; deploymentId?: number; onSelectInstance?: (uuid: string) => void }) {
  const { t } = useTranslation("deployments");
  const [selection, setSelection] = useMetricWindow();
  const liveInstances = useMemo(
    () => instances.filter((inst) => LIVE_STATES.has(inst.state)),
    [instances],
  );
  const instanceKey = instances.map((inst) => inst.uuid).sort().join(",");
  const eff = resolveRange(selection, Date.now());
  const queryKey = `${deploymentId}/${instanceKey}/${selectionKey(selection)}`;
  const metrics = usePolling(async () => ({
    queryKey, ...await loadMetricData(instances, deploymentId, selection),
  }), eff.pollMs, [queryKey]);
  const result = metrics.data?.queryKey === queryKey ? metrics.data : null;
  const data = result?.data ?? EMPTY_DATA;
  const hasAnyMetric = Object.values(data).some(hasPoints);
  const gateway = useGatewayOnlyMetrics(
    result && !result.error && selection.kind === "quick" && !eff.beyondRetention && liveInstances.length > 0 && !hasAnyMetric ? deploymentId : undefined,
    eff.endMS - eff.startMS,
    eff.pollMs || 15000,
    liveInstances.every((inst) => inst.state === "running"),
  );

  const rangeHeader = (
    <RangeHeader selection={selection} onChange={setSelection} beyondRetention={eff.beyondRetention} />
  );
  if (gateway) return (
    <div className="flex flex-col gap-3">
      <GatewayMetricsNotice data={gateway} />
      {rangeHeader}
    </div>
  );

  const promptRate = rateSeries(data.promptTokens);
  const generationRate = rateSeries(data.generationTokens);
  const activeIds = new Set(liveInstances.map((inst) => inst.uuid));
  const runningNow = liveCardValue(data.running, activeIds);
  const waitingNow = liveCardValue(data.waiting, activeIds);
  const cachePeak = liveCardValue(data.cache, activeIds, "max");
  const liveGeneration = liveCardValue(data.generationTokens, activeIds, "rate");
  const promptNow = latestSum(promptRate);
  const generationNow = latestSum(generationRate);

  const concurrencySeries = [
    aggregateSeries(data.running, { metric: t("metrics.totalRunningRequests") }),
    aggregateSeries(data.waiting, { metric: t("metrics.totalWaitingRequests") }),
  ];
  const tokenSeries = [
    aggregateSeries(promptRate, { metric: "Prompt tok/s" }),
    aggregateSeries(generationRate, { metric: "Generation tok/s" }),
  ];

  const sampled = new Map<string, string>();
  for (const series of Object.values(data).flat()) {
    if (!series.points.length) continue;
    const uuid = series.labels.instance;
    const role = instances.find((inst) => inst.uuid === uuid)?.role || series.labels.role;
    sampled.set(uuid, [role && t(`detail.pd.role.${role}`, { defaultValue: role }), uuid.slice(0, 12)].filter(Boolean).join(" · "));
  }
  const historical = selection.kind === "custom" || liveInstances.length === 0;
  const empty = !hasPoints(concurrencySeries) && !hasPoints(tokenSeries) && !hasPoints(data.cache);
  return (
    <div className="deployment-metric-panel">
      {result?.error ? (
        <Alert type="error" showIcon message={t("metrics.loadFailed")} description={result.error.message} style={{ marginBottom: 12 }} />
      ) : empty && (
        <Alert type="info" role="status" showIcon
          message={historical && result ? t("metrics.emptyWindow") : t("metrics.waiting")}
          description={historical && result ? undefined : t("metrics.waitingDescription")} style={{ marginBottom: 12 }} />
      )}
      <div className="deployment-metrics-summary" role="region" aria-label={t("detail.metrics")} tabIndex={0}>
        <MetricStat title={t("metrics.runningInstances")} value={liveInstances.length} />
        <MetricStat title={t("metrics.runningRequests")} value={runningNow} />
        <MetricStat title={t("metrics.waitingRequests")} value={waitingNow} />
        <MetricStat title={t("metrics.peakCache")} value={cachePeak != null ? percentScaled(cachePeak) : undefined} suffix="%" />
        <MetricStat title={t("metrics.generationThroughput")} value={liveGeneration} suffix="tok/s" precision={1} />
      </div>
      {rangeHeader}

      <div className="deployment-metric-charts">
        <ChartCard title={t("metrics.concurrency")} description={t("metrics.concurrencyDescription")}>
          <MetricPlot series={concurrencySeries} height={190} valueFmt={(v) => v.toFixed(0)} integer />
        </ChartCard>
        <ChartCard title={t("metrics.tokenThroughput")} description={t("metrics.tokenDescription", {
          prompt: promptNow?.toFixed(1) ?? "—", generation: generationNow?.toFixed(1) ?? "—",
        })}>
          <MetricPlot series={tokenSeries} height={190} unit="tok/s" valueFmt={autoValueFmt} />
        </ChartCard>
        <ChartCard title={t("metrics.instanceCache")} description={t("metrics.instanceCacheDescription")}
          action={onSelectInstance && sampled.size > 0 && (
            <Select size="sm" value="" onValueChange={onSelectInstance}>
              <SelectTrigger aria-label={t("metrics.inspectInstance")} placeholder={t("metrics.inspectInstance")} contentSized />
              <SelectContent>{Array.from(sampled, ([uuid,label])=><SelectItem key={uuid} value={uuid}>{label}</SelectItem>)}</SelectContent>
            </Select>
          )}>
          <MetricPlot series={data.cache} height={210} unit="%" valueFmt={v => percentScaled(v).toFixed(0)}
            onSeriesClick={onSelectInstance && (labels => onSelectInstance(labels.instance))} />
        </ChartCard>
      </div>
    </div>
  );
}

async function loadMetricData(instances: Instance[], deploymentId: number | undefined, selection: TimeRangeSelection) {
  const eff = resolveRange(selection, Date.now());
  const params = { start: eff.startMS, end: eff.endMS, step: stepString(eff.stepMS) };
  const next: MetricData = { ...EMPTY_DATA };
  const reads = await Promise.allSettled((Object.keys(METRICS) as MetricKey[]).map(async (key) => {
    const groups = await Promise.all(instances.map(async ({ uuid }) => {
      const { items } = await api.metricsQuery(METRICS[key], { ...params, instance: uuid });
      return withInstanceLabel(items.filter(({ labels }) =>
        (!labels.instance || labels.instance === uuid) &&
        (!labels.deployment || deploymentId == null || labels.deployment === String(deploymentId))), uuid);
    }));
    next[key] = groups.flat();
  }));
  const failed = reads.find((read) => read.status === "rejected");
  return { data: next, error: failed ? new Error(String(failed.reason?.message ?? failed.reason)) : null };
}

function ChartCard({ title, description, action, children }: { title: string; description: string; action?: React.ReactNode; children: React.ReactNode }) {
  return (
    <div className="deployment-trend-card">
      <div className="flex flex-wrap items-center justify-between gap-2"><Typography.Text strong>{title}</Typography.Text>{action}</div>
      <Typography.Text type="secondary" style={{ display: "block", fontSize: "var(--font-size-label)", marginBottom: 8 }}>
        {description}
      </Typography.Text>
      {children}
    </div>
  );
}

function MetricPlot(chart: ComponentProps<typeof TimeSeriesChart>) {
  const { t } = useTranslation("common");
  return hasPoints(chart.series) ? <TimeSeriesChart {...chart} /> : (
    <div style={{ height: chart.height }}><EmptyState compact description={t("dataTable.empty")} /></div>
  );
}

function withInstanceLabel(series: MetricSeries[], instanceUuid: string): MetricSeries[] {
  return series.map((item) => ({
    labels: { ...item.labels, instance: instanceUuid },
    points: item.points ?? [],
  }));
}

function hasPoints(series?: MetricSeries[] | null): boolean {
  return !!series?.some((item) => item.points?.length > 0);
}

function latestSum(series: MetricSeries[]): number | undefined {
  const values = latestValues(series);
  return values.length ? values.reduce((sum, value) => sum + value, 0) : undefined;
}

function liveCardValue(series: MetricSeries[], activeIds: Set<string>, aggregation: "sum" | "max" | "rate" = "sum"): number | undefined {
  const values: number[] = [];
  for (const item of series) {
    const last = item.points[item.points.length - 1];
    if (!last || !activeIds.has(item.labels.instance)) continue;
    if (aggregation === "rate") {
      const previous = item.points[item.points.length - 2];
      if (!previous || previous[0] >= last[0]) continue;
      values.push(Math.max(0, last[1] - previous[1]) / ((last[0] - previous[0]) / 1000));
    } else {
      values.push(last[1]);
    }
  }
  if (!values.length) return undefined;
  return aggregation === "max" ? Math.max(...values) : values.reduce((sum, value) => sum + value, 0);
}

function latestValues(series: MetricSeries[]): number[] {
  const latest = series.flatMap((item) => item.points?.slice(-1) ?? []);
  const time = Math.max(...latest.map(([at]) => at));
  return latest.filter(([at]) => at === time).map(([, value]) => value);
}

function aggregateSeries(series: MetricSeries[], labels: Record<string, string>): MetricSeries {
  const pointsByTime = new Map<number, number>();
  for (const item of series) {
    for (const point of item.points ?? []) {
      pointsByTime.set(point[0], (pointsByTime.get(point[0]) ?? 0) + point[1]);
    }
  }
  return {
    labels,
    points: Array.from(pointsByTime.entries())
      .sort(([a], [b]) => a - b)
      .map(([time, value]): MetricPoint => [time, value]),
  };
}
