import { api, usePolling, type MetricPoint, type MetricSeries } from "./api";
import { resolveRange, stepString, type TimeRangeSelection } from "./timeRange";

export function useInstanceMetricSeries(metric: string, instanceUuid: string, selection: TimeRangeSelection) {
  const rangeKey = selection.kind === "quick" ? selection.key : `${selection.startMS}-${selection.endMS}`;
  const queryKey = `${instanceUuid}/${metric}/${rangeKey}`;
  const { pollMs } = resolveRange(selection, Date.now());
  const result = usePolling(async () => {
    const { startMS, endMS, stepMS } = resolveRange(selection, Date.now());
    try {
      const data = await api.metricsQuery(metric, { instance: instanceUuid, start: startMS, end: endMS, step: stepString(stepMS) });
      return { queryKey, data, error: null };
    } catch (err) {
      return { queryKey, data: null, error: err instanceof Error ? err : new Error(String(err)) };
    }
  }, pollMs, [queryKey]);
  const current = result.data?.queryKey === queryKey ? result.data : null;
  return { loading: !current, data: current?.data, error: current?.error };
}

export function rateSeries(series: MetricSeries[]): MetricSeries[] {
  return series.map((item) => ({
    labels: item.labels,
    points: toRatePoints(item.points ?? []),
  }));
}

function toRatePoints(points: MetricPoint[]): MetricPoint[] {
  const out: MetricPoint[] = [];
  for (let i = 1; i < points.length; i++) {
    const prev = points[i - 1];
    const cur = points[i];
    const seconds = (cur[0] - prev[0]) / 1000;
    if (seconds <= 0) continue;
    const delta = cur[1] < prev[1] ? cur[1] : cur[1] - prev[1];
    out.push([cur[0], delta / seconds]);
  }
  return out;
}
