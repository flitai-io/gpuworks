// Zeron Switch (ui/src/components/ui/switch.tsx). The app renders it
// controlled and disabled-until-loaded in the Kubernetes cluster drawer
// (ui/src/pages/kubernetes/KubeClusterDrawer.tsx, "keep cards").
import { dom, pendingFrameCount } from "./helpers/dom.ts";
import assert from "node:assert/strict";
import { afterEach, test as nodeTest } from "node:test";
import React, { createRef, useState } from "react";

for (const name of ["KeyboardEvent", "MouseEvent", "PointerEvent", "FormData", "HTMLFormElement", "Event"] as const) {
  if (dom.window[name]) Object.defineProperty(globalThis, name, { value: dom.window[name], configurable: true });
}
if (!("getAnimations" in dom.window.Element.prototype)) {
  Object.defineProperty(dom.window.Element.prototype, "getAnimations", { configurable: true, value: () => [] });
}
// The drag handlers capture the pointer; jsdom has no pointer capture.
const captured: number[] = [];
Object.defineProperty(dom.window.Element.prototype, "setPointerCapture", { configurable: true, value: (id: number) => { captured.push(id); } });
Object.defineProperty(dom.window.Element.prototype, "releasePointerCapture", { configurable: true, value() {} });
Object.defineProperty(dom.window.Element.prototype, "hasPointerCapture", { configurable: true, value: () => false });

const { render, screen, fireEvent, waitFor, cleanup, act } = await import("@testing-library/react");
const { default: userEvent } = await import("@testing-library/user-event");
const { Switch } = await import("../src/components/ui/switch.tsx");
afterEach(cleanup);
// helpers/dom.ts drops a finished test's pending animation frames, which would
// leave framer-motion's frame loop waiting forever. Let animations finish first.
const test = (name: string, fn: () => unknown, options?: { todo?: string }) => nodeTest(name, options ?? {}, async () => {
  try { await fn(); } finally { await waitFor(() => assert.equal(pendingFrameCount(), 0)); }
});

/** Mirrors KubeClusterDrawer: controlled, disabled until defaults load. */
function KeepCards({ loaded = true, onChange }: { loaded?: boolean; onChange?: (checked: boolean) => void }) {
  const [keep, setKeep] = useState(false);
  return <Switch label="Keep cards" checked={keep} disabled={!loaded} onCheckedChange={(checked) => { onChange?.(checked); setKeep(checked); }} />;
}

const toggle = () => screen.getByRole("switch", { name: "Keep cards" });
const checkedOf = () => toggle().getAttribute("aria-checked");

// ── App usage: controlled + disabled ────────────────────

test("the controlled switch is named by its label and toggles when the track or the label is clicked", async () => {
  const calls: boolean[] = [];
  render(<KeepCards onChange={(c) => calls.push(c)} />);
  const user = userEvent.setup();
  assert.equal(checkedOf(), "false");
  await user.click(toggle());
  assert.equal(checkedOf(), "true");
  await user.click(screen.getByText("Keep cards"));
  assert.equal(checkedOf(), "false");
  // One callback per click: the hidden checkbox click must not double toggle.
  assert.deepEqual(calls, [true, false]);
});

test("Space and Enter toggle the focused switch", async () => {
  const calls: boolean[] = [];
  render(<KeepCards onChange={(c) => calls.push(c)} />);
  const user = userEvent.setup();
  await user.tab();
  assert.equal(document.activeElement === toggle(), true);
  await user.keyboard(" ");
  assert.equal(checkedOf(), "true");
  await user.keyboard("{Enter}");
  assert.equal(checkedOf(), "false");
  assert.deepEqual(calls, [true, false]);
});

test("a disabled switch ignores clicks on the track and label and the keyboard", async () => {
  const calls: boolean[] = [];
  render(<KeepCards loaded={false} onChange={(c) => calls.push(c)} />);
  assert.equal(toggle().hasAttribute("data-disabled"), true);
  fireEvent.click(toggle());
  fireEvent.click(screen.getByText("Keep cards"));
  fireEvent.keyDown(toggle(), { key: " " });
  fireEvent.keyDown(toggle(), { key: "Enter" });
  fireEvent.pointerDown(toggle(), { pointerId: 1, clientX: 0, pointerType: "mouse", button: 0 });
  fireEvent.pointerMove(toggle(), { pointerId: 1, clientX: 20 });
  fireEvent.pointerUp(toggle(), { pointerId: 1, clientX: 20 });
  assert.equal(checkedOf(), "false");
  assert.deepEqual(calls, []);
});

test("a controlled switch only changes when the parent updates checked", async () => {
  const calls: boolean[] = [];
  const { rerender } = render(<Switch label="Keep cards" checked={false} onCheckedChange={(c) => calls.push(c)} />);
  fireEvent.click(screen.getByText("Keep cards"));
  assert.deepEqual(calls, [true]);
  assert.equal(checkedOf(), "false");
  rerender(<Switch label="Keep cards" checked onCheckedChange={(c) => calls.push(c)} />);
  assert.equal(checkedOf(), "true");
  assert.equal(toggle().hasAttribute("data-checked"), true);
});

// ── Other public props ──────────────────────────────────

test("an uncontrolled switch starts from defaultChecked and calls onToggle and onCheckedChange", async () => {
  let toggles = 0;
  const calls: boolean[] = [];
  render(<Switch label="Keep cards" defaultChecked onToggle={() => toggles++} onCheckedChange={(c) => calls.push(c)} />);
  assert.equal(checkedOf(), "true");
  await userEvent.setup().click(toggle());
  assert.equal(checkedOf(), "false");
  assert.equal(toggles, 1);
  assert.deepEqual(calls, [false]);
});

test("a read-only switch keeps its state on click, keyboard and drag", async () => {
  let toggles = 0;
  render(<Switch label="Keep cards" readOnly defaultChecked onToggle={() => toggles++} />);
  assert.equal(toggle().getAttribute("aria-readonly"), "true");
  const user = userEvent.setup();
  await user.click(toggle());
  await user.click(screen.getByText("Keep cards"));
  toggle().focus();
  await user.keyboard(" ");
  const root = toggle().parentElement!;
  fireEvent.pointerDown(root, { pointerId: 1, clientX: 20 });
  fireEvent.pointerMove(root, { pointerId: 1, clientX: 0 });
  fireEvent.pointerUp(root, { pointerId: 1 });
  assert.equal(checkedOf(), "true");
  assert.equal(toggles, 0);
});

test("an sr-only label stays the accessible name while hidden visually", () => {
  render(<>
    <Switch label="Keep cards" labelVisibility="sr-only" />
    <Switch label="Shown" />
  </>);
  assert.equal(screen.getByText("Keep cards").classList.contains("sr-only"), true);
  assert.equal(screen.getByText("Shown").classList.contains("sr-only"), false);
  assert.equal(toggle().getAttribute("aria-checked"), "false");
});

test("the switch submits value when on and uncheckedValue when off through its hidden input", async () => {
  const inputRef = createRef<HTMLInputElement>();
  render(<>
    <form data-testid="form" />
    <Switch label="Keep cards" name="keep" form="f" value="yes" uncheckedValue="no" inputRef={inputRef} required id="keep-switch" />
  </>);
  const form = screen.getByTestId("form") as HTMLFormElement;
  form.id = "f";
  assert.equal(inputRef.current instanceof dom.window.HTMLInputElement, true);
  assert.equal(inputRef.current!.name, "keep");
  assert.equal(inputRef.current!.required, true);
  // Base UI puts the id on the hidden input so an external <label htmlFor> targets it.
  assert.equal(inputRef.current!.id, "keep-switch");
  assert.equal(new FormData(form).get("keep"), "no");
  await userEvent.setup().click(toggle());
  assert.equal(new FormData(form).get("keep"), "yes");
});

test("a mouse hover widens the thumb and recolours the track; touch does not", async () => {
  render(<Switch label="Keep cards" />);
  const root = toggle().parentElement!;
  assert.equal(toggle().style.backgroundColor, "var(--secondary-action)");
  fireEvent.pointerEnter(root, { pointerType: "touch" });
  assert.equal(toggle().style.backgroundColor, "var(--secondary-action)");
  fireEvent.pointerEnter(root, { pointerType: "mouse" });
  assert.equal(toggle().style.backgroundColor, "var(--secondary-action-hover)");
  fireEvent.pointerLeave(root, { pointerType: "mouse" });
  assert.equal(toggle().style.backgroundColor, "var(--secondary-action)");
});

test("consumer pointer and click handlers run first and can cancel the switch's own handling", async () => {
  captured.length = 0;
  const seen: string[] = [];
  const cancel = (name: string) => (e: React.SyntheticEvent) => { seen.push(name); e.preventDefault(); };
  render(<Switch label="Keep cards" defaultChecked onClick={cancel("click")} onPointerEnter={cancel("enter")} onPointerLeave={cancel("leave")}
    onPointerDown={cancel("down")} onPointerMove={cancel("move")} onPointerUp={cancel("up")} onPointerCancel={cancel("cancel")} />);
  const root = toggle().parentElement!;
  fireEvent.pointerEnter(root, { pointerType: "mouse" });
  assert.equal(toggle().style.backgroundColor, "var(--brand)");
  fireEvent.pointerDown(root, { pointerId: 1, clientX: 20 });
  fireEvent.pointerMove(root, { pointerId: 1, clientX: 0 });
  fireEvent.pointerUp(root, { pointerId: 1 });
  fireEvent.pointerCancel(root, { pointerId: 1 });
  fireEvent.pointerLeave(root, { pointerType: "mouse" });
  fireEvent.click(screen.getByText("Keep cards"));
  assert.equal(checkedOf(), "true");
  assert.deepEqual(seen, ["enter", "down", "move", "up", "cancel", "leave", "click"]);
  assert.deepEqual(captured.length, 0);
});

// ── Pointer drag of the thumb ───────────────────────────
// Off the thumb rests at x=2; dragging is clamped to [2, 12] and the midpoint is 7.

const drag = (root: HTMLElement, moves: number[], end: "up" | "cancel" = "up") => {
  fireEvent.pointerDown(root, { pointerId: 7, clientX: 100, pointerType: "mouse", button: 0 });
  for (const dx of moves) fireEvent.pointerMove(root, { pointerId: 7, clientX: 100 + dx });
  if (end === "up") fireEvent.pointerUp(root, { pointerId: 7 });
  else fireEvent.pointerCancel(root, { pointerId: 7 });
  // The browser follows pointerup with a click on the same element.
  if (end === "up") fireEvent.click(root);
};

test("dragging the thumb past the midpoint turns the switch on exactly once", async () => {
  captured.length = 0;
  const calls: boolean[] = [];
  render(<Switch label="Keep cards" onCheckedChange={(c) => calls.push(c)} />);
  drag(toggle().parentElement!, [3, 8]);
  assert.equal(checkedOf(), "true");
  assert.deepEqual(calls, [true]);
  assert.deepEqual(captured, [7]);
  // The drag guard clears on the next frame; a later click toggles again.
  await waitFor(() => assert.equal(pendingFrameCount(), 0));
  await act(async () => { await new Promise((r) => setTimeout(r, 40)); });
  fireEvent.click(screen.getByText("Keep cards"));
  assert.equal(checkedOf(), "false");
});

test("dragging an on switch back past the midpoint turns it off", async () => {
  const calls: boolean[] = [];
  render(<Switch label="Keep cards" defaultChecked onCheckedChange={(c) => calls.push(c)} />);
  drag(toggle().parentElement!, [-3, -30]);
  assert.equal(checkedOf(), "false");
  assert.deepEqual(calls, [false]);
});

test("a short drag that stays on the same side snaps back without toggling, even for the follow-up click", async () => {
  const calls: boolean[] = [];
  render(<Switch label="Keep cards" onCheckedChange={(c) => calls.push(c)} />);
  drag(toggle().parentElement!, [4]);
  assert.equal(checkedOf(), "false");
  assert.deepEqual(calls, []);
});

test("movement inside the dead zone is a click, not a drag", async () => {
  const calls: boolean[] = [];
  render(<Switch label="Keep cards" onCheckedChange={(c) => calls.push(c)} />);
  drag(toggle().parentElement!, [1, -1]);
  assert.equal(checkedOf(), "true");
  assert.deepEqual(calls, [true]);
});

test("a cancelled drag snaps back and leaves the state alone", async () => {
  const calls: boolean[] = [];
  render(<Switch label="Keep cards" onCheckedChange={(c) => calls.push(c)} />);
  const root = toggle().parentElement!;
  drag(root, [3, 30], "cancel");
  drag(root, [], "cancel");
  assert.equal(checkedOf(), "false");
  assert.deepEqual(calls, []);
});

test("secondary mouse buttons and stray moves or ups without a press do nothing", async () => {
  captured.length = 0;
  const calls: boolean[] = [];
  render(<Switch label="Keep cards" onCheckedChange={(c) => calls.push(c)} />);
  const root = toggle().parentElement!;
  fireEvent.pointerMove(root, { pointerId: 2, clientX: 50 });
  fireEvent.pointerUp(root, { pointerId: 2 });
  fireEvent.pointerDown(root, { pointerId: 2, clientX: 0, pointerType: "mouse", button: 2 });
  fireEvent.pointerMove(root, { pointerId: 2, clientX: 30 });
  fireEvent.pointerUp(root, { pointerId: 2 });
  assert.equal(checkedOf(), "false");
  assert.deepEqual(calls, []);
  assert.deepEqual(captured, []);
});

test("an on switch dragged a little, or cancelled mid-drag, snaps back on", async () => {
  const calls: boolean[] = [];
  render(<Switch label="Keep cards" defaultChecked onCheckedChange={(c) => calls.push(c)} />);
  const root = toggle().parentElement!;
  drag(root, [-3]);
  drag(root, [-3, -30], "cancel");
  assert.equal(checkedOf(), "true");
  assert.deepEqual(calls, []);
});

test("a click on the hidden checkbox toggles once, through the primitive only", async () => {
  const inputRef = createRef<HTMLInputElement>();
  const calls: boolean[] = [];
  render(<Switch label="Keep cards" inputRef={inputRef} onCheckedChange={(c) => calls.push(c)} />);
  fireEvent.click(inputRef.current!);
  assert.equal(checkedOf(), "true");
  assert.deepEqual(calls, [true]);
});
