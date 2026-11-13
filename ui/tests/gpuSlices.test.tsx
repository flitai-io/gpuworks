import assert from "node:assert/strict";
import fs from "node:fs";
import { after, afterEach, test } from "node:test";
import { trackMessageChannels } from "./helpers/trackedMessageChannels.ts";
import { JSDOM } from "jsdom";
import React from "react";
import { createInstance } from "i18next";
import { I18nextProvider, initReactI18next } from "react-i18next";

// How a carved card reads on the GPU page. The answers are the server's own
// (ui/tests/fixtures/gpu-inventory, generated from the real handlers): a
// whole card, a card carved into three slices with one still starting, and a
// card passed through to a virtual machine.

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
const common = read("common");
const i18n = createInstance();
await i18n.use(initReactI18next).init({ lng: "en", fallbackLng: "en", resources: { en: { gpus, common } }, defaultNS: "gpus", interpolation: { escapeValue: false } });

const fixture = (name: string) => JSON.parse(fs.readFileSync(new URL(`./fixtures/gpu-inventory/${name}.json`, import.meta.url), "utf8"));
const gpusPage = fixture("gpus-page");
const dgpus = fixture("dgpus");

const { MemoryRouter } = await import("react-router-dom");
const { cleanup, fireEvent, render, screen, waitFor, within } = await import("@testing-library/react");
const { default: GPUPage } = await import("../src/pages/GPUPage.tsx");
const { movingAverage } = await import("../src/pages/gpus/GPUDetailDrawers.tsx");
const { memorySegments } = await import("../src/components/GPUMemoryComposition.tsx");
const { gpuModelSummary } = await import("../src/nodes/cardIdentity.ts");

const originalFetch = globalThis.fetch;
globalThis.fetch = async (input) => {
  const url = String(input);
  if (url.startsWith("/api/v1/gpus?")) return Response.json(gpusPage);
  if (url.startsWith("/api/v1/dgpus")) return Response.json(dgpus);
  if (url.startsWith("/api/v1/metrics/query")) return Response.json({ items: [], total: 0 });
  throw new Error(`Unexpected request: ${url}`);
};
afterEach(() => { cleanup(); localStorage.clear(); });
after(() => { globalThis.fetch = originalFetch; closeChannels(); dom.window.close(); });

const GIB = 1024 ** 3;

function renderPage() {
  render(<I18nextProvider i18n={i18n}><MemoryRouter initialEntries={["/gpus"]}><GPUPage /></MemoryRouter></I18nextProvider>);
}

async function rowOf(model: string) {
  const cell = await screen.findByText(model);
  return cell.closest("tr") as HTMLElement;
}

test("a carved card says how much is carved; a card that takes no work offers no dGPU", async () => {
  renderPage();
  const carved = await rowOf("GeForce RTX 2080 Ti");
  // 8 + 8 + 4 GiB of limits on a 22 GiB card; the starting slice holds its
  // quota and is counted.
  assert.ok(within(carved).getByText("3 slices · 20 GiB / 22 GiB"));
  const passedThrough = await rowOf("GeForce RTX 3090");
  assert.ok(!within(passedThrough).queryByText(common.partitionCapability.available), "a passed-through card is not offered as dGPU-ready");
  const whole = await rowOf("Quadro RTX 8000");
  assert.ok(within(whole).getByText(common.partitionCapability.available), "an idle-able whole card still shows it can be carved");
});

test("the card's detail draws its memory as booked, slice by slice", async () => {
  renderPage();
  fireEvent.click(within(await rowOf("GeForce RTX 2080 Ti")).getByRole("button", { name: "gpu3" }));
  const drawer = await screen.findByRole("dialog");
  const legend = await within(drawer).findByText(gpus.composition.title);
  const section = legend.closest("section") as HTMLElement;
  const items = [...section.querySelectorAll(".gpu-memory-composition-legend li")].map((li) => li.textContent);
  assert.deepEqual(items, [
    "qwen3-dgpu-a · limit 8.0 GiB8.0 GiB",
    "qwen3-dgpu-b · limit 8.0 GiB8.0 GiB",
    "qwen3-dgpu-c · limit 4.0 GiB4.0 GiB",
    `${gpus.composition.overhead}1.5 GiB`,
    `${gpus.composition.free}0.5 GiB`,
    `${gpus.composition.usedLegend}14.7 GiB`,
  ]);
});

test("the dGPU list shows a starting slice with its state", async () => {
  renderPage();
  fireEvent.click(await screen.findByRole("tab", { name: gpus.tab.dgpu }));
  const starting = (await screen.findByText("qwen3-dgpu-c")).closest("tr") as HTMLElement;
  assert.ok(within(starting).getByText(common.states.starting));
  const running = screen.getByText("qwen3-dgpu-a").closest("tr") as HTMLElement;
  assert.ok(within(running).getByText(common.states.running));
  assert.ok(within(running).getByText("7.0 GiB"));
});

test("a slice's detail shows its use only when its backend reports it", async () => {
  renderPage();
  fireEvent.click(await screen.findByRole("tab", { name: gpus.tab.dgpu }));
  fireEvent.click(await screen.findByRole("button", { name: "qwen3-dgpu-a" }));
  const reported = await screen.findByRole("dialog");
  assert.ok(await within(reported).findByRole("heading", { name: gpus.detail.currentUse }));
  assert.ok(within(reported).getByText("7.0 GiB / 8.0 GiB"));
  assert.ok(within(reported).getByRole("heading", { name: gpus.detail.monitoringData }));
  cleanup();

  // A starting slice, like any Ascend or PPU slice, has reported no use: no empty block stands in for it.
  renderPage();
  fireEvent.click(await screen.findByRole("tab", { name: gpus.tab.dgpu }));
  fireEvent.click(await screen.findByRole("button", { name: "qwen3-dgpu-c" }));
  const silent = await screen.findByRole("dialog");
  assert.ok(await within(silent).findByRole("heading", { name: gpus.detail.quotaInfo }));
  assert.equal(within(silent).queryByRole("heading", { name: gpus.detail.currentUse }), null);
  assert.equal(within(silent).queryByRole("heading", { name: gpus.detail.monitoringData }), null);
});

test("memory segments follow the books: limits, what slices hold beyond them, residue, free", () => {
  const card = gpusPage.items.find((gpu: { index: number }) => gpu.index === 1);
  const labels = { slice: (name: string, limit: string) => `${name}:${limit}`, wholeCard: (name: string) => `whole:${name}`, overhead: "overhead", sleeping: "sleeping", free: "free" };
  const segments = memorySegments(card, labels);
  assert.deepEqual(segments.map((s) => [s.label, s.bytes / GIB]), [["qwen3-dgpu-a:8.0", 8], ["qwen3-dgpu-b:8.0", 8], ["qwen3-dgpu-c:4.0", 4], ["overhead", 1.5], ["free", 0.5]]);
  const whole = gpusPage.items.find((gpu: { index: number }) => gpu.index === 0);
  assert.deepEqual(memorySegments(whole, labels).map((s) => [s.label, s.bytes / GIB]), [["whole:qwen3-fullcard", 45]]);
  // A sleeper keeps its residue, drawn as such and not as its slice.
  const asleep = { ...card, allocated_bytes: 8.5 * GIB, available_bytes: 12.5 * GIB, occupants: [card.occupants[0], { ...card.occupants[1], sleep_level: 1 }] };
  assert.deepEqual(memorySegments(asleep, labels).map((s) => [s.label, s.bytes / GIB]), [["qwen3-dgpu-a:8.0", 8], ["overhead", 0.5], ["sleeping", 1], ["free", 12.5]]);
});

test("the SM reference is averaged over the trailing minute", () => {
  const raw = [{ labels: { gpu: "1" }, points: [[0, 100], [10_000, 0], [20_000, 100], [30_000, 0], [70_000, 0]] as Array<[number, number]> }];
  const [smoothed] = movingAverage(raw, 60_000);
  assert.deepEqual(smoothed.points.map((p) => Math.round(p[1])), [100, 50, 67, 50, 33]);
});

test("a mixed node is summarised by model, not after its first card", () => {
  assert.equal(gpuModelSummary({ gpu_count: 2, gpus: [{ name: "Quadro RTX 8000" }, { name: "NVIDIA GeForce RTX 2080 Ti" }] }), "1 × Quadro RTX 8000 · 1 × GeForce RTX 2080 Ti");
  assert.equal(gpuModelSummary({ gpu_count: 8, gpus: Array.from({ length: 8 }, () => ({ name: "NVIDIA H100" })) }), "8 × H100");
  assert.equal(gpuModelSummary({ gpu_count: 2, gpus: [] }), "2 × GPU");
});
