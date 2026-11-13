import assert from "node:assert/strict";
import fs from "node:fs";
import { after, afterEach, beforeEach, test } from "node:test";
import { trackMessageChannels } from "./helpers/trackedMessageChannels.ts";
import { JSDOM } from "jsdom";
import React from "react";
import { createInstance } from "i18next";
import { I18nextProvider, initReactI18next } from "react-i18next";

const closeChannels = trackMessageChannels();
const dom = new JSDOM("<!doctype html><html><body></body></html>", { url: "http://localhost/" });
for (const name of ["window", "document", "navigator", "HTMLElement", "HTMLInputElement", "SVGElement", "Element", "Node", "MutationObserver", "ShadowRoot", "KeyboardEvent", "Document", "HTMLBodyElement"] as const) {
  Object.defineProperty(globalThis, name, { value: dom.window[name], configurable: true });
}
Object.defineProperty(globalThis, "localStorage", { value: dom.window.localStorage, configurable: true });
Object.defineProperty(globalThis, "getComputedStyle", { value: (element: Element) => dom.window.getComputedStyle(element), configurable: true });
Object.defineProperty(dom.window, "matchMedia", { configurable: true, value: () => ({ matches: false, addListener() {}, removeListener() {}, addEventListener() {}, removeEventListener() {}, dispatchEvent: () => false }) });
class ResizeObserverStub { observe() {} unobserve() {} disconnect() {} }
Object.defineProperty(globalThis, "ResizeObserver", { value: ResizeObserverStub, configurable: true });
class Events { addEventListener() {} removeEventListener() {} close() {} }
Object.defineProperty(globalThis, "EventSource", { value: Events, configurable: true });

const read = (ns: string) => JSON.parse(fs.readFileSync(new URL(`../src/i18n/locales/en/${ns}.json`, import.meta.url), "utf8"));
const models = read("models");
const i18n = createInstance();
await i18n.use(initReactI18next).init({ lng: "en", fallbackLng: "en", resources: { en: { models, common: read("common") } }, defaultNS: "models", interpolation: { escapeValue: false } });

const { MemoryRouter } = await import("react-router-dom");
const { act, cleanup, fireEvent, render, screen, waitFor, within } = await import("@testing-library/react");
const { message } = await import("antd");
const userEvent = (await import("@testing-library/user-event")).default;
const { api } = await import("../src/api.ts");
const { default: ModelsPage } = await import("../src/pages/ModelsPage.tsx");
import type { ListQuery, Model, ModelPage } from "../src/api";
import { sortColumn } from "./helpers/sortColumn";
import { installFleet } from "./helpers/fakeFleet.ts";

const presence = { ready_nodes: 3, cached_nodes: 0, ready_storages: 0, in_flight: 0, in_flight_percent: 0, failed: 0 };
const fleet = Array.from({ length: 45 }, (_, i) => ({
  id: 45 - i, source: i % 2 ? "modelscope" : "huggingface", source_id: `org/model-${45 - i}`, revision: "main", name: `model-${45 - i}`,
  arch: "", quant_label: "", weight_gib: 1, operations: [{ type: i % 3 ? "openai.chat" : "openai.embeddings", endpoint: "/v1" }], input_modalities: ["text"],
  created_at: "2026-10-01T00:00:00Z", presence,
}));

const queries: ListQuery[] = [];
const originals = { modelsPage: api.modelsPage, model: api.model, nodes: api.nodes, nodesPage: api.nodesPage, nodeFacets: api.nodeFacets, models: api.models };
let nodeFleet: ReturnType<typeof installFleet>;
beforeEach(() => {
  localStorage.clear();
  queries.length = 0;
  api.models = async () => { throw new Error("the page fetched every model"); };
  // The page searches nodes in its dialogs and sums the fleet for the
  // assistant; fetching every node fails the test.
  nodeFleet = installFleet(api as never, [
    { uuid: "n1", hostname: "gpu-01", state: "online", gpus: [{ index: 0, name: "H100", vendor: "nvidia", compute_capability: "9.0", memory_total_bytes: 80 * 1024 ** 3 }, { index: 1, name: "H100", vendor: "nvidia", compute_capability: "9.0", memory_total_bytes: 80 * 1024 ** 3 }] },
    { uuid: "n2", hostname: "gpu-02", state: "offline", gpus: [{ index: 0, name: "L40S", vendor: "nvidia", compute_capability: "8.9", memory_total_bytes: 48 * 1024 ** 3 }] },
  ] as never);
  api.modelsPage = async (query: ListQuery): Promise<ModelPage> => {
    queries.push(query);
    const sources = query.extra?.source ?? [];
    const matched = fleet.filter((m) => (!sources.length || sources.includes(m.source)) && (!query.q || m.name.includes(query.q)));
    const start = (query.page - 1) * query.page_size;
    return { items: matched.slice(start, start + query.page_size), page: query.page, page_size: query.page_size, total: matched.length, summary: { registered: 45, ready_files: 135, sources: 2, weight_gib: 45 } };
  };
});
afterEach(async () => {
  await act(async () => { message.destroy(); cleanup(); });
  Object.assign(api, originals);
});
after(() => { closeChannels(); dom.window.close(); });

function renderPage(view: "grid" | "list", path = "/models") {
  localStorage.setItem("gpuworks-model-view", view);
  render(<I18nextProvider i18n={i18n}><MemoryRouter initialEntries={[path]}><ModelsPage /></MemoryRouter></I18nextProvider>);
}

test("the card view asks the server for one page and pages through the rest", async () => {
  renderPage("grid");
  await screen.findByText("model-45");
  assert.deepEqual(queries[0], { q: "", extra: { source: [], operation: [] }, page: 1, page_size: 20 });
  assert.equal(document.querySelectorAll(".model-tile").length, 20);
  assert.ok(screen.getByText("45"), "the summary counts every model, from the server");
  fireEvent.click(screen.getByTitle("3"));
  await screen.findByText("model-5");
  assert.equal(queries.at(-1)?.page, 3);
  assert.equal(document.querySelectorAll(".model-tile").length, 5);
});

test("source, capability and search filters go to the server and start from page one", async () => {
  renderPage("grid");
  await screen.findByText("model-45");
  fireEvent.click(screen.getByTitle("2"));
  await screen.findByText("model-25");
  fireEvent.click(screen.getByRole("button", { name: /^Embedding/ }));
  await waitFor(() => assert.deepEqual(queries.at(-1)?.extra?.operation, ["openai.embeddings"]));
  assert.equal(queries.at(-1)?.page, 1);
  await userEvent.setup().type(screen.getByRole("textbox", { name: models.filter.searchAria }), "model-1");
  await waitFor(() => assert.equal(queries.at(-1)?.q, "model-1"));
});

test("the list view sorts through the server and shows the summed presence", async () => {
  renderPage("list");
  await screen.findByText("model-45");
  assert.equal(screen.getAllByRole("button", { name: "Ready on 3 nodes" }).length, 20);
  await sortColumn(/^Weight/, "Ascending");
  await waitFor(() => assert.equal(queries.at(-1)?.sort, "weight"));
});

test("the detail and the copy dialog read the model itself, with its locations", async () => {
  const full: Model = { ...fleet[0], nodes: [{ node_uuid: "n1", node_name: "gpu3", state: "present", path: "/m", progress: 1, size_bytes: 1, downloaded: 1, transferred_bytes: 0, rate_bps: 0 }], storages: [] } as never;
  const reads: number[] = [];
  api.model = async (id: number) => { reads.push(id); return full; };
  renderPage("list", "/models?detail=45");
  const dialog = await screen.findByRole("dialog");
  await within(dialog).findByText("gpu3");
  assert.deepEqual(reads, [45]);
  cleanup();
  renderPage("list");
  await screen.findByText("model-45");
  const row = screen.getByRole("button", { name: "model-45" }).closest("tr")!;
  fireEvent.click(within(row).getByRole("button", { name: models.action.copyTo }));
  await waitFor(() => assert.equal(reads.length, 2, "the copy dialog fetched the model's locations"));
});

test("the page reads no node: the assistant's fleet fit is computed on the server", async () => {
  renderPage("list");
  await screen.findByText("model-45");
  // Give a poll a chance to fire before asserting nothing was read.
  await new Promise((done) => setTimeout(done, 50));
  assert.deepEqual(nodeFleet.facetCalls, [], "the page no longer sums the fleet for a client-side prompt");
  assert.equal(nodeFleet.pages.length, 0, "no dialog is open, so no node is searched");
});

test("deleting the last row of a page steps back to the previous page instead of showing an empty one", async () => {
  // A 41-model fleet at page size 20 leaves exactly one row on page 3.
  const local = Array.from({ length: 41 }, (_, i) => ({ ...fleet[i], id: 41 - i, name: `model-${41 - i}`, source_id: `org/model-${41 - i}` }));
  const deleted: number[] = [];
  const originalDelete = api.deleteModel;
  api.modelsPage = async (query: ListQuery): Promise<ModelPage> => {
    queries.push(query);
    const live = local.filter((m) => !deleted.includes(m.id));
    const start = (query.page - 1) * query.page_size;
    return { items: live.slice(start, start + query.page_size), page: query.page, page_size: query.page_size, total: live.length, summary: { registered: live.length, ready_files: 0, sources: 2, weight_gib: 1 } };
  };
  api.deleteModel = (async (id: number) => { deleted.push(id); }) as never;
  try {
    renderPage("list");
    await screen.findByText("model-41");
    fireEvent.click(screen.getByRole("button", { name: "Last page" }));
    await screen.findByText("model-1");
    assert.equal(document.querySelectorAll("tr[data-row-key]").length, 1, "page 3 holds exactly one row");
    const row = screen.getByText("model-1").closest("tr")!;
    fireEvent.click(within(row).getByRole("button", { name: models.action.more }));
    fireEvent.click(await screen.findByRole("menuitem", { name: /Delete model/ }));
    const dialog = await screen.findByRole("dialog");
    fireEvent.click(within(dialog).getByRole("button", { name: "Delete" }));
    await screen.findByText("model-2");
    await screen.findByText(models.message.deleted);
    assert.equal(queries.at(-1)?.page, 2, "the emptied page 3 must not be refetched; the previous page loads");
    assert.equal(screen.queryByText("model-1"), null);
    assert.deepEqual(deleted, [1]);
  } finally {
    api.deleteModel = originalDelete;
  }
});
