// Zeron navigation primitives (ui/src/components/ui): NavMenu and NavItem the
// way AppSidebar builds the side menu (roving keyboard focus, router links,
// collapsed-rail tooltips), then their remaining parts and props; Breadcrumb
// with its resource switcher; MobileDrawer, the compact Sidebar's dialog.
import assert from "node:assert/strict";
import { after, afterEach, beforeEach, test as nodeTest } from "node:test";
import React, { useRef, useState } from "react";
import { dom, pendingFrameCount } from "./helpers/dom.ts";
import { trackMessageChannels } from "./helpers/trackedMessageChannels.ts";

const closeChannels = trackMessageChannels();
for (const name of ["KeyboardEvent", "PointerEvent", "MouseEvent", "FocusEvent", "Document", "HTMLAnchorElement", "HTMLDivElement", "HTMLSpanElement", "HTMLLIElement", "DocumentFragment", "Text", "Event", "CustomEvent", "AbortController", "AbortSignal"] as const) {
  if (dom.window[name]) Object.defineProperty(globalThis, name, { value: dom.window[name], configurable: true });
}
// jsdom has no Web Animations API; Base UI asks which animations are running before unmounting a popup.
if (!("getAnimations" in dom.window.Element.prototype)) {
  Object.defineProperty(dom.window.Element.prototype, "getAnimations", { configurable: true, value: () => [] });
}
// jsdom has no layout. Nav items report a box by their position in the list
// (rows 36px apart, or columns 90px apart in a horizontal menu); dropdown rows
// stack by their proximity index. Everything else has no box.
const proto = dom.window.HTMLElement.prototype;
const originalOffsets = Object.fromEntries((["offsetHeight", "offsetWidth", "offsetTop", "offsetLeft", "offsetParent"] as const).map((key) => [key, Object.getOwnPropertyDescriptor(proto, key)]));
function layout(element: HTMLElement) {
  if (element.dataset.slot === "nav-item") {
    const siblings = [...(element.parentElement?.children ?? [])].filter((child) => (child as HTMLElement).dataset.slot === "nav-item");
    const i = siblings.indexOf(element);
    const horizontal = element.closest("[data-slot=nav-menu]")?.getAttribute("data-orientation") === "horizontal";
    return horizontal ? { top: 0, left: i * 90, width: 80, height: 32 } : { top: i * 36, left: 0, width: 200, height: 32 };
  }
  const attr = element.getAttribute("data-proximity-index");
  if (attr !== null) return { top: 4 + Number(attr) * 34, left: 4, width: 200, height: 32 };
  return null;
}
Object.defineProperties(proto, {
  offsetHeight: { configurable: true, get(this: HTMLElement) { return layout(this)?.height ?? 0; } },
  offsetWidth: { configurable: true, get(this: HTMLElement) { return layout(this)?.width ?? 0; } },
  offsetTop: { configurable: true, get(this: HTMLElement) { return layout(this)?.top ?? 0; } },
  offsetLeft: { configurable: true, get(this: HTMLElement) { return layout(this)?.left ?? 0; } },
  offsetParent: { configurable: true, get(this: HTMLElement) { return layout(this) ? this.parentElement : null; } },
});
const originalRect = dom.window.Element.prototype.getBoundingClientRect;
dom.window.Element.prototype.getBoundingClientRect = function (this: Element) {
  const b = this instanceof dom.window.HTMLElement ? layout(this) : null;
  if (b) return { ...b, x: b.left, y: b.top, right: b.left + b.width, bottom: b.top + b.height, toJSON() {} } as DOMRect;
  return { x: 0, y: 0, left: 0, top: 0, width: 400, height: 400, right: 400, bottom: 400, toJSON() {} } as DOMRect;
};

const { cleanup, render, screen, waitFor, fireEvent, act, within } = await import("@testing-library/react");
const userEvent = (await import("@testing-library/user-event")).default;
const { MemoryRouter, Link, Routes, Route, useLocation } = await import("react-router-dom");
// helpers/dom.ts drops a finished test's pending animation frames; Base UI's
// process-wide frame scheduler would then wait forever for the dropped one.
const { resetAnimationFrameScheduler } = await import("@base-ui/utils/useAnimationFrame");
const { NavMenu, useNavMenu } = await import("../src/components/ui/nav-menu.tsx");
const {
  NavItem, NavItemTrigger, NavItemLeading, NavItemContent, NavItemLabel, NavItemDescription, NavItemTrailing, NavItemBadge,
  NavItemAction, NavItemSub, NavItemSubItem, NavItemSubTrigger,
} = await import("../src/components/ui/nav-item.tsx");
const { Breadcrumb, BreadcrumbList, BreadcrumbItem, BreadcrumbLink, BreadcrumbPage, BreadcrumbSeparator, BreadcrumbEllipsis } = await import("../src/components/ui/breadcrumb.tsx");
const { MobileDrawer } = await import("../src/components/ui/mobile-drawer.tsx");

beforeEach(() => resetAnimationFrameScheduler());
afterEach(() => { cleanup(); resetAnimationFrameScheduler(); });
after(() => {
  for (const [key, descriptor] of Object.entries(originalOffsets)) if (descriptor) Object.defineProperty(proto, key, descriptor);
  dom.window.Element.prototype.getBoundingClientRect = originalRect;
  closeChannels();
});

// framer-motion's frame loop is process-wide too: helpers/dom.ts would drop a
// frame it is waiting on and stall every later exit animation, so each test
// lets its animations finish first.
function test(name: string, ...rest: [() => unknown] | [{ todo: string }, () => unknown]) {
  const fn = rest.length === 1 ? rest[0] : rest[1];
  const options = rest.length === 1 ? {} : rest[0];
  return nodeTest(name, options, async () => {
    try {
      await fn();
    } finally {
      await waitFor(() => assert.equal(pendingFrameCount(), 0));
    }
  });
}
// Compares nodes by identity and names the focused element on failure:
// assert.equal would inspect whole DOM trees and exhaust memory.
function assertFocus(expected: Element, message?: string) {
  const active = document.activeElement;
  assert.ok(active === expected, `${message ?? "focus"}: expected ${expected.outerHTML.slice(0, 120)}, got ${active?.outerHTML.slice(0, 120)}`);
}
const gone = (query: () => Element | null) => waitFor(() => assert.ok(query() === null));
const slot = (name: string) => document.querySelector(`[data-slot=${name}]`);

// ---------------------------------------------------------------------------
// NavMenu + NavItem, as AppSidebar uses them
// ---------------------------------------------------------------------------

const pages = [
  { key: "/dashboard", label: "Dashboard" },
  { key: "/models", label: "Models" },
  { key: "/nodes", label: "Nodes", disabled: true },
  { key: "/settings", label: "Settings" },
];
function Where() {
  return <output aria-label="location">{useLocation().pathname}</output>;
}
function SideMenu({ collapsed = false, start = "/models" }: { collapsed?: boolean; start?: string }) {
  return (
    <MemoryRouter initialEntries={[start]}>
      <Routes>
        <Route path="*" element={<RoutedMenu collapsed={collapsed} />} />
      </Routes>
    </MemoryRouter>
  );
}
function RoutedMenu({ collapsed }: { collapsed: boolean }) {
  const { pathname } = useLocation();
  return <>
    <NavMenu activeValue={pathname} aria-label="Resources" keyboardNavigation="roving">
      {pages.map((page) => (
        <NavItem key={page.key} value={page.key} disabled={page.disabled}>
          <NavItemTrigger render={<Link to={page.key} />} tooltip={collapsed ? page.label : undefined} tooltipSide="right">
            <NavItemLeading aria-hidden="true">●</NavItemLeading>
            <NavItemContent><NavItemLabel>{page.label}</NavItemLabel></NavItemContent>
          </NavItemTrigger>
        </NavItem>
      ))}
    </NavMenu>
    <Where />
  </>;
}
const link = (name: string) => screen.getByRole("link", { name });

test("the side menu marks the current page and follows a clicked link", async () => {
  render(<SideMenu />);
  const nav = screen.getByRole("navigation", { name: "Resources" });
  assert.deepEqual(within(nav).getAllByRole("link").map((a) => a.textContent), ["●Dashboard", "●Models", "●Nodes", "●Settings"]);
  assert.equal(link("Models").getAttribute("aria-current"), "page");
  assert.equal(link("Dashboard").getAttribute("aria-current"), null);
  await userEvent.setup().click(link("Settings"));
  assert.equal(screen.getByLabelText("location").textContent, "/settings");
  assert.equal(link("Settings").getAttribute("aria-current"), "page");
  assert.equal(link("Models").getAttribute("aria-current"), null);
  await waitFor(() => assert.ok(slot("nav-item-active-indicator")), "the active row gets its highlight once the rows are measured");
});

test("a disabled item is announced as disabled and neither a click nor Enter navigates", async () => {
  render(<SideMenu />);
  const nodes = link("Nodes");
  assert.equal(nodes.getAttribute("aria-disabled"), "true");
  assert.equal(nodes.getAttribute("tabindex"), "-1");
  fireEvent.click(nodes);
  assert.equal(screen.getByLabelText("location").textContent, "/models");
  const enter = new window.KeyboardEvent("keydown", { key: "Enter", bubbles: true, cancelable: true });
  nodes.dispatchEvent(enter);
  assert.equal(enter.defaultPrevented, true);
  const space = new window.KeyboardEvent("keydown", { key: " ", bubbles: true, cancelable: true });
  nodes.dispatchEvent(space);
  assert.equal(space.defaultPrevented, true);
  const other = new window.KeyboardEvent("keydown", { key: "a", bubbles: true, cancelable: true });
  nodes.dispatchEvent(other);
  assert.equal(other.defaultPrevented, false, "other keys are left alone");
});

test("roving keyboard navigation keeps one tab stop on the current page and moves with arrows, Home and End, skipping disabled items", async () => {
  render(<><button>Before</button><SideMenu /></>);
  const user = userEvent.setup();
  assert.deepEqual(screen.getAllByRole("link").map((a) => a.getAttribute("tabindex")), ["-1", "0", "-1", "-1"]);
  screen.getByRole("button", { name: "Before" }).focus();
  await user.tab();
  assertFocus(link("Models"));
  await waitFor(() => assert.ok(slot("nav-item-focus-indicator")), "keyboard focus draws the focus ring");
  await user.keyboard("{ArrowDown}");
  assertFocus(link("Settings"), "the disabled Nodes row is skipped");
  await user.keyboard("{ArrowDown}");
  assertFocus(link("Dashboard"), "moving past the last item wraps to the first");
  await user.keyboard("{ArrowUp}");
  assertFocus(link("Settings"));
  await user.keyboard("{Home}");
  assertFocus(link("Dashboard"));
  await user.keyboard("{End}");
  assertFocus(link("Settings"));
  await user.keyboard("{ArrowLeft}");
  assertFocus(link("Settings"), "a vertical menu ignores horizontal arrows");
  await user.keyboard("{Enter}");
  assert.equal(screen.getByLabelText("location").textContent, "/settings");
  await user.tab();
  await gone(() => slot("nav-item-focus-indicator"));
});

test("a pointer press clears the keyboard focus ring", async () => {
  render(<SideMenu />);
  const user = userEvent.setup();
  await user.tab();
  assertFocus(link("Models"));
  await waitFor(() => assert.ok(slot("nav-item-focus-indicator")));
  fireEvent.pointerDown(link("Models"));
  await gone(() => slot("nav-item-focus-indicator"));
});

test("the collapsed rail names each icon in a tooltip on hover", async () => {
  render(<SideMenu collapsed />);
  await userEvent.setup().hover(link("Dashboard"));
  const tip = await screen.findByRole("tooltip");
  assert.equal(tip.textContent, "Dashboard");
});

test("hovering the menu highlights the row under the pointer and leaving clears it", async () => {
  render(<SideMenu />);
  const nav = screen.getByRole("navigation", { name: "Resources" });
  await waitFor(() => assert.ok(slot("nav-item-active-indicator")));
  fireEvent.mouseEnter(nav);
  fireEvent.mouseMove(nav, { clientX: 20, clientY: 112 });
  await waitFor(() => assert.ok(slot("nav-item-hover-indicator")));
  fireEvent.mouseLeave(nav);
  await gone(() => slot("nav-item-hover-indicator"));
});

test("a horizontal roving menu uses left and right arrows", async () => {
  render(<NavMenu as="div" orientation="horizontal" variant="segment" keyboardNavigation="roving" activeValue={null}>
    {["Overview", "Logs", "Events"].map((label) => <NavItem key={label} value={label}><NavItemTrigger href={`#${label}`}>{label}</NavItemTrigger></NavItem>)}
  </NavMenu>);
  assert.equal(screen.queryByRole("navigation"), null, "as=\"div\" renders inside a parent landmark instead of its own");
  const user = userEvent.setup();
  assert.equal(link("Overview").getAttribute("tabindex"), "0", "with nothing active the first enabled item holds the tab stop");
  await user.tab();
  await user.keyboard("{ArrowRight}");
  assertFocus(link("Logs"));
  await user.keyboard("{ArrowLeft}{ArrowLeft}");
  assertFocus(link("Events"));
  await user.keyboard("{ArrowDown}");
  assertFocus(link("Events"), "a horizontal menu ignores vertical arrows");
});

test("native keyboard navigation leaves every link in the tab order and arrows do nothing", async () => {
  render(<NavMenu aria-label="Docs">
    {["Intro", "Install"].map((label) => <NavItem key={label} value={label}><NavItemTrigger href={`#${label}`}>{label}</NavItemTrigger></NavItem>)}
  </NavMenu>);
  assert.deepEqual(screen.getAllByRole("link").map((a) => a.getAttribute("tabindex")), [null, null]);
  const user = userEvent.setup();
  await user.tab();
  await user.keyboard("{ArrowDown}");
  assertFocus(link("Intro"));
  await user.tab();
  assertFocus(link("Install"));
});

test("a consumer keydown handler can veto roving movement and the menu forwards its refs and focus handlers", async () => {
  const seen: string[] = [];
  let objectRef: HTMLElement | null = null;
  function Example() {
    const ref = useRef<HTMLElement | null>(null);
    return <>
      <NavMenu ref={ref} keyboardNavigation="roving" aria-label="Veto"
        onKeyDown={(event) => { seen.push(event.key); if (event.key === "ArrowDown") event.preventDefault(); }}
        onFocusCapture={() => seen.push("focus")} onBlurCapture={() => seen.push("blur")} onPointerDownCapture={() => seen.push("pointer")}>
        {["One", "Two"].map((label) => <NavItem key={label} value={label}><NavItemTrigger href={`#${label}`}>{label}</NavItemTrigger></NavItem>)}
      </NavMenu>
      <button onClick={() => { objectRef = ref.current; }}>Read ref</button>
    </>;
  }
  let callbackRef: HTMLElement | null = null;
  render(<><Example /><NavMenu ref={(node) => { callbackRef = node; }} aria-label="Callback"><NavItem value="x"><NavItemTrigger href="#x">X</NavItemTrigger></NavItem></NavMenu></>);
  const user = userEvent.setup();
  await user.tab();
  await user.keyboard("{ArrowDown}");
  assertFocus(link("One"), "a prevented ArrowDown does not move focus");
  await user.keyboard("{End}");
  assertFocus(link("Two"));
  fireEvent.pointerDown(link("Two"));
  fireEvent.click(screen.getByRole("button", { name: "Read ref" }));
  assert.ok(objectRef === screen.getByRole("navigation", { name: "Veto" }));
  assert.ok(callbackRef === screen.getByRole("navigation", { name: "Callback" }));
  assert.ok(seen.includes("focus") && seen.includes("blur") && seen.includes("pointer"));
});

test("roving keys on something that is not an item, or in a menu whose items are all disabled, do nothing", async () => {
  render(<NavMenu keyboardNavigation="roving" aria-label="Locked">
    <NavItem value="a" disabled><NavItemTrigger href="#a">A</NavItemTrigger><NavItemAction aria-label="More for A" /></NavItem>
    <li><button>Outside item</button></li>
  </NavMenu>);
  const user = userEvent.setup();
  screen.getByRole("button", { name: "Outside item" }).focus();
  await user.keyboard("{ArrowDown}");
  assertFocus(screen.getByRole("button", { name: "Outside item" }));
  screen.getByRole("button", { name: "More for A" }).focus();
  await user.keyboard("{ArrowDown}");
  assertFocus(screen.getByRole("button", { name: "More for A" }));
  assert.equal(link("A").getAttribute("tabindex"), "-1");
});

test("useNavMenu and NavItem parts throw outside their containers", () => {
  const originalError = console.error;
  console.error = () => {};
  try {
    function Probe() { useNavMenu(); return null; }
    assert.throws(() => render(<Probe />), /NavItem must be used within a NavMenu/);
    assert.throws(() => render(<NavItemTrigger href="#">Loose</NavItemTrigger>), /NavItem parts must be used within a NavItem/);
  } finally {
    console.error = originalError;
  }
});

// ---------------------------------------------------------------------------
// NavItem's other parts and standalone use
// ---------------------------------------------------------------------------

test("a standalone item outside a menu uses its explicit active state", () => {
  render(<>
    <NavItem value="home" active><NavItemTrigger href="#home">Home</NavItemTrigger></NavItem>
    <NavItem value="away"><NavItemTrigger href="#away">Away</NavItemTrigger></NavItem>
  </>);
  assert.equal(link("Home").getAttribute("aria-current"), "page");
  assert.equal(link("Home").closest("[data-slot=nav-item]")?.getAttribute("data-standalone"), "true");
  assert.equal(link("Away").getAttribute("aria-current"), null);
  assert.equal(link("Away").getAttribute("tabindex"), null, "outside a roving menu the link keeps its native tab stop");
});

test("an item shows its description, badge, trailing content, actions and sub-links", async () => {
  const actions: string[] = [];
  let itemRef: HTMLElement | null = null;
  render(<NavMenu variant="segment" aria-label="Projects" activeValue="alpha">
    <NavItem value="alpha" ref={(node) => { itemRef = node; }}>
      <NavItemTrigger href="#alpha" onClick={() => actions.push("open")} onKeyDown={(event) => actions.push(`key:${event.key}`)}>
        <NavItemLeading>A</NavItemLeading>
        <NavItemContent>
          <NavItemLabel>Alpha</NavItemLabel>
          <NavItemDescription>Inference cluster</NavItemDescription>
        </NavItemContent>
        <NavItemBadge>12</NavItemBadge>
        <NavItemTrailing>⌘1</NavItemTrailing>
      </NavItemTrigger>
      <NavItemAction aria-label="Alpha actions" onClick={() => actions.push("menu")} />
      <NavItemAction aria-label="Pin alpha">📌</NavItemAction>
    </NavItem>
    <NavItem value="beta">
      <NavItemTrigger href="#beta"><NavItemLabel><b>Beta</b></NavItemLabel></NavItemTrigger>
      <NavItemSub aria-label="Beta pages">
        <NavItemSubItem value="runs"><NavItemSubTrigger href="#runs">Runs</NavItemSubTrigger></NavItemSubItem>
        <NavItemSubItem value="logs"><NavItemSubTrigger render={<button type="button" />}>Logs</NavItemSubTrigger></NavItemSubItem>
      </NavItemSub>
    </NavItem>
  </NavMenu>);
  const alpha = link(/Alpha/);
  assert.ok(within(alpha).getByText("Inference cluster"));
  assert.ok(within(alpha).getByText("12"));
  assert.ok(within(alpha).getByText("⌘1"));
  assert.ok(itemRef === alpha.closest("li"));
  const user = userEvent.setup();
  await user.click(alpha);
  alpha.focus();
  await user.keyboard("{Enter}");
  const more = screen.getByRole("button", { name: "Alpha actions" });
  assert.ok(more.querySelector("svg"), "the default action shows the ellipsis icon");
  await user.click(more);
  assert.equal(screen.getByRole("button", { name: "Pin alpha" }).textContent, "📌");
  assert.deepEqual(actions.slice(0, 1), ["open"]);
  assert.ok(actions.includes("key:Enter") && actions.includes("menu"));
  const sub = screen.getByRole("list", { name: "Beta pages" });
  assert.deepEqual(within(sub).getAllByRole("listitem").map((li) => li.getAttribute("data-value")), ["runs", "logs"]);
  assert.ok(within(sub).getByRole("link", { name: "Runs" }));
  assert.ok(within(sub).getByRole("button", { name: "Logs" }));
  assert.ok(link("Beta").querySelector("b"), "a non-text label keeps its markup");
});

test("NavItem forwards an object ref", () => {
  function Example() {
    const ref = useRef<HTMLElement | null>(null);
    const [tag, setTag] = useState("");
    return <><NavItem ref={ref} value="solo"><NavItemTrigger href="#solo">Solo</NavItemTrigger></NavItem><button onClick={() => setTag(ref.current?.tagName ?? "")}>Read</button><span>{tag}</span></>;
  }
  render(<Example />);
  fireEvent.click(screen.getByRole("button", { name: "Read" }));
  assert.ok(screen.getByText("DIV"));
});

// ---------------------------------------------------------------------------
// Breadcrumb
// ---------------------------------------------------------------------------

test("a breadcrumb lists links, separators and the current page", () => {
  render(<Breadcrumb>
    <BreadcrumbList>
      <BreadcrumbItem><BreadcrumbLink href="/models">Models</BreadcrumbLink></BreadcrumbItem>
      <BreadcrumbSeparator />
      <BreadcrumbItem><BreadcrumbEllipsis /></BreadcrumbItem>
      <BreadcrumbSeparator>/</BreadcrumbSeparator>
      <BreadcrumbItem><BreadcrumbLink render={<button type="button" />}>Back</BreadcrumbLink></BreadcrumbItem>
      <BreadcrumbItem><BreadcrumbPage icon={<svg data-testid="model-icon" />}>qwen-7b</BreadcrumbPage></BreadcrumbItem>
    </BreadcrumbList>
  </Breadcrumb>);
  const nav = screen.getByRole("navigation", { name: "breadcrumb" });
  assert.equal(within(nav).getByRole("link", { name: "Models" }).getAttribute("href"), "/models");
  assert.ok(within(nav).getByRole("button", { name: "Back" }));
  const current = within(nav).getByText("qwen-7b").closest("[aria-current]");
  assert.equal(current?.getAttribute("aria-current"), "page");
  assert.ok(within(current as HTMLElement).getByTestId("model-icon"));
  // Separators and the ellipsis are decorative; only the real items are list items.
  assert.equal(within(nav).getAllByRole("listitem").length, 4);
  const separators = nav.querySelectorAll("[data-slot=breadcrumb-separator]");
  assert.ok(separators[0].querySelector("svg"), "the default separator is a chevron");
  assert.equal(separators[1].textContent, "/");
  assert.ok(within(nav).getByText("More"));
});

test("a current page with resources becomes a switcher menu", async () => {
  const picked: string[] = [];
  render(<Breadcrumb><BreadcrumbList><BreadcrumbItem>
    <BreadcrumbPage resources={[{ value: "a", label: "cluster-a" }, { value: "b", label: "cluster-b" }, { value: "c", label: "cluster-c", disabled: true }]} onValueChange={(v) => picked.push(v)}>fallback</BreadcrumbPage>
  </BreadcrumbItem></BreadcrumbList></Breadcrumb>);
  const user = userEvent.setup();
  const trigger = screen.getByRole("button", { name: "cluster-a" });
  assert.equal(trigger.getAttribute("aria-current"), "page");
  assert.equal(screen.queryByText("fallback"), null);
  trigger.focus();
  await user.keyboard("{Enter}");
  const menu = await screen.findByRole("menu");
  const options = within(menu).getAllByRole("menuitemradio");
  assert.deepEqual(options.map((o) => o.getAttribute("aria-checked")), ["true", "false", "false"]);
  assert.equal(options[2].getAttribute("aria-disabled"), "true");
  await user.click(options[1]);
  assert.deepEqual(picked, ["b"]);
  await gone(() => screen.queryByRole("menu"));
  assert.ok(screen.getByRole("button", { name: "cluster-b" }), "uncontrolled, the switcher shows the new choice");
  screen.getByRole("button", { name: "cluster-b" }).focus();
  await user.keyboard("{Enter}");
  const reopened = within(await screen.findByRole("menu")).getAllByRole("menuitemradio");
  assert.deepEqual(reopened.map((o) => o.getAttribute("aria-checked")), ["false", "true", "false"]);
  fireEvent.click(reopened[2]);
  assert.deepEqual(picked, ["b"], "a disabled resource cannot be chosen");
});

test("a controlled switcher shows the value it is given and an unknown value falls back to the first resource", async () => {
  const picked: string[] = [];
  const resources = [{ value: "a", label: "cluster-a" }, { value: "b", label: "cluster-b" }];
  const { rerender } = render(<BreadcrumbPage resources={resources} value="b" onValueChange={(v) => picked.push(v)} />);
  const user = userEvent.setup();
  screen.getByRole("button", { name: "cluster-b" }).focus();
  await user.keyboard("{Enter}");
  await user.click(within(await screen.findByRole("menu")).getAllByRole("menuitemradio")[0]);
  assert.deepEqual(picked, ["a"]);
  await gone(() => screen.queryByRole("menu"));
  assert.ok(screen.getByRole("button", { name: "cluster-b" }), "the parent did not accept the change, so the label stays");
  rerender(<BreadcrumbPage resources={resources} value="zzz" />);
  assert.ok(screen.getByRole("button", { name: "cluster-a" }));
  rerender(<BreadcrumbPage resources={resources} defaultValue="b" />);
  assert.ok(screen.getByRole("button", { name: "cluster-a" }), "defaultValue only applies on mount");
  cleanup();
  render(<BreadcrumbPage resources={resources} defaultValue="b" />);
  assert.ok(screen.getByRole("button", { name: "cluster-b" }));
  cleanup();
  render(<BreadcrumbPage resources={[]}>plain</BreadcrumbPage>);
  assert.equal(screen.queryByRole("button"), null, "an empty resource list renders a plain current page");
  assert.equal(screen.getByText("plain").closest("[aria-current]")?.getAttribute("aria-current"), "page");
});

test("a mouse click on the switcher, which a mouse reaches by hovering it first, leaves the menu open", { todo: "Zeron bug: BreadcrumbPage opens its resource menu on mouse pointerenter, and the click that follows toggles it shut again, so clicking the switcher with a mouse closes it" }, async () => {
  render(<BreadcrumbPage resources={[{ value: "a", label: "cluster-a" }, { value: "b", label: "cluster-b" }]} />);
  await userEvent.setup().click(screen.getByRole("button", { name: "cluster-a" }));
  await new Promise((resolve) => setTimeout(resolve, 300));
  assert.ok(screen.queryByRole("menu") !== null);
});

test("the switcher opens when a mouse hovers it and closes shortly after the mouse leaves; touch hover does nothing", async () => {
  const events: string[] = [];
  render(<BreadcrumbPage resources={[{ value: "a", label: "cluster-a" }, { value: "b", label: "cluster-b" }]}
    onPointerEnter={() => events.push("enter")} onPointerLeave={() => events.push("leave")} />);
  const trigger = screen.getByRole("button", { name: "cluster-a" });
  fireEvent.pointerEnter(trigger, { pointerType: "touch" });
  fireEvent.pointerLeave(trigger, { pointerType: "touch" });
  await new Promise((resolve) => setTimeout(resolve, 150));
  assert.equal(screen.queryByRole("menu"), null);
  fireEvent.pointerEnter(trigger, { pointerType: "mouse" });
  const menu = await screen.findByRole("menu");
  fireEvent.pointerLeave(trigger, { pointerType: "mouse" });
  fireEvent.pointerEnter(menu, { pointerType: "mouse" });
  await new Promise((resolve) => setTimeout(resolve, 150));
  assert.ok(screen.queryByRole("menu"), "moving from the trigger into the menu keeps it open");
  fireEvent.pointerLeave(menu, { pointerType: "mouse" });
  await gone(() => screen.queryByRole("menu"));
  assert.deepEqual(events, ["enter", "leave", "enter", "leave"]);
});

test("a plain current page forwards its pointer handlers", () => {
  const events: string[] = [];
  render(<BreadcrumbPage onPointerEnter={() => events.push("enter")} onPointerLeave={() => events.push("leave")}>Details</BreadcrumbPage>);
  const page = screen.getByText("Details").closest("[aria-current]") as HTMLElement;
  fireEvent.pointerEnter(page);
  fireEvent.pointerLeave(page);
  assert.deepEqual(events, ["enter", "leave"]);
});

// ---------------------------------------------------------------------------
// MobileDrawer
// ---------------------------------------------------------------------------

function DrawerExample({ side, dir }: { side?: "start" | "end"; dir?: string }) {
  const [open, setOpen] = useState(false);
  const triggerRef = useRef<HTMLButtonElement>(null);
  return <>
    <button ref={triggerRef} onClick={() => setOpen(true)}>Menu</button>
    <MobileDrawer open={open} onClose={() => setOpen(false)} triggerRef={triggerRef} ariaLabel="Main navigation" side={side} dir={dir} panelStyle={{ width: "18rem" }}>
      <a href="#models">Models</a>
      <button onClick={() => setOpen(false)}>Close</button>
    </MobileDrawer>
  </>;
}

test("the mobile drawer opens as a labelled dialog, closes with Escape and returns focus to its trigger", async () => {
  render(<DrawerExample />);
  const user = userEvent.setup();
  const trigger = screen.getByRole("button", { name: "Menu" });
  assert.equal(screen.queryByRole("dialog"), null);
  await user.click(trigger);
  const dialog = await screen.findByRole("dialog", { name: "Main navigation" });
  assert.ok(within(dialog).getByRole("link", { name: "Models" }));
  assert.equal((dialog as HTMLElement).style.width, "18rem");
  await user.keyboard("{Escape}");
  await gone(() => screen.queryByRole("dialog"));
  await waitFor(() => assertFocus(trigger));
});

test("the mobile drawer closes from its own close action and from the end side in a right-to-left page", async () => {
  render(<DrawerExample side="end" dir="rtl" />);
  const user = userEvent.setup();
  await user.click(screen.getByRole("button", { name: "Menu" }));
  const dialog = await screen.findByRole("dialog", { name: "Main navigation" });
  // The edge class is the only placement signal jsdom can see: the end side of an RTL page is the physical left.
  assert.equal(dialog.classList.contains("left-0"), true);
  assert.equal(dialog.classList.contains("right-0"), false);
  await user.click(within(dialog).getByRole("button", { name: "Close" }));
  await gone(() => screen.queryByRole("dialog"));
});

test("the mobile drawer docks to the physical edge its side and direction resolve to", async () => {
  const cases = [["start", "ltr", "left-0"], ["end", "ltr", "right-0"], ["start", "rtl", "right-0"], ["end", "rtl", "left-0"]] as const;
  for (const [side, dir, edge] of cases) {
    const { unmount } = render(<MobileDrawer open onClose={() => {}} side={side} dir={dir}><p>Body</p></MobileDrawer>);
    const dialog = await screen.findByRole("dialog", { name: "Navigation" });
    assert.equal(dialog.classList.contains(edge), true, `${side}/${dir} docks with ${edge}`);
    assert.equal(dialog.classList.contains(edge === "left-0" ? "right-0" : "left-0"), false);
    unmount();
  }
});

test("the mobile drawer defaults its accessible name to Navigation", async () => {
  render(<MobileDrawer open onClose={() => {}}><p>Body</p></MobileDrawer>);
  assert.ok(await screen.findByRole("dialog", { name: "Navigation" }));
});
