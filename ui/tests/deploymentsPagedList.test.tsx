import assert from "node:assert/strict";
import fs from "node:fs";
import { after, afterEach, test } from "node:test";
import { trackMessageChannels } from "./helpers/trackedMessageChannels.ts";
import { JSDOM } from "jsdom";
import React from "react";
import { createInstance } from "i18next";
import { I18nextProvider, initReactI18next } from "react-i18next";

const closeChannels = trackMessageChannels();
const dom = new JSDOM("<!doctype html><html><body></body></html>", { url: "http://localhost/" });
for (const name of ["window", "document", "navigator", "HTMLElement", "HTMLInputElement", "SVGElement", "Element", "Node", "MutationObserver", "ShadowRoot", "File", "Blob", "KeyboardEvent", "Document"] as const) {
  Object.defineProperty(globalThis, name, { value: dom.window[name], configurable: true });
}
Object.defineProperty(globalThis, "localStorage", { value: dom.window.localStorage, configurable: true });
Object.defineProperty(globalThis, "getComputedStyle", { value: (element: Element) => dom.window.getComputedStyle(element), configurable: true });
Object.defineProperty(dom.window, "matchMedia", { configurable: true, value: () => ({ matches: false, addListener() {}, removeListener() {}, addEventListener() {}, removeEventListener() {}, dispatchEvent: () => false }) });
class ResizeObserverStub { observe() {} unobserve() {} disconnect() {} }
Object.defineProperty(globalThis, "ResizeObserver", { value: ResizeObserverStub, configurable: true });
Object.defineProperties(dom.window.HTMLElement.prototype, {
  attachEvent: { configurable: true, value() {} },
  detachEvent: { configurable: true, value() {} },
});
class Events { addEventListener() {} removeEventListener() {} close() {} }
Object.defineProperty(globalThis, "EventSource", { value: Events, configurable: true });

const deploymentsLocale = JSON.parse(fs.readFileSync(new URL("../src/i18n/locales/en/deployments.json", import.meta.url), "utf8"));
const commonLocale = JSON.parse(fs.readFileSync(new URL("../src/i18n/locales/en/common.json", import.meta.url), "utf8"));
const i18n = createInstance();
await i18n.use(initReactI18next).init({
  lng: "en",
  fallbackLng: "en",
  resources: { en: { deployments: deploymentsLocale, common: commonLocale } },
  defaultNS: "deployments",
  interpolation: { escapeValue: false },
});

const { MemoryRouter } = await import("react-router-dom");
const { cleanup, fireEvent, render, screen, waitFor, within } = await import("@testing-library/react");
const userEvent = (await import("@testing-library/user-event")).default;
const { api } = await import("../src/api.ts");
const { default: DeploymentsPage } = await import("../src/pages/DeploymentsPage.tsx");
import type { DeploymentListItem, DeploymentPage, ListQuery } from "../src/api";
import { sortColumn } from "./helpers/sortColumn";
import { list } from "./helpers/listPage.ts";

// The fleet the fake server pages through: 45 deployments, every third one
// failed, the rest running, newest first. The failed ones have no replica
// running and their health check timed out; the rest answer it.
const fleet: DeploymentListItem[] = Array.from({ length: 45 }, (_, i) => {
  const id = 45 - i;
  return {
    id, name: `svc-${id}`, model_id: 1, model_name: id % 5 === 0 ? "Qwen/Qwen3-8B" : "org/other", served_name: `api-${id}`,
    engine: "vllm", node_uuid: "node-1", node_name: "gpu3", desired_state: "active", runtime_state: id % 3 === 0 ? "failed" : "running",
    sleepable: false, replicas: 1, template_deviated: false, created_at: "2026-10-03T00:00:00Z", operations: [],
    ready_replicas: id % 3 === 0 ? 0 : 1,
    health: id % 3 === 0
      ? { status: "timeout", threshold_sec: 60, last: null, recent_mean_ms: null, recent_ok: 0, recent: [] }
      : { status: "ok", threshold_sec: 60, last: { at_ms: Date.now(), status: "ok", latency_ms: 90 }, recent_mean_ms: 90, recent_ok: 1, recent: [] },
  };
});

// fakeServer answers GET /deployments?page=… the way the server does: search
// over the name, served name and model name, state filter, then the page.
function fakeServer(rows: DeploymentListItem[]) {
  const queries: ListQuery[] = [];
  const serve = async (query: ListQuery): Promise<DeploymentPage> => {
    queries.push(query);
    const q = query.q?.trim().toLowerCase() ?? "";
    // The server filters by the state a row shows, which follows sleep; a row
    // with no state of its own shows its runtime state.
    const matched = rows.filter((row) =>
      (!q || [row.name, row.served_name, row.model_name].some((name) => name.toLowerCase().includes(q))) &&
      (!query.state?.length || query.state.includes(row.state ?? row.runtime_state)));
    const start = (query.page - 1) * query.page_size;
    return { items: matched.slice(start, start + query.page_size), page: query.page, page_size: query.page_size, total: matched.length };
  };
  return { queries, serve };
}

const originals = { deploymentsPage: api.deploymentsPage, deployments: api.deployments, nodes: api.nodes, routingPolicies: api.routingPolicies };
afterEach(() => { cleanup(); Object.assign(api, originals); });
after(() => { closeChannels(); dom.window.close(); });

function renderPage(serve: (query: ListQuery) => Promise<DeploymentPage>) {
  api.deploymentsPage = serve;
  api.deployments = async () => { throw new Error("the list page fetched every deployment"); };
  api.nodes = async () => { throw new Error("the list page fetched every node without an open detail"); };
  api.routingPolicies = async () => list([]);
  render(<I18nextProvider i18n={i18n}><MemoryRouter initialEntries={["/deployments"]}><DeploymentsPage /></MemoryRouter></I18nextProvider>);
}

const rowNames = () => screen.getAllByRole("row").slice(1).map((row) => within(row).getAllByRole("cell")[0].textContent);

test("a legacy empty served name stays empty rather than copying the model name", async () => {
  const server = fakeServer([{ ...fleet[0], served_name: "" }]);
  renderPage(server.serve);
  const name = await screen.findByText("svc-45");
  const headers = screen.getAllByRole("columnheader");
  const index = headers.indexOf(screen.getByRole("columnheader", { name: "Served name" }));
  const cell = within(name.closest("tr")!).getAllByRole("cell")[index];
  assert.equal(cell.textContent, "—");
  assert.equal(within(cell).queryByRole("button"), null);
});

test("a long served name keeps its copy button clickable", async () => {
  // GWKS-145: the cell was ellipsized as a whole, so a long external model
  // name pushed the copy button past the column edge and it could not be
  // clicked at all. jsdom does no layout, so this pins the cause instead of
  // the pixels: the served name column must opt out of the table's cell-wide
  // ellipsis, the name must be truncated on its own, and the copy button must
  // be its sibling — never its child, where clipping the name would hide it.
  const long = "Qwen/Qwen2.5-72B-Instruct-AWQ-very-long-served-name-abcdefghijklmnop";
  const server = fakeServer([{ ...fleet[0], served_name: long }]);
  renderPage(server.serve);
  const name = await screen.findByText("svc-45");
  const headers = screen.getAllByRole("columnheader");
  const index = headers.indexOf(screen.getByRole("columnheader", { name: "Served name" }));
  const cell = within(name.closest("tr")!).getAllByRole("cell")[index];

  assert.equal(cell.classList.contains("ant-table-cell-ellipsis"), false, "the cell clips nothing, so the button stays inside the column");
  const text = within(cell).getByText(long);
  assert.ok(text.classList.contains("ant-typography-ellipsis"), "the name truncates on its own");
  assert.equal(text.querySelector("button"), null, "truncating the name cannot clip the button");
  assert.ok(cell.textContent?.includes(long), "the full name stays in the DOM, not a JS-sliced prefix");

  const button = within(cell).getByRole("button", { name: "Copy" });
  assert.equal(button.textContent, "", "the button is the icon-only copy affordance");
  assert.equal(button.hasAttribute("disabled"), false);

  // And it copies what the cell shows, not the truncated prefix.
  const originalExecCommand = document.execCommand;
  let copied = "";
  document.execCommand = (command) => {
    assert.equal(command, "copy");
    copied = window.getSelection()?.toString() || "";
    return true;
  };
  try {
    await userEvent.setup({ document: dom.window.document }).click(button);
  } finally {
    document.execCommand = originalExecCommand;
  }
  assert.equal(copied, long);
});

test("the list asks the server for one page and shows the total", async () => {
  const server = fakeServer(fleet);
  renderPage(server.serve);
  await screen.findByText("svc-45");
  assert.deepEqual(server.queries[0], { page: 1, page_size: 20, q: "", state: [] });
  assert.equal(rowNames().length, 20);
  assert.ok(screen.getByText("45 deployments"), "the header counts every deployment, not the page");
  // State, health and replicas side by side.
  const cells = (name: string) => within(screen.getByText(name).closest("tr")!).getAllByRole("cell").map((cell) => cell.textContent);
  assert.ok(cells("svc-44").includes("Healthy · 90 ms") && cells("svc-44").includes("1/1"), String(cells("svc-44")));
  assert.ok(cells("svc-45").includes("Timed out") && cells("svc-45").includes("0/1"), String(cells("svc-45")));

  fireEvent.click(screen.getByRole("button", { name: "Last page" }));
  await screen.findByText("svc-5");
  assert.deepEqual(rowNames(), ["svc-5", "svc-4", "svc-3", "svc-2", "svc-1"]);
  assert.equal(server.queries.at(-1)?.page, 3);
});

test("searching asks the server and starts again from the first page", async () => {
  const server = fakeServer(fleet);
  renderPage(server.serve);
  await screen.findByText("svc-45");
  fireEvent.click(screen.getByRole("button", { name: "Next page" }));
  await screen.findByText("svc-25");

  const search = screen.getByRole("searchbox", { name: deploymentsLocale.page.search });
  await userEvent.setup().type(search, "qwen3-8b{Enter}");
  await waitFor(() => assert.equal(server.queries.at(-1)?.q, "qwen3-8b"));
  assert.equal(server.queries.at(-1)?.page, 1);
  await screen.findByText("svc-45");
  assert.deepEqual(rowNames(), ["svc-45", "svc-40", "svc-35", "svc-30", "svc-25", "svc-20", "svc-15", "svc-10", "svc-5"]);
  assert.ok(screen.getByText("9 deployments"));
});

test("the state column's header filter asks the server for those states", async () => {
  const server = fakeServer(fleet);
  renderPage(server.serve);
  await screen.findByText("svc-45");
  await userEvent.setup().click(screen.getByRole("button", { name: "Filter by State" }));
  const failed = await screen.findByRole("checkbox", { name: /Failed/ });
  fireEvent.click(failed);
  await waitFor(() => assert.deepEqual(server.queries.at(-1)?.state, ["failed"]));
  assert.equal(server.queries.at(-1)?.page, 1);
  await waitFor(() => assert.ok(screen.getByText("15 deployments")));
  assert.ok(rowNames().every((name) => Number(name?.replace("svc-", "")) % 3 === 0));
});

test("sorting a column asks the server to order every row, not just the page", async () => {
  const server = fakeServer(fleet);
  renderPage(server.serve);
  await screen.findByText("svc-45");
  await sortColumn("Name", "Ascending");
  await waitFor(() => assert.equal(server.queries.at(-1)?.sort, "name"));
  await sortColumn("Name", "Descending");
  await waitFor(() => assert.equal(server.queries.at(-1)?.sort, "-name"));
  await sortColumn("Name", "Clear sorting");
  await waitFor(() => assert.equal(server.queries.at(-1)?.sort, undefined));
});

// The header always offers to create a deployment, so the empty state is
// checked on its own: it offers creating only when there is nothing at all.
const createButton = (scope: HTMLElement) => within(scope).queryByRole("button", { name: new RegExp(`${deploymentsLocale.page.create}$`) });
const emptyState = (description: string) => screen.getByText(description).closest('[data-slot="empty"]') as HTMLElement;
const pageHeader = () => document.querySelector(".page-header") as HTMLElement;

test("the header offers to create a deployment, whatever the list holds", async () => {
  renderPage(fakeServer(fleet).serve);
  await screen.findByText("svc-45");
  assert.ok(createButton(pageHeader()));
});

test("a search that matches nothing says so instead of offering to create a deployment", async () => {
  const server = fakeServer(fleet);
  renderPage(server.serve);
  await screen.findByText("svc-45");
  await userEvent.setup().type(screen.getByRole("searchbox", { name: deploymentsLocale.page.search }), "no-such-service{Enter}");
  await screen.findByText(deploymentsLocale.page.emptyFiltered);
  assert.equal(createButton(emptyState(deploymentsLocale.page.emptyFiltered)), null);
  assert.ok(createButton(pageHeader()), "the header keeps its button");
});

test("with no deployments at all the empty state offers to create one", async () => {
  renderPage(fakeServer([]).serve);
  await screen.findByText(deploymentsLocale.page.empty);
  assert.ok(createButton(emptyState(deploymentsLocale.page.empty)));
  assert.equal(screen.getAllByRole("button", { name: new RegExp(`${deploymentsLocale.page.create}$`) }).length, 2, "the header's and the empty state's");
});

test("a failed page load shows the error and retries on request", async () => {
  let calls = 0;
  const server = fakeServer(fleet);
  renderPage(async (query) => {
    calls += 1;
    if (calls === 1) throw new Error("list unavailable");
    return server.serve(query);
  });
  await screen.findByText("list unavailable");
  fireEvent.click(screen.getByRole("button", { name: commonLocale.actions.retry }));
  await screen.findByText("svc-45");
});


// The reported "界面无重试入口": a list row that reads 模型准备失败 carried no
// action at all, so the retry existed only inside the drawer behind it.
test("a list row whose model preparation failed offers the retry there and then", async () => {
  const asked: unknown[] = [];
  const retryOriginal = api.retryModelCache;
  api.retryModelCache = (async (id: number, node: string, storage: number) => {
    asked.push([id, node, storage]);
    return { status: "pending" };
  }) as never;
  try {
    renderPage(fakeServer([
      { ...fleet[0], id: 201, name: "broken-copy", model_id: 131, runtime_state: "failed", preparation: {
        model_id: 131, model_name: "org/m", node_uuid: "793bf494", node_name: "gpu-1", storage_id: 3, storage_name: "cache",
        state: "failed", path: "/cache/m", progress: 0, size_bytes: 0, downloaded: 0, transferred_bytes: 0, rate_bps: 0,
        acquisition: "copy", message: "copy .msc: open /cache/m/.msc: permission denied",
        preparation: { phase: "waiting", reason: "MODEL_PREPARATION_FAILED", total_known: false, idle_seconds: 0, retryable: true },
      } },
    ]).serve);
    await screen.findByText("broken-copy");
    const row = screen.getByText("broken-copy").closest("tr")!;
    assert.ok(within(row).getByText(deploymentsLocale.preparation.reason.MODEL_PREPARATION_FAILED));
    const user = userEvent.setup({ document: dom.window.document });
    // The row's own storage and node, not a guess.
    await user.click(within(row).getByRole("button", { name: deploymentsLocale.preparation.retry }));
    await waitFor(() => assert.deepEqual(asked, [[131, "793bf494", 3]]));
  } finally {
    api.retryModelCache = retryOriginal;
  }
});

test("a list row failed from a download says it cannot be retried instead of nothing", async () => {
  renderPage(fakeServer([
    { ...fleet[0], id: 202, name: "download-broken", model_id: 9, runtime_state: "failed", preparation: {
      model_id: 9, node_uuid: "n-1", node_name: "gpu-1", storage_id: 4, state: "failed", path: "/models/m", progress: 0,
      size_bytes: 0, downloaded: 0, transferred_bytes: 0, rate_bps: 0, acquisition: "download",
      preparation: { phase: "waiting", reason: "MODEL_PREPARATION_FAILED", total_known: false, idle_seconds: 0 },
    } },
  ]).serve);
  await screen.findByText("download-broken");
  const row = screen.getByText("download-broken").closest("tr")!;
  assert.equal(within(row).queryByRole("button", { name: deploymentsLocale.preparation.retry }), null);
  assert.ok(within(row).getByText(deploymentsLocale.preparation.retryUnavailableShort));
});

test("replica column labels ready versus desired for PD and ordinary rows, preserving unknown readiness", async () => {
  renderPage(fakeServer([
    { ...fleet[0], id: 101, name: "pd-ready", replicas: 2, ready_replicas: 2 },
    { ...fleet[0], id: 102, name: "pd-partial", replicas: 3, ready_replicas: 1 },
    { ...fleet[0], id: 103, name: "ordinary-pending", replicas: 1, ready_replicas: 0 },
    { ...fleet[0], id: 104, name: "unknown-readiness", replicas: 2, ready_replicas: undefined },
  ]).serve);
  await screen.findByText("pd-ready");
  assert.ok(screen.getByRole("columnheader", { name: /Ready \/ desired replicas/ }));
  for (const [name, count] of [["pd-ready", "2/2"], ["pd-partial", "1/3"], ["ordinary-pending", "0/1"], ["unknown-readiness", "—/2"]]) {
    assert.ok(within(screen.getByText(name).closest("tr")!).getByRole("cell", { name: count }));
  }
});

// A sleeping deployment is not green, and the actions that would only get a
// 503 say why they are unavailable. A partly slept one still serves, so it
// keeps the running badge but carries the count of what is asleep.
test("a sleeping deployment is not shown as running and refuses to open the Playground", async () => {
  const operations = [{ type: "openai.chat", endpoint: "/v1/chat/completions", renderer: "chat" }];
  renderPage(fakeServer([
    { ...fleet[0], id: 201, name: "awake", runtime_state: "running", state: "running", replicas: 1, ready_replicas: 1, operations },
    { ...fleet[0], id: 202, name: "draining", runtime_state: "running", state: "sleeping", replicas: 1, ready_replicas: 0, operations },
    { ...fleet[0], id: 203, name: "slept", runtime_state: "running", state: "asleep", sleep_level: 1, asleep_replicas: 1, replicas: 1, ready_replicas: 0, operations },
    { ...fleet[0], id: 204, name: "half-slept", runtime_state: "running", state: "running", sleep_level: 1, asleep_replicas: 1, replicas: 4, ready_replicas: 3, operations },
  ]).serve);
  await screen.findByText("awake");

  const row = (name: string) => within(screen.getByText(name).closest("tr")!);
  // The badge text is the state, and only the awake row is green: antd marks
  // the status on the dot beside the text.
  const badgeStatus = (name: string, text: string) => {
    const badge = row(name).getByText(text).closest(".ant-badge")!;
    return [...badge.querySelector(".ant-badge-status-dot")!.classList]
      .map((cls) => cls.match(/^ant-badge-status-(success|warning|processing|default|error)$/)?.[1])
      .find(Boolean);
  };
  assert.equal(badgeStatus("awake", commonLocale.states.running), "success");
  assert.equal(badgeStatus("draining", commonLocale.states.sleeping), "processing");
  assert.equal(badgeStatus("slept", commonLocale.states.asleep), "warning");
  // A partly slept deployment still serves, so it keeps the running text —
  // but not the green badge, which would hide the replicas answering nothing.
  assert.equal(badgeStatus("half-slept", commonLocale.states.running), "processing");
  assert.ok(row("half-slept").getByText("Partly asleep · 1/4 replicas"));

  // An asleep deployment cannot take requests; a partly slept one can, so its
  // actions stay open.
  assert.equal(row("awake").getByRole("button", { name: deploymentsLocale.page.openPlayground }).hasAttribute("disabled"), false);
  assert.equal(row("awake").getByRole("button", { name: deploymentsLocale.page.copyCurl }).hasAttribute("disabled"), false);
  assert.equal(row("half-slept").getByRole("button", { name: deploymentsLocale.page.openPlayground }).hasAttribute("disabled"), false);
  for (const name of ["draining", "slept"]) {
    assert.ok(row(name).getByRole("button", { name: deploymentsLocale.page.openPlayground }).hasAttribute("disabled"), name);
    assert.ok(row(name).getByRole("button", { name: deploymentsLocale.page.copyCurl }).hasAttribute("disabled"), name);
  }
});

// The filter offers the sleep states and the server is asked for them.
test("the state filter offers sleeping and asleep", async () => {
  const server = fakeServer([
    { ...fleet[0], id: 301, name: "awake", state: "running" },
    { ...fleet[0], id: 302, name: "slept", runtime_state: "running", state: "asleep", sleep_level: 1, asleep_replicas: 1, ready_replicas: 0 },
  ]);
  renderPage(server.serve);
  await screen.findByText("awake");
  await userEvent.setup().click(screen.getByRole("button", { name: "Filter by State" }));
  assert.ok(await screen.findByRole("checkbox", { name: new RegExp(commonLocale.states.sleeping) }));
  fireEvent.click(screen.getByRole("checkbox", { name: new RegExp(commonLocale.states.asleep) }));
  await waitFor(() => assert.deepEqual(server.queries.at(-1)?.state, ["asleep"]));
  await waitFor(() => assert.equal(screen.queryByText("awake"), null));
  assert.ok(screen.getByText("slept"));
});
