import assert from "node:assert/strict";
import fs from "node:fs";
import { after, afterEach, test } from "node:test";
import { JSDOM } from "jsdom";
import React from "react";
import { createInstance } from "i18next";
import { I18nextProvider, initReactI18next } from "react-i18next";
import { trackMessageChannels } from "./helpers/trackedMessageChannels.ts";

const closeChannels = trackMessageChannels();
const dom = new JSDOM("<!doctype html><html><body></body></html>", { url: "http://localhost/" });
for (const name of ["window", "document", "navigator", "HTMLElement", "HTMLBodyElement", "HTMLHtmlElement", "SVGElement", "Element", "Node", "MutationObserver", "ShadowRoot", "AbortController", "AbortSignal"] as const) {
  Object.defineProperty(globalThis, name, { value: dom.window[name], configurable: true });
}
Object.defineProperty(globalThis, "getComputedStyle", { value: (element: Element) => dom.window.getComputedStyle(element), configurable: true });
Object.defineProperty(dom.window, "matchMedia", { configurable: true, value: () => ({ matches: false, addListener() {}, removeListener() {}, addEventListener() {}, removeEventListener() {}, dispatchEvent: () => false }) });
Object.defineProperty(dom.window.Element.prototype, "getAnimations", { configurable: true, value: () => [] });
class ResizeObserverStub { observe() {} unobserve() {} disconnect() {} }
Object.defineProperty(globalThis, "ResizeObserver", { value: ResizeObserverStub, configurable: true });

const load = (ns: string) => JSON.parse(fs.readFileSync(new URL(`../src/i18n/locales/en/${ns}.json`, import.meta.url), "utf8"));
const i18n = createInstance();
await i18n.use(initReactI18next).init({
  lng: "en",
  fallbackLng: "en",
  resources: { en: { networks: load("networks"), common: load("common") } },
  defaultNS: "common",
  ns: ["networks", "common"],
  interpolation: { escapeValue: false },
});

const { cleanup, render, screen } = await import("@testing-library/react");
const { default: NetworkTestResult, failureText } = await import("../src/pages/networks/NetworkTestResult.tsx");
const { formatTime } = await import("../src/i18n/datetime.ts");
const detailFixture = JSON.parse(fs.readFileSync(new URL("./fixtures/network-detail.json", import.meta.url), "utf8"));

after(() => { closeChannels(); dom.window.close(); });
afterEach(() => cleanup());

type Scope = { scope: "all" | "nodes" | "label"; label_key?: string; label_value?: string };

// A finished run as the server returns it: label_key and label_value only on a label run.
function finishedRun(kind: string, pattern: string, scope: Scope) {
  return {
    id: 7, status: "done", kind, pattern, ...scope, bandwidth: false, node_count: 2, pair_count: 0, passed_count: 0, partial_count: 0,
    rail_count: 0, rail_passed_count: 0, node_uuids: ["a", "b"], pairs: [], started_at: "2026-09-30T04:00:00Z", finished_at: "2026-09-30T04:00:10Z",
  } as import("../src/networkTypes.ts").NetworkTestRun;
}

const renderIn = (element: React.ReactElement) => render(<I18nextProvider i18n={i18n}>{element}</I18nextProvider>);

/** The card title that carries the run's summary, found by the scope it names. */
function summaryTitle(scopeText: RegExp) {
  return screen.getByText(scopeText).textContent ?? "";
}

const scopes: { scope: Scope; shown: RegExp }[] = [
  { scope: { scope: "label", label_key: "rack", label_value: "r2" }, shown: /· label rack=r2 ·/ },
  { scope: { scope: "nodes" }, shown: /· selected nodes ·/ },
  { scope: { scope: "all" }, shown: /· all nodes ·/ },
];

test("a network test result names the label it ran on, and nothing else for other scopes", () => {
  for (const { scope, shown } of scopes) {
    const view = renderIn(<NetworkTestResult network={detailFixture} run={finishedRun("tcp", "mesh", scope)} onChanged={() => {}} />);
    const title = summaryTitle(shown);
    assert.match(title, /^TCP/);
    assert.doesNotMatch(title, /undefined/);
    if (scope.scope !== "label") assert.doesNotMatch(title, /=/);
    view.unmount();
  }
});

test("a card test result names the label it ran on, and nothing else for other scopes", () => {
  for (const { scope, shown } of scopes) {
    const view = renderIn(<NetworkTestResult network={detailFixture} run={finishedRun("card_ping", "ring", scope)} onChanged={() => {}} />);
    const title = summaryTitle(shown);
    assert.doesNotMatch(title, /undefined/);
    if (scope.scope !== "label") assert.doesNotMatch(title, /=/);
    view.unmount();
  }
});

test("latest test keeps its absolute start time and planned scope for every run state", () => {
  for (const status of ["running", "interrupted", "done"] as const) {
    const run = { ...finishedRun("tcp", "mesh", { scope: "all" }), status, pair_count: 2 };
    const view = renderIn(<NetworkTestResult network={detailFixture} run={run} onChanged={() => {}} />);
    const time = view.container.querySelector("time");
    assert.equal(time?.getAttribute("datetime"), run.started_at);
    assert.equal(time?.textContent, formatTime(run.started_at));
    assert.ok(screen.getByText("Directed pairs"));
    assert.equal(!!screen.queryByText("Failed", { selector: '[data-slot="metric-card-label"]' }), status === "done");
    view.unmount();
  }
});

test("overview tones distinguish active outcomes while zero counts stay neutral", () => {
  const run = { ...detailFixture.latest_test, passed_count: 1, partial_count: 1,
    pairs: [...detailFixture.latest_test.pairs, detailFixture.latest_test.pairs[0]], pair_count: 3 };
  const view = renderIn(<NetworkTestResult network={detailFixture} run={run} onChanged={() => {}} />);
  const tone = (label: string) => screen.getByText(label, { selector: '[data-slot="metric-card-label"]' }).closest('[data-slot="metric-card"]')?.getAttribute("data-tone");
  assert.equal(tone("Nodes tested"), "default");
  assert.equal(tone("Passed"), "positive");
  assert.equal(tone("Partial"), "warning");
  assert.equal(tone("Failed"), "critical");
  view.unmount();
  renderIn(<NetworkTestResult network={detailFixture} run={finishedRun("tcp", "mesh", { scope: "all" })} onChanged={() => {}} />);
  for (const label of ["Passed", "Partial", "Failed"]) assert.equal(tone(label), "default");
});

test("mesh filters combine the two node names with any selected test statuses, and issues open their pairs", async () => {
  const { fireEvent, within } = await import("@testing-library/react");
  const testPair = (source: string, target: string, status: string) => ({
    source_uuid: source, source_name: source, target_uuid: target, target_name: target,
    status, reachable: status !== "failed", passed: status === "passed",
    gbps: status === "failed" ? 0 : 180, avg_rail_gbps: status === "partial" ? 90 : undefined, rtt_ms_p50: 0, rtt_ms_p99: 0,
    ...(status === "failed" ? { code: "offline", side: "target" } : {}),
    rails: status === "partial" ? [
      { rail: "rail-a", gbps: 80 }, { rail: "rail-b", gbps: 100 },
      { rail: "rail-c", gbps: 0, code: "probe_failed", side: "target", target_rail: "port-c" },
    ] : [],
  });
  const pairs = [testPair("gpu-a", "gpu-b", "passed"), testPair("gpu-a", "gpu-c", "partial"),
    testPair("gpu-b", "gpu-c", "partial"), testPair("gpu-b", "gpu-a", "failed")];
  const run = { ...detailFixture.latest_test, id: 10, kind: "rdma_tool", pattern: "mesh", bandwidth: true, pairs,
    pair_count: 4, passed_count: 1, partial_count: 2, node_count: 3, rail_count: 6, rail_passed_count: 4 };
  renderIn(<NetworkTestResult network={detailFixture} run={run} onChanged={() => {}} />);
  const matrix = () => screen.getByRole("table", { name: "Global matrix" });
  const cells = () => within(matrix()).getAllByRole("button");
  assert.ok(screen.getByText("4 / 6"));
  assert.equal(cells().length, 4);
  // A historical pair can have only the aggregate. Keep it in the passing
  // filter without presenting the aggregate as a per-rail measurement.
  assert.ok(within(matrix()).getByRole("button", { name: "gpu-a → gpu-b: —" }));
  fireEvent.click(screen.getByRole("checkbox", { name: "Failed", exact: true }));
  assert.equal(cells().length, 3);
  fireEvent.click(screen.getByRole("checkbox", { name: "All passed" }));
  assert.equal(cells().length, 2);
  const source = screen.getByRole("searchbox", { name: "Filter source nodes" });
  fireEvent.change(source, { target: { value: "gpu-a" } });
  assert.equal(cells().length, 1);
  assert.equal(within(matrix()).getAllByRole("rowheader").length, 1);
  assert.equal(within(matrix()).getAllByRole("columnheader").length, 2);
  const target = screen.getByRole("searchbox", { name: "Filter target nodes" });
  fireEvent.change(target, { target: { value: "GPU-C" } });
  assert.match(cells()[0].getAttribute("aria-label") ?? "", /gpu-a → gpu-c: 90.0 Gb\/s · 2\/3 rails passed/);
  fireEvent.change(source, { target: { value: "absent" } });
  assert.ok(screen.getByText("No matching pairs. Adjust the node names or status filters."));
  fireEvent.click(screen.getByRole("tab", { name: /Issue summary.*2/ }));
  const issue = screen.getByText("port-c").closest("tr")!;
  assert.ok(within(issue).getByText("2 pairs"));
  assert.ok(screen.getByText("1 pair"));
  fireEvent.click(within(issue).getByRole("button", { name: /expand row/i }));
  const failingPair = await screen.findByRole("button", { name: /gpu-a → gpu-c/ });
  fireEvent.click(failingPair);
  const details = await screen.findByRole("dialog");
  assert.ok(within(details).getByText("90.0 Gb/s"));
  for (const rate of ["80.0 Gb/s", "100.0 Gb/s"]) assert.ok(within(details).getByText(rate));
  const failedRail = within(details).getByText("rail-c").closest("tr")!;
  assert.ok(within(failedRail).getByText("Failed"));
  assert.equal(within(details).queryByText("180.0 Gb/s"), null);
});

test("a large mesh keeps both axes accessible without mounting every pair", async () => {
  const { fireEvent, within } = await import("@testing-library/react");
  const NetworkPairMatrix = (await import("../src/pages/networks/NetworkPairMatrix.tsx")).default;
  const names = Array.from({ length: 64 }, (_, i) => `gpu-${String(i + 1).padStart(2, "0")}`);
  const pairs = names.flatMap(source => names.filter(target => target !== source).map(target => ({
    source_uuid: source, source_name: source, target_uuid: target, target_name: target, status: "passed",
  })));
  renderIn(<NetworkPairMatrix run={{ ...detailFixture.latest_test, pairs }} renderPair={p => <button>{p.source_name} → {p.target_name}</button>} />);
  const table = screen.getByRole("table", { name: "Global matrix" });
  const viewport = table.closest('[data-slot="scroll-area-viewport"]') as HTMLElement;
  Object.defineProperties(viewport, { clientWidth: { value: 640 }, clientHeight: { value: 440 } });
  assert.equal(table.getAttribute("aria-rowcount"), "65");
  assert.equal(table.getAttribute("aria-colcount"), "65");
  assert.ok(screen.getByText("4032 matching pairs"));
  const active = () => within(table).getAllByRole("button");
  assert.ok(active().length < 200);
  viewport.scrollLeft = 64 * 108 + 124 - 640;
  viewport.scrollTop = 64 * 44 + 32 - 440;
  fireEvent.scroll(viewport);
  assert.ok(active().length < 200);
  assert.ok(within(table).getByRole("rowheader", { name: "gpu-64" }));
  assert.ok(within(table).getByRole("columnheader", { name: "gpu-64" }));
  assert.equal(within(table).queryByRole("rowheader", { name: "gpu-01" }), null);
  assert.ok(within(table).getByRole("button", { name: "gpu-63 → gpu-64" }));
});

test("known failure reasons only introduce technical details when present", async () => {
  for (const [locale, reason, withDetail] of [
    ["en", "Connection failed", "Connection failed: timeout"],
    ["zh", "连接失败", "连接失败：timeout"],
  ] as const) {
    const ns = JSON.parse(fs.readFileSync(new URL(`../src/i18n/locales/${locale}/networks.json`, import.meta.url), "utf8"));
    const translator = createInstance();
    await translator.init({ lng: locale, resources: { [locale]: { networks: ns } }, defaultNS: "networks" });
    const t = translator.getFixedT(locale, "networks");
    assert.equal(failureText(t, { code: "probe_failed" }), reason);
    assert.equal(failureText(t, { code: "probe_failed", error: "timeout" }), withDetail);
  }
});
