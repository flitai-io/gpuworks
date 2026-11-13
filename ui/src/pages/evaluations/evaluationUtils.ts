import type { BenchmarkMeta, BenchmarkModeDefaults, BenchmarkPoint, BenchmarkPointSummary, BenchmarkRun } from "../../api.ts";
import type { TFunction } from "i18next";

export type BenchmarkMode = BenchmarkRun["mode"];

export function parseNumbers(raw?: string): number[] {
	return (raw || "").split(/[,，\s]+/).map(Number).filter((n) => Number.isInteger(n) && n > 0);
}

// parseRates reads a list of positive arrival rates (req/s); fractions allowed.
export function parseRates(raw?: string): number[] {
	return (raw || "").split(/[,，\s]+/).filter(Boolean).map(Number).filter((n) => Number.isFinite(n) && n > 0);
}

// interactivity is the per-user output speed in tok/s/user: 1000 / TPOT(ms),
// the rate a single streaming request receives tokens once decoding has
// started (InferenceMAX "interactivity"). TTFT is excluded by definition.
// Mirrors metrics.Interactivity on the server.
export function interactivity(tpotMillis?: number): number | undefined {
	return tpotMillis != null && Number.isFinite(tpotMillis) && tpotMillis > 0 ? 1000 / tpotMillis : undefined;
}

// isOpenLoop reports whether a point was driven by arrival rate.
export function isOpenLoop(point?: { request_rate?: number }) {
	return (point?.request_rate ?? 0) > 0;
}

// pointLoadLabel names a point's load: C=8 for closed loop, R=2 req/s (with
// its in-flight cap when one was set) for open loop.
export function pointLoadLabel(point: { concurrency: number; request_rate?: number }) {
	if (!isOpenLoop(point)) return `C=${point.concurrency}`;
	return `R=${format(point.request_rate, 2)} req/s${point.concurrency > 0 ? ` · C≤${point.concurrency}` : ""}`;
}

export function normalizeEndpointURL(raw: string): { url: string; strippedV1: boolean } {
	let url = raw.trim().replace(/\/+$/, "");
	const strippedV1 = url.endsWith("/v1");
	if (strippedV1) url = url.slice(0, -3).replace(/\/+$/, "");
	return { url, strippedV1 };
}

export function modeDefaults(meta: BenchmarkMeta, mode: BenchmarkMode): BenchmarkModeDefaults {
	return meta.mode_defaults[mode];
}

export function modeFormReset(meta: BenchmarkMeta, mode: BenchmarkMode) {
	const defaults = modeDefaults(meta, mode);
	return {
		...Object.fromEntries((meta.sla_constraints || []).map((field) => [field.key, undefined])),
		mode,
		concurrency: Array.isArray(defaults.concurrency) ? defaults.concurrency.join(",") : String(defaults.concurrency),
		max_concurrency: defaults.max_concurrency,
		isl: defaults.isl.join(","),
		osl: defaults.osl.join(","),
		requests_per_point_multiplier: defaults.requests_per_point_multiplier,
		min_throughput_gain: defaults.min_throughput_gain,
		objective: defaults.objective,
	};
}

export function initialBenchmarkDataset(sourceKind?: string) {
	return sourceKind || "random";
}

export function bestPoint(run?: BenchmarkRun): BenchmarkPoint | BenchmarkPointSummary | undefined {
	if (!run) return undefined;
	if (!run.points?.length) return run.best_point;
	const seq = run.verdict?.best_point_seq;
	const verdictPoint = run.points.find((point) => point.seq === seq);
	if (run.state !== "running" && run.state !== "pending") {
		if (run.verdict?.kind === "grid_done" && !seq) return undefined;
		return verdictPoint ?? run.best_point ?? bestSuccessfulOutput(run.points);
	}
	if (run.mode === "grid" || !run.mode) return bestMeasuredPoint(run, run.points);
	if (run.mode === "sla") {
		const completed = run.points.filter(isCompletedPoint);
		const feasible = completed.filter((point) => pointMeetsSLA(run, point));
		if (feasible.length) return [...feasible].sort((a, b) => objectiveValue(run, b) - objectiveValue(run, a))[0];
		return closestSLAPoint(run, completed);
	}
	return bestSuccessfulOutput(run.points) ?? verdictPoint ?? run.best_point;
}

function isCompletedPoint(point: BenchmarkPoint) {
	return !point.state || point.state === "done";
}

function bestSuccessfulOutput(points: BenchmarkPoint[]) {
	return [...points].filter((point) => isCompletedPoint(point) && point.success > 0).sort((a, b) => b.output_tput - a.output_tput)[0];
}

// Match search.BestMeasured: completed work satisfying the declared gates,
// then the configured objective, then lower arrival rate and concurrency.
function bestMeasuredPoint(run: BenchmarkRun, points: BenchmarkPoint[]) {
	return [...points].filter((point) => {
		const objective = objectiveValue(run, point);
		return isCompletedPoint(point) && point.success > 0 && Number.isFinite(objective) && objective > 0 && pointMeetsSLA(run, point);
	}).sort((a, b) => objectiveValue(run, b) - objectiveValue(run, a) || (a.request_rate ?? 0) - (b.request_rate ?? 0) || a.concurrency - b.concurrency)[0];
}

function objectiveValue(run: BenchmarkRun, point: BenchmarkPoint) {
	switch (String(run.search?.objective ?? "")) {
		case "rps": case "request_tps": return point.request_tput;
		case "total_tps": case "total_tok_s": return point.total_tput;
		default: return point.output_tput;
	}
}

// pointSLAValues mirrors search.SLAConfig.Violations on the server. The
// per_user_* entries keep the retired throughput ÷ concurrency definition for
// historical runs that stored those limits.
export function pointSLAValues(point: BenchmarkPoint): Record<string, number> {
	const evidence = point.long_tail?.evidence;
	const group = evidence?.version === 1 ? evidence.all : undefined;
	return {
		max_avg_ttft: group?.ttft.mean_ms ?? point.avg_ttft, max_p99_ttft: group?.ttft.p99_ms ?? point.p99_ttft,
		max_avg_tpot: group?.tpot.mean_ms ?? point.avg_tpot, max_p99_tpot: group?.tpot.p99_ms ?? point.p99_tpot,
		max_avg_latency: group?.last_output_latency.mean_ms ?? point.avg_latency, max_p99_latency: group?.last_output_latency.p99_ms ?? point.p99_latency,
		min_rps: point.request_tput, max_rps: point.request_tput,
		min_tps: point.output_tput, max_tps: point.output_tput,
		min_interactivity: interactivity(group?.tpot.p50_ms ?? point.p50_tpot) ?? 0,
		min_session_completion: point.long_tail?.evidence?.version === 1 && point.long_tail.evidence.plan.planned_sessions > 0 ? point.long_tail.evidence.outcomes.completed_sessions / point.long_tail.evidence.plan.planned_sessions : Number.NaN,
		min_per_user_total_tps: point.total_tput / Math.max(1, point.concurrency),
		max_per_user_total_tps: point.total_tput / Math.max(1, point.concurrency),
		min_per_user_output_tps: point.output_tput / Math.max(1, point.concurrency),
		max_per_user_output_tps: point.output_tput / Math.max(1, point.concurrency),
	};
}

export function pointSLAViolations(run: BenchmarkRun, point: BenchmarkPoint) {
	return Object.entries(pointSLAValues(point)).filter(([key, actual]) => {
		const limit = Number((run.search as Record<string, unknown> | undefined)?.[key] || 0);
		const e = point.long_tail?.evidence;
		let available = Number.isFinite(actual);
		const sample = key.includes("ttft") ? e?.all.ttft : key.includes("tpot") || key === "min_interactivity" ? e?.all.tpot : key.includes("latency") ? e?.all.last_output_latency : undefined;
		if (sample && e?.version === 1) available = available && sample.count > 0;
		else if (key.includes("ttft") || key.includes("tpot") || key.includes("latency") || key === "min_interactivity") available = actual > 0;
		return limit > 0 && (!available || (key.startsWith("max_") ? actual > limit : actual < limit));
	});
}

export function pointMeetsSLA(run: BenchmarkRun, point: BenchmarkPoint) {
	return pointSLAViolations(run, point).length === 0;
}

function closestSLAPoint(run: BenchmarkRun, points: BenchmarkPoint[]) {
	return [...points].sort((a, b) => pointSLAViolations(run, a).length - pointSLAViolations(run, b).length || b.concurrency - a.concurrency)[0];
}

type PointParameters = Pick<BenchmarkPoint, "concurrency" | "target_isl" | "target_osl"> & Partial<Pick<BenchmarkPoint, "actual_isl" | "actual_osl" | "request_rate" | "long_tail">>;

// Versioned distributions identify measured lengths, including real zero.
// Planned/live and historical points retain their original fallback contract.
export function pointTokenLength(point: PointParameters, kind: "input" | "output"): number | undefined {
	const evidence = point.long_tail?.evidence;
	if (evidence?.version === 1) {
		const distribution = evidence.all.tokens?.[kind];
		if (!distribution || !(distribution.count > 0)) return undefined;
		const value = distribution.total / distribution.count;
		return Number.isFinite(value) && value >= 0 ? value : undefined;
	}
	const actual = kind === "input" ? point.actual_isl : point.actual_osl;
	return actual && actual > 0 ? actual : kind === "input" ? point.target_isl : point.target_osl;
}

// Match CLI/report token-rate availability; request throughput is independent.
export function pointTokenRate(point: Pick<BenchmarkPoint, "long_tail" | "output_tput" | "total_tput">, field: "output_tput" | "total_tput"): number | undefined {
	const evidence = point.long_tail?.evidence;
	if (evidence?.version === 1 && evidence.all.success > 0
		&& (!(evidence.all.tokens?.input?.count > 0) || !(evidence.all.tokens?.output?.count > 0))) return undefined;
	return point[field];
}

export function pointParameterLabel(point?: PointParameters) {
	if (!point) return "";
	return `${pointLoadLabel(point)} · ISL=${format(pointTokenLength(point, "input"))} · OSL=${format(pointTokenLength(point, "output"))}`;
}

type TimingField = "avg_ttft" | "p50_ttft" | "p99_ttft" | "avg_tpot" | "p50_tpot" | "p99_tpot" | "avg_latency" | "p50_latency" | "p99_latency";

// Timing values and their availability come from the same sample set used by
// search and exports. A measured zero is valid; a zero-sample metric is unknown.
export function pointTimingValue(point: BenchmarkPoint, field: TimingField): number | undefined {
 const e = point.long_tail?.evidence;
 if (e?.version !== 1) return point[field];
 const summary = field.endsWith("ttft") ? e.all.ttft : field.endsWith("tpot") ? e.all.tpot : e.all.last_output_latency;
 if (!summary || !(summary.count > 0)) return undefined;
 const value = field.startsWith("avg") ? summary.mean_ms : field.startsWith("p50") ? summary.p50_ms : summary.p99_ms;
 return Number.isFinite(value) ? value : undefined;
}

function completedPointTTFT(point?: BenchmarkPoint | BenchmarkPointSummary): number | undefined {
	if (!point || ("state" in point && point.state && point.state !== "done")) return undefined;
	const fullPoint = point as BenchmarkPoint;
	const value = pointTimingValue(fullPoint, "p99_ttft");
	if (value == null || !Number.isFinite(value)) return undefined;
	return fullPoint.long_tail?.evidence?.version === 1 || value > 0 ? value : undefined;
}

export type MetricCardState = {
	kind: "waiting" | "warmup" | "live" | "formal";
	label: string;
	point?: PointParameters;
	values?: { outputTput?: number; totalTput?: number; requestTput: number; interactivity?: number; latencyP99?: number; ttftP50?: number; tpotP50?: number; successRate: number };
};

function tr(t: TFunction | undefined, key: string, fallback: string, params: Record<string, unknown> = {}) {
	if (t) return String(t(key, params));
	return fallback.replace(/{{(\w+)}}/g, (_match, name: string) => String(params[name] ?? ""));
}

export const benchmarkErrorCodes = ["BENCHMARK_SERVER_RESTARTED"] as const;

const knownBenchmarkErrorCodes = new Set<string>(benchmarkErrorCodes);

export function benchmarkErrorText(t: TFunction | undefined, value: string | undefined): string {
	const text = value?.trim() ?? "";
	if (!knownBenchmarkErrorCodes.has(text)) return text;
	return tr(t, `benchmarkError.${text}`, "The server restarted and interrupted the evaluation; completed points were retained.");
}

export function metricCardState(run: BenchmarkRun, t?: TFunction): MetricCardState {
	const terminal = run.state === "done" || run.state === "failed" || run.state === "canceled";
	if (terminal) {
		const point = bestPoint(run) as BenchmarkPoint | undefined;
		if (!point) return { kind: "waiting", label: tr(t, "utility.metric.waitingPoint", "Waiting for the first point to complete") };
		const total = point.success + point.errors;
		return { kind: "formal", label: tr(t, "utility.metric.formal", "Formal verdict-point metrics"), point, values: { outputTput: pointTokenRate(point, "output_tput"), totalTput: pointTokenRate(point, "total_tput"), requestTput: point.request_tput, interactivity: interactivity(pointTimingValue(point, "p50_tpot")), latencyP99: pointTimingValue(point, "p99_latency"), ttftP50: pointTimingValue(point, "p50_ttft"), tpotP50: pointTimingValue(point, "p50_tpot"), successRate: total ? point.success / total * 100 : 0 } };
	}
	const current = run.progress?.current_point;
	const currentPoint = current ? { concurrency: current.concurrency, target_isl: current.target_isl, target_osl: current.target_osl, request_rate: current.request_rate } : undefined;
	if (current?.warmup) return { kind: "warmup", label: tr(t, "utility.metric.warmup", "Warming up: {{count}} discarded requests", { count: current.request_total }), point: currentPoint };
	const sample = run.progress?.sample;
	const done = (sample?.success ?? 0) + (sample?.errors ?? 0);
	if (!current || !sample || done === 0) return { kind: "waiting", label: current ? tr(t, "utility.metric.waitingRequest", "Waiting for the first request to return") : tr(t, "utility.metric.waitingStart", "Waiting for the first point to start"), point: currentPoint };
	const liveLabel = tr(t, "utility.metric.live", "Current-point live metrics").split(" · ")[0];
	return { kind: "live", label: liveLabel, point: currentPoint, values: { outputTput: sample.output_tput, totalTput: sample.total_tput, requestTput: sample.request_tput, interactivity: interactivity(sample.tpot_p50), latencyP99: sample.latency_p99 || undefined, ttftP50: sample.ttft_p50, tpotP50: sample.tpot_p50, successRate: sample.success / done * 100 } };
}

export function benchmarkSeriesName(run: BenchmarkRun, labels: Record<string, string>, index: number, t?: TFunction) {
	const seq = Number(labels.point);
	const point = run.points?.find((candidate) => candidate.seq === seq);
	const current = run.progress?.current_point?.seq === seq ? run.progress.current_point : undefined;
	const config = point ?? current;
	if (!config) return tr(t, "common.point", "Point #{{value}}", { value: Number.isFinite(seq) && seq > 0 ? seq : index + 1 });
	return run.mode === "grid" ? pointParameterLabel(config) : pointLoadLabel(config);
}

export type VerdictPresentation = {
	tone: "success" | "warning" | "error" | "info";
	title: string;
	description: string;
	suggestion?: { label: string; maxConcurrency?: number };
};

const metricNames: Record<string, string> = {
	avg_ttft: "Average TTFT", p99_ttft: "TTFT P99", avg_tpot: "Average TPOT", p99_tpot: "TPOT P99",
	avg_latency: "Average Latency", p99_latency: "Latency P99", rps: "Request Throughput", tps: "Output Throughput",
	interactivity: "Interactivity", session_completion: "Session completion rate",
	per_user_total_tps: "Per-user Total Throughput (legacy)", per_user_output_tps: "Per-user Output Throughput (legacy)",
};

export function verdictPresentation(run: BenchmarkRun, t?: TFunction): VerdictPresentation {
	const verdict = run.verdict;
	const point = bestPoint(run);
	if (run.state === "pending") return { tone: "info", title: tr(t, "utility.verdict.pendingTitle", "Waiting to Start"), description: tr(t, "utility.verdict.pendingDescription", "The load task is queued and waiting for an executor.") };
	if (run.state === "running") {
		const progress = run.progress;
		if (run.mode === "grid") {
			return { tone: "info", title: tr(t, "utility.verdict.gridRunningTitle", "Parameter Scan in Progress"), description: tr(t, "utility.verdict.gridRunningDescription", "Completed {{done}}/{{total}} points · {{requests}} requests sent", { done: Math.max(0, (progress?.point_index ?? 1) - 1), total: progress?.point_total ?? "?", requests: progress?.completed ?? 0 }) };
		}
		return { tone: "info", title: tr(t, "utility.verdict.runningTitle", "Exploring the Concurrency Boundary"), description: tr(t, "utility.verdict.runningDescription", "Completed {{done}} points · exploring point {{current}} · {{requests}} requests sent", { done: Math.max(0, (progress?.point_index ?? 1) - 1), current: progress?.point_index ?? 1, requests: progress?.completed ?? 0 }) };
	}
	if (run.state === "canceled") return { tone: "warning", title: tr(t, "utility.verdict.canceledTitle", "Evaluation Canceled"), description: tr(t, "utility.verdict.canceledDescription", "No new requests will be sent; completed points remain available.") };
	if (run.state === "failed" || verdict?.kind === "failed") {
		return { tone: "error", title: run.points?.length ? tr(t, "utility.verdict.partialTitle", "Evaluation Partially Complete") : tr(t, "utility.verdict.failedTitle", "Evaluation Failed"), description: benchmarkErrorText(t, run.error) || tr(t, "utility.verdict.failedDescription", "An error occurred during execution; completed points were retained.") };
	}

	switch (verdict?.kind) {
		case "saturated": {
			const threshold = Number(run.search?.min_throughput_gain ?? .05) * 100;
			return { tone: "success", title: tr(t, "utility.verdict.saturatedTitle", "Concurrency {{concurrency}} is the saturation point", { concurrency: verdict.saturation_concurrency }), description: tr(t, "utility.verdict.saturatedDescription", "Throughput gain falls below {{threshold}}% at higher concurrency. Recommended capacity is about {{rps}} req/s per instance, with TTFT P99 {{ttft}} ms.", { threshold: threshold.toFixed(0), rps: format(point?.request_tput), ttft: format(completedPointTTFT(point)) }) };
		}
		case "truncated": {
			const current = verdict.saturation_concurrency || Math.max(1, ...((run.points ?? []).map((p) => p.concurrency)));
			return { tone: "warning", title: tr(t, "utility.verdict.truncatedTitle", "No Saturation Point Found"), description: tr(t, "utility.verdict.truncatedDescription", "Throughput was still rising (+{{headroom}}%) at concurrency limit {{current}}; results were truncated by the configured limit.", { headroom: format(verdict.headroom_pct), current }), suggestion: { label: tr(t, "utility.verdict.rerun", "Run again with limit {{limit}}", { limit: current * 2 }), maxConcurrency: current * 2 } };
		}
		case "sla_met":
			return { tone: "success", title: tr(t, "utility.verdict.slaMetTitle", "Maximum compliant concurrency is {{concurrency}}", { concurrency: verdict.sla_max_concurrency }), description: tr(t, "utility.verdict.slaMetDescription", "Output throughput is {{output}} tok/s and request throughput is {{request}} req/s at this point.", { output: format(point?.output_tput), request: format(point?.request_tput) }) };
		case "sla_none": {
			const violation = verdict.violated?.[0];
			const over = violation && violation.limit ? Math.abs(violation.actual / violation.limit - 1) * 100 : undefined;
			const metric = metricNames[violation?.metric ?? ""] ? tr(t, `utility.metricName.${violation?.metric}`, metricNames[violation?.metric ?? ""]) : violation?.metric;
			const violationValue = (value: number) => violation?.metric === "session_completion" ? `${format(value * 100, 2)}%` : format(value);
			return { tone: "warning", title: tr(t, "utility.verdict.slaNoneTitle", "No Point Meets All Constraints"), description: violation?.reason === "evidence_unavailable" ? tr(t, "evidence.slaMissing", "Insufficient evidence for {{metric}}.", { metric }) : violation ? tr(t, "utility.verdict.slaViolation", "Closest failure: {{metric}}, actual {{actual}}, limit {{limit}} (deviation {{deviation}}%).", { metric, actual: violationValue(violation.actual), limit: violationValue(violation.limit), deviation: format(over) }) : tr(t, "utility.verdict.slaRetry", "Relax constraints or lower the initial concurrency and retry.") };
		}
		case "grid_done": {
			if (!verdict.best_point_seq) return { tone: "warning", title: tr(t, "utility.verdict.slaNoneTitle", "No Point Meets All Constraints"), description: tr(t, "evidence.noEligible", "No measured point has sufficient evidence and meets the configured constraints.") };
			const latencyBest = [...(run.points ?? [])].filter((p) => p.success > 0 && completedPointTTFT(p) != null).sort((a, b) => completedPointTTFT(a)! - completedPointTTFT(b)!)[0];
			return { tone: "success", title: tr(t, "utility.verdict.gridDoneTitle", "Parameter scan complete with {{count}} points", { count: run.points?.length ?? 0 }), description: tr(t, "utility.verdict.gridDoneDescription", "Best measured point in this run: {{throughputLoad}} × ISL {{isl}}, output {{throughput}} tok/s; best measured TTFT P99 at {{latencyLoad}}: {{ttft}} ms.", { throughputLoad: point ? pointLoadLabel(point) : "-", isl: point?.target_isl ?? "-", throughput: format(point?.output_tput), latencyLoad: latencyBest ? pointLoadLabel(latencyBest) : "-", ttft: format(completedPointTTFT(latencyBest)) }) };
		}
	}
	return { tone: "info", title: tr(t, "utility.verdict.doneTitle", "Evaluation Complete"), description: tr(t, "utility.verdict.legacyDescription", "This legacy Run has no verdict; point data remains available.") };
}

export function estimateRequests(mode: BenchmarkMode, concurrency: number[], multiplier: number, maxConcurrency?: number, islCount = 1, oslCount = 1) {
	if (mode === "grid") return concurrency.reduce((sum, value) => sum + value * multiplier, 0) * Math.max(1, islCount) * Math.max(1, oslCount);
	const start = concurrency[0] || 1;
	const max = Math.max(start, maxConcurrency || start);
	let low = 0, high = 0;
	for (let value = start; value <= max; value *= 2) {
		high += value * multiplier;
		if (value > max / 2) break;
	}
	low = start * multiplier;
	return { low, high: high * 2 };
}

// estimateOpenLoop sizes an open-loop grid: every rate point sends the
// arrivals of one point duration (at least the server's floor), and lasts
// about that long.
export function estimateOpenLoop(rates: number[], durationSeconds: number, minRequests: number, islCount = 1, oslCount = 1) {
	const lengths = Math.max(1, islCount) * Math.max(1, oslCount);
	const requests = rates.reduce((sum, rate) => sum + Math.max(minRequests, Math.ceil(rate * durationSeconds)), 0) * lengths;
	const seconds = rates.reduce((sum, rate) => sum + Math.max(durationSeconds, minRequests / rate), 0) * lengths;
	return { requests, minutes: Math.max(1, Math.ceil(seconds / 60)) };
}

export function format(value?: number, digits = 1) {
	return value == null || !Number.isFinite(value) ? "—" : value.toLocaleString(undefined, { maximumFractionDigits: digits });
}

export function targetLabel(run: BenchmarkRun, deploymentName?: string, t?: TFunction) {
	if (run.target_kind === "instance") return deploymentName || tr(t, "utility.targetDeployment", "Deployment #{{id}}", { id: run.deployment_id });
	try { return new URL(run.base_url || "").host || run.base_url || tr(t, "common.external", "External endpoint"); } catch { return run.base_url || tr(t, "common.external", "External endpoint"); }
}

export function deletedTargetLabelKeys(run: BenchmarkRun): Array<"detail.target.deleted" | "detail.target.instanceDeleted"> {
	const labels: Array<"detail.target.deleted" | "detail.target.instanceDeleted"> = [];
	if (run.deployment_deleted) labels.push("detail.target.deleted");
	if (run.instance_deleted) labels.push("detail.target.instanceDeleted");
	return labels;
}
