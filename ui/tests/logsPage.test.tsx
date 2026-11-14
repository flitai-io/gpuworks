import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import { JSDOM } from "jsdom";
import React from "react";
import { createInstance } from "i18next";
import { I18nextProvider, initReactI18next } from "react-i18next";

const dom = new JSDOM("<!doctype html><html><body></body></html>", { url: "http://localhost/" });
for (const name of ["window", "document", "navigator", "HTMLElement", "HTMLTextAreaElement", "SVGElement", "Element", "Node", "MutationObserver", "ShadowRoot"] as const) {
	Object.defineProperty(globalThis, name, { value: dom.window[name], configurable: true });
}
Object.defineProperty(globalThis, "getComputedStyle", { value: (element: Element) => dom.window.getComputedStyle(element), configurable: true });
Object.defineProperty(dom.window, "matchMedia", { configurable: true, value: () => ({ matches: false, addListener() {}, removeListener() {}, addEventListener() {}, removeEventListener() {}, dispatchEvent: () => false }) });
Object.defineProperty(globalThis, "ResizeObserver", { value: class { observe() {} unobserve() {} disconnect() {} }, configurable: true });

const i18n = createInstance();
await i18n.use(initReactI18next).init({
	lng: "zh", defaultNS: "logs", interpolation: { escapeValue: false },
	resources: { zh: { logs: (await import("../src/i18n/locales/zh/logs.json")).default } },
});

const searchCalls: string[] = [];
const histogramCalls: string[] = [];
let searchResponse: unknown = { items: [] };

const { MemoryRouter } = await import("react-router-dom");
const { cleanup, render, screen, fireEvent, waitFor } = await import("@testing-library/react");

const nodeCalls: string[] = [];
const instanceCalls: string[] = [];
const fleetInstances = [
	{ uuid: "6f9a1c2daaaa", state: "running" },
	{ uuid: "b0b0c0c0dddd", state: "stopped" },
];
const fleetNodes = [
	{ uuid: "n1", name: "gpu-alpha", hostname: "alpha", state: "online", gpus: [], labels: {} },
	{ uuid: "n2", name: "gpu-beta", hostname: "beta", state: "online", gpus: [], labels: {} },
];
globalThis.fetch = (async (input: RequestInfo | URL) => {
	const url = String(input);
	if (url.includes("/api/v1/logs/search")) {
		searchCalls.push(url);
		return new Response(JSON.stringify(searchResponse), { status: 200 });
	}
	if (url.includes("/api/v1/logs/histogram")) {
		histogramCalls.push(url);
		return new Response(JSON.stringify({ items: [], total: 0 }), { status: 200 });
	}
	if (url.includes("/api/v1/logs/context")) {
		return new Response(JSON.stringify({
			items: [
				{ id: 0, instance: "6f9a1c2daaaabbbb", node: "n1", level: "info", source: "instance", time_ms: 1699999999000, line: "loading model weights" },
				{ id: 1, instance: "6f9a1c2daaaabbbb", node: "n1", level: "error", source: "instance", time_ms: 1700000000000, line: "CUDA out of memory attempt" },
				{ id: 3, instance: "6f9a1c2daaaabbbb", node: "n1", level: "info", source: "instance", time_ms: 1700000000500, line: "engine exiting" },
			],
			match_index: 1,
		}), { status: 200 });
	}
	if (url.includes("/api/v1/instances")) {
		// One page searched on the server, as GET /instances does with page
		// parameters.
		instanceCalls.push(url);
		const q = new URL(url, "http://localhost").searchParams.get("q")?.toLowerCase() ?? "";
		const items = fleetInstances.filter((inst) => !q || inst.uuid.includes(q));
		return new Response(JSON.stringify({ items, page: 1, page_size: 50, total: items.length }), { status: 200 });
	}
	if (url.includes("/api/v1/nodes")) {
		// The node filter searches one page on the server, as GET /nodes does
		// with page parameters.
		nodeCalls.push(url);
		const q = new URL(url, "http://localhost").searchParams.get("q")?.toLowerCase() ?? "";
		const items = fleetNodes.filter((node) => !q || node.name.toLowerCase().includes(q) || node.uuid.includes(q));
		return new Response(JSON.stringify({ items, page: 1, page_size: 50, total: items.length, all: fleetNodes.length, labels: [] }), { status: 200 });
	}
	return new Response("{}", { status: 200 });
}) as typeof fetch;

const { default: LogsPage, buildQuery, highlightTokens } = await import("../src/pages/LogsPage.tsx");

afterEach(() => cleanup());

test("buildQuery combines chips with OR and quotes multi-word signatures", () => {
	assert.equal(buildQuery("gpu_memory_utilization", ["OOM"]), "(gpu_memory_utilization) AND OOM");
	assert.equal(buildQuery("", ["Traceback", "out of memory"]), `(Traceback OR "out of memory")`);
	assert.equal(buildQuery("level>warn", []), "level>warn");
	assert.equal(buildQuery("", []), "");
});

test("highlightTokens drops FTS5 structure and keeps literal terms", () => {
	assert.deepEqual(highlightTokens(`err ("CUDA out of memory" OR OOM) AND extra`), ["err", "CUDA", "out", "of", "memory", "OOM", "extra"]);
});

// One mounted page drives the whole console flow — chips, level coloring,
// short IDs, highlights, and the parameters the API actually receives. A
// single mount keeps the jsdom lifecycle equivalent to one real visit.
test("log console: chips combine, results color and highlight, filters reach the API", async () => {
	searchResponse = {
		total: 2,
		items: [
			{ id: 1, instance: "6f9a1c2daaaabbbb", node: "n1", level: "error", source: "instance", time_ms: 1700000000000, line: "CUDA out of memory attempt" },
			{ id: 2, instance: "6f9a1c2daaaabbbb", node: "n1", level: "", source: "instance", time_ms: 1700000001000, line: "plain info line" },
		],
	};
	render(<I18nextProvider i18n={i18n}><MemoryRouter initialEntries={["/logs"]}><LogsPage /></MemoryRouter></I18nextProvider>);
	await waitFor(() => assert.equal(screen.getAllByTestId("log-row").length, 2));

	// Short IDs, never the raw full UUID; error level surfaced for coloring.
	const rows = screen.getAllByTestId("log-row");
	assert.match(rows[0].textContent!, /6f9a1c2d/);
	assert.ok(!rows[0].textContent!.includes("6f9a1c2daaaabbbb"), "full UUID must not be shown raw");
	assert.equal(screen.getAllByTestId("log-level")[0].textContent, "error");

	// The default head search: instance source, newest-first page request.
	const head = searchCalls[0];
	assert.ok(head.includes("source=instance"), `expected instance source: ${head}`);

	// Chips toggle and combine: one click ORs the signature into the query.
	fireEvent.click(screen.getByText("Traceback"));
	await waitFor(() => assert.ok(searchCalls.at(-1)!.includes("q=Traceback")));
	fireEvent.click(screen.getByText("out of memory"));
	await waitFor(() => {
		const latest = decodeURIComponent(searchCalls.at(-1)!).replace(/\+/g, " ");
		assert.ok(latest.includes('q=(Traceback OR "out of memory")'), `expected combined OR query: ${latest}`);
	});

	// Toggling a chip off removes it from the query again.
	fireEvent.click(screen.getByText("out of memory"));
	await waitFor(() => {
		const latest = decodeURIComponent(searchCalls.at(-1)!).replace(/\+/g, " ");
		assert.ok(latest.includes("q=Traceback") && !latest.includes("out of memory"), `expected chip removal: ${latest}`);
	});

	// Context button opens a grep -C style window around the hit.
	fireEvent.click(screen.getAllByTestId("context-button")[0]);
	await waitFor(() => assert.ok(screen.getByText(/上下文 ±5 行/)));
	assert.ok(screen.getByText("loading model weights"));
	assert.ok(screen.getByText("engine exiting"));
});

// /logs search results arrive newest-first (arrival-order pagination), which
// puts a traceback's frames ABOVE its head row; fixtures mirror that contract.
test("traceback blocks fold their frame lines until expanded", async () => {
	searchResponse = {
		items: [
			{ id: 4, instance: "inst-tb", node: "n1", level: "info", source: "instance", time_ms: 1003, line: "non-indented line ends the block" },
			{ id: 3, instance: "inst-tb", node: "n1", level: "", source: "instance", time_ms: 1002, line: "    sys.exit(main())" },
			{ id: 2, instance: "inst-tb", node: "n1", level: "", source: "instance", time_ms: 1001, line: '  File "/vllm-workspace/serve.py", line 122, in cmd' },
			{ id: 1, instance: "inst-tb", node: "n1", level: "error", source: "instance", time_ms: 1000, line: "Traceback (most recent call last):" },
		],
	};
	render(<I18nextProvider i18n={i18n}><MemoryRouter initialEntries={["/logs"]}><LogsPage /></MemoryRouter></I18nextProvider>);
	await waitFor(() => assert.ok(screen.getAllByTestId("traceback-toggle").length === 1));

	// Frames are folded: the frame text is absent until expanded.
	assert.equal(screen.queryByText(/serve.py/), null);
	fireEvent.click(screen.getByTestId("traceback-toggle"));
	assert.ok(screen.getByText(/serve.py/));
	assert.ok(screen.getByText("sys.exit(main())"));
	// The expanded block reads chronologically: the first frame line renders
	// above the second one, the way the engine printed it.
	const firstFrame = screen.getByText(/serve.py/);
	const secondFrame = screen.getByText("sys.exit(main())");
	assert.ok(!!(firstFrame.compareDocumentPosition(secondFrame) & Node.DOCUMENT_POSITION_FOLLOWING), "frames must render in chronological order");
	// The non-indented INFO line is NOT part of the block.
	assert.ok(screen.getByText("non-indented line ends the block"));
});

// Regression shape from gpu3 (2026-09-19): SGLang embeds the traceback head at
// the end of its ERROR message, and the frames follow chronologically — in the
// newest-first list they sit above the head. This used to yield a "expand 0
// frames" button that expanded nothing while the frames leaked as loose rows.
test("sglang error line embedding the traceback head folds its frames", async () => {
	const frame = (id: number, line: string) => ({ id: id, instance: "2e74d682", node: "n1", level: "", source: "instance", time_ms: 1700000000000 + id, line: line });
	searchResponse = {
		items: [
			frame(6, "RuntimeError: RMSNorm failed with error code no kernel image is available"),
			frame(5, '  File "layernorm.py", line 133, in forward_cuda'),
			frame(4, "    batch_result = self.run_batch(batch)"),
			frame(3, "    scheduler.event_loop_overlap()"),
			frame(2, '  File "scheduler.py", line 2974, in run_scheduler_loop'),
			{ id: 1, instance: "2e74d682", node: "n1", level: "error", source: "instance", time_ms: 1700000000001, line: "[2026-09-19 13:15:07 110] ERROR Scheduler hit an exception: Traceback (most recent call last):" },
		],
	};
	render(<I18nextProvider i18n={i18n}><MemoryRouter initialEntries={["/logs"]}><LogsPage /></MemoryRouter></I18nextProvider>);
	const toggle = await waitFor(() => screen.getByTestId("traceback-toggle"));
	// The four indented frames fold; the unindented RuntimeError line ends the
	// block (pinned semantics) and stays visible as its own row.
	assert.ok(toggle.textContent!.includes("4"), `toggle should offer 4 frames, got ${toggle.textContent}`);
	assert.ok(screen.getByText(/RMSNorm/), "the exception line stays visible outside the fold");
	assert.equal(screen.queryByText(/layernorm\.py/), null);
	fireEvent.click(toggle);
	assert.ok(screen.getByText(/layernorm\.py/));
	assert.ok(screen.getByText(/scheduler\.py/));
});

// A head whose frames scrolled past the loaded page (it is the newest row)
// folds to an empty block; the row must render unfolded with the context
// entry point, never as a dead "expand 0 frames" toggle.
test("traceback head without frames on the page renders unfolded", async () => {
	searchResponse = {
		items: [
			{ id: 2, instance: "inst-tb", node: "n1", level: "error", source: "instance", time_ms: 1001, line: "[2026-09-19 13:15:07 110] ERROR Scheduler hit an exception: Traceback (most recent call last):" },
			{ id: 1, instance: "inst-tb", node: "n1", level: "info", source: "instance", time_ms: 1000, line: "earlier plain line" },
		],
	};
	render(<I18nextProvider i18n={i18n}><MemoryRouter initialEntries={["/logs"]}><LogsPage /></MemoryRouter></I18nextProvider>);
	await waitFor(() => assert.ok(screen.getByText(/Scheduler hit an exception/)));
	assert.equal(screen.queryByTestId("traceback-toggle"), null);
	assert.ok(screen.getAllByTestId("context-button").length >= 1, "the head row keeps the context entry point");
});


// Unstamped rows (empty Level, from agents that predate classification) must
// still render histogram volume — a store of mostly-old rows used to draw a
// blank chart because only the five known level keys were summed.
test("histogram renders bars for buckets whose rows have no level", async () => {
	const { default: Histogram } = await import("../src/pages/LogsHistogram.tsx");
	render(<I18nextProvider i18n={i18n}><Histogram buckets={[
		{ start_ms: 1000, counts: { "": 40 } },
		{ start_ms: 2000, counts: { info: 5, error: 2 } },
	]} /></I18nextProvider>);
	const bars = [...document.querySelectorAll('[role="img"]')];
	assert.equal(bars.length, 2);
	// The 40 unstamped rows tower over the 7 stamped ones: both bars visible,
	// first taller than the second.
	const h = (el: Element) => parseFloat((el as HTMLElement).style.height);
	assert.ok(h(bars[0]) > h(bars[1]), `unstamped volume must dominate: ${h(bars[0])} vs ${h(bars[1])}`);
	assert.ok(h(bars[0]) > 3, "the unstamped bar must be visible, not the 3% floor");
});

// The service monitor page's per-line "→" link deep-links here with both an
// instance and an absolute from/to (/logs?instance=...&from=...&to=...);
// this seeds the console into that exact window on mount, one-way.
test("a deep link's absolute from/to seeds the search into a fixed custom window instead of a sliding preset", async () => {
	searchResponse = { items: [] };
	render(
		<I18nextProvider i18n={i18n}>
			<MemoryRouter initialEntries={["/logs?instance=6f9a1c2daaaabbbb&from=1000&to=5000"]}><LogsPage /></MemoryRouter>
		</I18nextProvider>,
	);
	await waitFor(() => assert.ok(searchCalls.length > 0));
	const latest = searchCalls.at(-1)!;
	assert.ok(latest.includes("instance=6f9a1c2daaaabbbb"), `expected the seeded instance: ${latest}`);
	assert.ok(latest.includes("from=1000"), `expected the seeded from: ${latest}`);
	assert.ok(latest.includes("to=5000"), `expected the seeded to: ${latest}`);
});

test("a to-before-from absolute window falls back to the normal preset default", async () => {
	searchResponse = { items: [] };
	render(
		<I18nextProvider i18n={i18n}>
			<MemoryRouter initialEntries={["/logs?from=5000&to=1000"]}><LogsPage /></MemoryRouter>
		</I18nextProvider>,
	);
	await waitFor(() => assert.ok(searchCalls.length > 0));
	assert.equal(searchCalls.at(-1)!.includes("to="), false, "an invalid window must not reach the request at all");
});

test("an absolute window missing one bound falls back to the normal preset default", async () => {
	searchResponse = { items: [] };
	render(
		<I18nextProvider i18n={i18n}>
			<MemoryRouter initialEntries={["/logs?from=1000"]}><LogsPage /></MemoryRouter>
		</I18nextProvider>,
	);
	await waitFor(() => assert.ok(searchCalls.length > 0));
	assert.equal(searchCalls.at(-1)!.includes("to="), false, "a from with no to must not enter custom mode");
});

test("a non-numeric absolute window falls back to the normal preset default", async () => {
	searchResponse = { items: [] };
	render(
		<I18nextProvider i18n={i18n}>
			<MemoryRouter initialEntries={["/logs?from=abc&to=def"]}><LogsPage /></MemoryRouter>
		</I18nextProvider>,
	);
	await waitFor(() => assert.ok(searchCalls.length > 0));
	assert.equal(searchCalls.at(-1)!.includes("to="), false, "Number(\"abc\") is NaN, which must never reach the request as a bound");
});

test("a deep link's absolute window shows as the localized custom choice, which the dropdown also offers", async () => {
	searchResponse = { items: [] };
	render(
		<I18nextProvider i18n={i18n}>
			<MemoryRouter initialEntries={["/logs?instance=6f9a1c2daaaabbbb&from=1000&to=5000"]}><LogsPage /></MemoryRouter>
		</I18nextProvider>,
	);
	await waitFor(() => assert.ok(searchCalls.length > 0));

	const select = screen.getByTestId("range-select");
	assert.equal(select.querySelector(".ant-select-selection-item")?.textContent, "自定义");

	fireEvent.mouseDown(select.querySelector(".ant-select-selector") as HTMLElement);
	const listed = [...document.querySelectorAll(".ant-select-item-option-content")].map((el) => el.textContent);
	assert.ok(listed.includes("自定义"), "custom is a choosable option");
	assert.ok(listed.includes("最近 24 小时"), "the normal presets are all still there");
});

test("the node filter searches nodes on the server and filters the logs by the one picked", async () => {
	searchResponse = { total: 0, items: [] };
	nodeCalls.length = 0;
	render(<I18nextProvider i18n={i18n}><MemoryRouter initialEntries={["/logs"]}><LogsPage /></MemoryRouter></I18nextProvider>);
	const picker = await screen.findByRole("combobox", { name: "节点" });
	await waitFor(() => assert.ok(nodeCalls.length > 0));
	assert.ok(nodeCalls.every((url) => url.includes("page=1") && url.includes("page_size=50")), `the node list was fetched whole: ${nodeCalls}`);
	fireEvent.change(picker, { target: { value: "beta" } });
	await waitFor(() => assert.ok(nodeCalls.at(-1)!.includes("q=beta")));
	let option: Element | null = null;
	await waitFor(() => assert.ok((option = document.querySelector('.ant-select-item-option[title="gpu-beta"]'))));
	assert.equal(document.querySelector('.ant-select-item-option[title="gpu-alpha"]'), null);
	fireEvent.click(option!);
	await waitFor(() => assert.ok(searchCalls.at(-1)!.includes("node=n2"), `expected the node filter in ${searchCalls.at(-1)}`));
});

test("the instance filter searches instances on the server instead of listing them all", async () => {
	searchResponse = { total: 0, items: [] };
	instanceCalls.length = 0;
	render(<I18nextProvider i18n={i18n}><MemoryRouter initialEntries={["/logs"]}><LogsPage /></MemoryRouter></I18nextProvider>);
	const picker = await screen.findByRole("combobox", { name: i18n.t("search.instancePlaceholder") });
	await waitFor(() => assert.ok(instanceCalls.length > 0));
	assert.ok(instanceCalls.every((url) => url.includes("page=1") && url.includes("page_size=50")), `the instance list was fetched whole: ${instanceCalls}`);
	fireEvent.change(picker, { target: { value: "b0b0" } });
	await waitFor(() => assert.ok(instanceCalls.at(-1)!.includes("q=b0b0"), `expected a server search: ${instanceCalls.at(-1)}`));
	let option: Element | null = null;
	await waitFor(() => assert.ok((option = document.querySelector('.ant-select-item-option[title="b0b0c0c0dddd"]'))));
	assert.equal(document.querySelector('.ant-select-item-option[title="6f9a1c2daaaa"]'), null);
	fireEvent.click(option!);
	await waitFor(() => assert.ok(searchCalls.at(-1)!.includes("instance=b0b0c0c0dddd"), `expected the instance filter in ${searchCalls.at(-1)}`));
});

// Phase 0: selected text alone does not name an AntD combobox for assistive technology.
test("log filters and automatic refresh have localized accessible names", async () => {
  searchResponse = { items: [] };
  render(<I18nextProvider i18n={i18n}><MemoryRouter><LogsPage /></MemoryRouter></I18nextProvider>);
  await screen.findByText(i18n.t("search.empty"));
  assert.ok(screen.getByRole("combobox", { name: "日志来源" }));
  assert.ok(screen.getByRole("combobox", { name: "时间范围" }));
  assert.ok(screen.getByRole("combobox", { name: i18n.t("search.levelPlaceholder") }));
  assert.equal(screen.getByRole("switch", { name: i18n.t("search.autoRefresh") }).getAttribute("aria-checked"), "false");
});

// These assertions pin the actual wire query; real SQLite and browser tests
// separately verify its matches instead of relying on this response stub.
test("text and quick filters send explicit grouped AND to search and histogram", async () => {
 searchCalls.length = 0;
 histogramCalls.length = 0;
 searchResponse = { items: [] };
 render(<I18nextProvider i18n={i18n}><MemoryRouter initialEntries={["/logs"]}><LogsPage /></MemoryRouter></I18nextProvider>);
 const input = screen.getByPlaceholderText(i18n.t("search.placeholder"));
 const expectQuery = async (query: string) => {
  await waitFor(() => {
   for (const calls of [searchCalls, histogramCalls]) {
    assert.equal(new URL(calls.at(-1)!, "http://localhost").searchParams.get("q"), query);
   }
  });
 };
 fireEvent.change(input, { target: { value: "WARNING" } });
 await expectQuery("WARNING");
 fireEvent.click(screen.getByText("WARNING"));
 await expectQuery("(WARNING) AND WARNING");
 fireEvent.click(screen.getByText("ERROR"));
 await expectQuery("(WARNING) AND (WARNING OR ERROR)");
 fireEvent.change(input, { target: { value: 'WARNING OR "cache miss"' } });
 await expectQuery('(WARNING OR "cache miss") AND (WARNING OR ERROR)');
 fireEvent.click(screen.getByText("ERROR"));
 await expectQuery('(WARNING OR "cache miss") AND WARNING');
 fireEvent.click(screen.getByText("WARNING"));
 await expectQuery('WARNING OR "cache miss"');
 fireEvent.change(input, { target: { value: '  "cache miss"  ' } });
 fireEvent.click(screen.getByText("out of memory"));
 await expectQuery('("cache miss") AND "out of memory"');
 fireEvent.change(input, { target: { value: " " } });
 await expectQuery('"out of memory"');
});

// Phase 0: quick filters must be reachable and announce their selected state;
// API stubs here pin interactions only, while browser E2E checks real matches.
test("all log quick filters support Tab, Enter, Space and pointer toggling", async () => {
  searchCalls.length = 0;
  histogramCalls.length = 0;
  searchResponse = { items: [] };
  const { default: userEvent } = await import("@testing-library/user-event");
  const user = userEvent.setup({ document: dom.window.document });
  render(<I18nextProvider i18n={i18n}><MemoryRouter><LogsPage /></MemoryRouter></I18nextProvider>);
  const labels = ["Traceback", "ERROR", "WARNING", "ERR99999", "out of memory", "no such container"];
  const buttons = labels.map((name) => screen.getByRole("button", { name, exact: true, pressed: false }));
  const input = screen.getByPlaceholderText(i18n.t("search.placeholder"));
  await user.type(input, "marker");
  const expectQuery = async (query: string) => {
    await waitFor(() => {
      for (const calls of [searchCalls, histogramCalls]) {
        assert.equal(new URL(calls.at(-1)!, "http://localhost").searchParams.get("q"), query);
      }
    });
  };
  await expectQuery("marker");
  // Start at the text field and traverse the actual tab order, including filters.
  for (let step = 0; step < 20 && document.activeElement !== buttons[0]; step++) await user.tab();
  assert.equal(document.activeElement, buttons[0]);
  for (let index = 0; index < labels.length; index++) {
    const button = buttons[index];
    const label = labels[index];
    assert.equal(document.activeElement, button, `${label} participates in tab order`);
    assert.equal(button.getAttribute("aria-pressed"), "false");
    const expected = `(marker) AND ${label.includes(" ") ? `"${label}"` : label}`;
    await user.keyboard("{Enter}");
    assert.equal(button.getAttribute("aria-pressed"), "true");
    await expectQuery(expected);
    await user.keyboard(" ");
    assert.equal(button.getAttribute("aria-pressed"), "false");
    await expectQuery("marker");
    await user.click(button);
    assert.equal(button.getAttribute("aria-pressed"), "true");
    await expectQuery(expected);
    await user.click(button);
    assert.equal(button.getAttribute("aria-pressed"), "false");
    await expectQuery("marker");
    assert.equal((input as HTMLInputElement).value, "marker", "toggling preserves free text");
    await user.tab();
  }
});

test("histogram bars are inert images without onPick and keyboard buttons with it", async () => {
	const { default: Histogram } = await import("../src/pages/LogsHistogram.tsx");
	const buckets = [{ start_ms: 2000, counts: { info: 1 } }, { start_ms: 1000, counts: { error: 1 } }];
	const { unmount } = render(<I18nextProvider i18n={i18n}><Histogram buckets={buckets} /></I18nextProvider>);
	assert.equal(document.querySelectorAll('[role="button"]').length, 0);
	assert.equal(document.querySelectorAll('[role="img"]').length, 2);
	unmount();
	const picked: number[] = [];
	render(<I18nextProvider i18n={i18n}><Histogram buckets={buckets} onPick={(ms) => picked.push(ms)} /></I18nextProvider>);
	const bars = [...document.querySelectorAll('[role="button"]')] as HTMLElement[];
	assert.equal(bars.length, 2);
	assert.equal(bars[0].tabIndex, 0);
	// Oldest first, whatever order the server sent.
	fireEvent.click(bars[0]);
	fireEvent.keyDown(bars[1], { key: " " });
	fireEvent.keyDown(bars[1], { key: "a" });
	assert.deepEqual(picked, [1000, 2000]);
});
