import assert from "node:assert/strict";
import fs from "node:fs";
import { afterEach, test } from "node:test";
import { JSDOM } from "jsdom";
import React from "react";
import { createInstance } from "i18next";
import { I18nextProvider, initReactI18next } from "react-i18next";
import { list } from "./helpers/listPage.ts";

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

const { cleanup, render, screen, waitFor } = await import("@testing-library/react");
const { MemoryRouter } = await import("react-router-dom");
const { api } = await import("../src/api.ts");
const { default: DeploymentMetricsPanel } = await import("../src/components/DeploymentMetricsPanel.tsx");
const { monitorRangeCovering } = await import("../src/components/GatewayMetricsNotice.tsx");
const originalMetricsQuery = api.metricsQuery;
const originalMonitorDeployment = api.monitorDeployment;

// The recorded deployment monitor response the panel reads its sources from.
const fixture = JSON.parse(fs.readFileSync(new URL("./fixtures/monitor-deployment.json", import.meta.url), "utf8"));

afterEach(() => {
  cleanup();
  api.metricsQuery = originalMetricsQuery;
  api.monitorDeployment = originalMonitorDeployment;
});

const runningInstance = { uuid: "inst-strata-1", state: "running", node_uuid: "node-1" } as never;

function renderPanel() {
  return render(
    <MemoryRouter>
      <I18nextProvider i18n={i18n}><DeploymentMetricsPanel instances={[runningInstance]} deploymentId={7} /></I18nextProvider>
    </MemoryRouter>,
  );
}

// An engine that reports nothing: every per-instance engine query is empty.
function noEngineSeries() {
  api.metricsQuery = (async () => list([])) as typeof api.metricsQuery;
}

// Read the displayed number independently of its unit.
function statValue(title: string): string | null | undefined {
  return screen.getByText(title).closest("[data-slot='metric-card']")?.querySelector("[data-slot='metric-card-value-row'] > span")?.firstChild?.textContent;
}

function snapshotWith(sources: Record<string, string>, charts: Record<string, unknown> = {}) {
  const requested: Array<[number, string]> = [];
  api.monitorDeployment = (async (id: number, range: string) => {
    requested.push([id, range]);
    return { ...fixture, deployment: { ...fixture.deployment, id }, sources, charts: { ...fixture.charts, ...charts } };
  }) as typeof api.monitorDeployment;
  return requested;
}

test("an engine without metrics shows what the gateway measured, with the way to the service monitor", async () => {
  noEngineSeries();
  const requested = snapshotWith(
    { ttft: "gateway", e2e: "gateway", prompt_tokens: "gateway", generation_tokens: "gateway", running: "gateway" },
    {
      latency: [
        { labels: { metric: "ttft_p95" }, points: [[1000, 900], [2000, 812]] },
        { labels: { metric: "e2e_p95" }, points: [[2000, 4300]] },
      ],
      throughput: [{ labels: { metric: "generation_tok_s", instance: "" }, points: [[2000, 88.5]] }],
      concurrency: [{ labels: { metric: "running", instance: "" }, points: [[2000, 3]] }],
    },
  );
  renderPanel();
  await waitFor(() => assert.ok(screen.getByText("The engine reports no inference metrics")));
  assert.deepEqual(requested[0], [7, "15m"]);
  // The newest point of each line; the absent waiting message stays absent.
  assert.equal(statValue("TTFT p95"), "812");
  assert.equal(statValue("End-to-end latency p95"), "4,300");
  assert.equal(statValue("Total Generation Throughput"), "88.5");
  assert.equal(statValue("In-flight requests"), "3");
  assert.equal(screen.queryByText("Waiting for Deployment Metrics"), null);
  const link = screen.getByRole("link", { name: "Open the service monitor" });
  assert.equal(link.getAttribute("href"), "/monitor/d/7");
});

test("a gateway signal the snapshot has no line for reads as a dash, not a zero", async () => {
  noEngineSeries();
  snapshotWith({ ttft: "gateway" }, { latency: [{ labels: { metric: "ttft_p95" }, points: [[2000, 640]] }], throughput: [], concurrency: [] });
  renderPanel();
  await waitFor(() => assert.equal(statValue("TTFT p95"), "640"));
  for (const title of ["End-to-end latency p95", "Total Generation Throughput", "In-flight requests"]) {
    assert.equal(statValue(title), "—", title);
  }
});

test("an idle engine that reports nothing says so rather than waiting", async () => {
  noEngineSeries();
  snapshotWith({});
  renderPanel();
  await waitFor(() => assert.ok(screen.getByText("No engine metrics, and no requests through the gateway in the selected range")));
  assert.equal(screen.queryByText("Waiting for Deployment Metrics"), null);
  assert.equal(screen.getByRole("link", { name: "Open the service monitor" }).getAttribute("href"), "/monitor/d/7");
});

test("the gateway snapshot covers the drawer's selected window", () => {
  const minute = 60 * 1000;
  assert.equal(monitorRangeCovering(15 * minute + 5), "15m");
  assert.equal(monitorRangeCovering(30 * minute), "1h");
  assert.equal(monitorRangeCovering(6 * 60 * minute), "6h");
  assert.equal(monitorRangeCovering(24 * 60 * minute), "24h");
  assert.equal(monitorRangeCovering(7 * 24 * 60 * minute), "168h");
  // Longer than the monitor keeps answering: the longest it has.
  assert.equal(monitorRangeCovering(365 * 24 * 60 * minute), "168h");
});

test("an instance still starting keeps the panel waiting", async () => {
  noEngineSeries();
  snapshotWith({});
  render(
    <MemoryRouter>
      <I18nextProvider i18n={i18n}>
        <DeploymentMetricsPanel instances={[{ uuid: "inst-starting", state: "starting", node_uuid: "node-1" } as never]} deploymentId={7} />
      </I18nextProvider>
    </MemoryRouter>,
  );
  await waitFor(() => assert.ok(screen.getByText("Waiting for Deployment Metrics")));
});

test("while the engine's own metrics are simply not in yet, the panel keeps waiting", async () => {
  for (const sources of [{ ttft: "engine", running: "gateway" }]) {
    noEngineSeries();
    snapshotWith(sources);
    renderPanel();
    await waitFor(() => assert.ok(screen.getByText("Waiting for Deployment Metrics")));
    assert.equal(screen.queryByText("The engine reports no inference metrics"), null);
    cleanup();
  }
});

test("a failed snapshot read falls back to the waiting message", async () => {
  noEngineSeries();
  api.monitorDeployment = (async () => {
    throw new Error("boom");
  }) as typeof api.monitorDeployment;
  renderPanel();
  await waitFor(() => assert.ok(screen.getByText("Waiting for Deployment Metrics")));
});
