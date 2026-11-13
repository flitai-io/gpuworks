import assert from "node:assert/strict";
import fs from "node:fs";
import { afterEach, test } from "node:test";
import { JSDOM } from "jsdom";
import React from "react";
import { createInstance } from "i18next";
import { I18nextProvider, initReactI18next } from "react-i18next";
import { list } from "./helpers/listPage.ts";
import { installCanvas } from "./helpers/analyticsFixtures.ts";

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

const { act, cleanup, render, screen, waitFor, fireEvent, within } = await import("@testing-library/react");
const userEventModule = await import("@testing-library/user-event");
const userEvent = userEventModule.default ?? userEventModule;

const { api } = await import("../src/api.ts");
const { default: DeploymentMetricsPanel } = await import("../src/components/DeploymentMetricsPanel.tsx");
const originalMetricsQuery = api.metricsQuery;

afterEach(() => {
  cleanup();
  api.metricsQuery = originalMetricsQuery;
});

const runningInstance = { uuid: "inst-range-1", state: "running", node_uuid: "node-1" } as never;

function renderPanel() {
  return render(
    <I18nextProvider i18n={i18n}><DeploymentMetricsPanel instances={[runningInstance]} /></I18nextProvider>,
  );
}

// The metrics stub returns no points, so the panel shows its waiting state
// with the range control; the queries themselves are what this file asserts.
function recordMetricsQuery() {
  const calls: Array<Record<string, string>> = [];
  api.metricsQuery = (async (metric: string, params: Record<string, string>) => {
    calls.push({ metric, ...params });
    return list([]);
  }) as typeof api.metricsQuery;
  return calls;
}

async function chooseRange(label: string) {
  const user = userEvent.setup({ document: dom.window.document });
  const combo = screen.getByRole("combobox", { name: "Time range" });
  fireEvent.click(combo);
  const option = await screen.findByRole("option", { name: label });
  await user.click(option);
}

test("the default 15-minute window queries a 10s step for every metric", async () => {
  const calls = recordMetricsQuery();
  renderPanel();

  await waitFor(() => assert.ok(calls.length >= 5, `expected the metric fan-out, got ${calls.length}`));
  for (const call of calls) {
    assert.equal(call.step, "10s", `unexpected step for ${call.metric}`);
    const windowMS = Number(call.end) - Number(call.start);
    assert.ok(Math.abs(windowMS - 15 * 60 * 1000) < 5_000, `window ${windowMS}ms is not ~15m`);
    assert.equal(call.instance, "inst-range-1");
  }
  assert.ok(screen.getByText("Waiting for Deployment Metrics"));
  assert.equal(screen.getAllByText("No data").length, 3);
  await waitFor(() => assert.equal(screen.getAllByText("—", { selector: "[data-slot='metric-card-value-row'] > span:first-child" }).length, 4));
});

test("a zero reading fills only its own chart; one token counter sample cannot establish throughput", async () => {
  let hasRequestSample = false;
  let release!: () => void;
  const pending = new Promise<void>((resolve) => { release = resolve; });
  api.metricsQuery = async (metric) => {
    await pending;
    return list((hasRequestSample && metric.endsWith("requests_running")) || metric.endsWith("generation_tokens_total")
      ? [{ labels: {}, points: [[1000, 0]] }]
      : []);
  };
  renderPanel();
  assert.equal(screen.getAllByText("No data").length, 3);
  await act(async () => release());
  assert.match(screen.getByRole("status").textContent ?? "", /Waiting for Deployment Metrics.*After the agent collects/);
  assert.equal(screen.getAllByText("No data").length, 3);
  hasRequestSample = true;
  await chooseRange("1 hour");
  await waitFor(() => assert.ok(!screen.queryByText("Waiting for Deployment Metrics")));
  const cards = within(screen.getByRole("region", { name: "Deployment Metrics" }));
  const value = (title: string) => cards.getByText(title).closest("[data-slot='metric-card']")?.querySelector("[data-slot='metric-card-value-row']")?.textContent;
  assert.equal(value("Running Instances"), "1");
  assert.equal(value("Running Requests"), "0");
  assert.equal(value("Waiting Requests"), "—");
  assert.equal(value("Generation Throughput"), "—");
  assert.equal(screen.getAllByText("No data").length, 2);
  assert.ok(screen.getByText(i18n.t("metrics.tokenDescription", {prompt:"—", generation:"—"})));
});

test("picking a quick range widens the window and coarsens the step", async () => {
  const calls = recordMetricsQuery();
  renderPanel();
  await waitFor(() => assert.ok(calls.length >= 5));
  calls.length = 0;

  await chooseRange("1 hour");

  await waitFor(() => assert.ok(calls.length >= 5, `re-query did not happen: ${calls.length}`));
  for (const call of calls) {
    assert.equal(call.step, "30s", `unexpected step for ${call.metric}`);
    const windowMS = Number(call.end) - Number(call.start);
    assert.ok(Math.abs(windowMS - 60 * 60 * 1000) < 5_000, `window ${windowMS}ms is not ~1h`);
  }
});

test("a range beyond retention keeps querying but shows the warning", async () => {
  const calls = recordMetricsQuery();
  renderPanel();
  await waitFor(() => assert.ok(calls.length >= 5));
  calls.length = 0;

  await chooseRange("1 month");

  assert.ok(await screen.findByText(/exceeds the 7-day retention/), "retention warning missing");
  await waitFor(() => assert.ok(calls.length >= 5, "1 month must still query the retained window"));
});


test("history includes replaced replicas while the summary reflects the active replicas", async () => {
  const inspected: string[] = [];
  api.metricsQuery = async (metric, params) => {
    const uuid = params?.instance;
    assert.ok(uuid, "every query must be scoped to an instance of the inspected deployment");
    if (metric.endsWith("generation_tokens_total")) {
      if (uuid === "live-2") return list([{ labels: { instance: uuid }, points: [[1000, 20], [2000, 7]] }]);
      if (uuid === "live-3") return list([{ labels: { instance: uuid }, points: [[1000, 10], [3000, 18]] }]);
    }
    if (!metric.endsWith("requests_waiting")) return list([]);
    if (uuid === "live-2") return list([{ labels: { instance: uuid }, points: [[3000, 3]] }]);
    if (uuid === "live-3") return list([{ labels: { instance: uuid }, points: [[4000, 4]] }]);
    if (uuid === "inst-range-1") return list([
      { labels: { deployment: "7" }, points: [[1000, 9]] },
      { labels: { deployment: "8", instance: "unrelated" }, points: [[2000, 900]] },
    ]);
    return list([]);
  };
  render(<I18nextProvider i18n={i18n}>
    <DeploymentMetricsPanel instances={[
      { uuid: "inst-range-1", state: "failed", role: "prefill" }, { uuid: "live-2", state: "running" },
      { uuid: "live-3", state: "starting" }, { uuid: "live-4", state: "optimizing" },
    ] as never} deploymentId={7}
      onSelectInstance={(uuid) => inspected.push(uuid)} />
  </I18nextProvider>);
  const chooser = await screen.findByRole("combobox", { name: "View instance metrics" });
  const summary = screen.getByRole("region", { name: "Deployment Metrics" });
  const titles = [...summary.querySelectorAll("[data-slot='metric-card-label']")].map(el => el.textContent);
  assert.deepEqual(titles, ["Running Instances", "Running Requests", "Waiting Requests", "Peak GPU Cache", "Generation Throughput"]);
  const values = [...summary.querySelectorAll("[data-slot='metric-card-value-row'] > span:first-child")].map(el => el.textContent);
  assert.deepEqual(values, ["3", "—", "7", "—", "4.0tok/s"], "count an active replica before its first sample, and combine each active replica’s own latest reading");
  assert.ok(screen.getByText(i18n.t("metrics.instanceCache")));
  fireEvent.click(chooser);
  assert.equal(screen.queryByRole("option", { name: /unrelated/ }), null);
  fireEvent.click(screen.getByRole("option", { name: /Prefill.*inst-range-1/i }));
  assert.deepEqual(inspected, ["inst-range-1"]);
});

test("switching from deployment to instance retains the selected time range for engine and KV queries", async () => {
  const { MetricWindowProvider } = await import("../src/components/MetricWindow.tsx");
  const { default: InstanceMetricsTab } = await import("../src/pages/InstanceMetricsTab.tsx");
  const calls = recordMetricsQuery();
  const inst = { uuid: "inst-range-1", state: "stopped" } as never;
  function view(detail: boolean) {
    return <I18nextProvider i18n={i18n}><MetricWindowProvider>{detail
      ? <InstanceMetricsTab inst={inst} />
      : <DeploymentMetricsPanel instances={[inst]} />}</MetricWindowProvider></I18nextProvider>;
  }
  const panel = render(view(false));
  await chooseRange("1 hour");
  calls.length = 0;
  panel.rerender(view(true));
  fireEvent.click(screen.getByRole("tab", { name: "Cache & engine" }));
  await waitFor(() => assert.ok(calls.some((call) => call.metric === "gpuworks_kv_disk_used_bytes")));
  assert.ok(calls.every((call) => call.step === "30s"), "all instance charts must use the inherited 1h window");
  assert.ok(await screen.findByText("No metric samples in the selected range"));
});
