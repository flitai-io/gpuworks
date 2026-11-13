import { createI18n } from "./helpers/dom.ts";
import assert from "node:assert/strict";
import { afterEach, beforeEach, test } from "node:test";
import React, { useState } from "react";
import { I18nextProvider } from "react-i18next";

// The column settings drawer on its own: locked name and actions columns,
// checkboxes that hide and show, and moving a shown column with Zeron's
// sortable list, by pointer or keyboard.

const { i18n } = await createI18n([]);
const { cleanup, fireEvent, render, screen, waitFor, within } = await import("@testing-library/react");
const userEvent = (await import("@testing-library/user-event")).default;
const { default: ColumnSettingsDrawer } = await import("../src/components/resourceTable/ColumnSettingsDrawer.tsx");
type ColumnPrefs = import("../src/components/resourceTable/columnPrefs.ts").ColumnPrefs;

// jsdom lays nothing out. The sortable list finds the drop row under the
// pointer (elementFromPoint) and compares the pointer with its box, so each row
// gets the 40px box a vertical list would give it.
const rows = () => [...document.querySelectorAll<HTMLElement>("[data-sortable-item-id]")];
const original = Element.prototype.getBoundingClientRect;
const originalFromPoint = document.elementFromPoint;
beforeEach(() => {
  Element.prototype.getBoundingClientRect = function (this: Element) {
    const index = rows().indexOf(this.closest<HTMLElement>("[data-sortable-item-id]")!);
    const top = index < 0 ? 0 : index * 40;
    const height = index < 0 ? 0 : 40;
    return { x: 0, y: top, top, left: 0, width: 240, height, right: 240, bottom: top + height, toJSON() {} } as DOMRect;
  };
  document.elementFromPoint = (_x: number, y: number) => rows()[Math.floor(y / 40)] ?? null;
  Element.prototype.setPointerCapture ??= () => {};
});
afterEach(() => { cleanup(); Element.prototype.getBoundingClientRect = original; document.elementFromPoint = originalFromPoint; });

const columns = [
  { key: "name", label: "Name", role: "name" as const },
  { key: "status", label: "Status" },
  { key: "gpus", label: "GPUs" },
  { key: "labels", label: "Labels" },
  { key: "actions", label: "Actions", role: "actions" as const },
];
function Example({ changes, initial, resets, closes }: { changes: ColumnPrefs[]; initial?: Partial<ColumnPrefs>; resets?: () => void; closes?: () => void }) {
  const [prefs, setPrefs] = useState<ColumnPrefs>({ order: ["status", "gpus", "labels"], hidden: [], widths: {}, ...initial });
  return <I18nextProvider i18n={i18n}><ColumnSettingsDrawer open columns={columns} prefs={prefs}
    onChange={(next) => { changes.push(next); setPrefs(next); }} onReset={() => resets?.()} onClose={() => closes?.()} /></I18nextProvider>;
}
const last = <T,>(items: T[]): T | undefined => items[items.length - 1];
const shownOrder = () => [...screen.getByRole("region", { name: /^Shown/ }).querySelectorAll("[data-slot=locked-column], [data-slot=sortable-collection-content]")].map((label) => label.textContent);

test("the shown list keeps the name first and actions last, locked, and counts every shown column", async () => {
  const changes: ColumnPrefs[] = [];
  render(<Example changes={changes} initial={{ hidden: ["labels"] }} />);
  await screen.findByRole("dialog", { name: "Customize columns" });
  assert.deepEqual(shownOrder(), ["Name", "Status", "GPUs", "Actions"]);
  assert.ok(screen.getByRole("region", { name: "Shown (4)" }));
  assert.ok(screen.getByRole("region", { name: "All columns (4/5)" }));
  assert.equal(screen.queryByRole("button", { name: "Hide Name" }), null, "the name column cannot be hidden");
  assert.equal((screen.getByRole("checkbox", { name: "Actions" }) as HTMLElement).getAttribute("aria-disabled") ?? String((screen.getByRole("checkbox", { name: "Actions" }) as HTMLButtonElement).disabled), "true");
  assert.equal(screen.getByRole("checkbox", { name: "Labels" }).getAttribute("aria-checked"), "false");
});

test("hiding from the shown list and showing from the checkboxes update the preferences", async () => {
  const changes: ColumnPrefs[] = [];
  let resets = 0;
  render(<Example changes={changes} initial={{ hidden: ["labels"] }} resets={() => { resets++; }} />);
  await screen.findByRole("dialog", { name: "Customize columns" });
  const user = userEvent.setup();
  await user.click(screen.getByRole("button", { name: "Hide GPUs" }));
  assert.deepEqual(last(changes)?.hidden, ["labels", "gpus"]);
  assert.deepEqual(shownOrder(), ["Name", "Status", "Actions"]);
  const labels = screen.getByRole("checkbox", { name: "Labels" });
  labels.focus();
  await user.keyboard(" ");
  assert.deepEqual(last(changes)?.hidden, ["gpus"]);
  assert.deepEqual(shownOrder(), ["Name", "Status", "Labels", "Actions"]);
  await user.click(screen.getByRole("button", { name: "Restore defaults" }));
  assert.equal(resets, 1);
});

test("a shown column moves down from the keyboard: Space lifts it, ArrowDown moves it, Space drops it", async () => {
  const changes: ColumnPrefs[] = [];
  render(<Example changes={changes} />);
  await screen.findByRole("dialog", { name: "Customize columns" });
  const user = userEvent.setup();
  screen.getByRole("button", { name: "Reorder Status" }).focus();
  await user.keyboard(" ");
  await user.keyboard("{ArrowDown}");
  await user.keyboard(" ");
  await waitFor(() => assert.deepEqual(last(changes)?.order, ["gpus", "status", "labels"]));
  assert.deepEqual(shownOrder(), ["Name", "GPUs", "Status", "Labels", "Actions"]);
});

test("a keyboard move skips hidden columns and keeps them where they were", async () => {
  const changes: ColumnPrefs[] = [];
  render(<Example changes={changes} initial={{ hidden: ["gpus"] }} />);
  await screen.findByRole("dialog", { name: "Customize columns" });
  const user = userEvent.setup();
  screen.getByRole("button", { name: "Reorder Status" }).focus();
  await user.keyboard(" {ArrowDown} ");
  await waitFor(() => assert.deepEqual(last(changes)?.order, ["labels", "gpus", "status"]));
  assert.deepEqual(last(changes)?.hidden, ["gpus"], "hiding is untouched");
});

// A pointer drag: press on the handle, move over the lower half of a row,
// release there. The handle captures the pointer, so every event is its own.
function drag(handle: HTMLElement, rows: number) {
  const from = 20 + [...document.querySelectorAll("[data-sortable-item-id]")].indexOf(handle.closest("[data-sortable-item-id]")!) * 40;
  fireEvent.pointerDown(handle, { isPrimary: true, button: 0, pointerId: 1, clientX: 10, clientY: from });
  fireEvent.pointerMove(handle, { isPrimary: true, pointerId: 1, clientX: 10, clientY: from + rows * 40 + 10 });
  fireEvent.pointerUp(handle, { isPrimary: true, pointerId: 1, clientX: 10, clientY: from + rows * 40 + 10 });
}

test("dragging a shown column onto the next one moves it there", async () => {
  const changes: ColumnPrefs[] = [];
  render(<Example changes={changes} />);
  await screen.findByRole("dialog", { name: "Customize columns" });
  drag(screen.getByRole("button", { name: "Reorder Status" }), 1);
  await waitFor(() => assert.deepEqual(last(changes)?.order, ["gpus", "status", "labels"]));
  assert.deepEqual(shownOrder(), ["Name", "GPUs", "Status", "Labels", "Actions"]);
});

test("dragging a column back onto itself changes nothing", async () => {
  const changes: ColumnPrefs[] = [];
  render(<Example changes={changes} />);
  await screen.findByRole("dialog", { name: "Customize columns" });
  drag(screen.getByRole("button", { name: "Reorder Status" }), 0);
  assert.deepEqual(changes, []);
  assert.deepEqual(shownOrder(), ["Name", "Status", "GPUs", "Labels", "Actions"]);
});

test("lifting a column from the keyboard and dropping it in place changes nothing", async () => {
  const changes: ColumnPrefs[] = [];
  render(<Example changes={changes} />);
  await screen.findByRole("dialog", { name: "Customize columns" });
  const user = userEvent.setup();
  const handle = screen.getByRole("button", { name: "Reorder GPUs" });
  handle.focus();
  await user.keyboard(" ");
  await waitFor(() => assert.equal(handle.getAttribute("aria-pressed"), "true"));
  await user.keyboard(" ");
  await waitFor(() => assert.notEqual(handle.getAttribute("aria-pressed"), "true"));
  assert.deepEqual(changes, []);
  assert.deepEqual(shownOrder(), ["Name", "Status", "GPUs", "Labels", "Actions"]);
});

test("Escape during a keyboard move cancels the move and leaves the drawer open", async () => {
  const changes: ColumnPrefs[] = [];
  let closes = 0;
  render(<Example changes={changes} closes={() => { closes++; }} />);
  await screen.findByRole("dialog", { name: "Customize columns" });
  const user = userEvent.setup();
  const handle = screen.getByRole("button", { name: "Reorder GPUs" });
  handle.focus();
  await user.keyboard(" ");
  await waitFor(() => assert.equal(handle.getAttribute("aria-pressed"), "true"));
  await user.keyboard("{ArrowUp}");
  await waitFor(() => assert.deepEqual(last(changes)?.order, ["gpus", "status", "labels"]));
  await user.keyboard("{Escape}");
  await waitFor(() => assert.deepEqual(last(changes)?.order, ["status", "gpus", "labels"]), "the move is undone");
  assert.equal(closes, 0, "Escape cancels the move, not the drawer");
  assert.ok(screen.getByRole("dialog", { name: "Customize columns" }));
});
