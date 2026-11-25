import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import { createI18n, dom } from "./helpers/dom.ts";
import React from "react";
import { I18nextProvider } from "react-i18next";

Object.defineProperty(globalThis, "localStorage", { value: dom.window.localStorage, configurable: true });

const { i18n, locale } = await createI18n(["models"]);
const text = locale.models;
const common = locale.common;

// The page subscribes to the shared server-sent event stream; the test fires
// "modelfile" events itself, the same trigger a running download produces.
class Events {
  static listeners = new Map<string, Set<EventListener>>();
  addEventListener(type: string, listener: EventListener) {
    const listeners = Events.listeners.get(type) ?? new Set<EventListener>();
    listeners.add(listener);
    Events.listeners.set(type, listeners);
  }
  removeEventListener(type: string, listener: EventListener) { Events.listeners.get(type)?.delete(listener); }
  close() {}
  static emit(type: string, reason = "progress") { for (const listener of Events.listeners.get(type) ?? []) listener(new dom.window.MessageEvent(type, { data: JSON.stringify({ key: "f42", reason }) })); }
}
Object.defineProperty(globalThis, "EventSource", { value: Events, configurable: true });

const { cleanup, fireEvent, render, screen, waitFor, within } = await import("@testing-library/react");
const userEventModule = await import("@testing-library/user-event");
const userEvent = userEventModule.default ?? userEventModule;
const { MemoryRouter } = await import("react-router-dom");
const { default: ModelsPage } = await import("../src/pages/ModelsPage.tsx");

const GiB = 1024 ** 3;
let downloadedGiB = 1;
let modelListRequests = 0;
let detailRequests = 0;
let presenceRequests = 0;
let downloading = true;
let visible = true;
let heldPresence: ((response: Response) => void) | null = null;
let holdPresence = false;
const patches: unknown[] = [];

// Every list request answers with freshly parsed objects, as a real HTTP
// response does, while the download keeps advancing.
function modelList() {
  return JSON.parse(JSON.stringify([{
    id: 9, source: "modelscope", source_id: "Qwen/Qwen3-8B", name: "Qwen3-8B", created_at: "2026-10-01T00:00:00Z",
    operations: [{ type: "openai.chat" }], input_modalities: ["text"],
    nodes: [{
      node_uuid: "n1", node_name: "gpu3", state: downloading ? "downloading" : "present", path: "/data/models/qwen",
      progress: downloadedGiB / 4, size_bytes: 4 * GiB, downloaded: downloadedGiB * GiB,
      transferred_bytes: 0, rate_bps: 1024, acquisition: "download",
    }],
  }]));
}

globalThis.fetch = (async (input: RequestInfo | URL, init?: RequestInit) => {
  const url = new URL(String(input), "http://localhost");
  if (url.pathname === "/api/v1/models/presence") {
    presenceRequests += 1;
    assert.equal(url.searchParams.get("ids"), "9", "only visible model IDs are read in one batch");
    if (holdPresence) return new Promise<Response>((resolve) => { heldPresence = resolve; });
    return Response.json({ 9: { ready_nodes: downloading ? 0 : 1, cached_nodes: 0, ready_storages: 0, in_flight: downloading ? 1 : 0, in_flight_percent: downloadedGiB / 4 * 100, failed: 0 } });
  }
  if (url.pathname === "/api/v1/models") {
    modelListRequests += 1;
    // The list is paged and its rows carry no per-node presence; the open
    // dialog reads the model itself.
    const { nodes: _nodes, ...row } = modelList()[0];
    return Response.json({ items: visible ? [{ ...row, presence: { ready_nodes: downloading ? 0 : 1, cached_nodes: 0, ready_storages: 0, in_flight: downloading ? 1 : 0, in_flight_percent: downloadedGiB / 4 * 100, failed: 0 } }] : [], page: 1, page_size: 20, total: visible ? 1 : 0, summary: { registered: 1, ready_files: 0, sources: 1, weight_gib: 0 } });
  }
  if (url.pathname === "/api/v1/models/9" && !init?.method) { detailRequests += 1; return Response.json(modelList()[0]); }
  if (url.pathname === "/api/v1/nodes") {
    return Response.json(url.searchParams.has("page")
      ? { items: [], page: Number(url.searchParams.get("page")), page_size: Number(url.searchParams.get("page_size")), total: 0, all: 0, labels: [] }
      : { items: [], total: 0 });
  }
  if (url.pathname === "/api/v1/models/9/inference-config" && init?.method === "PATCH") {
    patches.push(JSON.parse(String(init.body)));
    return Response.json(modelList()[0]);
  }
  if (url.pathname === "/api/v1/me") return Response.json({ role: "admin" });
  if (url.pathname === "/api/v1/storages") return Response.json({ items: [], total: 0 });
  throw new Error(`unexpected request: ${init?.method ?? "GET"} ${url.pathname}`);
}) as typeof fetch;

afterEach(() => {
  cleanup();
  downloadedGiB = 1;
  modelListRequests = 0;
  detailRequests = 0;
  presenceRequests = 0;
  downloading = true;
  visible = true;
  heldPresence = null;
  holdPresence = false;
  patches.length = 0;
  localStorage.removeItem("gpuworks-model-view");
});

async function pickOption(combobox: HTMLElement, label: string) {
  const user = userEvent.setup({ document: dom.window.document });
  fireEvent.mouseDown(combobox);
  await user.click(await screen.findByText(label, { selector: ".ant-select-item-option-content" }));
}

const selectedTags = (combobox: HTMLElement) =>
  [...(combobox.closest(".ant-select") as HTMLElement).querySelectorAll(".ant-select-selection-item-content")].map((el) => el.textContent);

test("a background model refresh during a download keeps unsaved capability and input modality edits", async () => {
  render(<I18nextProvider i18n={i18n}><MemoryRouter initialEntries={["/models?detail=9"]}><ModelsPage /></MemoryRouter></I18nextProvider>);

  const dialog = await screen.findByRole("dialog");
  const operations = await within(dialog).findByRole("combobox", { name: text.detail.operations });
  const modalities = within(dialog).getByRole("combobox", { name: text.detail.inputModalities });
  await waitFor(() => assert.deepEqual(selectedTags(operations), [common.operation.chat]));
  assert.deepEqual(selectedTags(modalities), [common.inputModality.text]);

  await pickOption(operations, common.operation.embeddings);
  await pickOption(modalities, common.inputModality.image);
  assert.deepEqual(selectedTags(operations), [common.operation.chat, common.operation.embeddings]);
  assert.deepEqual(selectedTags(modalities), [common.inputModality.text, common.inputModality.image]);

  // Progress rereads open detail and lightweight visible presence only.
  const before = detailRequests;
  const listBefore = modelListRequests;
  downloadedGiB = 2;
  Events.emit("modelfile");
  await waitFor(() => assert.ok(detailRequests > before, "the open model was re-requested"));
  await waitFor(() => assert.ok(within(dialog).getAllByText(/2\.0 GiB \/ 4\.0 GiB/).length > 0, "new progress rendered"));

  assert.equal(modelListRequests, listBefore, "progress must not re-request the full model list");
  assert.ok(presenceRequests > 0);
  assert.deepEqual(selectedTags(operations), [common.operation.chat, common.operation.embeddings], "capability edit survived the refresh");
  assert.deepEqual(selectedTags(modalities), [common.inputModality.text, common.inputModality.image], "input modality edit survived the refresh");

  const user = userEvent.setup({ document: dom.window.document });
  await user.click(within(dialog).getByRole("button", { name: text.action.save }));
  await waitFor(() => assert.equal(patches.length, 1));
  assert.deepEqual(patches[0], {
    operations: [{ type: "openai.chat" }, { type: "openai.embeddings" }],
    input_modalities: ["text", "image"],
  });
});


test("download progress updates visible percentages without reloading; terminal and model changes invalidate the page", async () => {
  localStorage.setItem("gpuworks-model-view", "list");
  render(<I18nextProvider i18n={i18n}><MemoryRouter initialEntries={["/models"]}><ModelsPage /></MemoryRouter></I18nextProvider>);
  await screen.findByText("Qwen3-8B");
  const before = modelListRequests;
  for (const value of [2, 3]) {
    downloadedGiB = value;
    const progressBefore = presenceRequests;
    Events.emit("modelfile");
    Events.emit("modelfile");
    await waitFor(() => assert.ok(presenceRequests > progressBefore));
    await screen.findByText(i18n.t("models:presenceSummary.inFlight", { count: 1, percent: value / 4 * 100 }));
    assert.equal(modelListRequests, before, "progress keeps the original list response");
  }
  assert.equal(presenceRequests, 2, "duplicate events are coalesced into one lightweight read");
  downloading = false;
  Events.emit("modelfile", "present");
  await waitFor(() => assert.equal(modelListRequests, before + 1, "terminal event reloads the list once"));
  await screen.findByRole("button", { name: i18n.t("models:presenceSummary.nodes", { count: 1 }) });
  Events.emit("model", "updated");
  await waitFor(() => assert.equal(modelListRequests, before + 2, "model metadata event reloads once"));
  visible = false;
  Events.emit("model", "deleted");
  await waitFor(() => assert.ok(screen.queryByText("Qwen3-8B") === null, "deleted model disappears"));
  visible = true;
  Events.emit("model", "created");
  await screen.findByText("Qwen3-8B");
});


test("a late progress response cannot restore a transfer after its terminal state refreshed the list", async () => {
  localStorage.setItem("gpuworks-model-view", "list");
  render(<I18nextProvider i18n={i18n}><MemoryRouter initialEntries={["/models"]}><ModelsPage /></MemoryRouter></I18nextProvider>);
  await screen.findByText("Qwen3-8B");
  holdPresence = true;
  Events.emit("modelfile");
  await waitFor(() => assert.ok(heldPresence));
  downloading = false;
  Events.emit("modelfile", "present");
  await screen.findByRole("button", { name: i18n.t("models:presenceSummary.nodes", { count: 1 }) });
  const resolve = heldPresence!;
  resolve(Response.json({ 9: { ready_nodes: 0, cached_nodes: 0, ready_storages: 0, in_flight: 1, in_flight_percent: 40, failed: 0 } }));
  await new Promise((resolve) => setTimeout(resolve, 0));
  assert.ok(screen.getByRole("button", { name: i18n.t("models:presenceSummary.nodes", { count: 1 }) }));
  assert.ok(screen.queryByText(i18n.t("models:presenceSummary.inFlight", { count: 1, percent: 40 })) === null, "late progress cannot restore a finished transfer");
});
