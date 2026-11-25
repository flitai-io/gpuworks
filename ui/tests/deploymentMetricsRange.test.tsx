import assert from "node:assert/strict";
import fs from "node:fs";
import { afterEach, beforeEach, test } from "node:test";
import { JSDOM } from "jsdom";
import React from "react";
import { createInstance } from "i18next";
import { I18nextProvider, initReactI18next } from "react-i18next";
import { list } from "./helpers/listPage.ts";
import { installCanvas, retentionView } from "./helpers/analyticsFixtures.ts";

const dom = new JSDOM("<!doctype html><html><body></body></html>", { url: "http://localhost/", pretendToBeVisual: true });
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

const { MemoryRouter } = await import("react-router-dom");
const { api } = await import("../src/api.ts");
const { default: DeploymentMetricsPanel } = await import("../src/components/DeploymentMetricsPanel.tsx");
const originalMetricsQuery = api.metricsQuery;
const originalSignals = api.monitorDeploymentSignals;
const originalDataRetention = api.dataRetention;

beforeEach(() => {
  api.dataRetention = async () => retentionView();
});

afterEach(() => {
  cleanup();
  api.metricsQuery = originalMetricsQuery;
  api.monitorDeploymentSignals = originalSignals;
  api.dataRetention = originalDataRetention;
});

const runningInstance = { uuid: "inst-range-1", state: "running", node_uuid: "node-1" } as never;

function renderPanel() {
  return render(
    <MemoryRouter><I18nextProvider i18n={i18n}><DeploymentMetricsPanel instances={[runningInstance]} deploymentId={7} /></I18nextProvider></MemoryRouter>,
  );
}

const emptySignals = { start_ms: 0, end_ms: 0, step_ms: 10_000, sources: {}, concurrency: [], kv: [], throughput: [] };

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

// Every signals query the panel issues, answered with no data.
function recordSignals() {
  const calls: Array<{ id: number; start: number; end: number; step: string }> = [];
  api.monitorDeploymentSignals = (async (id: number, window: { start: number; end: number; step: string }) => {
    calls.push({ id, ...window });
    return emptySignals;
  }) as typeof api.monitorDeploymentSignals;
  return calls;
}

async function chooseRange(label: string) {
  const user = userEvent.setup({ document: dom.window.document });
  const combo = screen.getByRole("combobox", { name: "Time range" });
  fireEvent.click(combo);
  const option = await screen.findByRole("option", { name: label });
  await user.click(option);
  await waitFor(() => assert.equal(combo.getAttribute("aria-expanded"), "false", "wait for the selection acknowledgment to close the popup"));
}

test("the default 15-minute window asks the server for the deployment's signals at a 10s step", async () => {
  const calls = recordSignals();
  renderPanel();

  await waitFor(() => assert.ok(calls.length >= 1, "the panel never asked for signals"));
  for (const call of calls) {
    assert.equal(call.id, 7);
    assert.equal(call.step, "10s");
    assert.ok(Math.abs(call.end - call.start - 15 * 60 * 1000) < 5_000, `window ${call.end - call.start}ms is not ~15m`);
  }
  // One request per window: no per-instance, per-metric fan-out.
  assert.equal(calls.length, 1);
  await waitFor(() => assert.ok(screen.getByText("No engine metrics, and no requests through the gateway in the selected range")));
  assert.equal(screen.getAllByText("No data").length, 3);
  await waitFor(() => assert.equal(screen.getAllByText("—", { selector: "[data-slot='metric-card-value-row'] > span:first-child" }).length, 4));
});

test("a zero reading fills only its own chart; one token counter sample cannot establish throughput", async () => {
  let release!: () => void;
  const pending = new Promise<void>((resolve) => { release = resolve; });
  api.monitorDeploymentSignals = (async () => {
    await pending;
    return { ...emptySignals, sources: { running: "engine" }, concurrency: [{ labels: { metric: "running", instance: "inst-range-1" }, points: [[1000, 0]] }] };
  }) as typeof api.monitorDeploymentSignals;
  renderPanel();
  assert.equal(screen.getAllByText("No data").length, 3);
  assert.match(screen.getByRole("status").textContent ?? "", /Waiting for Deployment Metrics.*After the agent collects/);
  await act(async () => release());
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
  const calls = recordSignals();
  renderPanel();
  await waitFor(() => assert.ok(calls.length >= 1));
  calls.length = 0;

  await chooseRange("1 hour");

  await waitFor(() => assert.ok(calls.length >= 1, "re-query did not happen"));
  for (const call of calls) {
    assert.equal(call.step, "30s");
    assert.ok(Math.abs(call.end - call.start - 60 * 60 * 1000) < 5_000, `window ${call.end - call.start}ms is not ~1h`);
  }
});

test("the default 72-hour retention warns for a week while still querying the selected window", async () => {
  const calls = recordSignals();
  renderPanel();
  await waitFor(() => assert.ok(calls.length >= 1));
  calls.length = 0;

  await chooseRange("1 week");

  assert.ok(await screen.findByText(/exceeds the 72-hour retention/), "retention warning missing");
  await waitFor(() => assert.ok(calls.length >= 1, "1 week must still query the selected window"));
  for (const call of calls) {
    assert.equal(call.end - call.start, 604_800_000, "retention must not truncate the requested window");
  }
});

test("metric retention is independent of log retention and refreshes when the range changes", async () => {
  recordSignals();
  renderPanel();
  await chooseRange("1 week");
  assert.ok(await screen.findByText(/exceeds the 72-hour retention/));

  api.dataRetention = async () => retentionView({ metrics_hours: 240, logs_hours: 24 });
  await chooseRange("1 month");
  assert.ok(await screen.findByText(/exceeds the 240-hour retention/));
  await chooseRange("1 week");
  await waitFor(() => assert.ok(!screen.queryByText(/exceeds the \d+-hour retention/), "the retained range must not carry an expiry warning"));
});

test("returning to an open panel picks up retention changes without changing the selected range", async () => {
  let hours = 72;
  api.dataRetention = async () => retentionView({ metrics_hours: hours });
  recordSignals();
  renderPanel();
  await chooseRange("1 week");
  assert.ok(await screen.findByText(/exceeds the 72-hour retention/));
  hours = 240;
  await act(async () => document.dispatchEvent(new dom.window.Event("visibilitychange")));
  await waitFor(() => assert.ok(!screen.queryByText(/exceeds the \d+-hour retention/), "the retained range must not carry an expiry warning"));
  assert.ok(screen.getByRole("combobox", { name: "Time range" }).textContent?.includes("1 week"));
});

test("pending retention does not block metric queries or invent a retention warning", async () => {
  let release!: (value: Awaited<ReturnType<typeof api.dataRetention>>) => void;
  const pending = new Promise<Awaited<ReturnType<typeof api.dataRetention>>>(resolve => { release = resolve; });
  api.dataRetention = () => pending;
  const calls = recordSignals();
  renderPanel();
  await waitFor(() => assert.ok(calls.length >= 1));
  await chooseRange("1 week");
  assert.ok(!screen.queryByText(/exceeds the .* retention/), "do not invent a retention warning");
  assert.ok(!screen.queryByText(/Unable to confirm metric retention/), "do not show an unavailable warning after a successful read");
  await act(async () => release(retentionView()));
  assert.ok(await screen.findByText(/exceeds the 72-hour retention/));
});

test("an unusable retention response is reported instead of treated as unlimited history", async () => {
  api.dataRetention = async () => retentionView({ metrics_hours: 0 });
  recordSignals();
  renderPanel();
  assert.ok(await screen.findByText("Unable to confirm metric retention. The selected range may include expired data."));
  await chooseRange("1 week");
  assert.ok(!screen.queryByText(/exceeds the .* retention/), "do not invent a retention warning");
});

test("retention read failures are explicit and recover without blocking queries", async () => {
  api.dataRetention = async () => { throw new Error("service unavailable"); };
  const calls = recordSignals();
  renderPanel();
  assert.ok(await screen.findByText("Unable to confirm metric retention. The selected range may include expired data."));
  await waitFor(() => assert.ok(calls.length >= 1));
  assert.ok(!screen.queryByText(/exceeds the .* retention/), "do not invent a retention warning");
  api.dataRetention = async () => retentionView();
  await chooseRange("1 week");
  assert.ok(await screen.findByText(/exceeds the 72-hour retention/));
  assert.ok(!screen.queryByText(/Unable to confirm metric retention/), "do not show an unavailable warning after a successful read");
});

test("instance metrics use the configured hours, including a retention that is not whole days", async () => {
  api.dataRetention = async () => retentionView({ metrics_hours: 36, logs_hours: 72 });
  const { default: InstanceMetricsTab } = await import("../src/pages/InstanceMetricsTab.tsx");
  recordMetricsQuery();
  render(<I18nextProvider i18n={i18n}><InstanceMetricsTab inst={runningInstance} /></I18nextProvider>);
  await chooseRange("1 day");
  assert.ok(!screen.queryByText(/exceeds the .* retention/), "do not invent a retention warning");
  await chooseRange("1 week");
  assert.ok(await screen.findByText(/exceeds the 36-hour retention/));
  fireEvent.click(screen.getByRole("tab", { name: "Cache & engine" }));
  assert.ok(screen.getByText(/exceeds the 36-hour retention/));
});


test("history includes replaced replicas while the summary reflects the active replicas", async () => {
  const inspected: string[] = [];
  const at = (instance: string, metric: string, points: Array<[number, number]>) => ({ labels: { metric, instance }, points });
  api.monitorDeploymentSignals = (async () => ({
    ...emptySignals,
    sources: { running: "engine", generation_tokens: "engine" },
    concurrency: [
      at("live-2", "waiting", [[3000, 3]]), at("live-3", "waiting", [[4000, 4]]),
      // The failed replica's last reading is history, not part of "now".
      at("inst-range-1", "waiting", [[1000, 9]]), at("inst-range-1", "running", [[1000, 5]]),
    ],
    throughput: [
      at("live-2", "generation_tok_s", [[2000, 7]]), at("live-3", "generation_tok_s", [[3000, 18]]),
      at("inst-range-1", "generation_tok_s", [[1000, 500]]),
    ],
  })) as typeof api.monitorDeploymentSignals;
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
  assert.deepEqual(values, ["3", "—", "7", "—", "25.0tok/s"], "count an active replica before its first sample, and combine each active replica’s own latest reading");
  assert.ok(screen.getByText(i18n.t("metrics.instanceCache")));
  fireEvent.click(chooser);
  assert.ok(screen.getByRole("option", { name: /live-2/ }));
  fireEvent.click(screen.getByRole("option", { name: /Prefill.*inst-range-1/i }));
  assert.deepEqual(inspected, ["inst-range-1"]);
});

test("switching from deployment to instance retains the selected time range for engine and KV queries", async () => {
  const { MetricWindowProvider } = await import("../src/components/MetricWindow.tsx");
  const { default: InstanceMetricsTab } = await import("../src/pages/InstanceMetricsTab.tsx");
  const calls = recordMetricsQuery();
  recordSignals();
  const inst = { uuid: "inst-range-1", state: "stopped" } as never;
  function view(detail: boolean) {
    return <I18nextProvider i18n={i18n}><MetricWindowProvider>{detail
      ? <InstanceMetricsTab inst={inst} />
      : <DeploymentMetricsPanel instances={[inst]} deploymentId={7} />}</MetricWindowProvider></I18nextProvider>;
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
