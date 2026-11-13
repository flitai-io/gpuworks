// Zeron hooks: use-proximity-hover (the nearest-item highlight behind NavMenu
// and Tabs) and use-touch-primary (the touch/desktop switch behind ScrollArea).
import { dom } from "./helpers/dom.ts";
import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import React, { useRef } from "react";

for (const name of ["AbortController", "AbortSignal", "Event", "PointerEvent", "MouseEvent"] as const) {
  if (dom.window[name]) Object.defineProperty(globalThis, name, { value: dom.window[name], configurable: true });
}

// jsdom does no layout. Elements carrying data-layout="top left width height"
// report that box through offset* and getBoundingClientRect; every other
// element has no layout box, like content inside a hidden popup.
function box(element: HTMLElement) {
  const raw = element.getAttribute("data-layout");
  if (!raw) return null;
  const [top, left, width, height] = raw.split(" ").map(Number);
  return { top, left, width, height };
}
const proto = dom.window.HTMLElement.prototype;
Object.defineProperties(proto, {
  offsetParent: { configurable: true, get(this: HTMLElement) { return box(this) ? this.parentElement : null; } },
  offsetTop: { configurable: true, get(this: HTMLElement) { return box(this)?.top ?? 0; } },
  offsetLeft: { configurable: true, get(this: HTMLElement) { return box(this)?.left ?? 0; } },
  offsetWidth: { configurable: true, get(this: HTMLElement) { return box(this)?.width ?? 0; } },
  offsetHeight: { configurable: true, get(this: HTMLElement) { return box(this)?.height ?? 0; } },
});
const originalRect = dom.window.Element.prototype.getBoundingClientRect;
dom.window.Element.prototype.getBoundingClientRect = function (this: HTMLElement) {
  const visual = this.getAttribute("data-visual");
  if (visual) {
    const [top, left, width, height] = visual.split(" ").map(Number);
    return { top, left, width, height, right: left + width, bottom: top + height, x: left, y: top, toJSON() {} } as DOMRect;
  }
  const b = box(this);
  if (!b) return originalRect.call(this);
  return { ...b, right: b.left + b.width, bottom: b.top + b.height, x: b.left, y: b.top, toJSON() {} } as DOMRect;
};

// matchMedia for "(pointer: coarse)", answering from a settable flag and
// honouring the AbortSignal passed with each listener.
let coarse = false;
const mediaListeners = new Set<() => void>();
Object.defineProperty(dom.window, "matchMedia", {
  configurable: true,
  value: (media: string) => ({
    get matches() { return media === "(pointer: coarse)" && coarse; },
    media,
    addListener() {}, removeListener() {},
    addEventListener: (_type: string, listener: () => void, options?: { signal?: AbortSignal }) => {
      mediaListeners.add(listener);
      options?.signal?.addEventListener("abort", () => mediaListeners.delete(listener));
    },
    removeEventListener: (_type: string, listener: () => void) => mediaListeners.delete(listener),
    dispatchEvent: () => false,
  }),
});
// jsdom defines window.ontouchstart, so touch support is read from maxTouchPoints alone here.
delete (dom.window as unknown as Record<string, unknown>).ontouchstart;
let touchPoints = 0;
Object.defineProperty(dom.window.navigator, "maxTouchPoints", { configurable: true, get: () => touchPoints });

const { render, screen, fireEvent, waitFor, cleanup, act } = await import("@testing-library/react");
const { useProximityHover, useRegisterProximityItem } = await import("../src/hooks/use-proximity-hover.ts");
const { useTouchPrimary } = await import("../src/hooks/use-touch-primary.tsx");

afterEach(() => {
  cleanup();
  coarse = false;
  touchPoints = 0;
  mediaListeners.clear();
});

type Axis = "x" | "y" | "xy";
function Item({ index, register, layout, label, radius }: {
  index: number; register: (index: number, element: HTMLElement | null) => void; layout?: string; label: string; radius?: string;
}) {
  const ref = useRef<HTMLDivElement>(null);
  useRegisterProximityItem(register, index, ref);
  return <div ref={ref} data-layout={layout} style={radius ? { borderTopLeftRadius: radius } : undefined}>{label}</div>;
}
function ProximityList({ axis, layouts, containerLayout = "0 0 400 400", visual, radius }: {
  axis?: Axis; layouts: (string | undefined)[]; containerLayout?: string; visual?: string; radius?: string;
}) {
  const ref = useRef<HTMLDivElement>(null);
  const { activeIndex, isMeasured, itemRects, handlers, registerItem, remeasure } = useProximityHover(ref, axis ? { axis } : undefined);
  return (
    <>
      <div ref={ref} data-testid="list" data-layout={containerLayout} data-visual={visual} {...handlers}>
        {layouts.map((layout, index) => <Item key={index} index={index} register={registerItem} layout={layout} label={`item ${index}`} radius={radius} />)}
      </div>
      <output aria-label="hovered">{activeIndex === null ? "none" : `item ${activeIndex}`}</output>
      <output aria-label="measured">{isMeasured ? "yes" : "no"}</output>
      <output aria-label="rects">{itemRects.map((r) => r ? `${r.top}/${r.left}/${r.width}/${r.height}/${r.radius}` : "-").join(" ")}</output>
      <button onClick={remeasure}>Remeasure</button>
    </>
  );
}
const hovered = () => screen.getByLabelText("hovered").textContent;
const frame = () => act(() => new Promise((resolve) => setTimeout(resolve, 40)));

test("a vertical list measures its items and highlights the item under the pointer, or the nearest one in a gap", async () => {
  render(<ProximityList layouts={["0 0 200 32", "40 0 200 32", "80 0 200 32"]} />);
  assert.equal(screen.getByLabelText("measured").textContent, "no", "nothing is reported as measured before the first frame");
  await waitFor(() => assert.equal(screen.getByLabelText("measured").textContent, "yes"));
  assert.equal(screen.getByLabelText("rects").textContent, "0/0/200/32/0 40/0/200/32/0 80/0/200/32/0");
  const list = screen.getByTestId("list");
  fireEvent.mouseEnter(list);
  fireEvent.mouseMove(list, { clientX: 10, clientY: 45 });
  await waitFor(() => assert.equal(hovered(), "item 1"));
  // 77 lies in the gap between item 1 (40-72) and item 2 (80-112) and is closer to item 2's centre.
  fireEvent.mouseMove(list, { clientX: 10, clientY: 77 });
  await waitFor(() => assert.equal(hovered(), "item 2"));
  fireEvent.mouseMove(list, { clientX: 10, clientY: 10 });
  fireEvent.mouseLeave(list);
  assert.equal(hovered(), "none", "leaving the list clears the highlight at once");
  await frame();
  assert.equal(hovered(), "none", "a move still pending when the pointer left does not bring the highlight back");
});

test("a horizontal strip resolves the pointer along x only", async () => {
  render(<ProximityList axis="x" layouts={["0 0 80 32", "0 90 80 32", "0 180 80 32"]} />);
  await waitFor(() => assert.equal(screen.getByLabelText("measured").textContent, "yes"));
  const list = screen.getByTestId("list");
  fireEvent.mouseMove(list, { clientX: 200, clientY: 300 });
  await waitFor(() => assert.equal(hovered(), "item 2"));
  fireEvent.mouseMove(list, { clientX: 100, clientY: 0 });
  await waitFor(() => assert.equal(hovered(), "item 1"));
});

test("a grid prefers the card the pointer is inside and otherwise picks the nearest card in both directions", async () => {
  render(<ProximityList axis="xy" layouts={["0 0 100 100", "0 120 100 100", "120 0 100 100", "120 120 100 100"]} />);
  await waitFor(() => assert.equal(screen.getByLabelText("measured").textContent, "yes"));
  const list = screen.getByTestId("list");
  fireEvent.mouseMove(list, { clientX: 150, clientY: 150 });
  await waitFor(() => assert.equal(hovered(), "item 3"));
  fireEvent.mouseMove(list, { clientX: 115, clientY: 20 });
  await waitFor(() => assert.equal(hovered(), "item 1"), "in the gap the closest centre wins: (115,20) is nearer card 1 than card 0");
  fireEvent.mouseMove(list, { clientX: 20, clientY: 140 });
  await waitFor(() => assert.equal(hovered(), "item 2"));
});

test("a scaled container maps item positions into the pointer's visual space", async () => {
  // Laid out 200px tall but drawn 400px tall (transform: scale(2)).
  render(<ProximityList layouts={["0 0 100 32", "40 0 100 32", "80 0 100 32"]} containerLayout="0 0 100 200" visual="0 0 200 400" />);
  await waitFor(() => assert.equal(screen.getByLabelText("measured").textContent, "yes"));
  const list = screen.getByTestId("list");
  fireEvent.mouseMove(list, { clientX: 10, clientY: 100 });
  // Unscaled, y=100 would be inside item 2 (80-112); scaled, item 1 spans 80-144.
  await waitFor(() => assert.equal(hovered(), "item 1"));
});

test("a scaled grid maps both axes into the pointer's visual space", async () => {
  render(<ProximityList axis="xy" layouts={["0 0 50 50", "0 60 50 50"]} containerLayout="0 0 110 50" visual="0 0 220 100" />);
  await waitFor(() => assert.equal(screen.getByLabelText("measured").textContent, "yes"));
  const list = screen.getByTestId("list");
  fireEvent.mouseMove(list, { clientX: 90, clientY: 50 });
  // Unscaled, x=90 is inside card 1 (60-110); scaled by 2, card 0 spans 0-100.
  await waitFor(() => assert.equal(hovered(), "item 0"));
});

test("items without a layout box are never published as zeroed rects and readiness waits for a successful remeasure", async () => {
  const { rerender } = render(<ProximityList layouts={["0 0 100 32", undefined]} />);
  // The hook retries for three frames, then gives up while the list stays hidden.
  for (let i = 0; i < 5; i += 1) await frame();
  assert.equal(screen.getByLabelText("measured").textContent, "no");
  assert.equal(screen.getByLabelText("rects").textContent, "");
  rerender(<ProximityList layouts={["0 0 100 32", "40 0 100 32"]} radius="8px" />);
  fireEvent.click(screen.getByRole("button", { name: "Remeasure" }));
  await waitFor(() => assert.equal(screen.getByLabelText("measured").textContent, "yes"));
  assert.equal(screen.getByLabelText("rects").textContent, "0/0/100/32/8 40/0/100/32/8", "the corner radius is read from the computed style");
});

test("a theme radius change event remeasures the items' corner radius (currently the only trigger for a radius-only change)", async () => {
  const { rerender } = render(<ProximityList layouts={["0 0 100 32"]} radius="4px" />);
  await waitFor(() => assert.equal(screen.getByLabelText("rects").textContent, "0/0/100/32/4"));
  rerender(<ProximityList layouts={["0 0 100 32"]} radius="12px" />);
  await frame();
  assert.equal(screen.getByLabelText("rects").textContent, "0/0/100/32/4", "current behaviour, not a contract: a radius-only style change waits for the event");
  act(() => { window.dispatchEvent(new window.Event("zeron:radius-change")); });
  await waitFor(() => assert.equal(screen.getByLabelText("rects").textContent, "0/0/100/32/12"));
  rerender(<ProximityList layouts={["0 0 100 32"]} radius="1rem" />);
  act(() => { window.dispatchEvent(new window.Event("zeron:radius-change")); });
  await waitFor(() => assert.equal(screen.getByLabelText("rects").textContent, "0/0/100/32/0"), "a radius that is not in px counts as square");
});

test("removing an item remeasures the remaining set", async () => {
  const { rerender } = render(<ProximityList layouts={["0 0 100 32", "40 0 100 32"]} />);
  await waitFor(() => assert.equal(screen.getByLabelText("rects").textContent, "0/0/100/32/0 40/0/100/32/0"));
  rerender(<ProximityList layouts={["0 0 100 32"]} />);
  await waitFor(() => assert.equal(screen.getByLabelText("rects").textContent, "0/0/100/32/0"));
});

test("unmounting with a measurement and a pointer move pending cancels every frame still scheduled", async () => {
  const pending = new Set<number>();
  const realFrame = globalThis.requestAnimationFrame;
  const realCancel = globalThis.cancelAnimationFrame;
  globalThis.requestAnimationFrame = ((callback: FrameRequestCallback) => {
    const id = realFrame((time) => { pending.delete(id); callback(time); });
    pending.add(id);
    return id;
  }) as typeof requestAnimationFrame;
  globalThis.cancelAnimationFrame = ((id: number) => { pending.delete(id); realCancel(id); }) as typeof cancelAnimationFrame;
  try {
    render(<ProximityList layouts={["0 0 100 32"]} />);
    fireEvent.mouseMove(screen.getByTestId("list"), { clientX: 5, clientY: 5 });
    assert.ok(pending.size > 0, "the pointer move scheduled a frame");
    cleanup();
    assert.equal(pending.size, 0, "unmount cancelled the pending frames");
  } finally {
    globalThis.requestAnimationFrame = realFrame;
    globalThis.cancelAnimationFrame = realCancel;
  }
});

function TouchProbe() {
  return <output aria-label="touch">{useTouchPrimary() ? "touch" : "pointer"}</output>;
}
const touch = () => screen.getByLabelText("touch").textContent;

test("touch-primary needs both a coarse pointer and touch support", () => {
  coarse = true;
  touchPoints = 0;
  render(<TouchProbe />);
  assert.equal(touch(), "pointer", "a coarse pointer without touch points is not a touch device");
  cleanup();
  coarse = false;
  touchPoints = 5;
  render(<TouchProbe />);
  assert.equal(touch(), "pointer", "a touch screen whose primary pointer is fine (a laptop with touch) stays in pointer mode");
  cleanup();
  coarse = true;
  render(<TouchProbe />);
  assert.equal(touch(), "touch");
});

test("touch-primary follows pointer-mode media changes and pointer input, and stops listening once unmounted", () => {
  touchPoints = 5;
  render(<TouchProbe />);
  assert.equal(touch(), "pointer");
  coarse = true;
  act(() => { for (const listener of [...mediaListeners]) listener(); });
  assert.equal(touch(), "touch");
  coarse = false;
  act(() => { window.dispatchEvent(new window.Event("pointerdown")); });
  assert.equal(touch(), "pointer", "a pointerdown re-reads the pointer mode");
  assert.equal(mediaListeners.size, 1);
  cleanup();
  assert.equal(mediaListeners.size, 0, "the media listener is removed on unmount");
});
