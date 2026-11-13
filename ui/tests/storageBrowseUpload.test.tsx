// The browse tab's upload dialog driven end to end through a hand-driven XMLHttpRequest: progress, cancel, an existing target and its overwrite, unpacking, and leaving the tab.
import assert from "node:assert/strict";
import { afterEach, beforeEach, test } from "node:test";
import { modals, renderDetail, restoreApi, rtl, s, stubDetail, type BrowseQuery } from "./helpers/storageDetailHarness.tsx";
import { fileItem, nfsStorage, nodeStatus } from "./helpers/storageFixtures.ts";
import { FakeXHR, installFakeXHR, lastXHR } from "./helpers/fakeXhr.ts";
import type { StorageFileList } from "../src/storageTypes";

const { act, cleanup, fireEvent, screen, waitFor, within } = rtl;
let restoreXHR: () => void;
beforeEach(() => { restoreXHR = installFakeXHR(); });
afterEach(() => { cleanup(); restoreApi(); restoreXHR(); });
const b = s.browse;
const u = b.uploadDialog;
const fill = (text: string, values: Record<string, string | number>) => text.replace(/{{(\w+)}}/g, (_: string, key: string) => String(values[key]));

const shared = nfsStorage({ id: 2, name: "nfs-shared", nodes: { total: 1, ready: 1, problems: [], all: [nodeStatus("n1", "gpu-01", "ready")] } });
const listing = (query: BrowseQuery): StorageFileList => (query.path === ""
  ? { path: "", writable: false, items: [fileItem("files", { dir: true, writable: false, size: 0 })], next_cursor: "" }
  : { path: query.path, writable: true, items: [fileItem(`${query.path}/old.txt`)], next_cursor: "" });

/** Opens the upload dialog in files/ and picks a file of the given name and size. */
async function openUpload(name: string, size: number) {
  const calls = stubDetail({ storage: shared, browse: listing });
  const { user } = renderDetail("/storage/2?tab=browse", false);
  fireEvent.click(await screen.findByRole("button", { name: fill(b.open, { name: "files" }) }));
  await screen.findByText("old.txt");
  await user.click(screen.getByRole("button", { name: new RegExp(`${b.upload}$`) }));
  const dialog = await screen.findByRole("dialog", { name: fill(u.title, { path: "/files" }) });
  const input = dialog.querySelector("input[type=file]") as HTMLInputElement;
  await user.upload(input, new File([new Uint8Array(size)], name));
  await within(dialog).findByText(new RegExp(`^${name.replace(/\./g, "\\.")} · `));
  return { calls, user, dialog };
}

const startButton = (dialog: HTMLElement) => within(dialog).getByRole("button", { name: u.start });

test("an upload shows its progress, then the node's turn, and on success closes and lists again", async () => {
  const { calls, user, dialog } = await openUpload("notes.txt", 4096);
  assert.equal((within(dialog).getByLabelText(u.saveAs) as HTMLInputElement).value, "notes.txt");
  assert.ok(within(dialog).getByText(u.noExtract), "a plain file cannot be unpacked");
  await user.click(startButton(dialog));
  const xhr = lastXHR();
  assert.equal(xhr.url, "/api/v1/storages/2/files/upload?path=files%2Fnotes.txt");
  await act(async () => xhr.progress(1024, 4096));
  await waitFor(() => assert.equal(within(dialog).getByRole("progressbar").getAttribute("aria-valuenow"), "25"));
  assert.ok(within(dialog).getByText(fill(u.progress, { loaded: "1.0 KiB", total: "4.0 KiB" })));
  await act(async () => xhr.progress(4096, 4096));
  assert.ok(await within(dialog).findByText(u.processing));
  const before = (calls.storageFiles ?? []).length;
  await act(async () => xhr.respond(200, JSON.stringify({ path: "files/notes.txt", bytes: 4096, files: 1 })));
  await waitFor(() => assert.equal(modals().length, 0));
  assert.ok((calls.storageFiles ?? []).length > before, "the directory is read again");
});

test("cancel aborts the request in flight and lets the same file go again", async () => {
  const { user, dialog } = await openUpload("big.bin", 1000);
  await user.click(startButton(dialog));
  const first = lastXHR();
  await act(async () => first.progress(100, 1000));
  await user.click(within(dialog).getByRole("button", { name: u.cancelUpload }));
  assert.ok(first.aborted);
  assert.ok(await within(dialog).findByText(u.cancelled));
  assert.ok(within(dialog).queryByRole("progressbar") === null, "Expected the element to be absent");
  await user.click(startButton(dialog));
  assert.ok(lastXHR() !== first, "a new request");
  await act(async () => lastXHR().respond(200, JSON.stringify({ path: "files/big.bin", bytes: 1000, files: 1 })));
  await waitFor(() => assert.equal(modals().length, 0));
});

test("an existing target is reported; overwrite sends again with overwrite=true", async () => {
  const { user, dialog } = await openUpload("old.txt", 10);
  await user.click(startButton(dialog));
  await act(async () => lastXHR().respond(409, JSON.stringify({ code: "STORAGE_FILE_EXISTS", error: "files/old.txt exists" })));
  assert.ok(await within(dialog).findByText(fill(u.exists, { path: "/files/old.txt" })));
  assert.ok(within(dialog).getByText(u.overwriteHint));
  await user.click(within(dialog).getByRole("button", { name: u.overwrite }));
  assert.equal(lastXHR().url, "/api/v1/storages/2/files/upload?path=files%2Fold.txt&overwrite=true");
  await act(async () => lastXHR().respond(200, JSON.stringify({ path: "files/old.txt", bytes: 10, files: 1 })));
  await waitFor(() => assert.equal(modals().length, 0));
});

test("choosing another name after an existing target goes back to a plain upload", async () => {
  const { user, dialog } = await openUpload("old.txt", 10);
  await user.click(startButton(dialog));
  await act(async () => lastXHR().respond(409, JSON.stringify({ code: "STORAGE_FILE_EXISTS", error: "exists" })));
  await within(dialog).findByText(fill(u.exists, { path: "/files/old.txt" }));
  const name = within(dialog).getByLabelText(u.saveAs);
  await user.clear(name);
  await user.type(name, "new.txt");
  await user.click(startButton(dialog));
  assert.equal(lastXHR().url, "/api/v1/storages/2/files/upload?path=files%2Fnew.txt");
  await act(async () => lastXHR().respond(200, JSON.stringify({ path: "files/new.txt", bytes: 10, files: 1 })));
});

test("an archive can be unpacked into a directory named after it", async () => {
  const { user, dialog } = await openUpload("gsm8k.tar.gz", 20);
  const unpack = within(dialog).getByRole("checkbox", { name: fill(u.extract, { format: "tar.gz" }) });
  assert.equal((unpack as HTMLInputElement).checked, false, "unpacking is a choice, not the default");
  await user.click(unpack);
  assert.equal((within(dialog).getByLabelText(u.unpackInto) as HTMLInputElement).value, "gsm8k");
  assert.ok(within(dialog).getByText(fill(u.target, { path: "/files/gsm8k" })));
  await user.click(startButton(dialog));
  assert.equal(lastXHR().url, "/api/v1/storages/2/files/upload?path=files%2Fgsm8k&extract=tar.gz");
  await act(async () => lastXHR().respond(200, JSON.stringify({ path: "files/gsm8k", bytes: 20, files: 3 })));
  await waitFor(() => assert.equal(modals().length, 0));
});

test("a failure shows the reason; a broken connection says so", async () => {
  const { user, dialog } = await openUpload("a.zip", 5);
  await user.click(startButton(dialog));
  await act(async () => lastXHR().respond(422, JSON.stringify({ code: "STORAGE_FILE_EXTRACT_FAILED", error: "archive entry ../x escapes", params: { detail: "archive entry ../x escapes" } })));
  assert.ok(await within(dialog).findByText(u.failed));
  assert.ok(within(dialog).getByText(/archive entry \.\.\/x escapes/));
  await user.click(startButton(dialog));
  await act(async () => lastXHR().fail());
  assert.ok(await within(dialog).findByText(u.networkError));
});

test("Esc during an upload cancels it and closes the dialog", async () => {
  const { user, dialog } = await openUpload("big.bin", 1000);
  await user.click(startButton(dialog));
  const xhr = lastXHR();
  fireEvent.keyDown(dialog, { key: "Escape", code: "Escape", keyCode: 27 });
  await waitFor(() => assert.ok(xhr.aborted));
  await waitFor(() => assert.equal(modals().length, 0));
});

test("leaving the tab cancels a running upload", async () => {
  const { user, dialog } = await openUpload("big.bin", 1000);
  await user.click(startButton(dialog));
  const xhr = lastXHR();
  // The tabs are behind the modal for a pointer; the page can still switch (the address, browser back).
  fireEvent.click(screen.getByRole("tab", { name: s.detail.tab.overview }));
  await waitFor(() => assert.ok(xhr.aborted, "the request is aborted when the tab goes away"));
  assert.equal(FakeXHR.instances.length, 1);
});
