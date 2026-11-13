// Zeron layout primitives (ui/src/components/ui): Tabs the way ui-kit's
// Segmented drives them (controlled segment tabs), then their other variants
// and props; PageLayout as BatchesPage composes it, then its remaining parts;
// ScrollArea on desktop and on touch-primary devices.
import assert from "node:assert/strict";
import { after, afterEach, beforeEach, test as nodeTest } from "node:test";
import React, { useRef, useState } from "react";
import { dom, pendingFrameCount } from "./helpers/dom.ts";
import { trackMessageChannels } from "./helpers/trackedMessageChannels.ts";

const closeChannels = trackMessageChannels();
for (const name of ["KeyboardEvent", "PointerEvent", "MouseEvent", "FocusEvent", "Document", "HTMLDivElement", "HTMLSpanElement", "DocumentFragment", "Text", "Event", "CustomEvent", "AbortController", "AbortSignal"] as const) {
  if (dom.window[name]) Object.defineProperty(globalThis, name, { value: dom.window[name], configurable: true });
}
// jsdom has no Web Animations API; Base UI's ScrollArea asks which animations are running.
if (!("getAnimations" in dom.window.Element.prototype)) {
  Object.defineProperty(dom.window.Element.prototype, "getAnimations", { configurable: true, value: () => [] });
}
// jsdom has no layout. Tabs (and closable-tab wrappers) report an 80px-wide
// box by their proximity index; everything else has no box.
const proto = dom.window.HTMLElement.prototype;
const originalOffsets = Object.fromEntries((["offsetHeight", "offsetWidth", "offsetTop", "offsetLeft", "offsetParent"] as const).map((key) => [key, Object.getOwnPropertyDescriptor(proto, key)]));
function layout(element: HTMLElement) {
  const attr = element.getAttribute("data-proximity-index");
  if (attr === null) return null;
  return { top: 4, left: 4 + Number(attr) * 90, width: 80, height: 32 };
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
  return { x: 0, y: 0, left: 0, top: 0, width: 400, height: 40, right: 400, bottom: 40, toJSON() {} } as DOMRect;
};
// A settable "(pointer: coarse)" answer for ScrollArea's touch switch.
let coarse = false;
delete (dom.window as unknown as Record<string, unknown>).ontouchstart;
Object.defineProperty(dom.window.navigator, "maxTouchPoints", { configurable: true, get: () => (coarse ? 5 : 0) });
Object.defineProperty(dom.window, "matchMedia", {
  configurable: true,
  value: (media: string) => ({ matches: media === "(pointer: coarse)" && coarse, media, addListener() {}, removeListener() {}, addEventListener() {}, removeEventListener() {}, dispatchEvent: () => false }),
});

const { cleanup, render, screen, waitFor, fireEvent, within } = await import("@testing-library/react");
const userEvent = (await import("@testing-library/user-event")).default;
// helpers/dom.ts drops a finished test's pending animation frames; Base UI's
// process-wide frame scheduler would then wait forever for the dropped one.
const { resetAnimationFrameScheduler } = await import("@base-ui/utils/useAnimationFrame");
const { Tabs, TabsList, TabItem, TabPanel } = await import("../src/components/ui/tabs.tsx");
const {
  PageLayout, PageHeader, PageHeaderContent, PageTitle, PageDescription, PageActions, PageSidebar, PageContent, PageContentHeader, PageBody,
  PageColumns, PagePrimary, PageAside, PageSubnav, PageSubnavList, PageSubnavItem,
} = await import("../src/components/ui/page-layout.tsx");
const { ScrollArea, ScrollBar } = await import("../src/components/ui/scroll-area.tsx");

beforeEach(() => resetAnimationFrameScheduler());
afterEach(() => { cleanup(); resetAnimationFrameScheduler(); coarse = false; });
after(() => {
  for (const [key, descriptor] of Object.entries(originalOffsets)) if (descriptor) Object.defineProperty(proto, key, descriptor);
  dom.window.Element.prototype.getBoundingClientRect = originalRect;
  closeChannels();
});
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
// Compares nodes by identity and names the focused element on failure:
// assert.equal would inspect whole DOM trees and exhaust memory.
function assertFocus(expected: Element, message?: string) {
  const active = document.activeElement;
  assert.ok(active === expected, `${message ?? "focus"}: expected ${expected.outerHTML.slice(0, 120)}, got ${active?.outerHTML.slice(0, 120)}`);
}
const selected = () => screen.getAllByRole("tab").filter((tab) => tab.getAttribute("aria-selected") === "true").map((tab) => tab.textContent);
const Icon = (props: { className?: string; size?: number; strokeWidth?: number }) => <svg data-testid="tab-icon" data-stroke={props.strokeWidth} />;

// ---------------------------------------------------------------------------
// Tabs
// ---------------------------------------------------------------------------

function Period({ onChange }: { onChange?: (v: string) => void }) {
  const [value, setValue] = useState("day");
  return <Tabs value={value} onValueChange={(v) => { setValue(v); onChange?.(v); }} variant="segment">
    <TabsList aria-label="Period">
      <TabItem value="day" label="Day" />
      <TabItem value="week" label="Week" disabled />
      <TabItem value="month" label="Month" />
    </TabsList>
  </Tabs>;
}

test("controlled segment tabs select by click and by arrow keys, and a disabled tab is never selected", async () => {
  const changes: string[] = [];
  render(<Period onChange={(v) => changes.push(v)} />);
  const list = screen.getByRole("tablist", { name: "Period" });
  assert.deepEqual(within(list).getAllByRole("tab").map((t) => t.textContent), ["Day", "Week", "Month"]);
  assert.deepEqual(selected(), ["Day"]);
  const user = userEvent.setup();
  await user.click(screen.getByRole("tab", { name: "Week" }));
  assert.deepEqual(selected(), ["Day"], "a disabled tab cannot be chosen");
  await user.click(screen.getByRole("tab", { name: "Month" }));
  assert.deepEqual(selected(), ["Month"]);
  await user.keyboard("{ArrowLeft}");
  assertFocus(screen.getByRole("tab", { name: "Week" }), "arrows can reach a disabled tab");
  assert.deepEqual(selected(), ["Month"], "focusing a disabled tab does not select it");
  await user.keyboard("{ArrowLeft}");
  assertFocus(screen.getByRole("tab", { name: "Day" }));
  assert.deepEqual(selected(), ["Day"], "selection follows focus by default");
  assert.deepEqual(changes, ["month", "day"]);
  await waitFor(() => assert.ok(list.querySelector(".bg-brand")), "the selected tab gets the brand-coloured indicator once measured");
});

test("a controlled host can refuse a selection and the tabs stay on its value", async () => {
  const requests: string[] = [];
  render(<Tabs value="a" onValueChange={(v) => requests.push(v)}>
    <TabsList aria-label="Locked"><TabItem value="a" label="Alpha" /><TabItem value="b" label="Beta" /></TabsList>
  </Tabs>);
  await userEvent.setup().click(screen.getByRole("tab", { name: "Beta" }));
  assert.ok(requests.length > 0 && requests.every((r) => r === "b"));
  assert.deepEqual(selected(), ["Alpha"]);
});

test("uncontrolled tabs start on defaultValue, or on the first tab, and show the matching panel", async () => {
  render(<Tabs defaultValue="logs">
    <TabsList aria-label="Detail"><TabItem value="info" label="Info" /><TabItem value="logs" label="Logs" /></TabsList>
    <TabPanel value="info">Info panel</TabPanel>
    <TabPanel value="logs">Logs panel</TabPanel>
  </Tabs>);
  assert.deepEqual(selected(), ["Logs"]);
  const shown = () => screen.getAllByRole("tabpanel", { hidden: true }).filter((panel) => !panel.hidden).map((panel) => panel.textContent);
  assert.deepEqual(shown(), ["Logs panel"]);
  await userEvent.setup().click(screen.getByRole("tab", { name: "Info" }));
  await waitFor(() => assert.deepEqual(shown(), ["Info panel"]));
  cleanup();
  render(<Tabs><TabsList aria-label="Plain"><TabItem value="one" label="One" /><TabItem value="two" label="Two" /></TabsList></Tabs>);
  await waitFor(() => assert.deepEqual(selected(), ["One"]));
});

test("index-driven tabs report the chosen index", async () => {
  function Indexed() {
    const [index, setIndex] = useState(1);
    return <Tabs selectedIndex={index} onSelect={setIndex} variant="underline" color="neutral">
      <TabsList aria-label="Indexed"><TabItem value="x" label="X" /><TabItem value="y" label="Y" /><TabItem value="z" label="Z" /></TabsList>
    </Tabs>;
  }
  render(<Indexed />);
  await waitFor(() => assert.deepEqual(selected(), ["Y"]));
  await userEvent.setup().click(screen.getByRole("tab", { name: "Z" }));
  assert.deepEqual(selected(), ["Z"]);
});

test("manual activation moves focus with the arrows and selects only on Enter", async () => {
  render(<Tabs defaultValue="a" color="default">
    <TabsList aria-label="Manual" activationMode="manual"><TabItem value="a" label="A" /><TabItem value="b" label="B" /></TabsList>
  </Tabs>);
  const user = userEvent.setup();
  await user.tab();
  assertFocus(screen.getByRole("tab", { name: "A" }));
  await user.keyboard("{ArrowRight}");
  assertFocus(screen.getByRole("tab", { name: "B" }));
  assert.deepEqual(selected(), ["A"]);
  await user.keyboard("{Enter}");
  assert.deepEqual(selected(), ["B"]);
  await user.tab();
});

test("icon tabs can show only the selected label, naming the others for assistive technology", async () => {
  render(<Tabs defaultValue="grid">
    <TabsList aria-label="View" labelVisibility="active">
      <TabItem value="grid" label="Grid" icon={Icon} />
      <TabItem value="list" label="List" icon={Icon} />
      <TabItem value="flag" label="Flagged" leading={<span>⚑</span>} />
    </TabsList>
  </Tabs>);
  const list = screen.getByRole("tab", { name: "List" });
  assert.equal(list.getAttribute("aria-label"), "List");
  assert.equal(within(list).queryByText("List"), null, "the label is hidden while unselected");
  assert.ok(within(screen.getByRole("tab", { name: "Grid" })).getByText("Grid"));
  assert.ok(within(screen.getByRole("tab", { name: "⚑Flagged" })).getByText("Flagged"), "a tab with a leading marker always shows its label");
  await userEvent.setup().click(list);
  await waitFor(() => assert.ok(within(list).queryByText("List")));
  assert.equal(list.getAttribute("aria-label"), null);
  await waitFor(() => assert.ok(within(screen.getByRole("tab", { name: "Grid" })).queryByText("Grid") === null));
});

test("the deprecated activeLabel prop behaves like labelVisibility active", () => {
  render(<Tabs defaultValue="a"><TabsList aria-label="Old" activeLabel>
    <TabItem value="a" label="Alpha" icon={Icon} /><TabItem value="b" label="Beta" icon={Icon} />
  </TabsList></Tabs>);
  assert.equal(screen.getByRole("tab", { name: "Beta" }).getAttribute("aria-label"), "Beta");
});

test("tabs show plain and configured badges", () => {
  render(<Tabs defaultValue="a" variant="pill"><TabsList aria-label="Queues">
    <TabItem value="a" label="Pending" badge={3} />
    <TabItem value="b" label="Failed" badge={{ children: "9+", color: "red" }} />
    <TabItem value="c" label="Done" badge={<em>new</em>} />
  </TabsList></Tabs>);
  assert.equal(screen.getByRole("tab", { name: "Pending3" }).querySelector("[data-slot=badge]")?.textContent, "3");
  assert.equal(screen.getByRole("tab", { name: "Failed9+" }).querySelector("[data-slot=badge]")?.textContent, "9+");
  assert.ok(screen.getByRole("tab", { name: "Donenew" }).querySelector("[data-slot=badge] em"));
});

test("closable tabs close through their own button, which is only tabbable on the selected tab", async () => {
  const closed: string[] = [];
  const clicks: string[] = [];
  let tabRef: HTMLButtonElement | null = null;
  function Closable() {
    const objectRef = useRef<HTMLButtonElement>(null);
    return <Tabs defaultValue="a"><TabsList aria-label="Files">
      <TabItem ref={(node) => { tabRef = node; }} value="a" label="a.py" onClose={() => closed.push("a")} onClick={() => clicks.push("a")} />
      <TabItem ref={objectRef} value="b" label="b.py" onClose={() => closed.push("b")} closeLabel="Close file b" />
      <TabItem value="c" label="c.py" onClose={() => closed.push("c")} disabled />
      <span>not a tab</span>
    </TabsList></Tabs>;
  }
  render(<Closable />);
  const user = userEvent.setup();
  assert.ok(tabRef === screen.getByRole("tab", { name: "a.py" }));
  assert.equal(screen.getByRole("button", { name: "Close a.py" }).getAttribute("tabindex"), "0");
  assert.equal(screen.getByRole("button", { name: "Close file b" }).getAttribute("tabindex"), "-1");
  assert.equal((screen.getByRole("button", { name: "Close c.py" }) as HTMLButtonElement).disabled, true);
  await user.click(screen.getByRole("button", { name: "Close file b" }));
  assert.deepEqual(closed, ["b"]);
  assert.deepEqual(selected(), ["a.py"], "closing does not select the tab");
  await user.click(screen.getByRole("tab", { name: "a.py" }));
  assert.deepEqual(clicks, ["a"]);
});

test("hovering draws the hover highlight and bolder icons, leaving clears them, and keyboard focus passes through the list", async () => {
  render(<><Tabs defaultValue="a"><TabsList aria-label="Hover">
    <TabItem value="a" label="A" icon={Icon} /><TabItem value="b" label="B" icon={Icon} /><TabItem value="c" label="C" />
  </TabsList></Tabs><button>After</button></>);
  const list = screen.getByRole("tablist", { name: "Hover" });
  await waitFor(() => assert.ok(list.querySelector(".bg-brand")));
  fireEvent.mouseMove(list, { clientX: 100, clientY: 10 });
  await waitFor(() => assert.ok(list.querySelector(".bg-hover")), "the pill variant highlights the hovered tab");
  const iconStrokes = () => screen.getAllByTestId("tab-icon").map((svg) => svg.getAttribute("data-stroke"));
  assert.deepEqual(iconStrokes(), ["2", "2"], "the hovered and the selected tab's icons draw bolder");
  fireEvent.mouseLeave(list);
  await waitFor(() => assert.ok(list.querySelector(".bg-hover") === null));
  assert.deepEqual(iconStrokes(), ["2", "1.5"]);
  const user = userEvent.setup();
  await user.tab();
  assertFocus(screen.getByRole("tab", { name: "A" }));
  await user.tab();
  assertFocus(screen.getByRole("button", { name: "After" }));
});

// ---------------------------------------------------------------------------
// PageLayout
// ---------------------------------------------------------------------------

test("a batches-style page puts its body inside a content panel", () => {
  render(<PageLayout gutter="none" className="h-auto gap-0" data-testid="layout">
    <PageContent><PageBody><table aria-label="Batches"><tbody><tr><td>batch-1</td></tr></tbody></table></PageBody></PageContent>
  </PageLayout>);
  const layout = screen.getByTestId("layout");
  const table = screen.getByRole("table", { name: "Batches" });
  assert.equal(table.closest("[data-slot=page-body]")?.parentElement?.getAttribute("data-slot"), "page-content");
  assert.ok(table.closest("[data-slot=page-layout]") === layout);
  assert.ok(layout.className.includes("h-auto"), "consumer classes are kept");
});

test("a page header carries a single title, its description, icon and actions", () => {
  const HeaderIcon = (props: React.SVGProps<SVGSVGElement> & { size?: number; strokeWidth?: number }) => <svg data-testid="header-icon" aria-hidden={props["aria-hidden"]} className={props.className} />;
  render(<PageLayout size="md">
    <PageHeader>
      <PageHeaderContent icon={HeaderIcon}><div><PageTitle>Models</PageTitle><PageDescription>Deployed model services</PageDescription></div></PageHeaderContent>
      <PageActions><button>Deploy</button></PageActions>
    </PageHeader>
    <PageHeaderContent>No icon</PageHeaderContent>
  </PageLayout>);
  const header = screen.getByRole("banner");
  assert.equal(within(header).getByRole("heading", { level: 1 }).textContent, "Models");
  assert.ok(within(header).getByText("Deployed model services"));
  assert.ok(within(header).getByRole("button", { name: "Deploy" }));
  assert.equal(within(header).getByTestId("header-icon").getAttribute("aria-hidden"), "true");
  assert.equal(screen.getByText("No icon").querySelector("svg"), null);
});

test("a page sidebar and an aside column take the widths they are given", () => {
  render(<PageLayout>
    <PageSidebar aria-label="Filters" style={{ color: "red" }}>filters</PageSidebar>
    <PageSidebar aria-label="Default width">x</PageSidebar>
    <PageContent>
      <PageColumns asideWidth={320} data-testid="numeric"><PagePrimary>main</PagePrimary><PageAside aria-label="Details">details</PageAside></PageColumns>
      <PageColumns asideWidth="30%" columnsAt="xl" asideSide="left" data-testid="string"><PagePrimary>main</PagePrimary></PageColumns>
      <PageColumns data-testid="default"><PagePrimary>main</PagePrimary></PageColumns>
    </PageContent>
  </PageLayout>);
  const filters = screen.getByRole("complementary", { name: "Filters" });
  assert.equal(filters.style.getPropertyValue("--page-sidebar-width"), "200px");
  assert.equal(filters.style.color, "red");
  assert.equal(screen.getByTestId("numeric").style.getPropertyValue("--page-aside-width"), "320px", "a number is read as pixels");
  assert.equal(screen.getByTestId("string").style.getPropertyValue("--page-aside-width"), "30%");
  assert.equal(screen.getByTestId("default").style.getPropertyValue("--page-aside-width"), "25rem");
  assert.ok(screen.getByRole("complementary", { name: "Details" }));
});

test("a page subnav marks the active link and can collapse the inactive ones to named icons", async () => {
  const SubIcon = (props: { className?: string; strokeWidth?: number }) => <svg data-testid="sub-icon" data-stroke={props.strokeWidth} />;
  function Subnav() {
    const [active, setActive] = useState("overview");
    return <PageContent>
      <PageContentHeader>
        <PageSubnav aria-label="Sections">
          <PageSubnavList activeValue={active} labelVisibility="active" keyboardNavigation="roving">
            <PageSubnavItem value="overview" href="#overview" icon={SubIcon} onClick={(e) => { e.preventDefault(); setActive("overview"); }}>Overview</PageSubnavItem>
            <PageSubnavItem value="metrics" href="#metrics" icon={SubIcon} onClick={(e) => { e.preventDefault(); setActive("metrics"); }}>Metrics</PageSubnavItem>
            <PageSubnavItem value="logs" href="#logs" icon={SubIcon} label="Container logs"><b>Logs</b></PageSubnavItem>
          </PageSubnavList>
        </PageSubnav>
        <PageActions><button>Refresh</button></PageActions>
      </PageContentHeader>
    </PageContent>;
  }
  render(<Subnav />);
  const nav = screen.getByRole("navigation", { name: "Sections" });
  const overview = within(nav).getByRole("link", { name: "Overview" });
  assert.equal(overview.getAttribute("aria-current"), "page");
  const metrics = within(nav).getByRole("link", { name: "Metrics" });
  assert.equal(metrics.getAttribute("aria-label"), "Metrics");
  assert.equal(within(metrics).queryByText("Metrics"), null);
  assert.ok(within(nav).getByRole("link", { name: "Container logs" }), "a non-text item is named by its label prop");
  assert.deepEqual(screen.getAllByTestId("sub-icon").map((svg) => svg.getAttribute("data-stroke")), ["2", "1.5", "1.5"]);
  const user = userEvent.setup();
  await user.click(metrics);
  assert.equal(metrics.getAttribute("aria-current"), "page");
  assert.ok(within(metrics).getByText("Metrics"));
  assert.equal(within(nav).getByRole("link", { name: "Overview" }).getAttribute("aria-label"), "Overview");
  await user.keyboard("{ArrowRight}");
  assertFocus(within(nav).getByRole("link", { name: "Container logs" }), "the subnav is a horizontal roving menu");
});

test("a subnav item outside a list shows its label and takes an explicit active state", () => {
  render(<PageSubnav aria-label="Loose"><ul>
    <PageSubnavItem value="a" href="#a" active>Alpha</PageSubnavItem>
    <PageSubnavItem value="b" href="#b">{7}</PageSubnavItem>
  </ul></PageSubnav>);
  assert.equal(screen.getByRole("link", { name: "Alpha" }).getAttribute("aria-current"), "page");
  assert.equal(screen.getByRole("link", { name: "7" }).getAttribute("aria-current"), null);
});

// ---------------------------------------------------------------------------
// ScrollArea
// ---------------------------------------------------------------------------

test("on a desktop the scroll area wraps its content in a managed viewport and exposes it through viewportRef", () => {
  let viewport: HTMLDivElement | null = null;
  render(<ScrollArea data-testid="area" viewportRef={(node) => { viewport = node; }} viewportProps={{ "aria-label": "Log output" } as never} contentClassName="content" contentProps={{ id: "log-content" }}>
    <p>line 1</p>
  </ScrollArea>);
  const area = screen.getByTestId("area");
  assert.notEqual(area.getAttribute("role"), "group", "the desktop variant is not the touch fallback group");
  assert.ok(viewport);
  assert.equal((viewport as HTMLDivElement).getAttribute("aria-label"), "Log output");
  assert.ok((viewport as HTMLDivElement).contains(screen.getByText("line 1")));
  assert.equal(screen.getByText("line 1").parentElement?.id, "log-content");
});

test("each desktop orientation shows only the scrollbars it scrolls along once content overflows", async () => {
  // jsdom has no layout: the viewport reports content ten times larger than itself on both axes.
  const isViewport = (el: HTMLElement) => el.getAttribute("data-slot") === "scroll-area-viewport";
  const sizes = { scrollHeight: 1000, scrollWidth: 1000, clientHeight: 100, clientWidth: 100 } as const;
  const target = dom.window.HTMLElement.prototype;
  for (const [key, size] of Object.entries(sizes)) {
    Object.defineProperty(target, key, { configurable: true, get(this: HTMLElement) { return isViewport(this) ? size : 0; } });
  }
  try {
    const expected = { vertical: ["vertical"], horizontal: ["horizontal"], both: ["vertical", "horizontal"] } as const;
    for (const orientation of ["vertical", "horizontal", "both"] as const) {
      const { unmount } = render(<ScrollArea orientation={orientation} data-testid={orientation}><span>{orientation} content</span></ScrollArea>);
      const area = screen.getByTestId(orientation);
      assert.ok(within(area).getByText(`${orientation} content`));
      await waitFor(() => assert.deepEqual(
        Array.from(area.querySelectorAll("[data-slot=scroll-area-scrollbar]")).map((bar) => bar.getAttribute("data-orientation")),
        expected[orientation],
      ));
      unmount();
    }
  } finally {
    for (const key of Object.keys(sizes)) delete (target as unknown as Record<string, unknown>)[key];
  }
});

test("on a touch-primary device the scroll area falls back to a native, focusable scroller", async () => {
  coarse = true;
  let viewport: HTMLDivElement | null = null;
  const { rerender } = render(<ScrollArea data-testid="area" viewportRef={(node) => { viewport = node; }} orientation="both" viewportClassName="vp">
    <p>row</p><ScrollBar />
  </ScrollArea>);
  await waitFor(() => assert.equal(screen.getByTestId("area").getAttribute("role"), "group"));
  const area = screen.getByTestId("area");
  assert.equal(area.getAttribute("aria-roledescription"), "scroll area");
  assert.equal((viewport as HTMLDivElement | null)?.getAttribute("tabindex"), "0", "keyboard users can focus and scroll it");
  assert.ok((viewport as HTMLDivElement).className.includes("overflow-auto"));
  assert.equal(area.querySelector("[data-slot=scroll-area-scrollbar]"), null, "custom scrollbars are skipped on touch");
  assert.ok(screen.getByText("row").parentElement === viewport, "without content props the children sit directly in the viewport");
  rerender(<ScrollArea data-testid="area" viewportRef={(node) => { viewport = node; }} orientation="horizontal" contentProps={{ id: "inner" }}><p>row</p></ScrollArea>);
  assert.ok((viewport as HTMLDivElement).className.includes("overflow-x-auto"));
  assert.equal(screen.getByText("row").parentElement?.id, "inner");
  assert.equal(screen.getByText("row").parentElement?.getAttribute("role"), "presentation");
  rerender(<ScrollArea data-testid="area" viewportRef={(node) => { viewport = node; }} contentClassName="c"><p>row</p></ScrollArea>);
  assert.ok((viewport as HTMLDivElement).className.includes("overflow-y-auto"));
});
