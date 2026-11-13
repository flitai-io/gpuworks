import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import React from "react";
import { createI18n, dom } from "./helpers/dom.ts";
import { I18nextProvider } from "react-i18next";
import { nfsStorage, nodeStatus } from "./helpers/storageFixtures.ts";
import type { Storage } from "../src/storageTypes.ts";

const { i18n, locale } = await createI18n(["storage"]);
Object.defineProperty(globalThis, "Document", { value: dom.window.Document, configurable: true });
const { act, cleanup, fireEvent, render, screen, within, waitFor } = await import("@testing-library/react");
const { default: userEvent } = await import("@testing-library/user-event");
const { MemoryRouter } = await import("react-router-dom");
const { ConfigProvider } = await import("antd");
const { api } = await import("../src/api.ts");
const { default: Nodes } = await import("../src/pages/storage/StorageCacheNodes.tsx");
// The cache state's explanation, the mount and the cache usage sit in each
// row's expanded detail; open every row.
const expandAll = (label: string) => { for (const button of screen.queryAllByRole("button", { name: label })) fireEvent.click(button); };
const original = {...api};
const s = locale.storage;
const GIB = 1024 ** 3;
afterEach(() => { cleanup(); Object.assign(api, original); });
function fixture(count = 1): Storage {
  return nfsStorage({type: "nfs_cache", updated_at: "2026-10-16T01:00:00Z", cache_path: "/cache", cache_max_bytes: 200 * GIB,
    nodes: {total: count, ready: count, problems: [], all: Array.from({length: count}, (_, i) => nodeStatus(`n${i}`, `gpu-${String(i).padStart(2,"0")}`, "ready", { cache_path: "/cache", cache_total_bytes: 1000 * GIB, cache_available_bytes: 800 * GIB, cache: {used_bytes: 100 * GIB, reclaimable_bytes: 50 * GIB, protected_bytes: 50 * GIB, limit_bytes: 200 * GIB, state: "satisfied"}}))} });
}
function view(storage = fixture(), admin = true) {
  let changed = 0;
  const tree = (current: Storage) => <ConfigProvider theme={{token:{motion:false}}}><I18nextProvider i18n={i18n}><MemoryRouter><Nodes storage={current} nodes={[]} files={null} isAdmin={admin} onChanged={() => changed++} onEditScope={() => {}} /></MemoryRouter></I18nextProvider></ConfigProvider>;
  const rendered = render(tree(storage));
  return {user: userEvent.setup({document:dom.window.document}), changed: () => changed, update: (current: Storage) => rendered.rerender(tree(current))};
}

test("node drawer saves explicit unlimited atomically; reset inherits without touching directory", async () => {
  const calls: unknown[] = [];
  api.updateStorageNodeOverrides = async (...args) => {calls.push(args); return fixture();};
  const {user} = view();
  await user.click(screen.getByRole("button", {name:s.cacheManage.configure}));
  let dialog = screen.getByRole("dialog");
  assert.ok(within(dialog).getByText(s.cacheManage.filesUnknown));
  fireEvent.click(within(dialog).getByRole("radio", {name:s.cachePolicy.unlimited}));
  await user.click(within(dialog).getByRole("button", {name:s.cacheManage.save}));
  await waitFor(() => assert.deepEqual(calls, [[2,{node_uuids:["n0"],cache_max_bytes:0}]]));
});

test("default editor sends only dirty fields and the opening revision; failure retains draft", async () => {
  const calls: unknown[] = [];
  api.updateStorage = async (...args) => {calls.push(args); throw new Error("conflict example");};
  const {user} = view();
  await user.click(screen.getByRole("button", {name:s.cacheManage.defaults}));
  const dialog = screen.getByRole("dialog");
  fireEvent.click(within(dialog).getByRole("radio", {name:s.cachePolicy.unlimited}));
  await user.click(within(dialog).getByRole("button", {name:s.cacheManage.save}));
  await waitFor(() => assert.deepEqual(calls, [[2,{cache_max_bytes:0,expected_updated_at:"2026-10-16T01:00:00Z"}]]));
  assert.ok(await within(dialog).findByText("conflict example"));
  assert.equal((within(dialog).getByRole("radio", {name:s.cachePolicy.unlimited}) as HTMLInputElement).checked, true);
});

for (const kind of ["node", "bulk", "defaults"] as const) {
  test(`${kind} cache save freezes inputs, prevents duplicate submits and restores the draft after failure`, async () => {
    const calls: unknown[] = [];
    let reject!: (error: Error) => void;
    const pending = new Promise<Storage>((_resolve, fail) => { reject = fail; });
    const save = async (...args: unknown[]) => {
      calls.push(args);
      return calls.length === 1 ? pending : fixture();
    };
    api.updateStorage = save;
    api.updateStorageNodeOverrides = save;
    const { user, changed } = view();
    if (kind === "bulk") {
      await user.click(within(screen.getByText("gpu-00").closest("tr")!).getByRole("checkbox"));
      await user.click(screen.getByRole("button", { name: s.cacheManage.batch }));
    } else {
      await user.click(screen.getByRole("button", { name: s.cacheManage[kind === "node" ? "configure" : "defaults"] }));
    }
    const dialog = within(screen.getByRole("dialog"));
    if (kind === "bulk") await user.click(dialog.getByRole("checkbox", { name: s.cacheManage.changeLimit }));
    if (kind !== "defaults") await user.click(dialog.getByRole("radio", { name: s.cacheManage.customPath }));
    const path = dialog.getByRole("textbox", { name: s.cacheManage.pathValue }) as HTMLInputElement;
    await user.clear(path);
    await user.type(path, "/new-cache");
    if (kind !== "defaults") await user.click(dialog.getByRole("radio", { name: s.cachePolicy.custom }));
    const limit = dialog.getByRole("spinbutton", { name: kind === "defaults" ? s.cacheManage.defaultLimit : s.cacheManage.limit }) as HTMLInputElement;
    await user.clear(limit);
    await user.type(limit, "32");
    const submit = dialog.getByRole("button", { name: s.cacheManage.save });
    await user.click(submit);

    const controls = [path, limit, dialog.getByRole("combobox", { name: s.cachePolicy.unit }), ...dialog.getAllByRole("radio"), ...dialog.queryAllByRole("checkbox")];
    for (const control of controls) assert.equal((control as HTMLInputElement).disabled, true, "pending save freezes every policy control");
    await user.click(dialog.getByRole("radio", { name: s.cachePolicy.unlimited }));
    await user.type(path, "/discarded");
    fireEvent.click(submit);
    fireEvent.click(submit);
    assert.equal(calls.length, 1, "pending save does not send a second mutation");
    assert.equal(path.value, "/new-cache");
    assert.equal(limit.value, "32");
    assert.equal(changed(), 0);

    await act(async () => { reject(new Error("save unavailable")); await pending.catch(() => undefined); });
    assert.ok(await dialog.findByText("save unavailable"));
    for (const control of controls) assert.equal((control as HTMLInputElement).disabled, false, "failed save restores editable controls");
    assert.equal(path.value, "/new-cache");
    assert.equal(limit.value, "32");
    await user.click(submit);
    await waitFor(() => assert.ok(screen.queryByRole("dialog") === null, "Expected the element to be absent"));
    assert.equal(changed(), 1);
    const body = kind === "defaults"
      ? { cache_max_bytes: 32 * GIB, cache_path: "/new-cache", expected_updated_at: "2026-10-16T01:00:00Z" }
      : { node_uuids: ["n0"], cache_max_bytes: 32 * GIB, cache_path: "/new-cache" };
    assert.deepEqual(calls, [[2, body], [2, body]], "retry saves the preserved draft exactly once");
  });
}

test("management batch spans pages and keeps fixed node IDs while changing only the limit", async () => {
  const calls: unknown[] = [];
  api.updateStorageNodeOverrides = async (...args) => {calls.push(args); return fixture(26);};
  const {user} = view(fixture(26));
  await user.click(screen.getByTitle("gpu-00", {selector:"input"}));
  await user.click(screen.getByRole("button", {name:s.cacheManage.selectFiltered.replace("{{count}}","26")}));
  await user.click(screen.getByRole("button", {name:s.cacheManage.batch}));
  const dialog = screen.getByRole("dialog");
  await user.click(within(dialog).getByRole("checkbox", {name:s.cacheManage.changeLimit}));
  fireEvent.click(within(dialog).getByRole("radio", {name:s.cachePolicy.unlimited}));
  await user.click(within(dialog).getByRole("button", {name:s.cacheManage.save}));
  await waitFor(() => assert.deepEqual(calls, [[2,{node_uuids:Array.from({length:26},(_,i)=>`n${i}`),cache_max_bytes:0}]]));
});

test("viewer sees policy and honest unknown state but cannot modify configuration", () => {
  const storage = fixture();
  delete storage.nodes.all![0].cache;
  view(storage, false);
  expandAll(s.detail.expand);
  assert.ok(screen.getAllByText(s.cacheManage.unknown).length);
  assert.ok(screen.queryByRole("button",{name:s.cacheManage.configure}) === null, "Expected the element to be absent");
  assert.ok(screen.queryByRole("button",{name:s.cacheManage.defaults}) === null, "Expected the element to be absent");
  assert.ok(screen.queryByRole("checkbox") === null, "Expected the element to be absent");
});

test("leaving management destroys a pending destructive confirmation", async () => {
  let calls = 0;
  api.reconcileStorageCache = async () => {calls++;};
  const {user} = view();
  await user.click(screen.getByTitle("gpu-00", {selector:"input"}));
  await user.click(screen.getByRole("button", {name:s.cacheManage.clear}));
  await screen.findByRole("dialog");
  cleanup();
  await waitFor(() => assert.ok(screen.queryByRole("dialog") === null, "Expected the element to be absent"));
  assert.equal(calls,0);
});


test("default preview totals only known inherited nodes and marks unknown observations", async () => {
  const storage = fixture(2);
  storage.nodes.all![1].state = "offline";
  const {user} = view(storage);
  await user.click(screen.getByRole("button", {name:s.cacheManage.defaults}));
  const dialog = screen.getByRole("dialog");
  fireEvent.click(within(dialog).getByRole("radio", {name:s.cachePolicy.unlimited}));
  const expected = s.cacheManage.knownUsage.replace("{{value}}","100.0 GiB").replace("{{count}}","1").replace("{{total}}","2");
  assert.ok(within(dialog).getByText(expected));
  const input = within(dialog).getByRole("textbox", {name:s.cacheManage.pathValue});
  fireEvent.change(input, {target:{value:"/new-cache"}});
  assert.ok(within(dialog).queryByText(expected) === null, "Expected the element to be absent");
  assert.ok(within(dialog).getAllByText(s.cacheManage.unknown).length >= 3);
});

test("manual page selection survives filtering and an open batch keeps its IDs across polling", async () => {
  const calls: unknown[] = [];
  api.updateStorageNodeOverrides = async (...args) => { calls.push(args); return fixture(27); };
  const { user, update } = view(fixture(26));
  await user.click(within(screen.getByText("gpu-00").closest("tr")!).getByRole("checkbox"));
  await user.click(screen.getByRole("button", { name: "Next page" }));
  await user.click(within(screen.getByText("gpu-25").closest("tr")!).getByRole("checkbox"));
  fireEvent.change(screen.getByRole("searchbox", { name: s.nodes.search }), { target: { value: "gpu-00" } });
  await user.click(screen.getByRole("button", { name: s.cacheManage.batch }));
  const dialog = screen.getByRole("dialog");
  assert.ok(within(dialog).getByText(s.cacheManage.fixedSelection.replace("{{count}}", "2")));
  const polled = fixture(27);
  polled.nodes.all = polled.nodes.all!.filter(row => row.node_uuid !== "n25");
  update(polled);
  await user.click(within(dialog).getByRole("checkbox", { name: s.cacheManage.changeLimit }));
  await user.click(within(dialog).getByRole("radio", { name: s.cachePolicy.unlimited }));
  await user.click(within(dialog).getByRole("button", { name: s.cacheManage.save }));
  await waitFor(() => assert.deepEqual(calls, [[2, { node_uuids: ["n0", "n25"], cache_max_bytes: 0 }]]));
});

test("keyboard can select a node, open batch settings and choose an unlimited policy", async () => {
  const calls: unknown[] = [];
  api.updateStorageNodeOverrides = async (...args) => { calls.push(args); return fixture(); };
  const { user } = view();
  const checkbox = screen.getByRole("checkbox", { name: s.cacheManage.selectNode.replace("{{node}}", "gpu-00") }) as HTMLInputElement;
  checkbox.focus();
  assert.ok(document.activeElement === checkbox, "node checkbox receives keyboard focus");
  await user.keyboard("[Space]");
  await waitFor(() => assert.equal(checkbox.getAttribute("aria-checked"), "true"));
  screen.getByRole("button", { name: s.cacheManage.batch }).focus();
  await user.keyboard("{Enter}");
  const dialog = screen.getByRole("dialog");
  within(dialog).getByRole("checkbox", { name: s.cacheManage.changeLimit }).focus();
  await user.keyboard(" ");
  within(dialog).getByRole("radio", { name: s.cachePolicy.unlimited }).focus();
  await user.keyboard("[Space]");
  assert.equal((within(dialog).getByRole("radio", { name: s.cachePolicy.unlimited }) as HTMLInputElement).checked, true);
  within(dialog).getByRole("button", { name: s.cacheManage.save }).focus();
  await user.keyboard("{Enter}");
  await waitFor(() => assert.deepEqual(calls, [[2, { node_uuids: ["n0"], cache_max_bytes: 0 }]]));
});

for (const filter of [
  { name: s.cacheManage.filterMode, label: s.cacheManage.mode.inherit },
  { name: s.cacheManage.filterState, label: s.cacheManage.state.failed },
]) {
  test(`cache filter searches its visible label: ${filter.name}`, async () => {
    const storage = fixture(2);
    storage.targets = [{ node_uuid: "n0", override_only: true, cache_max_bytes: 50 * GIB }];
    storage.nodes.all![1].cache!.state = "failed";
    const { user } = view(storage);
    const picker = screen.getByRole("combobox", { name: filter.name });
    await user.type(picker, filter.label);
    assert.ok(await screen.findByRole("option", { name: filter.label }));
    const dropdown = document.querySelector(".ant-select-dropdown:not(.ant-select-dropdown-hidden)") as HTMLElement;
    await user.click(within(dropdown).getByText(filter.label, { selector: ".ant-select-item-option-content" }));
    await user.keyboard("{Escape}");
    await waitFor(() => assert.ok(screen.queryByRole("link", { name: "gpu-00" }) === null));
    assert.ok(screen.getByRole("link", { name: "gpu-01" }));
  });
}

test("following a node's link does not also expand its row", async () => {
  view(fixture(2));
  const link = await screen.findByRole("link", { name: "gpu-00" });
  const row = link.closest("tr") as HTMLElement;
  fireEvent.click(link);
  assert.equal(row.nextElementSibling?.classList.contains("ant-table-expanded-row") ?? false, false);
});

test("cache state reasons render localized explanations instead of machine codes", async () => {
  const reasons = ["directory_unverified", "disk_unverified", "storage_unavailable", "node_offline", "storage_not_ready", "cleanup_required", "cache_full_in_use", "insufficient_disk", "model_exceeds_cache_limit"];
  for (const language of ["en", "zh"] as const) {
    cleanup();
    const localized = await createI18n(["storage"], language);
    const storage = fixture(reasons.length);
    reasons.forEach((reason, index) => { storage.nodes.all![index].cache = { ...storage.nodes.all![index].cache!, state: "pending", reason, message: reason }; });
    render(<ConfigProvider theme={{token:{motion:false}}}><I18nextProvider i18n={localized.i18n}><MemoryRouter><Nodes storage={storage} nodes={[]} files={null} isAdmin={false} onChanged={() => {}} onEditScope={() => {}} /></MemoryRouter></I18nextProvider></ConfigProvider>);
    expandAll(localized.i18n.t("detail.expand", { ns: "storage" }));
    for (const reason of reasons) {
      assert.ok(screen.queryByText(reason) === null, `${language}: raw ${reason} must not appear`);
      assert.ok(screen.getAllByText(localized.i18n.t(`cacheManage.reason.${reason}`, { ns: "storage" })).length > 0);
    }
  }
});

test("cache cleanup failures retain the actual RPC diagnostic", () => {
  const storage = fixture();
  const diagnostic = "rpc delete: permission denied for /cache/model/shard-01";
  storage.nodes.all![0].cache = { ...storage.nodes.all![0].cache!, state: "failed", reason: "cleanup_failed", message: diagnostic };
  view(storage);
  expandAll(s.detail.expand);
  assert.ok(screen.getByText(diagnostic));
  assert.ok(screen.getByRole("button", { name: s.cacheManage.retry }));
});

for (const mode of ["clear", "trim"] as const) {
  test(`disabled NFS cache permits ${mode} without changing storage writability`, async () => {
    const storage = fixture();
    storage.enabled = false;
    storage.nodes.all![0].writable = false;
    storage.nodes.all![0].cache!.state = "failed";
    const calls: unknown[] = [];
    api.reconcileStorageCache = async (...args) => { calls.push(args); };
    const { user, changed } = view(storage);
    if (mode === "clear") {
      const button = screen.getByRole("button", { name: s.cacheManage.clear }) as HTMLButtonElement;
      assert.equal(button.disabled, true, "selection is still required");
      await user.click(within(screen.getByText("gpu-00").closest("tr")!).getByRole("checkbox"));
      assert.equal(button.disabled, false, "read-only NFS does not disable local cache cleanup");
      await user.click(button);
      await user.click(within(screen.getByRole("dialog")).getByRole("button", { name: s.cacheManage.clear }));
    } else {
      const retry = screen.getByRole("button", { name: s.cacheManage.retry }) as HTMLButtonElement;
      assert.equal(retry.disabled, false, "read-only NFS does not disable retry");
      await user.click(retry);
    }
    await waitFor(() => assert.deepEqual(calls, [[storage.id, ["n0"], mode]]));
    assert.equal(changed(), 1);
    assert.equal(storage.enabled, false);
    assert.equal(storage.nodes.all![0].writable, false);
  });
}

test("disabled NFS cache cleanup remains unavailable to a viewer", () => {
  const storage = fixture();
  storage.enabled = false;
  storage.nodes.all![0].cache!.state = "failed";
  view(storage, false);
  assert.ok(screen.queryByRole("button", { name: s.cacheManage.clear }) === null, "Expected the element to be absent");
  assert.ok(screen.queryByRole("button", { name: s.cacheManage.retry }) === null, "Expected the element to be absent");
});
