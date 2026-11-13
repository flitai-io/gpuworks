import assert from "node:assert/strict";
import fs from "node:fs";
import { afterEach, test } from "node:test";
import { JSDOM } from "jsdom";
import React from "react";
import { createInstance } from "i18next";
import { I18nextProvider, initReactI18next } from "react-i18next";
import type { BenchmarkPoint, BenchmarkRun } from "../src/api.ts";

const dom = new JSDOM("<!doctype html><html><body></body></html>", { url: "http://localhost/" });
for (const name of ["window", "document", "navigator", "HTMLElement", "SVGElement", "Element", "Node", "MutationObserver", "ShadowRoot", "localStorage"] as const) {
	Object.defineProperty(globalThis, name, { value: dom.window[name], configurable: true });
}
Object.defineProperty(globalThis, "getComputedStyle", { value: (element: Element) => dom.window.getComputedStyle(element), configurable: true });
Object.defineProperty(dom.window, "matchMedia", { configurable: true, value: () => ({ matches: false, addListener() {}, removeListener() {}, addEventListener() {}, removeEventListener() {}, dispatchEvent: () => false }) });
Object.defineProperty(globalThis, "ResizeObserver", { value: class { observe() {} unobserve() {} disconnect() {} }, configurable: true });

const zh = JSON.parse(fs.readFileSync(new URL("../src/i18n/locales/zh/evaluations.json", import.meta.url), "utf8"));
const i18n = createInstance();
await i18n.use(initReactI18next).init({ lng: "zh", defaultNS: "evaluations", interpolation: { escapeValue: false }, resources: { zh: { evaluations: zh } } });

const { cleanup, render, screen, within } = await import("@testing-library/react");
const userEvent = (await import("@testing-library/user-event")).default;
const { default: PointResultsTable } = await import("../src/pages/evaluations/PointResultsTable.tsx");
const { addFullPointColumns } = await import("../src/pages/evaluations/reportPointTable.ts");
const { POINT_COLUMNS_STORAGE_KEY, POINT_COLUMN_KEYS } = await import("../src/pages/evaluations/pointColumnPrefs.ts");

afterEach(() => {
	cleanup();
	localStorage.clear();
});

const base = { state: "done", success: 10, errors: 0, actual_isl: 8192, actual_osl: 256, target_isl: 8192, target_osl: 256, input_tokens: 0, output_tokens: 0, total_tput: 0, max_output_tput: 0, max_total_tput: 0, request_tput: 1, avg_ttft: 0, p50_ttft: 2000, p99_ttft: 2600, avg_tpot: 0, p99_tpot: 25, avg_latency: 0, p50_latency: 7100, p99_latency: 9000 };
const point = (fields: Partial<BenchmarkPoint>) => ({ ...base, ...fields }) as BenchmarkPoint;

// The sticky header renders the header and the body as two tables.
function renderTable(run: BenchmarkRun) {
	render(<I18nextProvider i18n={i18n}><PointResultsTable run={run} onSelect={() => {}} /></I18nextProvider>);
	return document.body;
}

function headerTitles(container: HTMLElement) {
	return within(container).getAllByRole("columnheader").map((cell) => cell.textContent || "");
}

test("offline points retain the preview column order, selected row and detail navigation", () => {
	const visible = ["point", "max_output", "request", "detail"];
	const order = [...visible, ...POINT_COLUMN_KEYS.filter(key => !visible.includes(key))];
	localStorage.setItem(POINT_COLUMNS_STORAGE_KEY, JSON.stringify({ order, visible }));
	const run = { id: 4, mode: "grid", state: "done", points: [
		point({seq: 1, max_output_tput: 321.5, request_tput: 7.5, detail_path: "/private/retained.gz"}),
		point({seq: 2}),
	] } as BenchmarkRun;
	const live = renderTable(run);
	const titles = headerTitles(live);
	const offline = live.cloneNode(true) as HTMLElement;
	const full = document.createElement("section");
	full.innerHTML = '<table><thead><tr><th>output_tok_s</th></tr></thead><tbody><tr data-seq="1"><td>321.500</td></tr><tr data-seq="2"><td>—</td></tr></tbody></table>';
	assert.ok(addFullPointColumns(offline, full, zh.pointTable.showFullMetrics));
	assert.deepEqual(Array.from(offline.querySelectorAll("thead th:not(.full-metric)"), c => c.textContent), titles);
	const row = offline.querySelector<HTMLTableRowElement>('tr[data-seq="1"]')!;
	assert.deepEqual(Array.from(row.cells).slice(0, 3).map(c => c.textContent), ["#1", "321.5 tok/s", "7.5 req/s"]);
	assert.equal(row.cells[0].querySelector("a")?.getAttribute("href"), "#point-1");
	assert.equal(row.querySelector(".full-metric")?.textContent, "321.500");
});

function columnValues(container: HTMLElement, title: string) {
	const headers = headerTitles(container);
	const index = headers.findIndex((header) => header.startsWith(title));
	assert.ok(index >= 0, `column ${title} in ${headers.join(" | ")}`);
	return [...container.querySelectorAll<HTMLElement>("tr[data-row-key]")].map((row) => within(row).getAllByRole("cell")[index].textContent);
}

test("interactivity is 1000 / TPOT P50 and E2E latency is a default column", () => {
	// 2 s TTFT, 20 ms TPOT: throughput ÷ concurrency would read 36 tok/s/user.
	const run = { id: 1, mode: "grid", state: "done", points: [point({ seq: 1, concurrency: 8, output_tput: 288, p50_tpot: 20 }), point({ seq: 2, concurrency: 16, output_tput: 300, p50_tpot: 0 })] } as BenchmarkRun;
	const table = renderTable(run);
	assert.deepEqual(columnValues(table, zh.pointTable.column.interactivity), ["50 tok/s/user", "—"]);
	assert.deepEqual(columnValues(table, zh.pointTable.column.latency_p99), ["9,000 ms", "9,000 ms"]);
	// Closed-loop runs have no arrival-rate column.
	const headers = headerTitles(table);
	assert.equal(headers.some((header) => header.startsWith(zh.pointTable.column.request_rate)), false);
	assert.equal(headers.some((header) => header.includes("每用户")), false);
});

test("open-loop points show their arrival rate and in-flight cap", () => {
	const run = { id: 2, mode: "grid", state: "done", points: [point({ seq: 1, concurrency: 0, request_rate: 2.5, output_tput: 100, p50_tpot: 10 }), point({ seq: 2, concurrency: 16, request_rate: 5, output_tput: 180, p50_tpot: 12.5 })] } as BenchmarkRun;
	const table = renderTable(run);
	assert.deepEqual(columnValues(table, zh.pointTable.column.request_rate), ["2.5 req/s", "5 req/s"]);
	assert.deepEqual(columnValues(table, zh.pointTable.column.concurrency), [zh.pointTable.unlimited, "≤16"]);
	assert.deepEqual(columnValues(table, zh.pointTable.column.interactivity), ["100 tok/s/user", "80 tok/s/user"]);
});

test("metric headers explain themselves on hover", async () => {
	const user = userEvent.setup();
	const run = { id: 3, mode: "grid", state: "done", points: [point({ seq: 1, concurrency: 1, output_tput: 10, p50_tpot: 10 })] } as BenchmarkRun;
	renderTable(run);
	const help = screen.getAllByLabelText(zh.metricHelp.interactivity)[0];
	await user.hover(help);
	assert.ok(await screen.findByRole("tooltip"));
	assert.match(screen.getByRole("tooltip").textContent || "", /1000 \/ TPOT P50/);
	for (const key of ["output", "total", "request", "ttft", "tpot", "latency", "concurrency"]) assert.ok(screen.getAllByLabelText(zh.metricHelp[key]).length > 0, `help for ${key}`);
});

test("unrepresentable token aggregates stay unknown without hiding observed zero or request throughput", () => {
	const metric = { count: 0, mean_ms: 0, p50_ms: 0, p99_ms: 0 };
	const dist = (count: number, total: number) => ({ count, total, min: 0, max: total, p50: 0, p99: 0 });
	const observed = (seq: number, input: number, output: number) => point({
		seq, concurrency: 1, state: "failed", success: 2, errors: 0, actual_isl: 0, actual_osl: 0, target_isl: 128, target_osl: 32,
		output_tput: 0, total_tput: 0, request_tput: 2,
		long_tail: { evidence: { version: 1, all: { success: 2, ttft: metric, tpot: metric, last_output_latency: metric,
			tokens: { input: dist(input, input), output: dist(output, output) } } } },
	} as unknown as Partial<BenchmarkPoint>);
	const zero = observed(3, 2, 2);
	zero.long_tail!.evidence!.all.tokens.input.total = 0;
	zero.long_tail!.evidence!.all.tokens.output.total = 0;
	const table = renderTable({ id: 1, mode: "grid", state: "failed", points: [observed(1, 0, 2), observed(2, 2, 0), zero] } as BenchmarkRun);
	assert.deepEqual(columnValues(table, zh.pointTable.column.request), ["2 req/s", "2 req/s", "2 req/s"]);
	assert.deepEqual(columnValues(table, "ISL / OSL"), ["— / 1", "1 / —", "0 / 0"]);
	assert.deepEqual(columnValues(table, zh.pointTable.column.avg_output), ["—", "—", "0 tok/s"]);
	assert.deepEqual(columnValues(table, zh.pointTable.column.avg_total), ["—", "—", "0 tok/s"]);
});
