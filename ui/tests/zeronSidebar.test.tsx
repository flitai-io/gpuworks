// Zeron's Sidebar (ui/src/components/ui/sidebar.tsx): first the way the console
// uses it (App.tsx's controlled SidebarProvider with breakpointBehavior
// "collapse", AppSidebar's icon-collapsible Sidebar with groups and a footer
// toggle), then the provider's other modes (compact drawer, persistence,
// keyboard shortcut) and the remaining parts.
import assert from "node:assert/strict";
import { after, afterEach, beforeEach, test as nodeTest } from "node:test";
import React, { useState } from "react";
import { dom, pendingFrameCount } from "./helpers/dom.ts";
import { trackMessageChannels } from "./helpers/trackedMessageChannels.ts";

const closeChannels = trackMessageChannels();
for (const name of ["KeyboardEvent", "PointerEvent", "MouseEvent", "FocusEvent", "Document", "HTMLAnchorElement", "HTMLDivElement", "HTMLSpanElement", "DocumentFragment", "Text", "Event", "CustomEvent", "AbortController", "AbortSignal"] as const) {
  if (dom.window[name]) Object.defineProperty(globalThis, name, { value: dom.window[name], configurable: true });
}
// jsdom has no Web Animations API; Base UI asks which animations are running.
if (!("getAnimations" in dom.window.Element.prototype)) {
  Object.defineProperty(dom.window.Element.prototype, "getAnimations", { configurable: true, value: () => [] });
}
Object.defineProperty(globalThis, "localStorage", { value: dom.window.localStorage, configurable: true });

// Sidebar reads "(max-width: 1279px)" through window.matchMedia and follows its
// change events. This answers width queries from a settable window width and,
// like a browser, fires "change" only on lists whose answer flips.
let viewportWidth = 1440;
type MediaListener = (event: { matches: boolean; media: string }) => void;
const mediaLists = new Set<{ media: string; last: boolean; listeners: Set<MediaListener> }>();
function matchesQuery(query: string) {
  const max = /max-width:\s*(\d+(?:\.\d+)?)px/.exec(query);
  return max ? viewportWidth <= Number(max[1]) : false;
}
Object.defineProperty(dom.window, "matchMedia", {
  configurable: true,
  value: (media: string) => {
    const list = { media, last: matchesQuery(media), listeners: new Set<MediaListener>() };
    mediaLists.add(list);
    return {
      get matches() { return matchesQuery(media); },
      media,
      addListener: (listener: MediaListener) => list.listeners.add(listener),
      removeListener: (listener: MediaListener) => list.listeners.delete(listener),
      addEventListener: (_type: string, listener: MediaListener) => list.listeners.add(listener),
      removeEventListener: (_type: string, listener: MediaListener) => list.listeners.delete(listener),
      dispatchEvent: () => false,
    };
  },
});
function resizeTo(width: number) {
  viewportWidth = width;
  for (const list of mediaLists) {
    const matches = matchesQuery(list.media);
    if (matches === list.last) continue;
    list.last = matches;
    for (const listener of [...list.listeners]) listener({ matches, media: list.media });
  }
}

const { cleanup, render, screen, waitFor, fireEvent, act, within } = await import("@testing-library/react");
const userEvent = (await import("@testing-library/user-event")).default;
// helpers/dom.ts drops a finished test's pending animation frames; Base UI's
// process-wide frame scheduler would then wait forever for the dropped one.
const { resetAnimationFrameScheduler } = await import("@base-ui/utils/useAnimationFrame");
const {
  SidebarProvider, Sidebar, SidebarTrigger, SidebarFloatingTrigger, SidebarRail, SidebarHeader, SidebarContent, SidebarFooter,
  SidebarGroup, SidebarGroupLabel, SidebarGroupTrigger, SidebarGroupAction, SidebarGroupContent, SidebarSeparator, useSidebar,
} = await import("../src/components/ui/sidebar.tsx");

beforeEach(() => { resetAnimationFrameScheduler(); localStorage.clear(); act(() => resizeTo(1440)); });
afterEach(() => { cleanup(); resetAnimationFrameScheduler(); mediaLists.clear(); });
after(() => closeChannels());
// framer-motion's frame loop is process-wide too: let each test's animations finish.
function test(name: string, fn: () => unknown) {
  return nodeTest(name, async () => {
    try {
      await fn();
    } finally {
      await waitFor(() => assert.equal(pendingFrameCount(), 0));
    }
  });
}
// Compares nodes by identity: assert.equal would inspect whole DOM trees on failure.
function assertFocus(expected: Element, message?: string) {
  const active = document.activeElement;
  assert.ok(active === expected, `${message ?? "focus"}: expected ${expected.outerHTML.slice(0, 120)}, got ${active?.outerHTML.slice(0, 120)}`);
}
const gone = (query: () => Element | null) => waitFor(() => assert.ok(query() === null));
const sidebarRoot = () => document.querySelector("[data-slot=sidebar]") as HTMLElement;
const sidebarState = () => sidebarRoot().getAttribute("data-state");

/** The footer toggle AppSidebar renders: it reads the state and calls toggle(). */
function FooterToggle() {
  const { state, toggle } = useSidebar();
  const collapsed = state === "collapsed";
  return <button aria-expanded={!collapsed} onClick={toggle}>{collapsed ? "Expand navigation" : "Collapse navigation"}</button>;
}
function ConsoleSidebar() {
  return <Sidebar collapsible="icon" width="200px" ariaLabel="Navigation menu">
    <SidebarContent contentClassName="gap-3" viewportClassName="mask">
      <SidebarGroup aria-label="Resources">
        <SidebarGroupLabel>Resources</SidebarGroupLabel>
        <SidebarGroupContent><a href="#models">Models</a></SidebarGroupContent>
      </SidebarGroup>
    </SidebarContent>
    <SidebarFooter><FooterToggle /></SidebarFooter>
  </Sidebar>;
}
/** App.tsx: the console owns the open state (it keeps it in localStorage itself). */
function ConsoleFrame({ onOpenChange }: { onOpenChange?: (open: boolean) => void }) {
  const [open, setOpen] = useState(true);
  return <SidebarProvider open={open} onOpenChange={(next) => { setOpen(next); onOpenChange?.(next); }} breakpointBehavior="collapse">
    <ConsoleSidebar />
    <main>page</main>
  </SidebarProvider>;
}

// ---------------------------------------------------------------------------
// The console's configuration
// ---------------------------------------------------------------------------

test("the console's side menu is a labelled panel that folds to its icon rail and back from the footer toggle", async () => {
  const changes: boolean[] = [];
  render(<ConsoleFrame onOpenChange={(open) => changes.push(open)} />);
  const panel = screen.getByRole("complementary", { name: "Navigation menu" });
  assert.ok(within(panel).getByRole("region", { name: "Resources" }), "each group is a named section");
  assert.ok(within(panel).getByRole("link", { name: "Models" }));
  assert.ok(within(panel).getByText("Resources"));
  assert.equal(sidebarState(), "expanded");
  assert.equal(sidebarRoot().getAttribute("data-collapsible"), "icon");
  assert.equal(sidebarRoot().style.getPropertyValue("--sidebar-width"), "200px");
  const user = userEvent.setup();
  await user.click(screen.getByRole("button", { name: "Collapse navigation" }));
  assert.equal(sidebarState(), "collapsed");
  assert.equal(screen.getByRole("button", { name: "Expand navigation" }).getAttribute("aria-expanded"), "false");
  await user.click(screen.getByRole("button", { name: "Expand navigation" }));
  assert.equal(sidebarState(), "expanded");
  assert.deepEqual(changes, [false, true], "the controlled owner hears each change");
});

test("below the compact breakpoint the console's menu folds on its own without changing the user's choice", async () => {
  const changes: boolean[] = [];
  render(<ConsoleFrame onOpenChange={(open) => changes.push(open)} />);
  act(() => resizeTo(1024));
  assert.equal(sidebarState(), "collapsed");
  assert.equal(sidebarRoot().getAttribute("data-mobile"), "false", "the collapse behaviour keeps the persistent panel, not a drawer");
  assert.equal(screen.queryByRole("dialog"), null);
  const user = userEvent.setup();
  await user.click(screen.getByRole("button", { name: "Expand navigation" }));
  assert.equal(sidebarState(), "expanded", "the rail can still be opened temporarily");
  await user.click(screen.getByRole("button", { name: "Collapse navigation" }));
  assert.equal(sidebarState(), "collapsed");
  assert.deepEqual(changes, [], "a temporary open at the breakpoint is not the user's preference");
  await user.click(screen.getByRole("button", { name: "Expand navigation" }));
  act(() => resizeTo(1440));
  assert.equal(sidebarState(), "expanded", "widening restores the preferred state");
  act(() => resizeTo(1024));
  assert.equal(sidebarState(), "collapsed", "the temporary open does not survive leaving the breakpoint");
});

// ---------------------------------------------------------------------------
// Uncontrolled provider, persistence and keyboard shortcut
// ---------------------------------------------------------------------------

function Basic({ persistenceKey, keyboardShortcut, defaultOpen, collapsible = "offcanvas" as const, breakpointBehavior }: {
  persistenceKey?: string; keyboardShortcut?: string; defaultOpen?: boolean; collapsible?: "offcanvas" | "icon" | "none"; breakpointBehavior?: "drawer" | "collapse";
}) {
  return <SidebarProvider persistenceKey={persistenceKey} keyboardShortcut={keyboardShortcut} defaultOpen={defaultOpen} breakpointBehavior={breakpointBehavior}>
    <Sidebar collapsible={collapsible}><SidebarHeader>Header</SidebarHeader><a href="#home">Home</a></Sidebar>
    <SidebarTrigger />
    <input aria-label="Search" />
  </SidebarProvider>;
}

test("an uncontrolled sidebar starts from defaultOpen and toggles from its trigger", async () => {
  render(<Basic defaultOpen={false} />);
  assert.equal(sidebarState(), "collapsed");
  const user = userEvent.setup();
  await user.click(screen.getByRole("button", { name: "Toggle sidebar" }));
  assert.equal(sidebarState(), "expanded");
});

test("the open state is remembered under its persistence key and restored on the next visit", async () => {
  const first = render(<Basic persistenceKey="nav" />);
  assert.equal(localStorage.getItem("nav"), "open");
  await userEvent.setup().click(screen.getByRole("button", { name: "Toggle sidebar" }));
  assert.equal(localStorage.getItem("nav"), "closed");
  first.unmount();
  render(<Basic persistenceKey="nav" />);
  await waitFor(() => assert.equal(sidebarState(), "collapsed"));
  cleanup();
  localStorage.setItem("nav", "garbage");
  render(<Basic persistenceKey="nav" defaultOpen={false} />);
  assert.equal(sidebarState(), "collapsed", "an unknown stored value leaves the default in place");
});

test("unreadable storage falls back to the default state", () => {
  const getItem = dom.window.Storage.prototype.getItem;
  const setItem = dom.window.Storage.prototype.setItem;
  dom.window.Storage.prototype.getItem = () => { throw new Error("denied"); };
  dom.window.Storage.prototype.setItem = () => { throw new Error("denied"); };
  try {
    render(<Basic persistenceKey="nav" />);
    assert.equal(sidebarState(), "expanded");
  } finally {
    dom.window.Storage.prototype.getItem = getItem;
    dom.window.Storage.prototype.setItem = setItem;
  }
});

test("the keyboard shortcut toggles with Ctrl or Cmd, but not without a modifier or while typing", () => {
  render(<Basic keyboardShortcut="mod+b" />);
  const press = (init: KeyboardEventInit, target: EventTarget = window) => {
    const event = new window.KeyboardEvent("keydown", { bubbles: true, cancelable: true, ...init });
    act(() => { target.dispatchEvent(event); });
    return event;
  };
  assert.equal(press({ key: "b", ctrlKey: true }).defaultPrevented, true);
  assert.equal(sidebarState(), "collapsed");
  press({ key: "B", metaKey: true });
  assert.equal(sidebarState(), "expanded");
  press({ key: "b" });
  assert.equal(sidebarState(), "expanded", "a plain b does nothing");
  press({ key: "k", ctrlKey: true });
  assert.equal(sidebarState(), "expanded");
  press({ key: "b", ctrlKey: true }, screen.getByRole("textbox", { name: "Search" }));
  assert.equal(sidebarState(), "expanded", "typing in a field keeps the shortcut for the field");
  const editable = document.createElement("div");
  editable.contentEditable = "true";
  Object.defineProperty(editable, "isContentEditable", { value: true });
  document.body.append(editable);
  press({ key: "b", ctrlKey: true }, editable);
  assert.equal(sidebarState(), "expanded");
  editable.remove();
});

test("a shortcut that is not of the form mod+key is ignored", () => {
  render(<Basic keyboardShortcut="ctrl+b" />);
  act(() => { window.dispatchEvent(new window.KeyboardEvent("keydown", { key: "b", ctrlKey: true })); });
  assert.equal(sidebarState(), "expanded");
});

test("collapsible none keeps the panel expanded whatever the state", async () => {
  render(<Basic collapsible="none" />);
  await userEvent.setup().click(screen.getByRole("button", { name: "Toggle sidebar" }));
  assert.equal(sidebarState(), "expanded");
});

// ---------------------------------------------------------------------------
// Compact drawer (the default breakpoint behaviour)
// ---------------------------------------------------------------------------

test("below the breakpoint the default sidebar becomes a drawer opened by its trigger and closed with Escape, returning focus", async () => {
  act(() => resizeTo(800));
  render(<Basic />);
  assert.equal(screen.queryByRole("dialog"), null);
  assert.equal(screen.queryByRole("complementary"), null, "the persistent panel is not rendered at this width");
  const user = userEvent.setup();
  const trigger = screen.getByRole("button", { name: "Toggle sidebar" });
  await user.click(trigger);
  const dialog = await screen.findByRole("dialog", { name: "Navigation" });
  assert.ok(within(dialog).getByRole("link", { name: "Home" }));
  assert.equal(within(dialog).getByRole("complementary", { name: "Navigation" }).closest("[data-slot=sidebar]")?.getAttribute("data-mobile"), "true");
  await user.keyboard("{Escape}");
  await gone(() => screen.queryByRole("dialog"));
  await waitFor(() => assertFocus(trigger));
});

test("the shortcut opens the compact drawer and closing it returns focus to the element focused before, not the trigger", async () => {
  act(() => resizeTo(800));
  render(<><Basic keyboardShortcut="mod+b" /><button>Settings</button></>);
  const settings = screen.getByRole("button", { name: "Settings" });
  const user = userEvent.setup();
  settings.focus();
  await user.keyboard("{Control>}b{/Control}");
  await screen.findByRole("dialog");
  await waitFor(() => assert.ok(document.activeElement !== settings, "focus moved into the drawer"));
  await user.keyboard("{Control>}b{/Control}");
  await gone(() => screen.queryByRole("dialog"));
  await waitFor(() => assertFocus(settings));
  assert.ok(document.activeElement !== screen.getByRole("button", { name: "Toggle sidebar" }));
});

test("growing past the breakpoint closes an open drawer and puts focus back on its trigger", async () => {
  act(() => resizeTo(800));
  render(<Basic />);
  const trigger = screen.getByRole("button", { name: "Toggle sidebar" });
  await userEvent.setup().click(trigger);
  await screen.findByRole("dialog");
  act(() => resizeTo(1440));
  await gone(() => screen.queryByRole("dialog"));
  assert.equal(sidebarState(), "expanded");
  await waitFor(() => assertFocus(trigger));
});

test("a controlled drawer reports open requests and programmatic opens do not reuse a stale trigger", async () => {
  act(() => resizeTo(800));
  const requests: boolean[] = [];
  function Controlled() {
    const [mobileOpen, setMobileOpen] = useState(false);
    return <SidebarProvider mobileOpen={mobileOpen} onMobileOpenChange={(open) => { requests.push(open); setMobileOpen(open); }}>
      <Sidebar ariaLabel="Menu"><Inner /></Sidebar>
      <SidebarTrigger label="Open menu" />
      <Opener />
      <button onClick={() => setMobileOpen(true)}>Force open</button>
    </SidebarProvider>;
  }
  function Inner() {
    const { closeMobile } = useSidebar();
    return <button onClick={closeMobile}>Done</button>;
  }
  function Opener() {
    const { setMobileOpen, setActiveTrigger } = useSidebar();
    return <>
      <button onClick={() => setMobileOpen(true)}>Programmatic</button>
      <button onClick={(event) => { setActiveTrigger(event.currentTarget); setMobileOpen(true); }}>Owned</button>
    </>;
  }
  render(<Controlled />);
  const user = userEvent.setup();
  await user.click(screen.getByRole("button", { name: "Open menu" }));
  const dialog = await screen.findByRole("dialog", { name: "Menu" });
  await user.click(within(dialog).getByRole("button", { name: "Done" }));
  await gone(() => screen.queryByRole("dialog"));
  assert.deepEqual(requests, [true, false]);
  await user.click(screen.getByRole("button", { name: "Owned" }));
  await user.click(within(await screen.findByRole("dialog")).getByRole("button", { name: "Done" }));
  await gone(() => screen.queryByRole("dialog"));
  await waitFor(() => assertFocus(screen.getByRole("button", { name: "Owned" })), "an explicitly prepared trigger regains focus");
  await user.click(screen.getByRole("button", { name: "Programmatic" }));
  await screen.findByRole("dialog");
  await user.keyboard("{Escape}");
  await gone(() => screen.queryByRole("dialog"));
  await user.click(screen.getByRole("button", { name: "Force open" }));
  await screen.findByRole("dialog");
  await user.keyboard("{Escape}");
  await gone(() => screen.queryByRole("dialog"));
});

test("the trigger shows a menu icon for the drawer and expand or collapse icons otherwise, and an onClick can veto the toggle", async () => {
  function Shown() {
    const { state } = useSidebar();
    return <output aria-label="state">{state}</output>;
  }
  render(<SidebarProvider>
    <Sidebar><p>x</p></Sidebar>
    <SidebarTrigger label="Plain" />
    <SidebarTrigger label="Custom" icon={<span>☰</span>} />
    <SidebarTrigger label="Veto" onClick={(event) => event.preventDefault()} />
    <SidebarTrigger label="Rail icon" collapsedIcon={<span>logo</span>} />
    <Shown />
  </SidebarProvider>);
  const user = userEvent.setup();
  assert.equal(screen.getByRole("button", { name: "Custom" }).textContent, "☰");
  await user.click(screen.getByRole("button", { name: "Veto" }));
  assert.equal(screen.getByLabelText("state").textContent, "expanded");
  assert.equal(screen.getByRole("button", { name: "Rail icon" }).textContent, "", "the collapsed-rail icon is only used while collapsed");
  await user.click(screen.getByRole("button", { name: "Plain" }));
  assert.equal(screen.getByLabelText("state").textContent, "collapsed");
  assert.equal(screen.getByRole("button", { name: "Rail icon" }).textContent, "logo");
  assert.ok(screen.getByRole("button", { name: "Plain" }).querySelector("svg"));
});

// ---------------------------------------------------------------------------
// Rail, floating trigger and the panel's other props
// ---------------------------------------------------------------------------

test("the rail toggles on click unless its onClick prevents it, and stays out of the tab order", () => {
  let veto = false;
  render(<SidebarProvider><Sidebar><SidebarRail data-testid="rail" onClick={(event) => { if (veto) event.preventDefault(); }} /></Sidebar></SidebarProvider>);
  const rail = screen.getByTestId("rail");
  assert.equal(rail.getAttribute("tabindex"), "-1");
  assert.equal(rail.getAttribute("aria-hidden"), "true");
  fireEvent.click(rail);
  assert.equal(sidebarState(), "collapsed");
  veto = true;
  fireEvent.click(rail);
  assert.equal(sidebarState(), "collapsed");
});

test("the floating trigger appears only for a collapsed offcanvas sidebar and expands it on click", async () => {
  render(<SidebarProvider defaultOpen={false}>
    <Sidebar><p>x</p></Sidebar>
    <SidebarFloatingTrigger renderContent={() => <a href="#quick">Quick nav</a>} />
    <SidebarFloatingTrigger collapsedBehavior="icon" label="Icon mode" renderContent={() => null} />
  </SidebarProvider>);
  assert.equal(screen.queryByRole("button", { name: "Icon mode" }), null, "an icon rail has no floating trigger");
  const user = userEvent.setup();
  await user.click(screen.getByRole("button", { name: "Expand sidebar" }));
  assert.equal(sidebarState(), "expanded");
  assert.equal(screen.queryByRole("button", { name: "Expand sidebar" }), null);
});

test("a menu floating trigger shows the navigation in a popover on hover, which its content can close", async () => {
  const clicks: string[] = [];
  render(<SidebarProvider defaultOpen={false}>
    <Sidebar><p>x</p></Sidebar>
    <SidebarFloatingTrigger clickBehavior="menu" onClick={() => clicks.push("click")}
      renderContent={({ close }) => <button onClick={close}>Go to models</button>} />
  </SidebarProvider>);
  const user = userEvent.setup();
  const trigger = screen.getByRole("button", { name: "Open sidebar menu" });
  await user.hover(trigger);
  await user.click(await screen.findByRole("button", { name: "Go to models" }));
  await gone(() => screen.queryByRole("button", { name: "Go to models" }));
  fireEvent.click(trigger);
  assert.equal(sidebarState(), "collapsed", "a menu trigger does not expand the sidebar");
  assert.deepEqual(clicks, ["click"]);
});

test("a floating trigger whose onClick prevents default does not expand", async () => {
  render(<SidebarProvider defaultOpen={false}>
    <Sidebar><p>x</p></Sidebar>
    <SidebarFloatingTrigger onClick={(event) => event.preventDefault()} renderContent={() => null} />
  </SidebarProvider>);
  await userEvent.setup().click(screen.getByRole("button", { name: "Expand sidebar" }));
  assert.equal(sidebarState(), "collapsed");
});

test("with breakpoint collapse, the floating trigger stays mounted and turns into a menu button at the breakpoint", async () => {
  render(<SidebarProvider breakpointBehavior="collapse">
    <Sidebar><p>x</p></Sidebar>
    <SidebarFloatingTrigger renderContent={() => <span>Menu body</span>} />
  </SidebarProvider>);
  assert.ok(screen.getByRole("button", { name: "Expand sidebar", hidden: true }), "kept mounted (hidden by CSS) while expanded");
  act(() => resizeTo(1024));
  const trigger = screen.getByRole("button", { name: "Open sidebar menu" });
  await userEvent.setup().click(trigger);
  assert.equal(sidebarState(), "collapsed", "at the breakpoint a click opens the menu instead of expanding");
});

test("the floating trigger hides on the drawer breakpoint", () => {
  act(() => resizeTo(800));
  render(<SidebarProvider defaultOpen={false}>
    <SidebarFloatingTrigger renderContent={() => null} />
  </SidebarProvider>);
  assert.equal(screen.queryByRole("button"), null);
});

test("panel props: deprecated mobileLabel names it, end side and right-to-left mirror it, floating variant and refs are applied", () => {
  let ref: HTMLDivElement | null = null;
  render(<SidebarProvider defaultOpen={false}>
    <Sidebar ref={(node) => { ref = node; }} mobileLabel="Old label" side="end" dir="rtl" variant="floating" collapsedWidth="48px" mobileWidth="20rem"><p>x</p></Sidebar>
  </SidebarProvider>);
  assert.ok(screen.getByRole("complementary", { name: "Old label" }));
  const root = sidebarRoot();
  assert.ok(ref === root);
  assert.equal(root.getAttribute("data-side"), "end");
  assert.equal(root.getAttribute("data-variant"), "floating");
  assert.equal(root.getAttribute("dir"), "rtl");
  assert.equal(root.style.getPropertyValue("--sidebar-width-collapsed"), "48px");
  assert.equal(root.style.getPropertyValue("--sidebar-width-mobile"), "20rem");
  assert.equal(sidebarState(), "collapsed");
});

// ---------------------------------------------------------------------------
// Groups and the remaining parts
// ---------------------------------------------------------------------------

test("a collapsible group folds its content from the label trigger; a plain group is always open", async () => {
  const actions: string[] = [];
  render(<SidebarProvider><Sidebar>
    <SidebarHeader>Workspace</SidebarHeader>
    <SidebarContent>
      <SidebarGroup collapsible>
        <SidebarGroupTrigger>Projects</SidebarGroupTrigger>
        <SidebarGroupAction aria-label="Add project" onClick={() => actions.push("add")}>+</SidebarGroupAction>
        <SidebarGroupContent><a href="#alpha">Alpha</a></SidebarGroupContent>
      </SidebarGroup>
      <SidebarSeparator />
      <SidebarGroup collapsible defaultOpen={false}>
        <SidebarGroupTrigger indicator={<span>▸</span>}>Archive</SidebarGroupTrigger>
        <SidebarGroupContent><a href="#old">Old</a></SidebarGroupContent>
      </SidebarGroup>
      <SidebarGroup>
        <SidebarGroupLabel>Fixed</SidebarGroupLabel>
        <SidebarGroupContent><a href="#fixed">Pinned</a></SidebarGroupContent>
      </SidebarGroup>
    </SidebarContent>
    <SidebarFooter>v1.0</SidebarFooter>
  </Sidebar></SidebarProvider>);
  const user = userEvent.setup();
  const projects = screen.getByRole("button", { name: "Projects" });
  assert.equal(projects.getAttribute("aria-expanded"), "true");
  assert.ok(projects.querySelector("svg"), "the default indicator is a chevron");
  await user.click(projects);
  assert.equal(projects.getAttribute("aria-expanded"), "false");
  await waitFor(() => assert.ok(screen.queryByRole("link", { name: "Alpha" }) === null), "folded content leaves the accessibility tree");
  const archive = screen.getByRole("button", { name: "Archive▸" });
  assert.equal(archive.getAttribute("aria-expanded"), "false");
  archive.focus();
  await user.keyboard("{Enter}");
  assert.equal(archive.getAttribute("aria-expanded"), "true");
  assert.ok(await screen.findByRole("link", { name: "Old" }));
  assert.ok(screen.getByRole("link", { name: "Pinned" }));
  await user.click(screen.getByRole("button", { name: "Add project" }));
  assert.deepEqual(actions, ["add"]);
  assert.ok(screen.getByRole("separator"));
  assert.ok(screen.getByText("Workspace") && screen.getByText("v1.0"));
});

test("a controlled collapsible group follows its open prop", () => {
  const { rerender } = render(<SidebarProvider><Sidebar><SidebarGroup collapsible open={false}>
    <SidebarGroupTrigger>Team</SidebarGroupTrigger><SidebarGroupContent>members</SidebarGroupContent>
  </SidebarGroup></Sidebar></SidebarProvider>);
  assert.equal(screen.getByRole("button", { name: "Team" }).getAttribute("aria-expanded"), "false");
  rerender(<SidebarProvider><Sidebar><SidebarGroup collapsible open>
    <SidebarGroupTrigger>Team</SidebarGroupTrigger><SidebarGroupContent>members</SidebarGroupContent>
  </SidebarGroup></Sidebar></SidebarProvider>);
  assert.equal(screen.getByRole("button", { name: "Team" }).getAttribute("aria-expanded"), "true");
});

test("useSidebar outside a provider explains what is missing", () => {
  const originalError = console.error;
  console.error = () => {};
  try {
    assert.throws(() => render(<SidebarTrigger />), /useSidebar must be used within a SidebarProvider/);
  } finally {
    console.error = originalError;
  }
});
