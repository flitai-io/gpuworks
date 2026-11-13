// Zeron SortableCollection (ui/src/components/ui/sortable-collection.tsx): an
// ordered list reordered by keyboard or pointer, with optional add, remove and
// edit. The app reaches it through DataTableViewOptions and the column
// settings drawer.
import { dom, pendingFrameCount } from "./helpers/dom.ts";
import assert from "node:assert/strict";
import { afterEach, beforeEach, test as nodeTest } from "node:test";
import React, { useState } from "react";

for (const name of ["KeyboardEvent", "Document", "AbortController", "AbortSignal"] as const) {
  Object.defineProperty(globalThis, name, { value: dom.window[name], configurable: true });
}
if (!("getAnimations" in dom.window.Element.prototype)) {
  Object.defineProperty(dom.window.Element.prototype, "getAnimations", { configurable: true, value: () => [] });
}

const { render, screen, fireEvent, waitFor, cleanup, within, act } = await import("@testing-library/react");
const { default: userEvent } = await import("@testing-library/user-event");
const { SortableCollection, reorderCollectionItems } = await import("../src/components/ui/sortable-collection.tsx");
type Item = import("../src/components/ui/sortable-collection.tsx").SortableCollectionItem;
const { resetAnimationFrameScheduler } = await import("@base-ui/utils/useAnimationFrame");

// jsdom lays nothing out: every row gets a 40px box stacked in list order, and
// elementFromPoint returns the row under the pointer's y.
const rows = () => [...document.querySelectorAll<HTMLElement>("[data-sortable-item-id]")];
const originalRect = dom.window.Element.prototype.getBoundingClientRect;
const originalFromPoint = document.elementFromPoint;
beforeEach(() => {
  resetAnimationFrameScheduler();
  dom.window.Element.prototype.getBoundingClientRect = function (this: Element) {
    const index = rows().indexOf(this.closest<HTMLElement>("[data-sortable-item-id]")!);
    const top = index < 0 ? 0 : index * 40;
    const height = index < 0 ? 0 : 40;
    return { x: 0, y: top, top, left: 0, width: 240, height, right: 240, bottom: top + height, toJSON() {} } as DOMRect;
  };
  document.elementFromPoint = (_x: number, y: number) => rows()[Math.floor(y / 40)] ?? null;
  (dom.window.Element.prototype as Element).setPointerCapture ??= () => {};
});
afterEach(() => {
  cleanup();
  resetAnimationFrameScheduler();
  dom.window.Element.prototype.getBoundingClientRect = originalRect;
  document.elementFromPoint = originalFromPoint;
});
// framer-motion's frame loop is process-wide: let each test's animations finish.
const test = (name: string, fn: () => unknown) => nodeTest(name, async () => {
  try { await fn(); } finally { await waitFor(() => assert.equal(pendingFrameCount(), 0)); }
});

const base: Item[] = [
  { id: "a", title: "Alpha" },
  { id: "b", title: "Beta", description: "second" },
  { id: "c", title: "Gamma" },
];
type Props = Partial<React.ComponentProps<typeof SortableCollection<Item>>> & { initial?: Item[]; log?: Item[][] };
function List({ initial = base, log, ...props }: Props) {
  const [items, setItems] = useState(initial);
  return <SortableCollection<Item> aria-label="Columns" items={items} onItemsChange={(next) => { log?.push(next); setItems(next); }} {...props} />;
}
const order = () => rows().map((row) => row.getAttribute("data-sortable-item-id")).join("");
const handle = (title: string) => screen.getByRole("button", { name: `Reorder ${title}` });
const announcement = () => document.querySelector("[aria-live=polite]")?.textContent;
function drag(from: number, toY: number, pointer: Partial<{ button: number }> = {}) {
  const h = rows()[from].querySelector<HTMLElement>("button[aria-label^=Reorder]")!;
  fireEvent.pointerDown(h, { isPrimary: true, button: 0, pointerId: 1, clientX: 10, clientY: from * 40 + 20, ...pointer });
  fireEvent.pointerMove(h, { isPrimary: true, pointerId: 1, clientX: 10, clientY: toY });
  return h;
}

test("reorderCollectionItems moves an item before or after another and leaves invalid moves alone", () => {
  assert.deepEqual(reorderCollectionItems(base, "a", "c").map((i) => i.id), ["b", "a", "c"]);
  assert.deepEqual(reorderCollectionItems(base, "a", "c", "after").map((i) => i.id), ["b", "c", "a"]);
  assert.equal(reorderCollectionItems(base, "a", "a"), base);
  assert.equal(reorderCollectionItems(base, "x", "a"), base);
});

test("items render as a list with titles and descriptions", () => {
  render(<List />);
  const list = screen.getByRole("list");
  const items = within(list).getAllByRole("listitem");
  assert.deepEqual(items.map((item) => item.textContent?.replace(/\s+/g, " ").trim()), ["Alpha", "Betasecond", "Gamma"]);
});

test("Space lifts an item, arrows move it, and Space places it", async () => {
  const log: Item[][] = [];
  const reorders: string[] = [];
  render(<List log={log} onReorder={(next) => reorders.push(next.map((i) => i.id).join(""))} />);
  const user = userEvent.setup();
  handle("Alpha").focus();
  await user.keyboard(" ");
  assert.equal(handle("Alpha").getAttribute("aria-pressed"), "true");
  assert.equal(announcement(), "Moving Alpha. Use up or down arrow keys to reorder.");
  assert.equal(rows()[0].getAttribute("data-state"), "dragging");
  await user.keyboard("{ArrowDown}");
  assert.equal(order(), "bac");
  assert.equal(announcement(), "Alpha moved to position 2.");
  await user.keyboard("{ArrowDown}");
  assert.equal(order(), "bca");
  await user.keyboard("{ArrowDown}");
  assert.equal(order(), "bca", "the last position is the end of the list");
  await user.keyboard("{ArrowUp}");
  assert.equal(order(), "bac");
  await user.keyboard(" ");
  assert.equal(handle("Alpha").getAttribute("aria-pressed"), null);
  assert.equal(announcement(), "Alpha placed at position 2.");
  assert.deepEqual(reorders, ["bac", "bca", "bac"]);
  assert.equal(log.length, 3);
});

test("Enter also lifts, Escape cancels and restores the original order", async () => {
  render(<List />);
  const user = userEvent.setup();
  handle("Gamma").focus();
  await user.keyboard("{Enter}");
  await user.keyboard("{ArrowUp}{ArrowUp}");
  assert.equal(order(), "cab");
  await user.keyboard("{Escape}");
  assert.equal(order(), "abc");
  assert.equal(announcement(), "Move cancelled.");
  await user.keyboard("{ArrowDown}");
  assert.equal(order(), "abc", "arrows do nothing once the item is down");
});

test("Escape without a lifted item and unrelated keys do nothing", async () => {
  const log: Item[][] = [];
  render(<List log={log} />);
  const user = userEvent.setup();
  handle("Beta").focus();
  await user.keyboard("{Escape}x{ArrowDown}");
  assert.equal(log.length, 0);
  assert.equal(announcement(), "");
});

test("moving focus away from a lifted item cancels the move", async () => {
  render(<List />);
  const user = userEvent.setup();
  handle("Alpha").focus();
  await user.keyboard(" {ArrowDown}");
  assert.equal(order(), "bac");
  await user.tab();
  assert.equal(order(), "abc");
  assert.equal(announcement(), "Move cancelled.");
});

test("cancelling keeps items added during the move after the original order", async () => {
  function Growing() {
    const [items, setItems] = useState(base);
    return <><button onClick={() => setItems((current) => [...current, { id: "d", title: "Delta" }])}>Grow</button>
      <SortableCollection<Item> items={items} onItemsChange={setItems} /></>;
  }
  render(<Growing />);
  handle("Alpha").focus();
  fireEvent.keyDown(handle("Alpha"), { key: " " });
  fireEvent.keyDown(handle("Alpha"), { key: "ArrowDown" });
  fireEvent.click(screen.getByRole("button", { name: "Grow" }));
  fireEvent.keyDown(handle("Alpha"), { key: "Escape" });
  assert.equal(order(), "abcd");
});

test("removing the lifted item ends the keyboard move", async () => {
  function Shrinking() {
    const [items, setItems] = useState(base);
    return <><button onClick={() => setItems((current) => current.filter((i) => i.id !== "a"))}>Drop alpha</button>
      <SortableCollection<Item> items={items} onItemsChange={setItems} /></>;
  }
  render(<Shrinking />);
  fireEvent.keyDown(handle("Alpha"), { key: " " });
  fireEvent.click(screen.getByRole("button", { name: "Drop alpha" }));
  await waitFor(() => assert.equal(announcement(), "Move cancelled."));
  fireEvent.keyDown(handle("Beta"), { key: " " });
  assert.equal(handle("Beta").getAttribute("aria-pressed"), "true", "a new move can start");
});

test("a non-draggable item has a disabled handle and ignores keyboard and pointer", () => {
  const log: Item[][] = [];
  render(<List log={log} initial={[{ id: "a", title: "Alpha", draggable: false }, ...base.slice(1)]} />);
  assert.equal((handle("Alpha") as HTMLButtonElement).disabled, true);
  fireEvent.keyDown(handle("Alpha"), { key: " " });
  drag(0, 100);
  fireEvent.pointerUp(handle("Alpha"), { pointerId: 1, clientX: 10, clientY: 100 });
  assert.equal(log.length, 0);
});

test("dragging a handle with the pointer shows a preview and a drop line, and drops after the lower half", async () => {
  const log: Item[][] = [];
  render(<List log={log} />);
  const h = drag(0, 105);
  assert.equal(announcement(), "Moving Alpha. Choose a new position.");
  const preview = document.querySelector<HTMLElement>("[data-slot=sortable-collection-drag-preview]");
  assert.ok(preview, "a drag preview follows the pointer");
  assert.equal(preview!.textContent, "Alpha");
  assert.match(preview!.style.transform, /translate3d\(0px, 85px, 0\)/);
  assert.ok(document.querySelector("[data-slot=sortable-collection-original-position]") === null, "the original slot is no longer the target");
  fireEvent.pointerUp(h, { pointerId: 1, clientX: 10, clientY: 105 });
  assert.equal(order(), "bca");
  assert.equal(announcement(), "Alpha moved to position 3.");
  assert.equal(document.querySelector("[data-slot=sortable-collection-drag-preview]") === null, true);
});

test("dropping on the upper half of a row places the item before it", () => {
  render(<List />);
  const h = drag(2, 45);
  fireEvent.pointerUp(h, { pointerId: 1, clientX: 10, clientY: 45 });
  assert.equal(order(), "acb");
});

test("hovering the original row marks the original position, and dropping there changes nothing", () => {
  const log: Item[][] = [];
  render(<List log={log} />);
  const h = drag(1, 50);
  assert.ok(document.querySelector("[data-slot=sortable-collection-original-position]"));
  fireEvent.pointerUp(h, { pointerId: 1, clientX: 10, clientY: 50 });
  assert.equal(log.length, 0);
});

test("dropping outside every row, or cancelling the pointer, keeps the order", () => {
  const log: Item[][] = [];
  render(<List log={log} />);
  let h = drag(0, 500);
  fireEvent.pointerUp(h, { pointerId: 1, clientX: 10, clientY: 500 });
  h = drag(0, 95);
  fireEvent.pointerCancel(h, { pointerId: 1 });
  assert.equal(document.querySelector("[data-slot=sortable-collection-drag-preview]") === null, true);
  fireEvent.pointerUp(h, { pointerId: 1, clientX: 10, clientY: 95 });
  assert.equal(log.length, 0);
  assert.equal(order(), "abc");
});

test("only the primary button starts a drag, and moves without a drag are ignored", () => {
  const log: Item[][] = [];
  render(<List log={log} />);
  const h = drag(0, 95, { button: 2 });
  fireEvent.pointerUp(h, { pointerId: 1, clientX: 10, clientY: 95 });
  assert.equal(document.querySelector("[data-slot=sortable-collection-drag-preview]") === null, true);
  assert.equal(log.length, 0);
});

test("a pointer drag cannot start while a keyboard move is in progress", () => {
  const log: Item[][] = [];
  render(<List log={log} />);
  fireEvent.keyDown(handle("Beta"), { key: " " });
  const h = drag(0, 95);
  fireEvent.pointerUp(h, { pointerId: 1, clientX: 10, clientY: 95 });
  assert.equal(log.length, 0);
});

test("removing the dragged item mid-drag ends the drag", async () => {
  function Shrinking() {
    const [items, setItems] = useState(base);
    return <><button onClick={() => setItems((current) => current.filter((i) => i.id !== "a"))}>Drop alpha</button>
      <SortableCollection<Item> items={items} onItemsChange={setItems} /></>;
  }
  render(<Shrinking />);
  drag(0, 95);
  fireEvent.click(screen.getByRole("button", { name: "Drop alpha" }));
  await waitFor(() => assert.equal(document.querySelector("[data-slot=sortable-collection-drag-preview]") === null, true));
});

test("the drag preview mirrors the item's icon, meta and handle placement", () => {
  render(<List dragHandlePosition="end" initial={[{ id: "a", title: "Alpha", description: "first", leadingIcon: <i data-testid="lead" />, meta: <b>meta</b> }, ...base.slice(1)]} />);
  drag(0, 95);
  const preview = document.querySelector<HTMLElement>("[data-slot=sortable-collection-drag-preview]")!;
  assert.ok(within(preview).getByTestId("lead"));
  assert.equal(within(preview).getByText("first").textContent, "first");
  assert.ok(within(preview).getByText("meta"));
  // The handle sits after the content in an end-placed list.
  const listItem = rows()[0];
  const buttons = within(listItem).getAllByRole("button");
  assert.equal(buttons[buttons.length - 1].getAttribute("aria-label"), "Reorder Alpha");
});

test("remove buttons remove items and announce it; non-removable items have none", async () => {
  const removed: string[] = [];
  render(<List onRemove={(item) => removed.push(item.id)} initial={[{ id: "a", title: "Alpha", removable: false }, ...base.slice(1)]} />);
  assert.ok(screen.queryByRole("button", { name: "Remove Alpha" }) === null);
  await userEvent.setup().click(screen.getByRole("button", { name: "Remove Beta" }));
  assert.equal(order(), "ac");
  assert.deepEqual(removed, ["b"]);
  assert.equal(announcement(), "Beta removed.");
});

test("a non-string title is announced generically and the reorder label can be localized", () => {
  render(<List reorderLabel={(title) => `Move ${title}`} initial={[{ id: "a", title: <span>Rich</span> }]} />);
  assert.ok(screen.getByRole("button", { name: "Move item" }));
  assert.ok(screen.getByRole("button", { name: "Remove item" }));
});

test("the edit action swaps in editing content, disables sorting, and can be closed", async () => {
  render(<List showEditAction renderEditingContent={(item, { close }) => <div><span>Editing {String(item.title)}</span><button onClick={close}>Done</button></div>}
    initial={[...base.slice(0, 2), { id: "c", title: "Gamma", editable: false }]} />);
  assert.ok(screen.queryByRole("button", { name: "Edit Gamma" }) === null, "a non-editable item has no edit action");
  const user = userEvent.setup();
  await user.click(screen.getByRole("button", { name: "Edit Alpha" }));
  await screen.findByText("Editing Alpha");
  assert.equal(rows()[0].getAttribute("data-state"), "editing");
  assert.ok(screen.getByRole("button", { name: "Finish editing Alpha" }));
  assert.equal((handle("Beta") as HTMLButtonElement).disabled, true, "sorting is disabled while editing");
  await user.click(screen.getByRole("button", { name: "Done" }));
  await waitFor(() => assert.ok(screen.queryByText("Editing Alpha") === null));
  await user.click(screen.getByRole("button", { name: "Edit Alpha" }));
  await screen.findByText("Editing Alpha");
  await user.click(screen.getByRole("button", { name: "Finish editing Alpha" }));
  await waitFor(() => assert.ok(screen.queryByText("Editing Alpha") === null));
});

test("removing the item being edited leaves edit mode", async () => {
  render(<List showEditAction renderEditingContent={() => <span>Editing</span>} />);
  const user = userEvent.setup();
  await user.click(screen.getByRole("button", { name: "Edit Alpha" }));
  await screen.findByText("Editing");
  await user.click(screen.getByRole("button", { name: "Remove Alpha" }));
  assert.equal((handle("Beta") as HTMLButtonElement).disabled, false);
});

test("an item removed from outside while edited ends editing", async () => {
  function External() {
    const [items, setItems] = useState(base);
    return <><button onClick={() => setItems((current) => current.slice(1))}>Drop first</button>
      <SortableCollection<Item> items={items} onItemsChange={setItems} showEditAction renderEditingContent={() => <span>Editing</span>} /></>;
  }
  render(<External />);
  const user = userEvent.setup();
  await user.click(screen.getByRole("button", { name: "Edit Alpha" }));
  await screen.findByText("Editing");
  await user.click(screen.getByRole("button", { name: "Drop first" }));
  await waitFor(() => assert.equal((handle("Beta") as HTMLButtonElement).disabled, false));
});

test("leading, content and action render props receive an action context", async () => {
  render(<List renderLeading={(item) => <input type="checkbox" aria-label={`Show ${String(item.title)}`} />}
    renderContent={(item, ctx) => <span>{String(item.title)}{ctx.editing ? " (editing)" : ""}</span>}
    renderEditingContent={(item, { close }) => <button onClick={close}>Save {String(item.title)}</button>}
    renderActions={(item, ctx) => <><button onClick={ctx.edit}>Rename {String(item.title)}</button><button onClick={ctx.remove}>Delete {String(item.title)}</button></>} />);
  assert.ok(screen.getByRole("checkbox", { name: "Show Alpha" }));
  const user = userEvent.setup();
  await user.click(screen.getByRole("button", { name: "Rename Beta" }));
  await user.click(await screen.findByRole("button", { name: "Save Beta" }));
  await waitFor(() => assert.ok(screen.queryByRole("button", { name: "Save Beta" }) === null));
  await user.click(screen.getByRole("button", { name: "Delete Gamma" }));
  assert.equal(order(), "ab");
});

test("edit from the action context is ignored when the item cannot be edited", async () => {
  render(<List renderActions={(item, ctx) => <button onClick={ctx.edit}>Rename {String(item.title)}</button>} />);
  await userEvent.setup().click(screen.getByRole("button", { name: "Rename Alpha" }));
  assert.equal(rows()[0].getAttribute("data-state"), null);
});

test("an empty list shows its empty state", () => {
  render(<List initial={[]} emptyState="Nothing here" />);
  assert.ok(screen.getByText("Nothing here"));
});

test("the add popover lists grouped options, marks added ones, filters by search and adds a choice", async () => {
  const added: string[] = [];
  render(<List addLabel="Add column" onAdd={(option) => added.push(option.id)} addOptions={[
    { id: "a", title: "Alpha", group: "Basic" },
    { id: "d", title: "Delta", description: "fourth", group: "Basic", meta: "new", leadingIcon: <i data-testid="opt-icon" /> },
    { id: "e", title: "Epsilon", disabled: true },
  ]} />);
  const user = userEvent.setup();
  await user.click(screen.getByRole("button", { name: "Add column" }));
  const search = await screen.findByRole("searchbox", { name: "Search items to add" });
  assert.ok(screen.getByText("Basic"));
  assert.ok(screen.getByText("Items"), "ungrouped options fall under Items");
  assert.ok(screen.getByText("Added"));
  assert.equal((screen.getByRole("button", { name: /^Alpha/ }) as HTMLButtonElement).disabled, true);
  assert.equal((screen.getByRole("button", { name: /^Epsilon/ }) as HTMLButtonElement).disabled, true);
  await user.type(search, "four");
  assert.ok(screen.queryByRole("button", { name: /^Alpha/ }) === null);
  await user.click(screen.getByRole("button", { name: /^Delta/ }));
  assert.deepEqual(added, ["d"]);
  await waitFor(() => assert.ok(screen.queryByRole("searchbox") === null));
});

test("the add search reports when nothing matches", async () => {
  render(<List addOptions={[{ id: "d", title: "Delta" }]} />);
  const user = userEvent.setup();
  await user.click(screen.getByRole("button", { name: "Add item" }));
  await user.type(await screen.findByRole("searchbox"), "zzz");
  assert.ok(screen.getByText("No matching items"));
});

test("duplicates can be allowed, and the add button is disabled at maxItems", async () => {
  const added: string[] = [];
  const { unmount } = render(<List allowDuplicates onAdd={(o) => added.push(o.id)} addOptions={[{ id: "a", title: "Alpha" }]} />);
  const user = userEvent.setup();
  await user.click(screen.getByRole("button", { name: "Add item" }));
  await user.click(await screen.findByRole("button", { name: /^Alpha/ }));
  assert.deepEqual(added, ["a"]);
  unmount();
  render(<List maxItems={3} addOptions={[{ id: "d", title: "Delta" }]} />);
  assert.equal((screen.getByRole("button", { name: "Add item" }) as HTMLButtonElement).disabled, true);
});
