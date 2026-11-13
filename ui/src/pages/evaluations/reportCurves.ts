import type { CurveMetric } from "./benchmarkCurveSeries";
import type { ReportChartData } from "./standaloneCharts";
import { reportElement as el, reportText } from "./reportLanguage";

export function processSamples(data: ReportChartData, metrics: CurveMetric[], zh: boolean) {
	const details = el("details");
	details.append(el("summary", reportText("Process samples", zh)));
	for (const p of data.curves || []) {
		const group = el("details"), table = el("table"), head = table.createTHead().insertRow();
		head.append(el("th", "UTC"));
		for (const metric of metrics) head.append(el("th", `${metric.label} (${metric.unit})`));
		const body = table.createTBody();
		for (const sample of p.samples || []) {
			if (!sample) continue;
			const date = new Date(sample.t), row = body.insertRow();
			row.append(el("th", sample.t > 0 && Number.isFinite(date.getTime()) ? date.toISOString() : "—"));
			for (const {value} of metrics) row.insertCell().textContent = sample[value] == null ? "—" : String(sample[value]);
		}
		group.append(el("summary", reportText("Point", zh) + " #" + p.seq), table);
		details.append(group);
	}
	return details;
}
