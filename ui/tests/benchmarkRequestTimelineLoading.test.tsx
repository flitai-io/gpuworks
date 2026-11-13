import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import { JSDOM } from "jsdom";
import type { BenchmarkRequestTimeline } from "../src/api.ts";

const dom = new JSDOM("<!doctype html><html><body></body></html>", { url: "http://localhost/" });
for (const name of ["window", "document", "navigator", "HTMLElement", "Element", "Node", "MutationObserver"] as const) {
	Object.defineProperty(globalThis, name, { value: dom.window[name], configurable: true });
}
const { cleanup, renderHook, act, waitFor } = await import("@testing-library/react");
const { api } = await import("../src/api.ts");
const { default: useBenchmarkRequestTimeline } = await import("../src/pages/evaluations/useBenchmarkRequestTimeline.ts");
const original = api.benchmarkPointTimeline;
afterEach(() => { cleanup(); api.benchmarkPointTimeline = original; });

function deferred() {
	let resolve!: (value: BenchmarkRequestTimeline) => void;
	const promise = new Promise<BenchmarkRequestTimeline>((done) => { resolve = done; });
	return { promise, resolve };
}

test("switching points aborts old reads and never mixes their records", async () => {
	const first = deferred(), second = deferred();
	const signals: AbortSignal[] = [];
	api.benchmarkPointTimeline = (_id, seq, signal) => { signals.push(signal!); return seq === 1 ? first.promise : second.promise; };
	const { result, rerender, unmount } = renderHook(({ seq }) => useBenchmarkRequestTimeline(7, seq), { initialProps: { seq: 1 } });
	assert.equal(result.current.loading, true);
	rerender({ seq: 2 });
	assert.equal(signals[0].aborted, true);
	await act(async () => second.resolve({ status: "complete", records: [] }));
	assert.equal(result.current.loading, false);
	assert.equal(result.current.data?.status, "complete");
	await act(async () => first.resolve({ status: "unavailable", records: [] }));
	assert.equal(result.current.data?.status, "complete");
	unmount();
	assert.equal(signals[1].aborted, true);
});

test("loading another point hides the preceding successful response", async () => {
	const second = deferred();
	api.benchmarkPointTimeline = async (_id, seq) => seq === 1 ? { status: "disabled", records: [] } : second.promise;
	const { result, rerender } = renderHook(({ seq }) => useBenchmarkRequestTimeline(7, seq), { initialProps: { seq: 1 } });
	await waitFor(() => assert.equal(result.current.data?.status, "disabled"));
	rerender({ seq: 2 });
	assert.equal(result.current.data, undefined);
	assert.equal(result.current.loading, true);
	await act(async () => second.resolve({ status: "truncated", records: [] }));
	assert.equal(result.current.data?.status, "truncated");
});

test("a failed read can be retried without showing stale results", async () => {
	let calls = 0;
	api.benchmarkPointTimeline = async () => { if (++calls === 1) throw new Error("network failure"); return { status: "complete", records: [] }; };
	const { result } = renderHook(() => useBenchmarkRequestTimeline(7, 1));
	await waitFor(() => assert.equal(result.current.error?.message, "network failure"));
	act(() => result.current.refetch());
	assert.equal(result.current.loading, true);
	assert.equal(result.current.error, undefined);
	await waitFor(() => assert.equal(result.current.data?.status, "complete"));
	assert.equal(calls, 2);
});
