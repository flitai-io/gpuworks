import assert from "node:assert/strict";
import fs from "node:fs";
import { afterEach, test } from "node:test";
import { JSDOM } from "jsdom";
import React from "react";
import { createInstance } from "i18next";
import { I18nextProvider, initReactI18next } from "react-i18next";

const dom = new JSDOM("<!doctype html><html><body></body></html>", { url: "http://localhost/" });
for (const name of ["window", "document", "navigator", "HTMLElement", "HTMLInputElement", "SVGElement", "Element", "Node", "MutationObserver", "ShadowRoot"] as const) {
  Object.defineProperty(globalThis, name, { value: dom.window[name], configurable: true });
}
Object.defineProperty(globalThis, "getComputedStyle", { value: (element: Element) => dom.window.getComputedStyle(element), configurable: true });
Object.defineProperty(dom.window, "matchMedia", { configurable: true, value: () => ({ matches: false, addListener() {}, removeListener() {}, addEventListener() {}, removeEventListener() {}, dispatchEvent: () => false }) });
class ResizeObserverStub { observe() {} unobserve() {} disconnect() {} }
Object.defineProperty(globalThis, "ResizeObserver", { value: ResizeObserverStub, configurable: true });
Object.defineProperties(dom.window.HTMLElement.prototype, {
  attachEvent: { configurable: true, value() {} },
  detachEvent: { configurable: true, value() {} },
});
// echarts needs a 2d context JSDOM cannot provide.
const anyContext = new Proxy(function () {} as never, {
  get: (_target: never, prop: string | symbol) => {
    if (prop === "canvas") return { width: 300, height: 200, style: {} };
    if (prop === "dpr") return 1;
    if (prop === "measureText") return () => ({ width: 10 });
    return anyContext;
  },
  set: () => true,
  apply: () => anyContext,
});
dom.window.HTMLCanvasElement.prototype.getContext = () => anyContext as CanvasRenderingContext2D;
class Events { addEventListener() {} removeEventListener() {} close() {} }
Object.defineProperty(globalThis, "EventSource", { value: Events, configurable: true });
class ShimPort {
  onmessage: ((ev: { data: unknown }) => void) | null = null;
  postMessage(data: unknown) { setTimeout(() => this.onmessage?.({ data }), 0); }
}
class ShimMessageChannel { port1 = new ShimPort(); port2 = new ShimPort(); }
Object.defineProperty(globalThis, "MessageChannel", { value: ShimMessageChannel, configurable: true });

const dashboard = JSON.parse(fs.readFileSync(new URL("../src/i18n/locales/en/dashboard.json", import.meta.url), "utf8"));
const i18n = createInstance();
await i18n.use(initReactI18next).init({ lng: "en", fallbackLng: "en", resources: { en: { dashboard, common: JSON.parse(fs.readFileSync(new URL("../src/i18n/locales/en/common.json", import.meta.url), "utf8")) } }, defaultNS: "dashboard", interpolation: { escapeValue: false } });
const { MemoryRouter, useLocation } = await import("react-router-dom");
const { cleanup, fireEvent, render, screen, waitFor } = await import("@testing-library/react");
const { api } = await import("../src/api.ts");
const { analyticsApi } = await import("../src/analyticsApi.ts");
const { default: DashboardPage } = await import("../src/pages/DashboardPage.tsx");
import type { DashboardSummary } from "../src/api";

const cards = { nodes: 3, online_nodes: 2, cards: 24, awake: 13, asleep: 4, free: 5, held_outside: 1, unhealthy: 1, offline: 0, contention: { waiting_wakes: 0, held_requests: 2, yields_last_hour: 6 }, allocated: 17 };
const summary: DashboardSummary = {
  current_metrics: { utilization_pct: 61.4, temperature_c: 74, power_watts: 4200, memory_used_bytes: 1, vram_used_bytes: 1084 * 1024 ** 3, vram_total_bytes: 1408 * 1024 ** 3 },
  nodes: { total: 3, online: 2, offline: 1, connecting: 0, gpus: 7, unusable_gpus: 1, vram_gib: 560, memory_gib: 192 },
  deployments: { total: 12, running: 7, warming: 1, sleeping: 2, pending: 1, other: 1, sleepable: 3 },
  cards,
  tasks: { batches: { active: 2, waiting: 1 }, benchmarks: 0 },
  by_vendor: [{ key: "nvidia", nodes: 3, gpus: 7, vram_gib: 560, unusable_gpus: 1 }],
  label_keys: [], label_key: "", by_label: [],
};
const originals = {
  health: api.health, dashboardSummary: api.dashboardSummary, providers: api.providers,
  modelsPage: api.modelsPage, gpus: api.gpus,
  nodesPage: api.nodesPage, monitorOverview: api.monitorOverview, storages: api.storages,
  storageUsage: api.storageUsage, storage: api.storage, usage: analyticsApi.usage,
};
let summaryRequests: unknown[][] = [];
const card = (uuid: string, name: string, vendor: string, gib: number) => ({ uuid, name, vendor, memory_total_bytes: gib * 1024 ** 3, allocation_state: "available" });
function stub() {
  summaryRequests = [];
  api.health = async () => ({ status: "ok", sessions: 3, version: "v0.9.0" }) as never;
  api.dashboardSummary = async (...args: unknown[]) => { summaryRequests.push(args); return summary; };
  api.providers = async () => ({ items: [{ id: 1, name: "zs", kind: "zstack", category: "private_cloud", state: "valid" }], total: 1 }) as never;
  api.modelsPage = async () => ({ items: [], total: 0, summary: { registered: 0, ready_files: 0, sources: 0, weight_gib: 0 } }) as never;
  api.gpus = async () => ({ items: [card("gpu-1", "H100", "nvidia", 80)], overview: { total: 1, allocation: { allocatable: 1, unusable: 0 } } }) as never;
  api.nodesPage = async () => ({ items: [], total: 0 }) as never;
  api.monitorOverview = async () => ({ as_of: 1000, stale: false, services: [], signals: [
    { key: "ready_replicas", collected: true, value: 14, denominator: 16 },
    { key: "request_rate", collected: true, value: 12.3, denominator: 19.96 },
    { key: "error_rate", collected: true, value: 1.2 },
    { key: "ttft", collected: true, value: 345.6, detail: { upper_bound: true } },
    { key: "waiting", collected: true, value: 2.4 },
  ] }) as never;
  api.storages = async () => ({ items: [], total: 0 });
  api.storageUsage = async () => ({ items: [], total: 0 });
  api.storage = async () => { throw Error("no storages"); };
  analyticsApi.usage = async () => ({ range: { from_ms: 1000, to_ms: 2000, granularity: "hour", time_zone: "UTC", earliest_ms: 1000, live_since_ms: 1000, buckets: [] }, totals: { requests: 8, no_usage: 1, prompt_tokens: 135, completion_tokens: 42, reasoning_tokens: 0, reasoning_reported: 0, cached_tokens: 32, cache_reported: 1, cache_reported_prompt: 135 }, rows: [], series: [] });
}
afterEach(() => { cleanup(); Object.assign(api, originals); analyticsApi.usage = originals.usage; });
let seenPath = "";
// The address with its query, so a link that opens a list filtered is told from one that does not.
function LocationProbe() { const { pathname, search } = useLocation(); seenPath = pathname + search; return null; }
function renderPage() { render(<I18nextProvider i18n={i18n}><MemoryRouter><DashboardPage /><LocationProbe /></MemoryRouter></I18nextProvider>); }
// node:assert parses this whole file to word a failed assert.ok without a
// message, and waitFor fails it on every poll until the page settles; a
// message keeps each poll cheap.
const ok = (value: unknown, message = "expected a truthy value") => assert.ok(value, message);
const block = (title: string) => [...document.querySelectorAll<HTMLElement>(".dashboard-block")].find((element) => element.querySelector("h2")?.textContent === title)!;
const panel = (title: string) => [...document.querySelectorAll<HTMLElement>(".dashboard-panel")].find((element) => element.querySelector("h3")?.textContent === title)!;
const arrow = (element: Element) => element.querySelector<HTMLAnchorElement>(".dashboard-panel-head a.dashboard-overview-module-arrow")?.getAttribute("href");

test("the page reads hardware first: compute, then platform services, then storage and connections", async () => {
  stub(); renderPage();
  assert.equal(screen.getByRole("heading", { name: "Overview", level: 2 }).textContent, "Overview");
  await waitFor(() => ok(document.querySelector(".dashboard-header-meta")?.textContent?.includes("Normal")));
  ok(document.querySelector(".dashboard-header-meta")?.textContent?.includes("Normal"));
  assert.deepEqual([...document.querySelectorAll(".dashboard-block > .dashboard-block-head h2")].map((title) => title.textContent), ["Compute resources", "Platform services", "Storage and connections"]);
  assert.deepEqual(summaryRequests, [[]]);
  assert.equal(document.querySelectorAll(".dashboard-overview-label-key").length, 0);
  ok(!document.body.textContent?.includes("by node label"));
});

test("compute is two panels: the hardware there is, then how it is used now", async () => {
  stub(); renderPage();
  const compute = await waitFor(() => { const element = block("Compute resources"); ok(element?.querySelector(".dashboard-kpis")); return element; });
  assert.deepEqual([...compute.querySelectorAll(".dashboard-compute > .dashboard-panel > .dashboard-panel-head h3")].map((title) => title.textContent), ["Hardware", "Usage"]);
  assert.equal(arrow(panel("Hardware")), "/nodes");
  assert.equal(arrow(panel("Usage")), "/scheduling");
});

test("the hardware panel counts nodes, GPUs and VRAM and greys zero node states without links", async () => {
  stub(); renderPage();
  const hardware = await waitFor(() => { const element = panel("Hardware"); ok(element?.textContent?.includes("560")); return element; });
  assert.deepEqual([...hardware.querySelectorAll(".dashboard-overview-metric.is-kpi")].map((metric) => [metric.querySelector('[data-slot="metric-card-label"]')?.textContent, metric.querySelector('[data-slot="metric-card-value-row"]')?.textContent, metric.querySelector('[data-slot="metric-card-footer"]')]),
    [["Compute nodes", "3nodes", null], ["GPUs", "7cards", null], ["Total VRAM", "560GiB", null]]);
  assert.deepEqual([...hardware.querySelectorAll(".dashboard-states-line .dashboard-overview-status")].map((status) => [status.textContent, status.classList.contains("is-zero")]), [["Online 2", false], ["Connecting 0", true], ["Offline 1", false]]);
  assert.equal(hardware.querySelectorAll(".dashboard-states-line a").length, 0);
  assert.equal(hardware.querySelector(".dashboard-subsection-head a")?.getAttribute("href"), "/gpus");
});

test("card occupancy shows all six states in two columns with zero greyed", async () => {
  stub(); renderPage();
  const usage = await waitFor(() => { const element = panel("Usage"); ok(element?.querySelector(".dashboard-ring-legend")); return element; });
  const rows = [...usage.querySelectorAll(".dashboard-ring-row")].map((row) => [row.querySelector("span")?.textContent, row.querySelector("b")?.textContent, row.classList.contains("is-zero")]);
  assert.deepEqual(rows, [["Serving", "13", false], ["Asleep", "4", false], ["Free", "5", false], ["Held outside the platform", "1", false], ["Faulty", "1", false], ["Node offline", "0", true]]);
  assert.equal(usage.querySelector(".dashboard-ring-center strong")?.textContent, "24");
  assert.equal(usage.querySelectorAll(".dashboard-ring-arc").length, 5);
  assert.equal(usage.querySelector(".dashboard-ring svg")?.getAttribute("aria-label"), "24 cards by state");
  const foot = usage.querySelector(".dashboard-usage-foot")!;
  ok(foot.textContent?.includes("74") && foot.textContent?.includes("4.20 kW"));
  assert.deepEqual([...foot.querySelectorAll<HTMLAnchorElement>("a")].map((link) => link.getAttribute("href")), ["/analytics/gpus?metric=temp", "/analytics/gpus?metric=power", "/scheduling"]);
  ok(!usage.textContent?.includes("Requests held for a wake"));
});

test("usage puts allocation, GPU and VRAM utilization first", async () => {
  stub(); renderPage();
  const usage = await waitFor(() => { const element = panel("Usage"); ok(element?.textContent?.includes("71")); return element; });
  const kpis = [...usage.querySelectorAll(".dashboard-overview-metric.is-kpi")].map((metric) => [metric.querySelector('[data-slot="metric-card-label"]')?.textContent, metric.querySelector('[data-slot="metric-card-value-row"]')?.textContent, metric.querySelector('[data-slot="metric-card-footer"]')?.textContent]);
  assert.deepEqual(kpis, [["Allocation", "71%", "17 / 24 cards"], ["GPU utilization", "61%", "Average of online cards"], ["VRAM utilization", "77%", "1,084 / 1,408 GiB"]]);
});

test("allocation shows the server's allocated count and does not add card states itself", async () => {
  stub();
  api.dashboardSummary = async () => ({ ...summary, cards: { ...cards, allocated: 12 } });
  renderPage();
  const usage = await waitFor(() => { const element = panel("Usage"); ok(element?.textContent?.includes("12 / 24 cards")); return element; });
  assert.equal(usage.querySelector('.dashboard-overview-metric.is-kpi [data-slot="metric-card-value-row"]')?.textContent, "50%");
});

test("missing samples and an unknown card summary read as unknown, not as zero", async () => {
  stub();
  api.dashboardSummary = async () => ({ ...summary, cards: null, current_metrics: { utilization_pct: null, temperature_c: null, power_watts: null, memory_used_bytes: null, vram_used_bytes: null, vram_total_bytes: null } });
  renderPage();
  const usage = await waitFor(() => { const element = panel("Usage"); ok(element); return element; });
  await waitFor(() => assert.deepEqual([...usage.querySelectorAll('.dashboard-overview-metric.is-kpi [data-slot="metric-card-value-row"]')].map((value) => value.textContent), ["—", "—", "—"]));
  assert.equal(usage.querySelectorAll(".dashboard-ring").length, 0);
  ok(usage.textContent?.includes("Some overview data is temporarily unavailable"));
  assert.deepEqual([...usage.querySelectorAll<HTMLAnchorElement>(".dashboard-usage-foot a")].map((link) => link.getAttribute("href")), []);
});

test("GPU models group by vendor with two models per row, and fold vendors beyond three", async () => {
  stub();
  api.gpus = async () => ({ items: [
    card("a", "L20", "nvidia", 48), card("b", "L20", "nvidia", 48), card("c", "RTX 8000", "nvidia", 48), card("d", "A100", "nvidia", 80),
    card("e", "Atlas 300I Duo", "ascend", 96), card("f", "K100 AI", "hygon", 64), card("g", "MI210", "amd", 64), card("h", "M4 Max", "apple", 32),
    { ...card("passthrough", "H100", "nvidia", 80), ownership: "vm" },
  ], overview: { total: 9, allocation: {} } }) as never;
  renderPage();
  const models = await waitFor(() => { const element = panel("Hardware"); ok(element?.querySelector(".dashboard-model-row")); return element.querySelector(".dashboard-subsection")!; });
  assert.equal(models.querySelector(".dashboard-subsection-tag")?.textContent, "5 vendors · 7 models");
  const rows = [...models.querySelectorAll(".dashboard-model-row")];
  assert.equal(rows.length, 3);
  assert.equal(rows[0].querySelector("img")?.getAttribute("alt"), "NVIDIA");
  assert.equal(rows[0].querySelector(".dashboard-model-name")?.textContent, "L20, A100+1 more", "the English list separator");
  assert.deepEqual([...rows[0].querySelectorAll(".dashboard-model-num")].map((cell) => cell.textContent), ["4 cards", "224 GiB"]);
  ok(rows[0].getAttribute("title")?.includes("RTX 8000"));
  ok(!models.textContent?.includes("H100"), "cards owned elsewhere are nobody's capacity");
  ok(rows[2].classList.contains("is-more"));
  ok(rows[2].textContent?.includes("3 more vendors"));
});

test("a single vendor shows its mark once, two model rows, and folds the rest", async () => {
  stub();
  api.gpus = async () => ({ items: [card("a", "L20", "nvidia", 48), card("b", "A100", "nvidia", 80), card("c", "A100", "nvidia", 80), card("d", "RTX 8000", "nvidia", 48)], overview: { total: 4, allocation: {} } }) as never;
  renderPage();
  const models = await waitFor(() => { const element = panel("Hardware")?.querySelector(".is-single"); ok(element); return element!; });
  ok(models.querySelector(".dashboard-models-single-head")?.textContent?.includes("All NVIDIA"));
  assert.equal(models.querySelectorAll("img").length, 1);
  assert.deepEqual([...models.querySelectorAll(".dashboard-model-row .dashboard-model-name")].map((name) => name.textContent), ["A100", "L20", "1 more model"]);
  assert.equal(panel("Hardware").querySelector(".dashboard-subsection-tag")?.textContent, "3 models");
});

test("deployments count sleeping apart and link pending placements to scheduling only when there are some", async () => {
  stub(); renderPage();
  const deployments = await waitFor(() => { const element = panel("Inference services"); ok(element?.textContent?.includes("12")); return element; });
  assert.deepEqual([...deployments.querySelectorAll(".dashboard-facts .dashboard-overview-status")].map((status) => status.textContent), ["Running 7", "Starting 1", "Asleep 2", "Other 1"]);
  assert.equal(arrow(deployments), "/deployments");
  const foot = deployments.querySelector(".dashboard-panel-foot")!;
  ok(foot.textContent?.includes("14/16"));
  assert.deepEqual([...foot.querySelectorAll<HTMLAnchorElement>("a")].map((link) => [link.textContent, link.getAttribute("href")]), [["Awaiting placement1", "/scheduling"]]);
  ok([...foot.querySelectorAll(".dashboard-figure.is-zero")].some((figure) => figure.textContent?.includes("Waiting to wake")));
});

test("tasks count batches and evaluations, a zero is not a link and unknown batches read as —", async () => {
  stub(); renderPage();
  const tasks = await waitFor(() => { const element = document.querySelector(".dashboard-tasks"); ok(element?.textContent?.includes("Batch jobs")); return element!; });
  const rows = [...tasks.querySelectorAll(".dashboard-task")];
  assert.deepEqual(rows.map((row) => [row.querySelector(".dashboard-task-count")?.textContent, row.querySelector(".dashboard-task-name")?.textContent, row.querySelector("a.dashboard-overview-module-arrow")?.getAttribute("href")]), [["2", "Batch jobs", "/batches"], ["0", "Evaluations", "/evaluations"]]);
  ok(rows[0].textContent?.includes("Validating1"));
  ok(rows[1].querySelector(".dashboard-task-count")?.classList.contains("is-zero"));
  assert.equal(rows[1].querySelectorAll("a").length, 1);
  cleanup();
  api.dashboardSummary = async () => ({ ...summary, tasks: { batches: null, benchmarks: 1 } });
  renderPage();
  await waitFor(() => assert.equal(document.querySelector(".dashboard-tasks .dashboard-task-count")?.textContent, "—"));
});

test("every panel arrow goes to a real page and both clouds link to the connected platforms", async () => {
  stub(); renderPage();
  await waitFor(() => ok(panel("Token usage")));
  ok(arrow(panel("Token usage"))?.startsWith("/analytics/usage"));
  assert.equal(arrow(panel("Inference quality")), "/monitor");
  assert.equal(arrow(panel("Shared storage used / total")), "/storage");
  assert.equal(arrow(panel("Resource connections")), undefined);
  const sources = await waitFor(() => { const links = [...panel("Resource connections").querySelectorAll<HTMLAnchorElement>(".dashboard-source")]; assert.equal(links.length, 2); return links; });
  assert.deepEqual(sources.map((link) => [link.querySelector(".dashboard-source-value")?.textContent, link.querySelector(".dashboard-source-label")?.textContent, link.getAttribute("href")]), [["0", "Public clouds", null], ["1", "Private clouds", "/providers?category=private_cloud"]]);
  ok(sources[0].querySelector(".dashboard-source-value")?.classList.contains("is-zero"));
});

test("attention stays below the page header and keeps direct links when more issues collapse", async () => {
  stub();
  api.nodesPage = async () => ({ items: [{ name: "host3", hostname: "host3" }], total: 1 }) as never;
  api.gpus = async () => ({ items: [{ uuid: "gpu-1", name: "H100", arch_family: "Hopper", vendor: "nvidia", allocation_state: "unusable" }], overview: { total: 1, allocation: { allocatable: 0, unusable: 1 } } }) as never;
  api.monitorOverview = async () => ({ services: [{ deployment_id: 7, name: "service7", state: "running", health: { status: "failed" } }] }) as never;
  renderPage();
  await waitFor(() => ok(screen.getByRole("button", { name: "1 more" })));
  const strip = document.querySelector(".dashboard-overview-attention");
  ok(strip);
  ok(strip.querySelector('[role="status"][data-status="warning"]'));
  // The shared page header's root is .page-header (page-header-wrap went with the old header).
  ok(document.querySelector(".page-header")!.compareDocumentPosition(strip) & dom.window.Node.DOCUMENT_POSITION_FOLLOWING);
  assert.deepEqual([...strip.querySelectorAll<HTMLAnchorElement>("a")].map((link) => link.getAttribute("href")), ["/nodes", "/gpus?gpu=gpu-1"]);
  assert.equal(document.querySelectorAll(".dashboard-overview-issues").length, 0);
  fireEvent.click(screen.getByRole("button", { name: "1 more" }));
  fireEvent.click(await screen.findByRole("menuitem", { name: /service7/ }));
  await waitFor(() => assert.equal(seenPath, "/monitor/d/7"));
});

test("an offline node's unknown GPU is visible in inventory state but not counted again as a confirmed GPU fault", async () => {
  stub();
  api.dashboardSummary = async () => ({ ...summary, nodes: { ...summary.nodes, total: 3, online: 2, offline: 1, unusable_gpus: 2 } });
  api.nodesPage = async () => ({ items: [{ name: "host3", hostname: "host3" }], total: 1 }) as never;
  api.gpus = async () => ({ items: [
    { uuid: "gpu-offline", name: "RTX 4090", node_state: "offline", allocation_state: "unknown" },
    { uuid: "gpu-online", name: "RTX 4090", node_state: "online", allocation_state: "partial" },
    { uuid: "gpu-fault", name: "RTX 3090", node_state: "online", allocation_state: "unusable" },
  ], overview: { total: 3, allocation: { allocatable: 1, unusable: 1, unknown: 1 } } }) as never;
  renderPage();
  await waitFor(() => assert.equal(document.querySelectorAll(".dashboard-overview-attention-items a").length, 2));
  const attention = document.querySelector(".dashboard-overview-attention")!;
  ok(attention.textContent?.includes("Needs attention 2"));
  ok(attention.textContent?.includes("host3") && attention.textContent?.includes("RTX 3090"));
  ok(!attention.textContent?.includes("RTX 4090"));
});

test("five issues retain object links while six switch to a single category count", async () => {
  stub();
  api.dashboardSummary = async () => ({ ...summary, nodes: { ...summary.nodes, total: 2, online: 2, offline: 0 } });
  api.gpus = async () => ({ items: Array.from({ length: 5 }, (_, index) => ({ uuid: `gpu-${index}`, name: `card-${index}`, vendor: "nvidia", memory_total_bytes: 24 * 1024 ** 3, allocation_state: "unusable" })), overview: { total: 5, allocation: { unusable: 5 } } }) as never;
  renderPage();
  await waitFor(() => ok(screen.getByRole("button", { name: "3 more" })));
  ok(document.querySelector(".dashboard-overview-attention")?.textContent?.includes("Needs attention 5"));
  ok(screen.getByRole("link", { name: /card-0/ }).getAttribute("href")?.includes("gpu-0"));
  cleanup();
  api.gpus = async () => ({ items: Array.from({ length: 6 }, (_, index) => ({ uuid: `gpu-${index}`, name: `card-${index}`, vendor: "nvidia", memory_total_bytes: 24 * 1024 ** 3, allocation_state: "unusable" })), overview: { total: 6, allocation: { unusable: 6 } } }) as never;
  renderPage();
  await waitFor(() => ok(screen.getByRole("link", { name: "Unavailable GPUs 6" })));
  ok(document.querySelector(".dashboard-overview-attention")?.textContent?.includes("Needs attention 1 category"));
  assert.equal(document.querySelectorAll(".dashboard-overview-attention-link").length, 1);
  ok(!document.querySelector(".dashboard-overview-attention")?.textContent?.includes("card-0"));
});

test("attention shows every item that fits and folds only the remainder after resizing", async () => {
  stub();
  api.dashboardSummary = async () => ({ ...summary, nodes: { ...summary.nodes, total: 5, online: 0, offline: 5 } });
  api.nodesPage = async () => ({ items: Array.from({ length: 5 }, (_, index) => ({ name: `node-${index + 1}`, hostname: `node-${index + 1}` })), total: 5 }) as never;
  let availableWidth = 900;
  const clientWidth = Object.getOwnPropertyDescriptor(HTMLElement.prototype, "clientWidth");
  const offsetWidth = Object.getOwnPropertyDescriptor(HTMLElement.prototype, "offsetWidth");
  Object.defineProperty(HTMLElement.prototype, "clientWidth", { configurable: true, get() { return this.classList.contains("dashboard-overview-attention-content") ? availableWidth : 0; } });
  Object.defineProperty(HTMLElement.prototype, "offsetWidth", { configurable: true, get() {
    if (this.classList.contains("dashboard-overview-attention-heading")) return 100;
    if (this.classList.contains("dashboard-overview-attention-measure-entry")) return 120;
    if (this.classList.contains("dashboard-overview-attention-measure-more")) return 70;
    return 0;
  } });
  try {
    renderPage();
    await waitFor(() => assert.equal(document.querySelectorAll(".dashboard-overview-attention-items a").length, 5));
    ok(!screen.queryByRole("button", { name: /more/ }));
    availableWidth = 450;
    fireEvent.resize(window);
    await waitFor(() => assert.equal(document.querySelectorAll(".dashboard-overview-attention-items a").length, 2));
    ok(screen.getByRole("button", { name: "3 more" }));
    availableWidth = 580;
    fireEvent.resize(window);
    await waitFor(() => assert.equal(document.querySelectorAll(".dashboard-overview-attention-items a").length, 3));
    ok(screen.getByRole("button", { name: "2 more" }));
  } finally {
    if (clientWidth) Object.defineProperty(HTMLElement.prototype, "clientWidth", clientWidth);
    else delete (HTMLElement.prototype as unknown as { clientWidth?: number }).clientWidth;
    if (offsetWidth) Object.defineProperty(HTMLElement.prototype, "offsetWidth", offsetWidth);
    else delete (HTMLElement.prototype as unknown as { offsetWidth?: number }).offsetWidth;
  }
});

test("incomplete model pagination marks the known issue count as a lower bound", async () => {
  stub();
  api.dashboardSummary = async () => ({ ...summary, nodes: { ...summary.nodes, total: 2, online: 2, offline: 0 } });
  api.modelsPage = async ({ page }: { page: number }) => ({
    items: page === 1 ? [{ id: 1, name: "failed model", presence: { failed: 1 } }] : [],
    total: 101,
  }) as never;
  renderPage();
  await waitFor(() => ok(screen.getByRole("link", { name: "Model file issues: at least 1" })));
  ok(document.querySelector(".dashboard-overview-attention")?.textContent?.includes("Needs attention 1 category"));
  ok(screen.getAllByText("Some overview data is temporarily unavailable").some(el => el.closest('[role="status"]')));
  ok(!document.querySelector(".dashboard-overview-attention")?.textContent?.includes("failed model"));
});

test("hundreds of issues across every category remain a seven-category strip with accurate node count", async () => {
  stub();
  api.dashboardSummary = async () => ({ ...summary, nodes: { ...summary.nodes, total: 1000, online: 590, connecting: 10, offline: 400 } });
  api.nodesPage = async () => ({ items: Array.from({ length: 20 }, (_, index) => ({ name: `offline-${index}`, hostname: `offline-${index}` })), total: 400 }) as never;
  api.gpus = async () => ({ items: Array.from({ length: 80 }, (_, index) => ({ uuid: `gpu-${index}`, name: `faulty-${index}`, vendor: "nvidia", memory_total_bytes: 24 * 1024 ** 3, allocation_state: "unusable" })), overview: { total: 80, allocation: { unusable: 80 } } }) as never;
  api.monitorOverview = async () => ({ services: [
    ...Array.from({ length: 60 }, (_, index) => ({ deployment_id: index + 1, name: `service-${index}`, state: "running", health: { status: "failed" } })),
    { deployment_id: 61, name: "stopped", state: "stopped", health: { status: "failed" } },
  ] }) as never;
  api.modelsPage = async () => ({ items: Array.from({ length: 70 }, (_, index) => ({ id: index + 1, name: `model-${index}`, presence: { failed: 1 } })), total: 70 }) as never;
  api.storages = async () => ({ items: Array.from({ length: 40 }, (_, index) => ({ id: index + 1, name: `storage-${index}`, type: index % 2 ? "nfs" : "local", nodes: { problems: [{ state: "failed" }] } })), total: 40 }) as never;
  api.storage = async (id: number) => ({ id, name: `storage-${id - 1}`, type: "nfs", nodes: { all: [], problems: [{ state: "failed" }] } }) as never;
  api.providers = async () => ({ items: [
    ...Array.from({ length: 20 }, (_, index) => ({ id: index + 1, name: `public-${index}`, kind: "runpod", category: "public_cloud", state: "invalid" })),
    ...Array.from({ length: 20 }, (_, index) => ({ id: index + 21, name: `private-${index}`, kind: "zstack", category: "private_cloud", state: "invalid" })),
  ], total: 40 }) as never;
  renderPage();
  await waitFor(() => ok(screen.getByRole("button", { name: "5 more categories" })));
  const strip = document.querySelector(".dashboard-overview-attention")!;
  ok(strip.textContent?.includes("Needs attention 7 categories"));
  assert.deepEqual([...strip.querySelectorAll("a")].map((link) => [link.textContent, link.getAttribute("href")]), [["Offline nodes 400", "/nodes"], ["Unavailable GPUs 80", "/gpus"]]);
  ok(!strip.textContent?.includes("offline-0") && !strip.textContent?.includes("690"));
  fireEvent.click(screen.getByRole("button", { name: "5 more categories" }));
  const labels = ["Inference service issues 60", "Model file issues 70", "Storage issues 40", "Public cloud issues 20", "Private cloud issues 20"];
  await waitFor(() => assert.deepEqual(screen.getAllByRole("menuitem").map(item => item.textContent), labels));
  for (const [index, to] of ["/monitor", "/models", "/storage", "/providers?category=public_cloud", "/providers?category=private_cloud"].entries()) {
    if (index) fireEvent.click(screen.getByRole("button", { name: "5 more categories" }));
    fireEvent.click(await screen.findByRole("menuitem", { name: labels[index] }));
    await waitFor(() => assert.equal(seenPath, to));
  }
});

test("default local storage stays in Storage and an overview with no shared storage has an honest empty state", async () => {
  stub();
  api.storages = async () => ({ items: [{ id: 1, name: "default", type: "local" }], total: 1 }) as never;
  let fetchedDetails = 0;
  api.storage = async () => { fetchedDetails++; throw new Error("local detail must stay in Storage"); };
  renderPage();
  await waitFor(() => ok(screen.getByText("No shared storage")));
  assert.equal(fetchedDetails, 0);
  assert.equal(document.querySelectorAll(".dashboard-overview-storage-row").length, 0);
  ok(screen.getByText("View local storage in Storage"));
  ok(!document.querySelector(".dashboard-overview")?.textContent?.includes("Model cache"));
});

test("shared storage uses storage-level capacity once and leaves node cache details in Storage", async () => {
  stub();
  api.storages = async () => ({ items: [{ id: 2, name: "shared", type: "nfs_cache" }], total: 1 }) as never;
  api.storage = async () => ({ id: 2, name: "shared", type: "nfs_cache", total_bytes: 110 * 1024 ** 3, available_bytes: 40 * 1024 ** 3, nodes: { all: [
    { node_uuid: "a", node_name: "a", total_bytes: 100 * 1024 ** 3, available_bytes: 50 * 1024 ** 3, cache: { used_bytes: 1024 ** 3, reclaimable_bytes: 0 } },
    { node_uuid: "b", node_name: "b", total_bytes: 100 * 1024 ** 3, available_bytes: 50 * 1024 ** 3, cache: { used_bytes: 2 * 1024 ** 3, reclaimable_bytes: 0 } },
  ], problems: [{ node_uuid: "a", state: "offline" }] } }) as never;
  renderPage();
  await waitFor(() => ok(screen.getByText("Used 70.0 GiB / total 110.0 GiB")));
  assert.equal(document.querySelectorAll(".dashboard-overview-storage-row").length, 1);
  ok(!document.querySelector(".dashboard-overview-storage-row")?.textContent?.includes("nodes with issues"));
  ok(!document.querySelector(".dashboard-overview")?.textContent?.includes("Model cache"));
  ok(!document.querySelector(".dashboard-overview")?.textContent?.includes("3.0 GiB"));
});

test("multiple shared storages stay in a compact two-row page with the full count visible", async () => {
  stub();
  api.storages = async () => ({ items: [1, 2, 3].map((id) => ({ id, name: `shared-${id}`, type: "nfs_cache" })), total: 3 }) as never;
  api.storage = async (id: number) => ({ id, name: `shared-${id}`, type: "nfs_cache", total_bytes: 100 * 1024 ** 3, available_bytes: id * 10 * 1024 ** 3, nodes: { all: [], problems: [] } }) as never;
  renderPage();
  const capacity = document.querySelector(".dashboard-storage")!;
  await waitFor(() => assert.equal(capacity.querySelectorAll(".dashboard-overview-storage-row").length, 2));
  ok(capacity.textContent?.includes("3 shared storages"));
  assert.deepEqual([...capacity.querySelectorAll(".dashboard-overview-storage-row a")].map((link) => link.textContent), ["shared-1", "shared-2"]);
  assert.equal(capacity.querySelectorAll(".dashboard-overview-storage-progress").length, 2);
  ok([...capacity.querySelectorAll(".dashboard-overview-storage-row")].every((row) => row.lastElementChild?.classList.contains("dashboard-overview-storage-progress")));
  fireEvent.click(screen.getByRole("button", { name: "Next page" }));
  await waitFor(() => assert.deepEqual([...capacity.querySelectorAll(".dashboard-overview-storage-row a")].map((link) => link.textContent), ["shared-3"]));
  ok(capacity.textContent?.includes("Used 70.0 GiB / total 100.0 GiB"));
});

test("one failed storage detail keeps the other storage's capacity visible and warns about incomplete data", async () => {
  stub();
  api.storages = async () => ({ items: [
    { id: 1, name: "broken", type: "nfs" },
    { id: 2, name: "healthy", type: "nfs_cache" },
  ], total: 2 }) as never;
  api.storage = async (id: number) => {
    if (id === 1) throw new Error("detail unavailable");
    return { id, name: "healthy", type: "nfs_cache", total_bytes: 100 * 1024 ** 3, available_bytes: 40 * 1024 ** 3,
      nodes: { all: [{ node_uuid: "a", node_name: "a", cache: { used_bytes: 2 * 1024 ** 3, reclaimable_bytes: 1024 ** 3 } }], problems: [] } } as never;
  };
  renderPage();
  await waitFor(() => ok(screen.getByText("Used 60.0 GiB / total 100.0 GiB")));
  assert.equal(document.querySelectorAll(".dashboard-overview-storage-row").length, 1);
  ok(screen.getAllByText("Some overview data is temporarily unavailable").some(el => el.closest('[role="status"]')));
});

test("unavailable shared storage details do not claim no shared storage is configured", async () => {
  stub();
  api.storages = async () => ({ items: [{ id: 1, name: "inaccessible", type: "nfs_cache" }], total: 1 }) as never;
  api.storage = async () => { throw new Error("detail unavailable"); };
  renderPage();
  await waitFor(() => ok(screen.getAllByText("Some overview data is temporarily unavailable").some(el => el.closest('[role="status"]'))));
  const panel = document.querySelector(".dashboard-storage > .dashboard-panel");
  ok(panel?.textContent?.includes("Some overview data is temporarily unavailable"));
  ok(!panel?.textContent?.includes("No shared storage"));
});

test("stopped services do not become attention items", async () => {
  stub();
  api.dashboardSummary = async () => ({ ...summary, nodes: { ...summary.nodes, total: 2, online: 2, offline: 0 } });
  api.monitorOverview = async () => ({ services: [{ deployment_id: 2, name: "manually stopped", state: "stopped", health: { status: "failed" } }] }) as never;
  renderPage();
  await waitFor(() => ok(screen.getByText("H100")));
  assert.equal(document.querySelectorAll(".dashboard-overview-attention").length, 0);
});

for (const scenario of [
  { name: "one node and one GPU", nodes: 1, gpus: 1, vram: 80 },
  { name: "nodes without GPUs", nodes: 2, gpus: 0, vram: 0 },
  { name: "an empty platform", nodes: 0, gpus: 0, vram: 0 },
]) {
  test(`overview handles ${scenario.name} without linking zero counts`, async () => {
    stub();
    api.dashboardSummary = async () => ({ ...summary,
      nodes: { ...summary.nodes, total: scenario.nodes, online: scenario.nodes, offline: 0, connecting: 0, gpus: scenario.gpus, vram_gib: scenario.vram },
      cards: { ...cards, cards: scenario.gpus, awake: 0, asleep: 0, allocated: 0, free: scenario.gpus, held_outside: 0, unhealthy: 0, offline: 0 },
      current_metrics: { ...summary.current_metrics!, utilization_pct: 0, temperature_c: 0, power_watts: 0 },
      deployments: { ...summary.deployments, total: 0, running: 0, warming: 0, sleeping: 0, pending: 0, other: 0 },
      tasks: { batches: { active: 0, waiting: 0 }, benchmarks: 0 },
    });
    api.gpus = async () => ({ items: scenario.gpus ? [card("only", "H100", "nvidia", 80)] : [], overview: { total: scenario.gpus, allocation: { unusable: 0 } } }) as never;
    api.providers = async () => ({ items: [], total: 0 });
    renderPage();
    await waitFor(() => ok(panel("Hardware")?.textContent?.includes(`${scenario.nodes}nodes`)));
    assert.equal(panel("Hardware").querySelectorAll('[data-slot="metric-card-value-row"] a').length, scenario.gpus ? 3 : scenario.nodes ? 1 : 0);
    assert.equal(panel("Resource connections").querySelectorAll("a").length, 0);
    assert.equal(document.querySelectorAll('a[href="/analytics/gpus?metric=temp"], a[href="/analytics/gpus?metric=power"]').length, 0);
    if (!scenario.gpus) ok(panel("Hardware").textContent?.includes(dashboard.overview.noGpu));
    assert.deepEqual([...document.querySelectorAll(".dashboard-services > .dashboard-panel .dashboard-panel-head h3")].map(el => el.textContent), ["Inference services", "Token usage", "Tasks", "Inference quality"]);
  });
}

test("the usage panel reads the 24-hour usage once and opens usage analytics on that window", async () => {
  stub();
  const original = analyticsApi.usage;
  let calls = 0;
  analyticsApi.usage = async (...args) => { calls++; return original(...args); };
  renderPage();
  await waitFor(() => assert.equal(arrow(panel("Token usage")), "/analytics/usage?range=custom&from=1000&to=2000&gran=hour", "usage analytics opens on the window the panel shows"));
  assert.equal(calls, 1);
});

test("storage pages: Previous is off on the first page, Next is off on the last, and Previous goes back", async () => {
  stub();
  api.storages = async () => ({ items: [1, 2, 3, 4, 5].map((id) => ({ id, name: `shared-${id}`, type: "nfs_cache" })), total: 5 }) as never;
  api.storage = async (id: number) => ({ id, name: `shared-${id}`, type: "nfs_cache", total_bytes: 100 * 1024 ** 3, available_bytes: 50 * 1024 ** 3, nodes: { all: [], problems: [] } }) as never;
  renderPage();
  const capacity = document.querySelector(".dashboard-storage")!;
  const names = () => [...capacity.querySelectorAll(".dashboard-overview-storage-row a")].map((link) => link.textContent);
  await waitFor(() => assert.deepEqual(names(), ["shared-1", "shared-2"]));
  const pages = screen.getByRole("navigation", { name: "Shared storage used / total" });
  const previous = screen.getByRole("button", { name: "Previous page" }) as HTMLButtonElement;
  const next = screen.getByRole("button", { name: "Next page" }) as HTMLButtonElement;
  assert.equal(previous.disabled, true);
  ok(pages.textContent?.includes("1 / 3"));
  fireEvent.click(next);
  await waitFor(() => assert.deepEqual(names(), ["shared-3", "shared-4"]));
  assert.equal(previous.disabled, false);
  fireEvent.click(next);
  await waitFor(() => assert.deepEqual(names(), ["shared-5"]));
  ok(pages.textContent?.includes("3 / 3"));
  assert.equal(next.disabled, true, "the last page has no next");
  fireEvent.click(previous);
  await waitFor(() => assert.deepEqual(names(), ["shared-3", "shared-4"]));
  ok(pages.textContent?.includes("2 / 3"));
});

test("two shared storages fit one page and show no page controls", async () => {
  stub();
  api.storages = async () => ({ items: [1, 2].map((id) => ({ id, name: `shared-${id}`, type: "nfs_cache" })), total: 2 }) as never;
  api.storage = async (id: number) => ({ id, name: `shared-${id}`, type: "nfs_cache", total_bytes: 100 * 1024 ** 3, available_bytes: 50 * 1024 ** 3, nodes: { all: [], problems: [] } }) as never;
  renderPage();
  await waitFor(() => assert.equal(document.querySelectorAll(".dashboard-overview-storage-row").length, 2));
  assert.equal(screen.queryByRole("button", { name: "Next page" }), null);
  assert.equal(screen.queryByRole("navigation", { name: "Shared storage used / total" }), null);
});

test("three vendors are all listed without a fold; a vendor without a logo is named in text", async () => {
  stub();
  api.gpus = async () => ({ items: [
    card("a", "MI300X", "amd", 192), card("b", "P800", "kunlunxin", 96), card("c", "", "metax", 64),
  ], overview: { total: 3, allocation: {} } }) as never;
  renderPage();
  const models = await waitFor(() => { const element = panel("Hardware")?.querySelector(".dashboard-models"); ok(element?.querySelector(".dashboard-model-row")); return element!; });
  const rows = [...models.querySelectorAll(".dashboard-model-row")];
  assert.equal(rows.length, 3);
  ok(!rows.some((row) => row.classList.contains("is-more")), "no fold at three vendors");
  assert.deepEqual(rows.map((row) => row.querySelector("img")?.getAttribute("alt") ?? row.querySelector(".dashboard-vendor-mark")?.textContent), ["AMD", "Kunlunxin", "Unknown Vendor"]);
  assert.equal(rows[2].querySelector("img"), null, "an unknown vendor has no logo, only its name");
  assert.equal(rows[2].querySelector(".dashboard-model-name")?.textContent, "Unidentified", "a card without a name is grouped as unidentified");
});

test("four vendors fold the last two into one counted row", async () => {
  stub();
  api.gpus = async () => ({ items: [
    card("a", "H100", "nvidia", 80), card("b", "910B", "ascend", 64), card("c", "PPU", "ppu", 96), card("d", "M3", "apple", 16),
  ], overview: { total: 4, allocation: {} } }) as never;
  renderPage();
  const models = await waitFor(() => { const element = panel("Hardware")?.querySelector(".dashboard-models"); ok(element?.querySelector(".is-more")); return element!; });
  const rows = [...models.querySelectorAll(".dashboard-model-row")];
  assert.equal(rows.length, 3);
  assert.equal(rows[0].querySelector("img")?.getAttribute("alt"), "T-Head", "T-Head cards use the T-Head mark");
  assert.equal(rows[0].querySelector("img")?.getAttribute("src"), "/vendor-logos/thead.png");
  assert.equal(rows[2].querySelector(".dashboard-vendor-mark")?.textContent, "2 more vendors");
  assert.equal(rows[2].querySelector(".dashboard-model-name")?.textContent, "Ascend, Apple");
  assert.deepEqual([...rows[2].querySelectorAll(".dashboard-model-num")].map((cell) => cell.textContent), ["2 cards", "80 GiB"]);
});

test("a single AMD fleet reads All AMD and lists every model when there are no more than two", async () => {
  stub();
  api.gpus = async () => ({ items: [card("a", "MI300X", "amd", 192), card("b", "MI210", "amd", 64)], overview: { total: 2, allocation: {} } }) as never;
  renderPage();
  const models = await waitFor(() => { const element = panel("Hardware")?.querySelector(".is-single"); ok(element); return element!; });
  ok(models.querySelector(".dashboard-models-single-head")?.textContent?.includes("All AMD"));
  assert.deepEqual([...models.querySelectorAll(".dashboard-model-row .dashboard-model-name")].map((name) => name.textContent), ["MI300X", "MI210"]);
  assert.equal(models.querySelector(".is-more"), null);
});
