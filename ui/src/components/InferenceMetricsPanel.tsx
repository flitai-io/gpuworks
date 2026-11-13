import { MetricCard } from "./ui/metric-card";
import { Tooltip, Typography } from "antd";
import { Alert } from "../pages/DeploymentControls";
import { InfoCircleOutlined } from "@ant-design/icons";
import { Activity, useCallback, useMemo, useState, type ReactNode } from "react";
import { type MetricPoint, type MetricSeries } from "../api";
import TimeSeriesChart, { autoValueFmt, defaultSeriesName } from "./TimeSeriesChart";
import { useInstanceMetricSeries, rateSeries } from "../instanceMetricSeries";
import { StageCharts, useInstanceStages } from "./InstanceStagePanel";
import { RangeHeader, useMetricWindow } from "./MetricWindow";
import { colors } from "../theme/tokens";
import { Tabs, TabsList, TabItem, TabPanel } from "./ui/tabs";
import { resolveRange, type TimeRangeSelection } from "../timeRange";
import { useTranslation } from "react-i18next";

const METRICS = {
  running: "gpuworks_infer_requests_running",
  waiting: "gpuworks_infer_requests_waiting",
  cache: "gpuworks_infer_gpu_cache_usage_percent",
  promptTokens: "gpuworks_infer_prompt_tokens_total",
  generationTokens: "gpuworks_infer_generation_tokens_total",
  kvHits: "gpuworks_infer_kv_hits_total",
  kvQueries: "gpuworks_infer_kv_queries_total",
  kvHitRateInstant: "gpuworks_infer_kv_hit_rate_instant",
  specAcceptedTokens: "gpuworks_infer_spec_accepted_tokens_total",
  specDraftTokens: "gpuworks_infer_spec_draft_tokens_total",
  specAcceptRateInstant: "gpuworks_infer_spec_accept_rate",
} as const;

export default function InferenceMetricsPanel({ instanceUuid, historical = false, gpu, cacheTiers }: {
  instanceUuid: string;
  historical?: boolean;
  gpu?: (selection: TimeRangeSelection) => ReactNode;
  cacheTiers?: ReactNode;
}) {
  const { t } = useTranslation("deployments");
  const [selection, setSelection] = useMetricWindow();
  const [selectedGroup, setGroup] = useState("requests");
  const group = selectedGroup === "gpu" && !gpu ? "requests" : selectedGroup;
  const eff = resolveRange(selection, Date.now());
  const running = useInstanceMetricSeries(METRICS.running, instanceUuid, selection);
  const waiting = useInstanceMetricSeries(METRICS.waiting, instanceUuid, selection);
  const cache = useInstanceMetricSeries(METRICS.cache, instanceUuid, selection);
  const promptTokens = useInstanceMetricSeries(METRICS.promptTokens, instanceUuid, selection);
  const generationTokens = useInstanceMetricSeries(METRICS.generationTokens, instanceUuid, selection);
  const kvHits = useInstanceMetricSeries(METRICS.kvHits, instanceUuid, selection);
  const kvQueries = useInstanceMetricSeries(METRICS.kvQueries, instanceUuid, selection);
  const kvHitRateInstant = useInstanceMetricSeries(METRICS.kvHitRateInstant, instanceUuid, selection);
  const specAcceptedTokens = useInstanceMetricSeries(METRICS.specAcceptedTokens, instanceUuid, selection);
  const specDraftTokens = useInstanceMetricSeries(METRICS.specDraftTokens, instanceUuid, selection);
  const specAcceptRateInstant = useInstanceMetricSeries(METRICS.specAcceptRateInstant, instanceUuid, selection);

  const promptRate = useMemo(() => rateSeries(promptTokens.data?.items ?? []), [promptTokens.data]);
  const generationRate = useMemo(() => rateSeries(generationTokens.data?.items ?? []), [generationTokens.data]);
  const concurrencySeries = useMemo(() => [
    ...withMetricLabel(running.data?.items ?? [], "running"),
    ...withMetricLabel(waiting.data?.items ?? [], "waiting"),
  ], [running.data, waiting.data]);
  const concurrencySeriesName = useCallback((labels: Record<string, string>, index: number) => {
    if (labels.metric === "running") return t("instanceMetrics.running");
    if (labels.metric === "waiting") return t("instanceMetrics.waitingRequests");
    return defaultSeriesName(labels, index);
  }, [t]);
  // vLLM reports cumulative counters, so the window ratio is the precise
  // metric; SGLang only reports an instantaneous gauge, which is used as the
  // fallback when the counter pair is absent. The two are rendered under the
  // same card but never merged into one series.
  const kvWindowSeries = useMemo(
    () => ratioSeries(kvHits.data?.items ?? [], kvQueries.data?.items ?? []),
    [kvHits.data, kvQueries.data],
  );
  const kvCardSeries = useMemo(
    () => (hasPoints(kvWindowSeries) ? kvWindowSeries : kvHitRateInstant.data?.items ?? []),
    [kvWindowSeries, kvHitRateInstant.data],
  );
  const specWindowSeries = useMemo(
    () => ratioSeries(specAcceptedTokens.data?.items ?? [], specDraftTokens.data?.items ?? []),
    [specAcceptedTokens.data, specDraftTokens.data],
  );
  const specCardSeries = useMemo(
    () => (hasPoints(specWindowSeries) ? specWindowSeries : specAcceptRateInstant.data?.items ?? []),
    [specWindowSeries, specAcceptRateInstant.data],
  );
  // Counters that exist but never advanced mean the engine reports the metric
  // while no qualifying traffic happened yet — a different state from the
  // engine not exposing the metric at all.
  const kvCountersReported = hasPoints(kvHits.data?.items) || hasPoints(kvQueries.data?.items);
  const specCountersReported = hasPoints(specAcceptedTokens.data?.items) || hasPoints(specDraftTokens.data?.items);
  const stages = useInstanceStages(instanceUuid, selection);
  const queries = [running, waiting, cache, promptTokens, generationTokens, kvHits, kvQueries, kvHitRateInstant,
    specAcceptedTokens, specDraftTokens, specAcceptRateInstant];
  const error = queries.find((query) => query.error)?.error;
  const all = [...queries, ...Object.values(stages)];
  const empty = !all.some((query) => query.error || hasPoints(query.data?.items));
  const settled = all.every((query) => !query.loading);
  const runningNow = latest(running.data?.items ?? []);
  const waitingNow = latest(waiting.data?.items ?? []);
  // Cache gauges are 0..1 fractions on both engines; percentScaled renders
  // them (and any already-percent value) as percentages.
  const cacheNow = latest(cache.data?.items ?? []);
  const promptNow = latest(promptRate);
  const generationNow = latest(generationRate);

  return (
    <div className="deployment-metric-panel">
      {error ? <Alert type="error" showIcon message={t("metrics.loadFailed")} description={error.message} style={{ marginBottom: 12 }} /> :
        empty && <Alert type="info" showIcon role="status" style={{ marginBottom: 12 }}
          message={(historical || selection.kind === "custom") && settled ? t("metrics.emptyWindow") : t("instanceMetrics.waiting")}
          description={historical || selection.kind === "custom" ? undefined : t("instanceMetrics.waitingDescription")} />}
      {(historical || selection.kind === "custom") && [runningNow, waitingNow, cacheNow, promptNow, generationNow].some(v => v != null) && (
        <Typography.Text type="secondary">{t("instanceMetrics.lastInWindow")}</Typography.Text>
      )}
      <div className="deployment-metrics-summary" role="region" aria-label={t("instance.tab.metrics")} tabIndex={0}>
        <MetricStat title={t("instanceMetrics.running")} value={runningNow} />
        <MetricStat title={t("instanceMetrics.waitingRequests")} value={waitingNow} />
        <MetricStat title={t("instanceMetrics.gpuCache")} value={cacheNow != null ? percentScaled(cacheNow) : undefined} suffix="%" />
        <MetricStat title={t("instanceMetrics.promptThroughput")} value={promptNow} suffix="tok/s" precision={1} />
        <MetricStat title={t("instanceMetrics.generationThroughput")} value={generationNow} suffix="tok/s" precision={1} />
      </div>
      <Tabs variant="segment" value={group} onValueChange={setGroup}>
        <RangeHeader selection={selection} onChange={setSelection} beyondRetention={eff.beyondRetention}>
          <TabsList aria-label={t("instanceMetrics.groupLabel")} className="max-w-full overflow-x-auto">
            <TabItem value="requests" label={t("instanceMetrics.requestsGroup")} />
            <TabItem value="cache" label={t("instanceMetrics.cacheGroup")} />
            {gpu && <TabItem value="gpu" label={t("instanceMetrics.gpuGroup")} />}
          </TabsList>
        </RangeHeader>
        <TabPanel value="requests">
          <div className="deployment-metric-charts">
            <ChartCard title={t("instanceMetrics.requestConcurrency")} description={t("instanceMetrics.requestConcurrencyDescription")}>
              <TimeSeriesChart
                height={190}
                series={concurrencySeries}
                seriesName={concurrencySeriesName}
                valueFmt={(v) => v.toFixed(0)}
                integer
              />
            </ChartCard>
            <StageCharts {...stages} />
            <ChartCard title={t("metrics.tokenThroughput")} description={t("instanceMetrics.counterRate")}>
              <TimeSeriesChart height={190} series={[...withMetricLabel(promptRate, "prompt"), ...withMetricLabel(generationRate, "generation")]}
                seriesName={labels => t(labels.metric === "prompt" ? "instanceMetrics.promptThroughput" : "instanceMetrics.generationThroughput")}
                unit="tok/s" valueFmt={autoValueFmt} />
            </ChartCard>
          </div>
        </TabPanel>
        <TabPanel value="cache">
          <div className="deployment-metric-charts">
            <ChartCard title={t("instanceMetrics.cacheUsage")} description={t("instanceMetrics.cacheUsageDescription")}>
              <TimeSeriesChart height={190} series={cache.data?.items ?? []} unit="%" valueFmt={(v) => percentScaled(v).toFixed(0)} />
            </ChartCard>
            <ChartCard
              title={t("instanceMetrics.specAcceptRate")}
              description={t("instanceMetrics.specAcceptRateDescription")}
              hint={t("instanceMetrics.specAcceptRateScopeHint")}
            >
              <RatioChart series={specCardSeries} countersReported={specCountersReported} />
            </ChartCard>
            <ChartCard
              title={t("instanceMetrics.kvHitRate")}
              description={t("instanceMetrics.kvHitRateDescription")}
              hint={t("instanceMetrics.kvHitRateScopeHint")}
            >
              <RatioChart series={kvCardSeries} countersReported={kvCountersReported} />
            </ChartCard>
          </div>
          {group === "cache" && cacheTiers}
        </TabPanel>
        {gpu && (
          <TabPanel value="gpu" keepMounted>
            <Activity mode={group === "gpu" ? "visible" : "hidden"}>{gpu(selection)}</Activity>
          </TabPanel>
        )}
      </Tabs>
    </div>
  );
}

function RatioChart({ series, countersReported }: { series: MetricSeries[]; countersReported: boolean }) {
  const { t } = useTranslation("deployments");
  if (!hasPoints(series)) {
    return (
      <div style={{ height: 190, display: "flex", alignItems: "center", justifyContent: "center" }}>
        <Typography.Text type="secondary">
          — {countersReported ? t("instanceMetrics.metricNoTraffic") : t("instanceMetrics.metricUnavailable")}
        </Typography.Text>
      </div>
    );
  }
  return (
    <TimeSeriesChart
      height={190}
      series={series}
      unit="%"
      valueFmt={(v) => percentScaled(v).toFixed(1)}
    />
  );
}

// Engines disagree on whether ratio gauges are 0..1 fractions or 0..100
// percentages across versions; anything above 1 can only be a percentage.
export function percentScaled(value: number): number {
  return value > 1 ? value : value * 100;
}

function withMetricLabel(series: MetricSeries[], metric: string): MetricSeries[] {
  return series.map((item) => ({
    labels: { ...item.labels, metric },
    points: item.points ?? [],
  }));
}

function ChartCard({
  title,
  description,
  hint,
  children,
}: {
  title: ReactNode;
  description: string;
  hint?: string;
  children: ReactNode;
}) {
  return (
    <div className="deployment-trend-card">
      <Typography.Text strong>
        {title}
        {hint && (
          <Tooltip title={hint} trigger={["hover", "focus"]}>
            <InfoCircleOutlined tabIndex={0} style={{ marginLeft: 6, color: colors.muted }} aria-label={description} />
          </Tooltip>
        )}
      </Typography.Text>
      <Typography.Text type="secondary" style={{ display: "block", fontSize: "var(--font-size-label)", marginBottom: 8 }}>
        {description}
      </Typography.Text>
      {children}
    </div>
  );
}

function hasPoints(series?: MetricSeries[] | null): boolean {
  return !!series?.some((item) => item.points?.length > 0);
}

function latest(series: MetricSeries[]): number | undefined {
  for (let i = series.length - 1; i >= 0; i--) {
    const points = series[i].points;
    if (points?.length) return points[points.length - 1][1];
  }
  return undefined;
}

// ratioSeries derives a windowed ratio (e.g. cache hits / queries) from two
// cumulative counter series scraped in the same batch, so timestamps align.
// Windows where either counter reset (engine restart) or the denominator did
// not advance produce no point instead of a fabricated value.
export function ratioSeries(numerator: MetricSeries[], denominator: MetricSeries[]): MetricSeries[] {
  const denomByKey = new Map(denominator.map((item) => [JSON.stringify(item.labels ?? {}), item]));
  return numerator.map((num) => {
    const den = denomByKey.get(JSON.stringify(num.labels ?? {}));
    return {
      labels: num.labels,
      points: den ? toRatioPoints(num.points ?? [], den.points ?? []) : [],
    };
  });
}

function toRatioPoints(num: MetricPoint[], den: MetricPoint[]): MetricPoint[] {
  const denByTime = new Map(den.map((p) => [p[0], p[1]]));
  const out: MetricPoint[] = [];
  let prev: { timeMS: number; num: number; den: number } | null = null;
  for (const point of num) {
    const denValue = denByTime.get(point[0]);
    if (denValue === undefined) continue;
    if (prev) {
      const numDelta = point[1] - prev.num;
      const denDelta = denValue - prev.den;
      if (denDelta > 0 && numDelta >= 0) {
        out.push([point[0], numDelta / denDelta]);
      }
    }
    prev = { timeMS: point[0], num: point[1], den: denValue };
  }
  return out;
}

export function MetricStat({
  title,
  value,
  suffix,
  precision = 0,
}: {
  title: string;
  value?: number;
  suffix?: string;
  precision?: number;
}) {
  return (
    <MetricCard label={title} labelClassName="text-body text-fg-subtle font-normal" valueClassName="text-title font-semibold"
      value={value == null ? "—" : value.toLocaleString(undefined, {minimumFractionDigits:precision, maximumFractionDigits:precision})}
      unit={value==null ? undefined : suffix} />
  );
}
