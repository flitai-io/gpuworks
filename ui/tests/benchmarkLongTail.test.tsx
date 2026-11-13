import assert from "node:assert/strict";
import fs from "node:fs";
import { afterEach, test } from "node:test";
import { JSDOM } from "jsdom";
import React from "react";
import { createInstance } from "i18next";
import { I18nextProvider, initReactI18next } from "react-i18next";
import type { BenchmarkPoint, BenchmarkRun } from "../src/api.ts";

const dom = new JSDOM("<!doctype html><html><body></body></html>", { url: "http://localhost/" });
for (const name of ["window", "document", "navigator", "HTMLElement", "SVGElement", "Element", "Node", "MutationObserver", "ShadowRoot"] as const) {
	Object.defineProperty(globalThis, name, { value: dom.window[name], configurable: true });
}
Object.defineProperty(globalThis, "getComputedStyle", { value: (element: Element) => dom.window.getComputedStyle(element), configurable: true });
Object.defineProperty(dom.window, "matchMedia", { configurable: true, value: () => ({ matches: false, addListener() {}, removeListener() {}, addEventListener() {}, removeEventListener() {}, dispatchEvent: () => false }) });
Object.defineProperty(globalThis, "ResizeObserver", { value: class { observe() {} unobserve() {} disconnect() {} }, configurable: true });

const en = JSON.parse(fs.readFileSync(new URL("../src/i18n/locales/en/evaluations.json", import.meta.url), "utf8"));
const i18n = createInstance();
await i18n.use(initReactI18next).init({ lng: "en", defaultNS: "evaluations", interpolation: { escapeValue: false }, resources: { en: { evaluations: en } } });

const { cleanup, render, screen, within } = await import("@testing-library/react");
const userEvent = (await import("@testing-library/user-event")).default;
const { AdvancedResult } = await import("../src/pages/EvaluationSupport.tsx");

const { pointEvidence } = await import("../src/pages/evaluations/reportSections.ts");

afterEach(() => cleanup());

const run = { id: 7 } as BenchmarkRun;

// A point's long_tail as GET /api/v1/benchmarks/{id} serves it
// (dto.BenchmarkLongTail): every percentile in milliseconds keyed by the
// percentile, kv_hit_rate and speculative only when observed.
function point(longTail: unknown): BenchmarkPoint {
	return JSON.parse(JSON.stringify({ seq: 1, long_tail: longTail })) as BenchmarkPoint;
}

async function expand() {
	await userEvent.setup({ document: dom.window.document }).click(screen.getByText(en.support.advanced));
}

test("the advanced result reads the snake_case long tail: percentiles, KV hit rate, speculative decoding and the length method", async () => {
	render(<I18nextProvider i18n={i18n}><AdvancedResult run={run} point={point({
		ttft_ms: { "50": 120, "99": 480 },
		tpot_ms: { "50": 12, "99": 30 },
		latency_ms: { "50": 900, "99": 2100 },
		itl_ms: { "50": 11, "99": 29 },
		approximate_tokens: true,
		kv_hit_rate: 0.42,
		cache_report_coverage: 1,
		peak_input_tokens: 8192,
		speculative: { tokens_per_iteration: 2.5, acceptance_rate: 0.6 },
	})} /></I18nextProvider>);
	await expand();
	assert.ok(screen.getByText("42%"));
	assert.ok(screen.getByText("2.5 tokens/iteration · acceptance rate 60%"));
	assert.ok(screen.getByText(en.support.estimated));
	const p99 = screen.getByText("P99").closest("tr") as HTMLElement;
	assert.deepEqual([...p99.querySelectorAll("td")].map((cell) => cell.textContent), ["P99", "480 ms", "30 ms", "2,100 ms", "29 ms"]);
	const p5 = screen.getByText("P5").closest("tr") as HTMLElement;
	assert.equal(within(p5).getAllByText("-").length, 4, "a percentile the run did not report stays empty");
});

test("without KV or speculative facts, and with exact token counts, the advanced result says so", async () => {
	render(<I18nextProvider i18n={i18n}><AdvancedResult run={run} point={point({
		ttft_ms: { "50": 120 }, tpot_ms: null, latency_ms: null, itl_ms: null,
		approximate_tokens: false, peak_input_tokens: 0,
	})} /></I18nextProvider>);
	await expand();
	assert.ok(screen.getByText(en.support.kvMissing));
	assert.ok(screen.getByText(en.support.speculativeMissing));
	assert.ok(screen.getByText(en.support.exact));
});


test("versioned timing percentiles use observable samples and preserve measured zero", async () => {
	render(<I18nextProvider i18n={i18n}><AdvancedResult run={run} point={point({
		ttft_ms: { "50": 999, "95": 999, "99": 999 },
		tpot_ms: { "50": 0, "95": 0, "99": 0 },
		latency_ms: { "50": 999, "95": 999, "99": 999 },
		itl_ms: { "50": 0, "95": 0, "99": 0 },
		evidence: { version: 1, all: {
			ttft: { count: 1, p50_ms: 0, p99_ms: 0 },
			tpot: { count: 0, p50_ms: 0, p99_ms: 0 },
			last_output_latency: { count: 2, p50_ms: 80, p99_ms: 100 },
		} },
	})} /></I18nextProvider>);
	await expand();
	const cells = (label: string) => [...(screen.getByText(label).closest("tr") as HTMLElement).querySelectorAll("td")].map((cell) => cell.textContent);
	assert.deepEqual(cells("P50"), ["P50", "0 ms", "-", "80 ms", "-"]);
	assert.deepEqual(cells("P99"), ["P99", "0 ms", "-", "100 ms", "-"]);
	assert.deepEqual(cells("P95"), ["P95", "-", "-", "-", "-"], "unretained timing percentiles must not fall back to incompatible legacy samples");
});


test("versioned advanced summaries use weighted cache evidence and explicit token sources", async () => {
	render(<I18nextProvider i18n={i18n}><AdvancedResult run={run} point={point({
		kv_hit_rate: .99, approximate_tokens: false,
		evidence: { version: 1, all: {
			ttft: { count: 0 }, tpot: { count: 0 }, last_output_latency: { count: 0 },
			cache: { input_tokens: 1000, cached_tokens: 100 },
			tokens: { input_sources: { server_usage: 2 }, output_sources: { character_estimate: 2 } },
		} },
	})} /></I18nextProvider>);
	await expand();
	assert.ok(screen.getByText("10%"));
	assert.equal(screen.queryByText("99%"), null);
	assert.equal(screen.queryByText(en.support.exact), null);
	assert.ok(screen.getByText(/Server usage: 2/));
	assert.ok(screen.getByText(/Character estimate: 2/));
});

test("versioned advanced summaries with no cache or token source facts stay unknown", async () => {
	render(<I18nextProvider i18n={i18n}><AdvancedResult run={run} point={point({
		kv_hit_rate: 0, approximate_tokens: false,
		evidence: { version: 1, all: {
			ttft: { count: 0 }, tpot: { count: 0 }, last_output_latency: { count: 0 },
			cache: { input_tokens: 0, cached_tokens: 0 },
			tokens: { input_sources: null, output_sources: null },
		} },
	})} /></I18nextProvider>);
	await expand();
	assert.equal(screen.queryByText("0%"), null);
	assert.equal(screen.queryByText(en.support.exact), null);
	assert.ok(screen.getAllByText(/Unknown/).length >= 2);
});

test("offline advanced metrics preserve the four expanded preview fields for each point, including unknown measurements", async () => {
	const selected = point({ evidence: { version: 1, all: {
		cache: { input_tokens: 40, cached_tokens: 10 },
		tokens: { input_sources: { server_usage: 3 }, output_sources: { character_estimate: 3 } },
		ttft: { count: 0 }, tpot: { count: 0 }, last_output_latency: { count: 0 },
	} } });
	const preview = render(<I18nextProvider i18n={i18n}><AdvancedResult run={run} point={selected} /></I18nextProvider>);
	await expand();
	const pairs = Array.from(preview.container.querySelectorAll(".ant-descriptions-item"), item =>
		["label", "content"].map(part => item.querySelector(".ant-descriptions-item-" + part)?.textContent));
	assert.equal(pairs.length, 4);
	const section = document.createElement("section");
	section.innerHTML = "<h3>Point 1</h3>";
	pointEvidence(section, selected, false, i18n.getFixedT("en", "evaluations"));
	const rows = section.querySelector("details table")!.rows;
	assert.deepEqual(Array.from(rows, row => Array.from(row.cells, cell => cell.textContent)), pairs);
});
