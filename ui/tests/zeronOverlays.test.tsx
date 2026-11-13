// Zeron overlay primitives (ui/src/components/ui): the dropdown menu and its
// MenuItem rows, popover, tooltip and dialog, driven the way the console uses
// them (LanguageSwitcher, the dashboard "more" menu, ConfirmPopover, NoticeBell,
// SideDrawer, BatchCreateModal) and then through their remaining public props.
import assert from "node:assert/strict";
import { after, afterEach, beforeEach, test } from "node:test";
import React, { useState } from "react";
import { dom } from "./helpers/dom.ts";
import { trackMessageChannels } from "./helpers/trackedMessageChannels.ts";

const closeChannels = trackMessageChannels();
for (const name of ["KeyboardEvent", "PointerEvent", "MouseEvent", "FocusEvent", "Document", "HTMLDivElement", "HTMLSpanElement", "DocumentFragment", "Text", "Event", "CustomEvent", "AbortController", "AbortSignal"] as const) {
  if (dom.window[name]) Object.defineProperty(globalThis, name, { value: dom.window[name], configurable: true });
}
// jsdom has no Web Animations API; Base UI asks which animations are running before unmounting a popup.
if (!("getAnimations" in dom.window.Element.prototype)) {
  Object.defineProperty(dom.window.Element.prototype, "getAnimations", { configurable: true, value: () => [] });
}
// jsdom has no layout. Menu rows report a 32px-tall box stacked by their index,
// so the dropdown's proximity hover can measure them and resolve the pointer.
const proto = dom.window.HTMLElement.prototype;
const originalOffsets = Object.fromEntries((["offsetHeight", "offsetWidth", "offsetTop", "offsetLeft"] as const).map((key) => [key, Object.getOwnPropertyDescriptor(proto, key)]));
const rowIndex = (element: HTMLElement) => {
  const attr = element.getAttribute("data-proximity-index");
  return attr === null ? null : Number(attr);
};
Object.defineProperties(proto, {
  offsetHeight: { configurable: true, get(this: HTMLElement) { return rowIndex(this) === null ? 0 : 32; } },
  offsetWidth: { configurable: true, get(this: HTMLElement) { return rowIndex(this) === null ? 0 : 200; } },
  offsetTop: { configurable: true, get(this: HTMLElement) { const i = rowIndex(this); return i === null ? 0 : 4 + i * 34; } },
  offsetLeft: { configurable: true, get(this: HTMLElement) { return rowIndex(this) === null ? 0 : 4; } },
});
const originalRect = dom.window.Element.prototype.getBoundingClientRect;
dom.window.Element.prototype.getBoundingClientRect = function (this: Element) {
  const i = this instanceof dom.window.HTMLElement ? rowIndex(this) : null;
  if (i !== null) return { x: 4, y: 4 + i * 34, left: 4, top: 4 + i * 34, width: 200, height: 32, right: 204, bottom: 36 + i * 34, toJSON() {} } as DOMRect;
  const role = this.getAttribute("role");
  if (role === "menu" || role === "group") return { x: 0, y: 0, left: 0, top: 0, width: 208, height: 200, right: 208, bottom: 200, toJSON() {} } as DOMRect;
  // Triggers and popups: a small box mid-viewport, so every side has room and nothing flips.
  return { x: 400, y: 300, left: 400, top: 300, width: 100, height: 40, right: 500, bottom: 340, toJSON() {} } as DOMRect;
};
const html = dom.window.document.documentElement;
Object.defineProperty(html, "clientWidth", { configurable: true, value: 1024 });
Object.defineProperty(html, "clientHeight", { configurable: true, value: 768 });

const { cleanup, render, screen, waitFor, fireEvent, act, within } = await import("@testing-library/react");
const userEvent = (await import("@testing-library/user-event")).default;
// helpers/dom.ts drops a finished test's pending animation frames; Base UI's
// process-wide frame scheduler would then wait forever for the dropped one.
const { resetAnimationFrameScheduler } = await import("@base-ui/utils/useAnimationFrame");
const { Dropdown, DropdownMenu, DropdownTrigger, DropdownContent, DropdownLabel, DropdownSeparator, useDropdown, useDropdownMaybe } = await import("../src/components/ui/dropdown.tsx");
const { MenuItem } = await import("../src/components/ui/menu-item.tsx");
const { Popover, PopoverTrigger, PopoverContent, PopoverTitle, PopoverDescription, PopoverHeader, PopoverFooter, PopoverClose, PopoverPortalContainer } = await import("../src/components/ui/popover.tsx");
const { Tooltip, TooltipProvider, TooltipPortalContainer } = await import("../src/components/ui/tooltip.tsx");
const { Dialog, DialogTrigger, DialogContent, DialogHeader, DialogFooter, DialogTitle, DialogDescription, DialogClose } = await import("../src/components/ui/dialog.tsx");
const { PortalContainerProvider } = await import("../src/lib/portal-container-context.tsx");

beforeEach(() => resetAnimationFrameScheduler());
afterEach(() => { cleanup(); resetAnimationFrameScheduler(); });
after(() => {
  for (const [key, descriptor] of Object.entries(originalOffsets)) if (descriptor) Object.defineProperty(proto, key, descriptor);
  dom.window.Element.prototype.getBoundingClientRect = originalRect;
  closeChannels();
});

const gone = (query: () => Element | null) => waitFor(() => assert.ok(query() === null));

// ---------------------------------------------------------------------------
// Dropdown menu (popup) + MenuItem
// ---------------------------------------------------------------------------

function LanguageMenu({ onPick, initial = 0 }: { onPick?: (key: string) => void; initial?: number }) {
  const [current, setCurrent] = useState(initial);
  const languages = ["English", "Deutsch", "Français"];
  return <DropdownMenu>
    <DropdownTrigger render={<button type="button">Language</button>} />
    <DropdownContent align="end" checkedIndex={current}>
      {languages.map((label, i) => <MenuItem key={label} index={i} label={label} checked={i === current} onSelect={() => { setCurrent(i); onPick?.(label); }} />)}
    </DropdownContent>
  </DropdownMenu>;
}

test("dropdown menu opens from its trigger, announces the checked option and closes after a pick", async () => {
  const picks: string[] = [];
  render(<LanguageMenu onPick={(key) => picks.push(key)} />);
  const user = userEvent.setup();
  const trigger = screen.getByRole("button", { name: "Language" });
  assert.equal(trigger.getAttribute("aria-expanded"), "false");
  await user.click(trigger);
  const menu = await screen.findByRole("menu");
  assert.equal(trigger.getAttribute("aria-expanded"), "true");
  const options = within(menu).getAllByRole("menuitemradio");
  assert.deepEqual(options.map((o) => o.getAttribute("aria-label")), ["English", "Deutsch", "Français"]);
  assert.deepEqual(options.map((o) => o.getAttribute("aria-checked")), ["true", "false", "false"]);
  await user.click(options[1]);
  assert.deepEqual(picks, ["Deutsch"]);
  await gone(() => screen.queryByRole("menu"));
  await user.click(trigger);
  const reopened = await screen.findByRole("menu");
  assert.deepEqual(within(reopened).getAllByRole("menuitemradio").map((o) => o.getAttribute("aria-checked")), ["false", "true", "false"]);
});

test("dropdown menu is operable from the keyboard and Escape dismisses it", async () => {
  const picks: string[] = [];
  render(<LanguageMenu onPick={(key) => picks.push(key)} />);
  const user = userEvent.setup();
  const trigger = screen.getByRole("button", { name: "Language" });
  trigger.focus();
  await user.keyboard("{Enter}");
  await screen.findByRole("menu");
  // Opening from the keyboard lands on the first row.
  await waitFor(() => assert.equal(document.activeElement?.getAttribute("aria-label"), "English"));
  await user.keyboard("{ArrowDown}");
  await waitFor(() => assert.equal(document.activeElement?.getAttribute("aria-label"), "Deutsch"));
  await user.keyboard("{Enter}");
  assert.deepEqual(picks, ["Deutsch"]);
  await gone(() => screen.queryByRole("menu"));
  await user.click(trigger);
  await screen.findByRole("menu");
  await user.keyboard("{Escape}");
  await gone(() => screen.queryByRole("menu"));
  assert.deepEqual(picks, ["Deutsch"]);
});

test("action items select without a checked state, a disabled row ignores activation and closeOnClick false keeps the menu open", async () => {
  const calls: string[] = [];
  render(<DropdownMenu>
    <DropdownTrigger render={<button type="button">More</button>} />
    <DropdownContent>
      <DropdownLabel>Attention</DropdownLabel>
      <MenuItem index={0} label="Pin" closeOnClick={false} onSelect={() => calls.push("pin")} onClick={() => calls.push("click")} />
      <DropdownSeparator />
      <MenuItem index={1} label="Archived" disabled onSelect={() => calls.push("archived")} />
      <MenuItem index={2} label="Open alert" trailing={<span>→</span>} onSelect={() => calls.push("open")} />
    </DropdownContent>
  </DropdownMenu>);
  const user = userEvent.setup();
  await user.click(screen.getByRole("button", { name: "More" }));
  const menu = await screen.findByRole("menu");
  assert.ok(within(menu).getByText("Attention"));
  assert.ok(within(menu).getByRole("separator"));
  assert.equal(within(menu).queryAllByRole("menuitemradio").length, 0);
  const [pin, archived, open] = within(menu).getAllByRole("menuitem");
  assert.equal(pin.hasAttribute("aria-checked"), false);
  await user.click(pin);
  assert.deepEqual(calls, ["click", "pin"]);
  assert.ok(screen.queryByRole("menu"));
  assert.equal(archived.getAttribute("aria-disabled"), "true");
  fireEvent.click(archived);
  assert.deepEqual(calls, ["click", "pin"]);
  assert.ok(within(open).getByText("→"));
  await user.click(open);
  assert.deepEqual(calls, ["click", "pin", "open"]);
  await gone(() => screen.queryByRole("menu"));
});

test("controlled dropdown menu reports open changes and a disabled one never opens", async () => {
  const changes: boolean[] = [];
  function Controlled() {
    const [open, setOpen] = useState(false);
    return <DropdownMenu open={open} onOpenChange={(next) => { changes.push(next); setOpen(next); }}>
      <DropdownTrigger render={<button type="button">Controlled</button>} />
      <DropdownContent side="top" align="center"><MenuItem index={0} label="Only" /></DropdownContent>
    </DropdownMenu>;
  }
  const { unmount } = render(<><Controlled /><DropdownMenu disabled>
    <DropdownTrigger render={<button type="button">Locked</button>} />
    <DropdownContent><MenuItem index={0} label="Hidden" /></DropdownContent>
  </DropdownMenu></>);
  const user = userEvent.setup();
  await user.click(screen.getByRole("button", { name: "Locked" }));
  assert.equal(screen.queryByRole("menuitem", { name: "Hidden" }), null);
  await user.click(screen.getByRole("button", { name: "Controlled" }));
  await screen.findByRole("menuitem", { name: "Only" });
  await user.keyboard("{Escape}");
  await gone(() => screen.queryByRole("menu"));
  assert.deepEqual(changes, [true, false]);
  unmount();
});

test("dropdown menu opened by default highlights the row under the pointer", async () => {
  render(<DropdownMenu defaultOpen>
    <DropdownTrigger render={<button type="button">Rows</button>} />
    <DropdownContent checkedIndex={0}>
      <MenuItem index={0} label="First" checked />
      <MenuItem index={1} label="Second" checked={false} />
    </DropdownContent>
  </DropdownMenu>);
  const menu = await screen.findByRole("menu");
  const second = within(menu).getByRole("menuitemradio", { name: "Second" });
  // Wait for the hook to measure the rows, then point at the second one.
  await new Promise((resolve) => setTimeout(resolve, 60));
  fireEvent.mouseEnter(menu);
  fireEvent.mouseMove(second, { clientX: 50, clientY: 52 });
  await waitFor(() => assert.equal(second.querySelector("span span")?.className.includes("text-fg-default"), true));
  // A move from content outside the popup (e.g. a submenu portal) clears the highlight.
  const outside = document.createElement("div");
  document.body.append(outside);
  fireEvent.mouseMove(outside, { clientX: 50, clientY: 52 });
  fireEvent.mouseLeave(menu);
  await waitFor(() => assert.equal(second.querySelector("span span")?.className.includes("text-fg-muted"), true));
  outside.remove();
});

test("dropdown content outside a DropdownMenu fails loudly", () => {
  const original = console.error;
  console.error = () => {};
  try {
    assert.throws(() => render(<DropdownContent><span>Loose</span></DropdownContent>), /must be inside <DropdownMenu>/);
  } finally { console.error = original; }
});

// ---------------------------------------------------------------------------
// Inline Dropdown panel + MenuItem
// ---------------------------------------------------------------------------

test("inline dropdown is a named group whose rows rove focus with the arrow, Home and End keys", async () => {
  const picks: string[] = [];
  const Icon = (props: { size?: number; strokeWidth?: number; className?: string }) => <svg data-testid="icon" data-stroke={props.strokeWidth} />;
  render(<Dropdown aria-label="Sort" checkedIndex={1}>
    <MenuItem index={0} label="Name" icon={Icon} checked={false} onSelect={() => picks.push("name")} />
    <MenuItem index={1} label="Created" icon={Icon} checked onSelect={() => picks.push("created")} />
    <MenuItem index={2} label="Updated" leading={<span>L</span>} checked={false} onSelect={() => picks.push("updated")} />
  </Dropdown>);
  const group = screen.getByRole("group", { name: "Sort" });
  const rows = within(group).getAllByRole("menuitemradio");
  assert.deepEqual(rows.map((r) => r.tabIndex), [-1, 0, -1]);
  assert.deepEqual(screen.getAllByTestId("icon").map((i) => i.getAttribute("data-stroke")), ["1.5", "2"]);
  assert.ok(within(rows[2]).getByText("L"));
  rows[1].focus();
  fireEvent.keyDown(rows[1], { key: "ArrowDown" });
  assert.ok(document.activeElement === rows[2]);
  fireEvent.keyDown(rows[2], { key: "ArrowRight" });
  assert.ok(document.activeElement === rows[0]);
  fireEvent.keyDown(rows[0], { key: "ArrowUp" });
  assert.ok(document.activeElement === rows[2]);
  fireEvent.keyDown(rows[2], { key: "ArrowLeft" });
  assert.ok(document.activeElement === rows[1]);
  fireEvent.keyDown(rows[1], { key: "Home" });
  assert.ok(document.activeElement === rows[0]);
  fireEvent.keyDown(rows[0], { key: "End" });
  assert.ok(document.activeElement === rows[2]);
  fireEvent.keyDown(rows[2], { key: "Enter" });
  fireEvent.keyDown(rows[0], { key: " " });
  fireEvent.keyDown(rows[0], { key: "a" });
  assert.deepEqual(picks, ["updated", "name"]);
  // Keys from a non-row descendant are left alone.
  fireEvent.keyDown(group, { key: "ArrowDown" });
  assert.ok(document.activeElement === rows[2]);
  fireEvent.blur(rows[0], { relatedTarget: rows[1] });
  fireEvent.blur(rows[0], { relatedTarget: document.body });
});

test("inline dropdown rows act on click, skip disabled rows and highlight under the pointer", async () => {
  const calls: string[] = [];
  render(<Dropdown aria-label="Actions">
    <MenuItem index={0} label="Copy" onClick={() => calls.push("click")} onSelect={() => calls.push("copy")} />
    <MenuItem index={1} label="Delete" disabled onSelect={() => calls.push("delete")} />
  </Dropdown>);
  const group = screen.getByRole("group", { name: "Actions" });
  const [copy, del] = within(group).getAllByRole("menuitem");
  assert.equal(copy.tabIndex, 0);
  assert.equal(del.tabIndex, -1);
  assert.equal(del.getAttribute("aria-disabled"), "true");
  fireEvent.click(copy);
  fireEvent.click(del);
  fireEvent.keyDown(del, { key: "Enter" });
  assert.deepEqual(calls, ["click", "copy"]);
  await new Promise((resolve) => setTimeout(resolve, 60));
  fireEvent.mouseEnter(group);
  fireEvent.mouseMove(group, { clientX: 50, clientY: 10 });
  await waitFor(() => assert.equal(copy.querySelector("span span")?.className.includes("text-fg-default"), true));
  fireEvent.mouseLeave(group);
  await waitFor(() => assert.equal(copy.querySelector("span span")?.className.includes("text-fg-muted"), true));
});

test("menu rows outside any dropdown fail loudly while the maybe hook returns null", () => {
  let seen: unknown = "unset";
  function Probe() { seen = useDropdownMaybe(); return null; }
  function Strict() { useDropdown(); return null; }
  render(<Probe />);
  assert.equal(seen, null);
  const original = console.error;
  console.error = () => {};
  try {
    assert.throws(() => render(<Strict />), /useDropdown must be used within a Dropdown/);
    assert.throws(() => render(<MenuItem index={0} label="Loose" />), /useDropdown must be used within a Dropdown/);
  } finally { console.error = original; }
});

// ---------------------------------------------------------------------------
// Popover
// ---------------------------------------------------------------------------

test("click popover opens a labelled dialog, reports changes and closes from its close button or Escape", async () => {
  const changes: boolean[] = [];
  render(<Popover onOpenChange={(open) => changes.push(open)}>
    <PopoverTrigger>Remove</PopoverTrigger>
    <PopoverContent>
      <PopoverHeader><PopoverTitle>Remove node?</PopoverTitle><PopoverDescription>Workloads stop.</PopoverDescription></PopoverHeader>
      <PopoverFooter><PopoverClose>Cancel</PopoverClose></PopoverFooter>
    </PopoverContent>
  </Popover>);
  const user = userEvent.setup();
  const trigger = screen.getByRole("button", { name: "Remove" });
  await user.click(trigger);
  const dialog = await screen.findByRole("dialog", { name: "Remove node?" });
  assert.equal(dialog.getAttribute("aria-describedby") !== null, true);
  assert.equal(document.getElementById(dialog.getAttribute("aria-describedby")!)?.textContent, "Workloads stop.");
  await user.click(within(dialog).getByRole("button", { name: "Cancel" }));
  await gone(() => screen.queryByRole("dialog"));
  await user.click(trigger);
  await screen.findByRole("dialog");
  await user.keyboard("{Escape}");
  await gone(() => screen.queryByRole("dialog"));
  assert.deepEqual(changes, [true, false, true, false]);
});

test("hover popover opens on pointer hover after its delay and closes when the pointer leaves", async () => {
  render(<Popover trigger="hover" hoverDelay={10} closeDelay={10}>
    <PopoverTrigger>Notices</PopoverTrigger>
    <PopoverContent><span>Two unread</span></PopoverContent>
  </Popover>);
  const trigger = screen.getByRole("button", { name: "Notices" });
  fireEvent.pointerEnter(trigger, { pointerType: "mouse" });
  fireEvent.mouseEnter(trigger);
  fireEvent.mouseMove(trigger);
  await screen.findByText("Two unread");
  fireEvent.pointerLeave(trigger, { pointerType: "mouse" });
  fireEvent.mouseLeave(trigger);
  await gone(() => screen.queryByText("Two unread"));
});

test("liquid popover draws its connector for every side and alignment", async () => {
  const placedSides = new Set<string>();
  for (const [side, align] of [["bottom", "start"], ["top", "end"], ["left", "center"], ["left", "end"], ["right", "start"]] as const) {
    const { unmount } = render(<Popover defaultOpen liquid gooStrength={8}>
      <PopoverTrigger>Anchor</PopoverTrigger>
      <PopoverContent side={side} align={align} sideOffset={4} surfaceShadow="overlay" surfaceClassName="custom-surface"><span>Panel {side}</span></PopoverContent>
    </Popover>);
    const panel = await screen.findByText(`Panel ${side}`);
    const content = panel.closest("[data-slot='popover-content']")!;
    const blur = content.querySelector("feGaussianBlur");
    assert.equal(blur?.getAttribute("stdDeviation"), "8");
    // The connector reaches back toward the trigger from whichever side the panel settled on.
    const placed = content.getAttribute("data-side");
    placedSides.add(`${side}->${placed}`);
    const style = (content.querySelector("span.rounded-full") as HTMLElement).getAttribute("style") ?? "";
    const facing = { bottom: "top", top: "bottom", left: "right", right: "left" }[placed as "top"];
    assert.match(style, new RegExp(`${facing}: -4px`));
    unmount();
  }
  assert.ok(["top", "bottom", "right"].every((placed) => [...placedSides].some((entry) => entry.endsWith(`->${placed}`))));
  // The connector retracts with the panel when it closes.
  render(<Popover defaultOpen liquid><PopoverTrigger>Anchor</PopoverTrigger><PopoverContent><PopoverClose>Hide</PopoverClose></PopoverContent></Popover>);
  fireEvent.click(await screen.findByRole("button", { name: "Hide" }));
  await gone(() => screen.queryByRole("button", { name: "Hide" }));
});

test("popover content can opt out of liquid and renders into an explicit or ambient container", async () => {
  const host = document.createElement("div");
  const ambient = document.createElement("div");
  document.body.append(host, ambient);
  const { unmount } = render(<Popover defaultOpen liquid>
    <PopoverTrigger>Anchor</PopoverTrigger>
    <PopoverContent liquid={false} container={host}><span>Hosted</span></PopoverContent>
  </Popover>);
  const hosted = await screen.findByText("Hosted");
  assert.ok(host.contains(hosted));
  assert.equal(hosted.closest("[data-slot='popover-content']")!.querySelector("filter"), null);
  unmount();
  const second = render(<PopoverPortalContainer value={ambient}><Popover defaultOpen>
    <PopoverTrigger openOnHover={false} delay={0} closeDelay={0}>Anchor</PopoverTrigger>
    <PopoverContent><span>Ambient</span></PopoverContent>
  </Popover></PopoverPortalContainer>);
  assert.ok(ambient.contains(await screen.findByText("Ambient")));
  second.unmount();
  host.remove();
  ambient.remove();
});

// ---------------------------------------------------------------------------
// Tooltip
// ---------------------------------------------------------------------------

test("tooltip appears on hover and on keyboard focus and describes its trigger", async () => {
  const changes: boolean[] = [];
  render(<Tooltip content="Cluster health" delayDuration={0} onOpenChange={(open) => changes.push(open)}><button type="button">Info</button></Tooltip>);
  const user = userEvent.setup();
  const trigger = screen.getByRole("button", { name: "Info" });
  assert.equal(trigger.getAttribute("aria-describedby"), null);
  await user.hover(trigger);
  const tip = await screen.findByRole("tooltip");
  assert.equal(tip.textContent, "Cluster health");
  assert.equal(trigger.getAttribute("aria-describedby"), tip.id);
  await user.unhover(trigger);
  await gone(() => screen.queryByRole("tooltip"));
  assert.equal(trigger.getAttribute("aria-describedby"), null);
  await user.tab();
  assert.ok(document.activeElement === trigger);
  await screen.findByRole("tooltip");
  await user.tab();
  await gone(() => screen.queryByRole("tooltip"));
  assert.deepEqual(changes, [true, false, true, false]);
});

test("an open tooltip keeps the trigger's own aria-describedby and appends itself", { todo: "Tooltip computes `${child aria-describedby} ${contentId}` but passes it to Trigger with render={children}; the child's own aria-describedby wins the prop merge, so the open tooltip is not announced" }, async () => {
  render(<Tooltip content="Cluster health" delayDuration={0}><button type="button" aria-describedby="extra">Info</button></Tooltip>);
  const trigger = screen.getByRole("button", { name: "Info" });
  await userEvent.setup().hover(trigger);
  const tip = await screen.findByRole("tooltip");
  assert.equal(trigger.getAttribute("aria-describedby"), `extra ${tip.id}`);
});

test("forceOpen pins a tooltip open or shut regardless of hover", async () => {
  const { rerender } = render(<Tooltip content="Pinned" side="bottom" forceOpen><button type="button" id="pin">Pin</button></Tooltip>);
  const tip = await screen.findByRole("tooltip");
  assert.equal(tip.id, "pin-tooltip");
  assert.equal(screen.getByRole("button", { name: "Pin" }).getAttribute("aria-describedby"), "pin-tooltip");
  rerender(<Tooltip content="Pinned" side="bottom" forceOpen={false}><button type="button" id="pin">Pin</button></Tooltip>);
  await gone(() => screen.queryByRole("tooltip"));
  const user = userEvent.setup();
  await user.hover(screen.getByRole("button", { name: "Pin" }));
  await new Promise((resolve) => setTimeout(resolve, 260));
  assert.equal(screen.queryByRole("tooltip") === null, true);
});

test("tooltips under a provider share its delay and render into a portal container on any side", async () => {
  const host = document.createElement("div");
  document.body.append(host);
  try {
    // 600ms, well past the 200ms a tooltip waits without a provider. (An open sibling would let the group skip the delay.)
    render(<TooltipProvider delayDuration={600} skipDelayDuration={0}><TooltipPortalContainer value={host}>
      <Tooltip content="Left tip" side="left"><button type="button">L</button></Tooltip>
      <Tooltip content="Right tip" side="right"><button type="button">R</button></Tooltip>
    </TooltipPortalContainer></TooltipProvider>);
    const shown = (text: string) => screen.queryAllByRole("tooltip").some((t) => t.textContent === text);
    const user = userEvent.setup();
    await user.hover(screen.getByRole("button", { name: "L" }));
    await new Promise((resolve) => setTimeout(resolve, 350));
    assert.equal(shown("Left tip"), false, "the provider's longer delay applies, not the default");
    await waitFor(() => assert.ok(shown("Left tip")), { timeout: 1500 });
    assert.ok(host.contains(screen.getByRole("tooltip")), "the tooltip renders into the portal container");
    await user.unhover(screen.getByRole("button", { name: "L" }));
    await waitFor(() => assert.equal(shown("Left tip"), false), { timeout: 1500 });
    await user.hover(screen.getByRole("button", { name: "R" }));
    await new Promise((resolve) => setTimeout(resolve, 350));
    assert.equal(shown("Right tip"), false, "the second tooltip shares the same delay");
    await waitFor(() => assert.ok(shown("Right tip")), { timeout: 1500 });
  } finally {
    host.remove();
  }
});

// ---------------------------------------------------------------------------
// Dialog
// ---------------------------------------------------------------------------

test("dialog opens from its trigger, is labelled by its title and closes from the built-in close button", async () => {
  const changes: Array<[boolean, string]> = [];
  render(<Dialog onOpenChange={(open, details) => changes.push([open, details.reason])}>
    <DialogTrigger>Create batch</DialogTrigger>
    <DialogContent size="lg">
      <DialogHeader><DialogTitle>New batch</DialogTitle><DialogDescription>Upload requests.</DialogDescription></DialogHeader>
      <DialogFooter><DialogClose>Cancel</DialogClose></DialogFooter>
    </DialogContent>
  </Dialog>);
  const user = userEvent.setup();
  await user.click(screen.getByRole("button", { name: "Create batch" }));
  const dialog = await screen.findByRole("dialog", { name: "New batch" });
  assert.equal(document.getElementById(dialog.getAttribute("aria-describedby")!)?.textContent, "Upload requests.");
  assert.equal(dialog.getAttribute("data-placement"), "center");
  await user.click(within(dialog).getByRole("button", { name: "Close" }));
  await gone(() => screen.queryByRole("dialog"));
  await user.click(screen.getByRole("button", { name: "Create batch" }));
  await screen.findByRole("dialog");
  await user.keyboard("{Escape}");
  await gone(() => screen.queryByRole("dialog"));
  await user.click(screen.getByRole("button", { name: "Create batch" }));
  await user.click(within(await screen.findByRole("dialog")).getByRole("button", { name: "Cancel" }));
  await gone(() => screen.queryByRole("dialog"));
  assert.deepEqual(changes.map(([open]) => open), [true, false, true, false, true, false]);
  assert.equal(changes[3][1], "escape-key");
});

test("a busy dialog disables its relabelled close button and pointer dismissal keeps it open", async () => {
  const changes: boolean[] = [];
  render(<Dialog defaultOpen disablePointerDismissal onOpenChange={(open) => changes.push(open)}>
    <DialogContent closeButtonLabel="Dismiss" closeButtonDisabled style={{ width: 300 }} data-testid="panel">
      <DialogTitle>Submitting</DialogTitle>
    </DialogContent>
  </Dialog>);
  const dialog = await screen.findByRole("dialog", { name: "Submitting" });
  assert.equal(dialog.style.width, "300px");
  const close = within(dialog).getByRole("button", { name: "Dismiss" }) as HTMLButtonElement;
  assert.equal(close.disabled || close.getAttribute("aria-disabled") === "true", true);
  const user = userEvent.setup();
  await user.click(close);
  const backdrop = document.querySelector("[data-slot='dialog-backdrop']")!;
  await user.click(backdrop);
  assert.ok(screen.getByRole("dialog"));
  assert.deepEqual(changes, []);
});

test("edge-placed dialogs render a sticky close button and scope themselves to a container", async () => {
  const host = document.createElement("div");
  document.body.append(host);
  for (const placement of ["left", "right", "top", "bottom"] as const) {
    const { unmount } = render(<Dialog defaultOpen modal={false}>
      <DialogContent placement={placement} container={host} backdropClassName="scrim" backdropStyle={{ opacity: 0.5 }}><DialogTitle>Drawer {placement}</DialogTitle></DialogContent>
    </Dialog>);
    const dialog = await screen.findByRole("dialog", { name: `Drawer ${placement}` });
    assert.ok(host.contains(dialog));
    assert.equal(dialog.getAttribute("data-placement"), placement);
    // The close button comes before the content so it stays reachable while the drawer scrolls.
    const close = within(dialog).getByRole("button", { name: "Close" });
    assert.ok(close.compareDocumentPosition(within(dialog).getByText(`Drawer ${placement}`)) & Node.DOCUMENT_POSITION_FOLLOWING);
    unmount();
  }
  for (const placement of ["right", "bottom"] as const) {
    const { unmount } = render(<Dialog defaultOpen><DialogContent placement={placement} showCloseButton={false}><DialogTitle>Bare {placement}</DialogTitle></DialogContent></Dialog>);
    const dialog = await screen.findByRole("dialog", { name: `Bare ${placement}` });
    assert.equal(within(dialog).queryByRole("button"), null);
    unmount();
  }
  host.remove();
});

test("a controlled dialog follows its open prop and renders into an ambient portal container", async () => {
  const ambient = document.createElement("div");
  document.body.append(ambient);
  function Controlled() {
    const [open, setOpen] = useState(false);
    return <PortalContainerProvider value={ambient}>
      <button type="button" onClick={() => setOpen(true)}>Open</button>
      <Dialog open={open} onOpenChange={setOpen}><DialogContent nestedBackdrop showCloseButton={false}><DialogTitle>Ambient</DialogTitle><DialogClose>Done</DialogClose></DialogContent></Dialog>
    </PortalContainerProvider>;
  }
  render(<Controlled />);
  assert.equal(screen.queryByRole("dialog"), null);
  fireEvent.click(screen.getByRole("button", { name: "Open" }));
  const dialog = await screen.findByRole("dialog", { name: "Ambient" });
  assert.ok(ambient.contains(dialog));
  await act(async () => { fireEvent.click(within(dialog).getByRole("button", { name: "Done" })); });
  await gone(() => screen.queryByRole("dialog"));
  ambient.remove();
});
