import type { EChartsOption } from "echarts";
import { colors } from "../../theme/tokens";
import { paretoTooltip, type ParetoDatum, type ParetoLabels } from "./paretoUtils";

export function buildParetoOption(plotted: ParetoDatum[], selectedSeq: number | undefined, labels: ParetoLabels): EChartsOption {
	const ordered = [...plotted].sort((a, b) => a.x - b.x);
	return {
		animation: false,
		grid: { top: 20, right: 20, bottom: 52, left: 72 },
		tooltip: { trigger: "item", confine: true, formatter: (params: unknown) => paretoTooltip((params as { data: ParetoDatum }).data, labels.tooltip) },
		xAxis: { type: "value", min: 0, name: labels.xAxis, nameLocation: "middle", nameGap: 34, axisLabel: { color: colors.chartTick }, axisLine: { lineStyle: { color: colors.chartGrid } }, splitLine: { show: true, lineStyle: { color: colors.chartGrid, type: "dashed" } } },
		yAxis: { type: "value", min: 0, name: labels.yAxis, nameLocation: "middle", nameGap: 52, axisLabel: { color: colors.chartTick }, axisLine: { show: false }, splitLine: { show: true, lineStyle: { color: colors.chartGrid, type: "dashed" } } },
		series: [
			{ id: "pareto-line", type: "line", silent: true, showSymbol: false, data: ordered.map((datum) => [datum.x, datum.y]), lineStyle: { color: colors.chartPrimary, width: 2 } },
			{ id: "pareto-points", type: "scatter", data: plotted.map((datum) => ({ ...datum, value: [datum.x, datum.y], symbolSize: datum.seq === selectedSeq ? 16 : 10, itemStyle: { color: datum.seq === selectedSeq ? colors.warning : colors.chartPrimary, borderColor: colors.white, borderWidth: 2 } })), emphasis: { scale: 1.35 } },
		],
	};
}
