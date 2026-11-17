// Pure builder of the scheduling activity chart's ECharts option, kept apart
// from the component so it can be tested against plain objects (no canvas
// under the test DOM). The reasons, their order and the buckets are the
// server's; this only draws them.
import type { EChartsOption } from "echarts";
import type { SchedulingActivity } from "../../schedulingApi";
import { colors } from "../../theme/tokens";

// The colour each reason the server names is drawn in; a reason it adds
// later takes the next colour of the palette.
const REASON_COLORS: Record<string, string> = {
  idle_yield: colors.chartTeal,
  rank_preemption: colors.chartPrimary,
  timeslice_turn: colors.chartQuaternary,
  operator_yield: colors.chartViolet,
  scheduled: colors.success,
  placement: colors.text,
  failure: colors.danger,
  queued: colors.muted,
};
const FALLBACK = [colors.chartSecondary, colors.chartTick];

export function reasonColor(key: string, index: number): string {
  return REASON_COLORS[key] ?? FALLBACK[index % FALLBACK.length];
}

/** bucketLabel is a bucket's start as the axis shows it: the time, and the date for day-long buckets or a range over a day. */
export function bucketLabel(startISO: string, bucketSeconds: number, spanSeconds: number, locale: string): string {
  const at = new Date(startISO);
  if (bucketSeconds >= 86_400) return at.toLocaleDateString(locale, { month: "2-digit", day: "2-digit" });
  const time = at.toLocaleTimeString(locale, { hour: "2-digit", minute: "2-digit", hour12: false });
  if (spanSeconds > 86_400) return `${at.toLocaleDateString(locale, { month: "2-digit", day: "2-digit" })} ${time}`;
  return time;
}

/**
 * activityOption stacks one bar series per reason over the buckets; name
 * labels a reason (the page's translation of its key).
 */
export function activityOption(data: SchedulingActivity, name: (key: string) => string, locale: string): EChartsOption {
  const span = (Date.parse(data.to) - Date.parse(data.from)) / 1000;
  const labels = data.buckets.map((bucket) => bucketLabel(bucket.start, data.bucket_seconds, span, locale));
  return {
    animation: false,
    grid: { left: 40, right: 12, top: 36, bottom: 28 },
    legend: { top: 0, left: 0, itemWidth: 10, itemHeight: 10, textStyle: { color: colors.muted } },
    tooltip: { trigger: "axis", axisPointer: { type: "shadow" } },
    xAxis: {
      type: "category",
      data: labels,
      axisLine: { lineStyle: { color: colors.chartGrid } },
      axisTick: { show: false },
      axisLabel: { color: colors.chartTick, hideOverlap: true },
    },
    yAxis: {
      type: "value",
      minInterval: 1,
      splitLine: { lineStyle: { color: colors.chartGrid } },
      axisLabel: { color: colors.chartTick },
    },
    series: data.reasons.map((reason, index) => ({
      type: "bar" as const,
      name: name(reason.key),
      stack: "activity",
      barMaxWidth: 24,
      itemStyle: { color: reasonColor(reason.key, index) },
      emphasis: { focus: "series" as const },
      data: data.buckets.map((bucket) => bucket.counts[index] ?? 0),
    })),
  };
}

/** isEmpty says the range holds no event at all. */
export function isEmpty(data: SchedulingActivity): boolean {
  return data.reasons.every((reason) => reason.count === 0);
}
