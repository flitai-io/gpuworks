import assert from "node:assert/strict";
import fs from "node:fs";
import { afterEach, test } from "node:test";
import { JSDOM } from "jsdom";
import React from "react";
import { createInstance } from "i18next";
import { I18nextProvider, initReactI18next } from "react-i18next";
import type { MetricSeries } from "../src/api";
import { list } from "./helpers/listPage.ts";

const dom = new JSDOM("<!doctype html><html><body></body></html>", { url: "http://localhost/" });
for (const name of ["window", "document", "navigator", "HTMLElement", "SVGElement", "Element", "Node", "MutationObserver", "ShadowRoot"] as const) {
  Object.defineProperty(globalThis, name, { value: dom.window[name], configurable: true });
}
Object.defineProperty(globalThis, "getComputedStyle", { value: (element: Element) => dom.window.getComputedStyle(element), configurable: true });
Object.defineProperty(dom.window, "matchMedia", { configurable: true, value: () => ({ matches: false, addListener() {}, removeListener() {}, addEventListener() {}, removeEventListener() {}, dispatchEvent: () => false }) });
class ResizeObserverStub { observe() {} unobserve() {} disconnect() {} }
Object.defineProperty(globalThis, "ResizeObserver", { value: ResizeObserverStub, configurable: true });

// echarts needs a 2d context that JSDOM cannot provide; a catch-all proxy
// keeps chart mounting alive so panel behaviour stays testable.
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
const { default: InferenceMetricsPanel, percentScaled, ratioSeries } = await import("../src/components/InferenceMetricsPanel.tsx");
const originalMetricsQuery = api.metricsQuery;

afterEach(() => {
  cleanup();
  api.metricsQuery = originalMetricsQuery;
});

const LABELS = { instance: "inst-1", engine: "vllm", node: "node-1" };
function series(values: Array<[number, number]>): MetricSeries[] {
  return [{ labels: LABELS, points: values }];
}

function stubMetrics(byMetric: Record<string, MetricSeries[]>) {
  api.metricsQuery = (async (metric: string) => list(byMetric[metric] ?? [])) as typeof api.metricsQuery;
}

function summary() {
  return screen.getByRole("region", { name: "Instance Metrics" });
}

function card(title: string): HTMLElement {
  const heading = screen.getByText(title);
  return heading.closest('.deployment-trend-card') as HTMLElement;
}

test("ratioSeries divides counter windows and skips resets and unaligned samples", () => {
  const numerator = series([
    [1000, 10], [2000, 20], [3000, 35], [4000, 5], [5000, 6], [6000, 10],
  ]);
  // Both counters reset at 4000 (engine restart) and the denominator misses
  // the 5000 sample: the 3000→4000 transition window is dropped, the window
  // after the restart resumes, the unaligned sample is ignored.
  const denominator = series([
    [1000, 20], [2000, 40], [3000, 70], [4000, 1], [6000, 3],
  ]);
  const out = ratioSeries(numerator, denominator);
  assert.equal(out.length, 1);
  assert.deepEqual(out[0].points, [
    [2000, 0.5], [3000, 0.5], [6000, 2.5],
  ]);
});

test("percentScaled tolerates fraction and percentage gauges", () => {
  assert.equal(percentScaled(0.42), 42);
  assert.equal(percentScaled(1), 100);
  assert.equal(percentScaled(42), 42);
  assert.equal(percentScaled(100), 100);
});

test("vLLM counter data renders the KV card and leaves spec decode unavailable", async () => {
  stubMetrics({
    gpuworks_infer_requests_running: series([[1000, 1]]),
    // vLLM's cache gauge is a 0..1 fraction; the stat must render it as 55%.
    gpuworks_infer_gpu_cache_usage_percent: series([[1000, 0.55]]),
    gpuworks_infer_kv_hits_total: series([[1000, 100], [2000, 220]]),
    gpuworks_infer_kv_queries_total: series([[1000, 200], [2000, 400]]),
  });
  render(<I18nextProvider i18n={i18n}><InferenceMetricsPanel instanceUuid="inst-1" /></I18nextProvider>);
  fireEvent.click(screen.getByRole("tab", { name: "Cache & engine" }));

  await waitFor(() => assert.ok(card("KV Cache Hit Rate").querySelector("canvas")));
  assert.match(summary().textContent ?? "", /GPU Cache55%/, "cache gauge 0.55 must render as 55%");
  assert.ok(!card("KV Cache Hit Rate").textContent?.includes("Engine did not report this metric"));
  assert.ok(card("Speculative Accept Rate").textContent?.includes("Engine did not report this metric"));
  assert.ok(card("KV Cache Hit Rate").querySelector('[aria-label="Share of prompt tokens reused from the prefix cache"]'));
});

test("counters reported without traffic show the no-traffic state, not unreported", async () => {
  stubMetrics({
    gpuworks_infer_requests_running: series([[1000, 1]]),
    // vLLM emits both counter families from process start at zero; without
    // traffic no ratio window exists, which must read as "no traffic", not
    // as "engine did not report the metric".
    gpuworks_infer_kv_hits_total: series([[1000, 0], [2000, 0]]),
    gpuworks_infer_kv_queries_total: series([[1000, 0], [2000, 0]]),
    gpuworks_infer_spec_draft_tokens_total: series([[1000, 0], [2000, 0]]),
    gpuworks_infer_spec_accepted_tokens_total: series([[1000, 0], [2000, 0]]),
  });
  render(<I18nextProvider i18n={i18n}><InferenceMetricsPanel instanceUuid="inst-1" /></I18nextProvider>);
  fireEvent.click(screen.getByRole("tab", { name: "Cache & engine" }));

  await waitFor(() => assert.ok(card("KV Cache Hit Rate").textContent?.includes("no qualifying traffic yet")));
  assert.ok(card("Speculative Accept Rate").textContent?.includes("no qualifying traffic yet"));
  assert.ok(!card("KV Cache Hit Rate").textContent?.includes("Engine did not report this metric"));
});

test("SGLang instant gauges fall back when counter pairs are absent", async () => {
  stubMetrics({
    gpuworks_infer_requests_running: series([[1000, 1]]),
    gpuworks_infer_kv_hit_rate_instant: series([[1000, 0.42]]),
    gpuworks_infer_spec_accept_rate: series([[1000, 0.81]]),
  });
  render(<I18nextProvider i18n={i18n}><InferenceMetricsPanel instanceUuid="inst-1" /></I18nextProvider>);
  fireEvent.click(screen.getByRole("tab", { name: "Cache & engine" }));

  await waitFor(() => assert.ok(card("KV Cache Hit Rate").querySelector("canvas")));
  assert.ok(card("Speculative Accept Rate").querySelector("canvas"));
  assert.ok(!card("KV Cache Hit Rate").textContent?.includes("Engine did not report this metric"));
});

test("a panel without any engine metrics keeps showing the waiting alert", async () => {
  stubMetrics({});
  render(<I18nextProvider i18n={i18n}><InferenceMetricsPanel instanceUuid="inst-1" /></I18nextProvider>);

  await waitFor(() => assert.ok(screen.getByText("Waiting for Inference Metrics")));
  assert.deepEqual([...summary().querySelectorAll("[data-slot='metric-card-value-row']")].map(el => el.textContent), Array(5).fill("—"));
  assert.ok(screen.getByText("Stage Duration p95"));
  fireEvent.click(screen.getByRole("tab", { name: "Cache & engine" }));
  assert.ok(await screen.findByText("KV Cache Hit Rate"));
});

test("missing numeric metrics stay unknown while an observed zero remains zero", async () => {
  stubMetrics({
    gpuworks_infer_requests_running: series([[1000, 2]]),
    gpuworks_infer_requests_waiting: series([[1000, 0]]),
  });
  render(<I18nextProvider i18n={i18n}><InferenceMetricsPanel instanceUuid="inst-1" /></I18nextProvider>);
  await waitFor(() => assert.deepEqual([...summary().querySelectorAll("[data-slot='metric-card-value-row']")].map(el => el.textContent), ["2", "0", "—", "—", "—"]));
  assert.deepEqual([...summary().querySelectorAll("[data-slot='metric-card-label']")].map(el => el.textContent), [
    "Running Requests", "Waiting Requests", "GPU Cache", "Prompt Throughput", "Generation Throughput",
  ]);
});

test("failed metric reads remain explicit and do not suppress the other diagnostics", async () => {
  api.metricsQuery = async (metric) => {
    if (metric.endsWith("requests_waiting")) throw new Error("read unavailable");
    return list([]);
  };
  render(<I18nextProvider i18n={i18n}><InferenceMetricsPanel instanceUuid="inst-1" /></I18nextProvider>);
  assert.ok(await screen.findByText("read unavailable"));
  assert.equal(screen.queryByText("Waiting for Inference Metrics"), null);
  assert.ok(screen.getByText("Stage Duration p95"));
});
