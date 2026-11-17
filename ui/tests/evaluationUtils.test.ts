import assert from "node:assert/strict";
import test from "node:test";

import { benchmarkSeriesName, bestPoint, deletedTargetLabelKeys, estimateOpenLoop, estimateRequests, initialBenchmarkDataset, interactivity, metricCardState, modeFormReset, normalizeEndpointURL, parseNumbers, parseRates, pointLoadLabel, pointMeetsSLA, pointParameterLabel, pointSLAValues, pointSLAViolations, pointTimingValue, verdictPresentation } from "../src/pages/evaluations/evaluationUtils.ts";
import type { BenchmarkMeta, BenchmarkPoint, BenchmarkRun } from "../src/api.ts";

test("parseNumbers accepts Chinese separators and rejects invalid values", () => {
	assert.deepEqual(parseNumbers("1, 2，4 0 -1 nope 3.5"), [1, 2, 4]);
});

test("parseNumbers treats hidden optional fields as empty", () => {
	assert.deepEqual(parseNumbers(undefined), []);
});

test("endpoint normalization removes a trailing v1 exactly once", () => {
	assert.deepEqual(normalizeEndpointURL(" http://host:3000/v1/ "), { url: "http://host:3000", strippedV1: true });
	assert.deepEqual(normalizeEndpointURL("http://host:3000/openai"), { url: "http://host:3000/openai", strippedV1: false });
});

test("switching modes resets all mode-sensitive load fields from server metadata", () => {
	const meta = {
		mode_defaults: {
			grid: { concurrency: [1, 2, 4], isl: [128, 512], osl: [32], requests_per_point_multiplier: 10 },
			saturation: { concurrency: 2, max_concurrency: 64, isl: [256], osl: [64], requests_per_point_multiplier: 6, min_throughput_gain: .08 },
			sla: { concurrency: 1, max_concurrency: 32, isl: [128], osl: [32], requests_per_point_multiplier: 10, objective: "rps" },
		},
	} as BenchmarkMeta;
	assert.deepEqual(modeFormReset(meta, "saturation"), { mode: "saturation", concurrency: "2", max_concurrency: 64, isl: "256", osl: "64", requests_per_point_multiplier: 6, min_throughput_gain: .08, objective: undefined });
});

test("new evaluations keep fixed-length random as the default dataset", () => {
	assert.equal(initialBenchmarkDataset(), "random");
	assert.equal(initialBenchmarkDataset("agentic"), "agentic");
});

test("grid request estimate includes every ISL and OSL combination", () => {
	assert.equal(estimateRequests("grid", [1, 2], 1, undefined, 2, 2), 12);
});

test("list verdicts use the lightweight persisted best-point summary", () => {
	const run = { verdict: { kind: "grid_done", best_point_seq: 2 }, best_point: { seq: 2, output_tput: 25 } } as BenchmarkRun;
	assert.equal(bestPoint(run)?.output_tput, 25);
});

test("truncated verdict is honest and offers a doubled search limit", () => {
	const run = { state: "done", mode: "saturation", verdict: { kind: "truncated", saturation_concurrency: 32, headroom_pct: 22 }, points: [{ seq: 1, concurrency: 32, output_tput: 638 }] } as BenchmarkRun;
	const presentation = verdictPresentation(run);
	assert.equal(presentation.title, "No Saturation Point Found");
	assert.match(presentation.description, /rising.*22%/);
	assert.deepEqual(presentation.suggestion, { label: "Run again with limit 64", maxConcurrency: 64 });
});

test("SLA verdict names the persisted maximum feasible concurrency", () => {
	const run = { state: "done", mode: "sla", verdict: { kind: "sla_met", best_point_seq: 1, sla_max_concurrency: 11 }, points: [{ seq: 1, concurrency: 11, output_tput: 200, request_tput: 8 }] } as BenchmarkRun;
	assert.equal(verdictPresentation(run).title, "Maximum compliant concurrency is 11");
});

test("running grid and saturation select the highest successful output point", () => {
	for (const mode of ["grid", "saturation"] as const) {
		const run = { state: "running", mode, points: [{ seq: 1, success: 1, output_tput: 20 }, { seq: 2, success: 2, output_tput: 35 }, { seq: 3, success: 0, output_tput: 99 }] } as BenchmarkRun;
		assert.equal(bestPoint(run)?.seq, 2);
	}
});

test("running SLA selects best objective among feasible points or nearest violation", () => {
	const feasible = { state: "running", mode: "sla", search: { max_p99_ttft: 100, objective: "rps" }, points: [{ seq: 1, concurrency: 1, success: 1, p99_ttft: 90, request_tput: 2 }, { seq: 2, concurrency: 2, success: 1, p99_ttft: 80, request_tput: 4 }] } as BenchmarkRun;
	assert.equal(bestPoint(feasible)?.seq, 2);
	const none = { ...feasible, points: [{ seq: 1, concurrency: 1, success: 1, p99_ttft: 120 }, { seq: 2, concurrency: 4, success: 1, p99_ttft: 130 }] } as BenchmarkRun;
	assert.equal(bestPoint(none)?.seq, 2);
});

test("live cards use current cumulative sample and terminal cards use verdict point", () => {
	const running = { state: "running", mode: "grid", progress: { completed: 3, current_point: { seq: 2, concurrency: 4, target_isl: 64, target_osl: 16, request_total: 8 }, sample: { output_tput: 40, total_tput: 50, request_tput: 2, ttft_p50: 10, tpot_p50: 4, latency_p99: 900, success: 2, errors: 1 } } } as BenchmarkRun;
	const liveState = metricCardState(running);
	assert.equal(liveState.label, "Current-point live metrics");
	const live = liveState.values!;
	// Interactivity is 1000 / TPOT P50, not throughput ÷ concurrency (10 here).
	assert.deepEqual({ ...live, successRate: undefined }, { outputTput: 40, totalTput: 50, requestTput: 2, interactivity: 250, latencyP99: 900, ttftP50: 10, tpotP50: 4, successRate: undefined });
	assert.ok(Math.abs(live.successRate - 200 / 3) < 1e-10);
	const waiting = { ...running, progress: { ...running.progress, sample: { ...running.progress!.sample!, success: 0, errors: 0 } } } as BenchmarkRun;
	assert.equal(metricCardState(waiting).kind, "waiting");
	const done = { state: "done", mode: "grid", verdict: { kind: "grid_done", best_point_seq: 1 }, points: [{ seq: 1, concurrency: 2, success: 3, errors: 1, output_tput: 20, total_tput: 24, request_tput: 1, p50_ttft: 5, p50_tpot: 2 }] } as BenchmarkRun;
	assert.equal(metricCardState(done).kind, "formal");
	assert.equal(metricCardState(done).values?.successRate, 75);
	assert.equal(metricCardState(done).values?.interactivity, 500);
	assert.equal(metricCardState(done).values?.latencyP99, undefined, "a point without E2E latency shows none rather than 0");
});

test("warmup is its own card state and names the discarded requests", () => {
	const warming = { state: "running", mode: "grid", progress: { current_point: { seq: 0, concurrency: 1, target_isl: 64, target_osl: 16, request_total: 3, warmup: true } } } as BenchmarkRun;
	const state = metricCardState(warming);
	assert.equal(state.kind, "warmup");
	assert.equal(state.label, "Warming up: 3 discarded requests");
	assert.equal(state.values, undefined);
});

test("interactivity is the inverse of TPOT and absent without one", () => {
	assert.equal(interactivity(20), 50);
	assert.equal(interactivity(0), undefined);
	assert.equal(interactivity(undefined), undefined);
	assert.equal(interactivity(Number.NaN), undefined);
});

test("open-loop points are named by arrival rate and in-flight cap", () => {
	const point = { concurrency: 0, request_rate: 2.5, target_isl: 128, target_osl: 32 } as BenchmarkPoint;
	assert.equal(pointLoadLabel(point), "R=2.5 req/s");
	assert.equal(pointLoadLabel({ ...point, concurrency: 16 }), "R=2.5 req/s · C≤16");
	assert.equal(pointParameterLabel(point), "R=2.5 req/s · ISL=128 · OSL=32");
	assert.equal(pointLoadLabel({ concurrency: 4 }), "C=4");
	const grid = { mode: "grid", state: "done", verdict: { kind: "grid_done", best_point_seq: 2 }, points: [{ seq: 1, success: 1, concurrency: 0, request_rate: 1, target_isl: 8, output_tput: 10, p99_ttft: 50 }, { seq: 2, success: 1, concurrency: 0, request_rate: 4, target_isl: 8, output_tput: 30, p99_ttft: 90 }] } as BenchmarkRun;
	assert.equal(verdictPresentation(grid).description, "Best measured point in this run: R=4 req/s × ISL 8, output 30 tok/s; best measured TTFT P99 at R=1 req/s: 50 ms.");
});

test("parseRates accepts fractional rates and drops invalid entries", () => {
	assert.deepEqual(parseRates("0.5, 2，4 x -1 0"), [0.5, 2, 4]);
	assert.deepEqual(parseRates(""), []);
});

test("open-loop estimate counts rate × duration with the request floor", () => {
	// 0.1 req/s for 60 s is 6 arrivals, raised to the floor of 20 (200 s);
	// 2 req/s gives 120 requests over 60 s.
	assert.deepEqual(estimateOpenLoop([0.1, 2], 60, 20), { requests: 140, minutes: 5 });
	assert.deepEqual(estimateOpenLoop([2], 60, 20, 2, 1), { requests: 240, minutes: 2 });
});

test("SLA values judge interactivity by TPOT P50 and keep the legacy per-user keys", () => {
	const point = { concurrency: 4, output_tput: 36, total_tput: 240, p50_tpot: 25 } as BenchmarkPoint;
	const values = pointSLAValues(point);
	assert.equal(values.min_interactivity, 40);
	assert.equal(values.min_per_user_output_tps, 9);
	const run = { mode: "sla", search: { min_interactivity: 45 } } as BenchmarkRun;
	assert.deepEqual(pointSLAViolations(run, point).map(([key]) => key), ["min_interactivity"]);
	assert.equal(pointMeetsSLA({ ...run, search: { min_interactivity: 40 } } as BenchmarkRun, point), true);
});

test("completed point labels prefer measured token lengths", () => {
	const point = { concurrency: 1, target_isl: 10_000, target_osl: 220, actual_isl: 10_526.288, actual_osl: 220 } as BenchmarkPoint;
	assert.equal(pointParameterLabel(point), "C=1 · ISL=10,526.3 · OSL=220");
	assert.equal(pointParameterLabel({ ...point, actual_isl: 0, actual_osl: 0 }), "C=1 · ISL=10,000 · OSL=220");
});

test("benchmark curve legend resolves mode parameters and old data fallback", () => {
	const grid = { mode: "grid", points: [{ seq: 3, concurrency: 4, target_isl: 64, target_osl: 16 }] } as BenchmarkRun;
	assert.equal(benchmarkSeriesName(grid, { point: "3", node: "server" }, 0), "C=4 · ISL=64 · OSL=16");
	assert.equal(benchmarkSeriesName({ ...grid, mode: "saturation" }, { point: "3", node: "server" }, 0), "C=4");
	assert.equal(benchmarkSeriesName(grid, { point: "9", node: "server" }, 0), "Point #9");
	const open = { mode: "grid", points: [{ seq: 1, concurrency: 0, request_rate: 3, target_isl: 64, target_osl: 16 }] } as BenchmarkRun;
	assert.equal(benchmarkSeriesName(open, { point: "1", node: "server" }, 0), "R=3 req/s · ISL=64 · OSL=16");
});


test("session completion and missing timing samples gate the same points as the server", () => {
 const point = { concurrency: 1, success: 39, errors: 1, p99_ttft: 10, long_tail: { evidence: { version: 1, plan: { planned_sessions: 4 }, outcomes: { completed_sessions: 3 }, all: { ttft: { count: 0 }, tpot: { count: 0 }, last_output_latency: { count: 0 } } } } } as unknown as BenchmarkPoint;
 assert.equal(pointMeetsSLA({ search: { min_session_completion: 0.95 } } as BenchmarkRun, point), false);
 assert.equal(pointMeetsSLA({ search: { min_session_completion: 0.75 } } as BenchmarkRun, point), true);
 assert.equal(pointMeetsSLA({ search: { max_p99_ttft: 100 } } as BenchmarkRun, point), false);
 const legacy = { ...point, long_tail: undefined };
 assert.equal(pointMeetsSLA({ search: { max_p99_ttft: 100 } } as BenchmarkRun, legacy), true);
 assert.equal(pointMeetsSLA({ search: { min_session_completion: 0.5 } } as BenchmarkRun, legacy), false);
 const run = { state: "done", mode: "grid", verdict: { kind: "grid_done" }, points: [point] } as BenchmarkRun;
 assert.equal(bestPoint(run), undefined);
 assert.match(verdictPresentation(run).description, /No measured point/);
});


test("versioned evidence values override incompatible legacy timing aggregates", () => {
 const point = { avg_latency: 10, p99_latency: 10, p50_tpot: 1, long_tail: { evidence: { version: 1, plan: { planned_sessions: 1 }, outcomes: { completed_sessions: 1 }, all: { ttft: { count: 1, mean_ms: 1, p99_ms: 1 }, tpot: { count: 1, mean_ms: 20, p50_ms: 20, p99_ms: 25 }, last_output_latency: { count: 1, mean_ms: 200, p99_ms: 250 } } } } } as unknown as BenchmarkPoint;
 assert.equal(pointSLAValues(point).max_avg_latency, 200);
 assert.equal(pointSLAValues(point).min_interactivity, 50);
 assert.equal(pointMeetsSLA({ search: { max_avg_latency: 100 } } as BenchmarkRun, point), false);
 assert.equal(pointMeetsSLA({ search: { min_interactivity: 100 } } as BenchmarkRun, point), false);
});


test("changing load modes clears experience constraints from form state", () => {
 const meta = { mode_defaults: { saturation: { concurrency: 1, isl: [8], osl: [4], requests_per_point_multiplier: 1 } }, sla_constraints: [{ key: "min_session_completion" }, { key: "max_p99_ttft" }] } as unknown as BenchmarkMeta;
 const reset = modeFormReset(meta, "saturation");
 assert.ok(Object.hasOwn(reset, "min_session_completion"));
 assert.equal(reset.min_session_completion, undefined);
 assert.equal(reset.max_p99_ttft, undefined);
});

test("grid verdict TTFT uses observed completed samples rather than unknown zero or canceled results", () => {
	const run = {
		state: "done", mode: "grid", verdict: { kind: "grid_done", best_point_seq: 2 },
		points: [
			{ seq: 1, state: "done", concurrency: 1, success: 1, output_tput: 10, p99_ttft: 0, long_tail: { evidence: { version: 1, all: { ttft: { count: 0, p99_ms: 0 } } } } },
			{ seq: 2, state: "done", concurrency: 2, success: 1, output_tput: 20, p99_ttft: 900, long_tail: { evidence: { version: 1, all: { ttft: { count: 1, p99_ms: 80 } } } } },
			{ seq: 3, state: "canceled", concurrency: 4, success: 1, output_tput: 5, p99_ttft: 1 },
			{ seq: 4, state: "failed", concurrency: 8, success: 1, output_tput: 5, p99_ttft: 2 },
			{ seq: 5, concurrency: 16, success: 1, output_tput: 5, p99_ttft: 120 },
		],
	} as unknown as BenchmarkRun;
	assert.match(verdictPresentation(run).description, /best measured TTFT P99 at C=2: 80 ms/);
});

test("grid verdict leaves TTFT unknown when no completed point has an observable sample", () => {
	const run = {
		state: "done", mode: "grid", verdict: { kind: "grid_done", best_point_seq: 1 },
		points: [
			{ seq: 1, state: "done", concurrency: 1, success: 1, output_tput: 10, p99_ttft: 0, long_tail: { evidence: { version: 1, all: { ttft: { count: 0, p99_ms: 0 } } } } },
			{ seq: 2, concurrency: 2, success: 1, output_tput: 5, p99_ttft: 0 },
			{ seq: 3, state: "running", concurrency: 4, success: 1, output_tput: 5, p99_ttft: 1 },
		],
	} as unknown as BenchmarkRun;
	assert.match(verdictPresentation(run).description, /best measured TTFT P99 at -: — ms/);
});

test("saturation verdict shares TTFT availability and preserves legacy nonzero observations", () => {
	const base = { seq: 1, concurrency: 2, success: 1, request_tput: 2, p99_ttft: 7 };
	const measured = { version: 1, all: { ttft: { count: 1, p99_ms: 80 } } };
	for (const [point, expected] of [
		[{ ...base, state: "done", long_tail: { evidence: measured } }, "80"],
		[{ ...base, long_tail: { evidence: measured } }, "80"],
		[{ ...base, state: "canceled", long_tail: { evidence: measured } }, "—"],
		[{ ...base, state: "failed", long_tail: { evidence: measured } }, "—"],
		[{ ...base, state: "running", long_tail: { evidence: measured } }, "—"],
		[{ ...base, state: "done", long_tail: { evidence: { version: 1, all: { ttft: { count: 1, p99_ms: 0 } } } } }, "0"],
		[{ ...base, state: "done", long_tail: { evidence: { version: 1, all: { ttft: { count: 0, p99_ms: 0 } } } } }, "—"],
		[{ ...base, p99_ttft: 80 }, "80"],
		[{ ...base, p99_ttft: 0 }, "—"],
	] as const) {
		const run = { state: "done", mode: "saturation", verdict: { kind: "saturated", best_point_seq: 1, saturation_concurrency: 2 }, points: [point] } as unknown as BenchmarkRun;
		assert.ok(verdictPresentation(run).description.endsWith(`TTFT P99 ${expected} ms.`), JSON.stringify(point));
	}
});

test("a positive sample count cannot make a nonfinite timing pass an SLA limit", () => {
	for (const value of [undefined, Number.NaN, Number.POSITIVE_INFINITY]) {
		const point = {
			long_tail: { evidence: {
				version: 1, plan: { planned_sessions: 1 }, outcomes: { completed_sessions: 1 },
				all: {
					ttft: { count: 1, mean_ms: value, p99_ms: value },
					tpot: { count: 0, mean_ms: 0, p50_ms: 0, p99_ms: 0 },
					last_output_latency: { count: 0, mean_ms: 0, p99_ms: 0 },
				},
			} },
		} as unknown as BenchmarkPoint;
		assert.equal(pointMeetsSLA({ search: { max_p99_ttft: 100 } } as BenchmarkRun, point), false);
	}
});


test("session completion verdict preserves the actual and required rates as percentages", () => {
 const run = { state: "done", mode: "sla", verdict: { kind: "sla_none", violated: [{ metric: "session_completion", actual: 0.75, limit: 0.95 }] } } as BenchmarkRun;
 assert.match(verdictPresentation(run).description, /actual 75%, limit 95%/);
});


test("running grid applies experience gates and does not invent a closest recommendation", () => {
	const complete = { seq: 1, state: "done", concurrency: 1, success: 4, output_tput: 10, long_tail: { evidence: { version: 1, plan: { planned_sessions: 4 }, outcomes: { completed_sessions: 4 }, all: { ttft: { count: 0 }, tpot: { count: 0 }, last_output_latency: { count: 0 } } } } };
	const incomplete = { ...complete, seq: 2, concurrency: 2, output_tput: 20, long_tail: { evidence: { version: 1, plan: { planned_sessions: 4 }, outcomes: { completed_sessions: 3 }, all: { ttft: { count: 0 }, tpot: { count: 0 }, last_output_latency: { count: 0 } } } } };
	const run = { state: "running", mode: "grid", search: { min_session_completion: .95 }, points: [complete, incomplete] } as unknown as BenchmarkRun;
	assert.equal(bestPoint(run)?.seq, 1);
	assert.equal(bestPoint({ ...run, points: [incomplete as unknown as BenchmarkPoint] }), undefined);
});

test("running grid honors objective aliases, completed work and lower-load ties", () => {
	const base = { state: "done", success: 1, concurrency: 4, request_rate: 4, output_tput: 10, request_tput: 5, total_tput: 100 };
	const points = [
		{ ...base, seq: 1, output_tput: 30, request_tput: 1, total_tput: 50 },
		{ ...base, seq: 2 },
		{ ...base, seq: 3, concurrency: 2 },
		{ ...base, seq: 4, concurrency: 8, request_rate: 2 },
		...(["running", "canceled", "failed"] as const).map((state, i) => ({ ...base, seq: 5 + i, state, output_tput: 100, request_tput: 100, total_tput: 1000 })),
		{ ...base, seq: 8, success: 0, request_tput: 200, total_tput: 2000 },
	];
	for (const objective of ["rps", "request_tps", "total_tps", "total_tok_s"]) {
		const run = { state: "running", mode: "grid", search: { objective }, points } as unknown as BenchmarkRun;
		assert.equal(bestPoint(run)?.seq, 4, objective);
	}
	const output = { state: "running", mode: "grid", points } as unknown as BenchmarkRun;
	assert.equal(bestPoint(output)?.seq, 1);
	assert.equal(bestPoint({ ...output, points: [{ ...base, seq: 1, output_tput: 0 }] as BenchmarkPoint[] }), undefined);
	assert.equal(bestPoint({ ...output, points: points.slice(1, 3) as BenchmarkPoint[] })?.seq, 3);
});


test("versioned timing availability requires a positive retained sample count", () => {
	for (const count of [undefined, 0]) {
		const point = { p50_tpot: 9, long_tail: { evidence: { version: 1, all: { tpot: { count, p50_ms: 0 } } } } } as unknown as BenchmarkPoint;
		assert.equal(pointTimingValue(point, "p50_tpot"), undefined);
	}
	const observed = { long_tail: { evidence: { version: 1, all: { tpot: { count: 1, p50_ms: 0 } } } } } as unknown as BenchmarkPoint;
	assert.equal(pointTimingValue(observed, "p50_tpot"), 0);
	for (const p50_ms of [undefined, Number.NaN, Number.POSITIVE_INFINITY]) {
		const invalid = { long_tail: { evidence: { version: 1, all: { tpot: { count: 1, p50_ms } } } } } as unknown as BenchmarkPoint;
		assert.equal(pointTimingValue(invalid, "p50_tpot"), undefined);
	}
	assert.equal(pointTimingValue({ p50_tpot: 9 } as BenchmarkPoint, "p50_tpot"), 9);
});

test("versioned token lengths use measured distributions and preserve zero instead of planned budgets", () => {
	const base = { concurrency: 1, target_isl: 128, target_osl: 32, actual_isl: 99, actual_osl: 99 };
	const measured = (input: { count: number; total: number }, output: { count: number; total: number }) => ({
		...base, long_tail: { evidence: { version: 1, all: { tokens: { input, output } } } },
	}) as unknown as BenchmarkPoint;
	assert.equal(pointParameterLabel(measured({ count: 0, total: 0 }, { count: 2, total: 2 })), "C=1 · ISL=— · OSL=1");
	assert.equal(pointParameterLabel(measured({ count: 2, total: 3 }, { count: 0, total: 0 })), "C=1 · ISL=1.5 · OSL=—");
	assert.equal(pointParameterLabel(measured({ count: 2, total: 0 }, { count: 2, total: 0 })), "C=1 · ISL=0 · OSL=0");
	assert.equal(pointParameterLabel({ concurrency: 1, target_isl: 128, target_osl: 32 }), "C=1 · ISL=128 · OSL=32");
	assert.equal(pointParameterLabel({ ...base, actual_isl: 0, actual_osl: 0 }), "C=1 · ISL=128 · OSL=32");
});

test("formal token rates cannot turn missing aggregate evidence into measured zero, while request rate stays known", () => {
	const metric = { count: 0, mean_ms: 0, p50_ms: 0, p99_ms: 0 };
	for (const [input, output, expected] of [[0, 2, undefined], [2, 0, undefined], [2, 2, 0]] as const) {
		const point = { seq: 1, state: "failed", success: 2, errors: 0, output_tput: 0, total_tput: 0, request_tput: 2,
			long_tail: { evidence: { version: 1, all: { success: 2, ttft: metric, tpot: metric, last_output_latency: metric,
				tokens: { input: { count: input, total: 0 }, output: { count: output, total: 0 } } } } },
		} as unknown as BenchmarkPoint;
		const state = metricCardState({ state: "failed", verdict: { kind: "failed", best_point_seq: 1 }, points: [point] } as BenchmarkRun);
		assert.equal(state.values?.outputTput, expected);
		assert.equal(state.values?.totalTput, expected);
		assert.equal(state.values?.requestTput, 2);
	}
});

test("terminal recommendations never recreate an invalid point rejected by the server", () => {
	const base = { success: 3, concurrency: 1, request_tput: 2, output_tput: 100 };
	const invalid = ["failed", "canceled", "running"].map((state, index) => ({ ...base, state, seq: index + 1 }));
	const run = { state: "failed", mode: "grid", verdict: { kind: "failed" }, points: invalid } as unknown as BenchmarkRun;
	assert.equal(bestPoint(run), undefined);
	assert.equal(metricCardState(run).values, undefined);
	const completed = { ...base, seq: 4, state: "done", output_tput: 10 };
	assert.equal(bestPoint({ ...run, points: [...invalid, completed] } as BenchmarkRun)?.seq, 4);
	assert.equal(bestPoint({ ...run, points: [...invalid, { ...completed, state: "" }] } as BenchmarkRun)?.seq, 4);
});

test("running SLA excludes invalid persisted points from both feasible and closest selection", () => {
	const base = { success: 3, concurrency: 1, output_tput: 100, p99_ttft: 200 };
	const invalid = ["failed", "canceled", "running"].map((state, index) => ({ ...base, seq: index + 1, state, request_tput: 1000, p99_ttft: 1 }));
	const completed = [
		{ ...base, seq: 4, state: "done", request_tput: 4 },
		{ ...base, seq: 5, request_tput: 5, output_tput: 1 },
	];
	const run = { state: "running", mode: "sla", search: { objective: "rps" }, points: [...invalid, ...completed] } as unknown as BenchmarkRun;
	assert.equal(bestPoint(run)?.seq, 5);
	assert.equal(bestPoint({ ...run, points: invalid as BenchmarkPoint[] }), undefined);
	assert.equal(bestPoint({ ...run, search: { ...run.search, max_p99_ttft: 100 }, points: [...invalid, completed[0]] as BenchmarkPoint[] })?.seq, 4);
});

test("historical evaluation target labels distinguish deleted deployments and instances", () => {
	assert.deepEqual(deletedTargetLabelKeys({ deployment_deleted: true } as BenchmarkRun), ["detail.target.deleted"]);
	assert.deepEqual(deletedTargetLabelKeys({ instance_deleted: true } as BenchmarkRun), ["detail.target.instanceDeleted"]);
	assert.deepEqual(deletedTargetLabelKeys({ deployment_deleted: true, instance_deleted: true } as BenchmarkRun), ["detail.target.deleted", "detail.target.instanceDeleted"]);
});

test("benchmark failures render from the code and fall back to the English message", async () => {
	const { benchmarkErrorText, benchmarkErrorCodes } = await import("../src/pages/evaluations/evaluationUtils.ts");
	const en = (await import("../src/i18n/locales/en/evaluations.json", { with: { type: "json" } })).default as { benchmarkError: Record<string, string> };
	const zh = (await import("../src/i18n/locales/zh/evaluations.json", { with: { type: "json" } })).default as { benchmarkError: Record<string, string> };
	for (const code of benchmarkErrorCodes) {
		assert.ok(en.benchmarkError[code] && zh.benchmarkError[code], `${code} needs en and zh text`);
	}
	const failure = { code: "BENCHMARK_DATASET_LOAD_FAILED", params: { dataset: "multiturn" }, message: "load the built-in multiturn dataset: EOF" };
	const t = ((key: string, options: Record<string, unknown>) => key === "benchmarkError.BENCHMARK_DATASET_LOAD_FAILED" ? en.benchmarkError.BENCHMARK_DATASET_LOAD_FAILED.replace("{{dataset}}", String(options.dataset)) : String(options.defaultValue)) as never;
	assert.equal(benchmarkErrorText(t, failure.message, failure), "The built-in multiturn dataset could not be loaded, so the evaluation stopped before sending any request.");
	assert.equal(benchmarkErrorText(undefined, failure.message, failure), failure.message);
	// An unknown code and an uncoded run keep the server's text.
	assert.equal(benchmarkErrorText(t, "boom", { code: "SOMETHING_NEW", message: "boom" }), "boom");
	assert.equal(benchmarkErrorText(t, "all 3 warmup requests failed"), "all 3 warmup requests failed");
});
