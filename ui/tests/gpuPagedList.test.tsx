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
// The detail drawer mounts its metric charts; a canvas that answers every
// call keeps them alive, as the node-page tests do.
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

const read = (ns: string) => JSON.parse(fs.readFileSync(new URL(`../src/i18n/locales/en/${ns}.json`, import.meta.url), "utf8"));
const gpus = read("gpus");
const i18n = createInstance();
await i18n.use(initReactI18next).init({ lng: "en", fallbackLng: "en", resources: { en: { gpus, common: read("common") } }, defaultNS: "gpus", interpolation: { escapeValue: false } });

const { MemoryRouter } = await import("react-router-dom");
const userEvent = (await import("@testing-library/user-event")).default;
const { cleanup, fireEvent, render, screen, waitFor, within } = await import("@testing-library/react");
const { api } = await import("../src/api.ts");
const { default: GPUPage } = await import("../src/pages/GPUPage.tsx");
import { sortColumn } from "./helpers/sortColumn";
import type { GPUPage as GPUListPage, ListQuery, PhysicalGPU } from "../src/api";

const fleet: PhysicalGPU[] = Array.from({ length: 30 }, (_, i) => ({
  index: i % 8, uuid: `GPU-${i}`, name: "NVIDIA H100", vendor: "nvidia", memory_total_bytes: 80 << 30, numa_node: 0, schedulable: i % 5 !== 0,
  node_uuid: `node-${Math.floor(i / 8)}`, node_name: `gpu-${Math.floor(i / 8)}`, node_state: "online",
  allocation_state: i % 5 ? "available" : "unusable", allocated_bytes: 0, available_bytes: 80 << 30, occupants: [], load: {},
} as never));

const queries: ListQuery[] = [];
const originalFetch = globalThis.fetch;
const originals = { gpusPage: api.gpusPage, gpus: api.gpus, dgpus: api.dgpus };
beforeEach(() => {
  localStorage.clear();
  queries.length = 0;
  api.gpus = async () => { throw new Error("the page fetched every GPU"); };
  api.dgpus = async () => ({ items: [] });
  api.gpusPage = async (query: ListQuery): Promise<GPUListPage> => {
    queries.push(query);
    const states = query.extra?.allocation_state ?? [];
    const matched = fleet.filter((gpu) => (!states.length || states.includes(gpu.allocation_state)) && (!query.q || gpu.uuid === query.q || gpu.node_uuid === query.q));
    const start = (query.page - 1) * query.page_size;
    return {
      items: matched.slice(start, start + query.page_size), page: query.page, page_size: query.page_size, total: matched.length,
      overview: { total: 30, health: { ok: 24, warning: 0, error: 6 }, allocation: { allocated: 0, allocatable: 24, unusable: 6, unknown: 0 } },
      facets: { nodes: [0, 1, 2, 3].map((n) => ({ uuid: `node-${n}`, name: `gpu-${n}` })), models: ["NVIDIA H100"], vendors: ["nvidia"] },
    };
  };
});
afterEach(() => { cleanup(); Object.assign(api, originals); globalThis.fetch = originalFetch; });
after(() => { closeChannels(); dom.window.close(); });

function renderPage(path = "/gpus") {
  render(<I18nextProvider i18n={i18n}><MemoryRouter initialEntries={[path]}><GPUPage /></MemoryRouter></I18nextProvider>);
}

// A card on a node nobody can reach is still listed, and is counted as
// neither allocatable nor unusable: every reading beside it is as the
// node's last report left it, so "unknown" is the only honest word. It is
// not "takes no work" either, which would say something about the card.
test("a card on an unreachable node is listed as unknown and offers nothing", async () => {
  globalThis.fetch = async (input) => {
    if (String(input).startsWith("/api/v1/gpus?")) return Response.json({
      items: [{ ...fleet[0], node_state: "offline", allocation_state: "unknown", available_bytes: 0 }],
      page: 1, page_size: 20, total: 1,
      overview: { total: 1, health: { ok: 1, warning: 0, error: 0 }, allocation: { allocated: 0, allocatable: 0, unusable: 0, unknown: 1 } },
      facets: { nodes: [{ uuid: fleet[0].node_uuid, name: fleet[0].node_name }], models: ["NVIDIA H100"], vendors: ["nvidia"] },
    });
    throw new Error(`Unexpected request: ${input}`);
  };
  api.gpusPage = originals.gpusPage;
  renderPage();
  const unknown = await waitFor(() => {
    const value = screen.getAllByText("Unknown").find((el) => el.closest("tr"));
    assert.ok(value);
    return value;
  });
  const row = unknown.closest("tr")!;
  assert.ok(!within(row).queryByText("Available"));
  assert.ok(!within(row).queryByText("Unavailable for allocation"), "an unreachable card is not known to be unusable");
  assert.equal(screen.getByText("Available").parentElement?.querySelector("strong")?.textContent, "0");
  assert.ok(within(row).getByText("H100"));
  const legend = document.querySelectorAll(".gpu-overview-legend")[1];
  assert.ok(legend.textContent?.includes("Unknown1"), "allocation summary must include offline devices exactly once");
});

test("the GPU list asks the server for one page; the overview counts the fleet", async () => {
  renderPage();
  await waitFor(() => assert.equal(screen.getAllByRole("row").slice(1).length, 20));
  assert.deepEqual(queries[0], { q: "", extra: {}, page: 1, page_size: 20 });
  assert.ok(screen.getByText(gpus.page.physicalCount.replace("{{count}}", "30")));
  fireEvent.click(screen.getByRole("button", { name: "Next page" }));
  await waitFor(() => assert.equal(screen.getAllByRole("row").slice(1).length, 10));
  assert.equal(queries.at(-1)?.page, 2);
});

test("header filters and sort go to the server; filter choices come from the server's facets", async () => {
  renderPage();
  await waitFor(() => assert.ok(queries.length > 0));
  await userEvent.setup().click(screen.getByRole("button", { name: `Filter by ${gpus.column.nodeGPU}` }));
  for (const node of ["gpu-0", "gpu-3"]) await screen.findByRole("checkbox", { name: new RegExp(node) });
  fireEvent.keyDown(document.activeElement ?? document.body, { key: "Escape" });

  await userEvent.setup().click(screen.getByRole("button", { name: `Filter by ${gpus.column.allocationState}` }));
  fireEvent.click(await screen.findByRole("checkbox", { name: new RegExp(gpus.allocation.unusable) }));
  await waitFor(() => assert.deepEqual(queries.at(-1)?.extra, { allocation_state: ["unusable"] }));
  assert.equal(queries.at(-1)?.page, 1);

  fireEvent.keyDown(document.activeElement ?? document.body, { key: "Escape" });
  await sortColumn(gpus.column.allocationState, "Ascending");
  await waitFor(() => assert.equal(queries.at(-1)?.sort, "allocation_state"));
});

test("a deep link opens a GPU that is not on the first page", async () => {
  renderPage("/gpus?gpu=GPU-29");
  await waitFor(() => assert.ok(queries.some((query) => query.q === "GPU-29")));
  await waitFor(() => assert.ok(screen.queryByRole("dialog", { name: /GPU 5/ })));
});

// A measured zero is a real reading. Unknown capacity must not become a
// zero-sized device in either the table or the detail drawer.
test("unknown capacity stays unknown while measured zero usage remains visible", async () => {
  const gpu: PhysicalGPU = { ...fleet[0], vendor: "amd", name: "AMD test GPU", memory_total_bytes: 0, memory_capacity_known: false,
    load: { memory_used_bytes: 0 }, occupants: [], allocation_state: "unusable" };
  api.gpusPage = async (query) => ({ items: [gpu], page: query.page, page_size: query.page_size, total: 1,
    overview: { total: 1, health: { ok: 0, warning: 0, error: 1 }, allocation: { allocated: 0, allocatable: 0, unusable: 1, unknown: 0 } },
    facets: { nodes: [{ uuid: gpu.node_uuid, name: gpu.node_name }], models: [gpu.name], vendors: ["amd"] } });
  renderPage();
  const usage = await screen.findByText("0.0 GiB / —");
  assert.equal(within(usage.closest("tr")!).queryByText("AMD") !== null, true);
  fireEvent.click(usage.closest("tr")!);
  await waitFor(() => {
    const drawer = document.querySelector(".gpu-detail-drawer");
    assert.equal(drawer !== null, true);
    const labels = Array.from(drawer?.querySelectorAll(".ant-descriptions-item-label") ?? []);
    const capacity = labels.find((label) => label.textContent === "VRAM");
    assert.equal(capacity?.parentElement?.querySelector(".ant-descriptions-item-content")?.textContent, "—");
  });
});
