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

const i18n = createInstance();
await i18n.use(initReactI18next).init({
  lng: "en",
  fallbackLng: "en",
  resources: { en: { deployments: JSON.parse(fs.readFileSync(new URL("../src/i18n/locales/en/deployments.json", import.meta.url), "utf8")) } },
  defaultNS: "deployments",
  interpolation: { escapeValue: false },
});

const { cleanup, render, screen, waitFor } = await import("@testing-library/react");
const { api } = await import("../src/api.ts");
const { default: InstanceGpuStrip } = await import("../src/components/InstanceGpuStrip.tsx");
const originalMetricsQuery = api.metricsQuery;

afterEach(() => {
  cleanup();
  api.metricsQuery = originalMetricsQuery;
});

function seriesFor(byGpu: Record<number, number>): MetricSeries[] {
  return Object.entries(byGpu).map(([gpu, value]) => ({
    labels: { node: "node-1", gpu },
    points: [[1000, value]],
  }));
}

function stubMetrics(byMetric: Record<string, MetricSeries[]>) {
  api.metricsQuery = (async (metric: string) => list(byMetric[metric] ?? [])) as typeof api.metricsQuery;
}

test("with no telemetry reported, the strip shows the waiting state instead of empty tiles", async () => {
  stubMetrics({});
  render(<I18nextProvider i18n={i18n}><InstanceGpuStrip nodeUuid="node-1" gpuIndexes={[0]} /></I18nextProvider>);
  await waitFor(() => assert.ok(screen.getByText("Waiting for GPU Telemetry")));
});

test("a single GPU renders its tile with no group skew line", async () => {
  stubMetrics({
    gpuworks_gpu_utilization: seriesFor({ 0: 42 }),
    gpuworks_gpu_memory_used_bytes: seriesFor({ 0: 4 * 1024 ** 3 }),
    gpuworks_gpu_memory_total_bytes: seriesFor({ 0: 24 * 1024 ** 3 }),
    gpuworks_gpu_temperature_celsius: seriesFor({ 0: 61 }),
    gpuworks_gpu_power_watts: seriesFor({ 0: 220 }),
  });
  render(<I18nextProvider i18n={i18n}><InstanceGpuStrip nodeUuid="node-1" gpuIndexes={[0]} /></I18nextProvider>);
  await waitFor(() => assert.ok(screen.getByText("GPU0")));
  assert.ok(screen.getByText("42%"));
  assert.ok(screen.getByText("4.0 / 24 GiB"));
  assert.equal(screen.queryByText(/Group utilization/), null, "a single GPU has no group to compare against");
});

test("four GPUs render four tiles plus a deterministic group util max/min ratio", async () => {
  stubMetrics({
    gpuworks_gpu_utilization: seriesFor({ 0: 80, 1: 20, 2: 40, 3: 40 }),
    gpuworks_gpu_memory_used_bytes: seriesFor({ 0: 1, 1: 1, 2: 1, 3: 1 }),
    gpuworks_gpu_memory_total_bytes: seriesFor({ 0: 1, 1: 1, 2: 1, 3: 1 }),
  });
  render(<I18nextProvider i18n={i18n}><InstanceGpuStrip nodeUuid="node-1" gpuIndexes={[0, 1, 2, 3]} /></I18nextProvider>);
  await waitFor(() => assert.ok(screen.getByText("GPU3")));
  for (const label of ["GPU0", "GPU1", "GPU2", "GPU3"]) assert.ok(screen.getByText(label));
  // max 80 / min 20 = 4.00x, a plain deterministic ratio with no threshold coloring.
  assert.ok(screen.getByText("Group utilization max/min: 4.00x"));
});

test("a zero minimum utilization reports the ratio as not computable rather than Infinity", async () => {
  stubMetrics({
    gpuworks_gpu_utilization: seriesFor({ 0: 55, 1: 0 }),
  });
  render(<I18nextProvider i18n={i18n}><InstanceGpuStrip nodeUuid="node-1" gpuIndexes={[0, 1]} /></I18nextProvider>);
  await waitFor(() => assert.ok(screen.getByText("GPU1")));
  assert.ok(screen.getByText("Group utilization max/min: —"));
});

// Ascend's npu-smi mapping reports utilization and memory but not temperature
// or power on some builds - a shape worth naming explicitly, since a silent
// "0" there would read as "0 degrees" / "0 watts" instead of "not reported".
test("an Ascend-shaped payload with only util and memory shows temperature/power as a dash, not zero", async () => {
  stubMetrics({
    gpuworks_gpu_utilization: seriesFor({ 0: 30 }),
    gpuworks_gpu_memory_used_bytes: seriesFor({ 0: 8 * 1024 ** 3 }),
    gpuworks_gpu_memory_total_bytes: seriesFor({ 0: 32 * 1024 ** 3 }),
  });
  render(<I18nextProvider i18n={i18n}><InstanceGpuStrip nodeUuid="node-1" gpuIndexes={[0]} /></I18nextProvider>);
  await waitFor(() => assert.ok(screen.getByText("GPU0")));
  assert.ok(screen.getByText((_, el) => el?.textContent === "Temp —"), "temperature must read as unreported, not 0C");
  assert.ok(screen.getByText((_, el) => el?.textContent === "Power —"), "power must read as unreported, not 0W");
});

test("the VRAM bar is hidden (not drawn at 0%) when the total is unknown", async () => {
  stubMetrics({
    gpuworks_gpu_utilization: seriesFor({ 0: 30 }),
    gpuworks_gpu_memory_used_bytes: seriesFor({ 0: 8 * 1024 ** 3 }),
    // No gpuworks_gpu_memory_total_bytes at all: total is unknown.
  });
  const { container } = render(<I18nextProvider i18n={i18n}><InstanceGpuStrip nodeUuid="node-1" gpuIndexes={[0]} /></I18nextProvider>);
  await waitFor(() => assert.ok(screen.getByText("GPU0")));
  // Only the utilization bar remains; the VRAM row keeps its text but drops
  // its progress bar rather than drawing a misleading 0%.
  assert.equal(screen.getAllByRole("progressbar").length, 1);
});

test("rerendering with a same-content but new GPU index array does not restart the fetch fan-out", async () => {
  let calls = 0;
  api.metricsQuery = (async () => {
    calls += 1;
    return list([]);
  }) as typeof api.metricsQuery;

  const view = render(<I18nextProvider i18n={i18n}><InstanceGpuStrip nodeUuid="node-1" gpuIndexes={[0, 1]} /></I18nextProvider>);
  await waitFor(() => assert.ok(calls > 0));
  const afterFirstMount = calls;

  // A fresh array with identical content - exactly what a parent re-render
  // produces when it recomputes `inst.gpu_indexes ?? []` inline every render.
  view.rerender(<I18nextProvider i18n={i18n}><InstanceGpuStrip nodeUuid="node-1" gpuIndexes={[0, 1]} /></I18nextProvider>);
  await new Promise((resolve) => setTimeout(resolve, 20));
  assert.equal(calls, afterFirstMount, "an equal-content array must not restart the fetch effect");
});

// A dGPU instance shares its card: the card's own figures include every other
// slice on it. The tile leads with the instance's own share - its memory
// against its limit and its SM reference, read by instance - and keeps the
// card's figures as a line of context, named as such.
test("a dGPU instance's tile shows its own slice first and the whole card as context", async () => {
  const GIB = 1024 ** 3;
  const asked: Array<{ metric: string; instance?: string }> = [];
  api.metricsQuery = (async (metric: string, params: { instance?: string }) => {
    asked.push({ metric, instance: params.instance });
    const byMetric: Record<string, MetricSeries[]> = {
      gpuworks_gpu_utilization: seriesFor({ 1: 100 }),
      gpuworks_gpu_memory_used_bytes: seriesFor({ 1: 14.8 * GIB }),
      gpuworks_gpu_memory_total_bytes: seriesFor({ 1: 22 * GIB }),
      gpuworks_dgpu_memory_used_bytes: [{ labels: { node: "node-1", gpu: "1", instance: "inst-a" }, points: [[1000, 7 * GIB]] }],
      gpuworks_dgpu_sm_util: [{ labels: { node: "node-1", gpu: "1", instance: "inst-a" }, points: [[1000, 63]] }],
    };
    return list(byMetric[metric] ?? []);
  }) as typeof api.metricsQuery;
  render(<I18nextProvider i18n={i18n}><InstanceGpuStrip nodeUuid="node-1" gpuIndexes={[1]} instanceUuid="inst-a" vramLimitBytes={8 * GIB} /></I18nextProvider>);
  await waitFor(() => assert.ok(screen.getByText("GPU1 · dGPU slice (limit 8.0 GiB)")));
  assert.ok(screen.getByText("7.0 / 8.0 GiB"));
  assert.ok(screen.getByText("63%"));
  assert.ok(screen.getByText("Whole card (with its other instances): 100% utilization · 14.8 / 22 GiB memory"));
  for (const metric of ["gpuworks_dgpu_memory_used_bytes", "gpuworks_dgpu_sm_util"]) {
    assert.ok(asked.some((query) => query.metric === metric && query.instance === "inst-a"), `${metric} is read for this instance`);
  }
});

test("a whole-card instance asks for no slice metrics", async () => {
  const asked: string[] = [];
  api.metricsQuery = (async (metric: string) => { asked.push(metric); return list([]); }) as typeof api.metricsQuery;
  render(<I18nextProvider i18n={i18n}><InstanceGpuStrip nodeUuid="node-1" gpuIndexes={[0]} instanceUuid="inst-b" /></I18nextProvider>);
  await waitFor(() => assert.ok(screen.getByText("Waiting for GPU Telemetry")));
  assert.equal(asked.filter((metric) => metric.startsWith("gpuworks_dgpu_")).length, 0);
});

test("the latest sample for the selected GPU remains readable when a different sensor fails", async () => {
  const selection = { kind: "custom" as const, startMS: 1000, endMS: 2000 };
  const calls: Array<{ start?: number; end?: number }> = [];
  api.metricsQuery = async (metric, params) => {
    calls.push(params ?? {});
    if (metric === "gpuworks_gpu_temperature_celsius") throw new Error("temperature unavailable");
    const value = metric === "gpuworks_gpu_memory_used_bytes" ? 2 * 1024 ** 3
      : metric === "gpuworks_gpu_memory_total_bytes" ? 24 * 1024 ** 3 : 42;
    return list([
      { labels: { node: "node-1", gpu: "2" }, points: [[1000, 0], [2000, value]] },
      { labels: { node: "node-1", gpu: "3" }, points: [[2000, 99]] },
      { labels: { node: "old-node", gpu: "2" }, points: [[2000, 99]] },
    ]);
  };
  render(<I18nextProvider i18n={i18n}><InstanceGpuStrip nodeUuid="node-1" gpuIndexes={[2]} selection={selection} /></I18nextProvider>);
  await screen.findByText("temperature unavailable");
  assert.ok(screen.getByText("42%"));
  assert.ok(screen.getByText("2.0 / 24 GiB"));
  assert.equal(screen.queryByText("GPU3"), null);
  assert.ok(calls.every(params => params.start === 1000 && params.end === 2000));
});

test("historical dGPU data is restricted to the selected slice after its physical card has been reassigned", async () => {
  const calls: string[] = [];
  api.metricsQuery = async (metric, params) => {
    assert.equal(params?.instance, "retired");
    calls.push(metric);
    return list([
      { labels: { gpu: "0", instance: "retired" }, points: [[1000, 0]] },
      { labels: { gpu: "0", instance: "current" }, points: [[1000, 90]] },
    ]);
  };
  render(<I18nextProvider i18n={i18n}><InstanceGpuStrip historical nodeUuid="node-1" gpuIndexes={[0]} instanceUuid="retired" vramLimitBytes={1024 ** 3} /></I18nextProvider>);
  await screen.findByText("0.0 / 1.0 GiB");
  assert.deepEqual(calls.sort(), ["gpuworks_dgpu_memory_used_bytes", "gpuworks_dgpu_sm_util"]);
  assert.equal(screen.queryByText(/Whole card \(/), null);
});
