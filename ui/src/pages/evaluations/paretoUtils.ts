import type { BenchmarkPoint } from "../../api";
import { pareto as english } from "../../i18n/locales/en/evaluations.json";
import { format, interactivity, pointParameterLabel, pointTimingValue, pointTokenRate } from "./evaluationUtils.ts";

export type ParetoLabels = typeof english;
export interface ParetoDatum {
	seq: number; x: number; y: number; loadLabel: string; successRate?: number;
	total?: number; request?: number; ttft?: number; tpot?: number; latency?: number;
}

export function paretoData(points: BenchmarkPoint[]): ParetoDatum[] {
	return points.flatMap(point => {
		const tpot = pointTimingValue(point, "p50_tpot");
		const x = interactivity(tpot);
		const y = pointTokenRate(point, "output_tput");
		const requests = point.success + point.errors;
		return x == null || y == null ? [] : [{
			seq: point.seq, x, y, loadLabel: pointParameterLabel(point), successRate: requests ? point.success / requests * 100 : 0,
			total: pointTokenRate(point, "total_tput"), request: point.request_tput, ttft: pointTimingValue(point, "p99_ttft"),
			tpot, latency: pointTimingValue(point, "p99_latency"),
		}];
	});
}

export function paretoTooltip(d: ParetoDatum, labels = english.tooltip) {
	const label = d.loadLabel.replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;");
	return [
		`<b>${labels.point.replace("{{value}}", String(d.seq))}</b> · ${label}`,
		`${labels.interactivity}: ${format(d.x)} tok/s/user`,
		`${labels.average}: ${format(d.y)} tok/s`,
		`${labels.total}: ${format(d.total)} tok/s`,
		`${labels.request}: ${format(d.request)} req/s`,
		`TTFT P99: ${format(d.ttft)} ms`,
		`TPOT P50: ${format(d.tpot)} ms`,
		`${labels.latency}: ${format(d.latency)} ms`,
		`${labels.success}: ${format(d.successRate)}%`,
	].join("<br/>");
}
