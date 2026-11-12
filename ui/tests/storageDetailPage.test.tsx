import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import { api, chooseMore, confirmDialog, localStorage, moreItems, locale, nodeStatus, renderDetail, restoreApi, rtl, s, stubDetail, where } from "./helpers/storageDetailHarness.tsx";
import { nfsStorage } from "./helpers/storageFixtures.ts";

const { cleanup, fireEvent, screen, waitFor, within } = rtl;
const { APIError, apiResponseError } = await import("../src/apiError.ts");
const { Modal } = await import("antd");
// Confirmation dialogs live outside the rendered tree; close them between tests.
afterEach(() => { Modal.destroyAll(); cleanup(); restoreApi(); });

const archive = nfsStorage({ nodes: { total: 130, ready: 128, problems: nfsStorage().nodes.problems, all: [nodeStatus("n1", "gpu-01", "ready")] } });
const cache = nfsStorage({
  id: 3, name: "team-cache", type: "nfs_cache", cache_path: "/data/cache", cache_max_bytes: 200 * 1024 ** 3, deployment_ids: [], model_count: 12,
  all_nodes: false, targets: [{ node_uuid: "n1", node_name: "gpu-01", path: "/mnt/x", cache_path: "/nvme/cache", cache_max_bytes: 50 * 1024 ** 3, existing: true }, { label_key: "pool", label_value: "a" }],
  nodes: { total: 3, ready: 3, problems: [] },
});
const q = (label: string) => new RegExp(`${label}$`);

test("while loading it shows a spinner; a failed load says so with retry, and the drawer closes back to the list", async () => {
  let release: (value: unknown) => void = () => {};
  stubDetail();
  api.storage = (() => new Promise((resolve) => { release = resolve; })) as never;
  const pending = renderDetail("/storage/2");
  await waitFor(() => assert.ok(document.querySelector('[role="dialog"] .ant-spin'), "a spinner while the first load runs"));
  release(archive);
  await screen.findByRole("heading", { name: /model-archive/ });
  pending.view.unmount();

  stubDetail({ storage: new Error("storage gone") });
  const { user } = renderDetail("/storage/2");
  assert.ok(await screen.findByText(s.detail.loadFailed));
  assert.ok(screen.getByText("storage gone"));
  assert.ok(screen.getByRole("button", { name: "Close" }), "the drawer closes back to the list");
  const calls = stubDetail({ storage: archive });
  await user.click(screen.getByRole("button", { name: s.detail.retry }));
  await waitFor(() => assert.ok(calls.storage.length > 0));
  assert.ok(await screen.findByRole("heading", { name: /model-archive/ }));
});

test("the drawer title names the storage with its type and marks, over the list", async () => {
  stubDetail({ storage: nfsStorage({ is_default: true, enabled: false, writable: false }) });
  renderDetail("/storage/2");
  const title = await screen.findByRole("heading", { name: /model-archive/ });
  assert.ok(within(title.parentElement as HTMLElement).getByText(locale.common.storageType.nfs));
  assert.ok(within(title.parentElement as HTMLElement).getByText(s.tag.default));
  assert.ok(within(title.parentElement as HTMLElement).getByText(s.tag.readOnly));
  assert.equal(where(), "/storage?storage=2", "the old address opened the drawer over the list");
});

test("the tab lives in the URL: ?tab= opens it, choosing a tab writes it, an unknown one falls back to the overview", async () => {
  const calls = stubDetail({ storage: archive });
  const first = renderDetail("/storage?storage=2&tab=nodes");
  assert.ok(await screen.findByRole("columnheader", { name: s.nodes.column.node }));
  assert.equal(screen.getByRole("tab", { name: new RegExp(`^${s.detail.tab.nodes}\\s*130$`) }).getAttribute("aria-selected"), "true");
  assert.deepEqual(calls.storage[0], [2, true], "the node list is asked for");
  await first.user.click(screen.getByRole("tab", { name: new RegExp(`^${s.detail.tab.files}\\s*0$`) }));
  await waitFor(() => assert.equal(where(), "/storage?storage=2&tab=files"));
  assert.ok(await screen.findByText(s.files.emptyShared));
  cleanup();

  renderDetail("/storage/2?tab=bogus");
  await screen.findByText(s.overview.config);
  assert.equal(screen.getByRole("tab", { name: s.detail.tab.overview }).getAttribute("aria-selected"), "true");
});

test("when nodes are not ready a warning counts them, names the first ones and jumps to the nodes tab", async () => {
  stubDetail({ storage: archive });
  const { user } = renderDetail("/storage/2");
  assert.ok(await screen.findByText(s.detail.notReady.replace("{{count}}", "2").replace("{{total}}", "130")));
  assert.ok(screen.getByText(/node-47: Failed · nfs client missing/));
  await user.click(screen.getByRole("button", { name: s.detail.showNodes }));
  await waitFor(() => assert.equal(where(), "/storage?storage=2&tab=nodes"));
});

test("a storage ready everywhere has no warning", async () => {
  stubDetail({ storage: nfsStorage({ nodes: { total: 2, ready: 2, problems: [] } }) });
  renderDetail("/storage/2");
  await screen.findByText(s.overview.config);
  assert.ok(screen.queryByText(/not ready on/) === null, "Expected the element to be absent");
});

test("overview of an NFS storage: configuration, capacity once, who else uses it and the bound deployments", async () => {
  stubDetail({ storage: archive, planes: [{ id: "cp-2", name: "team-b", version: "0.9.7", last_seen_seconds: 200, fresh: true }, { id: "cp-3", name: "team-old", last_seen_seconds: 7200, fresh: false }] });
  renderDetail("/storage/2");
  await screen.findByText(s.overview.config);
  const config = screen.getByRole("heading", { name: s.overview.config }).closest("section") as HTMLElement;
  assert.ok(within(config).getByText("nfs.example.internal:/export/models"));
  assert.ok(within(config).getByText("/mnt/gpuworks/model-archive"));
  assert.ok(within(config).getByText("vers=4.2"));
  assert.ok(within(config).getByText(s.scope.all));
  const capacity = screen.getByRole("heading", { name: s.overview.capacity }).closest("section") as HTMLElement;
  assert.ok(within(capacity).getByText(/Used 69\.0 TiB of 117\.0 TiB, 48\.0 TiB available/));
  assert.ok(within(capacity).getByText(s.capacity.once));
  assert.ok(await screen.findByText("team-b"));
  assert.ok(screen.getByText("in use, seen 3 min ago"));
  assert.ok(screen.getByText("last seen 2 h ago"));
  assert.equal(screen.getByRole("link", { name: "#4" }).getAttribute("href"), "/deployments?deployment=4");
  assert.equal(screen.getByRole("link", { name: "#9" }).getAttribute("href"), "/deployments?deployment=9");
});

test("overview of an NFS cache shows the cache settings and each chosen node's overrides", async () => {
  stubDetail({ storage: cache });
  renderDetail("/storage/3");
  await screen.findByText(s.overview.config);
  assert.ok(screen.getByText("/data/cache"));
  assert.ok(screen.getByText("200.0 GiB"));
  assert.ok(screen.getByText(s.overview.scopeChosen));
  assert.ok(screen.getByText(/gpu-01/));
  assert.ok(screen.getByText(/path \/mnt\/x · cache directory \/nvme\/cache · cache limit 50\.0 GiB · uses the existing mount/));
  assert.ok(screen.getByText("pool=a"));
  assert.ok(screen.getByText(s.overview.noDeployments));
});

test("overview of a local storage summarises free space per node and links to the nodes tab; it asks nobody else", async () => {
  const calls = stubDetail({ storage: localStorage({ nodes: { total: 2, ready: 2, problems: [], all: [
    nodeStatus("n1", "gpu-01", "ready", { total_bytes: 1000 * 1024 ** 3, available_bytes: 800 * 1024 ** 3 }),
    nodeStatus("n2", "gpu-02", "ready", { total_bytes: 500 * 1024 ** 3, available_bytes: 100 * 1024 ** 3 }),
  ] } }) });
  const { user } = renderDetail("/storage/1");
  assert.ok(await screen.findByText(s.capacity.perNode));
  assert.ok(screen.getByText("Free space per node: least 100.0 GiB on gpu-02, most 800.0 GiB on gpu-01"));
  assert.ok(screen.queryByText(s.overview.controlPlanes) === null, "Expected the element to be absent");
  assert.equal(calls.storageControlPlanes, undefined);
  await user.click(screen.getByRole("button", { name: s.overview.showNodes }));
  await waitFor(() => assert.equal(where(), "/storage?storage=1&tab=nodes"));
});

test("a read-only shared storage says it tracks nobody instead of asking", async () => {
  const calls = stubDetail({ storage: nfsStorage({ enabled: false, writable: false }) });
  renderDetail("/storage/2");
  assert.ok(await screen.findByText(s.controlPlanes.readOnly));
  assert.equal(calls.storageControlPlanes, undefined);
});

test("an admin can make the storage default and disable and enable it from the keyboard; each reloads the page data", async () => {
  const calls = stubDetail({ storage: archive });
  renderDetail("/storage/2");
  const user = (await import("@testing-library/user-event")).default.setup({ document: document });
  await screen.findByRole("heading", { name: /model-archive/ });
  await chooseMore(s.actions.setDefault);
  await waitFor(() => assert.deepEqual(calls.updateStorage[0], [2, { is_default: true }]));
  await waitFor(() => assert.ok(calls.storage.length >= 2 && calls.storageModelFiles.length >= 2), "storage and model files are reloaded");

  const toggle = screen.getByRole("switch", { name: s.actions.enabledSwitch.replace("{{name}}", "model-archive") });
  toggle.focus();
  await user.keyboard(" ");
  await waitFor(() => assert.deepEqual(calls.updateStorage[1], [2, { enabled: false }]));
});

test("a default storage's delete action is disabled and explains why", async () => {
  const calls = stubDetail({ storage: localStorage() });
  renderDetail("/storage/1");
  await screen.findByRole("heading", { name: /default/ });
  fireEvent.click(screen.getByRole("button", { name: new RegExp(s.actions.more) }));
  const item = await screen.findByRole("menuitem", { name: new RegExp(s.actions.delete) });
  assert.equal(item.getAttribute("aria-disabled"), "true");
  assert.match(item.textContent ?? "", new RegExp(s.actions.deleteDefaultBlocked));
  fireEvent.click(item);
  assert.equal(calls.deleteStorage, undefined);
  assert.ok(document.querySelector(".ant-modal-confirm:not(.ant-zoom-leave)") === null, "Expected the element to be absent");
});

test("a default or read-only storage offers no Make default", async () => {
  stubDetail({ storage: localStorage() });
  const first = renderDetail("/storage/1");
  await screen.findByRole("heading", { name: /default/ });
  assert.ok(!(await moreItems()).includes(s.actions.setDefault));
  cleanup();
  stubDetail({ storage: nfsStorage({ enabled: false, writable: false }) });
  renderDetail("/storage/2");
  await screen.findByRole("heading", { name: /model-archive/ });
  const items = await moreItems();
  assert.ok(items.includes(s.actions.delete), "the menu is there");
  assert.ok(!items.includes(s.actions.setDefault));
});

test("enabling a storage no node can write to shows the server's reason", async () => {
  stubDetail({ storage: nfsStorage({ enabled: false, writable: false }) });
  const { user } = renderDetail("/storage/2");
  const toggle = await screen.findByRole("switch");
  api.updateStorage = (async () => { throw new APIError("STORAGE_NOT_WRITABLE", undefined, "no node can write", 409); }) as never;
  await user.click(toggle);
  assert.ok(await screen.findByText(s.actions.failed));
  assert.ok(screen.getByText(locale.common.apiError.STORAGE_NOT_WRITABLE));
});

test("only an NFS cache has Clear cache, and only NFS types switch into each other", async () => {
  const calls = stubDetail({ storage: cache });
  const first = renderDetail("/storage/3");
  await screen.findByRole("heading", { name: /team-cache/ });
  await chooseMore(s.actions.clearCache);
  await confirmDialog(s.actions.clearCache);
  await waitFor(() => assert.deepEqual(calls.clearStorageCache, [[3]]));
  await chooseMore(s.actions.switchTo.replace("{{type}}", locale.common.storageType.nfs));
  assert.ok(await screen.findByText(s.actions.switchFromCacheHelp), "leaving the cache type says the node caches go first");
  await confirmDialog(s.actions.switch);
  await waitFor(() => assert.deepEqual(calls.updateStorage.at(-1), [3, { type: "nfs" }]));
  assert.deepEqual(calls.clearStorageCache, [[3], [3]], "the node caches are cleared again before the switch");
  // The confirmations above are Modal.confirm portals; clear them as afterEach does.
  Modal.destroyAll();
  cleanup();

  const plain = stubDetail({ storage: archive });
  renderDetail("/storage/2");
  await screen.findByRole("heading", { name: /model-archive/ });
  assert.ok(!(await moreItems()).includes(s.actions.clearCache));
  await chooseMore(s.actions.switchTo.replace("{{type}}", locale.common.storageType.nfs_cache));
  await confirmDialog(s.actions.switch);
  await waitFor(() => assert.deepEqual(plain.updateStorage.at(-1), [2, { type: "nfs_cache" }]));
  cleanup();

  stubDetail({ storage: localStorage() });
  renderDetail("/storage/1");
  await screen.findByRole("heading", { name: /default/ });
  assert.ok(!(await moreItems()).some((label) => label.startsWith(s.actions.switchTo.split("{{")[0])));
  assert.ok(screen.queryByRole("button", { name: s.actions.scan }) === null, "a local storage is not scanned");
});

test("Scan and import and Edit open their dialogs for this storage", async () => {
  stubDetail({ storage: archive });
  // jsdom does not emit CSS animationend; real motion is covered by browser E2E.
  const { user } = renderDetail("/storage/2", false);
  await user.click(await screen.findByRole("button", { name: s.actions.scan }));
  assert.ok(await screen.findByText(s.scan.title.replace("{{name}}", "model-archive")));
  assert.ok(await screen.findByText(s.scan.empty));
  fireEvent.click(screen.getAllByRole("button", { name: s.scan.close }).at(-1)!);
  await waitFor(() => assert.ok(screen.queryByText(s.scan.title.replace("{{name}}", "model-archive")) === null, "closed dialog content must disappear"));

  await user.click(screen.getByRole("button", { name: s.actions.edit, hidden: true }));
  assert.ok(await screen.findByText(s.form.editTitle.replace("{{name}}", "model-archive")));
  const editDialog = screen.getAllByRole("dialog").find((d) => within(d).queryByText(s.form.editTitle.replace("{{name}}", "model-archive")))!;
  fireEvent.keyDown(editDialog, { key: "Escape", code: "Escape", keyCode: 27 });
  await waitFor(() => assert.ok(screen.queryByText(s.form.editTitle.replace("{{name}}", "model-archive")) === null, "closed dialog content must disappear"));
});

test("deleting asks first and goes back to the list; one a deployment uses is refused and stays", async () => {
  const calls = stubDetail({ storage: archive });
  renderDetail("/storage/2");
  await screen.findByRole("heading", { name: /model-archive/ });
  api.deleteStorage = (async (...args: unknown[]) => {
    (calls.deleteStorage ??= []).push(args);
    throw new APIError("STORAGE_IN_USE", { deployment_ids: [4, 9] }, "storage in use", 409);
  }) as never;
  await chooseMore(s.actions.delete);
  await confirmDialog(s.actions.delete);
  assert.ok(await screen.findByText(s.actions.failed));
  assert.ok(screen.getByText(/#4, #9/));
  assert.equal(where(), "/storage?storage=2");

  api.deleteStorage = (async (...args: unknown[]) => { (calls.deleteStorage ??= []).push(args); }) as never;
  await chooseMore(s.actions.delete);
  await confirmDialog(s.actions.delete);
  await waitFor(() => assert.equal(where(), "/storage"));
});

test("a viewer sees the page but none of the admin actions", async () => {
  stubDetail({ storage: cache, admin: false });
  renderDetail("/storage/3");
  await screen.findByRole("heading", { name: /team-cache/ });
  await waitFor(() => assert.ok(screen.queryByRole("switch") === null, "Expected the element to be absent"));
  for (const name of [s.actions.edit, s.actions.scan, s.actions.more]) {
    assert.ok(screen.queryByRole("button", { name: new RegExp(name) }) === null, name);
  }
});


test("cancelling the storage editor returns focus to Edit without saving", async () => {
  const calls = stubDetail({ storage: localStorage() });
  const { user } = renderDetail("/storage/1", false);
  const trigger = await screen.findByRole("button", { name: s.actions.edit });
  await user.click(trigger);
  const title = s.form.editTitle.replace("{{name}}", "default");
  const dialog = await screen.findByRole("dialog", { name: title });
  await user.click(within(dialog).getByLabelText(s.form.name));
  fireEvent.change(within(dialog).getByLabelText(s.form.name), { target: { value: "cancelled-storage" } });
  await user.click(within(dialog).getByRole("button", { name: s.form.cancel }));
  await waitFor(() => assert.ok(!screen.queryByRole("dialog", { name: title })));
  await waitFor(() => assert.ok(document.activeElement === trigger, "closing the modal must restore its trigger focus"));
  assert.equal(calls.updateStorage, undefined);
});

// The errors below come from the exact bodies the server answers with,
// decoded by the same client code the console uses.
const wireError = (body: unknown) =>
  apiResponseError(new Response(JSON.stringify(body), { status: 409, headers: { "Content-Type": "application/json" } }), "request failed");

test("leaving an NFS cache stops when its caches cannot all be cleared, and says on which node", async () => {
  const order: string[] = [];
  const calls = stubDetail({ storage: cache });
  const refusal = await wireError({
    code: "STORAGE_CACHE_CLEAR_FAILED", error: "cache could not be cleared on 1 node(s); first gpu-07: node is offline",
    failed: [{ message: "node is offline", node_name: "gpu-07", node_uuid: "n7" }], freed_bytes: 0, params: { failed: 1, node: "gpu-07" }, removed: 0,
  });
  api.clearStorageCache = (async () => {
    order.push("clear");
    throw refusal;
  }) as never;
  const update = api.updateStorage;
  api.updateStorage = (async (...args: Parameters<typeof update>) => {
    order.push("switch");
    return update(...args);
  }) as never;
  renderDetail("/storage/3");
  await screen.findByRole("heading", { name: /team-cache/ });
  await chooseMore(s.actions.switchTo.replace("{{type}}", locale.common.storageType.nfs));
  await confirmDialog(s.actions.switch);
  assert.ok(await screen.findByText(locale.common.apiError.STORAGE_CACHE_CLEAR_FAILED.replace("{{failed}}", "1").replace("{{node}}", "gpu-07")));
  assert.deepEqual(order, ["clear"], "a failed clear must not be followed by the switch");
  assert.equal(calls.updateStorage, undefined);
});

test("a refused switch after a partial clear reloads the storage so the remaining caches show", async () => {
  const calls = stubDetail({ storage: cache });
  const refusal = await wireError({
    code: "STORAGE_CACHE_NOT_EMPTY", error: "node caches still hold entries (entries: 1, nodes: 1, transferring: 1); clear them before switching to nfs",
    params: { entries: 1, node_uuids: ["n1"], nodes: 1, transferring: 1 },
  });
  api.updateStorage = (async () => { throw refusal; }) as never;
  renderDetail("/storage/3");
  await screen.findByRole("heading", { name: /team-cache/ });
  const loadsBefore = calls.storage.length;
  await chooseMore(s.actions.switchTo.replace("{{type}}", locale.common.storageType.nfs));
  await confirmDialog(s.actions.switch);
  await screen.findByText(locale.common.apiError.STORAGE_CACHE_NOT_EMPTY.replace("{{entries}}", "1").replace("{{nodes}}", "1").replace("{{transferring}}", "1"));
  await waitFor(() => assert.ok(calls.storage.length > loadsBefore, "the storage is read again after the clear"));
});

test("a switch refused because node caches remain explains it in the person's language", async () => {
  stubDetail({ storage: cache });
  const refusal = await wireError({
    code: "STORAGE_CACHE_NOT_EMPTY", error: "node caches still hold entries (entries: 2, nodes: 2, transferring: 1); clear them before switching to nfs",
    params: { entries: 2, node_uuids: ["n1", "n2"], nodes: 2, transferring: 1 },
  });
  api.updateStorage = (async () => {
    throw refusal;
  }) as never;
  renderDetail("/storage/3");
  await screen.findByRole("heading", { name: /team-cache/ });
  await chooseMore(s.actions.switchTo.replace("{{type}}", locale.common.storageType.nfs));
  await confirmDialog(s.actions.switch);
  const want = locale.common.apiError.STORAGE_CACHE_NOT_EMPTY.replace("{{entries}}", "2").replace("{{nodes}}", "2").replace("{{transferring}}", "1");
  assert.ok(await screen.findByText(want));
});

test("switching storage hides the previous resource's cached models until its own files arrive", async () => {
  const React = await import("react");
  const { MemoryRouter, Routes, Route, useNavigate } = await import("react-router-dom");
  const { I18nextProvider } = await import("react-i18next");
  const { i18n, modelFile, DrawerHost } = await import("./helpers/storageDetailHarness.tsx");
  const { list } = await import("./helpers/listPage.ts");
  stubDetail();
  const base = nfsStorage({type:"nfs_cache",cache_path:"/cache",nodes:{total:1,ready:1,problems:[],all:[nodeStatus("n1","shared-node","ready")]}});
  api.storage = async (id) => ({...base,id,name:`storage-${id}`});
  let release!: (value: ReturnType<typeof list>) => void;
  api.storageModelFiles = (async (id: number) => id === 3 ? list([modelFile({cache:true,node_uuid:"n1",model_name:"previous-only-model"})]) : new Promise((resolve) => {release = resolve;})) as typeof api.storageModelFiles;
  let navigateTo!: ReturnType<typeof useNavigate>;
  function Navigation() { navigateTo = useNavigate(); return null; }
  rtl.render(React.createElement(I18nextProvider,{i18n},React.createElement(MemoryRouter,{initialEntries:["/storage?storage=3&tab=nodes"]},React.createElement(Navigation),React.createElement(Routes,null,React.createElement(Route,{path:"/storage",element:React.createElement(DrawerHost)})))));
  await screen.findAllByText("storage-3");
  await rtl.act(async () => navigateTo("/storage?storage=4&tab=nodes"));
  await waitFor(() => assert.ok(screen.queryAllByText("storage-4").length));
  fireEvent.click(screen.getByRole("button",{name:s.cacheManage.configure}));
  const dialog = await screen.findByRole("dialog", { name: s.cacheManage.nodeTitle.replace("{{node}}", "shared-node") });
  assert.ok(within(dialog).getByText(s.cacheManage.filesUnknown));
  assert.ok(within(dialog).queryByText("previous-only-model") === null);
  release(list([]));
  await waitFor(() => assert.ok(within(dialog).queryByText(s.cacheManage.filesUnknown) === null));
});

test("refresh failure retains the storage with an explicit stale-data warning and recovers on retry", async () => {
  stubDetail({ storage: { ...archive, type: "nfs_cache", cache_path: "/cache" } });
  const { user } = renderDetail("/storage?storage=2&tab=nodes", false);
  await screen.findByRole("heading", { name: /model-archive/ });
  api.storage = async () => { throw new Error("refresh unavailable"); };
  await user.click(screen.getByRole("button", { name: locale.common.dataTable.refresh }));
  assert.ok(await screen.findByText("refresh unavailable"));
  assert.ok(screen.getByRole("heading", { name: /model-archive/ }));
  api.storage = async () => archive;
  await user.click(screen.getByRole("button", { name: s.detail.retry }));
  await waitFor(() => assert.ok(screen.queryByText("refresh unavailable") === null));
});
