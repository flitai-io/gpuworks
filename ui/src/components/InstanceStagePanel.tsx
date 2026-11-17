import { Notice } from "../ui-kit/Notice";
import { Typography } from "antd";
import { useCallback, useMemo } from "react";
import { useTranslation } from "react-i18next";
import type { TFunction } from "i18next";
import { type MetricSeries } from "../api";
import { rateSeries, useInstanceMetricSeries } from "../instanceMetricSeries";
import type { TimeRangeSelection } from "../timeRange";
import TimeSeriesChart, { defaultSeriesName } from "./TimeSeriesChart";

export function useInstanceStages(instanceUuid: string, selection: TimeRangeSelection) {
  const queue = useInstanceMetricSeries("gpuworks_infer_queue_time_seconds", instanceUuid, selection);
  const prefill = useInstanceMetricSeries("gpuworks_infer_prefill_time_seconds", instanceUuid, selection);
  const decode = useInstanceMetricSeries("gpuworks_infer_decode_time_seconds", instanceUuid, selection);
  const preemptions = useInstanceMetricSeries("gpuworks_infer_preemptions_total", instanceUuid, selection);
  return { queue, prefill, decode, preemptions };
}

export default function InstanceStagePanel({ instanceUuid, selection }: { instanceUuid: string; selection: TimeRangeSelection }) {
  const stages = useInstanceStages(instanceUuid, selection);
  return (
    <div className="deployment-metric-panel">
      <div className="deployment-metric-charts"><StageCharts {...stages} /></div>
    </div>
  );
}

export function StageCharts({ queue, prefill, decode, preemptions }: ReturnType<typeof useInstanceStages>) {
  const { t } = useTranslation("deployments");
  const stageSeries = useMemo(() => [
    ...stageP95Millis(queue.data?.items, "queue_ms"),
    ...stageP95Millis(prefill.data?.items, "prefill_ms"),
    ...stageP95Millis(decode.data?.items, "decode_ms"),
  ], [queue.data, prefill.data, decode.data]);
  const preemptSeries = useMemo(() => rateSeries(preemptions.data?.items ?? []), [preemptions.data]);
  const seriesName = useCallback((labels: Record<string, string>, index: number) => stageSeriesName(labels, index, t), [t]);

  return (
    <>
      <StageCard title={t("instanceStages.stageTitle")} description={t("instanceStages.stageDescription")}>
        {(queue.error || prefill.error || decode.error) && <Notice tone="danger" title={t("metrics.loadFailed")}>{(queue.error || prefill.error || decode.error)?.message}</Notice>}
        {hasPoints(stageSeries) ? (
          <TimeSeriesChart height={190} series={stageSeries} seriesName={seriesName} unit="ms" valueFmt={(v) => v.toFixed(0)} />
        ) : (
          <NotCollected text={t("instanceStages.notCollected")} />
        )}
      </StageCard>
      <StageCard title={t("instanceStages.preemptTitle")} description={t("instanceStages.preemptDescription")}>
        {preemptions.error && <Notice tone="danger" title={t("metrics.loadFailed")}>{preemptions.error.message}</Notice>}
        {hasPoints(preemptSeries) ? (
          <TimeSeriesChart height={190} series={preemptSeries} seriesName={seriesName} unit="/s" valueFmt={(v) => v.toFixed(2)} />
        ) : (
          <NotCollected text={t("instanceStages.notCollected")} />
        )}
      </StageCard>
    </>
  );
}

export function stageSeriesName(labels: Record<string, string>, index: number, t: TFunction<"deployments">): string {
  switch (labels.metric) {
    case "queue_ms": return t("instanceStages.queue");
    case "prefill_ms": return t("instanceStages.prefill");
    case "decode_ms": return t("instanceStages.decode");
    default: return defaultSeriesName(labels, index);
  }
}

export function stageP95Millis(series: MetricSeries[] | undefined, metric: string): MetricSeries[] {
  return (series ?? []).filter((item) => item.labels.quantile === "0.95").map((item) => ({
    labels: { ...item.labels, metric },
    points: item.points.map(([time, seconds]) => [time, seconds * 1000]),
  }));
}

function hasPoints(series?: MetricSeries[] | null): boolean {
  return !!series?.some((item) => item.points?.length > 0);
}

function NotCollected({ text }: { text: string }) {
  return (
    <div style={{ height: 190, display: "flex", alignItems: "center", justifyContent: "center" }}>
      <Typography.Text type="secondary">— {text}</Typography.Text>
    </div>
  );
}

function StageCard({ title, description, children }: { title: string; description: string; children: React.ReactNode }) {
  return (
    <div className="deployment-trend-card">
      <Typography.Text strong>{title}</Typography.Text>
      <Typography.Text type="secondary" style={{ display: "block", fontSize: "var(--font-size-label)", marginBottom: 8 }}>{description}</Typography.Text>
      {children}
    </div>
  );
}
