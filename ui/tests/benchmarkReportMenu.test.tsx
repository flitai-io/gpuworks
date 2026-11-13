import assert from "node:assert/strict";
import { after, afterEach, test } from "node:test";
import { JSDOM } from "jsdom";
import React from "react";
import { createInstance } from "i18next";
import { I18nextProvider, initReactI18next } from "react-i18next";

const dom = new JSDOM("<!doctype html><html><body></body></html>", { url: "http://localhost/" });
for (const name of ["window", "document", "navigator", "HTMLElement", "SVGElement", "Element", "Node", "MutationObserver", "ShadowRoot"] as const) {
	Object.defineProperty(globalThis, name, { value: dom.window[name], configurable: true });
}
Object.defineProperty(globalThis, "getComputedStyle", { value: (element: Element) => dom.window.getComputedStyle(element), configurable: true });
Object.defineProperty(dom.window, "matchMedia", { configurable: true, value: () => ({ matches: false, addListener() {}, removeListener() {}, addEventListener() {}, removeEventListener() {}, dispatchEvent: () => false }) });
// rc-overflow expects browser MessageChannel; JSDOM has none. Avoid Node ports keeping the test alive.
Object.defineProperty(globalThis, "MessageChannel", { value: undefined, configurable: true });
Object.defineProperty(globalThis, "ResizeObserver", { value: class { observe() {} unobserve() {} disconnect() {} }, configurable: true });

const i18n = createInstance();
await i18n.use(initReactI18next).init({
	lng: "zh", defaultNS: "evaluations", interpolation: { escapeValue: false },
	resources: { zh: { evaluations: { agentic: { export: { button: "导出报告", png: "下载 PNG 长图", exporting: "正在生成报告…", visualFailed: "图片导出失败", markdown: "下载 Markdown", csv: "下载 CSV", html: "下载离线 HTML（与页面同款）" } } } } },
});

const { message } = await import("antd");
const { cleanup, render, screen, fireEvent, waitFor, act } = await import("@testing-library/react");
const { benchmarkReportDownloads, benchmarkReportURL, default: BenchmarkReportMenu } = await import("../src/pages/evaluations/BenchmarkReportMenu.tsx");
const { savedReportContext } = await import("../src/pages/evaluations/reportContext.ts");
const { localizeLegacy, reportText } = await import("../src/pages/evaluations/reportLanguage.ts");

const { legacyParetoData } = await import("../src/pages/evaluations/reportSections.ts");
const { addFullPointColumns } = await import("../src/pages/evaluations/reportPointTable.ts");

afterEach(() => { cleanup(); act(() => message.destroy()); });
after(() => dom.window.close());

test("the redacted configuration belongs to this run even with multiline quoted fields in the CSV", () => {
	const context = { run_id: 42, tokenizer: "label, with quotes", target_snapshot: { argv: ["a", "b"] } };
	const csv = 'note,report_context_json\r\n"legacy,\nrecord","' + JSON.stringify(context).replace(/"/g, '""') + '"\r\n';
	assert.deepEqual(savedReportContext(csv, 42), context);
	assert.equal(savedReportContext(csv, 41), undefined);
	assert.equal(savedReportContext("note\nold record", 42), undefined);
	assert.equal(savedReportContext("report_context_json\ntruncated", 42), undefined);
});

test("localizing a legacy report preserves metric identifiers and user-provided model names and outputs", () => {
	const doc = new JSDOM('<body><h2>Point results</h2><table><tr><th>Model</th><td>failed</td></tr>' +
		'<tr><th>point_seq</th><td>2</td></tr></table><pre>first_turn: original output</pre>' +
		'<ul><li><code>e2el</code> is the end-to-end request latency, from sending the request to its last generated token.</li></ul></body>').window.document;
	localizeLegacy(doc, true);
	assert.equal(doc.querySelector("h2")?.textContent, "点位结果");
	assert.equal(doc.querySelector("td")?.textContent, "failed");
	assert.equal(doc.querySelector("th[title=point_seq]")?.textContent, "点位序号");
	assert.equal(doc.querySelector("li code")?.textContent, "e2el");
	assert.equal(doc.querySelector("pre")?.textContent, "first_turn: original output");
	assert.ok(reportText("random benchmark for failed", true).endsWith(" · failed"));
	assert.match(reportText("best measured point in this run: arrival rate 2 req/s; objective=output_tps; output throughput 1.000 tok/s; interactivity 3 tok/s/user (decode only); comparison warnings and declared constraints apply; no capacity upper bound", true), /本轮已测点最佳：到达率 2 req\/s/);
});

test("report menu exposes Markdown, CSV, and standalone HTML downloads", () => {
	assert.equal(benchmarkReportURL(7, "markdown"), "/api/v1/benchmarks/7/report?format=markdown");
	assert.deepEqual(benchmarkReportDownloads(7), [
		{ format: "markdown", href: "/api/v1/benchmarks/7/report?format=markdown" },
		{ format: "csv", href: "/api/v1/benchmarks/7/report?format=csv" },
		{ format: "html", href: "/api/v1/benchmarks/7/report?format=html" },
	]);
	render(<I18nextProvider i18n={i18n}><BenchmarkReportMenu runID={7} /></I18nextProvider>);
	assert.ok(screen.getByRole("button", { name: /导出报告/ }));
	assert.equal(benchmarkReportDownloads(7, "zh")[2].href, "/api/v1/benchmarks/7/report?format=html&lang=zh");
	assert.equal(benchmarkReportURL(7, "html", "en"), "/api/v1/benchmarks/7/report?format=html&lang=en");
	assert.equal(benchmarkReportURL(7, "csv", "zh"), "/api/v1/benchmarks/7/report?format=csv");
});

test("one offline row per point preserves preview order, full precision and points completed during export", () => {
	const container = document.createElement("div");
	container.innerHTML = `<div class="ant-table-title"><strong>Point Results</strong></div><table>
		<thead class="ant-table-thead"><tr><th>Point</th><th>Throughput</th></tr></thead>
		<tbody class="ant-table-tbody"><tr data-row-key="2"><td>#2</td><td>19.8 tok/s</td></tr>
		<tr data-row-key="1"><td>#1</td><td>7.1 tok/s</td></tr><tr data-row-key="3"><td>#3</td><td>—</td></tr></tbody></table>`;
	const original = `<section id="all-points"><h2>Point results</h2><div class="scroll"><table>
		<thead><tr><th>Point</th><th>output_tok_s</th></tr></thead><tbody>
		<tr data-seq="1"><th>#1</th><td>7.118</td></tr><tr data-seq="2"><th>#2</th><td>19.812</td></tr>
		<tr data-seq="4"><th>#4</th><td>22.134</td></tr></tbody></table></div></section>`;
	container.querySelector("tbody")!.className = "ant-table-tbody";
	container.querySelector("thead")!.className = "ant-table-thead";
	const originalSection = JSDOM.fragment(original).querySelector("section")!;
	assert.equal(addFullPointColumns(container, originalSection, "Show full metrics"), true);
	assert.equal(container.querySelectorAll("table").length, 1);
	const rows = Array.from(container.querySelectorAll("tbody tr"));
	assert.deepEqual(rows.map(row => row.dataset.seq || row.dataset.rowKey), ["2", "1", "3", "4"]);
	assert.deepEqual(Array.from(rows[0].children, cell => cell.textContent), ["#2", "19.8 tok/s", "#2", "19.812"]);
	assert.ok(rows[2].classList.contains("preview-only"));
	assert.ok(rows[3].classList.contains("full-only"));
	assert.equal(container.querySelector("label")?.htmlFor, "full-point-columns");
});

test("a historical attachment restores measured scatter points without interpreting missing throughput as zero", () => {
	const fragment = JSDOM.fragment(`<section><table><thead><tr><th>output_tok_s</th><th>interactivity_tok_s_user</th><th>success_rate</th></tr></thead>
		<tbody><tr><td>0.000</td><td>19.75</td><td>95.50%</td></tr><tr><td></td><td>21.0</td><td></td></tr></tbody></table></section>
		<section id="point-3"><h3>Point 3 (R=2)</h3></section><section id="point-7"><h3>Point 7 (R=3)</h3></section>`);
	const data = legacyParetoData(fragment.querySelector("section")!, [...fragment.querySelectorAll("[id]")]);
	assert.equal(data.length, 1);
	const { seq, loadLabel, x, y, successRate } = data[0];
	assert.deepEqual({ seq, loadLabel, x, y, successRate }, { seq: 3, loadLabel: "R=2", x: 19.75, y: 0, successRate: 95.5 });
});

test("interrupted transfers and invalid responses cannot save a partial or unrelated HTML file", async (t) => {
	const { downloadChartReport } = await import("../src/pages/evaluations/downloadChartReport.ts");
	const view = { curveMetric: "output_tput" as const, curveMetrics: [], curveTitle: "", plotted: [], curveSeriesName: () => "",
		curves: [], root: null, fullMetricsLabel: "Show full metrics" };
	const headers = { "content-type": "text/html", "content-disposition": "attachment; filename=run-9.html" };
	const downloads = t.mock.method(URL, "createObjectURL", () => "blob:test");
	for (const response of [
		new Response("Login", { status: 401 }),
		new Response("Server unavailable", { status: 503 }),
		new Response("<html>Login</html>", { headers: { "content-type": "text/html" } }),
		new Response("{}", { headers: { ...headers, "content-type": "application/json" } }),
		new Response("<!doctype html><html><body><table>", { headers }),
		new Response(new ReadableStream({ start(controller) { controller.error(new Error("transfer interrupted")); } }), { headers }),
	]) {
		const request = t.mock.method(globalThis, "fetch", async () => response);
		await assert.rejects(downloadChartReport("/report", view, 9));
		request.mock.restore();
	}
	assert.equal(downloads.mock.callCount(), 0);
});

for (const format of ["png", "html"] as const) test(`${format} export supports keyboard selection and shows progress until complete`, async () => {
	let finish!: () => void;
	const calls: string[] = [];
	render(<I18nextProvider i18n={i18n}><BenchmarkReportMenu runID={7} onExportVisual={(value) => { calls.push(value); return new Promise<void>((resolve) => { finish = resolve; }); }} /></I18nextProvider>);
	fireEvent.click(screen.getByRole("button", { name: /导出报告/ }));
	const item = await screen.findByRole("menuitem", { name: format === "png" ? "下载 PNG 长图" : "下载离线 HTML（与页面同款）" });
	assert.equal(item.querySelector("a"), null, "visual reports must use the rendered page, not the legacy API template");
	item.focus();
	fireEvent.keyDown(item, { key: "Enter", code: "Enter", keyCode: 13, which: 13 });
	await waitFor(() => assert.deepEqual(calls, [format]));
	assert.ok(screen.getByRole("button", { name: /正在生成报告/ }));
	await act(async () => finish());
	assert.ok(screen.getByRole("button", { name: /导出报告/ }));
});

test("PNG export reports errors and permits another attempt", async () => {
	let calls = 0;
	render(<I18nextProvider i18n={i18n}><BenchmarkReportMenu runID={7} onExportVisual={async () => { calls++; throw new Error("canvas unavailable"); }} /></I18nextProvider>);
	for (let attempt = 1; attempt <= 2; attempt++) {
		fireEvent.click(screen.getByRole("button", { name: /导出报告/ }));
		fireEvent.click(await screen.findByRole("menuitem", { name: "下载 PNG 长图" }));
		await waitFor(() => assert.equal(calls, attempt));
		await waitFor(() => assert.ok(screen.getByRole("button", { name: /导出报告/ })));
	}
	assert.ok((await screen.findAllByText("图片导出失败")).length > 0);
});

test("PNG export is disabled while report data is loading", async () => {
	let calls = 0;
	render(<I18nextProvider i18n={i18n}><BenchmarkReportMenu runID={7} visualDisabled onExportVisual={async () => { calls++; }} /></I18nextProvider>);
	fireEvent.click(screen.getByRole("button", { name: /导出报告/ }));
	const item = await screen.findByRole("menuitem", { name: "下载 PNG 长图" });
	assert.equal(item.getAttribute("aria-disabled"), "true");
	fireEvent.click(item);
	assert.equal(calls, 0);
});


test("the page menu has one HTML export alongside PNG, Markdown, and CSV", async () => {
	render(<I18nextProvider i18n={i18n}><BenchmarkReportMenu runID={7} onExportVisual={async () => {}} /></I18nextProvider>);
	fireEvent.click(screen.getByRole("button", { name: /导出报告/ }));
	const items = await screen.findAllByRole("menuitem");
	assert.equal(items.length, 4);
	assert.deepEqual(items.map((item) => item.textContent), ["下载离线 HTML（与页面同款）", "下载 PNG 长图", "下载 Markdown", "下载 CSV"]);
	assert.equal(screen.getByRole("link", { name: "下载 Markdown" }).getAttribute("href"), "/api/v1/benchmarks/7/report?format=markdown");
	assert.equal(screen.getByRole("link", { name: "下载 CSV" }).getAttribute("href"), "/api/v1/benchmarks/7/report?format=csv");
	assert.equal(document.querySelector('a[href="/api/v1/benchmarks/7/report?format=html"]'), null);
});
