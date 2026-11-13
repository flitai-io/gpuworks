// Zeron Select (ui/src/components/ui/select.tsx), a Base UI select with
// proximity-hover overlays and an acknowledged close. The app uses it through
// OptionSelect (ui/src/ui-kit/OptionSelect.tsx) and the rows-per-page control of
// DataTablePagination.
import { dom, pendingFrameCount } from "./helpers/dom.ts";
import assert from "node:assert/strict";
import { afterEach, beforeEach, test as nodeTest } from "node:test";
import React, { useState } from "react";

for (const name of ["KeyboardEvent", "Document", "AbortController", "AbortSignal"] as const) {
  Object.defineProperty(globalThis, name, { value: dom.window[name], configurable: true });
}
// jsdom has no Web Animations API; Base UI asks which animations are running before unmounting a popup.
if (!("getAnimations" in dom.window.Element.prototype)) {
  Object.defineProperty(dom.window.Element.prototype, "getAnimations", { configurable: true, value: () => [] });
}
// jsdom has no layout: options (data-proximity-index) get a 32px box stacked by index.
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
const { Select, SelectTrigger, SelectContent, SelectItem, SelectGroup, SelectLabel, SelectSeparator } = await import("../src/components/ui/select.tsx");
// helpers/dom.ts drops a finished test's pending animation frames; Base UI's
// process-wide frame scheduler would then wait forever for the dropped one.
const { resetAnimationFrameScheduler } = await import("@base-ui/utils/useAnimationFrame");
beforeEach(() => resetAnimationFrameScheduler());
afterEach(() => { cleanup(); resetAnimationFrameScheduler(); });
// framer-motion's frame loop is process-wide too: let each test's animations finish.
const test = (name: string, fn: () => unknown) => nodeTest(name, async () => {
  try { await fn(); } finally { await waitFor(() => assert.equal(pendingFrameCount(), 0)); }
});

const frame = (ms = 40) => new Promise((resolve) => setTimeout(resolve, ms));
const trigger = (name = "Engine") => screen.getByRole("combobox", { name });
const isOpen = () => screen.queryByRole("listbox") !== null;

function Engine(props: Partial<React.ComponentProps<typeof Select>> & { triggerProps?: Partial<React.ComponentProps<typeof SelectTrigger>>; contentProps?: Partial<React.ComponentProps<typeof SelectContent>> }) {
  const { triggerProps, contentProps, ...root } = props;
  return <Select {...root}>
    <SelectTrigger aria-label="Engine" {...triggerProps} />
    <SelectContent {...contentProps}>
      <SelectItem value="vllm">vLLM</SelectItem>
      <SelectItem value="sglang">SGLang</SelectItem>
      <SelectItem value="llama" disabled>llama.cpp</SelectItem>
    </SelectContent>
  </Select>;
}

test("the trigger shows the placeholder until a value is chosen, then the chosen label", async () => {
  const changes: string[] = [];
  render(<Engine onValueChange={(value) => changes.push(value)} triggerProps={{ placeholder: "Pick an engine" }} />);
  assert.equal(trigger().textContent, "Pick an engine");
  const user = userEvent.setup();
  await user.click(trigger());
  await user.click(await screen.findByRole("option", { name: "SGLang" }));
  assert.deepEqual(changes, ["sglang"]);
  await waitFor(() => assert.equal(trigger().textContent, "SGLang"));
});

test("picking an option holds the popup open briefly to acknowledge the choice, then closes", async () => {
  render(<Engine />);
  const user = userEvent.setup();
  await user.click(trigger());
  await user.click(await screen.findByRole("option", { name: "vLLM" }));
  assert.ok(isOpen(), "the popup stays open during the acknowledgment");
  await waitFor(() => assert.equal(screen.getByRole("option", { name: "vLLM" }).getAttribute("aria-selected"), "true"));
  await waitFor(() => assert.equal(trigger().getAttribute("aria-expanded"), "false"));
});

test("Escape closes the popup immediately without changing the value", async () => {
  let changes = 0;
  render(<Engine defaultValue="vllm" onValueChange={() => { changes += 1; }} />);
  assert.equal(trigger().textContent, "vLLM", "the initial label resolves before the popup ever mounts");
  const user = userEvent.setup();
  await user.click(trigger());
  await screen.findByRole("listbox");
  await user.keyboard("{Escape}");
  await waitFor(() => assert.equal(trigger().getAttribute("aria-expanded"), "false"));
  assert.equal(changes, 0);
  assert.equal(trigger().textContent, "vLLM");
});

test("keyboard opens the list, moves the highlight and selects with Enter", async () => {
  function Controlled() {
    const [value, setValue] = useState("");
    return <Engine value={value} onValueChange={setValue} />;
  }
  render(<Controlled />);
  const user = userEvent.setup();
  trigger().focus();
  await user.keyboard("{ArrowDown}");
  await waitFor(() => assert.ok(document.activeElement === screen.getByRole("option", { name: "vLLM" })));
  await user.keyboard("{ArrowDown}");
  await waitFor(() => assert.ok(document.activeElement === screen.getByRole("option", { name: "SGLang" })));
  await user.keyboard("{Enter}");
  await waitFor(() => assert.equal(trigger().textContent, "SGLang"));
});

test("a disabled option cannot be chosen by pointer or keyboard, while enabled ones can", async () => {
  const changes: string[] = [];
  render(<Engine onValueChange={(value) => changes.push(value)} />);
  const user = userEvent.setup();
  await user.click(trigger());
  const disabled = await screen.findByRole("option", { name: "llama.cpp" });
  assert.equal(disabled.getAttribute("aria-disabled"), "true");
  await user.click(disabled);
  await frame(50);
  assert.deepEqual(changes, [], "a real click on the disabled option is ignored");
  assert.ok(isOpen(), "the popup stays open");
  // Keyboard: Base UI keeps disabled items highlightable (focusableWhenDisabled), but Enter on one commits nothing.
  await user.hover(screen.getByRole("option", { name: "SGLang" }));
  await waitFor(() => assert.ok(document.activeElement === screen.getByRole("option", { name: "SGLang" })));
  await user.keyboard("{ArrowDown}");
  await waitFor(() => assert.ok(document.activeElement === screen.getByRole("option", { name: "llama.cpp" })));
  await user.keyboard("{Enter}");
  await frame(50);
  assert.deepEqual(changes, [], "Enter on the highlighted disabled option is ignored");
  assert.ok(isOpen(), "the popup is still open");
  // Positive control: the same kind of click on an enabled option selects it.
  await user.click(screen.getByRole("option", { name: "vLLM" }));
  assert.deepEqual(changes, ["vllm"]);
});

test("a disabled select does not open", async () => {
  render(<Engine disabled />);
  assert.equal((trigger() as HTMLButtonElement).disabled, true);
  await userEvent.setup().click(trigger()).catch(() => {});
  assert.equal(isOpen(), false);
});

test("controlled open state follows the caller and reports requested changes", async () => {
  const requests: boolean[] = [];
  function Controlled() {
    const [open, setOpen] = useState(true);
    return <><button onClick={() => setOpen(false)}>Close it</button><Engine open={open} onOpenChange={(next) => { requests.push(next); }} /></>;
  }
  render(<Controlled />);
  await screen.findByRole("listbox");
  const user = userEvent.setup();
  await user.keyboard("{Escape}");
  assert.deepEqual(requests, [false]);
  assert.ok(isOpen(), "the caller owns open; the popup stays until it says otherwise");
  await user.click(screen.getByRole("button", { name: "Close it" }));
  await waitFor(() => assert.equal(trigger().getAttribute("aria-expanded"), "false"));
});

test("defaultOpen opens the list on mount, and re-picking during the acknowledgment restarts it", async () => {
  // The acknowledgment window (selectionAckMs, 300ms in select.tsx) is shorter than
  // two clicks can take on a loaded CI runner, so this test stretches that one
  // timer to 5s: the second pick then always lands inside the window.
  // select.tsx starts that timer with window.setTimeout but cancels it with the
  // bare clearTimeout. In a browser they are one API; under jsdom the bare one is
  // Node's and ignores jsdom's numeric ids, so route those to jsdom here.
  const realSetTimeout = dom.window.setTimeout;
  const realClearTimeout = globalThis.clearTimeout;
  dom.window.setTimeout = ((handler: TimerHandler, ms?: number, ...args: unknown[]) =>
    realSetTimeout(handler, ms === 300 ? 5000 : ms, ...args)) as typeof dom.window.setTimeout;
  globalThis.clearTimeout = ((id?: Parameters<typeof clearTimeout>[0]) =>
    typeof id === "number" ? dom.window.clearTimeout(id) : realClearTimeout(id)) as typeof clearTimeout;
  try {
    const opens: boolean[] = [];
    render(<Engine defaultOpen onOpenChange={(next) => opens.push(next)} />);
    await screen.findByRole("listbox");
    const user = userEvent.setup();
    await user.click(screen.getByRole("option", { name: "vLLM" }));
    await user.click(screen.getByRole("option", { name: "SGLang" }));
    await waitFor(() => assert.equal(trigger().textContent, "SGLang"));
    assert.deepEqual(opens, [], "still acknowledging: the second pick restarted the window instead of closing");
    await waitFor(() => assert.deepEqual(opens, [false]), { timeout: 8000 });
    await new Promise((resolve) => setTimeout(resolve, 500));
    assert.deepEqual(opens, [false], "one close after the last acknowledgment, not one per pick");
  } finally {
    dom.window.setTimeout = realSetTimeout;
    globalThis.clearTimeout = realClearTimeout;
  }
});

test("an error marks the trigger invalid and shows its message", () => {
  render(<Engine triggerProps={{ error: "Choose an engine" }} />);
  assert.equal(trigger().getAttribute("aria-invalid"), "true");
  assert.ok(screen.getByText("Choose an engine"));
});

test("the trigger renders a leading icon and prefix, and sizes follow the select size", () => {
  const Icon = (props: { size?: number }) => <svg data-testid="engine-icon" data-size={props.size} />;
  render(<Engine size="sm" triggerProps={{ icon: Icon, prefix: "Engine:", contentSized: true, variant: "borderless" }} />);
  assert.equal(screen.getByTestId("engine-icon").getAttribute("data-size"), "14", "a small select uses the 14px control icon");
  assert.ok(screen.getByText("Engine:"));
  assert.equal(trigger().getAttribute("data-size"), "sm");
  assert.equal(trigger().getAttribute("data-variant"), "borderless");
});

test("a rich option label is shown in the closed trigger, and textValue drives typeahead", async () => {
  render(<Select defaultValue="b">
    <SelectTrigger aria-label="Model" />
    <SelectContent>
      <SelectGroup aria-label="Families">
        <SelectLabel>Families</SelectLabel>
        <SelectItem value="a" textValue="Alpha"><strong>Alpha</strong> 7B</SelectItem>
        <SelectSeparator />
        <SelectItem value="b" label="Beta (label)"><em>Beta</em></SelectItem>
      </SelectGroup>
    </SelectContent>
  </Select>);
  assert.equal(trigger("Model").textContent, "Beta (label)", "label wins over children for nested items");
  const user = userEvent.setup();
  await user.click(trigger("Model"));
  await screen.findByRole("listbox");
  assert.ok(screen.getByRole("group", { name: "Families" }));
  assert.ok(screen.getByRole("separator"));
  await user.keyboard("a");
  await waitFor(() => assert.equal(document.activeElement?.getAttribute("data-value"), "a"));
});

test("an item outside a Select throws a descriptive error", () => {
  const original = console.error;
  console.error = () => {};
  try {
    assert.throws(() => render(<SelectTrigger />), /must be inside <Select>/);
  } finally {
    console.error = original;
  }
});

test("hovering options highlights the nearest one and leaving clears it", async () => {
  render(<Engine defaultOpen defaultValue="vllm" />);
  const list = await screen.findByRole("listbox");
  await frame(80);
  const hover = () => Array.from(list.querySelectorAll<HTMLElement>(".bg-hover"));
  assert.equal(list.querySelectorAll(".bg-selection").length, 1, "the selected option has a background");
  fireEvent.mouseEnter(list);
  fireEvent.mouseMove(list, { clientX: 5, clientY: 40 });
  await waitFor(() => assert.deepEqual(hover().map((el) => el.style.top), ["32px"]), "the highlight moves to the second option");
  fireEvent.mouseLeave(list);
  await waitFor(() => assert.equal(hover().length, 0));
});

test("caller popup handlers run first and can opt out of the built-in hover and focus tracking", async () => {
  const calls: string[] = [];
  const prevent = (name: string) => (event: React.SyntheticEvent) => { calls.push(name); event.preventDefault(); };
  render(<Engine defaultOpen contentProps={{ animated: false, onMouseEnter: prevent("enter"), onMouseMove: prevent("move"), onMouseLeave: prevent("leave"), onFocus: prevent("focus"), onBlur: prevent("blur") }} />);
  const list = await screen.findByRole("listbox");
  await frame(80);
  fireEvent.mouseEnter(list);
  fireEvent.mouseMove(list, { clientX: 5, clientY: 40 });
  await frame(60);
  assert.equal(list.querySelectorAll(".bg-hover").length, 0, "a prevented move does not highlight");
  fireEvent.mouseLeave(list);
  calls.splice(0, calls.indexOf("enter"));
  assert.deepEqual(calls, ["enter", "move", "leave"]);
  const option = screen.getByRole("option", { name: "SGLang" });
  fireEvent.focus(option);
  await frame(60);
  assert.equal(list.querySelectorAll(".bg-hover").length, 0, "a prevented focus does not highlight");
  fireEvent.blur(option);
  assert.ok(calls.includes("focus") && calls.includes("blur"));
});

test("focus moving to an option highlights it; focus leaving the list clears it", async () => {
  render(<><Engine defaultOpen /><button>Elsewhere</button></>);
  const list = await screen.findByRole("listbox");
  await frame(80);
  const option = screen.getByRole("option", { name: "SGLang" });
  act(() => { option.focus(); });
  await waitFor(() => assert.equal(list.querySelectorAll(".bg-hover").length, 1));
  fireEvent.focus(screen.getByRole("option", { name: "vLLM" }), { relatedTarget: option });
  fireEvent.blur(option, { relatedTarget: screen.getByRole("option", { name: "vLLM" }) });
  assert.equal(list.querySelectorAll(".bg-hover").length, 1, "moving within the list keeps the highlight");
  fireEvent.blur(screen.getByRole("option", { name: "vLLM" }), { relatedTarget: screen.getByRole("button", { name: "Elsewhere" }) });
  await waitFor(() => assert.equal(list.querySelectorAll(".bg-hover").length, 0));
});

test("item density changes the option row size and its check icon", async () => {
  render(<Engine defaultOpen defaultValue="vllm" itemDensity="compact" />);
  const option = await screen.findByRole("option", { name: "vLLM" });
  assert.ok(option.classList.contains("h-control-md"));
  const check = option.querySelector("svg");
  assert.equal(check?.getAttribute("width"), "14");
});

test("item refs receive the option element", async () => {
  const ref = React.createRef<HTMLDivElement>();
  let fnNode: HTMLDivElement | null = null;
  render(<Select defaultOpen><SelectTrigger aria-label="Engine" /><SelectContent>
    <SelectItem ref={ref} value="a">A</SelectItem>
    <SelectItem ref={(node) => { fnNode = node; }} value="b">B</SelectItem>
  </SelectContent></Select>);
  await screen.findByRole("listbox");
  assert.equal(ref.current?.getAttribute("data-value"), "a");
  assert.equal((fnNode as HTMLDivElement | null)?.getAttribute("data-value"), "b");
});

test("the hidden form input carries the name and value", () => {
  const { container } = render(<form><Engine name="engine" defaultValue="sglang" required /></form>);
  const input = container.querySelector("input[name=engine]") as HTMLInputElement;
  assert.equal(input.value, "sglang");
});
