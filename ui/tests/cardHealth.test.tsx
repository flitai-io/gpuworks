import assert from "node:assert/strict";
import fs from "node:fs";
import { after, afterEach, test } from "node:test";
import { trackMessageChannels } from "./helpers/trackedMessageChannels.ts";
import { JSDOM } from "jsdom";
import React from "react";
import { createInstance } from "i18next";
import { I18nextProvider, initReactI18next } from "react-i18next";

// A node's health checks: the table of cards by checks the server lays out,
// what a check last did when a cell is chosen, and the failure records below,
// narrowed to the chosen cell.

const closeChannels = trackMessageChannels();
const dom = new JSDOM("<!doctype html><html><body></body></html>", { url: "http://localhost/" });
for (const name of ["window", "document", "navigator", "HTMLElement", "SVGElement", "Element", "Node", "MutationObserver", "ShadowRoot", "File", "Blob", "HTMLBodyElement"] as const) {
  Object.defineProperty(globalThis, name, { value: dom.window[name], configurable: true });
}
Object.defineProperty(globalThis, "getComputedStyle", { value: (element: Element) => dom.window.getComputedStyle(element), configurable: true });
Object.defineProperty(dom.window, "matchMedia", { configurable: true, value: () => ({ matches: false, addListener() {}, removeListener() {}, addEventListener() {}, removeEventListener() {}, dispatchEvent: () => false }) });
class ResizeObserverStub { observe() {} unobserve() {} disconnect() {} }
Object.defineProperty(globalThis, "ResizeObserver", { value: ResizeObserverStub, configurable: true });

const read = (ns: string) => JSON.parse(fs.readFileSync(new URL(`../src/i18n/locales/en/${ns}.json`, import.meta.url), "utf8"));
const i18n = createInstance();
await i18n.use(initReactI18next).init({ lng: "en", resources: { en: { nodes: read("nodes"), common: read("common") } }, interpolation: { escapeValue: false } });
const ch = i18n.getResourceBundle("en", "nodes").cardHealth;

const { cleanup, render, screen, waitFor } = await import("@testing-library/react");
const userEvent = (await import("@testing-library/user-event")).default;
const { api } = await import("../src/api.ts");
const { APIError } = await import("../src/apiError.ts");
const NodeHealthTab = (await import("../src/components/cardHealth/NodeHealthTab.tsx")).default;
const CardHealthTab = (await import("../src/components/cardHealth/CardHealthTab.tsx")).default;

const apiAny = api as unknown as Record<string, unknown>;
const saved = { nodeHealthChecks: apiAny.nodeHealthChecks, nodeHealthRecords: apiAny.nodeHealthRecords, cardCheckDetail: apiAny.cardCheckDetail, recheckCard: apiAny.recheckCard };
afterEach(() => { cleanup(); Object.assign(apiAny, saved); });
after(() => { closeChannels(); dom.window.close(); });

const now = new Date().toISOString();
const table = (extra: object = {}) => ({
  node_uuid: "n-1", online: true, reports: true, stale: false, received_at: now,
  checks: [{ id: "vendor_health", period_seconds: 60 }, { id: "pcie_link", period_seconds: 60 }, { id: "runtime", period_seconds: 900 }],
  cards: [
    { ident: "uuid:GPU-a", uuid: "GPU-a", pcie_bdf: "0000:3f:00.0", name: "H20", index: 0, worst: "pass", can_recheck: true,
      cells: [
        { check: "vendor_health", state: "pass", checked_at: now, stale: false },
        { check: "pcie_link", state: "pass", checked_at: now, stale: false },
        { check: "runtime", state: "skip", reason: "busy", checked_at: now, stale: false },
      ] },
    { ident: "uuid:GPU-b", uuid: "GPU-b", pcie_bdf: "0000:40:00.0", name: "H20", index: 1, worst: "warn", can_recheck: true,
      cells: [
        { check: "vendor_health", state: "pass", checked_at: now, stale: false },
        { check: "pcie_link", state: "warn", reason: "pcie_link_degraded", checked_at: now, stale: false },
        { check: "runtime", state: "fail", reason: "runtime_unusable", checked_at: now, duration_ms: 812, stale: false },
      ] },
  ],
  summary: { pass: 3, warn: 1, fail: 1, skip: 1 },
  ...extra,
});
const records = (list: object[] = []) => ({ records: list, total: list.length, page: 1, page_size: 50, retention_days: 30 });
const record = {
  id: 7, card_ident: "uuid:GPU-b", card_index: 1, card_name: "H20", pcie_bdf: "0000:40:00.0", removed: false, check: "runtime", level: "fail",
  reason: "runtime_unusable", started_at: now, last_seen_at: now, duration_seconds: 7200, count: 9, flapping: true, ongoing: true,
  first_evidence: [{ name: "Detail", value: "cuCtxCreate=999" }], latest_evidence: [{ name: "Detail", value: "cuCtxCreate=999" }],
};
const show = (ui: React.ReactNode) => render(<I18nextProvider i18n={i18n}>{ui}</I18nextProvider>);
const fill = (template: string, values: Record<string, unknown>) => template.replace(/{{(\w+)}}/g, (_, k) => String(values[k]));

test("the table shows the server's columns and each card's cells, and the records below", async () => {
  apiAny.nodeHealthChecks = async () => table();
  apiAny.nodeHealthRecords = async () => records([record]);
  show(<NodeHealthTab nodeUUID="n-1" />);
  await screen.findByText(ch.check.vendor_health);
  assert.ok(screen.getByText(ch.check.pcie_link));
  assert.ok(screen.getAllByText(ch.check.runtime).length >= 1);
  assert.ok(screen.getByText(fill(ch.summary, { cards: 2, fail: 1, warn: 1 })));
  const cells = screen.getAllByRole("button").filter((b) => b.getAttribute("data-state"));
  assert.deepEqual(cells.map((b) => b.getAttribute("data-state")), ["pass", "pass", "skip", "pass", "warn", "fail"]);
  assert.match(cells[2].getAttribute("aria-label") ?? "", new RegExp(ch.skip.busy));
  await screen.findByText(ch.records.ongoing);
  assert.ok(screen.getByText(ch.records.flapping));
  assert.ok(screen.getByText(fill(ch.records.count, { count: 9 })));
});

test("choosing a cell shows what the check last did and narrows the records to it", async () => {
  const user = userEvent.setup({ document: dom.window.document });
  const asked: unknown[] = [];
  const recordQueries: unknown[] = [];
  apiAny.nodeHealthChecks = async () => table();
  apiAny.nodeHealthRecords = async (_n: string, opts: unknown) => { recordQueries.push(opts); return records(); };
  apiAny.cardCheckDetail = async (...args: unknown[]) => {
    asked.push(args);
    return { ident: "uuid:GPU-b", check: "runtime", agent_started_at: now,
      last: { state: "fail", reason: "runtime_unusable", at: now, duration_ms: 812, probe: "CUDA_VISIBLE_DEVICES='GPU-b' python3 -c ...", evidence: [{ name: "Detail", value: "cuCtxCreate=999" }] } };
  };
  show(<NodeHealthTab nodeUUID="n-1" />);
  await screen.findByText(ch.check.runtime);
  const failing = screen.getAllByRole("button").find((b) => b.getAttribute("data-state") === "fail")!;
  await user.click(failing);
  await screen.findByText("cuCtxCreate=999");
  assert.deepEqual(asked[0], ["n-1", { pcie_bdf: "0000:40:00.0", uuid: "GPU-b" }, "runtime"]);
  assert.ok(screen.getByText(/CUDA_VISIBLE_DEVICES/));
  await waitFor(() => assert.deepEqual(recordQueries.at(-1), { card: { pcie_bdf: "0000:40:00.0", uuid: "GPU-b" }, check: "runtime", page: 1 }));
  assert.ok(screen.getByText(fill(ch.records.filteredBy, { card: fill(ch.card, { index: 1 }), check: ch.check.runtime })));

  await user.click(screen.getByRole("button", { name: ch.records.clear }));
  await waitFor(() => assert.deepEqual(recordQueries.at(-1), { card: undefined, check: undefined, page: 1 }));
});

test("a cell can be chosen from the keyboard", async () => {
  const user = userEvent.setup({ document: dom.window.document });
  let asked = 0;
  apiAny.nodeHealthChecks = async () => table();
  apiAny.nodeHealthRecords = async () => records();
  apiAny.cardCheckDetail = async () => { asked++; return { ident: "uuid:GPU-a", check: "vendor_health", last: { state: "pass", at: now, evidence: [] }, agent_started_at: now }; };
  show(<NodeHealthTab nodeUUID="n-1" />);
  await screen.findByText(ch.check.vendor_health);
  const first = screen.getAllByRole("button").find((b) => b.getAttribute("data-state") === "pass")!;
  first.focus();
  await user.keyboard("{Enter}");
  await screen.findByText(ch.detail.noEvidence);
  assert.equal(asked, 1);
  // When the agent started is a caveat on every answer: it waits behind the help mark.
  assert.equal(screen.queryByText(/only results since then/), null);
  await user.hover(screen.getByRole("button", { name: "More information" }));
  assert.match((await screen.findByRole("tooltip")).textContent ?? "", /^The agent started at .+; only results since then are known\.$/);
});

test("rechecking a card's runtime check reports its answer and reads the table again", async () => {
  const user = userEvent.setup({ document: dom.window.document });
  let loads = 0;
  const rechecked: unknown[] = [];
  apiAny.nodeHealthChecks = async () => { loads++; return table(); };
  apiAny.nodeHealthRecords = async () => records();
  apiAny.cardCheckDetail = async () => ({ ident: "uuid:GPU-b", check: "runtime", last: { state: "fail", at: now, evidence: [] } });
  apiAny.recheckCard = async (...args: unknown[]) => { rechecked.push(args); return { ident: "uuid:GPU-b", check: "runtime", last: { state: "pass", at: now, evidence: [] } }; };
  show(<NodeHealthTab nodeUUID="n-1" />);
  await screen.findByText(ch.check.runtime);
  await user.click(screen.getAllByRole("button").find((b) => b.getAttribute("data-state") === "fail")!);
  await user.click(await screen.findByRole("button", { name: ch.recheck }));
  await waitFor(() => assert.equal(rechecked.length, 1));
  assert.deepEqual(rechecked[0], ["n-1", { pcie_bdf: "0000:40:00.0", uuid: "GPU-b" }]);
  await screen.findByText(ch.recheckDone);
  await waitFor(() => assert.ok(loads >= 2));
});

test("an offline node says its results are the last known; an old agent says to upgrade", async () => {
  apiAny.nodeHealthChecks = async () => table({ online: false, stale: true });
  apiAny.nodeHealthRecords = async () => records();
  show(<NodeHealthTab nodeUUID="n-1" />);
  await screen.findByText(ch.banner.offline);
  assert.ok(screen.getByText(fill(ch.records.empty, { days: 30 })));
  cleanup();

  apiAny.nodeHealthChecks = async () => table({ reports: false, cards: [], checks: [], summary: {} });
  show(<NodeHealthTab nodeUUID="n-1" />);
  await screen.findByText(ch.banner.noReports);
});

test("a failure to load says why", async () => {
  apiAny.nodeHealthChecks = async () => { throw new APIError("NOT_FOUND", undefined, "node not found", 404); };
  apiAny.nodeHealthRecords = async () => records();
  show(<NodeHealthTab nodeUUID="n-1" />);
  await screen.findByRole("alert");
});

test("a detail the agent cannot give says why", async () => {
  const user = userEvent.setup({ document: dom.window.document });
  apiAny.nodeHealthChecks = async () => table();
  apiAny.nodeHealthRecords = async () => records();
  apiAny.cardCheckDetail = async () => { throw new APIError("NODE_OFFLINE", undefined, "the node is offline", 409); };
  show(<NodeHealthTab nodeUUID="n-1" />);
  await screen.findByText(ch.check.runtime);
  await user.click(screen.getAllByRole("button").find((b) => b.getAttribute("data-state") === "warn")!);
  await waitFor(() => assert.ok(screen.getAllByRole("alert").length >= 1));
});

test("one card's tab lists each check on its own line and narrows the records to the chosen one", async () => {
  const user = userEvent.setup({ document: dom.window.document });
  const recordQueries: unknown[] = [];
  apiAny.nodeHealthChecks = async (_n: string, card: unknown) => {
    assert.deepEqual(card, { pcie_bdf: "0000:40:00.0", uuid: "GPU-b" });
    const t = table();
    return { ...t, cards: [t.cards[1]] };
  };
  apiAny.nodeHealthRecords = async (_n: string, opts: unknown) => { recordQueries.push(opts); return records([record]); };
  apiAny.cardCheckDetail = async () => ({ ident: "uuid:GPU-b", check: "pcie_link", last: { state: "warn", reason: "pcie_link_degraded", at: now, evidence: [{ name: "PCIe Link 0000:40:00.0", value: "x8 of x16" }] } });
  show(<CardHealthTab nodeUUID="n-1" card={{ pcie_bdf: "0000:40:00.0", uuid: "GPU-b" }} cardLabel="GPU 1" />);
  await screen.findByText(ch.check.pcie_link);
  assert.ok(screen.getByText("812 ms"));
  await user.click(screen.getByText(ch.check.pcie_link));
  await screen.findByText("x8 of x16");
  await waitFor(() => assert.deepEqual(recordQueries.at(-1), { card: { pcie_bdf: "0000:40:00.0", uuid: "GPU-b" }, check: "pcie_link", page: 1 }));
});
