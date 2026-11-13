// The storage detail page's browse tab (ADR 0053): choosing a node, walking the tree, what is offered where, and the dialogs.
import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import { api, confirm, emptyListing, findModal, modals, localStorage, nodeStatus, renderDetail, restoreApi, rtl, s, stubDetail, type BrowseQuery } from "./helpers/storageDetailHarness.tsx";
import { fileItem, nfsStorage } from "./helpers/storageFixtures.ts";
import type { StorageFileList } from "../src/storageTypes";

const { cleanup, fireEvent, screen, waitFor, within } = rtl;
afterEach(() => { cleanup(); restoreApi(); });
const b = s.browse;
const fill = (text: string, values: Record<string, string | number>) => text.replace(/{{(\w+)}}/g, (_: string, key: string) => String(values[key]));

const shared = nfsStorage({ id: 2, name: "nfs-shared", nodes: { total: 2, ready: 2, problems: [], all: [nodeStatus("n1", "gpu-01", "ready"), nodeStatus("n2", "gpu-02", "ready")] } });
const local = localStorage({
  id: 1,
  nodes: { total: 3, ready: 2, problems: [], all: [
    nodeStatus("n1", "gpu-01", "ready"),
    nodeStatus("n2", "gpu-02", "ready"),
    nodeStatus("n3", "gpu-03", "offline"),
  ] },
});

/** A tree as the server lists it: the root, files/ with a directory and a file, and a model directory. */
function tree(query: BrowseQuery): StorageFileList {
  const node = query.node ?? "n1";
  switch (query.path) {
    case "":
      return { path: "", node_uuid: node, writable: false, next_cursor: "", items: [
        fileItem(".gpuworks", { dir: true, writable: false, size: 0 }),
        fileItem("Qwen", { dir: true, writable: false, size: 0 }),
        fileItem("files", { dir: true, writable: false, size: 0 }),
      ] };
    case "files":
      return { path: "files", node_uuid: node, writable: true, next_cursor: "", items: [
        fileItem("files/data sets", { dir: true, size: 0 }),
        fileItem("files/notes&1.txt", { size: 2048 }),
        fileItem("files/latest", { symlink: true, size: 9 }),
      ] };
    case "Qwen":
      return { path: "Qwen", node_uuid: node, writable: false, next_cursor: "", items: [fileItem("Qwen/config.json", { writable: false })] };
    default:
      return emptyListing(query);
  }
}

const row = async (name: string) => (await screen.findByText(name, { selector: "tbody *" })).closest("tr") as HTMLElement;
const open = async (name: string) => fireEvent.click(await screen.findByRole("button", { name: fill(b.open, { name }) }));
const listed = (calls: Record<string, unknown[][]>) => (calls.storageFiles ?? []).map((c) => c[1] as BrowseQuery);

test("a viewer does not get the tab; an admin's tab is labelled Browse", async () => {
  stubDetail({ storage: shared, admin: false });
  renderDetail("/storage/2");
  await screen.findByRole("tab", { name: new RegExp(s.detail.tab.overview) });
  await waitFor(() => assert.ok(screen.queryByRole("tab", { name: s.detail.tab.browse }) === null, "Expected the element to be absent"));
  cleanup();
  stubDetail({ storage: shared });
  renderDetail("/storage/2");
  assert.ok(await screen.findByRole("tab", { name: s.detail.tab.browse }));
});

test("a viewer's ?tab=browse opens the overview instead of an empty tab", async () => {
  stubDetail({ storage: shared, admin: false });
  renderDetail("/storage?storage=2&tab=browse");
  const overview = await screen.findByRole("tab", { name: new RegExp(s.detail.tab.overview) });
  await waitFor(() => assert.equal(overview.getAttribute("aria-selected"), "true"));
});

test("a local storage asks for a node first; offline nodes cannot be chosen; choosing one lists its root", async () => {
  const calls = stubDetail({ storage: local, browse: tree });
  renderDetail("/storage/1?tab=browse", false);
  assert.ok(await screen.findByText(b.chooseNodeHint));
  assert.equal(listed(calls).length, 0, "nothing is listed before a node is chosen");

  fireEvent.mouseDown(await screen.findByRole("combobox", { name: b.nodeLabel }));
  const offline = await screen.findByText(fill(b.nodeState, { node: "gpu-03", state: "Offline" }));
  assert.ok(offline.closest(".ant-select-item-option-disabled"), "an offline node is disabled");
  fireEvent.click(await screen.findByText("gpu-02", { selector: ".ant-select-item-option-content" }));

  assert.ok(await row("Qwen"));
  assert.deepEqual(listed(calls).at(-1), { node: "n2", path: "" });
  const location = screen.getByRole("navigation", { name: b.location });
  assert.equal(within(location).getByText("default").getAttribute("aria-current"), "page");
});

test("a local storage with one ready node opens on it", async () => {
  const calls = stubDetail({ storage: localStorage({ id: 1, nodes: { total: 2, ready: 1, problems: [], all: [nodeStatus("n1", "gpu-01", "ready"), nodeStatus("n2", "gpu-02", "failed")] } }), browse: tree });
  renderDetail("/storage/1?tab=browse", false);
  assert.ok(await row("files"));
  assert.deepEqual(listed(calls)[0], { node: "n1", path: "" });
});

test("a shared storage lists without a node, says which node read it, and the breadcrumb walks back up", async () => {
  const calls = stubDetail({ storage: shared, browse: tree });
  renderDetail("/storage/2?tab=browse", false);
  assert.ok(await row("Qwen"));
  assert.deepEqual(listed(calls)[0], { node: undefined, path: "" });
  assert.ok(screen.queryByRole("combobox", { name: b.nodeLabel }) === null, "Expected the element to be absent");
  assert.ok(screen.getByText(fill(b.readBy, { node: "gpu-01" })));

  await open("files");
  const notes = await row("notes&1.txt");
  assert.ok(within(notes).getByText("2.0 KiB"));
  assert.ok(within(notes).getByText(/2026-10-21 /));
  const dirRow = await row("data sets");
  assert.ok(within(dirRow).getByText("—"), "a directory has no size");
  assert.ok(within(await row("latest")).getByText(b.symlink));
  assert.ok(screen.queryByRole("button", { name: fill(b.open, { name: "latest" }) }) === null, "a link is not followed");

  await open("data sets");
  assert.ok(await screen.findByText(b.empty));
  assert.deepEqual(listed(calls).at(-1), { node: undefined, path: "files/data sets" });
  const location = screen.getByRole("navigation", { name: b.location });
  assert.equal(within(location).getByText("data sets").getAttribute("aria-current"), "page");
  fireEvent.click(within(location).getByRole("button", { name: "files" }));
  assert.ok(await row("notes&1.txt"));
  fireEvent.click(within(screen.getByRole("navigation", { name: b.location })).getByRole("button", { name: "nfs-shared" }));
  assert.ok(await row("Qwen"));
});

test("listing shows a spinner while loading, and a failure with a retry that lists again", async () => {
  let release: (page: StorageFileList) => void = () => {};
  let fail = true;
  const calls = stubDetail({
    storage: shared,
    browse: (query) => {
      if (query.path === "") return new Promise<StorageFileList>((resolve) => { release = resolve; });
      if (fail) return new Error("node gpu-01 is offline");
      return tree(query);
    },
  });
  renderDetail("/storage/2?tab=browse", false);
  await waitFor(() => assert.ok(document.querySelector('[role="dialog"] .ant-spin-spinning'), "loading"));
  release(tree({ path: "" }));
  await open("files");
  assert.ok(await screen.findByText("node gpu-01 is offline"));
  fail = false;
  fireEvent.click(screen.getByRole("button", { name: /Retry/ }));
  assert.ok(await row("notes&1.txt"));
  assert.equal(listed(calls).filter((q) => q.path === "files").length, 2);
});

test("load more asks for the next page with the cursor and appends it, until there is none", async () => {
  const calls = stubDetail({
    storage: shared,
    browse: (query) => (query.cursor
      ? { path: "files", writable: true, items: [fileItem("files/c.txt")], next_cursor: "" }
      : { path: "files", writable: true, items: [fileItem("files/a.txt"), fileItem("files/b.txt")], next_cursor: "b.txt" }),
  });
  renderDetail("/storage/2?tab=browse", false);
  assert.ok(await row("a.txt"));
  assert.ok(screen.getByText(fill(b.loaded, { count: 2 })));
  fireEvent.click(screen.getByRole("button", { name: b.loadMore }));
  assert.ok(await row("c.txt"));
  assert.ok(screen.getByText("a.txt"), "the first page stays");
  assert.deepEqual(listed(calls).at(-1), { node: undefined, path: "", cursor: "b.txt" });
  await waitFor(() => assert.ok(screen.queryByRole("button", { name: b.loadMore }) === null, "Expected the element to be absent"));
});

test("a read-only place disables upload and new directory and says why, on hover, focus and to a screen reader", async () => {
  stubDetail({ storage: shared, browse: tree });
  const { user } = renderDetail("/storage/2?tab=browse", false);
  await row("Qwen");
  const reason = b.readonly.region;
  assert.ok(screen.getByText(reason, { selector: ".ant-typography" }), "the reason is visible next to the buttons");
  const upload = screen.getByRole("button", { name: new RegExp(`${b.upload}$`) });
  assert.equal((upload as HTMLButtonElement).disabled, true);
  const described = document.getElementById(upload.getAttribute("aria-describedby") ?? "");
  assert.equal(described?.textContent, reason);

  const wrapper = upload.parentElement as HTMLElement;
  assert.equal(wrapper.tabIndex, 0);
  screen.getByRole("button", { name: "Close", exact: true }).focus();
  for (let step = 0; step < 50 && document.activeElement !== wrapper; step++) await user.tab();
  assert.ok(document.activeElement === wrapper, "the disabled-action explanation is reachable within the drawer");
  const tip = await screen.findByRole("tooltip");
  assert.ok(tip.textContent?.includes(reason));

  // The model directory: the rows say so too.
  await open("Qwen");
  const config = await row("config.json");
  const rename = within(config).getByRole("button", { name: fill(b.rename, { name: "config.json" }) });
  assert.equal((rename as HTMLButtonElement).disabled, true);
  assert.equal(document.getElementById(rename.getAttribute("aria-describedby") ?? "")?.textContent, reason);
});

test("a region root can take new things but cannot itself be renamed or deleted", async () => {
  stubDetail({ storage: shared, browse: tree });
  renderDetail("/storage/2?tab=browse", false);
  const files = await row("files");
  const del = within(files).getByRole("button", { name: fill(b.delete, { name: "files" }) });
  assert.equal((del as HTMLButtonElement).disabled, true);
  assert.equal(document.getElementById(del.getAttribute("aria-describedby") ?? "")?.textContent, fill(b.readonly.regionRoot, { name: "files" }));
  await open("files");
  await row("notes&1.txt");
  assert.equal((screen.getByRole("button", { name: new RegExp(`${b.upload}$`) }) as HTMLButtonElement).disabled, false);
  assert.ok(screen.queryByText(b.readonly.region, { selector: ".ant-typography" }) === null, "Expected the element to be absent");
});

test("a disabled storage is read-only everywhere and says the storage is disabled", async () => {
  stubDetail({
    storage: { ...shared, enabled: false },
    browse: (query) => ({ ...tree(query), writable: false, items: tree(query).items.map((item) => ({ ...item, writable: false })) }),
  });
  renderDetail("/storage/2?tab=browse", false);
  await open("files");
  const notes = await row("notes&1.txt");
  assert.ok(screen.getByText(b.readonly.disabled, { selector: ".ant-typography" }));
  const del = within(notes).getByRole("button", { name: fill(b.delete, { name: "notes&1.txt" }) });
  assert.equal(document.getElementById(del.getAttribute("aria-describedby") ?? "")?.textContent, b.readonly.disabled);
});

test("download links carry the encoded node and path; a directory downloads as .zip, or .tar.gz from the menu", async () => {
  stubDetail({ storage: local, browse: tree });
  renderDetail("/storage/1?tab=browse", false);
  fireEvent.mouseDown(await screen.findByRole("combobox", { name: b.nodeLabel }));
  fireEvent.click(await screen.findByText("gpu-01", { selector: ".ant-select-item-option-content" }));
  await open("files");
  const notes = await row("notes&1.txt");
  const fileLink = within(notes).getByRole("link", { name: fill(b.download, { name: "notes&1.txt" }) });
  assert.equal(fileLink.getAttribute("href"), "/api/v1/storages/1/files/download?node_uuid=n1&path=files%2Fnotes%261.txt");
  assert.ok(fileLink.hasAttribute("download"));

  const dir = await row("data sets");
  const zip = within(dir).getByRole("link", { name: fill(b.download, { name: "data sets" }) });
  assert.equal(zip.getAttribute("href"), "/api/v1/storages/1/files/download?node_uuid=n1&path=files%2Fdata%20sets&format=zip");

  const clicked: string[] = [];
  const original = window.HTMLAnchorElement.prototype.click;
  window.HTMLAnchorElement.prototype.click = function click(this: HTMLAnchorElement) { clicked.push(this.getAttribute("href") ?? ""); };
  try {
    fireEvent.click(within(dir).getByRole("button", { name: fill(b.downloadFormats, { name: "data sets" }) }));
    fireEvent.click(await screen.findByText(b.downloadTarGz));
    assert.deepEqual(clicked, ["/api/v1/storages/1/files/download?node_uuid=n1&path=files%2Fdata%20sets&format=tar.gz"]);
  } finally {
    window.HTMLAnchorElement.prototype.click = original;
  }

  const link = await row("latest");
  const linkDownload = within(link).getByRole("button", { name: fill(b.download, { name: "latest" }) });
  assert.equal((linkDownload as HTMLButtonElement).disabled, true, "a symbolic link is not downloaded");
});

test("new directory opens from the keyboard, creates under the current path, and Esc closes it", async () => {
  const calls = stubDetail({ storage: shared, browse: tree });
  const { user } = renderDetail("/storage/2?tab=browse", false);
  await open("files");
  await row("notes&1.txt");
  const mkdir = screen.getByRole("button", { name: new RegExp(`${b.mkdir}$`) });
  mkdir.focus();
  await user.keyboard("{Enter}");
  const dialog = await screen.findByRole("dialog", { name: fill(b.mkdirTitle, { path: "/files" }) });
  const input = within(dialog).getByLabelText(b.name);
  await user.type(input, "a/b");
  assert.ok(await within(dialog).findByText(b.nameInvalid));
  assert.equal((within(dialog).getByRole("button", { name: b.mkdirOk }) as HTMLButtonElement).disabled, true);
  await user.clear(input);
  await user.type(input, "eval sets{Enter}");
  await waitFor(() => assert.deepEqual(calls.storageFilesMkdir?.[0], [2, undefined, "files/eval sets"]));
  await waitFor(() => assert.equal(modals().length, 0));
  assert.ok(listed(calls).filter((q) => q.path === "files").length >= 2, "the list is read again");

  mkdir.focus();
  await user.keyboard(" ");
  const again = await findModal();
  fireEvent.keyDown(within(again).getByLabelText(b.name), { key: "Escape", code: "Escape", keyCode: 27 });
  await waitFor(() => assert.equal(modals().length, 0));
  assert.equal(calls.storageFilesMkdir?.length, 1);
});

test("a refused new directory keeps the dialog open with the server's reason", async () => {
  stubDetail({ storage: shared, browse: tree });
  api.storageFilesMkdir = (async () => { throw new Error("files/x already exists as a file"); }) as never;
  const { user } = renderDetail("/storage/2?tab=browse", false);
  await open("files");
  await row("notes&1.txt");
  await user.click(screen.getByRole("button", { name: new RegExp(`${b.mkdir}$`) }));
  const dialog = await findModal();
  await user.type(within(dialog).getByLabelText(b.name), "x");
  await user.click(within(dialog).getByRole("button", { name: b.mkdirOk }));
  assert.ok(await within(dialog).findByText("files/x already exists as a file"));
});

test("rename starts from the old name and renames within the same directory", async () => {
  const calls = stubDetail({ storage: local, browse: tree });
  const { user } = renderDetail("/storage/1?tab=browse", false);
  fireEvent.mouseDown(await screen.findByRole("combobox", { name: b.nodeLabel }));
  fireEvent.click(await screen.findByText("gpu-02", { selector: ".ant-select-item-option-content" }));
  await open("files");
  const notes = await row("notes&1.txt");
  await user.click(within(notes).getByRole("button", { name: fill(b.rename, { name: "notes&1.txt" }) }));
  const dialog = await screen.findByRole("dialog", { name: fill(b.renameTitle, { name: "notes&1.txt" }) });
  const input = within(dialog).getByLabelText(b.name) as HTMLInputElement;
  assert.equal(input.value, "notes&1.txt");
  assert.equal((within(dialog).getByRole("button", { name: b.renameOk }) as HTMLButtonElement).disabled, true, "an unchanged name is not a rename");
  await user.clear(input);
  await user.type(input, "notes.txt");
  await user.click(within(dialog).getByRole("button", { name: b.renameOk }));
  await waitFor(() => assert.deepEqual(calls.storageFilesRename?.[0], [1, "n2", "files/notes&1.txt", "files/notes.txt"]));
});

test("delete asks first, then deletes on the chosen node and lists again; cancelling deletes nothing", async () => {
  const calls = stubDetail({ storage: shared, browse: tree });
  const { user } = renderDetail("/storage/2?tab=browse", false);
  await open("files");
  const notes = await row("notes&1.txt");
  await user.click(within(notes).getByRole("button", { name: fill(b.delete, { name: "notes&1.txt" }) }));
  assert.ok(await screen.findByText(fill(b.deleteConfirm, { name: "notes&1.txt" })));
  assert.ok(screen.getByText(b.deleteDescription));
  fireEvent.click(screen.getByRole("button", { name: b.cancel }));
  assert.equal(calls.deleteStorageFile, undefined);

  await user.click(within(notes).getByRole("button", { name: fill(b.delete, { name: "notes&1.txt" }) }));
  const before = listed(calls).length;
  await confirm(user, b.deleteOk);
  await waitFor(() => assert.deepEqual(calls.deleteStorageFile?.[0], [2, undefined, "files/notes&1.txt"]));
  await waitFor(() => assert.ok(listed(calls).length > before, "the list is read again"));
});

test("a failed delete says why and keeps the entry", async () => {
  const calls = stubDetail({ storage: shared, browse: tree });
  api.deleteStorageFile = (async () => { throw new Error("node gpu-01 lost its data lane"); }) as never;
  const { user } = renderDetail("/storage/2?tab=browse", false);
  await open("files");
  const notes = await row("notes&1.txt");
  await user.click(within(notes).getByRole("button", { name: fill(b.delete, { name: "notes&1.txt" }) }));
  const before = listed(calls).length;
  await confirm(user, b.deleteOk);
  assert.ok(await screen.findByText("node gpu-01 lost its data lane"));
  assert.equal(listed(calls).length, before);
});
