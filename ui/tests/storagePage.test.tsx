import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import React from "react";
import { createI18n, dom } from "./helpers/dom.ts";
import { localStorage, nfsStorage, node } from "./helpers/storageFixtures.ts";
import { I18nextProvider } from "react-i18next";
import { list } from "./helpers/listPage.ts";

const { i18n, locale } = await createI18n(["storage"]);
const { MemoryRouter, Route, Routes, useLocation } = await import("react-router-dom");
const { cleanup, fireEvent, render, screen, waitFor, within } = await import("@testing-library/react");
const { default: userEvent } = await import("@testing-library/user-event");
const { api } = await import("../src/api.ts");
const { ConfigProvider } = await import("antd");
const { APIError } = await import("../src/apiError.ts");
const { default: StoragePage } = await import("../src/pages/StoragePage.tsx");

const s = locale.storage;
const originals = { ...api };
afterEach(() => { cleanup(); Object.assign(api, originals); });

const nfsCache = nfsStorage({ id: 3, name: "team-cache", type: "nfs_cache", cache_path: "/data/cache", cache_max_bytes: 200 * 1024 ** 3, is_default: false, deployment_ids: [], nodes: { total: 3, ready: 3, problems: [] }, model_count: 12 });
const readOnly = nfsStorage({ id: 4, name: "library", enabled: false, writable: false, nodes: { total: 2, ready: 2, problems: [] }, deployment_ids: [] });
const sharedDir = nfsStorage({ id: 5, name: "ceph", type: "shared_dir", export: undefined, path: "/mnt/ceph/models", total_bytes: 0, available_bytes: 0, nodes: { total: 0, ready: 0, problems: [] }, deployment_ids: [], all_nodes: false, targets: [{ node_uuid: "n1", node_name: "gpu-01" }, { label_key: "pool", label_value: "a" }] });

type Calls = Record<string, unknown[][]>;
function stubApi(options: { storages?: unknown[] | Error; admin?: boolean } = {}) {
  const calls: Calls = {};
  const record = (name: string, result: unknown) => async (...args: unknown[]) => { (calls[name] ??= []).push(args); if (result instanceof Error) throw result; return result; };
  api.me = record("me", { role: options.admin === false ? "viewer" : "admin" }) as never;
  const storages = options.storages ?? [localStorage(), nfsStorage(), nfsCache, readOnly, sharedDir];
  api.storages = record("storages", storages instanceof Error ? storages : list(storages)) as never;
  api.nodes = record("nodes", list([node("n1", "gpu-01"), node("n2", "gpu-02")])) as never;
  api.storageSettings = record("storageSettings", { control_plane_id: "cp-1", control_plane_name: "team-a", reserve_free_bytes: 50 * 1024 ** 3 }) as never;
  api.updateStorageSettings = record("updateStorageSettings", { control_plane_id: "cp-1", control_plane_name: "renamed", reserve_free_bytes: 0 }) as never;
  api.deleteStorage = record("deleteStorage", undefined) as never;
  api.createStorage = record("createStorage", nfsStorage({ id: 9, name: "fresh", type: "local" })) as never;
  return calls;
}

function Where() { const l = useLocation(); return <div data-testid="where">{l.pathname}{l.search}</div>; }
function renderPage(motion = true) {
  const view = render(
    <ConfigProvider theme={{ token: { motion } }}><I18nextProvider i18n={i18n}>
      <MemoryRouter initialEntries={["/storage"]}>
        <Routes><Route path="/storage" element={<><StoragePage /><Where /></>} /></Routes>
      </MemoryRouter>
    </I18nextProvider></ConfigProvider>,
  );
  return { view, user: userEvent.setup({ document: dom.window.document }) };
}
const rowOf = async (name: string) => {
  const link = await screen.findByRole("link", { name });
  return link.closest("tr") as HTMLElement;
};
// Confirm in the open popover that has the confirm button; a previous popover may still be fading out.
const confirm = async (user: ReturnType<typeof userEvent.setup>, label: string) => {
  const ok = await waitFor(() => {
    const found = [...document.querySelectorAll(".ant-popover:not(.ant-popover-hidden)")]
      .flatMap((pop) => within(pop as HTMLElement).queryAllByRole("button", { name: new RegExp(`${label}$`) }));
    assert.ok(found.length > 0);
    return found.at(-1) as HTMLElement;
  });
  await user.click(ok);
};

test("while loading it shows a spinner; an empty system invites adding the first storage; a failed load offers retry", async () => {
  let release: (rows: unknown) => void = () => {};
  stubApi();
  api.storages = (() => new Promise((resolve) => { release = resolve as never; })) as never;
  const first = renderPage();
  assert.ok(screen.getByRole("table").getAttribute("aria-busy") === "true", "the list reports loading");
  release(list([]));
  assert.ok(await screen.findByText(s.page.empty));
  assert.ok(screen.getAllByRole("button", { name: new RegExp(`${s.page.add}$`) }).length >= 2, "header and empty state both offer adding");
  first.view.unmount();

  const calls = stubApi({ storages: new Error("boom") });
  const second = renderPage();
  assert.ok(await screen.findByText("boom"));
  const before = calls.storages.length;
  await second.user.click(screen.getByRole("button", { name: locale.common.actions.retry }));
  await waitFor(() => assert.ok(calls.storages.length > before));
});

test("a viewer gets the empty text without an add button", async () => {
  stubApi({ storages: [], admin: false });
  renderPage();
  assert.ok(await screen.findByText(s.page.empty));
  await waitFor(() => assert.ok(screen.queryByRole("button", { name: new RegExp(`${s.page.add}$`) }) === null, "Expected the element to be absent"));
});

test("one row per storage, with name link, tags, type, location, scope, node health, capacity, models and deployments", async () => {
  stubApi();
  renderPage();
  const archive = await rowOf("model-archive");
  assert.equal(screen.getAllByRole("row").length, 1 + 5, "header plus a row per storage");
  assert.equal(within(archive).getByRole("link", { name: "model-archive" }).getAttribute("href"), "/storage?storage=2");
  assert.ok(within(archive).getByText(locale.common.storageType.nfs));
  assert.ok(within(archive).getByText("nfs.example.internal:/export/models"));
  assert.ok(within(archive).getByText(s.list.mountedAt.replace("{{path}}", "/mnt/gpuworks/model-archive")));
  assert.ok(within(archive).getByText(s.scope.all));
  assert.ok(within(archive).getByText(s.health.summary.replace("{{ready}}", "128").replace("{{total}}", "130")));
  assert.ok(within(archive).getByText(/Used 69\.0 TiB of 117\.0 TiB, 48\.0 TiB available/));
  assert.ok(within(archive).getByText("480"));
  assert.ok(within(archive).getByText("2"), "two deployments");

  const local = await rowOf("default");
  assert.ok(within(local).getByText(s.tag.default));
  assert.ok(within(local).getByText("/data/models"));
  assert.ok(within(local).getByText(s.capacity.perNode));

  assert.ok(within(await rowOf("library")).getByText(s.tag.readOnly));
  assert.ok(within(archive).queryByText(s.tag.readOnly) === null, "Expected the element to be absent");

  const dir = await rowOf("ceph");
  assert.ok(within(dir).getByText("/mnt/ceph/models"));
  assert.ok(within(dir).getByText(s.scope.labels.replace("{{labels}}", "pool=a") + " · " + s.scope.nodes.replace("{{count}}", "1")));
  assert.ok(within(dir).getByText(s.health.none));
  assert.ok(within(dir).getByText(s.capacity.unknown));
  assert.ok(screen.queryByText(/Related Resources|Managed Path/) === null, "the images and managed path blocks are gone from this page");
});

test("a storage with nodes in trouble shows a warning mark that names them", async () => {
  stubApi();
  const { user } = renderPage();
  const archive = await rowOf("model-archive");
  const mark = within(archive).getByRole("img", { name: s.health.problems.replace("{{count}}", "2") });
  await user.hover(mark);
  assert.ok(await screen.findByText(/node-47: Failed · nfs client missing/));
  assert.ok(screen.getByText(/node-07: Not responding/));
  assert.ok(within(await rowOf("team-cache")).queryByRole("img", { name: /with problems/ }) === null, "Expected the element to be absent");
});

test("opening a storage's drawer reads the node list once, with the page", async () => {
  const calls = stubApi();
  api.storage = (async () => nfsStorage()) as never;
  api.storageModelFiles = (async () => ({ items: [] })) as never;
  const { user } = renderPage(false);
  const archive = await rowOf("model-archive");
  await user.click(within(archive).getByRole("button", { name: s.actions.detail }));
  await screen.findByRole("tab", { name: new RegExp(s.detail.tab.overview) });
  assert.equal(calls.nodes?.length, 1, "the drawer takes the page's nodes");
});

test("the default storage's delete control is disabled and explains why", async () => {
  const calls = stubApi();
  renderPage();
  const row = await rowOf("default");
  const button = within(row).getByRole("button", { name: s.actions.delete }) as HTMLButtonElement;
  assert.equal(button.disabled, true);
  assert.equal(document.getElementById(button.getAttribute("aria-describedby") ?? "")?.textContent, s.actions.deleteDefaultBlocked);
  fireEvent.click(button);
  assert.equal(calls.deleteStorage, undefined);
});

test("an admin has Edit and Delete on each row; Details opens the storage's drawer", async () => {
  stubApi();
  const { user } = renderPage();
  const archive = await rowOf("model-archive");
  assert.ok(within(archive).getByRole("button", { name: s.actions.edit }));
  assert.ok(within(archive).getByRole("button", { name: s.actions.delete }));
  await user.click(within(archive).getByRole("button", { name: s.actions.detail }));
  await waitFor(() => assert.equal(screen.getByTestId("where").textContent, "/storage?storage=2"));
});

test("a viewer sees the storages and Details, none of the admin actions", async () => {
  stubApi({ admin: false });
  renderPage();
  const archive = await rowOf("model-archive");
  assert.ok(within(archive).getByRole("button", { name: s.actions.detail }));
  for (const name of [s.actions.edit, s.actions.delete]) assert.ok(within(archive).queryByRole("button", { name }) === null, name);
  assert.ok(screen.queryByRole("button", { name: new RegExp(`${s.page.add}$`) }) === null, "Expected the element to be absent");
  assert.ok(screen.getByRole("button", { name: new RegExp(`${s.page.settings}$`) }), "settings stay readable");
});

test("deleting asks first; a storage a deployment uses is refused with the deployments named", async () => {
  const calls = stubApi();
  const { user } = renderPage();
  const archive = await rowOf("model-archive");
  api.deleteStorage = (async (...args: unknown[]) => {
    (calls.deleteStorage ??= []).push(args);
    throw new APIError("STORAGE_IN_USE", { deployment_ids: [4, 9] }, "storage in use", 409);
  }) as never;
  await user.click(within(archive).getByRole("button", { name: s.actions.delete }));
  await confirm(user, s.actions.delete);
  assert.ok(await screen.findByText(s.actions.failed));
  assert.ok(screen.getByText(new RegExp(`${locale.common.apiError.STORAGE_IN_USE.slice(0, 30)}.*#4, #9`)));
  assert.deepEqual(calls.deleteStorage, [[2]]);
});

test("deleting an unused storage succeeds and the list is reloaded", async () => {
  const calls = stubApi();
  const { user } = renderPage();
  const cache = await rowOf("team-cache");
  const before = calls.storages.length;
  await user.click(within(cache).getByRole("button", { name: s.actions.delete }));
  await confirm(user, s.actions.delete);
  await waitFor(() => assert.deepEqual(calls.deleteStorage, [[3]]));
  await waitFor(() => assert.ok(calls.storages.length > before));
});

test("Edit opens the edit dialog for that storage, and Escape closes it", async () => {
  stubApi();
  // jsdom does not emit CSS animationend; real motion is covered by browser E2E.
  const { user } = renderPage(false);
  await user.click(within(await rowOf("model-archive")).getByRole("button", { name: s.actions.edit }));
  assert.ok(await screen.findByText(s.form.editTitle.replace("{{name}}", "model-archive")));
  fireEvent.keyDown(screen.getByRole("dialog"), { key: "Escape", code: "Escape", keyCode: 27 });
  await waitFor(() => assert.ok(screen.queryByText(s.form.editTitle.replace("{{name}}", "model-archive")) === null, "closed dialog content must disappear"));
});

test("adding a storage opens the add dialog, and a created storage opens its drawer", async () => {
  const calls = stubApi();
  const { user } = renderPage();
  await rowOf("model-archive");
  await user.click(screen.getAllByRole("button", { name: new RegExp(`${s.page.add}$`) })[0]);
  const dialog = await screen.findByRole("dialog", { name: s.form.addTitle });
  await user.type(within(dialog).getByLabelText(s.form.name), "fresh");
  await user.type(within(dialog).getByLabelText(s.form.path), "/data/fresh");
  await user.click(within(dialog).getByRole("button", { name: new RegExp(`^${s.form.save}$`) }));
  await waitFor(() => assert.equal(calls.createStorage?.length, 1));
  await waitFor(() => assert.equal(screen.getByTestId("where").textContent, "/storage?storage=9"));
});

test("Storage settings opens a dialog with the control plane name and reserve, and saves both", async () => {
  const calls = stubApi();
  const { user } = renderPage();
  await rowOf("model-archive");
  assert.equal(calls.storageSettings, undefined, "not loaded until asked for");
  await user.click(screen.getByRole("button", { name: new RegExp(`${s.page.settings}$`) }));
  const name = await screen.findByLabelText(s.settings.controlPlaneName) as HTMLInputElement;
  await waitFor(() => assert.equal(name.value, "team-a"));
  assert.equal((screen.getByLabelText(s.settings.reserve) as HTMLInputElement).value, "50");
  assert.ok(screen.getByText(s.settings.controlPlaneId.replace("{{id}}", "cp-1")));
  await user.clear(name);
  await user.type(name, "renamed");
  await user.click(screen.getByRole("button", { name: s.settings.save }));
  await waitFor(() => assert.deepEqual(calls.updateStorageSettings, [[{ control_plane_name: "renamed", reserve_free_bytes: 50 * 1024 ** 3 }]]));
});

test("an unset reserve shows the default instead of 0 and saves as unset", async () => {
  const calls = stubApi();
  api.storageSettings = (async () => ({ control_plane_id: "cp-1", control_plane_name: "team-a", reserve_free_bytes: 0 })) as never;
  const { user } = renderPage();
  await user.click(await screen.findByRole("button", { name: new RegExp(`${s.page.settings}$`) }));
  const name = await screen.findByLabelText(s.settings.controlPlaneName) as HTMLInputElement;
  await waitFor(() => assert.equal(name.value, "team-a"));
  const reserve = screen.getByLabelText(s.settings.reserve) as HTMLInputElement;
  assert.equal(reserve.value, "");
  assert.equal(reserve.placeholder, s.settings.reserveDefault);
  await user.click(screen.getByRole("button", { name: s.settings.save }));
  await waitFor(() => assert.deepEqual(calls.updateStorageSettings, [[{ control_plane_name: "team-a", reserve_free_bytes: 0 }]]));
});

test("settings that cannot be saved say so; a viewer cannot edit them; a failed load offers retry", async () => {
  stubApi();
  api.updateStorageSettings = (async () => { throw new APIError("STORAGE_NAME_INVALID", undefined, "bad name", 400); }) as never;
  const first = renderPage();
  await first.user.click(await screen.findByRole("button", { name: new RegExp(`${s.page.settings}$`) }));
  await first.user.click(await screen.findByRole("button", { name: s.settings.save }));
  assert.ok(await screen.findByText(s.settings.saveFailed));
  first.view.unmount();

  stubApi({ admin: false });
  const second = renderPage();
  await second.user.click(await screen.findByRole("button", { name: new RegExp(`${s.page.settings}$`) }));
  const name = await screen.findByLabelText(s.settings.controlPlaneName) as HTMLInputElement;
  assert.equal(name.disabled, true);
  assert.ok(screen.queryByRole("button", { name: s.settings.save }) === null, "Expected the element to be absent");
  second.view.unmount();

  stubApi();
  api.storageSettings = (async () => { throw new Error("settings down"); }) as never;
  const third = renderPage();
  await third.user.click(await screen.findByRole("button", { name: new RegExp(`${s.page.settings}$`) }));
  assert.ok(await screen.findByText(s.settings.loadFailed));
  assert.ok(screen.getByText("settings down"));
});

test("offline nodes are counted apart and raise no warning; only online nodes where the storage is not ready do", async () => {
  const offline = (uuid: string, name: string) => ({ node_uuid: uuid, node_name: name, state: "offline", writable: false });
  stubApi({ storages: [localStorage({ nodes: { total: 12, ready: 7, problems: [offline("a", "h200-1"), offline("b", "h200-2"), offline("c", "4090-1"), offline("d", "4090-2"), offline("e", "accept")] } })] });
  renderPage();
  const row = await rowOf("default");
  assert.ok(within(row).getByText(s.health.summary.replace("{{ready}}", "7").replace("{{total}}", "7")));
  assert.ok(within(row).getByText(s.health.offline.replace("{{count}}", "5")));
  assert.ok(row.querySelector(".anticon-exclamation-circle") === null, "no warning for offline nodes");
});


test("cancelled storage creation returns keyboard focus to Add storage without mutation", async () => {
  const calls = stubApi();
  const { user } = renderPage(false);
  const trigger = await screen.findByRole("button", { name: new RegExp(`${s.page.add}$`) });
  await user.click(trigger);
  const dialog = await screen.findByRole("dialog", { name: s.form.addTitle });
  fireEvent.change(within(dialog).getByLabelText(s.form.name), { target: { value: "cancelled-storage" } });
  await user.click(within(dialog).getByRole("button", { name: s.form.cancel }));
  await waitFor(() => assert.ok(!screen.queryByRole("dialog", { name: s.form.addTitle })));
  await waitFor(() => assert.ok(document.activeElement === trigger, "closing the modal must restore its trigger focus"));
  assert.equal(calls.createStorage, undefined);
});
