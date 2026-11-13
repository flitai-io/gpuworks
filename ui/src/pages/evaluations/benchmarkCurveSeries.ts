import type { BenchmarkCurveResponse, BenchmarkCurveSample, MetricPoint, MetricSeries } from "../../api";

export type CurveField = Exclude<keyof BenchmarkCurveSample, "t">;

export interface CurveMetric {
	value: CurveField;
	label: string;
	unit: string;
}

export function benchmarkCurveSeries(points: Pick<BenchmarkCurveResponse["points"][number], "seq" | "samples">[], runID: number, field: CurveField): MetricSeries[] {
	return points.map(point => ({
		labels: { benchmark: String(runID), point: String(point.seq) },
		points: (point.samples ?? []).flatMap((sample): MetricPoint[] => {
			const value = sample?.[field];
			return sample?.t > 0 && Number.isFinite(sample.t) ? [[sample.t, value != null && Number.isFinite(value) ? value : Number.NaN]] : [];
		}),
	})).filter(series => series.points.length > 0);
}
