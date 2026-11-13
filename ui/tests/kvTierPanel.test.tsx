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
const { default: KVTierPanel } = await import("../src/components/KVTierPanel.tsx");
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

function show(params: Record<string, unknown>) {
  render(
    <I18nextProvider i18n={i18n}>
      <KVTierPanel params={params} instanceUuid="inst-1" />
    </I18nextProvider>,
  );
}

// A deployment that keeps its KV cache on the GPU says so for both other tiers
// rather than showing empty measurements.
test("a deployment without offload tiers shows both as not enabled", async () => {
  stubMetrics({});
  show({});
  await waitFor(() => assert.equal(screen.getAllByText("Not enabled").length, 2));
  assert.ok(screen.getByText("See GPU Cache Usage for the engine cache trend."));
});

// An older image has the tiers but reports none of the tiering metrics. The
// configured capacity is still the truth about the deployment, and the missing
// measurement is named rather than rendered as a zero.
test("a configured tier whose engine reports nothing is labelled, not zeroed", async () => {
  stubMetrics({});
  show({ kv_offloading_size: 8, kv_disk_offload: true, shared_kv_dir: "/mnt/kv" });
  await waitFor(() => assert.ok(screen.getByText("8 GiB")));
  assert.ok(screen.getByText("/mnt/kv"));
  const silent = screen.getAllByText((_, element) => element?.textContent === "Hit rate: Engine did not report this metric");
  assert.ok(silent.length >= 2, "both tiers must say the engine reported nothing");
  assert.ok(screen.getByText("Waiting for the agent to report directory usage"));
});

// With both sources reporting, the panel shows the window hit rate from the
// engine and the directory usage from the agent, which is the only place the
// size of the disk tier can come from.
test("hit rates come from the engine and directory usage from the agent", async () => {
  stubMetrics({
    gpuworks_infer_kv_memory_queries_total: series([[1000, 100], [2000, 200]]),
    gpuworks_infer_kv_memory_hits_total: series([[1000, 10], [2000, 90]]),
    gpuworks_infer_kv_disk_queries_total: series([[1000, 100], [2000, 200]]),
    gpuworks_infer_kv_disk_hits_total: series([[1000, 0], [2000, 25]]),
    gpuworks_kv_disk_used_bytes: series([[2000, 512 * 1024 * 1024]]),
    gpuworks_kv_disk_limit_bytes: series([[2000, 2 * 1024 * 1024 * 1024]]),
  });
  show({ kv_offloading_size: 8, kv_disk_offload: true, shared_kv_dir: "/mnt/kv" });

  // 80 hits over 100 queries in the window, and 25 over 100.
  await waitFor(() => assert.ok(screen.getByText((_, element) => element?.textContent === "Hit rate: 80.0%")));
  assert.ok(screen.getByText((_, element) => element?.textContent === "Hit rate: 25.0%"));
  assert.ok(screen.getByText((_, element) => element?.textContent === "Used: 512 MiB / 2.0 GiB"));
});

// Write failures mean the tier is full or unwritable; the engine keeps serving,
// so nothing else on the page would say anything is wrong.
test("write failures in the window are called out", async () => {
  stubMetrics({
    gpuworks_infer_kv_disk_write_failures_total: series([[1000, 2], [2000, 9]]),
    gpuworks_kv_disk_used_bytes: series([[2000, 1024]]),
    gpuworks_kv_disk_limit_bytes: series([[2000, 2048]]),
  });
  show({ kv_offloading_size: 8, kv_disk_offload: true, shared_kv_dir: "/mnt/kv" });
  const alert = await waitFor(() => screen.getByRole("alert"));
  assert.equal(alert.textContent, "7 write failures in this window (the disk tier may be full or unwritable)");
});
