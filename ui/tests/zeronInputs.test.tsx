// Zeron input primitives: the compound InputGroup (as PasswordInput composes
// it), the legacy InputFieldGroup/InputField, Input, Checkbox and Button.
import { dom } from "./helpers/dom.ts";
import assert from "node:assert/strict";
import { after, afterEach, before, test } from "node:test";
import React, { useState } from "react";

for (const name of ["KeyboardEvent", "MouseEvent", "PointerEvent", "AbortController", "AbortSignal"] as const) {
  if (dom.window[name]) Object.defineProperty(globalThis, name, { value: dom.window[name], configurable: true });
}
if (!("getAnimations" in dom.window.Element.prototype)) {
  Object.defineProperty(dom.window.Element.prototype, "getAnimations", { configurable: true, value: () => [] });
}
const { render, screen, fireEvent, waitFor, cleanup, act } = await import("@testing-library/react");
const { default: userEvent } = await import("@testing-library/user-event");
const { InputGroup, InputGroupAddon, InputGroupButton, InputGroupText, InputGroupInput, InputGroupTextarea, InputFieldGroup, InputField } =
  await import("../src/components/ui/input-group.tsx");
const { Input } = await import("../src/components/ui/input.tsx");
const { Checkbox } = await import("../src/components/ui/checkbox.tsx");
const { Button } = await import("../src/components/ui/button.tsx");
afterEach(cleanup);

/** An icon that exposes the props the component hands it. */
const ProbeIcon = ({ size, strokeWidth, ...rest }: { size?: number; strokeWidth?: number; [key: string]: unknown }) =>
  <svg data-testid={String(rest["data-slot"] ?? "probe-icon")} data-size={size} data-stroke={strokeWidth} />;

// ── Compound InputGroup ──────────────────────────────────

test("an input group composed like the password field toggles visibility from its addon button", async () => {
  function Password() {
    const [shown, setShown] = useState(false);
    return (
      <InputGroup>
        <InputGroupInput aria-label="Password" type={shown ? "text" : "password"} defaultValue="secret" />
        <InputGroupAddon align="inline-end">
          <InputGroupButton iconOnly aria-label={shown ? "Hide password" : "Show password"} aria-pressed={shown} onClick={() => setShown(!shown)}>
            <ProbeIcon />
          </InputGroupButton>
        </InputGroupAddon>
      </InputGroup>
    );
  }
  render(<Password />);
  const user = userEvent.setup();
  const input = screen.getByLabelText("Password") as HTMLInputElement;
  assert.equal(input.type, "password");
  const toggle = screen.getByRole("button", { name: "Show password" }) as HTMLButtonElement;
  assert.equal(toggle.type, "button");
  await user.click(toggle);
  assert.equal(input.type, "text");
  assert.equal(screen.getByRole("button", { name: "Hide password" }).getAttribute("aria-pressed"), "true");
  // Clicking the button itself must not steal focus back to the input.
  assert.notEqual(document.activeElement, input);
});

test("clicking an addon focuses the group's control unless the click was handled", async () => {
  let clicks = 0;
  render(
    <>
      <InputGroup aria-label="Search group">
        <InputGroupAddon data-testid="prefix"><InputGroupText>https://</InputGroupText></InputGroupAddon>
        <InputGroupInput aria-label="Host" />
      </InputGroup>
      <InputGroup>
        <InputGroupAddon data-testid="handled" onClick={(event) => { clicks++; event.preventDefault(); }}>@</InputGroupAddon>
        <InputGroupInput aria-label="User" />
      </InputGroup>
    </>,
  );
  fireEvent.click(screen.getByText("https://"));
  assert.equal(document.activeElement, screen.getByLabelText("Host"));
  fireEvent.click(screen.getByTestId("handled"));
  assert.equal(clicks, 1);
  assert.equal(document.activeElement, screen.getByLabelText("Host"));
  assert.equal(screen.getByRole("group", { name: "Search group" }).getAttribute("data-size"), "md");
  assert.equal(screen.getByTestId("prefix").getAttribute("data-align"), "inline-start");
});

test("a textarea addon at the block edge receives focus from its caption", () => {
  render(
    <InputGroup size="lg">
      <InputGroupTextarea aria-label="Prompt" />
      <InputGroupAddon align="block-end"><InputGroupText>Shift+Enter for a new line</InputGroupText></InputGroupAddon>
    </InputGroup>,
  );
  fireEvent.click(screen.getByText("Shift+Enter for a new line"));
  assert.equal(document.activeElement, screen.getByLabelText("Prompt"));
});

test("the group size flows to its controls and buttons, and defaults to medium outside a group", () => {
  render(
    <>
      <InputGroup size="sm">
        <InputGroupInput aria-label="Small" />
        <InputGroupAddon align="inline-end"><InputGroupButton>Go</InputGroupButton></InputGroupAddon>
      </InputGroup>
      <InputGroupInput aria-label="Standalone" />
    </>,
  );
  assert.equal(screen.getByLabelText("Small").getAttribute("data-size"), "sm");
  assert.equal(screen.getByRole("button", { name: "Go" }).getAttribute("data-size"), "sm");
  assert.equal(screen.getByLabelText("Standalone").getAttribute("data-size"), "md");
});

// ── Legacy InputFieldGroup / InputField ──────────────────

// The proximity hover measures rows with offset* (40px rows, 60px apart);
// jsdom has no layout, so the geometry is stubbed for these tests only.
const offsetProps = ["offsetHeight", "offsetWidth", "offsetTop"] as const;
const savedOffsets = offsetProps.map((name) => Object.getOwnPropertyDescriptor(dom.window.HTMLElement.prototype, name));
before(() => {
  Object.defineProperty(dom.window.HTMLElement.prototype, "offsetHeight", { configurable: true, get() { return 40; } });
  Object.defineProperty(dom.window.HTMLElement.prototype, "offsetWidth", { configurable: true, get() { return 280; } });
  Object.defineProperty(dom.window.HTMLElement.prototype, "offsetTop", {
    configurable: true,
    get(this: HTMLElement) { return this.parentElement ? Array.prototype.indexOf.call(this.parentElement.children, this) * 60 : 0; },
  });
});
after(() => {
  offsetProps.forEach((name, i) => { if (savedOffsets[i]) Object.defineProperty(dom.window.HTMLElement.prototype, name, savedOffsets[i]!); });
});

function Fields({ error, disabled }: { error?: string; disabled?: boolean }) {
  const [name, setName] = useState("");
  const [mail, setMail] = useState("");
  return (
    <InputFieldGroup data-testid="fields">
      <InputField index={0} label="Name" placeholder="Your name" icon={ProbeIcon as never} value={name} onChange={setName} disabled={disabled} />
      <InputField index={1} label="Email" icon={ProbeIcon as never} value={mail} onChange={setMail} error={error} data-testid="mail" />
    </InputFieldGroup>
  );
}

test("input fields are labelled controls that report typed values", async () => {
  render(<Fields />);
  const user = userEvent.setup();
  const name = screen.getByLabelText("Name") as HTMLInputElement;
  assert.equal(name.placeholder, "Your name");
  await user.type(name, "Ada");
  assert.equal(name.value, "Ada");
  await user.type(screen.getByLabelText("Email"), "a@b.c");
  assert.equal((screen.getByLabelText("Email") as HTMLInputElement).value, "a@b.c");
});

test("focusing a field emphasises its icon and clicking the frame focuses the input", () => {
  render(<Fields />);
  const [nameIcon] = screen.getAllByTestId("probe-icon");
  assert.equal(nameIcon.getAttribute("data-stroke"), "1.5");
  const name = screen.getByLabelText("Name");
  fireEvent.mouseDown(nameIcon);
  assert.equal(document.activeElement, name);
  fireEvent.focus(name);
  assert.equal(nameIcon.getAttribute("data-stroke"), "2");
  // A mousedown on the input itself keeps its default caret handling.
  const event = new dom.window.MouseEvent("mousedown", { bubbles: true, cancelable: true });
  name.dispatchEvent(event);
  assert.equal(event.defaultPrevented, false);
  fireEvent.blur(name);
  assert.equal(nameIcon.getAttribute("data-stroke"), "1.5");
});

test("hovering near a field highlights the nearest one", async () => {
  render(<Fields />);
  const icons = screen.getAllByTestId("probe-icon");
  const group = screen.getByTestId("fields");
  // Same visual size as the stubbed layout size, so the hook sees no scale transform.
  group.getBoundingClientRect = () => ({ top: 0, left: 0, width: 280, height: 40, right: 280, bottom: 40, x: 0, y: 0, toJSON() {} }) as DOMRect;
  // Let the registration pass measure the rows first.
  await new Promise((resolve) => setTimeout(resolve, 50));
  fireEvent.mouseEnter(group);
  fireEvent.mouseMove(group, { clientX: 10, clientY: 75 });
  await waitFor(() => assert.equal(icons[1].getAttribute("data-stroke"), "2"));
  assert.equal(icons[0].getAttribute("data-stroke"), "1.5");
  fireEvent.mouseLeave(group);
  await waitFor(() => assert.equal(icons[1].getAttribute("data-stroke"), "1.5"));
});

test("an errored field is invalid and describes its error; a disabled field is disabled", async () => {
  render(<Fields error="Enter a valid address" disabled />);
  const mail = screen.getByLabelText("Email");
  assert.equal(mail.getAttribute("aria-invalid"), "true");
  const message = screen.getByText("Enter a valid address");
  await waitFor(() => assert.ok((mail.getAttribute("aria-describedby") ?? "").split(" ").includes(message.id)));
  assert.equal((screen.getByLabelText("Name") as HTMLInputElement).disabled, true);
  // Focus and hover on the errored field keep it usable.
  fireEvent.focus(mail);
  assert.equal(screen.getAllByTestId("probe-icon")[1].getAttribute("data-stroke"), "2");
});

test("an input field outside an input field group fails loudly", () => {
  const original = console.error;
  console.error = () => {};
  try {
    assert.throws(() => render(<InputField index={0} label="Orphan" value="" onChange={() => {}} />), /must be used within an InputFieldGroup/);
  } finally {
    console.error = original;
  }
});

test("an input field group forwards function and object refs to its container", () => {
  let fnNode: HTMLDivElement | null = null;
  const objRef = React.createRef<HTMLDivElement>();
  const fieldRef = React.createRef<HTMLDivElement>();
  let fieldFnNode: HTMLDivElement | null = null;
  render(
    <>
      <InputFieldGroup ref={(node) => { fnNode = node; }} data-testid="a"><InputField ref={fieldRef} index={0} label="A" value="" onChange={() => {}} /></InputFieldGroup>
      <InputFieldGroup ref={objRef} data-testid="b"><InputField ref={(node) => { fieldFnNode = node; }} index={0} label="B" value="" onChange={() => {}} /></InputFieldGroup>
    </>,
  );
  assert.equal(fnNode, screen.getByTestId("a"));
  assert.equal(objRef.current, screen.getByTestId("b"));
  assert.ok(fieldRef.current?.contains(screen.getByLabelText("A")));
  assert.ok((fieldFnNode as HTMLDivElement | null)?.contains(screen.getByLabelText("B")));
});

// ── Input ────────────────────────────────────────────────

test("input accepts typing, reports its size and variant, and honours disabled", async () => {
  let value = "";
  render(
    <>
      <Input aria-label="Filter" onChange={(e) => { value = e.target.value; }} />
      <Input aria-label="Ghost" size="xs" variant="ghost" disabled />
      <Input aria-label="Defaulted" size={null} />
      <Input aria-label="Archive" type="file" size="lg" />
    </>,
  );
  const user = userEvent.setup();
  await user.type(screen.getByLabelText("Filter"), "gpu");
  assert.equal(value, "gpu");
  const filter = screen.getByLabelText("Filter");
  assert.equal(filter.getAttribute("data-size"), "md");
  assert.equal(filter.getAttribute("data-variant"), "outline");
  const ghost = screen.getByLabelText("Ghost") as HTMLInputElement;
  assert.equal(ghost.disabled, true);
  assert.equal(ghost.getAttribute("data-size"), "xs");
  assert.equal(ghost.getAttribute("data-variant"), "ghost");
  assert.equal(screen.getByLabelText("Defaulted").getAttribute("data-size"), "md");
  const file = screen.getByLabelText("Archive") as HTMLInputElement;
  assert.equal(file.type, "file");
  // A file input's line height tracks its control size so the picker label is centred.
  assert.ok(file.className.split(" ").includes("leading-8"));
  assert.ok(!filter.className.split(" ").includes("leading-7"));
});

// ── Checkbox ─────────────────────────────────────────────

test("checkbox toggles by click and Space and reports the new state", async () => {
  const changes: boolean[] = [];
  function Example() {
    const [checked, setChecked] = useState(false);
    return <label><Checkbox checked={checked} onCheckedChange={(v) => { changes.push(v); setChecked(v); }} />Allow host</label>;
  }
  render(<Example />);
  const user = userEvent.setup();
  const box = screen.getByRole("checkbox", { name: "Allow host" });
  assert.equal(box.getAttribute("aria-checked"), "false");
  await user.click(box);
  assert.equal(box.getAttribute("aria-checked"), "true");
  box.focus();
  await user.keyboard(" ");
  assert.equal(box.getAttribute("aria-checked"), "false");
  assert.deepEqual(changes, [true, false]);
});

test("checkbox shows a mixed state from either indeterminate form", () => {
  render(
    <>
      <Checkbox aria-label="Some models" checked="indeterminate" />
      <Checkbox aria-label="Some nodes" indeterminate />
      <Checkbox aria-label="All" defaultChecked />
    </>,
  );
  assert.equal(screen.getByRole("checkbox", { name: "Some models" }).getAttribute("aria-checked"), "mixed");
  assert.equal(screen.getByRole("checkbox", { name: "Some nodes" }).getAttribute("aria-checked"), "mixed");
  assert.equal(screen.getByRole("checkbox", { name: "All" }).getAttribute("aria-checked"), "true");
});

test("a disabled checkbox ignores clicks", async () => {
  let changed = false;
  render(<Checkbox aria-label="Locked" disabled onCheckedChange={() => { changed = true; }} />);
  const user = userEvent.setup();
  const box = screen.getByRole("checkbox", { name: "Locked" });
  await user.click(box);
  assert.equal(changed, false);
  assert.equal(box.getAttribute("aria-checked"), "false");
  assert.equal(box.getAttribute("aria-disabled"), "true");
});

// ── Button ───────────────────────────────────────────────

test("button activates by click and keyboard and stays inert while disabled", async () => {
  let clicks = 0;
  const { rerender } = render(<Button onClick={() => clicks++}>Deploy</Button>);
  const user = userEvent.setup();
  const button = screen.getByRole("button", { name: "Deploy" });
  await user.click(button);
  button.focus();
  await user.keyboard("{Enter}");
  await user.keyboard(" ");
  assert.equal(clicks, 3);
  rerender(<Button onClick={() => clicks++} disabled>Deploy</Button>);
  await user.click(screen.getByRole("button", { name: "Deploy" }));
  assert.equal(clicks, 3);
});

test("a loading button is disabled, keeps its accessible name and shows a spinner instead of icons", async () => {
  let clicks = 0;
  render(<Button loading leadingIcon={ProbeIcon as never} trailingIcon={ProbeIcon as never} onClick={() => clicks++}>Saving</Button>);
  const button = screen.getByRole("button", { name: "Saving" }) as HTMLButtonElement;
  assert.equal(button.disabled, true);
  await userEvent.setup().click(button);
  assert.equal(clicks, 0);
  // The label and icons are kept for width but hidden; the spinner is the visible content.
  assert.ok(button.querySelector("path[pathLength='100']"));
  assert.equal(screen.getByTestId("button-leading-icon").getAttribute("data-stroke"), "2");
  assert.ok(screen.getByTestId("button-trailing-icon"));
});

test("a loading icon-only button hides its icons and keeps only the spinner", () => {
  render(<Button loading iconOnly aria-label="Refresh" leadingIcon={ProbeIcon as never}><ProbeIcon /></Button>);
  const button = screen.getByRole("button", { name: "Refresh" });
  assert.equal(screen.queryByTestId("button-leading-icon") === null, true);
  assert.ok(button.querySelector("path[pathLength='100']"));
});

test("button renders leading and trailing icons at the control size and icon-only content without a label", () => {
  render(
    <>
      <Button size="sm" leadingIcon={ProbeIcon as never} trailingIcon={ProbeIcon as never}>Export</Button>
      <Button iconOnly size="xl" aria-label="Settings" leadingIcon={ProbeIcon as never}><ProbeIcon /></Button>
    </>,
  );
  const lead = screen.getByTestId("button-leading-icon");
  assert.equal(lead.getAttribute("data-stroke"), "1.5");
  assert.ok(Number(lead.getAttribute("data-size")) > 0);
  assert.ok(screen.getByTestId("button-trailing-icon"));
  const settings = screen.getByRole("button", { name: "Settings" });
  // Icon-only buttons render their child as the icon and ignore leading/trailing icons.
  assert.equal(settings.querySelector("[data-slot='button-label']") === null, true);
  assert.equal(settings.querySelector("[data-slot='button-leading-icon']") === null, true);
  assert.ok(settings.querySelector("[data-slot='button-icon'] svg"));
});

test("asChild renders the child element as the button root without button semantics", async () => {
  let clicks = 0;
  render(
    <Button asChild variant="link" tone="warning" style={{ color: "red" }} onClick={() => clicks++}>
      <a href="#docs" className="docs" style={{ fontWeight: 600 }}>Read the docs</a>
    </Button>,
  );
  const link = screen.getByRole("link", { name: "Read the docs" });
  assert.equal(screen.queryByRole("button") === null, true);
  assert.equal(link.getAttribute("data-slot"), "button");
  assert.equal(link.getAttribute("data-tone"), "warning");
  assert.ok(link.classList.contains("docs"));
  assert.equal((link as HTMLElement).style.color, "red");
  assert.equal((link as HTMLElement).style.fontWeight, "600");
  await userEvent.setup().click(link);
  assert.equal(clicks, 1);
});

test("asChild with a plain-text child falls back to a regular button", () => {
  render(<Button asChild>Plain</Button>);
  assert.ok(screen.getByRole("button", { name: "Plain" }));
});

test("a warning tone is exposed on the button and changes its surface for rest and held states", () => {
  render(
    <>
      <Button tone="warning">Rest</Button>
      <Button tone="warning" active>Held</Button>
      <Button variant="tertiary" active dashed>Filter</Button>
      <Button variant="secondary" contentSized contentClassName="content" labelClassName="label">Multi line</Button>
    </>,
  );
  const background = (name: string) => screen.getByRole("button", { name }).querySelector("[data-slot='button-background']") as HTMLElement;
  assert.equal(screen.getByRole("button", { name: "Rest" }).getAttribute("data-tone"), "warning");
  assert.equal(screen.getByRole("button", { name: "Filter" }).getAttribute("data-tone"), null);
  // The pressed look of a held (menu-open) button is its contract: it must differ from the resting surface.
  assert.notEqual(background("Rest").className, background("Held").className);
  assert.ok(background("Filter").classList.contains("border-dashed"));
  assert.ok(background("Filter").classList.contains("bg-active"));
  const multi = screen.getByRole("button", { name: "Multi line" });
  assert.ok(multi.querySelector("[data-slot='button-content'].content"));
  assert.ok(multi.querySelector("[data-slot='button-label'].label"));
});

test("button forwards its ref to the native button", () => {
  const ref = React.createRef<HTMLButtonElement>();
  render(<Button ref={ref}>Ref</Button>);
  assert.equal(ref.current, screen.getByRole("button", { name: "Ref" }));
  act(() => { ref.current!.focus(); });
  assert.equal(document.activeElement, ref.current);
});
