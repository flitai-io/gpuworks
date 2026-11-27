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
        { check: "vendor_health", status: "pass", checked_at: now, stale: false },
        { check: "pcie_link", status: "pass", checked_at: now, stale: false },
        { check: "runtime", status: "skip", code: "CARD_CHECK_SKIPPED_BUSY", checked_at: now, stale: false },
      ] },
    { ident: "uuid:GPU-b", uuid: "GPU-b", pcie_bdf: "0000:40:00.0", name: "H20", index: 1, worst: "warn", can_recheck: true,
      cells: [
        { check: "vendor_health", status: "pass", checked_at: now, stale: false },
        { check: "pcie_link", status: "warn", code: "PCIE_LINK_DEGRADED", checked_at: now, stale: false },
        { check: "runtime", status: "fail", code: "RUNTIME_UNUSABLE", checked_at: now, duration_ms: 812, stale: false },
      ] },
  ],
  summary: { pass: 3, warn: 1, fail: 1, skip: 1 },
  ...extra,
});
const records = (list: object[] = []) => ({ records: list, total: list.length, page: 1, page_size: 50, retention_days: 30 });
const record = {
  id: 7, card_ident: "uuid:GPU-b", card_index: 1, card_name: "H20", pcie_bdf: "0000:40:00.0", removed: false, check: "runtime", status: "fail",
  code: "RUNTIME_UNUSABLE", started_at: now, last_seen_at: now, duration_seconds: 7200, count: 9, flapping: true, ongoing: true,
  first_evidence: [{ name: "Detail", value: "cuCtxCreate=999" }], latest_evidence: [{ name: "Detail", value: "cuCtxCreate=999" }],
};
const show = (ui: React.ReactNode) => render(<I18nextProvider i18n={i18n}>{ui}</I18nextProvider>);
const fill = (template: string, values: Record<string, unknown>) => template.replace(/{{(\w+)}}/g, (_, k) => String(values[k]));

test("every server card-health notice has English and Chinese copy", () => {
  const source = fs.readFileSync(new URL("../../internal/server/messages/card_health.go", import.meta.url), "utf8");
  const codes = [...source.matchAll(/=\s*"(CARD_CHECKS_[A-Z_]+)"/g)].map((match) => match[1]);
  assert.ok(codes.length > 0, "server notice codes must be discovered");
  for (const locale of ["en", "zh"]) {
    const messages = JSON.parse(fs.readFileSync(new URL(`../src/i18n/locales/${locale}/nodes.json`, import.meta.url), "utf8"));
    for (const code of codes) {
      assert.equal(typeof messages.cardHealth.banner[code], "string", `${locale}: ${code}`);
      assert.ok(messages.cardHealth.banner[code].trim().length > 0, `${locale}: ${code}`);
    }
  }
});

test("missing-driver and occupied-driver notices explain the recovery action", async () => {
  for (const [code, message] of [
    ["CARD_CHECKS_NO_DRIVER", "Install and load the GPU vendor driver to enable host health checks."],
    ["CARD_CHECKS_HELD_ELSEWHERE", "The GPUs are bound to another driver. To run host health checks, bind them to the GPU vendor driver."],
  ]) {
    apiAny.nodeHealthChecks = async () => table({ notice: { code, level: "info" } });
    apiAny.nodeHealthRecords = async () => records();
    show(<NodeHealthTab nodeUUID="n-1" />);
    assert.equal((await screen.findByRole("alert")).textContent, message);
    cleanup();
  }
});

test("passthrough checks use the server notice without suggesting an Agent upgrade", async () => {
  apiAny.nodeHealthChecks = async () => table({
    reports: false, cards: [], checks: [], summary: {},
    notice: { code: "CARD_CHECKS_PASSTHROUGH", level: "info" },
  });
  apiAny.nodeHealthRecords = async () => records();
  show(<NodeHealthTab nodeUUID="n-1" />);
  await screen.findByRole("alert");
  assert.equal(screen.getByRole("alert").textContent, "GPU passthrough is configured; the host does not run health checks for these cards.");
  assert.equal(screen.queryByText(ch.banner.CARD_CHECKS_UNSUPPORTED), null);
  assert.ok(screen.getByText("No failure records in the last 30 days."));
  cleanup();
  show(<CardHealthTab nodeUUID="n-1" card={{ pcie_bdf: "0000:09:00.0" }} cardLabel="GPU 0" />);
  await screen.findByRole("alert");
  assert.equal(screen.getByRole("alert").textContent, "GPU passthrough is configured; the host does not run health checks for these cards.");
  assert.equal(screen.queryByText(ch.banner.CARD_CHECKS_UNSUPPORTED), null);
});

test("missing reports alone do not imply an old Agent", async () => {
  apiAny.nodeHealthChecks = async () => table({
    reports: false, cards: [], checks: [], summary: {},
    notice: { code: "CARD_CHECKS_WAITING", level: "info" },
  });
  apiAny.nodeHealthRecords = async () => records();
  show(<NodeHealthTab nodeUUID="n-1" />);
  await screen.findByRole("alert");
  assert.equal(screen.getByRole("alert").textContent, ch.banner.CARD_CHECKS_WAITING);
  assert.equal(screen.queryByText(ch.banner.CARD_CHECKS_UNSUPPORTED), null);
});

test("an unknown notice code shows the localized fallback", async () => {
  apiAny.nodeHealthChecks = async () => table({ notice: { code: "CARD_CHECKS_NEW_REASON", level: "info" } });
  apiAny.nodeHealthRecords = async () => records();
  show(<NodeHealthTab nodeUUID="n-1" />);
  assert.equal((await screen.findByRole("alert")).textContent, ch.banner.unavailable);
  assert.equal(screen.queryByText("cardHealth.banner.CARD_CHECKS_NEW_REASON"), null);
});

test("unavailable devices display their specific server reason", async () => {
  for (const [code, key] of [
    ["CARD_CHECKS_NO_DRIVER", "CARD_CHECKS_NO_DRIVER"],
    ["CARD_CHECKS_HELD_ELSEWHERE", "CARD_CHECKS_HELD_ELSEWHERE"],
    ["CARD_CHECKS_NO_USABLE_CARDS", "CARD_CHECKS_NO_USABLE_CARDS"],
    ["CARD_CHECKS_NO_CARDS", "CARD_CHECKS_NO_CARDS"],
    ["CARD_CHECKS_STALE", "CARD_CHECKS_STALE"],
  ]) {
    apiAny.nodeHealthChecks = async () => table({
      reports: false, cards: [], checks: [], summary: {},
      notice: { code, level: code === "CARD_CHECKS_STALE" ? "warning" : "info" },
    });
    apiAny.nodeHealthRecords = async () => records();
    show(<NodeHealthTab nodeUUID="n-1" />);
    await screen.findByRole("alert");
    assert.equal(screen.getByRole("alert").textContent, ch.banner[key]);
    assert.equal(screen.queryByText(ch.banner.CARD_CHECKS_UNSUPPORTED), null);
    cleanup();
  }
});

test("Chinese passthrough notice explains ownership without suggesting an upgrade", async () => {
  const zh = createInstance();
  await zh.use(initReactI18next).init({ lng: "zh", resources: { zh: {
    nodes: JSON.parse(fs.readFileSync(new URL("../src/i18n/locales/zh/nodes.json", import.meta.url), "utf8")),
    common: JSON.parse(fs.readFileSync(new URL("../src/i18n/locales/zh/common.json", import.meta.url), "utf8")),
  } }, interpolation: { escapeValue: false } });
  apiAny.nodeHealthChecks = async () => table({
    reports: false, cards: [], checks: [], summary: {},
    notice: { code: "CARD_CHECKS_PASSTHROUGH", level: "info" },
  });
  apiAny.nodeHealthRecords = async () => records();
  render(<I18nextProvider i18n={zh}><NodeHealthTab nodeUUID="n-1" /></I18nextProvider>);
  await screen.findByRole("alert");
  assert.equal(screen.getByRole("alert").textContent, "GPU 已配置直通，宿主机不执行这些卡的健康检查。");
  assert.equal(screen.queryByText(/agent 版本过旧/), null);
});

test("the table shows the server's columns and each card's cells, and the records below", async () => {
  apiAny.nodeHealthChecks = async () => table();
  apiAny.nodeHealthRecords = async () => records([record]);
  show(<NodeHealthTab nodeUUID="n-1" />);
  await screen.findByText(ch.check.vendor_health);
  assert.ok(screen.getByText(ch.check.pcie_link));
  assert.ok(screen.getAllByText(ch.check.runtime).length >= 1);
  assert.ok(screen.getByText(fill(ch.summary, { cards: 2, fail: 1, warn: 1, skip: 1 })));
  const cells = screen.getAllByRole("button").filter((b) => b.getAttribute("data-status"));
  assert.deepEqual(cells.map((b) => b.getAttribute("data-status")), ["pass", "pass", "skip", "pass", "warn", "fail"]);
  assert.match(cells[2].getAttribute("aria-label") ?? "", new RegExp(ch.skip.CARD_CHECK_SKIPPED_BUSY));
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
      last: { status: "fail", code: "RUNTIME_UNUSABLE", at: now, duration_ms: 812, probe: "CUDA_VISIBLE_DEVICES='GPU-b' python3 -c ...", evidence: [{ name: "Detail", value: "cuCtxCreate=999" }] } };
  };
  show(<NodeHealthTab nodeUUID="n-1" />);
  await screen.findByText(ch.check.runtime);
  const failing = screen.getAllByRole("button").find((b) => b.getAttribute("data-status") === "fail")!;
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
  apiAny.cardCheckDetail = async () => { asked++; return { ident: "uuid:GPU-a", check: "vendor_health", last: { status: "pass", at: now, evidence: [] }, agent_started_at: now }; };
  show(<NodeHealthTab nodeUUID="n-1" />);
  await screen.findByText(ch.check.vendor_health);
  const first = screen.getAllByRole("button").find((b) => b.getAttribute("data-status") === "pass")!;
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
  apiAny.cardCheckDetail = async () => ({ ident: "uuid:GPU-b", check: "runtime", last: { status: "fail", at: now, evidence: [] } });
  apiAny.recheckCard = async (...args: unknown[]) => { rechecked.push(args); return { ident: "uuid:GPU-b", check: "runtime", last: { status: "pass", at: now, evidence: [] } }; };
  show(<NodeHealthTab nodeUUID="n-1" />);
  await screen.findByText(ch.check.runtime);
  await user.click(screen.getAllByRole("button").find((b) => b.getAttribute("data-status") === "fail")!);
  await user.click(await screen.findByRole("button", { name: ch.recheck }));
  await waitFor(() => assert.equal(rechecked.length, 1));
  assert.deepEqual(rechecked[0], ["n-1", { pcie_bdf: "0000:40:00.0", uuid: "GPU-b" }]);
  await screen.findByText(ch.recheckDone);
  await waitFor(() => assert.ok(loads >= 2));
});

test("an offline node says its results are the last known; an old agent says to upgrade", async () => {
  apiAny.nodeHealthChecks = async () => table({ online: false, stale: true, notice: { code: "CARD_CHECKS_OFFLINE", level: "warning" } });
  apiAny.nodeHealthRecords = async () => records();
  show(<NodeHealthTab nodeUUID="n-1" />);
  await screen.findByText(ch.banner.CARD_CHECKS_OFFLINE);
  assert.ok(screen.getByText(fill(ch.records.empty, { days: 30 })));
  cleanup();

  apiAny.nodeHealthChecks = async () => table({ reports: false, cards: [], checks: [], summary: {}, notice: { code: "CARD_CHECKS_UNSUPPORTED", level: "info" } });
  show(<NodeHealthTab nodeUUID="n-1" />);
  await screen.findByText(ch.banner.CARD_CHECKS_UNSUPPORTED);
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
  await user.click(screen.getAllByRole("button").find((b) => b.getAttribute("data-status") === "warn")!);
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
  apiAny.cardCheckDetail = async () => ({ ident: "uuid:GPU-b", check: "pcie_link", last: { status: "warn", code: "PCIE_LINK_DEGRADED", at: now, evidence: [{ name: "PCIe Link 0000:40:00.0", value: "x8 of x16" }] } });
  show(<CardHealthTab nodeUUID="n-1" card={{ pcie_bdf: "0000:40:00.0", uuid: "GPU-b" }} cardLabel="GPU 1" />);
  await screen.findByText(ch.check.pcie_link);
  assert.ok(screen.getByText("812 ms"));
  await user.click(screen.getByText(ch.check.pcie_link));
  await screen.findByText("x8 of x16");
  await waitFor(() => assert.deepEqual(recordQueries.at(-1), { card: { pcie_bdf: "0000:40:00.0", uuid: "GPU-b" }, check: "pcie_link", page: 1 }));
});

// Each of the five statuses reads as itself: only a pass is called passed,
// a skipped check names why it did not run, a check that has not run yet and
// one that does not apply say so, and none of them looks like a pass.
test("each check status renders in its own words and look", async () => {
  const cells = [
    { check: "vendor_health", status: "pass", checked_at: now, stale: false },
    { check: "pcie_link", status: "warn", code: "PCIE_LINK_DEGRADED", checked_at: now, stale: false },
    { check: "pcie_errors", status: "fail", code: "PCIE_FATAL_ERRORS", checked_at: now, stale: false },
    { check: "runtime", status: "skip", code: "CARD_CHECK_SKIPPED_PROBE_FAILED", checked_at: now, stale: false },
    { check: "xid", status: "skip", code: "CARD_CHECK_NOT_RUN_YET", stale: false },
    { check: "nvlink", status: "na", stale: false },
  ];
  apiAny.nodeHealthChecks = async () => table({
    checks: cells.map((c) => ({ id: c.check, period_seconds: 60 })),
    cards: [{ ident: "uuid:GPU-a", uuid: "GPU-a", index: 0, worst: "fail", can_recheck: false, cells }],
    summary: { pass: 1, warn: 1, fail: 1, skip: 2, na: 1 },
  });
  apiAny.nodeHealthRecords = async () => records();
  show(<NodeHealthTab nodeUUID="n-1" />);
  await screen.findByText(ch.check.vendor_health);
  const buttons = screen.getAllByRole("button").filter((b) => b.getAttribute("data-status"));
  const label = (i: number) => buttons[i].getAttribute("aria-label") ?? "";
  assert.deepEqual(buttons.map((b) => b.getAttribute("data-status")), ["pass", "warn", "fail", "skip", "skip", "na"]);
  const common = i18n.getResourceBundle("en", "common").gpuHealth.code;
  assert.equal(label(0).split(" · ")[1], ch.state.pass);
  assert.deepEqual(label(1).split(" · ").slice(1, 3), [ch.state.warn, common.PCIE_LINK_DEGRADED.label]);
  assert.deepEqual(label(2).split(" · ").slice(1, 3), [ch.state.fail, common.PCIE_FATAL_ERRORS.label]);
  assert.deepEqual(label(3).split(" · ").slice(1, 3), [ch.state.skip, ch.skip.CARD_CHECK_SKIPPED_PROBE_FAILED]);
  assert.deepEqual(label(4).split(" · ").slice(1, 3), [ch.state.skip, ch.skip.CARD_CHECK_NOT_RUN_YET]);
  assert.equal(label(5).split(" · ")[1], ch.state.na);
  for (const i of [1, 2, 3, 4, 5]) assert.notEqual(label(i).split(" · ")[1], ch.state.pass, `cell ${i} reads as passed`);
  // Not applicable cannot be chosen; a skipped check can, to see why.
  assert.equal((buttons[5] as HTMLButtonElement).disabled, true);
  assert.equal((buttons[3] as HTMLButtonElement).disabled, false);
  assert.ok(screen.getByText(fill(ch.summary, { cards: 1, fail: 1, warn: 1, skip: 2 })));
});

test("a skipped run in the detail names why it did not run", async () => {
  const user = userEvent.setup({ document: dom.window.document });
  apiAny.nodeHealthChecks = async () => table();
  apiAny.nodeHealthRecords = async () => records();
  apiAny.cardCheckDetail = async () => ({ ident: "uuid:GPU-a", check: "runtime", agent_started_at: now,
    last: { status: "skip", code: "CARD_CHECK_SKIPPED_BUSY", at: now, evidence: [] },
    last_ran: { status: "pass", at: now, evidence: [] } });
  show(<NodeHealthTab nodeUUID="n-1" />);
  await screen.findByText(ch.check.runtime);
  await user.click(screen.getAllByRole("button").find((b) => b.getAttribute("data-status") === "skip")!);
  await screen.findByText(ch.detail.lastRan);
  assert.ok(screen.getAllByText(ch.skip.CARD_CHECK_SKIPPED_BUSY).length >= 1);
  assert.ok(screen.getAllByText(ch.state.skip).length >= 1);
});

const HealthRecords = (await import("../src/components/cardHealth/HealthRecords.tsx")).default;

// The records list on its own: a placeholder while loading, the empty
// sentence, a pager only past one server page, and each record's evidence
// shown only once its section is opened.
test("the records list shows loading, empty and paged states and opens evidence on demand", async () => {
  const user = userEvent.setup({ document: dom.window.document });
  const pages: number[] = [];
  const view = show(<HealthRecords page={1} onPage={(p) => pages.push(p)} />);
  assert.ok(document.querySelector("[aria-busy=\"true\"]"), "a loading placeholder shows first");
  view.rerender(<I18nextProvider i18n={i18n}><HealthRecords data={records()} page={1} onPage={(p) => pages.push(p)} /></I18nextProvider>);
  assert.ok(screen.getByText(fill(ch.records.empty, { days: 30 })));
  assert.equal(screen.queryByRole("button", { name: "Next page" }), null, "no pager for a single page");

  const second = { ...record, id: 8, first_evidence: [{ name: "Detail", value: "first read" }], latest_evidence: [{ name: "Detail", value: "latest read" }] };
  view.rerender(<I18nextProvider i18n={i18n}><HealthRecords data={{ records: [second], total: 120, page: 1, page_size: 50, retention_days: 30 }} page={1} onPage={(p) => pages.push(p)} /></I18nextProvider>);
  assert.equal(screen.queryByText("first read"), null, "evidence stays folded");
  await user.click(screen.getByRole("button", { name: ch.records.first }));
  await screen.findByText("first read");
  assert.equal(screen.queryByText("latest read"), null);
  screen.getByRole("button", { name: ch.records.latest }).focus();
  await user.keyboard("{Enter}");
  await screen.findByText("latest read");

  assert.ok(screen.getByText("Page 1 of 3"));
  assert.equal(screen.queryByRole("combobox"), null, "the server fixes the page size, so there is no size select");
  assert.equal((screen.getByRole("button", { name: "Previous page" }) as HTMLButtonElement).disabled, true);
  await user.click(screen.getByRole("button", { name: "Next page" }));
  await user.click(screen.getByRole("button", { name: "Last page" }));
  assert.deepEqual(pages, [2, 3], "the pager reports one-based pages");
});
