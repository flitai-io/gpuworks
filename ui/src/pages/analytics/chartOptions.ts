// Pure builders of the analytics charts' ECharts options, kept apart from the
// component so the shading, the axes and the colour scales can be tested
// against plain objects (a canvas is not available under the test DOM).
import type { BarSeriesOption, EChartsOption } from "echarts";
import { colors } from "../../theme/tokens";
import type { GpuMetric, GPUHeatRow } from "../../analyticsApi";

export const SERIES_COLORS = {
  hit: colors.chartPrimary,
  miss: colors.chartSecondary,
  answer: colors.success,
  thinking: colors.chartTeal,
  ok: colors.success,
  err: colors.danger,
  interrupted: colors.warning,
  grey: "#8c8c8c",
  line: [colors.chartPrimary, colors.chartSecondary],
  deployments: [colors.chartPrimary, colors.chartTeal, colors.chartSecondary, colors.chartQuaternary, colors.chartTertiary, colors.chartViolet],
} as const;

const HEAT_RAMP = ["#eef6ff", "#b5d8ff", "#5aa6ff", "#1668dc", "#0b3d91"];
const BACKFILL_FILL = "rgba(255,144,0,.10)";
/** Past this many buckets the axis scrolls and zooms instead of crowding. */
const MANY = 48;

export interface BarSeries {
  name: string;
  color: string;
  data: (number | null)[];
}

const escapeHTML = (s: string) => s.replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[c] ?? c);

const baseText = { color: colors.muted, fontFamily: "inherit" };

function categoryAxis(labels: string[], boundaryGap = true) {
  return {
    type: "category" as const,
    data: labels,
    boundaryGap,
    axisLine: { lineStyle: { color: colors.chartGrid } },
    axisTick: { show: false },
    axisLabel: { color: colors.chartTick, hideOverlap: true },
  };
}

function zoom(many: boolean) {
  return many ? [{ type: "inside" as const }, { type: "slider" as const, height: 18, bottom: 6 }] : [];
}

type Shade = NonNullable<BarSeriesOption["markArea"]>;

/** shadeArea marks the first `leading` of `total` buckets: they were filled in from the request log. */
export function shadeArea(leading: number | undefined, total: number, label = ""): Shade | undefined {
  if (!leading || leading <= 0) return undefined;
  return {
    silent: true,
    animation: false,
    itemStyle: { color: BACKFILL_FILL },
    label: { show: true, position: "insideTopLeft", color: colors.chartTick, fontSize: 12, formatter: label },
    data: [[{ xAxis: -0.5 }, { xAxis: Math.min(leading, total) - 0.5 }]],
  };
}

export interface StackedBarInput {
  labels: string[];
  series: BarSeries[];
  /** The tooltip's figure, e.g. a count or "3.5 GPU hours". */
  valueFormat: (v: number) => string;
  axisFormat?: (v: number) => string;
  totalLabel: string;
  /** How many leading buckets were filled in from the request log; they are shaded. */
  shadeLeading?: number;
  shadeLabel?: string;
}

export function stackedBarOption(input: StackedBarInput): EChartsOption {
  const many = input.labels.length > MANY;
  const shade = shadeArea(input.shadeLeading, input.labels.length, input.shadeLabel);
  return {
    animation: false,
    color: input.series.map((s) => s.color),
    textStyle: baseText,
    tooltip: {
      trigger: "axis",
      axisPointer: { type: "shadow" },
      formatter: (raw) => {
        const items = (Array.isArray(raw) ? raw : [raw]) as { value?: unknown; marker?: string; seriesName?: string; axisValueLabel?: string }[];
        const shown = items.filter((p) => p.value != null);
        const total = shown.reduce((a, p) => a + (Number(p.value) || 0), 0);
        return `<div style="font-weight:600;margin-bottom:4px">${escapeHTML(items[0]?.axisValueLabel ?? "")}</div>`
          + shown.map((p) => `${p.marker ?? ""}${escapeHTML(p.seriesName ?? "")}<span style="float:right;margin-left:24px">${input.valueFormat(Number(p.value))}</span>`).join("<br>")
          + `<div style="border-top:1px solid ${colors.border};margin-top:4px;padding-top:4px">${escapeHTML(input.totalLabel)}<span style="float:right;margin-left:24px">${input.valueFormat(total)}</span></div>`;
      },
    },
    legend: { type: "scroll", top: 0, textStyle: { color: colors.muted }, data: input.series.map((s) => s.name) },
    grid: { left: 4, right: 12, top: 36, bottom: many ? 52 : 4, containLabel: true },
    xAxis: categoryAxis(input.labels),
    yAxis: { type: "value", axisLabel: { color: colors.chartTick, formatter: input.axisFormat }, splitLine: { lineStyle: { color: colors.chartGrid } } },
    dataZoom: zoom(many),
    series: input.series.map((s, i) => ({
      name: s.name,
      type: "bar" as const,
      stack: "total",
      data: s.data,
      barMaxWidth: 30,
      emphasis: { focus: "series" as const },
      markArea: i === 0 ? shade : undefined,
    })),
  };
}

export interface LineInput {
  labels: string[];
  series: BarSeries[];
  /** The tooltip's figure for a value in the axis unit. */
  valueFormat: (v: number) => string;
  axisName?: string;
  shadeLeading?: number;
  shadeLabel?: string;
}

export function lineOption(input: LineInput): EChartsOption {
  const many = input.labels.length > MANY;
  const shade = shadeArea(input.shadeLeading, input.labels.length, input.shadeLabel);
  return {
    animation: false,
    color: input.series.map((s) => s.color),
    textStyle: baseText,
    tooltip: {
      trigger: "axis",
      formatter: (raw) => {
        const items = (Array.isArray(raw) ? raw : [raw]) as { value?: unknown; marker?: string; seriesName?: string; axisValueLabel?: string }[];
        return `<div style="font-weight:600;margin-bottom:4px">${escapeHTML(items[0]?.axisValueLabel ?? "")}</div>`
          + items.map((p) => `${p.marker ?? ""}${escapeHTML(p.seriesName ?? "")}<span style="float:right;margin-left:24px">${p.value == null ? "—" : input.valueFormat(Number(p.value))}</span>`).join("<br>");
      },
    },
    legend: { top: 0, textStyle: { color: colors.muted }, data: input.series.map((s) => s.name) },
    grid: { left: 4, right: 16, top: 36, bottom: many ? 52 : 4, containLabel: true },
    xAxis: categoryAxis(input.labels, input.labels.length <= 2),
    yAxis: { type: "value", name: input.axisName, nameTextStyle: { color: colors.chartTick }, axisLabel: { color: colors.chartTick }, splitLine: { lineStyle: { color: colors.chartGrid } } },
    dataZoom: zoom(many),
    series: input.series.map((s, i) => ({
      name: s.name,
      type: "line" as const,
      data: s.data,
      smooth: false,
      connectNulls: false,
      showSymbol: input.labels.length <= MANY,
      symbolSize: 5,
      lineStyle: { width: 2 },
      markArea: i === 0 ? shade : undefined,
    })),
  };
}

/** heatScale is the colour scale's [min, max] for a metric: fixed for a percentage or a temperature, fitted to the data for watts and bytes. */
export function heatScale(metric: GpuMetric, rows: GPUHeatRow[]): [number, number] {
  let max = 0;
  for (const row of rows) for (const v of row.values) if (v != null && v > max) max = v;
  switch (metric) {
    case "util": return [0, 100];
    case "temp": return [20, Math.max(100, Math.ceil(max))];
    case "power": return [0, Math.max(1, Math.ceil(max))];
    case "mem": return [0, Math.max(1, max)];
  }
}

export interface HeatInput {
  labels: string[];
  rows: GPUHeatRow[];
  metric: GpuMetric;
  valueFormat: (v: number) => string;
}

/** heatData is [bucket, row, value] for every cell with a value; a cell without samples is left out and stays blank. */
export function heatData(rows: GPUHeatRow[]): [number, number, number][] {
  const out: [number, number, number][] = [];
  rows.forEach((row, ri) => row.values.forEach((v, bi) => {
    if (v != null) out.push([bi, ri, v]);
  }));
  return out;
}

export function heatmapOption(input: HeatInput): EChartsOption {
  const many = input.labels.length > MANY;
  const [min, max] = heatScale(input.metric, input.rows);
  return {
    animation: false,
    textStyle: baseText,
    tooltip: {
      position: "top",
      formatter: (raw) => {
        const p = raw as { value?: [number, number, number] };
        if (!p.value) return "";
        return `<div style="font-weight:600;margin-bottom:2px">${escapeHTML(input.rows[p.value[1]]?.label ?? "")}</div>${escapeHTML(input.labels[p.value[0]] ?? "")}`
          + `<span style="float:right;margin-left:24px">${input.valueFormat(p.value[2])}</span>`;
      },
    },
    grid: { left: 4, right: 12, top: 44, bottom: many ? 52 : 6, containLabel: true },
    xAxis: { ...categoryAxis(input.labels), splitArea: { show: false } },
    yAxis: { type: "category", data: input.rows.map((r) => r.label), inverse: true, axisLine: { show: false }, axisTick: { show: false }, axisLabel: { color: colors.muted } },
    visualMap: {
      min,
      max,
      calculable: false,
      orient: "horizontal",
      top: 0,
      right: 12,
      itemWidth: 12,
      itemHeight: 150,
      text: [input.valueFormat(max), input.valueFormat(min)],
      textStyle: { color: colors.chartTick },
      inRange: { color: HEAT_RAMP },
    },
    dataZoom: many ? [{ type: "inside", xAxisIndex: 0 }, { type: "slider", xAxisIndex: 0, height: 18, bottom: 6 }] : [],
    series: [{
      type: "heatmap",
      data: heatData(input.rows),
      progressive: 2000,
      itemStyle: { borderWidth: many ? 0 : 1, borderColor: colors.white },
      emphasis: { itemStyle: { borderColor: colors.ink, borderWidth: 1 } },
    }],
  };
}

/** heatHeight is the chart's height for its rows: tall enough to read every label, capped. */
export function heatHeight(rows: number): number {
  return Math.min(620, Math.max(220, rows * 22 + 110));
}
