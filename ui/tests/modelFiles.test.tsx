import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import { createI18n } from "./helpers/dom.ts";
import React from "react";
import { I18nextProvider } from "react-i18next";

const { i18n, locale } = await createI18n(["models", "storage"]);
const models = locale.models;
const { cleanup, fireEvent, render, screen, waitFor, within } = await import("@testing-library/react");
const userEventModule = await import("@testing-library/user-event");
const userEvent = userEventModule.default ?? userEventModule;
const apiModule = await import("../src/api.ts");
const { ModelFilesSection } = await import("../src/pages/models/ModelFilesSection.tsx");
const { ModelDetailModal } = await import("../src/pages/ModelsPageParts.tsx");

const api = apiModule.api;
const original = { me: api.me, deleteModelOnNode: api.deleteModelOnNode, syncModel: api.syncModel, downloadModel: api.downloadModel, downloadModelToStorage: api.downloadModelToStorage, deleteModelInStorage: api.deleteModelInStorage };
afterEach(() => { cleanup(); Object.assign(api, original); });

const fill = (template: string, values: Record<string, string | number>) => template.replace(/\{\{(\w+)\}\}/g, (_, key) => String(values[key]));
const GiB = 1024 ** 3;

function row(over: Record<string, unknown>) {
  return { node_uuid: "n1", node_name: "gpu3", state: "present", path: "/data/models/qwen", progress: 1, size_bytes: 15 * GiB, downloaded: 15 * GiB, transferred_bytes: 0, rate_bps: 0, acquisition: "download", deletable: true, ...over };
}
function shared(over: Record<string, unknown>) {
  return { storage_id: 3, storage_name: "archive", storage_type: "nfs", path: "Qwen/Qwen3-8B", state: "present", acquisition: "registered", size_bytes: 15 * GiB, downloaded: 15 * GiB, progress: 1, rate_bps: 0, ready_nodes: 128, cached_nodes: 0, writable: true, ...over };
}
function modelWith(over: Record<string, unknown>) {
  return { id: 9, source: "modelscope", source_id: "Qwen/Qwen3-8B", name: "Qwen3-8B", ...over } as never;
}
function stubMe(role: string) { api.me = (async () => ({ role })) as never; }
function renderSection(model: unknown, onChanged = () => {}) {
  return render(<I18nextProvider i18n={i18n}><ModelFilesSection model={model as never} onChanged={onChanged} /></I18nextProvider>);
}
function expandFile(name: string) {
  const row = screen.getByText(name, { exact: true }).closest("tr")!;
  fireEvent.click(within(row).getByRole("button", { name: locale.storage.detail.expand }));
  return within(row.nextElementSibling as HTMLElement);
}

test("one table compares copies while keeping each location's details on its own row", () => {
  stubMe("admin");
  renderSection(modelWith({
    nodes: [
      row({ storage_id: 1, storage_name: "default", storage_type: "local", last_used_at: new Date(2026, 8, 30, 8, 30).toISOString() }),
      row({ node_uuid: "n2", node_name: "legacy", path: "/old/path" }),
      row({ node_uuid: "n1", node_name: "cached-node", storage_id: 4, storage_name: "team-cache", storage_type: "nfs_cache", cache: true }),
    ],
    storages: [shared({}), shared({ storage_id: 4, storage_name: "team-cache", storage_type: "nfs_cache", ready_nodes: 30, cached_nodes: 1, writer_node: "gpu3" })],
  }));
  assert.equal(screen.getAllByRole("table").length, 1);
  assert.equal(document.querySelectorAll("tr[data-row-key]").length, 5);
  assert.equal(screen.queryByText("/old/path"), null);
  assert.ok(screen.getByText(models.files.cache));
  const local = expandFile("gpu3");
  assert.ok(local.getByText("2026-09-30 08:30"));
  assert.ok(local.getByText("/data/models/qwen"));
  const legacy = expandFile("legacy");
  assert.ok(legacy.getByText("/old/path"));
  assert.ok(legacy.getByText(models.files.neverUsed));
  const sharedFile = expandFile("archive");
  assert.ok(sharedFile.getByText("Qwen/Qwen3-8B"));
  assert.equal(sharedFile.getByText(models.files.readyNodes).nextElementSibling?.textContent, "128");
  assert.equal(sharedFile.queryByText(models.files.cachedNodes), null);
  assert.equal(sharedFile.queryByText(models.files.writerNodeLabel), null);
  const cached = expandFile("team-cache");
  assert.equal(cached.getByText(models.files.readyNodes).nextElementSibling?.textContent, "30");
  assert.equal(cached.getByText(models.files.cachedNodes).nextElementSibling?.textContent, "1");
  assert.equal(cached.getByText(models.files.writerNodeLabel).nextElementSibling?.textContent, "gpu3");
});

test("a file the storage no longer holds is shown as missing with the hint to scan again", () => {
  stubMe("admin");
  renderSection(modelWith({ storages: [shared({ state: "missing", ready_nodes: 0 })] }));
  const state = screen.getByText(models.presence.missing);
  assert.ok(state.className.includes("ant-tag-red"), "missing is red");
  assert.ok(expandFile("archive").getByText(models.files.missingHint));
});

test("a real disk-space failure exposes the original path and capacity detail", () => {
  stubMe("admin");
  const detail = "insufficient disk space at /data/models: available=1048576 required=2147483648 (download=1073741824 safety_margin=1073741824)";
  renderSection(modelWith({ nodes: [row({ state: "insufficient_disk_space", message: detail, size_bytes: GiB, downloaded: 0, progress: 0 })] }));
  const details = expandFile("gpu3");
  assert.ok(details.getByText("/data/models/qwen"));
  assert.equal(details.queryByText(detail)?.textContent, detail, "the disk failure detail must remain visible");
  assert.ok(screen.getByText("Insufficient disk space"));
  assert.equal(screen.queryByText("insufficient_disk_space"), null);
});

test("a shared-storage disk-space failure has a Chinese label and preserves its detail", async () => {
  stubMe("admin");
  const translated = await createI18n(["models", "storage"], "zh");
  const detail = "insufficient disk space at /data/models: available=1048576 required=2147483648";
  render(<I18nextProvider i18n={translated.i18n}><ModelFilesSection model={modelWith({ storages: [shared({ state: "insufficient_disk_space", message: detail, size_bytes: GiB, downloaded: 0, progress: 0 })] })} onChanged={() => {}} /></I18nextProvider>);
  const fileRow = screen.getByText("archive", { exact: true }).closest("tr")!;
  fireEvent.click(within(fileRow).getByRole("button", { name: translated.locale.storage.detail.expand }));
  assert.ok(within(fileRow.nextElementSibling as HTMLElement).getByText("Qwen/Qwen3-8B"));
  assert.ok(screen.getByText("磁盘空间不足"));
  assert.ok(screen.getByText(detail));
  assert.equal(screen.queryByText("insufficient_disk_space"), null);
});

test("only admins get a delete, for shared storage files and for a node's files", async () => {
  stubMe("viewer");
  renderSection(modelWith({ nodes: [row({ storage_id: 1, storage_name: "default", storage_type: "local" })], storages: [shared({})] }));
  await new Promise((resolve) => setTimeout(resolve, 20));
  assert.equal(screen.queryByRole("button", { name: models.action.deleteModelFiles }), null);
  assert.equal(screen.queryByRole("button", { name: models.files.deleteOnNode }), null);
});

test("an admin cannot delete a node's files the server says are not deletable", async () => {
  stubMe("admin");
  renderSection(modelWith({
    nodes: [
      // A read-only (disabled) local storage keeps its files.
      row({ storage_id: 1, storage_name: "default", storage_type: "local", deletable: false }),
      // A node cache stays deletable even when its nfs_cache storage is read-only.
      row({ node_uuid: "n2", node_name: "node-01", storage_id: 4, storage_name: "team-cache", storage_type: "nfs_cache", cache: true, deletable: true }),
    ],
    storages: [shared({ storage_id: 4, storage_name: "team-cache", storage_type: "nfs_cache", writable: false })],
  }));
  const buttons = await screen.findAllByRole("button", { name: models.files.deleteOnNode });
  assert.equal(buttons.length, 2);
  const [local, cache] = buttons as HTMLButtonElement[];
  assert.equal(local.disabled, true, "the read-only local storage keeps its files");
  assert.equal(cache.disabled, false, "a node's cache is evictable whatever the storage's setting");
  fireEvent.click(local);
  assert.equal(screen.queryByText(fill(models.files.deleteOnNodeConfirm, { node: "gpu3" })), null, "no confirmation for files that cannot be deleted");
});

test("deleting a node's files asks first and passes the storage; legacy rows pass none", async () => {
  stubMe("admin");
  const calls: unknown[][] = [];
  api.deleteModelOnNode = (async (...args: unknown[]) => { calls.push(args); }) as never;
  let changed = 0;
  renderSection(modelWith({ nodes: [
    row({ storage_id: 1, storage_name: "default", storage_type: "local" }),
    row({ node_uuid: "n2", node_name: "node-01" }),
  ] }), () => { changed += 1; });
  const [first, second] = await screen.findAllByRole("button", { name: models.files.deleteOnNode });
  fireEvent.click(first);
  assert.ok(await screen.findByText(fill(models.files.deleteOnNodeConfirm, { node: "gpu3" })));
  assert.equal(calls.length, 0, "nothing is deleted before confirming");
  fireEvent.click(screen.getByRole("button", { name: models.action.delete }));
  await waitFor(() => assert.equal(calls.length, 1));
  assert.deepEqual(calls[0], [9, "n1", 1]);
  await waitFor(() => assert.equal(changed, 1));
  fireEvent.click(second);
  fireEvent.click(await screen.findByText(fill(models.files.deleteOnNodeConfirm, { node: "node-01" })).then(() => screen.getAllByRole("button", { name: models.action.delete }).at(-1)!));
  await waitFor(() => assert.equal(calls.length, 2));
  assert.deepEqual(calls[1], [9, "n2", undefined]);
});

test("a failed delete reports the server's message and a failed row keeps its retry", async () => {
  stubMe("admin");
  api.deleteModelOnNode = (async () => { throw new Error("MODEL_IN_USE"); }) as never;
  const syncs: unknown[][] = [];
  api.syncModel = (async (...args: unknown[]) => { syncs.push(args); return { state: "syncing" }; }) as never;
  renderSection(modelWith({ nodes: [row({ state: "failed", message: "disk_full", acquisition: "peer", source_node_uuid: "n9", last_rate_limit_kib_s: 512, storage_id: 1, storage_name: "default", storage_type: "local" })] }));
  assert.equal(screen.queryByText("disk_full"), null);
  assert.ok(expandFile("gpu3").getByText("disk_full"));
  fireEvent.click(screen.getByRole("button", { name: models.action.retry }));
  await waitFor(() => assert.equal(syncs.length, 1));
  assert.deepEqual(syncs[0], [9, "n1", "n9", 512]);
  fireEvent.click(await screen.findByRole("button", { name: models.files.deleteOnNode }));
  fireEvent.click(await screen.findByRole("button", { name: models.action.delete }));
  assert.ok(await screen.findByText("MODEL_IN_USE"), "the error message is shown");
});

test("retrying a failed public download keeps its non-default storage and exact node", async () => {
  stubMe("admin");
  const downloads: unknown[][] = [];
  const legacy: unknown[][] = [];
  const syncs: unknown[][] = [];
  api.downloadModelToStorage = (async (...args: unknown[]) => { downloads.push(args); }) as never;
  api.downloadModel = (async (...args: unknown[]) => { legacy.push(args); }) as never;
  api.syncModel = (async (...args: unknown[]) => { syncs.push(args); return { state: "syncing" }; }) as never;
  let changed = 0;
  renderSection(modelWith({ nodes: [row({ state: "failed", node_uuid: "n-selected", node_name: "selected-node", storage_id: 7, storage_name: "nvme", storage_type: "local", message: "download failed" })] }), () => { changed += 1; });
  assert.ok(expandFile("selected-node").getByText("download failed"));
  fireEvent.click(screen.getByRole("button", { name: models.action.retry }));
  await waitFor(() => assert.equal(downloads.length + legacy.length + syncs.length, 1));
  assert.deepEqual(downloads, [[9, { storage_id: 7, node_uuids: ["n-selected"] }]]);
  assert.deepEqual(legacy, [], "an explicit storage must not use the default-storage endpoint");
  assert.deepEqual(syncs, [], "public Retry must not copy from a peer");
  await waitFor(() => assert.equal(changed, 1));
});

test("retrying a legacy public download without a storage uses the node download endpoint", async () => {
  stubMe("admin");
  const downloads: unknown[][] = [];
  const storages: unknown[][] = [];
  const syncs: unknown[][] = [];
  api.downloadModel = (async (...args: unknown[]) => { downloads.push(args); }) as never;
  api.downloadModelToStorage = (async (...args: unknown[]) => { storages.push(args); }) as never;
  api.syncModel = (async (...args: unknown[]) => { syncs.push(args); return { state: "syncing" }; }) as never;
  let changed = 0;
  renderSection(modelWith({ nodes: [row({ state: "failed", node_uuid: "n-legacy" })] }), () => { changed += 1; });
  fireEvent.click(screen.getByRole("button", { name: models.action.retry }));
  await waitFor(() => assert.equal(downloads.length + storages.length + syncs.length, 1));
  assert.deepEqual(downloads, [[9, "n-legacy"]]);
  assert.deepEqual(storages, []);
  assert.deepEqual(syncs, []);
  await waitFor(() => assert.equal(changed, 1));
});

test("a failed public retry shows the server error without reporting changed files", async () => {
  stubMe("admin");
  const downloads: unknown[][] = [];
  const syncs: unknown[][] = [];
  api.downloadModelToStorage = (async (...args: unknown[]) => { downloads.push(args); throw new Error("PUBLIC_DOWNLOAD_REJECTED"); }) as never;
  api.syncModel = (async (...args: unknown[]) => { syncs.push(args); return { state: "syncing" }; }) as never;
  let changed = 0;
  renderSection(modelWith({ nodes: [row({ state: "failed", storage_id: 7 })] }), () => { changed += 1; });
  fireEvent.click(screen.getByRole("button", { name: models.action.retry }));
  assert.ok(await screen.findByText("PUBLIC_DOWNLOAD_REJECTED"));
  assert.deepEqual(downloads, [[9, { storage_id: 7, node_uuids: ["n1"] }]]);
  assert.deepEqual(syncs, []);
  assert.equal(changed, 0);
  assert.ok(screen.getByRole("button", { name: models.action.retry }), "retry remains available after the rejection");
});

test("cached download rows preserve the existing peer source and rate retry", async () => {
  stubMe("admin");
  const syncs: unknown[][] = [];
  const downloads: unknown[][] = [];
  const legacy: unknown[][] = [];
  api.syncModel = (async (...args: unknown[]) => { syncs.push(args); return { state: "syncing" }; }) as never;
  api.downloadModelToStorage = (async (...args: unknown[]) => { downloads.push(args); }) as never;
  api.downloadModel = (async (...args: unknown[]) => { legacy.push(args); }) as never;
  let changed = 0;
  renderSection(modelWith({ nodes: [row({ state: "failed", cache: true, storage_id: 4, storage_name: "team-cache", storage_type: "nfs_cache", source_node_uuid: "n-origin", last_rate_limit_kib_s: 256 })] }), () => { changed += 1; });
  fireEvent.click(screen.getByRole("button", { name: models.action.retry }));
  await waitFor(() => assert.equal(syncs.length, 1));
  assert.deepEqual(syncs, [[9, "n1", "n-origin", 256]]);
  assert.deepEqual(downloads, []);
  assert.deepEqual(legacy, []);
  await waitFor(() => assert.equal(changed, 1));
});

test("a rejected peer retry preserves source and rate and does not report changed files", async () => {
  stubMe("admin");
  const syncs: unknown[][] = [];
  const downloads: unknown[][] = [];
  const legacy: unknown[][] = [];
  api.syncModel = (async (...args: unknown[]) => { syncs.push(args); throw new Error("PEER_RETRY_REJECTED"); }) as never;
  api.downloadModelToStorage = (async (...args: unknown[]) => { downloads.push(args); }) as never;
  api.downloadModel = (async (...args: unknown[]) => { legacy.push(args); }) as never;
  let changed = 0;
  renderSection(modelWith({ nodes: [row({ state: "failed", acquisition: "peer", storage_id: 7, source_node_uuid: "n-origin", last_rate_limit_kib_s: 512 })] }), () => { changed += 1; });
  fireEvent.click(screen.getByRole("button", { name: models.action.retry }));
  assert.ok(await screen.findByText("PEER_RETRY_REJECTED"));
  assert.deepEqual(syncs, [[9, "n1", "n-origin", 512]]);
  assert.deepEqual(downloads, []);
  assert.deepEqual(legacy, []);
  assert.equal(changed, 0);
});

test("a model without any file shows the empty state", () => {
  stubMe("admin");
  renderSection(modelWith({}));
  assert.ok(screen.getByText(models.files.empty));
});

test("the detail dialog carries the model files and the copy action", async () => {
  stubMe("admin");
  const copied: unknown[] = [];
  render(<I18nextProvider i18n={i18n}><ModelDetailModal
    model={modelWith({ nodes: [], storages: [shared({})], weight_gib: 15 })}
    onClose={() => {}} onDownload={() => {}} onCopy={(m: unknown) => copied.push(m)} onRegisterPath={() => {}} onChanged={() => {}}
  /></I18nextProvider>);
  assert.ok(expandFile("archive").getByText("128"));
  const copy = screen.getByRole("button", { name: new RegExp(models.action.copyTo) });
  copy.focus();
  await userEvent.keyboard("{Enter}");
  assert.equal(copied.length, 1);
});

test("a transfer in flight shows its rate next to the bytes; a finished file shows none", () => {
  stubMe("viewer");
  renderSection(modelWith({ nodes: [
    row({ state: "downloading", progress: 0.5, downloaded: 7.5 * GiB, rate_bps: 48 * 1024 ** 2 }),
    row({ node_uuid: "n2", node_name: "node-01", state: "syncing", progress: 0.25, downloaded: 3.75 * GiB, rate_bps: 1.5 * GiB }),
    row({ node_uuid: "n3", node_name: "node-02", rate_bps: 9 * 1024 ** 2 }),
  ], storages: [shared({ state: "downloading", progress: 0.1, downloaded: 1.5 * GiB, rate_bps: 512 * 1024 })] }));
  assert.ok(screen.getByText("7.5 GiB / 15.0 GiB · 48.0 MiB/s"));
  assert.ok(screen.getByText("3.8 GiB / 15.0 GiB · 1.5 GiB/s"));
  assert.ok(screen.getByText("1.5 GiB / 15.0 GiB · 512.0 KiB/s"), "a shared storage transfer shows its rate too");
  assert.equal(screen.queryByText(/9\.0 MiB\/s/), null, "a present file reports no rate");
});

test("only a finished transfer shows Completed, including shared downloads and node caches", () => {
  stubMe("viewer");
  renderSection(modelWith({
    nodes: [row({}), row({ node_uuid: "n2", node_name: "cache-node", cache: true, acquisition: "copy" })],
    storages: [
      shared({ acquisition: "download" }),
      shared({ storage_id: 4, storage_name: "verifying", acquisition: "download", state: "downloading", progress: 1 }),
      shared({ storage_id: 5, storage_name: "registered" }),
    ],
  }));
  const file = (name: string) => within(screen.getByText(name, { exact: true }).closest("tr")!);
  for (const name of ["gpu3", "cache-node", "archive"]) {
    assert.ok(file(name).getByText(models.files.completed));
    assert.equal(file(name).getByRole("progressbar").getAttribute("aria-valuenow"), "100");
  }
  assert.ok(file("verifying").getByText("100%"));
  assert.equal(file("verifying").queryByText(models.files.completed), null);
  assert.equal(file("registered").queryByText(models.files.completed), null);
});

test("the model ID and its three specs stay together without repeating the fields in the detail grid", () => {
  stubMe("viewer");
  const props = { onClose() {}, onDownload() {}, onCopy() {}, onRegisterPath() {}, onChanged() {} };
  const detail = (specs: Record<string, unknown>) => <I18nextProvider i18n={i18n}>
    <ModelDetailModal model={modelWith(specs)} {...props} />
  </I18nextProvider>;
  const view = render(detail({ context_length: 262144, weight_gib: 1.6, precision: "bfloat16" }));
  const dialog = within(screen.getByRole("dialog", { name: models.detail.title }));
  assert.ok(dialog.getByRole("heading", { name: "Qwen3-8B" }));
  assert.equal(screen.getByTitle(models.column.sourceID).textContent, "Qwen/Qwen3-8B");
  const labels = [models.detail.contextLength, models.column.weight, models.detail.precision];
  for (const [index, value] of [(262144).toLocaleString(), "1.6 GiB", "bfloat16"].entries()) {
    const tag = dialog.getByText(labels[index]);
    assert.ok(within(tag).getByText(value));
    assert.equal(screen.getAllByText(labels[index]).length, 1);
  }
  view.rerender(detail({ name: "Qwen/Qwen3-8B" }));
  const name = dialog.getByRole("heading", { name: "Qwen/Qwen3-8B" });
  const sourceID = dialog.getByTitle(models.column.sourceID);
  assert.equal(sourceID.textContent, name.textContent);
  assert.notEqual(sourceID, name, "the source ID stays visible separately even when it matches the model name");
  assert.ok(within(dialog.getByText(models.detail.contextLength)).getByText(models.unrecognized));
  assert.ok(within(dialog.getByText(models.detail.precision)).getByText(models.unrecognized));
  assert.ok(within(dialog.getByText(models.column.weight)).getByText("—"));
  assert.equal(dialog.queryByRole("button", { name: models.detail.editSourceID }), null);
  view.rerender(detail({ source: "local", source_id_editable: true }));
  const editableID = within(dialog.getByTitle(models.column.sourceID));
  fireEvent.click(editableID.getByRole("button", { name: models.detail.editSourceID }));
  assert.equal((editableID.getByRole("textbox") as HTMLTextAreaElement).value, "Qwen/Qwen3-8B");
});

test("each file shows its own revision even when the requested branch or another node's revision differs", () => {
  stubMe("viewer");
  const props = { onClose() {}, onDownload() {}, onCopy() {}, onRegisterPath() {}, onChanged() {} };
  const nodes = [
    row({ revision: "a1b2c3d4e5f67890" }),
    row({ node_uuid: "n2", node_name: "gpu4", revision: "de987654" }),
    row({ node_uuid: "n3", node_name: "gpu5" }),
  ];
  render(<I18nextProvider i18n={i18n}>
    <ModelDetailModal model={modelWith({ revision: "main", nodes, storages: [shared({})] })} {...props} />
  </I18nextProvider>);
  assert.ok(within(screen.getByText(models.detail.requestedRevision).closest("table")!).getByText("main"));
  assert.equal(screen.queryByText(models.detail.nodePinnedRevision), null);
  assert.ok(expandFile("gpu3").getByText("a1b2c3d4e5f67890"));
  assert.ok(expandFile("gpu4").getByText("de987654"));
  assert.ok(expandFile("gpu5").getByText(models.detail.noResolvedRevision));
  assert.equal(expandFile("archive").queryByText(models.detail.nodePinnedRevision), null);
});

test("the detail dialog names the published and added times as the list does", () => {
  stubMe("viewer");
  // Dates render in the local time zone, so the fixtures are local noons.
  const localNoon = (year: number, month: number, day: number) => new Date(year, month - 1, day, 12).toISOString();
  render(<I18nextProvider i18n={i18n}><ModelDetailModal
    model={modelWith({ nodes: [], storages: [], origin_updated_at: localNoon(2026, 5, 2), created_at: localNoon(2026, 10, 1) })}
    onClose={() => {}} onDownload={() => {}} onCopy={() => {}} onRegisterPath={() => {}} onChanged={() => {}}
  /></I18nextProvider>);
  const label = (text: string) => screen.getByText(text).closest("th")!.nextElementSibling!.textContent;
  assert.equal(models.detail.originCreatedAt, models.column.publishedAt);
  assert.equal(models.detail.createdAt, models.column.addedAt);
  assert.equal(label(models.detail.originCreatedAt), "2026-05-02", "without a creation date the published time falls back to the last update, as the list does");
  assert.equal(label(models.detail.createdAt), "2026-10-01");
});

test("a transfer names where its bytes come from: the mirror a download uses, the node a copy comes from", () => {
  stubMe("viewer");
  renderSection(modelWith({ nodes: [
    row({ state: "downloading", progress: 0.5, downloaded: 7.5 * GiB, message: "downloading from hf-mirror.com" }),
    row({ node_uuid: "n2", node_name: "node-01", state: "syncing", acquisition: "peer", source_node_uuid: "n1", source_node_name: "gpu3" }),
    row({ node_uuid: "n3", node_name: "node-02", message: "" }),
  ] }));
  assert.ok(expandFile("gpu3").getByText(`${models.acquisition.download} · ${fill(models.files.fromHost, { host: "hf-mirror.com" })}`));
  assert.ok(expandFile("node-01").getByText(`${models.acquisition.peer} · ${fill(models.files.fromNode, { node: "gpu3" })}`));
  assert.ok(expandFile("node-02").getByText(models.acquisition.download), "nothing known beyond how it was acquired");
});

test("download cancellation names local and shared locations and refreshes after confirmation", async () => {
  stubMe("viewer");
  const originalCancel = api.cancelModelDownload;
  const calls: unknown[] = [];
  let changes = 0;
  api.cancelModelDownload = (async (id, body) => { calls.push([id, body]); return { state: "canceled", message: "canceled" }; }) as typeof api.cancelModelDownload;
  try {
    renderSection(modelWith({ nodes: [row({ storage_id: 2, state: "downloading" })], storages: [shared({ state: "pending", acquisition: "download" })] }), () => { changes++; });
    const buttons = screen.getAllByRole("button", { name: models.presenceAction.cancelDownload });
    assert.equal(buttons.length, 2);
    fireEvent.click(buttons[0]);
    const confirm = await screen.findByText(models.presenceAction.cancelDownloadConfirm);
    fireEvent.click(within(confirm.closest(".ant-popover") as HTMLElement).getByRole("button", { name: models.presenceAction.cancelDownload }));
    await waitFor(() => assert.equal(calls.length, 1));
    assert.deepEqual(calls[0], [9, { storage_id: 2, node_uuid: "n1" }]);
    assert.equal(changes, 1);
    cleanup();
    renderSection(modelWith({ storages: [shared({ state: "downloading", acquisition: "download" })] }), () => { changes++; });
    fireEvent.click(screen.getByRole("button", { name: models.presenceAction.cancelDownload }));
    const sharedConfirm = await screen.findByText(models.presenceAction.cancelDownloadConfirm);
    fireEvent.click(within(sharedConfirm.closest(".ant-popover") as HTMLElement).getByRole("button", { name: models.presenceAction.cancelDownload }));
    await waitFor(() => assert.equal(calls.length, 2));
    assert.deepEqual(calls[1], [9, { storage_id: 3, node_uuid: undefined }]);
    assert.equal(changes, 2);
  } finally { api.cancelModelDownload = originalCancel; }
});

test("download cancellation errors preserve the task and do not announce a change", async () => {
  stubMe("viewer");
  const originalCancel = api.cancelModelDownload;
  let changes = 0;
  api.cancelModelDownload = async () => { throw new Error("writer cancellation unavailable"); };
  try {
    renderSection(modelWith({ nodes: [row({ storage_id: 2, state: "downloading" })] }), () => { changes++; });
    fireEvent.click(screen.getByRole("button", { name: models.presenceAction.cancelDownload }));
    const confirm = await screen.findByText(models.presenceAction.cancelDownloadConfirm);
    fireEvent.click(within(confirm.closest(".ant-popover") as HTMLElement).getByRole("button", { name: models.presenceAction.cancelDownload }));
    assert.ok(await screen.findByText("writer cancellation unavailable"));
    assert.equal(changes, 0);
    assert.ok(screen.getAllByText(models.presence.downloading).length > 0);
  } finally { api.cancelModelDownload = originalCancel; }
});


for (const scenario of [
  { name: "a public download into its selected storage", row: { storage_id: 7, node_uuid: "n-selected", cache: false, acquisition: "download" }, path: "/api/v1/models/9/download", body: { storage_id: 7, node_uuids: ["n-selected"] } },
  { name: "a legacy public download on its original node", row: { node_uuid: "n-legacy", cache: false, acquisition: "download" }, path: "/api/v1/models/9/download", body: { node_uuid: "n-legacy" } },
  { name: "a peer transfer with its current rate and no archived rate", row: { storage_id: 7, node_uuid: "n-selected", cache: false, acquisition: "peer", source_node_uuid: "n-origin", rate_limit_kib_s: 512 }, path: "/api/v1/models/9/nodes/n-selected/sync", body: { source_node: "n-origin", rate_limit_kib_s: 512 } },
  { name: "a peer transfer with its current rate ahead of an old archived rate", row: { storage_id: 7, node_uuid: "n-selected", cache: false, acquisition: "peer", source_node_uuid: "n-origin", rate_limit_kib_s: 512, last_rate_limit_kib_s: 1024 }, path: "/api/v1/models/9/nodes/n-selected/sync", body: { source_node: "n-origin", rate_limit_kib_s: 512 } },
  { name: "an unlimited peer transfer whose omitted current rate must not reuse an old limit", row: { storage_id: 7, node_uuid: "n-selected", cache: false, acquisition: "peer", source_node_uuid: "n-origin", last_rate_limit_kib_s: 512 }, path: "/api/v1/models/9/nodes/n-selected/sync", body: { source_node: "n-origin", rate_limit_kib_s: 0 } },
]) {
  test(`disk-space Retry preserves ${scenario.name} on the HTTP wire`, async () => {
    stubMe("admin");
    const originalFetch = globalThis.fetch;
    const seen: unknown[] = [];
    let changed = 0;
    globalThis.fetch = (async (input: RequestInfo | URL, init?: RequestInit) => {
      seen.push({ path: String(input), method: init?.method, body: init?.body ? JSON.parse(String(init.body)) : undefined });
      const accepted = String(input) === scenario.path && init?.method === "POST";
      return new Response(JSON.stringify(accepted ? (scenario.path.endsWith("/sync") ? { state: "syncing" } : { status: "downloading" }) : { error: "unexpected request" }), { status: accepted ? 202 : 404, headers: { "Content-Type": "application/json" } });
    }) as typeof fetch;
    try {
      renderSection(modelWith({ nodes: [row({ ...scenario.row, state: "insufficient_disk_space", message: "insufficient disk space", downloaded: 0, progress: 0 })] }), () => { changed++; });
      fireEvent.click(screen.getByRole("button", { name: models.action.retry }));
      await waitFor(() => assert.equal(changed, 1));
      assert.deepEqual(seen, [{ path: scenario.path, method: "POST", body: scenario.body }]);
    } finally { globalThis.fetch = originalFetch; }
  });
}

test("disk-space rows do not invent a cache, unknown-source or ambiguous-target retry", () => {
  stubMe("admin");
  renderSection(modelWith({ nodes: [
    row({ node_uuid: "n-cache", node_name: "cache", state: "insufficient_disk_space", cache: true, acquisition: "download", storage_type: "nfs_cache" }),
    row({ node_uuid: "n-copy", node_name: "copy", state: "insufficient_disk_space", cache: true, acquisition: "copy", storage_type: "nfs_cache" }),
    row({ node_uuid: "n-future", node_name: "future", state: "insufficient_disk_space", cache: false, acquisition: "future" }),
    row({ node_uuid: "n-source", node_name: "source-unknown", state: "insufficient_disk_space", cache: false, acquisition: "peer", source_node_name: "source-without-uuid" }),
    row({ node_uuid: "n-many", node_name: "many", storage_id: 7, state: "insufficient_disk_space", cache: false, acquisition: "peer", source_node_uuid: "n-origin", storage_type: "local" }),
    row({ node_uuid: "n-many", node_name: "many", storage_id: 8, state: "present", cache: false, acquisition: "peer", source_node_uuid: "n-origin", storage_type: "local" }),
    row({ node_uuid: "n-shared", node_name: "shared", storage_id: 3, state: "insufficient_disk_space", cache: false, acquisition: "peer", source_node_uuid: "n-origin", storage_type: "nfs" }),
    row({ node_uuid: "n-shared", node_name: "shared", storage_id: 7, state: "present", cache: false, acquisition: "registered", storage_type: "local" }),
  ] }));
  assert.equal(screen.queryAllByRole("button", { name: models.action.retry }).length, 0, "an unknown or ambiguous target must not expose Retry");
});

test("relay capacity refusal keeps its localized retry guidance in the task details", () => {
  stubMe("admin");
  renderSection(modelWith({ nodes: [row({ state: "download_required", acquisition: "peer", source_node_uuid: "source", transfer_error_code: "PEER_RELAY_BUSY", message: "raw server capacity message" })] }));
  const details = expandFile("gpu3");
  assert.ok(details.getByText(locale.common.apiError.PEER_RELAY_BUSY));
  assert.equal(details.queryByText("raw server capacity message"), null);
});
