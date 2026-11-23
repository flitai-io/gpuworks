import assert from "node:assert/strict";
import fs from "node:fs";
import { afterEach, beforeEach, test } from "node:test";
import { JSDOM } from "jsdom";
import React from "react";
import { createInstance } from "i18next";
import { I18nextProvider, initReactI18next } from "react-i18next";
import { installCanvas, retentionView } from "./helpers/analyticsFixtures.ts";

const dom = new JSDOM("<!doctype html><html><body></body></html>", { url: "http://localhost/" });
for (const name of ["window", "document", "navigator", "HTMLElement", "SVGElement", "Element", "Node", "MutationObserver", "ShadowRoot"] as const) {
  Object.defineProperty(globalThis, name, { value: dom.window[name], configurable: true });
}
Object.defineProperty(globalThis, "getComputedStyle", { value: (element: Element) => dom.window.getComputedStyle(element), configurable: true });
Object.defineProperty(dom.window, "matchMedia", {
  configurable: true,
  value: (query: string) => ({
    matches: false,
    media: query,
    addListener() {}, removeListener() {}, addEventListener() {}, removeEventListener() {}, dispatchEvent: () => false,
  }),
});
class ResizeObserverStub { observe() {} unobserve() {} disconnect() {} }
Object.defineProperty(globalThis, "ResizeObserver", { value: ResizeObserverStub, configurable: true });
installCanvas(dom);

const i18n = createInstance();
await i18n.use(initReactI18next).init({
  lng: "en",
  fallbackLng: "en",
  resources: {
    en: {
      deployments: JSON.parse(fs.readFileSync(new URL("../src/i18n/locales/en/deployments.json", import.meta.url), "utf8")),
      common: JSON.parse(fs.readFileSync(new URL("../src/i18n/locales/en/common.json", import.meta.url), "utf8")),
    },
  },
  defaultNS: "deployments",
  interpolation: { escapeValue: false },
});

const { cleanup, render, screen, waitFor, within } = await import("@testing-library/react");
const { MemoryRouter } = await import("react-router-dom");
const { api } = await import("../src/api.ts");
const { default: DeploymentMetricsPanel } = await import("../src/components/DeploymentMetricsPanel.tsx");
const originalSignals = api.monitorDeploymentSignals;
const originalDataRetention = api.dataRetention;

beforeEach(() => {
  api.dataRetention = async () => retentionView();
});

afterEach(() => {
  cleanup();
  api.monitorDeploymentSignals = originalSignals;
  api.dataRetention = originalDataRetention;
});

type Line = { labels: Record<string, string>; points: Array<[number, number]> };
const empty = { start_ms: 0, end_ms: 0, step_ms: 10_000, sources: {}, concurrency: [] as Line[], kv: [] as Line[], throughput: [] as Line[] };
const line = (metric: string, points: Array<[number, number]>, instance?: string): Line => ({ labels: { metric, ...(instance ? { instance } : {}) }, points });

function serve(signals: Partial<typeof empty>) {
  api.monitorDeploymentSignals = (async () => ({ ...empty, ...signals })) as typeof api.monitorDeploymentSignals;
}

const runningInstance = { uuid: "inst-pico-1", state: "running", node_uuid: "node-1" } as never;

function renderPanel(instances: unknown[] = [runningInstance]) {
  return render(
    <MemoryRouter>
      <I18nextProvider i18n={i18n}><DeploymentMetricsPanel instances={instances as never} deploymentId={62} /></I18nextProvider>
    </MemoryRouter>,
  );
}

// Read the displayed number independently of its unit.
function statValue(title: string): string | null | undefined {
  return screen.getByText(title).closest("[data-slot='metric-card']")?.querySelector("[data-slot='metric-card-value-row'] > span")?.firstChild?.textContent;
}

function chartCard(title: string): HTMLElement {
  return screen.getByText(title).closest(".deployment-trend-card") as HTMLElement;
}

// The deployment that showed an empty token chart: its engine reports running,
// waiting and cache usage but no token counters, while the gateway counts the
// tokens of the replies it forwards. The server resolves the token signals to
// the gateway and the rest to the engine; the panel draws each as resolved.
test("an engine without token counters still shows token throughput, from the gateway's lines", async () => {
  serve({
    sources: { running: "engine", prompt_tokens: "gateway", generation_tokens: "gateway" },
    concurrency: [line("running", [[1000, 2], [2000, 2]], "inst-pico-1"), line("waiting", [[1000, 0], [2000, 1]], "inst-pico-1")],
    kv: [line("usage_pct", [[1000, 20], [2000, 25]], "inst-pico-1")],
    throughput: [line("prompt_tok_s", [[1000, 10], [2000, 12.5]]), line("generation_tok_s", [[1000, 80], [2000, 88.5]])],
  });
  renderPanel();
  await waitFor(() => assert.equal(statValue("Generation Throughput"), "88.5"));
  assert.ok(screen.getByText("Prompt 12.5 tok/s · Generation 88.5 tok/s"));
  await waitFor(() => assert.ok(chartCard("Token Throughput").querySelector("canvas"), "the token chart has data"));
  // The signals the engine does report stay the engine's.
  assert.equal(statValue("Running Requests"), "2");
  assert.equal(statValue("Waiting Requests"), "1");
  assert.equal(statValue("Peak GPU Cache"), "25");
  assert.ok(!screen.queryByText("No data"));
  // Some signals are the engine's, so nothing says the engine reports none.
  assert.ok(!screen.queryByText("The engine reports no inference metrics"));
});

test("an engine that reports tokens keeps its own per-instance lines, and a stopped replica is not part of now", async () => {
  serve({
    sources: { running: "engine", prompt_tokens: "engine", generation_tokens: "engine" },
    concurrency: [line("running", [[2000, 3]], "inst-live"), line("running", [[1000, 9]], "inst-stopped")],
    kv: [line("usage_pct", [[2000, 40]], "inst-live"), line("usage_pct", [[1000, 90]], "inst-stopped")],
    throughput: [
      line("prompt_tok_s", [[2000, 30]], "inst-live"), line("generation_tok_s", [[2000, 20]], "inst-live"),
      line("generation_tok_s", [[1000, 700]], "inst-stopped"),
    ],
  });
  renderPanel([{ uuid: "inst-live", state: "running" }, { uuid: "inst-stopped", state: "stopped" }]);
  await waitFor(() => assert.equal(statValue("Generation Throughput"), "20.0"));
  assert.equal(statValue("Running Requests"), "3");
  assert.equal(statValue("Peak GPU Cache"), "40");
  assert.ok(screen.getByText("Prompt 30.0 tok/s · Generation 20.0 tok/s"));
});

test("an engine without metrics shows what the gateway measured, with the way to the service monitor", async () => {
  serve({
    sources: { running: "gateway", prompt_tokens: "gateway", generation_tokens: "gateway" },
    concurrency: [line("running", [[2000, 3]])],
    throughput: [line("generation_tok_s", [[2000, 88.5]])],
  });
  renderPanel();
  await waitFor(() => assert.ok(screen.getByText("The engine reports no inference metrics")));
  // The gateway's deployment-wide lines carry no instance and are the live now.
  assert.equal(statValue("Running Requests"), "3");
  assert.equal(statValue("Generation Throughput"), "88.5");
  assert.equal(screen.getByRole("link", { name: "Open the service monitor" }).getAttribute("href"), "/monitor/d/62");
  assert.equal(screen.queryByText("Waiting for Deployment Metrics"), null);
});

test("a deployment with nothing live has no 'now' even when the gateway has a last reading", async () => {
  serve({
    sources: { running: "gateway", generation_tokens: "gateway" },
    concurrency: [line("running", [[2000, 3]])],
    throughput: [line("generation_tok_s", [[2000, 88.5]])],
  });
  renderPanel([{ uuid: "inst-gone", state: "stopped" }]);
  await waitFor(() => assert.ok(screen.getByText("The engine reports no inference metrics")));
  assert.equal(statValue("Running Requests"), "—");
  assert.equal(statValue("Generation Throughput"), "—");
});

test("an idle engine that reports nothing says so rather than waiting", async () => {
  serve({});
  renderPanel();
  await waitFor(() => assert.ok(screen.getByText("No engine metrics, and no requests through the gateway in the selected range")));
  assert.equal(screen.queryByText("Waiting for Deployment Metrics"), null);
  assert.equal(screen.getByRole("link", { name: "Open the service monitor" }).getAttribute("href"), "/monitor/d/62");
});

test("unknown retention does not mistake absent historical samples for an engine without metrics", async () => {
  serve({});
  api.dataRetention = async () => { throw new Error("service unavailable"); };
  renderPanel();
  assert.ok(await screen.findByText("Unable to confirm metric retention. The selected range may include expired data."));
  assert.equal(screen.queryByText("No engine metrics, and no requests through the gateway in the selected range"), null);
});

test("an instance still starting keeps the panel waiting", async () => {
  serve({});
  renderPanel([{ uuid: "inst-starting", state: "starting", node_uuid: "node-1" }]);
  await waitFor(() => assert.ok(screen.getByText("Waiting for Deployment Metrics")));
  assert.equal(screen.queryByText("No engine metrics, and no requests through the gateway in the selected range"), null);
});

test("while the panel is loading it waits, with every chart empty and no verdict about the engine", () => {
  api.monitorDeploymentSignals = (() => new Promise(() => {})) as typeof api.monitorDeploymentSignals;
  renderPanel();
  assert.ok(screen.getByText("Waiting for Deployment Metrics"));
  assert.equal(screen.getAllByText("No data").length, 3);
  assert.equal(screen.queryByText("The engine reports no inference metrics"), null);
});

test("a failed read is reported with the server's message and no chart claims data", async () => {
  api.monitorDeploymentSignals = (async () => { throw new Error("signals unavailable"); }) as typeof api.monitorDeploymentSignals;
  renderPanel();
  const alert = await screen.findByText("signals unavailable");
  assert.ok(within(alert.closest("[role='alert']") as HTMLElement).getByText("Failed to Load Inference Metrics"));
  assert.equal(screen.queryByText("Waiting for Deployment Metrics"), null);
  assert.equal(screen.getAllByText("No data").length, 3);
});

// The chart's resolved option, read from the live ECharts instance in a card.
async function chartOption(title: string) {
  const echarts = await import("echarts/core");
  const host = chartCard(title).querySelector<HTMLElement>('[role="img"]');
  assert.ok(host, `${title} has a chart`);
  const instance = echarts.getInstanceByDom(host);
  assert.ok(instance, `${title} chart is mounted`);
  return instance.getOption() as {
    xAxis: Array<{ min?: number; max?: number }>;
    yAxis: Array<{ min?: number; max?: number; interval?: number }>;
    series: Array<{ showSymbol?: boolean; data: Array<[number, number]> }>;
  };
}

// A deployment whose instance has been up for one scrape: every chart holds a
// single point. They are drawn on the window the server answered for (start_ms
// .. end_ms), and cache usage, a percentage from the server, on a 0..100 axis.
test("one scrape per chart is drawn on the window the server answered for, with cache usage on 0..100", async () => {
  const end = Date.now();
  const start = end - 15 * 60_000;
  serve({
    start_ms: start, end_ms: end,
    sources: { running: "engine", prompt_tokens: "engine", generation_tokens: "engine" },
    concurrency: [line("running", [[end - 10_000, 2]], "inst-pico-1"), line("waiting", [[end - 10_000, 0]], "inst-pico-1")],
    kv: [line("usage_pct", [[end - 10_000, 20]], "inst-pico-1")],
    throughput: [line("prompt_tok_s", [[end - 10_000, 10]], "inst-pico-1"), line("generation_tok_s", [[end - 10_000, 80]], "inst-pico-1")],
  });
  renderPanel();
  await waitFor(() => assert.ok(chartCard("Instance GPU Cache").querySelector("canvas")));
  for (const title of ["Deployment Request Concurrency", "Token Throughput", "Instance GPU Cache"]) {
    const option = await chartOption(title);
    assert.deepEqual([option.xAxis[0].min, option.xAxis[0].max], [start, end], `${title} x axis is the answered window`);
    assert.equal(option.series[0].showSymbol, true, `${title} has one point, which must be visible`);
  }
  const cache = await chartOption("Instance GPU Cache");
  assert.deepEqual([cache.yAxis[0].min, cache.yAxis[0].max, cache.yAxis[0].interval], [0, 100, 20]);
  assert.deepEqual(cache.series[0].data, [[end - 10_000, 20]], "a percentage is drawn as served, not rescaled");
  // Concurrency is a count: not pinned.
  const concurrency = await chartOption("Deployment Request Concurrency");
  assert.deepEqual([concurrency.yAxis[0].min, concurrency.yAxis[0].max], [undefined, undefined]);
});
