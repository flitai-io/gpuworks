import { useEffect, useMemo, useRef } from "react";
import * as echarts from "echarts/core";
import { LineChart } from "echarts/charts";
import { GridComponent, LegendComponent, MarkLineComponent, TooltipComponent } from "echarts/components";
import { CanvasRenderer } from "echarts/renderers";
import { autoValueFmt, buildTimeSeriesOption, defaultSeriesName, type ChartMarkLine, type RightAxis } from "./timeSeriesOption";
import type { MetricSeries } from "../api";
import { colors } from "../theme/tokens";
import EmptyState from "./EmptyState";
import { useTranslation } from "react-i18next";

export * from "./timeSeriesOption";

echarts.use([LineChart, GridComponent, LegendComponent, TooltipComponent, MarkLineComponent, CanvasRenderer]);

interface Props {
  series: MetricSeries[];
  // Leave unavailable observations as gaps when false.
  connectNulls?: boolean;
  height?: number;
  unit?: string;
  valueFmt?: (v: number) => string;
  seriesName?: (labels: Record<string, string>, index: number) => string;
  resetKey?: string | number;
  // group joins this chart's crosshair/tooltip to every other mounted
  // TimeSeriesChart sharing the same group id via echarts.connect. Optional
  // and backwards compatible: charts that omit it behave exactly as before.
  group?: string;
  // markLines draws vertical event annotations (e.g. instance state
  // transitions) on the first visible series. Optional and additive.
  markLines?: ChartMarkLine[];
  // integer marks a count (requests, replicas): ticks stay whole numbers, so
  // an idle chart does not read "1 1 1 0 0 0" from rounded fractions.
  integer?: boolean;
  // yRange pins the value axis, e.g. [0, 100] for a percentage or
  // [0, limit] for a slice's memory: a fitted axis turns a flat 0% line into
  // ticks reading "1 1 1 0 0 0" and hides how far a value is from its cap.
  yRange?: [number, number];
  // leftName names the left value axis instead of unit (which still
  // suffixes tooltip values).
  leftName?: string;
  // rightAxis moves the series it matches onto a second value axis on the
  // right, e.g. the kv chart's hit rate beside its usage: two quantities
  // with a meaning of their own, each read against its own named axis.
  rightAxis?: RightAxis;
}

// applyChartGroup wires an ECharts instance into echarts.connect's shared
// crosshair group. Extracted from the effect below so a test can verify the
// wiring (chart.group gets set) without needing a real DOM/canvas-backed
// chart instance.
export function applyChartGroup(chart: { group: string } | null | undefined, group?: string): void {
  if (!chart || !group) return;
  chart.group = group;
  echarts.connect(group);
}

export default function TimeSeriesChart({ series, height = 200, unit, valueFmt = autoValueFmt, seriesName = defaultSeriesName, resetKey, group, markLines, yRange, integer, leftName, rightAxis, connectNulls }: Props) {
  const { t } = useTranslation("common");
  const hostRef = useRef<HTMLDivElement>(null);
  const chartRef = useRef<ReturnType<typeof echarts.init> | null>(null);
  const previousResetKey = useRef(resetKey);
  const visibleSeries = useMemo(() => series.filter((entry) => entry.points?.length > 0), [series]);

  useEffect(() => {
    if (!hostRef.current) return;
    const chart = echarts.init(hostRef.current, undefined, { renderer: "canvas" });
    chartRef.current = chart;
    const resize = () => chart.resize();
    const observer = new ResizeObserver(resize);
    observer.observe(hostRef.current);
    window.addEventListener("resize", resize);
    return () => {
      observer.disconnect();
      window.removeEventListener("resize", resize);
      chart.dispose();
      chartRef.current = null;
    };
  }, []);

  useEffect(() => {
    applyChartGroup(chartRef.current, group);
  }, [group]);

  useEffect(() => {
    const chart = chartRef.current;
    if (!chart) return;
    if (previousResetKey.current !== resetKey) {
      chart.clear();
      previousResetKey.current = resetKey;
    }
    const option = buildTimeSeriesOption(visibleSeries, { unit, valueFmt, seriesName, markLines, yRange, integer, leftName, rightAxis, connectNulls });
    chart.setOption(option, { notMerge: false, lazyUpdate: true, replaceMerge: ["series", "legend", "yAxis"] });
  }, [resetKey, seriesName, unit, valueFmt, visibleSeries, markLines, yRange?.[0], yRange?.[1], integer, leftName, rightAxis, connectNulls]);

  return <div style={{ position: "relative", height }}>
    <div ref={hostRef} role="img" aria-label={t("timeSeries.ariaLabel")} style={{ width: "100%", height }} />
    {visibleSeries.length === 0 && <div style={{ position: "absolute", inset: 0, background: colors.white }}><EmptyState compact kind="data" description={t("dataTable.empty")} /></div>}
  </div>;
}
