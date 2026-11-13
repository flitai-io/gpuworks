import assert from "node:assert/strict";
import fs from "node:fs";
import { afterEach, test } from "node:test";
import { JSDOM } from "jsdom";
import React from "react";
import { createInstance } from "i18next";
import { I18nextProvider, initReactI18next } from "react-i18next";
import { cursorPage, list } from "./helpers/listPage.ts";

const dom = new JSDOM("<!doctype html><html><body></body></html>", { url: "http://localhost/" });
for (const name of ["window", "document", "navigator", "HTMLElement", "SVGElement", "Element", "Node", "MutationObserver", "ShadowRoot"] as const) {
  Object.defineProperty(globalThis, name, { value: dom.window[name], configurable: true });
}
Object.defineProperty(globalThis, "getComputedStyle", { value: (element: Element) => dom.window.getComputedStyle(element), configurable: true });
Object.defineProperty(dom.window, "matchMedia", { configurable: true, value: () => ({ matches: false, addListener() {}, removeListener() {}, addEventListener() {}, removeEventListener() {}, dispatchEvent: () => false }) });
class ResizeObserverStub { observe() {} unobserve() {} disconnect() {} }
Object.defineProperty(globalThis, "ResizeObserver", { value: ResizeObserverStub, configurable: true });

// The metrics tab mounts ECharts (via TimeSeriesChart); jsdom has no 2d
// context, so stub one the same way inferenceMetrics.test.tsx does.
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

const { MemoryRouter } = await import("react-router-dom");
const { cleanup, render, screen, fireEvent, waitFor } = await import("@testing-library/react");

// A faithful EventSource: the product registers named change topics through
// addEventListener and reads event.data, so a stub without them would hide
// every subscription bug instead of exercising one.
const openEventSources: EventSourceStub[] = [];
class EventSourceStub {
  onmessage: ((event: MessageEvent) => void) | null = null;
  onerror: (() => void) | null = null;
  listeners = new Map<string, Set<(event: MessageEvent) => void>>();
  closed = false;
  constructor() { openEventSources.push(this); }
  addEventListener(type: string, handler: (event: MessageEvent) => void) {
    if (!this.listeners.has(type)) this.listeners.set(type, new Set());
    this.listeners.get(type)!.add(handler);
  }
  removeEventListener(type: string, handler: (event: MessageEvent) => void) {
    this.listeners.get(type)?.delete(handler);
  }
  close() { this.closed = true; }
}
function emitChange(topic: string, payload: Record<string, unknown>) {
  for (const source of openEventSources) {
    if (source.closed) continue;
    for (const handler of source.listeners.get(topic) ?? []) {
      handler({ data: JSON.stringify(payload) } as MessageEvent);
    }
  }
}
Object.defineProperty(globalThis, "EventSource", { value: EventSourceStub, configurable: true });

const { api } = await import("../src/api.ts");
const { default: InstancePanel } = await import("../src/pages/InstancePanel.tsx");
const { LogStream } = await import("../src/pages/InstanceLogStream.tsx");
const originalFetch = globalThis.fetch;
const originalInstanceEvents = api.instanceEvents;
const originalMetricsQuery = api.metricsQuery;
const originalMonitorDeployment = api.monitorDeployment;

afterEach(() => {
  cleanup();
  globalThis.fetch = originalFetch;
  api.instanceEvents = originalInstanceEvents;
  api.metricsQuery = originalMetricsQuery;
  api.monitorDeployment = originalMonitorDeployment;
});

test("failed instances show the persisted log tail in chronological order", async () => {
  globalThis.fetch = (async () => new Response(JSON.stringify({ items: [
    { id: 2, instance: "inst-1", node: "node-1", level: "", source: "instance", time_ms: 2, line: "second" },
    { id: 1, instance: "inst-1", node: "node-1", level: "", source: "instance", time_ms: 1, line: "first" },
  ] }), { status: 200, headers: { "Content-Type": "application/json" } })) as typeof fetch;

  const { container } = render(<I18nextProvider i18n={i18n}><LogStream uuid="inst-1" live={false} follow={true} range={null} /></I18nextProvider>);
  // Persisted lines carry a server-arrival ts prefix (timezone-dependent), so
  // assert its shape rather than a rendered clock value.
  await waitFor(() => assert.match(
    container.querySelector("pre")?.textContent ?? "",
    /^\[\d\d-\d\d \d\d:\d\d:\d\d\] first\n\[\d\d-\d\d \d\d:\d\d:\d\d\] second$/,
  ));
});

test("disabling follow keeps the view anchored; re-enabling snaps to the newest line", async () => {
  // JSDOM has no layout engine, so pin the <pre> geometry: scrollHeight 500
  // and a scrollTop that records assignments without any layout.
  const prePrototype = dom.window.HTMLPreElement.prototype as unknown as { scrollHeight: number; scrollTop: number };
  const savedScrollHeight = Object.getOwnPropertyDescriptor(prePrototype, "scrollHeight");
  const savedScrollTop = Object.getOwnPropertyDescriptor(prePrototype, "scrollTop");
  Object.defineProperty(prePrototype, "scrollHeight", { configurable: true, get: () => 500 });
  let assignedScrollTop = 0;
  Object.defineProperty(prePrototype, "scrollTop", {
    configurable: true,
    get: () => assignedScrollTop,
    set: (value: number) => { assignedScrollTop = value; },
  });

  try {
    globalThis.fetch = (async () => new Response(JSON.stringify({ items: [
      { id: 1, instance: "inst-scroll", node: "node-1", level: "", source: "instance", time_ms: 1, line: "only" },
    ] }), { status: 200, headers: { "Content-Type": "application/json" } })) as typeof fetch;

    const view = render(<I18nextProvider i18n={i18n}><LogStream uuid="inst-scroll" live={false} follow={false} range={null} /></I18nextProvider>);
    await waitFor(() => assert.match(
      view.container.querySelector("pre")?.textContent ?? "",
      /^\[\d\d-\d\d \d\d:\d\d:\d\d\] only$/,
    ));
    assert.equal(assignedScrollTop, 0, "follow disabled must not jump to the bottom");

    view.rerender(<I18nextProvider i18n={i18n}><LogStream uuid="inst-scroll" live={false} follow={true} /></I18nextProvider>);
    await waitFor(() => assert.equal(assignedScrollTop, 500), "re-enabling follow must snap to the newest line");
  } finally {
    if (savedScrollHeight) Object.defineProperty(prePrototype, "scrollHeight", savedScrollHeight);
    else delete prePrototype.scrollHeight;
    if (savedScrollTop) Object.defineProperty(prePrototype, "scrollTop", savedScrollTop);
    else delete prePrototype.scrollTop;
  }
});

test("startup logs appear below runtime details and stay in place when the instance becomes ready", async () => {
  api.instanceEvents = async () => cursorPage([]);
  api.metricsQuery = async () => list([]);
  api.monitorDeployment = (async () => ({
    as_of: 0, stale: false,
    deployment: { id: 1, name: "dep", engine: "vllm", placement: "", replicas: { ready: 1, desired: 1 } },
    alignment: null, ttft_breakdown: null,
    charts: { latency: [], tpot: [], concurrency: [], kv: [], throughput: [], stage_time: [], preemptions: [] },
    replicas: [],
  })) as typeof api.monitorDeployment;
  const base = {
    uuid: "inst-1", deployment_id: 1, node_uuid: "node-1", state: "starting",
    gpu_indexes: [0], ports: [30000], api_port: 30000, image: "image",
    message: "", container_id: "container-1", sleep_level: 0,
  } as never;
  const wrap = (inst: never) => <I18nextProvider i18n={i18n}><MemoryRouter><InstancePanel inst={inst} /></MemoryRouter></I18nextProvider>;
  const view = render(wrap(base));
  const logs = screen.getByRole("region", { name: "Logs" });
  const details = document.getElementById("instance-inst-1")!;
  assert.ok(details.contains(logs));
  assert.ok(screen.getByText("Follow latest logs"));
  assert.ok(screen.getByText("Launch Command").compareDocumentPosition(logs) & Node.DOCUMENT_POSITION_FOLLOWING);
  view.rerender(wrap({ ...base, state: "running" } as never));
  assert.equal(screen.getByRole("region", { name: "Logs" }), logs);
  assert.ok(logs.compareDocumentPosition(screen.getByRole("heading", { name: "Instance Metrics" })) & Node.DOCUMENT_POSITION_FOLLOWING);
});

test("changing metric groups preserves the independent log controls", async () => {
  api.instanceEvents = async () => cursorPage([]);
  api.metricsQuery = async () => list([]);
  api.monitorDeployment = (async () => ({
    as_of: 0, stale: false,
    deployment: { id: 1, name: "dep", engine: "vllm", placement: "", replicas: { ready: 1, desired: 1 } },
    alignment: null, ttft_breakdown: null,
    charts: { latency: [], tpot: [], concurrency: [], kv: [], throughput: [], stage_time: [], preemptions: [] },
    replicas: [],
  })) as typeof api.monitorDeployment;
  const base = {
    uuid: "inst-1", deployment_id: 1, node_uuid: "node-1", state: "running",
    gpu_indexes: [0], ports: [30000], api_port: 30000, image: "image",
    message: "", container_id: "container-1", sleep_level: 0,
  } as never;
  const wrap = (inst: never) => <I18nextProvider i18n={i18n}><MemoryRouter><InstancePanel inst={inst} /></MemoryRouter></I18nextProvider>;
  render(wrap(base));

  const follow = await screen.findByText("Follow latest logs");
  const logs = screen.getByRole("region", { name: "Logs" });
  assert.ok(logs.contains(follow));
  fireEvent.click(screen.getByRole("tab", { name: "Cache & engine" }));
  assert.equal(screen.getByText("Follow latest logs"), follow);
  assert.ok(screen.getByRole("tab", { name: "Cache & engine", selected: true }));

});

test("progress notifications do not refetch the instance event history; milestones do", async () => {
  let calls = 0;
  api.instanceEvents = async () => { calls += 1; return cursorPage([]); };
  const base = {
    uuid: "inst-1", deployment_id: 1, node_uuid: "node-1", state: "starting", phase: "image_pull",
    gpu_indexes: [0], ports: [30000], api_port: 30000, image: "image",
    message: "layer one", container_id: "container-1", sleep_level: 0,
  } as never;
  const wrap = (inst: never) => <I18nextProvider i18n={i18n}><MemoryRouter><InstancePanel inst={inst} /></MemoryRouter></I18nextProvider>;
  const view = render(wrap(base));
  await waitFor(() => assert.equal(calls, 1));

  // A new pull line re-renders the panel but appends no event.
  view.rerender(wrap({ ...base, message: "layer two" } as never));
  await new Promise((resolve) => setTimeout(resolve, 20));
  assert.equal(calls, 1, "a re-render must not refetch the timeline");

  emitChange("instance", { key: "inst-1", reason: "progress" });
  await new Promise((resolve) => setTimeout(resolve, 20));
  assert.equal(calls, 1, "a progress notification appends no event, so nothing to refetch");

  emitChange("instance", { key: "other-instance", reason: "running" });
  await new Promise((resolve) => setTimeout(resolve, 20));
  assert.equal(calls, 1, "another instance's milestone is not this instance's timeline");

  emitChange("instance", { key: "inst-1", reason: "running" });
  await waitFor(() => assert.equal(calls, 2), "a milestone for this instance refetches once");
});

test("the launch command field shows the desired-state command and the manual badge", () => {
  api.instanceEvents = async () => cursorPage([]);
  api.metricsQuery = async () => list([]);
  api.monitorDeployment = (async () => ({
    as_of: 0, stale: false,
    deployment: { id: 1, name: "dep", engine: "vllm", placement: "", replicas: { ready: 1, desired: 1 } },
    alignment: null, ttft_breakdown: null,
    charts: { latency: [], tpot: [], concurrency: [], kv: [], throughput: [], stage_time: [], preemptions: [] },
    replicas: [],
  })) as typeof api.monitorDeployment;
  const base = {
    uuid: "inst-cmd", deployment_id: 1, node_uuid: "node-1", state: "running",
    gpu_indexes: [0], ports: [30000], api_port: 30000, image: "image",
    message: "", container_id: "container-1", sleep_level: 0,
    entrypoint: ["/bin/sh"], argv: ["-c", "vllm serve /model --port 30000"],
    command_mode: "manual",
  } as never;
  const wrap = (inst: never) => <I18nextProvider i18n={i18n}><MemoryRouter><InstancePanel inst={inst} /></MemoryRouter></I18nextProvider>;
  render(wrap(base));

  assert.ok(screen.getByText("Launch Command"), "the launch command field replaces the injected-args ledger");
  const commandNode = screen.getByText((_, element) => element?.tagName === "CODE" && element?.textContent === "/bin/sh -c 'vllm serve /model --port 30000'");
  assert.ok(commandNode, "the command renders as one wrapping monospace block, not one token per line");
  assert.ok(screen.getByText("Manual command"), "manual command mode must be flagged");
  assert.equal(screen.queryByText("Injected Arguments"), null, "the injected-args ledger must not reappear");
});

test("an instance without a parseable launch snapshot shows the command placeholder", () => {
  api.instanceEvents = async () => cursorPage([]);
  api.metricsQuery = async () => list([]);
  api.monitorDeployment = (async () => ({
    as_of: 0, stale: false,
    deployment: { id: 1, name: "dep", engine: "vllm", placement: "", replicas: { ready: 1, desired: 1 } },
    alignment: null, ttft_breakdown: null,
    charts: { latency: [], tpot: [], concurrency: [], kv: [], throughput: [], stage_time: [], preemptions: [] },
    replicas: [],
  })) as typeof api.monitorDeployment;
  const base = {
    uuid: "inst-nocmd", deployment_id: 1, node_uuid: "node-1", state: "running",
    gpu_indexes: [0], ports: [30000], api_port: 30000, image: "image",
    message: "", container_id: "container-1", sleep_level: 0,
  } as never;
  const wrap = (inst: never) => <I18nextProvider i18n={i18n}><MemoryRouter><InstancePanel inst={inst} /></MemoryRouter></I18nextProvider>;
  render(wrap(base));

  assert.ok(screen.getByText("Launch Command"));
  assert.ok(screen.getAllByText("—").length >= 1, "an absent command falls back to the placeholder");
});
