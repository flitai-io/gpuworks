import type { EChartsOption } from "echarts";
import type { MetricSeries } from "../api";
import { colors } from "../theme/tokens";

export interface ChartMarkLine {
  time: number;
  label?: string;
  color?: string;
}

export interface RightAxis {
  matches: (labels: Record<string, string>) => boolean;
  name?: string;
  yRange?: [number, number];
}

const PALETTE = [colors.chartPrimary, colors.chartSecondary, colors.chartTertiary, colors.chartQuaternary, colors.chartTeal, colors.chartViolet];
// autoValueFmt keeps as many decimals as the magnitude needs: a fixed
// one-decimal format turned a 0..0.1 tok/s axis into "0.1 0.1 0.0 0.0".
export function autoValueFmt(value: number): string {
  if (Number.isInteger(value)) return String(value);
  const magnitude = Math.abs(value);
  if (magnitude >= 100) return value.toFixed(0);
  if (magnitude >= 1) return value.toFixed(1);
  return value.toFixed(2);
}
const defaultValueFmt = autoValueFmt;

// defaultSeriesName names a line by what tells it apart from its neighbours:
// the metric it is, the card it is on, the instance it belongs to, and only
// failing all three the node or the deployment. It never prints a label's key - a legend
// reading "metric Running requests" or "node b5070775… · gpu 1 · gpu_uuid
// GPU-e330…" names the wire format, not the line - and a caller that knows
// the nodes' names passes its own seriesName.
export function defaultSeriesName(labels: Record<string, string>, idx: number): string {
  if (!labels || Object.keys(labels).length === 0) return `series ${idx + 1}`;
  const parts: string[] = [];
  if (labels.metric) parts.push(labels.metric);
  // node_name is the server's display name for the node label
  // (GET /metrics/query): a card is "<node> · GPU 1", read across a fleet.
  if (labels.gpu) parts.push(labels.node_name ? `${labels.node_name} · GPU ${labels.gpu}` : `GPU ${labels.gpu}`);
  if (labels.instance) parts.push(shorten(labels.instance));
  if (!parts.length && labels.node) parts.push(labels.node_name || shorten(labels.node));
  // deployment_name is the server's display name for the deployment label.
  if (!parts.length && labels.deployment) parts.push(labels.deployment_name || `#${labels.deployment}`);
  if (parts.length) return parts.join(" · ");
  const known = new Set(["node", "node_name", "node_deleted", "gpu_uuid", "deployment", "deployment_name", "deployment_deleted"]);
  const rest = Object.entries(labels).filter(([key]) => !known.has(key)).map(([, value]) => shorten(value));
  return rest.length ? rest.join(" · ") : `series ${idx + 1}`;
}

// Labels identify a metric stream, but separate metrics can legitimately have
// the same labels when they share a chart. ECharts requires every series id in
// one option to be unique, so include the position after the stable wire label.
export function metricSeriesID(labels: Record<string, string>, index: number): string {
  return `${JSON.stringify(labels)}#${index}`;
}

function shorten(value: string): string { return value.length > 12 ? `${value.slice(0, 8)}…` : value; }

// buildTimeSeriesOption is the pure computation behind the chart's
// setOption call, extracted so markLines/legend/series shaping can be
// tested directly against a plain object instead of a live ECharts
// instance (which needs a canvas the test DOM cannot provide). It never
// sets a dataZoom: the drawer scrolls over charts, so both the slider and
// wheel zoom mis-trigger; the window is chosen by the time-range control
// above the chart instead.
export function buildTimeSeriesOption(
  visibleSeries: MetricSeries[],
  {
    unit,
    valueFmt = defaultValueFmt,
    seriesName = defaultSeriesName,
    markLines,
    yRange,
    integer,
    leftName,
    rightAxis,
    connectNulls = true,
  }: { unit?: string; valueFmt?: (v: number) => string; seriesName?: (labels: Record<string, string>, index: number) => string; markLines?: ChartMarkLine[]; yRange?: [number, number]; integer?: boolean; leftName?: string; rightAxis?: RightAxis; connectNulls?: boolean },
): EChartsOption {
  const legend = visibleSeries.map((entry, index) => seriesName(entry.labels, index));
  // About five time labels whatever the window or width: ECharts' time axis
  // rounds any interval asked of it to a dense minute step and its
  // hideOverlap lets touching labels stand, so the drawer's half-width charts
  // read "04:4404:4604:48…". The formatter keeps only labels a fifth of the
  // span apart.
  let first = Infinity;
  let last = -Infinity;
  for (const entry of visibleSeries) {
    for (const point of entry.points) {
      first = Math.min(first, point[0]);
      last = Math.max(last, point[0]);
    }
  }
  const span = last > first ? last - first : 0;
  // A chart of one line is named by its title; a legend would only repeat it
  // and take the strip the x-axis labels need.
  const showLegend = visibleSeries.length > 1;
  const onRight = visibleSeries.map((entry) => rightAxis?.matches(entry.labels) ?? false);
  const dualAxis = onRight.includes(true);
  const axisName = leftName ?? unit;
  // A pinned range is cut into five even steps: left to itself the axis
  // picks steps round in the raw unit (bytes), which read 0.9, 1.9, 2.8 GiB.
  const valueAxis = (name: string | undefined, range: [number, number] | undefined, position: "left" | "right") => ({
    type: "value" as const,
    position,
    scale: !range,
    ...(range ? { min: range[0], max: range[1], interval: (range[1] - range[0]) / 5 } : {}),
    ...(integer ? { minInterval: 1 } : {}),
    name,
    nameLocation: "end" as const,
    nameGap: 8,
    nameTextStyle: { color: colors.chartTick, align: position === "left" ? ("right" as const) : ("left" as const), fontSize: 11 },
    // A scaled axis over a narrow band (VRAM within 0.05 GiB) puts several
    // ticks on one rounded label, which read as "2.6 2.6 2.6"; each label is
    // drawn once, at its lowest tick.
    axisLabel: { color: colors.chartTick, formatter: onceEach(valueFmt) },
    axisLine: { show: false },
    // Only the left axis draws grid lines: two sets that disagree read as noise.
    splitLine: position === "left" ? { lineStyle: { color: colors.chartGrid, type: "dashed" as const } } : { show: false },
  });
  return {
    animation: false,
    color: PALETTE,
    // The grid holds its axis labels (containLabel): a fixed left margin cut
    // the first digits of wide values ("168 Kb/s" read "8 Kb/s"). Margins are
    // then outside the labels: the bottom leaves the legend its strip, the
    // top room for the unit above the y-axis.
    grid: { containLabel: true, top: axisName || dualAxis ? 28 : 18, right: 20, bottom: showLegend ? 30 : 6, left: 8 },
    tooltip: { trigger: "axis", axisPointer: { type: "cross" }, valueFormatter: (value) => `${valueFmt(Number(value))}${unit ? ` ${unit}` : ""}` },
    legend: { show: showLegend, type: "scroll", bottom: 2, left: 8, right: 8, textStyle: { color: colors.text, fontSize: 12 }, data: legend },
    xAxis: { type: "time", boundaryGap: [0, 0], axisLabel: { color: colors.chartTick, hideOverlap: true, ...(span ? { formatter: spacedTimeLabels(span) } : {}) }, axisLine: { lineStyle: { color: colors.chartGrid } }, splitLine: { lineStyle: { color: colors.chartGrid, type: "dashed" } } },
    yAxis: dualAxis ? [valueAxis(axisName, yRange, "left"), valueAxis(rightAxis?.name, rightAxis?.yRange, "right")] : valueAxis(axisName, yRange, "left"),
    series: visibleSeries.map((entry, index) => ({
      id: metricSeriesID(entry.labels, index),
      name: legend[index],
      type: "line",
      ...(dualAxis ? { yAxisIndex: onRight[index] ? 1 : 0 } : {}),
      showSymbol: false,
      smooth: false,
      connectNulls,
      emphasis: { focus: "series" },
      data: entry.points.map((point) => [point[0], point[1]]),
      // Only the first series carries the markLine - one set of vertical
      // event annotations per chart, not one per series. No markLines (or
      // an empty list) means no markLine key at all, not an empty one.
      markLine: index === 0 && markLines && markLines.length > 0
        ? {
            symbol: "none",
            silent: false,
            label: { formatter: (p: { value?: unknown; name?: string }) => String(p.name ?? ""), position: "insideEndTop", color: colors.chartTick, fontSize: 10 },
            lineStyle: { type: "dashed", width: 1 },
            data: markLines.map((m) => ({ xAxis: m.time, name: m.label ?? "", lineStyle: { color: m.color ?? colors.chartTick } })),
          }
        : undefined,
    })),
  };
}

// spacedTimeLabels formats an axis's ascending ticks, leaving blank any tick
// closer than a fifth of the span to the last label shown. ECharts lays the
// ticks out in order on every pass, so a tick earlier than the last shown one
// starts the next pass.
export function spacedTimeLabels(span: number): (value: number) => string {
  const gap = span / 5;
  let shown = -Infinity;
  const pad = (n: number) => String(n).padStart(2, "0");
  return (value: number) => {
    if (value < shown) shown = -Infinity;
    if (value - shown < gap) return "";
    shown = value;
    const at = new Date(value);
    const time = `${pad(at.getHours())}:${pad(at.getMinutes())}`;
    return span > 86_400_000 ? `${pad(at.getMonth() + 1)}-${pad(at.getDate())} ${time}` : time;
  };
}

// onceEach formats axis ticks, leaving blank a tick whose label repeats the
// one below it. echarts formats an axis's ticks in order from index 0.
function onceEach(format: (value: number) => string) {
  let previous: string | undefined;
  return (value: number, index: number) => {
    const label = format(value);
    if (index === 0) previous = undefined;
    if (label === previous) return "";
    previous = label;
    return label;
  };
}
