import assert from "node:assert/strict";
import fs from "node:fs";
import { afterEach, test } from "node:test";
import { JSDOM } from "jsdom";
import React from "react";
import { createInstance } from "i18next";
import { I18nextProvider, initReactI18next } from "react-i18next";
import type { MetricSeries } from "../src/api";
import type { TimeRangeSelection } from "../src/timeRange";
import { list } from "./helpers/listPage.ts";

// The server's deployment snapshot, generated from the real handler.
const monitorFixture = JSON.parse(fs.readFileSync(new URL("./fixtures/monitor-deployment.json", import.meta.url), "utf8"));

const dom = new JSDOM("<!doctype html><html><body></body></html>", { url: "http://localhost/" });
for (const name of ["window", "document", "navigator", "HTMLElement", "SVGElement", "Element", "Node", "MutationObserver", "ShadowRoot"] as const) {
  Object.defineProperty(globalThis, name, { value: dom.window[name], configurable: true });
}
Object.defineProperty(globalThis, "getComputedStyle", { value: (element: Element) => dom.window.getComputedStyle(element), configurable: true });
Object.defineProperty(dom.window, "matchMedia", { configurable: true, value: () => ({ matches: false, addListener() {}, removeListener() {}, addEventListener() {}, removeEventListener() {}, dispatchEvent: () => false }) });
class ResizeObserverStub { observe() {} unobserve() {} disconnect() {} }
Object.defineProperty(globalThis, "ResizeObserver", { value: ResizeObserverStub, configurable: true });

// The "data exists" branch mounts ECharts (via TimeSeriesChart); jsdom has no
// 2d context, so stub one the same way inferenceMetrics.test.tsx does.
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

const { cleanup, render, screen, waitFor } = await import("@testing-library/react");
const { api } = await import("../src/api.ts");
const { default: InstanceStagePanel, stageSeriesName, stageP95Millis } = await import("../src/components/InstanceStagePanel.tsx");
const { rateSeries } = await import("../src/instanceMetricSeries.ts");
const originalMetricsQuery = api.metricsQuery;
const t = i18n.getFixedT("en", "deployments");

afterEach(() => {
  cleanup();
  api.metricsQuery = originalMetricsQuery;
});

const quick = { kind: "quick", key: "15m" } as const;
const noSamples = "No samples for this instance in the selected range";

test("stage duration and preemptions query the selected replica and respect a fixed historical range", async () => {
  const calls: Array<[string, Parameters<typeof api.metricsQuery>[1]]> = [];
  api.metricsQuery = async (metric, params) => {
    calls.push([metric, params]);
    return list([]);
  };
  const panel = (instanceUuid: string, selection: TimeRangeSelection) =>
    <I18nextProvider i18n={i18n}><InstanceStagePanel instanceUuid={instanceUuid} selection={selection} /></I18nextProvider>;
  const view = render(panel("inst-one", quick));
  await waitFor(() => assert.equal(calls.length, 4));
  for (const [, params] of calls) {
    assert.equal(params?.instance, "inst-one");
    assert.equal(params!.end! - params!.start!, 15 * 60 * 1000);
  }
  calls.length = 0;
  view.rerender(panel("inst-two", { kind: "custom", startMS: 10000, endMS: 20000 }));
  await waitFor(() => assert.equal(calls.length, 4));
  assert.deepEqual(calls.map(([metric]) => metric).sort(), [
    "gpuworks_infer_decode_time_seconds", "gpuworks_infer_preemptions_total",
    "gpuworks_infer_prefill_time_seconds", "gpuworks_infer_queue_time_seconds",
  ]);
  for (const [, params] of calls) {
    assert.deepEqual(params, { instance: "inst-two", start: 10000, end: 20000, step: "10s" });
  }
  assert.equal(screen.getAllByText(noSamples, { exact: false }).length, 2);
});

test("the stage card selects p95 and converts seconds to milliseconds, including a real zero", () => {
  const series = (quantile: string): MetricSeries => ({
    labels: { instance: "inst-1", quantile }, points: [[1000, 0], [2000, 0.25]],
  });
  assert.deepEqual(stageP95Millis([series("0.5"), series("0.95"), series("0.99")], "prefill_ms"), [{
    labels: { instance: "inst-1", quantile: "0.95", metric: "prefill_ms" }, points: [[1000, 0], [2000, 250]],
  }]);
});

test("a restarted engine counts preemptions observed since the reset", () => {
  const [rate] = rateSeries([{ labels: { instance: "inst-1" }, points: [[1000, 40], [2000, 2], [3000, 2]] }]);
  assert.deepEqual(rate.points, [[2000, 2], [3000, 0]]);
});

test("an instance counter needs two samples before it can show a preemption rate", async () => {
  let samples: [number, number][] = [[1000, 3], [2000, 4]];
  api.metricsQuery = async (metric, params) => list(metric.endsWith("preemptions_total")
    ? [{ labels: { instance: params?.instance ?? "" }, points: samples }] : []);
  const panel = (uuid: string) =>
    <I18nextProvider i18n={i18n}><InstanceStagePanel instanceUuid={uuid} selection={quick} /></I18nextProvider>;
  const view = render(panel("inst-1"));
  await waitFor(() => assert.equal(screen.getAllByText(noSamples, { exact: false }).length, 1));
  samples = [[3000, 0]];
  view.rerender(panel("inst-2"));
  await waitFor(() => assert.equal(screen.getAllByText(noSamples, { exact: false }).length, 2));
});

test("stageSeriesName names the wire's stage lines through the translated stage vocabulary", () => {
  const wire = (monitorFixture.charts.stage_time as Array<{ labels: Record<string, string> }>).map((series) => series.labels);
  assert.deepEqual(wire.map((labels, index) => stageSeriesName(labels, index, t)), ["Queue", "Prefill", "Decode"]);
});

test("stageSeriesName falls back to defaultSeriesName for an unrecognized or absent metric label", () => {
  assert.equal(stageSeriesName({ instance: "inst-1" }, 0, t), "inst-1");
  assert.equal(stageSeriesName({ metric: "other" }, 3, t), "other");
});
