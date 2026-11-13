import { init, use } from "echarts/core";
import { LineChart, ScatterChart } from "echarts/charts";
import { GridComponent, LegendComponent, TooltipComponent } from "echarts/components";
import { CanvasRenderer } from "echarts/renderers";
import { buildTimeSeriesOption } from "../../components/timeSeriesOption";
import { pareto as en } from "../../i18n/locales/en/evaluations.json";
import { pareto as zh } from "../../i18n/locales/zh/evaluations.json";
import { benchmarkCurveSeries, type CurveField, type CurveMetric } from "./benchmarkCurveSeries";
import { buildParetoOption } from "./paretoOption";
import type { ParetoDatum } from "./paretoUtils";
import { format } from "./evaluationUtils";

export interface ReportChartData {
	runID: number;
	curves: Parameters<typeof benchmarkCurveSeries>[0];
	metrics: CurveMetric[];
	metric: CurveField;
	names: Record<string, string>;
	pareto: ParetoDatum[];
	selectedSeq?: number;
	legend?: { selected?: Record<string, boolean>; scrollDataIndex?: number };
}

use([LineChart, ScatterChart, GridComponent, LegendComponent, TooltipComponent, CanvasRenderer]);

function mountChart(host: HTMLElement, kind: string) {
	host.removeAttribute("_echarts_instance_");
	host.replaceChildren();
	host.classList.add(kind);
	const chart = init(host, undefined, { renderer: "canvas" });
	const resize = () => { if (host.clientWidth && host.clientHeight) chart.resize(); };
	if (typeof ResizeObserver === "function") new ResizeObserver(resize).observe(host);
	else window.addEventListener("resize", resize);
	return chart;
}

function mountCurves(data: ReportChartData) {
	const card = document.querySelector("[data-report-curve]");
	const host = card?.querySelector<HTMLElement>("[_echarts_instance_], [data-offline-chart]");
	if (!card || !host || !data.curves.length) return;
	const chart = mountChart(host, "native-curve");
	function draw(field: CurveField, initial = false) {
		const series = benchmarkCurveSeries(data.curves, data.runID, field);
		const option = buildTimeSeriesOption(series, {
			unit: data.metrics.find(metric => metric.value === field)?.unit,
			seriesName: labels => data.names[labels.point], connectNulls: false,
		});
		if (initial && data.legend) Object.assign(option.legend!, data.legend);
		chart.setOption(option, { notMerge: true });
	}
	for (const input of card.querySelectorAll<HTMLInputElement>('input[type="radio"]')) {
		input.checked = input.value === data.metric;
		input.addEventListener("change", () => {
			const metric = data.metrics.find(item => item.value === input.value);
			if (input.checked && metric) draw(metric.value);
		});
	}
	draw(data.metric, true);
}

function mountPareto(data: ReportChartData) {
	const card = document.querySelector("[data-report-pareto]");
	const host = card?.querySelector<HTMLElement>("[_echarts_instance_], [data-offline-chart]");
	if (!host || !data.pareto.length) return;
	const chart = mountChart(host, "native-pareto");
	const labels = document.documentElement.lang.startsWith("zh") ? zh : en;
	function select(point: ParetoDatum, interactive: boolean) {
		chart.setOption(buildParetoOption(data.pareto, point.seq, labels));
		if (!interactive) return;
		const values = [`#${point.seq} · ${point.loadLabel}`, `${format(point.x)} tok/s/user`, `${format(point.y)} tok/s`, `${format(point.successRate)}%`];
		card?.querySelectorAll<HTMLElement>(".ant-descriptions-item-content").forEach((cell, i) => {
			if (values[i] !== undefined) cell.textContent = values[i];
		});
		for (const row of document.querySelectorAll<HTMLElement>("#all-points tr[data-seq]")) {
			row.toggleAttribute("data-offline-selected", Number(row.dataset.seq) === point.seq);
		}
	}
	select(data.pareto.find(point => point.seq === data.selectedSeq) ?? data.pareto[0], false);
	chart.on("click", params => {
		const point = data.pareto.find(item => item.seq === (params.data as ParetoDatum)?.seq);
		if (point) select(point, true);
	});
	document.getElementById("all-points")?.addEventListener("click", event => {
		if (!(event.target instanceof Element)) return;
		const row = event.target.closest<HTMLElement>("tr[data-seq]");
		const point = data.pareto.find(item => item.seq === Number(row?.dataset.seq));
		if (point) select(point, true);
	});
}

const payload = document.getElementById("benchmark-chart-data");
if (payload?.textContent && document.createElement("canvas").getContext("2d")) {
	const data: ReportChartData = JSON.parse(payload.textContent);
	mountCurves(data);
	mountPareto(data);
}
