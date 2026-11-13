import assert from "node:assert/strict";
import fs from "node:fs";
import { afterEach, test } from "node:test";
import { JSDOM } from "jsdom";
import React from "react";
import { createInstance } from "i18next";
import { I18nextProvider, initReactI18next } from "react-i18next";
import type { Instance, MetricSeries, MonitorDeployment } from "../src/api";
import { list } from "./helpers/listPage.ts";

const dom = new JSDOM("<!doctype html><html><body></body></html>", { url: "http://localhost/" });
for (const name of ["window", "document", "navigator", "HTMLElement", "SVGElement", "Element", "Node", "MutationObserver", "ShadowRoot"] as const) {
  Object.defineProperty(globalThis, name, { value: dom.window[name], configurable: true });
}
Object.defineProperty(globalThis, "getComputedStyle", { value: (element: Element) => dom.window.getComputedStyle(element), configurable: true });
Object.defineProperty(dom.window, "matchMedia", { configurable: true, value: () => ({ matches: false, addListener() {}, removeListener() {}, addEventListener() {}, removeEventListener() {}, dispatchEvent: () => false }) });
class ResizeObserverStub { observe() {} unobserve() {} disconnect() {} }
Object.defineProperty(globalThis, "ResizeObserver", { value: ResizeObserverStub, configurable: true });

const anyContext = new Proxy(function () {} as never, {
  get: (_target: never, prop: string | symbol) => {
    if (prop === "canvas") return { width: 300, height: 190, style: {} };
    if (prop === "dpr") return 1;
    if (prop === "measureText") return () => ({ width: 10 });
    return anyContext;
  },
  set: () => true,
  apply: () => anyContext,
});
dom.window.HTMLCanvasElement.prototype.getContext = () => anyContext as CanvasRenderingContext2D;

const i18n = createInstance();
await i18n.use(initReactI18next).init({
  lng: "en",
  fallbackLng: "en",
  resources: { en: { deployments: JSON.parse(fs.readFileSync(new URL("../src/i18n/locales/en/deployments.json", import.meta.url), "utf8")) } },
  defaultNS: "deployments",
  interpolation: { escapeValue: false },
});

const { cleanup, render, screen, fireEvent, waitFor } = await import("@testing-library/react");
const { api } = await import("../src/api.ts");
const { default: InstanceMetricsTab } = await import("../src/pages/InstanceMetricsTab.tsx");
const originalMetricsQuery = api.metricsQuery;
const originalMonitorDeployment = api.monitorDeployment;

afterEach(() => {
  cleanup();
  api.metricsQuery = originalMetricsQuery;
  api.monitorDeployment = originalMonitorDeployment;
});

function stubMetrics(byMetric: Record<string, MetricSeries[]>) {
  api.metricsQuery = (async (metric: string) => list(byMetric[metric] ?? [])) as typeof api.metricsQuery;
}

function stubMonitorDeployment() {
  api.monitorDeployment = (async () => ({
    as_of: 0,
    stale: false,
    deployment: { id: 1, name: "dep", engine: "vllm", placement: "", replicas: { ready: 1, desired: 1 } },
    alignment: null,
    ttft_breakdown: null,
    charts: { latency: [], tpot: [], concurrency: [], kv: [], throughput: [], stage_time: [], preemptions: [] },
    replicas: [],
  } satisfies MonitorDeployment)) as typeof api.monitorDeployment;
}

function baseInstance(overrides: Partial<Instance> = {}): Instance {
  return {
    uuid: "inst-1", deployment_id: 1, node_uuid: "node-1", state: "running",
    gpu_indexes: [0], ports: [30000], api_port: 30000, image: "image",
    message: "", container_id: "container-1", sleep_level: 0,
    ...overrides,
  } as Instance;
}

test("a stopped instance can inspect retained samples without showing the current GPU occupant", async () => {
  stubMetrics({ gpuworks_infer_requests_waiting: [{ labels: {}, points: [[1000, 3]] }] });
  stubMonitorDeployment();
  render(<I18nextProvider i18n={i18n}><InstanceMetricsTab inst={baseInstance({ state: "stopped" })} /></I18nextProvider>);
  await waitFor(() => assert.match(screen.getByRole("region", { name: "Instance Metrics" }).textContent ?? "", /Waiting Requests3/));
  assert.ok(screen.getByText("Last readings in the selected time range"));
  assert.equal(screen.queryByText("Per-GPU Telemetry"), null);
});

test("the metric group changes the diagnostics for the same instance without stacking the groups", async () => {
  stubMetrics({});
  stubMonitorDeployment();
  render(<I18nextProvider i18n={i18n}><InstanceMetricsTab inst={baseInstance()} params={{ kv_offloading_size: 8 }} /></I18nextProvider>);
  assert.deepEqual(screen.getAllByRole("tab").map(tab => tab.textContent), ["Requests & latency", "Cache & engine", "GPU & memory"]);
  assert.ok(screen.getByText("Stage Duration p95"));
  assert.ok(screen.getByText("Preemptions /s"));
  assert.equal(screen.queryByText("KV Tiers"), null);
  fireEvent.click(screen.getByRole("tab", { name: "GPU & memory" }));
  const hardware = await screen.findByRole("tabpanel", { name: "GPU & memory" });
  assert.ok(await screen.findByText("Waiting for GPU Telemetry"));
  assert.equal(hardware.querySelector("canvas"), null);
  assert.equal(screen.queryByText("Stage Duration p95"), null);
  fireEvent.click(screen.getByRole("tab", { name: "Cache & engine" }));
  assert.ok(await screen.findByText("KV Tiers"));
  assert.ok(screen.getByText("8 GiB"));
  assert.equal(screen.queryByText("GPU utilization"), null);
  assert.equal(screen.getByRole("region", { name: "Instance Metrics" }).querySelectorAll("[data-slot='metric-card']").length, 5);
});

test("a historical instance keeps the selected tab visible when switching between cache and requests", async () => {
  stubMetrics({});
  render(<I18nextProvider i18n={i18n}><InstanceMetricsTab inst={baseInstance({ state: "failed" })} /></I18nextProvider>);
  const tabs = screen.getAllByRole("tab");
  assert.deepEqual(tabs.map(tab => tab.textContent), ["Requests & latency", "Cache & engine"]);
  // The selected background is measured from its trigger. JSDOM supplies no layout boxes.
  for (const tab of tabs) Object.defineProperty(tab, "offsetWidth", { value: 96 });
  const tablist = screen.getByRole("tablist");
  for (const tab of [tabs[1], tabs[0], tabs[1]]) {
    fireEvent.click(tab);
    await screen.findByRole("tabpanel", { name: tab.textContent! });
    await waitFor(() => assert.ok(tablist.querySelector(":scope > .bg-brand"), "the selected label needs its contrasting background"));
  }
});

test("GPU cards keep their last readings on return and only report missing telemetry after a response", async () => {
  let resume!: () => void;
  let pending = new Promise<void>(resolve => { resume = resolve; });
  let gpuCalls = 0;
  api.metricsQuery = async metric => {
    if (metric !== "gpuworks_gpu_utilization") return list([]);
    gpuCalls++;
    await pending;
    return list([{ labels: { node: "node-1", gpu: "0" }, points: [[1000, 42]] }]);
  };
  render(<I18nextProvider i18n={i18n}><InstanceMetricsTab inst={baseInstance()} /></I18nextProvider>);
  assert.equal(gpuCalls, 0);
  const gpuTab = screen.getByRole("tab", { name: "GPU & memory" });
  fireEvent.click(gpuTab);
  assert.ok(screen.getByText("GPU0"));
  assert.equal(screen.queryByText("Waiting for GPU Telemetry"), null);
  resume();
  await screen.findByText("42%");
  fireEvent.click(screen.getByRole("tab", { name: "Requests & latency" }));
  pending = new Promise<void>(resolve => { resume = resolve; });
  fireEvent.click(gpuTab);
  assert.ok(screen.getByText("42%"));
  assert.equal(screen.queryByText("Waiting for GPU Telemetry"), null);
  resume();
});
