import { useEffect, useMemo, useRef } from "react";
import { Descriptions, Empty } from "antd";
import * as echarts from "echarts/core";
import { LineChart, ScatterChart } from "echarts/charts";
import { GridComponent, TooltipComponent } from "echarts/components";
import { CanvasRenderer } from "echarts/renderers";
import { buildParetoOption } from "./paretoOption";
import type { ECElementEvent } from "echarts/core";
import { useTranslation } from "react-i18next";
import type { BenchmarkPoint } from "../../api";
import { colors } from "../../theme/tokens";
import { format } from "./evaluationUtils";
import { paretoData, type ParetoLabels } from "./paretoUtils";

echarts.use([LineChart, ScatterChart, GridComponent, TooltipComponent, CanvasRenderer]);

const CHART_HEIGHT = 192;

export default function ParetoChart({ points, selectedSeq, onSelect }: { points: BenchmarkPoint[]; selectedSeq?: number; onSelect?: (point: BenchmarkPoint) => void }) {
	const { t } = useTranslation("evaluations");
	const hostRef = useRef<HTMLDivElement>(null);
	const chartRef = useRef<ReturnType<typeof echarts.init> | null>(null);
	const selectRef = useRef(onSelect);
	selectRef.current = onSelect;
	const pointsRef = useRef(points);
	pointsRef.current = points;
	const plotted = useMemo(() => paretoData(points), [points]);
	const selected = plotted.find((datum) => datum.seq === selectedSeq) ?? plotted[0];

	useEffect(() => {
		if (!hostRef.current) return;
		const chart = echarts.init(hostRef.current, undefined, { renderer: "canvas" });
		chartRef.current = chart;
		const click = (params: ECElementEvent) => {
			const datum = params.data as { seq?: number } | null | undefined;
			const point = pointsRef.current.find((candidate) => candidate.seq === datum?.seq);
			if (point) selectRef.current?.(point);
		};
		chart.on("click", click);
		const resize = () => chart.resize();
		const observer = new ResizeObserver(resize);
		observer.observe(hostRef.current);
		window.addEventListener("resize", resize);
		return () => {
			observer.disconnect();
			window.removeEventListener("resize", resize);
			chart.off("click", click);
			chart.dispose();
			chartRef.current = null;
		};
	// The click handler resolves against the latest point list through option data;
	// selection callbacks are kept in a ref so the chart instance stays stable.
	}, []);

	useEffect(() => {
		const chart = chartRef.current;
		if (!chart) return;
		if (!plotted.length) {
			chart.clear();
			return;
		}
		const option = buildParetoOption(plotted, selected?.seq, t("pareto", { returnObjects: true }) as ParetoLabels);
		chart.setOption(option, { notMerge: false, lazyUpdate: true, replaceMerge: ["series"] });
	}, [plotted, selected?.seq, t]);

	return <div style={{ position: "relative" }}>
		<div ref={hostRef} role="img" aria-label={t("pareto.aria")} style={{ width: "100%", height: CHART_HEIGHT }} />
		{!selected && <div style={{ position: "absolute", inset: "0 0 auto", height: CHART_HEIGHT, background: colors.white }}><Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description={t("pareto.empty")} /></div>}
		{selected && <Descriptions size="small" column={2} style={{ marginTop: 8 }} items={[
			{ key: "point", label: t("pareto.selected"), children: `#${selected.seq} · ${selected.loadLabel}`, span: 2 },
			{ key: "x", label: t("pareto.interactivity"), children: `${format(selected?.x)} tok/s/user` },
			{ key: "y", label: t("pareto.average"), children: `${format(selected?.y)} tok/s` },
			{ key: "success", label: t("pareto.success"), children: `${format(selected.successRate)}%` },
		]} />}
	</div>;
}
