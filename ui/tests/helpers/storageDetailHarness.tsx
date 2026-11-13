// Shared setup for the storage detail page tests: jsdom and locale first, then a stubbed api and a router that shows where navigation went.
import React from "react";
import { createI18n, dom } from "./dom.ts";
import { localStorage, modelFile, node, nodeStatus } from "./storageFixtures.ts";
import type { Storage, StorageFileList, StorageModelFile } from "../../src/storageTypes";
import { I18nextProvider } from "react-i18next";
import { list } from "./listPage.ts";

export const { i18n, locale } = await createI18n(["storage", "models"]);
const router = await import("react-router-dom");
const rtl = await import("@testing-library/react");
const { default: userEvent } = await import("@testing-library/user-event");
const { api, usePolling } = await import("../../src/api.ts");
const { ConfigProvider } = await import("antd");
const { default: StorageDetailDrawer } = await import("../../src/pages/storage/StorageDetailDrawer.tsx");
const { StorageDetailRedirect } = await import("../../src/pages/storage/storageLinks.tsx");
const { numericDetailID, useDetailParam } = await import("../../src/components/detail/useDetailParam.ts");

// The list page's drawer, without the list: what ?storage= opens, with the
// nodes the list page reads once and hands down.
export function DrawerHost() {
  const detail = useDetailParam("storage");
  const nodes = usePolling(api.nodes, 0);
  return <StorageDetailDrawer id={numericDetailID(detail.id)} tab={detail.tab} onTab={detail.setTab} onClose={() => detail.open(null)} onDeleted={() => detail.open(null)} nodes={nodes.data?.items ?? []} />;
}

export { api, rtl, dom, localStorage, modelFile, node, nodeStatus };
export const s = locale.storage;
const originals = { ...api };
export const restoreApi = () => Object.assign(api, originals);

export type Calls = Record<string, unknown[][]>;
export interface DetailStub {
  storage?: Storage | Error;
  files?: StorageModelFile[] | Error;
  admin?: boolean;
  planes?: unknown[];
  /** The browse tab's directory pages, one per query; an Error is thrown, a promise awaited. */
  browse?: (query: BrowseQuery) => StorageFileList | Error | Promise<StorageFileList>;
}
export interface BrowseQuery { node?: string; path: string; cursor?: string }

/** Stubs every call the detail page makes; the returned map records the arguments of each call. */
export function stubDetail(options: DetailStub = {}): Calls {
  const calls: Calls = {};
  const record = (name: string, result: unknown) => async (...args: unknown[]) => {
    (calls[name] ??= []).push(args);
    if (result instanceof Error) throw result;
    return typeof result === "function" ? (result as () => unknown)() : result;
  };
  api.me = record("me", { role: options.admin === false ? "viewer" : "admin" }) as never;
  api.storage = record("storage", options.storage ?? localStorage({ id: 1 })) as never;
  api.storageModelFiles = record("storageModelFiles", options.files instanceof Error ? options.files : list(options.files ?? [])) as never;
  api.nodes = record("nodes", list([node("n1", "gpu-01"), node("n2", "gpu-02")])) as never;
  api.storageControlPlanes = record("storageControlPlanes", list(options.planes ?? [])) as never;
  api.updateStorage = record("updateStorage", options.storage instanceof Error || !options.storage ? localStorage() : options.storage) as never;
  api.deleteStorage = record("deleteStorage", undefined) as never;
  api.clearStorageCache = record("clearStorageCache", { removed: 2, freed_bytes: 30 * 1024 ** 3 }) as never;
  api.deleteModelOnNode = record("deleteModelOnNode", undefined) as never;
  api.deleteModelInStorage = record("deleteModelInStorage", undefined) as never;
  api.scanStorage = record("scanStorage", { node_uuid: "n1", candidates: [], skipped: [], complete: true }) as never;
  const browse = options.browse ?? emptyListing;
  api.storageFiles = (async (id: number, query: BrowseQuery) => {
    (calls.storageFiles ??= []).push([id, { ...query }]);
    const page = await browse(query);
    if (page instanceof Error) throw page;
    return page;
  }) as never;
  api.storageFilesMkdir = record("storageFilesMkdir", { path: "files/new" }) as never;
  api.storageFilesRename = record("storageFilesRename", { path: "files/b" }) as never;
  api.deleteStorageFile = record("deleteStorageFile", undefined) as never;
  return calls;
}

/** An empty listing as the server answers it: something can be created only inside files/ (the region root included). */
export function emptyListing(query: BrowseQuery): StorageFileList {
  const writable = query.path === "files" || query.path.startsWith("files/");
  return { path: query.path, node_uuid: query.node ?? "n1", writable, items: [], next_cursor: "" };
}

function Where() {
  const l = router.useLocation();
  return <div data-testid="where">{l.pathname}{l.search}</div>;
}

export function renderDetail(path: string, motion = true) {
  const { MemoryRouter, Route, Routes } = router;
  const view = rtl.render(
    <ConfigProvider theme={{ token: { motion } }}><I18nextProvider i18n={i18n}>
      <MemoryRouter initialEntries={[path]}>
        <Routes>
          {/* The old detail address redirects to the drawer over the list. */}
          <Route path="/storage/:id" element={<StorageDetailRedirect />} />
          <Route path="/storage" element={<><DrawerHost /><Where /></>} />
        </Routes>
      </MemoryRouter>
    </I18nextProvider></ConfigProvider>,
  );
  return { view, user: userEvent.setup({ document: dom.window.document }) };
}

/** Confirm in the open popover that has the confirm button; a previous popover may still be fading out. */
export async function confirm(_user: unknown, label: string) {
  const { waitFor, within } = rtl;
  const ok = await waitFor(() => {
    const found = [...document.querySelectorAll(".ant-popover:not(.ant-popover-hidden)")]
      .flatMap((pop) => within(pop as HTMLElement).queryAllByRole("button", { name: new RegExp(`${label}$`) }));
    if (found.length === 0) throw new Error(`no confirm button ${label}`);
    return found.at(-1) as HTMLElement;
  });
  // user-event refuses a popover that is still fading out; the click itself is what matters here.
  rtl.fireEvent.click(ok);
}

export const where = () => rtl.screen.getByTestId("where").textContent;

/** The labels of the header's "more" menu, empty when it is not there or not open. */
export async function moreItems(): Promise<string[]> {
  const { screen, fireEvent, waitFor } = rtl;
  const more = screen.queryByRole("button", { name: new RegExp(s.actions.more) });
  if (!more) return [];
  fireEvent.click(more);
  await waitFor(() => { if (screen.queryAllByRole("menuitem").length === 0) throw new Error("menu not open"); });
  const labels = screen.queryAllByRole("menuitem").map((el) => (el.textContent ?? "").trim());
  // Closed by its toggle: in the drawer, Escape would close the drawer too.
  fireEvent.click(more);
  return labels;
}

/** Opens the header's "more" menu and picks an item by its label. */
export async function chooseMore(label: string) {
  const { screen, fireEvent, waitFor } = rtl;
  fireEvent.click(screen.getByRole("button", { name: new RegExp(s.actions.more) }));
  const item = await waitFor(() => {
    const found = screen.queryAllByRole("menuitem").filter((el) => (el.textContent ?? "").trim() === label);
    if (found.length === 0) throw new Error(`no menu item ${label}`);
    return found.at(-1) as HTMLElement;
  });
  fireEvent.click(item);
}

/** Presses the OK button of the confirmation dialog a menu item opened. */
export async function confirmDialog(label: string) {
  const { waitFor, within, fireEvent } = rtl;
  const ok = await waitFor(() => {
    // A dialog that is closing stays in the document for its animation; only
    // the one still open counts.
    const found = [...document.querySelectorAll(".ant-modal-confirm:not(.ant-zoom-leave)")]
      .flatMap((m) => within(m as HTMLElement).queryAllByRole("button", { name: new RegExp(`${label}$`) }));
    if (found.length === 0) throw new Error(`no dialog button ${label}`);
    return found.at(-1) as HTMLElement;
  });
  fireEvent.click(ok);
}

/** Expands a table row and returns its expanded detail: what moved out of the row. */
export function detailOf(row: HTMLElement): HTMLElement {
  const { within, fireEvent } = rtl;
  const button = within(row).queryByRole("button", { name: s.detail.expand });
  if (button) fireEvent.click(button);
  const next = row.nextElementSibling as HTMLElement | null;
  if (!next || !next.querySelector(".detail-extra")) throw new Error("the row has no expanded detail");
  return next;
}

/** The open modals: the detail drawer is a dialog too, so a bare dialog query is ambiguous. */
export const modals = () => rtl.screen.queryAllByRole("dialog").filter((dialog) => !dialog.querySelector(".detail-body"));
export const findModal = () => rtl.waitFor(() => {
  const open = modals();
  if (open.length === 0) throw new Error("no modal is open");
  return open.at(-1) as HTMLElement;
});
