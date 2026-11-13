// Zeron CheckboxGroup (ui/src/components/ui/checkbox-group.tsx) and the
// selected-background merge/split it draws through useMergeSplitBlocks
// (ui/src/hooks/use-merge-split.tsx). The app uses it in the column filter
// popover (ui/src/ui-kit/ColumnFilter.tsx) and in DataTableFacetedFilter.
import { dom, pendingFrameCount } from "./helpers/dom.ts";
import assert from "node:assert/strict";
import { afterEach, test as nodeTest } from "node:test";
import React, { useState } from "react";

for (const name of ["KeyboardEvent", "Document"] as const) {
  Object.defineProperty(globalThis, name, { value: dom.window[name], configurable: true });
}
// jsdom has no layout. Every option row (data-proximity-index) gets a 32px box
// stacked by index, so the hover and selection overlays have rects to follow.
const proto = dom.window.HTMLElement.prototype;
const hasBox = (el: HTMLElement) => el.hasAttribute("data-proximity-index");
const indexOf = (el: HTMLElement) => Number(el.getAttribute("data-proximity-index"));
Object.defineProperties(proto, {
  offsetTop: { configurable: true, get() { return hasBox(this) ? indexOf(this) * 32 : 0; } },
  offsetLeft: { configurable: true, get() { return 0; } },
  offsetHeight: { configurable: true, get() { return hasBox(this) ? 32 : 0; } },
  offsetWidth: { configurable: true, get() { return hasBox(this) ? 200 : 0; } },
  offsetParent: { configurable: true, get() { return hasBox(this) ? this.parentElement : null; } },
});

const { render, screen, fireEvent, waitFor, cleanup, act } = await import("@testing-library/react");
const { default: userEvent } = await import("@testing-library/user-event");
const { CheckboxGroup, CheckboxItem } = await import("../src/components/ui/checkbox-group.tsx");
const { useMergeSplitBlocks } = await import("../src/hooks/use-merge-split.tsx");
afterEach(cleanup);
// helpers/dom.ts drops a finished test's pending animation frames, which would
// leave framer-motion's process-wide frame loop waiting forever on the dropped
// frame. Each test lets its animations finish first.
const test = (name: string, fn: () => unknown) => nodeTest(name, async () => {
  try { await fn(); } finally { await waitFor(() => assert.equal(pendingFrameCount(), 0)); }
});

const frame = (ms = 40) => new Promise((resolve) => setTimeout(resolve, ms));
const labels = ["Running", "Stopped", "Failed", "Pending", "Unknown"];

function Group({ initial = [] as number[], onChange, trailing, withIcon }: { initial?: number[]; onChange?: (checked: number[]) => void; trailing?: (index: number) => React.ReactNode; withIcon?: boolean }) {
  const [checked, setChecked] = useState(new Set(initial));
  const Icon = (props: { className?: string }) => <svg data-testid="option-icon" className={props.className} />;
  return <CheckboxGroup aria-label="State" checkedIndices={checked}>
    {labels.map((label, index) => <CheckboxItem key={label} index={index} label={label} checked={checked.has(index)}
      icon={withIcon ? Icon : undefined} trailing={trailing?.(index)} style={{ borderTopLeftRadius: "8px" }}
      onToggle={() => setChecked((current) => {
        const next = new Set(current);
        if (next.has(index)) next.delete(index); else next.add(index);
        onChange?.([...next].sort());
        return next;
      })} />)}
  </CheckboxGroup>;
}
const option = (name: string) => screen.getByRole("checkbox", { name });
const selectionBlocks = () => Array.from(document.querySelectorAll<HTMLElement>("[data-slot=checkbox-group] > .bg-selection"));

test("options expose checkbox semantics and toggle by click", async () => {
  let latest: number[] = [];
  render(<Group onChange={(checked) => { latest = checked; }} />);
  assert.ok(screen.getByRole("group", { name: "State" }));
  assert.equal(screen.getAllByRole("checkbox").length, labels.length, "the hidden inner checkbox is not exposed twice");
  const user = userEvent.setup();
  await user.click(option("Stopped"));
  assert.equal(option("Stopped").getAttribute("aria-checked"), "true");
  assert.deepEqual(latest, [1]);
  await user.click(option("Stopped"));
  assert.equal(option("Stopped").getAttribute("aria-checked"), "false");
  assert.deepEqual(latest, []);
});

test("clicking a row focuses the row itself so arrow navigation keeps working", async () => {
  render(<Group />);
  const user = userEvent.setup();
  await user.click(option("Failed"));
  assert.ok(document.activeElement === option("Failed"));
  await user.keyboard("{ArrowDown}");
  assert.ok(document.activeElement === option("Pending"));
});

test("clicking the inner checkbox square toggles once", async () => {
  let toggles = 0;
  render(<CheckboxGroup checkedIndices={new Set()}><CheckboxItem index={0} label="Only" checked={false} onToggle={() => { toggles += 1; }} /></CheckboxGroup>);
  const square = document.querySelector("[data-slot=checkbox]") as HTMLElement;
  fireEvent.click(square);
  assert.equal(toggles, 1);
});

test("an interactive child keeps its own focus on mouse down", async () => {
  render(<CheckboxGroup checkedIndices={new Set()}><CheckboxItem index={0} label="Only" checked={false} onToggle={() => {}} trailing={<button>Details</button>} /></CheckboxGroup>);
  const button = screen.getByRole("button", { name: "Details" });
  const event = fireEvent.mouseDown(button);
  assert.equal(event, true, "the row does not prevent the child's native focus");
  await userEvent.setup().click(button);
  assert.ok(document.activeElement === button, "the child button ends up focused, not the row");
});

test("arrow keys wrap through the options and Home/End jump to the ends", async () => {
  render(<Group />);
  const user = userEvent.setup();
  option("Running").focus();
  await user.keyboard("{ArrowUp}");
  assert.ok(document.activeElement === option("Unknown"), "ArrowUp wraps to the last option");
  await user.keyboard("{ArrowDown}");
  assert.ok(document.activeElement === option("Running"), "ArrowDown wraps to the first option");
  await user.keyboard("{End}");
  assert.ok(document.activeElement === option("Unknown"));
  await user.keyboard("{Home}");
  assert.ok(document.activeElement === option("Running"));
});

test("Space and Enter toggle the focused option; other keys do not", async () => {
  render(<Group />);
  const user = userEvent.setup();
  option("Pending").focus();
  await user.keyboard(" ");
  assert.equal(option("Pending").getAttribute("aria-checked"), "true");
  await user.keyboard("{Enter}");
  assert.equal(option("Pending").getAttribute("aria-checked"), "false");
  await user.keyboard("a");
  assert.equal(option("Pending").getAttribute("aria-checked"), "false");
});

test("keys pressed on a nested control are not treated as group navigation", () => {
  render(<CheckboxGroup checkedIndices={new Set()}>
    <CheckboxItem index={0} label="First" checked={false} onToggle={() => {}} trailing={<button>Details</button>} />
    <CheckboxItem index={1} label="Second" checked={false} onToggle={() => {}} />
  </CheckboxGroup>);
  const button = screen.getByRole("button", { name: "Details" });
  button.focus();
  const notCancelled = fireEvent.keyDown(button, { key: "ArrowDown" });
  assert.equal(notCancelled, true);
  assert.ok(document.activeElement === button);
});

test("icons and trailing counts render, and empty trailing values are omitted", () => {
  render(<Group withIcon initial={[0]} trailing={(index) => index === 0 ? 12 : index === 1 ? 0 : null} />);
  assert.equal(screen.getAllByTestId("option-icon").length, labels.length);
  const trailing = Array.from(document.querySelectorAll("[data-slot=checkbox-group-trailing]")).map((el) => el.textContent);
  assert.deepEqual(trailing, ["12", "0"], "zero is a value, null is not");
});

test("hovering and focusing rows highlights them, and leaving the group clears the highlight", async () => {
  render(<><Group withIcon /><button>Outside</button></>);
  const group = screen.getByRole("group", { name: "State" });
  await frame();
  const hover = () => group.querySelectorAll(":scope > .bg-hover").length;
  fireEvent.mouseEnter(group);
  fireEvent.mouseMove(group, { clientX: 10, clientY: 70 });
  await waitFor(() => assert.equal(hover(), 1));
  fireEvent.mouseLeave(group);
  await waitFor(() => assert.equal(hover(), 0));
  const user = userEvent.setup();
  await user.click(option("Stopped"));
  await user.keyboard("{ArrowDown}");
  await waitFor(() => assert.equal(hover(), 1));
  await user.click(screen.getByRole("button", { name: "Outside" }));
  await waitFor(() => assert.equal(hover(), 0));
});

test("an option rendered outside a group throws a descriptive error", () => {
  const original = console.error;
  console.error = () => {};
  try {
    assert.throws(() => render(<CheckboxItem index={0} label="Lonely" checked={false} onToggle={() => {}} />), /must be used within a CheckboxGroup/);
  } finally {
    console.error = original;
  }
});

test("the group forwards function and object refs to its element", () => {
  const objectRef = React.createRef<HTMLDivElement>();
  let fnNode: HTMLDivElement | null = null;
  const itemRef = React.createRef<HTMLDivElement>();
  render(<>
    <CheckboxGroup ref={objectRef} checkedIndices={new Set()}><CheckboxItem ref={itemRef} index={0} label="A" checked={false} onToggle={() => {}} /></CheckboxGroup>
    <CheckboxGroup ref={(node) => { fnNode = node; }} checkedIndices={new Set()}><CheckboxItem ref={() => {}} index={0} label="B" checked={false} onToggle={() => {}} /></CheckboxGroup>
  </>);
  assert.equal(objectRef.current?.getAttribute("data-slot"), "checkbox-group");
  assert.equal((fnNode as HTMLDivElement | null)?.getAttribute("data-slot"), "checkbox-group");
  assert.equal(itemRef.current?.getAttribute("aria-label"), "A");
});

test("contiguous checked options share one selection background", async () => {
  render(<Group initial={[0, 1, 3]} />);
  await waitFor(() => assert.equal(selectionBlocks().length, 2));
  const [top, lower] = selectionBlocks();
  await waitFor(() => assert.equal(top.style.height, "64px"));
  assert.equal(lower.style.top, "96px");
  assert.equal(lower.style.height, "32px");
});

test("checking the row between two selected runs glides them together, then settles into one background", async () => {
  render(<Group initial={[0, 2]} />);
  await waitFor(() => assert.equal(selectionBlocks().length, 2));
  const user = userEvent.setup();
  await user.click(option("Stopped"));
  // Mid-merge both halves meet at the bridging row's midpoint (row 1: 32..64).
  await waitFor(() => {
    const blocks = selectionBlocks();
    assert.equal(blocks.length, 2);
    assert.equal(blocks[0].style.height, "48px");
    assert.equal(blocks[1].style.top, "48px");
  });
  await waitFor(() => assert.equal(selectionBlocks().length, 1));
  await waitFor(() => assert.equal(selectionBlocks()[0].style.height, "96px"));
});

test("unchecking a middle row splits the background into two halves that part", async () => {
  render(<Group initial={[0, 1, 2]} />);
  await waitFor(() => assert.equal(selectionBlocks().length, 1));
  const user = userEvent.setup();
  await user.click(option("Stopped"));
  await waitFor(() => assert.equal(selectionBlocks().length, 2));
  await waitFor(() => {
    const [upper, lower] = selectionBlocks();
    assert.equal(upper.style.height, "32px");
    assert.equal(lower.style.top, "64px");
    assert.equal(lower.style.height, "32px");
  });
});

test("toggling the bridge row again mid-merge cancels the merge", async () => {
  render(<Group initial={[0, 2]} />);
  await waitFor(() => assert.equal(selectionBlocks().length, 2));
  const stopped = option("Stopped");
  fireEvent.click(stopped);
  fireEvent.click(stopped);
  await frame(400);
  const blocks = selectionBlocks();
  assert.equal(blocks.length, 2);
  assert.equal(blocks[0].style.height, "32px");
  assert.equal(blocks[1].style.top, "64px");
  assert.equal(stopped.getAttribute("aria-checked"), "false");
});

test("unmounting mid-merge clears every timer the merge left pending", async () => {
  const { unmount } = render(<Group initial={[0, 2]} />);
  await waitFor(() => assert.equal(selectionBlocks().length, 2));
  // Animation frames run on timers too (helpers/dom.ts); those are tagged and left out.
  const pending = new Set<unknown>();
  const clearedOnUnmount = new Set<unknown>();
  let inFrame = false;
  let unmounting = false;
  const realSet = globalThis.setTimeout;
  const realClear = globalThis.clearTimeout;
  const realFrame = globalThis.requestAnimationFrame;
  globalThis.requestAnimationFrame = ((callback: FrameRequestCallback) => {
    inFrame = true;
    try { return realFrame(callback); } finally { inFrame = false; }
  }) as typeof requestAnimationFrame;
  globalThis.setTimeout = ((fn: () => void, ms?: number) => {
    if (inFrame) return realSet(fn, ms);
    const id: ReturnType<typeof setTimeout> = realSet(() => { pending.delete(id); fn(); }, ms);
    pending.add(id);
    return id;
  }) as typeof setTimeout;
  globalThis.clearTimeout = ((id: ReturnType<typeof setTimeout>) => {
    if (pending.delete(id) && unmounting) clearedOnUnmount.add(id);
    realClear(id);
  }) as typeof clearTimeout;
  try {
    fireEvent.click(option("Stopped"));
    const leftPending = new Set(pending);
    assert.ok(leftPending.size > 0, "the merge scheduled a resolve timer");
    unmounting = true;
    unmount();
    unmounting = false;
    for (const id of leftPending) assert.ok(clearedOnUnmount.has(id), "a timer still pending at unmount was cleared");
  } finally {
    globalThis.setTimeout = realSet;
    globalThis.clearTimeout = realClear;
    globalThis.requestAnimationFrame = realFrame;
  }
});

// Direct hook checks for shapes the group cannot produce with stacked rows.
function Probe({ runs, rects, onBlocks }: { runs: { start: number; end: number; id: number }[]; rects: { top: number; left: number; width: number; height: number; radius: number }[]; onBlocks: (blocks: ReturnType<typeof useMergeSplitBlocks>) => void }) {
  onBlocks(useMergeSplitBlocks(runs, rects));
  return null;
}
const rect = (index: number) => ({ top: index * 32, left: 0, width: 200, height: 32, radius: 8 });

test("blocks are skipped for runs without measured rects and use the measured radius", () => {
  let blocks: ReturnType<typeof useMergeSplitBlocks> = [];
  render(<Probe runs={[{ start: 0, end: 1, id: 1 }, { start: 5, end: 5, id: 2 }]} rects={[rect(0), rect(1)]} onBlocks={(b) => { blocks = b; }} />);
  assert.equal(blocks.length, 1);
  assert.deepEqual({ top: blocks[0].top, height: blocks[0].height, radii: blocks[0].radii }, { top: 0, height: 64, radii: [8, 8, 8, 8] });
});

test("a merge whose absorbed id survives instead of the upper run still converges at the gap", async () => {
  let blocks: ReturnType<typeof useMergeSplitBlocks> = [];
  const rects = [0, 1, 2].map(rect);
  const { rerender } = render(<Probe runs={[{ start: 0, end: 0, id: 1 }, { start: 2, end: 2, id: 2 }]} rects={rects} onBlocks={(b) => { blocks = b; }} />);
  rerender(<Probe runs={[{ start: 0, end: 2, id: 2 }]} rects={rects} onBlocks={(b) => { blocks = b; }} />);
  await act(async () => {});
  const survivor = blocks.find((b) => b.key === "sel-2");
  const ghost = blocks.find((b) => b.key === "sel-1");
  assert.equal(survivor?.height, 48);
  assert.deepEqual(survivor?.radii, [8, 8, 0, 0]);
  assert.equal(ghost?.top, 48);
  assert.equal(ghost?.exitInstant, true);
});

test("a split is pinned at the seam on the very first render", () => {
  let blocks: ReturnType<typeof useMergeSplitBlocks> = [];
  const rects = [0, 1, 2].map(rect);
  const { rerender } = render(<Probe runs={[{ start: 0, end: 2, id: 1 }]} rects={rects} onBlocks={(b) => { blocks = b; }} />);
  let first: ReturnType<typeof useMergeSplitBlocks> | null = null;
  rerender(<Probe runs={[{ start: 0, end: 0, id: 1 }, { start: 2, end: 2, id: 2 }]} rects={rects} onBlocks={(b) => { blocks = b; first ??= b; }} />);
  const pinned = first as unknown as ReturnType<typeof useMergeSplitBlocks>;
  assert.equal(pinned[0].height, 48);
  assert.equal(pinned[1].top, 48);
  assert.equal(pinned[1].instant, true);
  assert.deepEqual(pinned[1].enterFrom?.radii, [0, 0, 8, 8]);
  assert.ok(blocks.length === 2);
});
