import assert from "node:assert/strict";
import fs from "node:fs";
import { afterEach, test } from "node:test";
import { JSDOM } from "jsdom";
import React from "react";
import { createInstance } from "i18next";
import { I18nextProvider, initReactI18next } from "react-i18next";

const dom = new JSDOM("<!doctype html><html><body></body></html>", { url: "http://localhost/monitor" });
for (const name of ["window", "document", "navigator", "HTMLElement", "SVGElement", "Element", "Node", "MutationObserver", "ShadowRoot"] as const) {
  Object.defineProperty(globalThis, name, { value: dom.window[name], configurable: true });
}
Object.defineProperty(globalThis, "getComputedStyle", { value: (element: Element) => dom.window.getComputedStyle(element), configurable: true });
Object.defineProperty(dom.window, "matchMedia", { configurable: true, value: () => ({ matches: false, addListener() {}, removeListener() {}, addEventListener() {}, removeEventListener() {}, dispatchEvent: () => false }) });
class ResizeObserverStub { observe() {} unobserve() {} disconnect() {} }
Object.defineProperty(globalThis, "ResizeObserver", { value: ResizeObserverStub, configurable: true });

// echarts needs a 2d context JSDOM cannot provide (ClusterTokensChart and
// MetricExplorer both mount a TimeSeriesChart); the same catch-all proxy the
// existing inference-metrics tests use keeps chart mounting alive here too.
const anyContext = new Proxy(function () {} as never, {
  get: (_target: never, prop: string | symbol) => {
    if (prop === "canvas") return { width: 300, height: 200, style: {} };
    if (prop === "dpr") return 1;
    if (prop === "measureText") return () => ({ width: 10 });
    return anyContext;
  },
  set: () => true,
  apply: () => anyContext,
});
dom.window.HTMLCanvasElement.prototype.getContext = () => anyContext as CanvasRenderingContext2D;

class Events {
  static listeners = new Map<string, Set<EventListener>>();
  addEventListener(type: string, listener: EventListener) {
    const listeners = Events.listeners.get(type) ?? new Set<EventListener>();
    listeners.add(listener);
    Events.listeners.set(type, listeners);
  }
  removeEventListener(type: string, listener: EventListener) { Events.listeners.get(type)?.delete(listener); }
  close() {}
}
Object.defineProperty(globalThis, "EventSource", { value: Events, configurable: true });

const i18n = createInstance();
await i18n.use(initReactI18next).init({
  lng: "en",
  fallbackLng: "en",
  resources: {
    en: {
      monitoring: JSON.parse(fs.readFileSync(new URL("../src/i18n/locales/en/monitoring.json", import.meta.url), "utf8")),
      common: JSON.parse(fs.readFileSync(new URL("../src/i18n/locales/en/common.json", import.meta.url), "utf8")),
    },
  },
  defaultNS: "monitoring",
  interpolation: { escapeValue: false },
});

const { MemoryRouter } = await import("react-router-dom");
const { cleanup, render, screen, waitFor, act } = await import("@testing-library/react");
const userEvent = (await import("@testing-library/user-event")).default;
const { api } = await import("../src/api.ts");
const { formatTimeSec } = await import("../src/i18n/datetime.ts");
const { default: MonitorPage } = await import("../src/pages/monitor/MonitorPage.tsx");
import type { MonitorOverview } from "../src/api";

const overview = JSON.parse(fs.readFileSync(new URL("./fixtures/monitor-overview.json", import.meta.url), "utf8")) as MonitorOverview;
const originalOverview = api.monitorOverview;
const originalFetch = globalThis.fetch;
const distributionCalls: Array<[string, number | undefined]> = [];
const DISTRIBUTIONS: typeof api.monitorDistributions = async (range, deploymentId) => {
  distributionCalls.push([range, deploymentId]);
  return {
    from_ms: 0, to_ms: 0, token_buckets: [0, 2, 4, 8, 16, 32, 64, 128, 256, 512, 1024, 2048], ttft_bounds_ms: [100],
    input_output: [{ x: 10, y: 7, n: 30 }, { x: 11, y: 8, n: 10 }],
    uncached_ttft: [], coverage: { total: 50, with_usage: 40, with_cache: 0 },
  };
};
api.monitorDistributions = DISTRIBUTIONS;

afterEach(() => {
  cleanup();
  api.monitorOverview = originalOverview;
  globalThis.fetch = originalFetch;
  api.monitorDistributions = DISTRIBUTIONS;
  distributionCalls.length = 0;
});

function renderPage() {
  return render(
    <I18nextProvider i18n={i18n}>
      <MemoryRouter initialEntries={["/monitor"]}>
        <MonitorPage />
      </MemoryRouter>
    </I18nextProvider>,
  );
}

test("shows a loading skeleton before the first response arrives", async () => {
  let resolve!: (value: MonitorOverview) => void;
  api.monitorOverview = () => new Promise((r) => { resolve = r; });
  const { container } = renderPage();
  assert.ok(container.querySelector(".ant-skeleton"));
  resolve(overview);
  await waitFor(() => assert.ok(screen.getByText("Qwen3-32B")));
});

test("shows an error alert with a retry action when the request fails", async () => {
  let calls = 0;
  api.monitorOverview = async () => { calls += 1; throw new Error("network down"); };
  renderPage();
  await waitFor(() => assert.ok(screen.getByRole("alert")));
  assert.ok(screen.getByText("network down"));
  assert.equal(calls, 1);
});

test("shows an empty state when there are no services to monitor", async () => {
  api.monitorOverview = async () => ({ ...overview, services: [] });
  renderPage();
  await waitFor(() => assert.ok(screen.getByText("No deployments to monitor yet")));
});

test("renders signals, the service state counts and the service table on success", async () => {
  api.monitorOverview = async () => overview;
  renderPage();
  await waitFor(() => assert.ok(screen.getByText("Qwen3-32B")));
  assert.ok(screen.getByRole("button", { name: /Ready replicas/ }));
  assert.ok(screen.getByText("Service state"));
  assert.equal(screen.queryByText("Health checks"), null);
  // One link per state present, serving first.
  const counts = ["Running 8", "Starting 1", "Asleep 1", "Failed 1"];
  const links = counts.map((name) => screen.getByRole("button", { name }));
  for (let i = 1; i < links.length; i++) {
    assert.ok(links[i - 1].compareDocumentPosition(links[i]) & dom.window.Node.DOCUMENT_POSITION_FOLLOWING, `${counts[i - 1]} before ${counts[i]}`);
  }
  assert.ok(screen.getByText(`Data as of ${formatTimeSec(overview.as_of)}`));
});

test("shows the stale sentence, not just a color change, when the server marks data stale", async () => {
  api.monitorOverview = async () => ({ ...overview, stale: true });
  renderPage();
  await waitFor(() => assert.ok(screen.getByText(/Data as of/)));
  // A color-only signal is invisible to plain-text assertions, colorblind
  // users and grayscale captures alike; the stale sentence must render as
  // its own text next to the caption.
  assert.ok(screen.getByText(/Stale — no fresh samples/));
});

test("does not show the stale sentence when the server marks data fresh", async () => {
  api.monitorOverview = async () => ({ ...overview, stale: false });
  renderPage();
  await waitFor(() => assert.ok(screen.getByText(/Data as of/)));
  assert.equal(screen.queryByText(/Stale/), null);
});

test("as_of <= 0 shows a 'no samples yet' caption instead of the 1970 epoch", async () => {
  api.monitorOverview = async () => ({ ...overview, as_of: 0 });
  renderPage();
  await waitFor(() => assert.ok(screen.getByText("No samples yet")));
  assert.equal(screen.queryByText(/1970/), null);
  assert.equal(screen.queryByText(/Data as of/), null);
});

test("clicking the ready replicas tile narrows the table to the services short of replicas", async () => {
  api.monitorOverview = async () => overview;
  renderPage();
  await waitFor(() => assert.ok(screen.getByText("Qwen3-32B")));
  screen.getByRole("button", { name: /^Ready replicas/ }).click();
  await waitFor(() => assert.ok(screen.getByText("Clear filter")));
  assert.ok(screen.getByText("Llama-3.1-70B"));
  assert.ok(screen.getByText("ChatGLM3-6B"));
  assert.equal(screen.queryByText("Qwen3-32B"), null);
});

test("clicking a service state count narrows the table to the services in that state", async () => {
  api.monitorOverview = async () => overview;
  renderPage();
  await waitFor(() => assert.ok(screen.getByText("Qwen3-32B")));
  screen.getByRole("button", { name: "Starting 1" }).click();
  await waitFor(() => assert.ok(screen.getByText("Clear filter")));
  assert.ok(screen.getByText("Llama-3.1-70B"));
  assert.equal(screen.queryByText("Qwen3-32B"), null);
  screen.getByText("Clear filter").click();
  await waitFor(() => assert.ok(screen.getByText("Qwen3-32B")));
});

test("a fleet total signal has no per-service verdict behind it: clicking it clears any filter instead of narrowing", async () => {
  api.monitorOverview = async () => overview;
  renderPage();
  await waitFor(() => assert.ok(screen.getByText("Qwen3-32B")));
  screen.getByRole("button", { name: "Starting 1" }).click();
  await waitFor(() => assert.ok(screen.getByText("Clear filter")));
  screen.getByRole("button", { name: /^Waiting p95/ }).click();
  await waitFor(() => assert.equal(screen.queryByText("Clear filter") === null, true));
  assert.ok(screen.getByText("Qwen3-32B"));
  assert.ok(screen.getByText("Qwen2.5-7B-Instruct"));
});

test("an uncollected signal tile is not clickable and is marked aria-disabled", async () => {
  // Every signal is collected in the fixture (phase 2); force one back to
  // collected:false to exercise the not-collected rendering path this test
  // is actually about, rather than depending on which signal the fixture
  // happens to leave uncollected.
  const uncollected = { ...overview, signals: overview.signals.map((s) => (s.key === "tpot" ? { key: s.key, collected: false } : s)) };
  api.monitorOverview = async () => uncollected;
  renderPage();
  await waitFor(() => assert.ok(screen.getByText("Qwen3-32B")));
  const tpotTile = screen.getByRole("button", { name: /^TPOT/ });
  assert.equal(tpotTile.getAttribute("aria-disabled"), "true");
  tpotTile.click();
  // Clicking must not produce a filter chip - there is nothing to filter by.
  assert.equal(screen.queryByText("Clear filter"), null);
});

test("the request_rate signal is collected:false in the fixture and renders the muted not-collected state", async () => {
  api.monitorOverview = async () => overview;
  renderPage();
  await waitFor(() => assert.ok(screen.getByText("Qwen3-32B")));
  const requestRateTile = screen.getByRole("button", { name: /^RPS \/ Concurrency/ });
  assert.equal(requestRateTile.getAttribute("aria-disabled"), "true");
  assert.ok(requestRateTile.textContent?.includes("Not collected"));
});

test("the preemptions signal carries detail.no_recent_requests in the fixture - it annotates the real (0) value rather than suppressing it", async () => {
  api.monitorOverview = async () => overview;
  renderPage();
  await waitFor(() => assert.ok(screen.getByText("Qwen3-32B")));
  const preemptionsTile = screen.getByRole("button", { name: /^Preemptions\/s/ });
  // Still collected (clickable, no aria-disabled), and the value row keeps
  // showing the real value - only the detail row adds the annotation.
  assert.equal(preemptionsTile.hasAttribute("aria-disabled"), false);
  assert.ok(preemptionsTile.textContent?.includes("No recent requests"));
  assert.equal(preemptionsTile.textContent?.includes("Not collected"), false);
});

test("the token_rate signal labels its two numbers Input/Output instead of a bare a / b", async () => {
  api.monitorOverview = async () => overview;
  renderPage();
  await waitFor(() => assert.ok(screen.getByText("Qwen3-32B")));
  const tokenTile = screen.getByRole("button", { name: /^Token throughput/ });
  assert.ok(tokenTile.textContent?.includes("Input"));
  assert.ok(tokenTile.textContent?.includes("Output"));
});

test("the collected error_rate signal shows its 5xx/429/503 sub-numbers", async () => {
  api.monitorOverview = async () => overview;
  renderPage();
  await waitFor(() => assert.ok(screen.getByText("Qwen3-32B")));
  const errorRateTile = screen.getByRole("button", { name: /^Error rate/ });
  assert.ok(errorRateTile.textContent?.includes("5xx 0.6"), errorRateTile.textContent ?? "");
  assert.ok(errorRateTile.textContent?.includes("429 2"), errorRateTile.textContent ?? "");
  assert.ok(errorRateTile.textContent?.includes("503 0.3"), errorRateTile.textContent ?? "");
});

test("ttft/tpot carry an upper-bound tooltip on the tile", async () => {
  api.monitorOverview = async () => overview;
  renderPage();
  await waitFor(() => assert.ok(screen.getByText("Qwen3-32B")));
  const ttftTile = screen.getByRole("button", { name: /^TTFT p95 \/ p99:/ });
  assert.ok(ttftTile.getAttribute("title")?.includes("upper bound"));
});

test("the cluster input/output card replaces VRAM by node and reads the whole cluster's distribution", async () => {
  api.monitorOverview = async () => overview;
  renderPage();
  await waitFor(() => assert.ok(screen.getByRole("grid", { name: "Input × output" })));
  assert.equal(screen.queryByText("VRAM by node"), null);
  assert.ok(screen.getByRole("img", { name: "About Input and output sizes" }));
  assert.deepEqual(distributionCalls[0], ["1h", undefined]);
  assert.ok(screen.getByRole("gridcell", { name: "Input 1K–2K · output 128–256: 30 requests (75%)" }));
  assert.ok(screen.getByText("Covers 80% of 50 requests (the rest returned no usage)"));
});

test("the cluster input/output card shows an error with a retry when the distribution fails", async () => {
  api.monitorOverview = async () => overview;
  api.monitorDistributions = async () => {
    throw new Error("ux.db unavailable");
  };
  renderPage();
  await waitFor(() => assert.ok(screen.getByText("Failed to load the request distribution")));
  assert.ok(screen.getByText("ux.db unavailable"));
});

function emitInstanceChange() {
  for (const listener of Events.listeners.get("instance") ?? []) {
    listener(new dom.window.MessageEvent("change", { data: JSON.stringify({ resource: "instance" }) }));
  }
}

test("keeps existing services usable without a loading mask during a slow event refresh", async () => {
  let calls = 0;
  let resolve!: (value: MonitorOverview) => void;
  api.monitorOverview = () => ++calls === 1 ? Promise.resolve(overview) : new Promise((r) => { resolve = r; });
  const { container } = renderPage();
  await waitFor(() => assert.ok(screen.getByText("Qwen3-32B")));
  await act(async () => emitInstanceChange());
  assert.equal(calls, 2);
  assert.ok(screen.getByText("Qwen3-32B"));
  await act(async () => { await new Promise((resolve) => setTimeout(resolve, 600)); });
  assert.equal(container.querySelector(".ant-spin-spinning"), null);
  assert.equal(container.querySelector(".ant-spin-blur"), null);
  await act(async () => resolve(overview));
});

test("enabling follow keeps the current services visible without a loading mask", async () => {
  let calls = 0;
  api.monitorOverview = () => ++calls === 1 ? Promise.resolve(overview) : new Promise(() => {});
  const { container } = renderPage();
  await waitFor(() => assert.ok(screen.getByText("Qwen3-32B")));
  await userEvent.setup().click(screen.getByRole("switch", { name: "Follow latest" }));
  assert.equal(calls, 2);
  assert.ok(screen.getByText("Qwen3-32B"));
  await act(async () => { await new Promise((resolve) => setTimeout(resolve, 600)); });
  assert.equal(container.querySelector(".ant-spin-spinning"), null);
});

test("changing range hides previous data and ignores an old in-flight refresh", async () => {
  let oldResolve!: (value: MonitorOverview) => void;
  let nextResolve!: (value: MonitorOverview) => void;
  let calls = 0;
  api.monitorOverview = (range) => {
    if (range === "24h") return new Promise((r) => { nextResolve = r; });
    return ++calls === 1 ? Promise.resolve(overview) : new Promise((r) => { oldResolve = r; });
  };
  const { container } = renderPage();
  await waitFor(() => assert.ok(screen.getByText("Qwen3-32B")));
  await act(async () => emitInstanceChange());
  await userEvent.setup().click(screen.getByText("24h"));
  assert.ok(screen.getByText("Qwen3-32B").closest("[hidden]"));
  assert.ok(container.querySelector(".ant-skeleton"));
  await act(async () => oldResolve(overview));
  assert.ok(screen.getByText("Qwen3-32B").closest("[hidden]"));
  await act(async () => nextResolve({ ...overview, services: [] }));
  await waitFor(() => assert.ok(screen.getByText("No deployments to monitor yet")));
});


test("changing range preserves the selected service filter", async () => {
  api.monitorOverview = async () => overview;
  renderPage();
  await waitFor(() => assert.ok(screen.getByText("Qwen3-32B")));
  await userEvent.setup().click(screen.getByRole("button", { name: "Starting 1" }));
  await userEvent.setup().click(screen.getByText("24h"));
  await waitFor(() => assert.ok(screen.getByText("Clear filter")));
  assert.ok(screen.getByText("Llama-3.1-70B"));
  assert.equal(screen.queryByText("Qwen3-32B"), null);
});


test("changing range preserves the open metric explorer and its aggregation", async () => {
  api.monitorOverview = async () => overview;
  globalThis.fetch = (async () => new Response(JSON.stringify({ items: [], total: 0 }), { status: 200, headers: { "Content-Type": "application/json" } })) as typeof fetch;
  renderPage();
  await waitFor(() => assert.ok(screen.getByText("Qwen3-32B")));
  const user = userEvent.setup();
  await user.click(screen.getByText("Metric explorer"));
  await user.click(screen.getByRole("combobox", { name: "Aggregation" }));
  await user.click(screen.getByText("max", { selector: ".ant-select-item-option-content" }));
  await waitFor(() => assert.ok(screen.getByText(/fn=max/)));
  await user.click(screen.getByText("24h"));
  await waitFor(() => assert.ok(screen.getByRole("combobox", { name: "Aggregation" })));
  assert.ok(screen.getByText(/fn=max/));
});
