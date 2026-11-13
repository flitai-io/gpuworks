import assert from "node:assert/strict";
import { test } from "node:test";
import { benchmarkCurveSeries, type CurveField } from "../src/pages/evaluations/benchmarkCurveSeries.ts";
import type { BenchmarkCurveSample } from "../src/api.ts";

test("each preview and offline metric uses its own samples, retaining measured zero and the source point identity", () => {
	const sample: BenchmarkCurveSample = { t: 12500, output_tput: 18, total_tput: 29, request_tput: 3, ttft_p50: 0, tpot_p50: 7, latency_p99: 81 };
	const points = [
		{ seq: 12, concurrency: 4, samples: [sample, { ...sample, t: 0 }] },
		{ seq: 21, concurrency: 8, samples: [{ ...sample, ttft_p50: null }] },
	];
	for (const field of ["output_tput", "total_tput", "request_tput", "ttft_p50", "tpot_p50", "latency_p99"] as CurveField[]) {
		const expected = [12, 21].map(seq => ({
			labels: { benchmark: "9", point: String(seq) }, points: [[12500, seq === 21 && field === "ttft_p50" ? Number.NaN : sample[field]]],
		}));
		assert.deepEqual(benchmarkCurveSeries(points, 9, field), expected);
	}
});
