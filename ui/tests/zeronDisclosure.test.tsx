// Zeron accordion (ui/src/components/ui/accordion.tsx): the grouped form the
// batch page uses (BatchAPIUsage) and the standalone form, driven by clicks
// and the keyboard through the trigger buttons and their panels.
import { dom } from "./helpers/dom.ts";
import assert from "node:assert/strict";
import { afterEach, beforeEach, test } from "node:test";
import React, { useState } from "react";

for (const name of ["KeyboardEvent", "MouseEvent", "FocusEvent", "Document"] as const) {
  Object.defineProperty(globalThis, name, { value: dom.window[name], configurable: true });
}
// jsdom has no Web Animations API; Base UI asks for running animations.
if (!("getAnimations" in dom.window.Element.prototype)) {
  Object.defineProperty(dom.window.Element.prototype, "getAnimations", { configurable: true, value: () => [] });
}
const { render, screen, fireEvent, waitFor, cleanup, act } = await import("@testing-library/react");
const { default: userEvent } = await import("@testing-library/user-event");
const { Accordion, AccordionGroup, AccordionItem, AccordionTrigger, AccordionContent } = await import("../src/components/ui/accordion.tsx");
const { resetAnimationFrameScheduler } = await import("@base-ui/utils/useAnimationFrame");

// jsdom has no layout: give every element a 100x32 box whose top is the sum of
// its and its ancestors' in-flow sibling positions, so distinct rows get distinct tops
// and the group's hover/focus geometry has something to read.
const proto = dom.window.HTMLElement.prototype;
const layoutKeys = ["offsetTop", "offsetLeft", "offsetWidth", "offsetHeight", "offsetParent"] as const;
const originals = new Map(layoutKeys.map((key) => [key, Object.getOwnPropertyDescriptor(proto, key)]));
function stubLayout() {
  // Absolutely positioned overlays (the highlight layers) take no flow space.
  const inFlow = (el: Element) => !el.classList.contains("absolute");
  const indexOf = (el: HTMLElement) => (el.parentElement ? [...el.parentElement.children].filter(inFlow).indexOf(el) : 0);
  Object.defineProperties(proto, {
    offsetTop: { configurable: true, get() { let top = 0; for (let el: HTMLElement | null = this; el; el = el.parentElement) top += indexOf(el) * 32; return top; } },
    offsetLeft: { configurable: true, get: () => 0 },
    offsetWidth: { configurable: true, get: () => 100 },
    offsetHeight: { configurable: true, get: () => 32 },
    offsetParent: { configurable: true, get: () => document.body },
  });
}
function restoreLayout() {
  for (const [key, descriptor] of originals) {
    if (descriptor) Object.defineProperty(proto, key, descriptor);
  }
}

beforeEach(() => { resetAnimationFrameScheduler(); stubLayout(); });
afterEach(() => { cleanup(); restoreLayout(); resetAnimationFrameScheduler(); });

const trigger = (name: string) => screen.getByRole("button", { name });
const expanded = (name: string) => trigger(name).getAttribute("aria-expanded") === "true";
const panelHidden = (text: string) => screen.getByText(text).closest("[role=region]")?.hasAttribute("hidden") ?? true;

function Faq(props: React.ComponentProps<typeof AccordionGroup>) {
  return (
    <AccordionGroup {...props}>
      <AccordionItem value="api" index={0}><AccordionTrigger>Batch API</AccordionTrigger><AccordionContent>Upload a JSONL file</AccordionContent></AccordionItem>
      <AccordionItem value="limits" index={1}><AccordionTrigger>Limits</AccordionTrigger><AccordionContent>At most 50,000 requests</AccordionContent></AccordionItem>
      <AccordionItem value="locked" index={2} disabled><AccordionTrigger>Locked</AccordionTrigger><AccordionContent>Never shown</AccordionContent></AccordionItem>
    </AccordionGroup>
  );
}

test("a grouped accordion starts closed, opens one panel at a time, and collapses the open one", async () => {
  render(<Faq collapsible />);
  const user = userEvent.setup();
  assert.equal(expanded("Batch API"), false);
  assert.equal(panelHidden("Upload a JSONL file"), true);
  await user.click(trigger("Batch API"));
  assert.equal(expanded("Batch API"), true);
  assert.equal(panelHidden("Upload a JSONL file"), false);
  const region = screen.getByRole("region");
  assert.equal(trigger("Batch API").getAttribute("aria-controls"), region.id);
  await user.click(trigger("Limits"));
  assert.equal(expanded("Limits"), true);
  assert.equal(expanded("Batch API"), false);
  await waitFor(() => assert.equal(panelHidden("Upload a JSONL file"), true));
  await user.click(trigger("Limits"));
  assert.equal(expanded("Limits"), false);
  await waitFor(() => assert.equal(panelHidden("At most 50,000 requests"), true));
});

test("a disabled item in a group cannot be opened", async () => {
  render(<Faq />);
  const user = userEvent.setup();
  await user.click(trigger("Locked"));
  assert.equal(expanded("Locked"), false);
  assert.equal(panelHidden("Never shown"), true);
});

test("a grouped accordion opens from its default value and reports changes to a controlling parent", async () => {
  const seen: string[] = [];
  function Controlled() {
    const [value, setValue] = useState("limits");
    return <Faq value={value} onValueChange={(next: string) => { seen.push(next); setValue(next); }} />;
  }
  render(<Controlled />);
  const user = userEvent.setup();
  assert.equal(expanded("Limits"), true);
  assert.equal(panelHidden("At most 50,000 requests"), false);
  await user.click(trigger("Batch API"));
  assert.deepEqual(seen, ["api"]);
  assert.equal(expanded("Batch API"), true);
  await user.click(trigger("Batch API"));
  assert.deepEqual(seen, ["api", ""]);
  assert.equal(expanded("Batch API"), false);
});

test("an uncontrolled single group honours its default value", () => {
  render(<Faq defaultValue="api" />);
  assert.equal(expanded("Batch API"), true);
  assert.equal(expanded("Limits"), false);
});

test("a multiple group keeps several panels open and reports the full set", async () => {
  const seen: string[][] = [];
  render(<Faq type="multiple" defaultValue={["api"]} />);
  const user = userEvent.setup();
  assert.equal(expanded("Batch API"), true);
  await user.click(trigger("Limits"));
  assert.equal(expanded("Batch API"), true);
  assert.equal(expanded("Limits"), true);
  cleanup();
  function Controlled() {
    const [value, setValue] = useState<string[]>([]);
    return <Faq type="multiple" value={value} onValueChange={(next: string[]) => { seen.push(next); setValue(next); }} />;
  }
  render(<Controlled />);
  await user.click(trigger("Batch API"));
  await user.click(trigger("Limits"));
  assert.deepEqual(seen, [["api"], ["api", "limits"]]);
  await user.click(trigger("Batch API"));
  assert.deepEqual(seen.at(-1), ["limits"]);
});

test("a multiple group without a default value starts with every panel closed", async () => {
  render(<Faq type="multiple" />);
  assert.equal(expanded("Batch API"), false);
  assert.equal(expanded("Limits"), false);
});

test("triggers are operable with the keyboard: Tab moves between triggers, Enter and Space toggle", async () => {
  render(<Faq />);
  const user = userEvent.setup();
  await user.tab();
  assert.ok(document.activeElement === trigger("Batch API"), "first trigger focused");
  await user.keyboard("{Enter}");
  assert.equal(expanded("Batch API"), true);
  await user.tab();
  assert.ok(document.activeElement === trigger("Limits"), "Tab skips the open panel's plain text and lands on the next trigger");
  await user.keyboard(" ");
  assert.equal(expanded("Limits"), true);
  assert.equal(expanded("Batch API"), false);
  await user.keyboard(" ");
  assert.equal(expanded("Limits"), false);
});

test("focus inside an open panel and leaving the group do not break toggling", async () => {
  render(
    <>
      <AccordionGroup defaultValue="api">
        <AccordionItem value="api" index={0}><AccordionTrigger>Batch API</AccordionTrigger><AccordionContent><button>Copy command</button></AccordionContent></AccordionItem>
        <AccordionItem value="limits" index={1}><AccordionTrigger>Limits</AccordionTrigger><AccordionContent>At most 50,000 requests</AccordionContent></AccordionItem>
      </AccordionGroup>
      <button>Outside</button>
    </>,
  );
  const user = userEvent.setup();
  await act(() => new Promise((resolve) => setTimeout(resolve, 60)));
  await user.click(trigger("Limits"));
  assert.equal(expanded("Limits"), true);
  await user.click(trigger("Batch API"));
  await user.click(screen.getByRole("button", { name: "Copy command" }));
  assert.ok(document.activeElement === screen.getByRole("button", { name: "Copy command" }), "panel control focused");
  await user.click(screen.getByRole("button", { name: "Outside" }));
  assert.equal(expanded("Batch API"), true, "moving focus does not close the open item");
});

test("hovering a grouped trigger highlights it and leaving clears the highlight", async () => {
  const { container } = render(<Faq defaultValue="limits" />);
  const group = container.firstElementChild as HTMLElement;
  group.getBoundingClientRect = () => ({ top: 0, left: 0, width: 100, height: 32, right: 100, bottom: 32, x: 0, y: 0, toJSON() {} }) as DOMRect;
  await act(() => new Promise((resolve) => setTimeout(resolve, 60)));
  const stroke = (name: string) => trigger(name).querySelector("svg")?.getAttribute("stroke-width");
  assert.equal(stroke("Batch API"), "1.5");
  fireEvent.mouseEnter(group);
  // Pointer positions come from the stubbed layout, where a trigger sits at its row's top.
  const rowTop = trigger("Batch API").offsetTop;
  fireEvent.mouseMove(group, { clientX: 10, clientY: rowTop + 5 });
  await waitFor(() => assert.equal(stroke("Batch API"), "2"));
  // The pointer over the open item's panel is not a trigger hover.
  // The open item measures from its trigger's top; one row (32px) down is inside its panel.
  fireEvent.mouseMove(group, { clientX: 10, clientY: trigger("Limits").offsetTop + 32 });
  await waitFor(() => assert.equal(stroke("Batch API"), "1.5"));
  assert.equal(stroke("Locked"), "1.5", "the row below the panel is not highlighted either");
  fireEvent.mouseMove(group, { clientX: 10, clientY: rowTop + 5 });
  await waitFor(() => assert.equal(stroke("Batch API"), "2"));
  fireEvent.mouseLeave(group);
  await waitFor(() => assert.equal(stroke("Batch API"), "1.5"));
});

test("a standalone accordion toggles a single item, highlights on hover, and reports values", async () => {
  const seen: string[] = [];
  render(
    <Accordion onValueChange={(v: string) => seen.push(v)} value="">
      <AccordionItem value="a"><AccordionTrigger>Alpha</AccordionTrigger><AccordionContent>Alpha body</AccordionContent></AccordionItem>
    </Accordion>,
  );
  const user = userEvent.setup();
  await user.click(trigger("Alpha"));
  assert.deepEqual(seen, ["a"]);
  assert.equal(expanded("Alpha"), false, "controlled value stays closed until the parent agrees");
  const stroke = () => trigger("Alpha").querySelector("svg")?.getAttribute("stroke-width");
  fireEvent.mouseEnter(trigger("Alpha").closest("div.relative")!);
  assert.equal(stroke(), "2");
  fireEvent.mouseLeave(trigger("Alpha").closest("div.relative")!);
  assert.equal(stroke(), "1.5");
});

test("an uncontrolled standalone accordion opens its default item and switches on click", async () => {
  render(
    <Accordion defaultValue="a">
      <AccordionItem value="a"><AccordionTrigger>Alpha</AccordionTrigger><AccordionContent>Alpha body</AccordionContent></AccordionItem>
      <AccordionItem value="b"><AccordionTrigger>Beta</AccordionTrigger><AccordionContent>Beta body</AccordionContent></AccordionItem>
    </Accordion>,
  );
  const user = userEvent.setup();
  assert.equal(expanded("Alpha"), true);
  await user.click(trigger("Beta"));
  assert.equal(expanded("Beta"), true);
  assert.equal(expanded("Alpha"), false);
  await user.click(trigger("Beta"));
  assert.equal(expanded("Beta"), false);
});

test("a standalone multiple accordion keeps items open independently, controlled or not", async () => {
  const user = userEvent.setup();
  render(
    <Accordion type="multiple" defaultValue={["a"]}>
      <AccordionItem value="a"><AccordionTrigger>Alpha</AccordionTrigger><AccordionContent>Alpha body</AccordionContent></AccordionItem>
      <AccordionItem value="b"><AccordionTrigger>Beta</AccordionTrigger><AccordionContent>Beta body</AccordionContent></AccordionItem>
    </Accordion>,
  );
  await user.click(trigger("Beta"));
  assert.equal(expanded("Alpha"), true);
  assert.equal(expanded("Beta"), true);
  cleanup();
  const seen: string[][] = [];
  render(
    <Accordion type="multiple" value={["b"]} onValueChange={(v: string[]) => seen.push(v)}>
      <AccordionItem value="a"><AccordionTrigger>Alpha</AccordionTrigger><AccordionContent>Alpha body</AccordionContent></AccordionItem>
      <AccordionItem value="b"><AccordionTrigger>Beta</AccordionTrigger><AccordionContent>Beta body</AccordionContent></AccordionItem>
    </Accordion>,
  );
  assert.equal(expanded("Beta"), true);
  await user.click(trigger("Alpha"));
  assert.deepEqual(seen, [["b", "a"]]);
  cleanup();
  render(
    <Accordion type="multiple">
      <AccordionItem value="a"><AccordionTrigger>Alpha</AccordionTrigger><AccordionContent>Alpha body</AccordionContent></AccordionItem>
    </Accordion>,
  );
  assert.equal(expanded("Alpha"), false);
  await user.click(trigger("Alpha"));
  assert.equal(expanded("Alpha"), true);
});

test("forwarded refs reach the group, standalone root and item elements", () => {
  const groupRef = React.createRef<HTMLDivElement>();
  const rootRef = React.createRef<HTMLDivElement>();
  let itemNode: HTMLDivElement | null = null;
  render(
    <>
      <AccordionGroup ref={groupRef} aria-label="Group"><AccordionItem value="x" index={0} ref={(n) => { itemNode = n; }}><AccordionTrigger>X</AccordionTrigger><AccordionContent>X body</AccordionContent></AccordionItem></AccordionGroup>
      <Accordion ref={rootRef} aria-label="Root"><AccordionItem value="y" ref={React.createRef()}><AccordionTrigger>Y</AccordionTrigger><AccordionContent>Y body</AccordionContent></AccordionItem></Accordion>
    </>,
  );
  assert.equal(groupRef.current?.getAttribute("aria-label"), "Group");
  assert.equal(rootRef.current?.getAttribute("aria-label"), "Root");
  assert.ok((itemNode as HTMLDivElement | null)?.contains(trigger("X")));
  let fnGroup: HTMLDivElement | null = null;
  cleanup();
  render(<AccordionGroup ref={(n) => { fnGroup = n; }}><AccordionItem value="z"><AccordionTrigger>Z</AccordionTrigger><AccordionContent>Z body</AccordionContent></AccordionItem></AccordionGroup>);
  assert.ok((fnGroup as HTMLDivElement | null)?.contains(trigger("Z")));
});

test("a trigger outside an item fails loudly", () => {
  const original = console.error;
  console.error = () => {};
  try {
    assert.throws(() => render(<Accordion><AccordionTrigger>Orphan</AccordionTrigger></Accordion>), /must be used within an AccordionItem/);
  } finally {
    console.error = original;
  }
});
