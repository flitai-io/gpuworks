import assert from "node:assert/strict";
import { afterEach, beforeEach, test } from "node:test";
import { createI18n } from "./helpers/dom.ts";
import React from "react";
import { installFleet } from "./helpers/fakeFleet.ts";
import { I18nextProvider } from "react-i18next";
import { list } from "./helpers/listPage.ts";

const { i18n, locale } = await createI18n(["models"]);
const models = locale.models;
const { cleanup, fireEvent, render, screen, waitFor } = await import("@testing-library/react");
const userEventModule = await import("@testing-library/user-event");
const userEvent = userEventModule.default ?? userEventModule;
const apiModule = await import("../src/api.ts");
const { ModelCopyModal } = await import("../src/pages/models/ModelCopyModal.tsx");

const api = apiModule.api;
const original = { storages: api.storages, copyModel: api.copyModel, nodesPage: api.nodesPage, nodes: api.nodes };
// The dialogs search nodes on the server (NodePicker); the fake answers as GET /nodes?page=… does,
// and refuses a request for the whole fleet.
afterEach(() => { cleanup(); Object.assign(api, original) });

function row(over: Record<string, unknown>) {
  return { node_uuid: "n1", node_name: "gpu3", state: "present", path: "/data/m", progress: 1, size_bytes: 1, downloaded: 1, transferred_bytes: 0, rate_bps: 0, acquisition: "download", storage_id: 1, storage_name: "default", storage_type: "local", ...over };
}
function shared(over: Record<string, unknown>) {
  return { storage_id: 3, storage_name: "archive", storage_type: "nfs", path: "Qwen/Qwen3-8B", state: "present", acquisition: "registered", size_bytes: 1, downloaded: 1, progress: 1, rate_bps: 0, ready_nodes: 3, cached_nodes: 0, writable: true, ...over };
}
function storage(over: Record<string, unknown>) {
  return { id: 1, name: "default", type: "local", enabled: true, is_default: true, all_nodes: true, path: "/data", targets: [], nodes: { total: 3, ready: 3, problems: [] }, writable: true, model_count: 1, deployment_ids: [], ...over };
}
const storageList = [
  storage({}),
  storage({ id: 2, name: "nvme", is_default: false, enabled: false }),
  storage({ id: 3, name: "archive", type: "nfs", is_default: false }),
  storage({ id: 4, name: "team-cache", type: "nfs_cache", is_default: false }),
];
const model = {
  id: 9, source: "huggingface", source_id: "Qwen/Qwen3-8B", name: "Qwen3-8B",
  nodes: [
    row({}),
    row({ node_uuid: "n2", node_name: "node-01" }),
    row({ node_uuid: "n5", node_name: "node-05", state: "syncing" }),
    row({ node_uuid: "n1", node_name: "gpu3", storage_id: 4, storage_name: "team-cache", storage_type: "nfs_cache", cache: true }),
  ],
  storages: [shared({}), shared({ storage_id: 4, storage_name: "team-cache", storage_type: "nfs_cache", state: "missing" })],
} as never;
const nodes = [
  { uuid: "n1", hostname: "gpu3", state: "online", gpu_count: 8 },
  { uuid: "n2", hostname: "node-01", state: "online", gpu_count: 4 },
  { uuid: "n3", hostname: "cloud-01", state: "online", gpu_count: 1 },
  { uuid: "n4", hostname: "off-01", state: "offline", gpu_count: 1 },
] as never[];

function stub(rows: unknown[] = storageList) {
  api.storages = (async () => list(rows)) as never;
  const calls: unknown[][] = [];
  api.copyModel = (async (...args: unknown[]) => { calls.push(args); return { state: "syncing" }; }) as never;
  return calls;
}
function renderModal(m: unknown = model, onDone = () => {}) {
  return render(<I18nextProvider i18n={i18n}><ModelCopyModal model={m as never} onClose={() => {}} onDone={onDone} /></I18nextProvider>);
}
async function pick(name: string, option: string | RegExp) {
  fireEvent.mouseDown(await screen.findByRole("combobox", { name }));
  fireEvent.click(await screen.findByText(option));
}
function visibleOptions() {
  // Closed dropdowns stay in the DOM hidden; only the open one counts.
  return Array.from(document.querySelectorAll(".ant-select-dropdown:not(.ant-select-dropdown-hidden) .ant-select-item-option")) as HTMLElement[];
}

let fleet: ReturnType<typeof installFleet>;
beforeEach(() => { fleet = installFleet(api as never, nodes as never); });

test("copying between two local storages sends source and target locations and omits an unset rate", async () => {
  const calls = stub();
  let done = 0;
  renderModal(model, () => { done += 1; });
  // The first source (storage default on gpu3) and the default target storage are preselected.
  await screen.findByRole("combobox", { name: models.copy.targetNode });
  assert.ok(await screen.findByText("default · gpu3"));
  fireEvent.mouseDown(screen.getByRole("combobox", { name: models.copy.targetNode }));
  // The picker asks the server for nodes with a live agent: off-01 (offline) is never offered.
  await waitFor(() => assert.ok(fleet.pages.some((query) => JSON.stringify(query.state) === JSON.stringify(["online"]))));
  // gpu3 is the source and node-01 already holds the files: listed, but not choosable.
  await waitFor(() => assert.deepEqual(visibleOptions().map((o) => o.textContent), ["gpu3", "node-01", "cloud-01"]));
  assert.deepEqual(visibleOptions().map((o) => o.className.includes("ant-select-item-option-disabled")), [true, true, false]);
  fireEvent.click(await screen.findByText("cloud-01"));
  assert.ok(screen.getByText(models.copy.wayDirect));
  fireEvent.click(screen.getByRole("button", { name: models.copy.start }));
  await waitFor(() => assert.equal(calls.length, 1));
  assert.deepEqual(calls[0], [9, { source: { storage_id: 1, node_uuid: "n1" }, target: { storage_id: 1, node_uuid: "n3" } }]);
  await waitFor(() => assert.equal(done, 1));
});

test("the sources are the finished node files and shared files, never caches or transfers in progress", async () => {
  stub();
  renderModal();
  fireEvent.mouseDown(await screen.findByRole("combobox", { name: models.copy.source }));
  assert.deepEqual(visibleOptions().map((o) => o.textContent), ["default · gpu3", "default · node-01", "archive"]);
});

test("a stopped storage cannot be the target and the source's own shared storage is excluded", async () => {
  stub();
  renderModal();
  await pick(models.copy.source, "archive");
  fireEvent.mouseDown(await screen.findByRole("combobox", { name: models.copy.targetStorage }));
  const options = visibleOptions();
  const byText = (text: string) => options.find((o) => o.textContent?.startsWith(`${text} · `)) as HTMLElement;
  assert.ok(byText("nvme").className.includes("ant-select-item-option-disabled"));
  assert.ok(byText("nvme").textContent?.includes(models.copy.readOnly), "read-only is spelled out");
  assert.ok(byText("archive").className.includes("ant-select-item-option-disabled"));
  assert.ok(byText("archive").textContent?.includes(models.copy.sameAsSource));
  assert.ok(!byText("team-cache").className.includes("disabled"));
});

test("copying from a shared storage into another shared storage needs no node and converts the rate to KiB/s", async () => {
  const calls = stub();
  renderModal();
  await pick(models.copy.source, "archive");
  await pick(models.copy.targetStorage, /team-cache/);
  assert.equal(screen.queryByRole("combobox", { name: models.copy.targetNode }), null, "a shared target has no node");
  assert.ok(screen.getByText(models.copy.wayServerPicks));
  fireEvent.change(screen.getByRole("spinbutton", { name: models.copy.rateLimit }), { target: { value: "2.5" } });
  fireEvent.click(screen.getByRole("button", { name: models.copy.start }));
  await waitFor(() => assert.equal(calls.length, 1));
  assert.deepEqual(calls[0], [9, { source: { storage_id: 3 }, target: { storage_id: 4 }, rate_limit_kib_s: 2560 }]);
});

test("the start button stays disabled until a target node is chosen, and works from the keyboard", async () => {
  const calls = stub();
  renderModal();
  const combo = await screen.findByRole("combobox", { name: models.copy.targetNode });
  assert.equal((screen.getByRole("button", { name: models.copy.start }) as HTMLButtonElement).disabled, true);
  combo.focus();
  fireEvent.keyDown(combo, { key: "ArrowDown", code: "ArrowDown", keyCode: 40 });
  fireEvent.keyDown(combo, { key: "Enter", code: "Enter", keyCode: 13 });
  await waitFor(() => assert.equal((screen.getByRole("button", { name: models.copy.start }) as HTMLButtonElement).disabled, false));
  screen.getByRole("button", { name: models.copy.start }).focus();
  await userEvent.keyboard("{Enter}");
  await waitFor(() => assert.equal(calls.length, 1));
  assert.equal((calls[0][1] as { target: { node_uuid: string } }).target.node_uuid, "n3");
});

test("a model without finished files, a failing start and a failing storage list are each reported", async () => {
  stub();
  const empty = renderModal({ id: 9, source: "huggingface", source_id: "x", name: "x", nodes: [row({ state: "failed" })] });
  assert.ok(await screen.findByText(models.copy.noSources));
  empty.unmount();

  api.storages = (async () => list(storageList)) as never;
  api.copyModel = (async () => { throw new Error("NO_STORAGE_PATH"); }) as never;
  const view = renderModal();
  await pick(models.copy.targetNode, "cloud-01");
  fireEvent.click(screen.getByRole("button", { name: models.copy.start }));
  assert.ok(await screen.findByText(/Copy failed: NO_STORAGE_PATH/));
  view.unmount();

  api.storages = (async () => { throw new Error("offline"); }) as never;
  renderModal();
  assert.ok(await screen.findByText(/Failed to load storages: offline/));
});

test("while the storages load the dialog shows a spinner and cannot be submitted", async () => {
  api.storages = (() => new Promise(() => {})) as never;
  renderModal();
  assert.ok(document.querySelector(".ant-spin"));
  assert.equal((screen.getByRole("button", { name: models.copy.start }) as HTMLButtonElement).disabled, true);
});


test("an explicit copy can retry a disk-space target without changing either location", async () => {
  const calls = stub();
  renderModal({
    id: 9, source: "huggingface", source_id: "Qwen/Qwen3-8B", name: "Qwen3-8B",
    nodes: [row({}), row({ node_uuid: "n2", node_name: "node-01", state: "insufficient_disk_space", downloaded: 0, progress: 0, message: "insufficient disk space at /data" })],
    storages: [],
  });
  fireEvent.mouseDown(await screen.findByRole("combobox", { name: models.copy.targetNode }));
  await waitFor(() => assert.deepEqual(visibleOptions().map((option) => option.textContent), ["gpu3", "node-01", "cloud-01"]));
  const target = visibleOptions().find((option) => option.textContent === "node-01")!;
  assert.equal(target.className.includes("ant-select-item-option-disabled"), false, "a disk-space failure must not count as an existing usable copy");
  fireEvent.click(target);
  fireEvent.click(screen.getByRole("button", { name: models.copy.start }));
  await waitFor(() => assert.equal(calls.length, 1));
  assert.deepEqual(calls[0], [9, { source: { storage_id: 1, node_uuid: "n1" }, target: { storage_id: 1, node_uuid: "n2" } }]);
});
