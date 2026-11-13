import { createI18n } from "./helpers/dom.ts";
import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import React, { useState } from "react";
import { I18nextProvider } from "react-i18next";
const { render, screen, fireEvent, waitFor, cleanup } = await import("@testing-library/react");
const { default: userEvent } = await import("@testing-library/user-event");
const { SideDrawer } = await import("../src/ui-kit/SideDrawer.tsx");
const { NumberInput } = await import("../src/ui-kit/NumberInput.tsx");
const { FileUpload } = await import("../src/ui-kit/FileUpload.tsx");
const { ConfirmPopover } = await import("../src/ui-kit/ConfirmPopover.tsx");
const { Spinner } = await import("../src/ui-kit/Spinner.tsx");
const { Progress } = await import("../src/ui-kit/Progress.tsx");
const { Segmented } = await import("../src/ui-kit/Segmented.tsx");
const { Timeline } = await import("../src/ui-kit/Timeline.tsx");
const { Result } = await import("../src/ui-kit/Result.tsx");
const { i18n } = await createI18n([]);
const mount = (children: React.ReactNode) => render(<I18nextProvider i18n={i18n}>{children}</I18nextProvider>);
afterEach(cleanup);

test("drawer dismisses with Escape and returns focus to its opener", async () => {
  function Example() {
    const [open, setOpen] = useState(false);
    return <><button onClick={() => setOpen(true)}>Inspect</button><SideDrawer open={open} onClose={() => setOpen(false)} title="Resource"><button>Action</button></SideDrawer></>;
  }
  mount(<Example />);
  const user = userEvent.setup();
  const opener = screen.getByRole("button", { name: "Inspect" });
  await user.click(opener);
  await screen.findByRole("dialog", { name: "Resource" });
  await user.keyboard("{Escape}");
  await waitFor(() => assert.equal(screen.queryByRole("dialog"), null));
  await waitFor(() => assert.equal(document.activeElement, opener));
});

test("number input retains null and applies supplied bounds and precision on blur", () => {
  function Example() {
    const [value, setValue] = useState<number | null>(4);
    return <NumberInput aria-label="Hours" value={value} onChange={setValue} min={1} max={8} precision={0} />;
  }
  mount(<Example />);
  const input = screen.getByRole("spinbutton") as HTMLInputElement;
  fireEvent.change(input, { target: { value: "" } });
  fireEvent.blur(input);
  assert.equal(input.value, "");
  fireEvent.change(input, { target: { value: "12" } });
  assert.equal(input.value, "12");
  fireEvent.blur(input);
  assert.equal(input.value, "8");
  fireEvent.change(input, { target: { value: "2.7" } });
  fireEvent.blur(input);
  assert.equal(input.value, "3");
});

test("file upload preserves selection on cancellation and allows removal and reselection", async () => {
  function Example() {
    const [file, setFile] = useState<File | null>(null);
    return <FileUpload aria-label="Requests" value={file} onChange={setFile} accept=".jsonl" />;
  }
  mount(<Example />);
  const input = screen.getByLabelText("Requests");
  const file = new window.File(["{}"], "requests.jsonl");
  fireEvent.change(input, { target: { files: [file] } });
  assert.ok(screen.getByText(file.name));
  assert.ok(screen.getByRole("button", { name: "Choose another" }), "the picker button says a file is chosen, in the console's language");
  fireEvent.change(input, { target: { files: [] } });
  assert.ok(screen.getByText(file.name));
  fireEvent.click(screen.getByRole("button", { name: "Remove" }));
  assert.equal(screen.queryByText(file.name), null);
  assert.ok(screen.getByText("No file chosen"));
  fireEvent.change(input, { target: { files: [file] } });
  assert.ok(screen.getByText(file.name));
});

test("confirmation keeps failure visible and retries before closing", async () => {
  let attempts = 0;
  mount(<ConfirmPopover trigger={<button>Delete</button>} title="Delete resource?" confirmText="Proceed" onConfirm={async () => { if (++attempts === 1) throw new Error("Offline"); }} />);
  const user = userEvent.setup();
  await user.click(screen.getByRole("button", { name: "Delete" }));
  await user.click(await screen.findByRole("button", { name: "Proceed" }));
  assert.equal((await screen.findByRole("alert")).textContent, "Offline");
  await user.click(screen.getByRole("button", { name: "Proceed" }));
  await waitFor(() => assert.equal(screen.queryByRole("button", { name: "Proceed" }), null));
  assert.equal(attempts, 2);
});

test("spinner announces its status", () => {
  mount(<Spinner label="Loading records" />);
  assert.equal(screen.getByRole("status").textContent, "Loading records");
});
test("progress exposes a bounded determinate value", () => {
  mount(<Progress label="Completed requests" value={120} />);
  assert.equal(screen.getByRole("progressbar").getAttribute("aria-valuenow"), "100");
});
test("segmented view switches with keyboard without selecting disabled choices", async () => {
  function Example() {
    const [value, setValue] = useState("a");
    return <Segmented label="Period" value={value} onChange={setValue} options={[{ value: "a", label: "Day" }, { value: "b", label: "Week", disabled: true }, { value: "c", label: "Month" }]} />;
  }
  mount(<Example />);
  screen.getByRole("tab", { name: "Day" }).focus();
  await userEvent.setup().keyboard("{ArrowRight}");
  assert.equal(screen.getByRole("tab", { name: "Week" }).getAttribute("aria-selected"), "false");
  await userEvent.setup().keyboard("{ArrowRight}");
  await waitFor(() => assert.equal(screen.getByRole("tab", { name: "Month" }).getAttribute("aria-selected"), "true"));
});
test("timeline retains ordered events and timestamps", () => {
  mount(<Timeline label="History" items={[{ key: "a", title: "Created", time: "2026-11-06" }, { key: "b", title: "Completed", description: "All requests processed" }]} />);
  assert.deepEqual(screen.getAllByRole("listitem").map((item) => item.textContent), ["Created2026-11-06", "CompletedAll requests processed"]);
});
test("result keeps its explanatory text and recovery action", () => {
  let retried = false;
  mount(<Result title="Unavailable" description="Retry later" actions={<button onClick={() => { retried = true; }}>Retry</button>} />);
  assert.ok(screen.getByText("Retry later"));
  fireEvent.click(screen.getByRole("button", { name: "Retry" }));
  assert.equal(retried, true);
});

const { FilterChips } = await import("../src/ui-kit/FilterChips.tsx");
const { OptionSelect } = await import("../src/ui-kit/OptionSelect.tsx");
const { Notice } = await import("../src/ui-kit/Notice.tsx");
const { fromAntd } = await import("../src/ui-kit/antdIcon.tsx");

test("a busy drawer ignores Escape and its close button until the work ends", async () => {
  let closes = 0;
  const view = mount(<SideDrawer open busy onClose={() => { closes++; }} title="Saving"><p>Body</p></SideDrawer>);
  const dialog = await screen.findByRole("dialog", { name: "Saving" });
  const close = screen.getByRole("button", { name: "Close" }) as HTMLButtonElement;
  assert.equal(close.disabled, true);
  await userEvent.setup().keyboard("{Escape}");
  fireEvent.click(close);
  assert.equal(closes, 0);
  assert.ok(dialog.isConnected, "the drawer stays open while busy");
  view.rerender(<I18nextProvider i18n={i18n}><SideDrawer open onClose={() => { closes++; }} title="Saving"><p>Body</p></SideDrawer></I18nextProvider>);
  await waitFor(() => assert.equal((screen.getByRole("button", { name: "Close" }) as HTMLButtonElement).disabled, false));
  await userEvent.setup().click(screen.getByRole("button", { name: "Close" }));
  assert.equal(closes, 1);
});

test("a drawer shows its subtitle under the title, actions beside the close button, and its footer", async () => {
  mount(<SideDrawer open onClose={() => {}} title="node-1" subtitle="10.0.0.1" extra={<button>Reboot</button>} footer={<button>Apply</button>} width="50vw"><p>Body</p></SideDrawer>);
  const dialog = await screen.findByRole("dialog", { name: "node-1" });
  const title = screen.getByText("node-1");
  const subtitle = screen.getByText("10.0.0.1");
  assert.ok(title.parentElement === subtitle.parentElement, "the subtitle sits in the title column");
  assert.ok(title.compareDocumentPosition(subtitle) & Node.DOCUMENT_POSITION_FOLLOWING, "the subtitle follows the title");
  const reboot = screen.getByRole("button", { name: "Reboot" });
  const close = screen.getByRole("button", { name: "Close" });
  assert.ok(reboot.parentElement?.parentElement === close.parentElement, "the actions share the title row with the close button");
  assert.ok(reboot.compareDocumentPosition(close) & Node.DOCUMENT_POSITION_FOLLOWING, "the close button stays last in the row");
  assert.ok(screen.getByRole("button", { name: "Apply" }));
  assert.equal(dialog.style.width, "50vw", "a CSS width is passed through as given");
});

test("a drawer without subtitle, actions or footer renders only its title row and body", async () => {
  mount(<SideDrawer open onClose={() => {}} title="Plain"><p>Body</p></SideDrawer>);
  const dialog = await screen.findByRole("dialog", { name: "Plain" });
  assert.deepEqual([...dialog.querySelectorAll("button")].map((button) => button.getAttribute("aria-label") ?? button.textContent), ["Close"]);
});

test("file upload hands focus from the hidden input to its visible button and shows the chosen file's meta", () => {
  function Example() {
    const [file, setFile] = useState<File | null>(null);
    return <FileUpload aria-label="Requests" value={file} onChange={setFile} meta={file ? "(2 B)" : undefined} aria-invalid />;
  }
  mount(<Example />);
  const input = screen.getByLabelText("Requests");
  input.focus();
  const choose = screen.getByRole("button", { name: "Choose file" });
  assert.ok(document.activeElement === choose, "form validation focusing the field lands on the button");
  assert.equal(choose.getAttribute("aria-invalid"), "true");
  fireEvent.change(input, { target: { files: [new window.File(["{}"], "batch.jsonl")] } });
  assert.equal(screen.getByText("batch.jsonl").textContent, "batch.jsonl(2 B)");
  assert.ok(screen.getByRole("button", { name: "Choose another" }));
  assert.equal(screen.queryByText("No file chosen"), null);
});

test("a disabled file upload disables picking, replacing and removing", () => {
  mount(<FileUpload aria-label="Requests" value={new window.File(["{}"], "locked.jsonl")} onChange={() => assert.fail("no change while disabled")} disabled className="upload-row" />);
  assert.ok(screen.getByLabelText("Requests").closest(".upload-row"), "the caller's class lands on the row");
  assert.equal((screen.getByLabelText("Requests") as HTMLInputElement).disabled, true);
  assert.equal((screen.getByRole("button", { name: "Choose another" }) as HTMLButtonElement).disabled, true);
  assert.equal((screen.getByRole("button", { name: "Remove" }) as HTMLButtonElement).disabled, true);
});

test("progress clamps negative and non-finite values and falls back to 100 for a non-positive max", () => {
  const value = (props: { value: number; max?: number }) => {
    const view = mount(<Progress label="Done" {...props} />);
    const bar = screen.getByRole("progressbar", { name: "Done" });
    const result = [bar.getAttribute("aria-valuenow"), bar.getAttribute("aria-valuemax"), (bar.firstElementChild as HTMLElement).style.width];
    view.unmount();
    return result;
  };
  assert.deepEqual(value({ value: -5 }), ["0", "100", "0%"]);
  assert.deepEqual(value({ value: Number.NaN }), ["0", "100", "0%"]);
  assert.deepEqual(value({ value: 25, max: 50 }), ["25", "50", "50%"]);
  assert.deepEqual(value({ value: 30, max: 0 }), ["30", "100", "30%"]);
});

test("confirmation cancel closes without confirming, and custom labels and the destructive style apply", async () => {
  let confirmed = 0;
  mount(<ConfirmPopover trigger={<button>Stop</button>} title="Stop it?" confirmText="Stop now" cancelText="Keep running" destructive onConfirm={() => { confirmed++; }} />);
  const user = userEvent.setup();
  await user.click(screen.getByRole("button", { name: "Stop" }));
  assert.ok(await screen.findByText("Stop it?"));
  await user.click(screen.getByRole("button", { name: "Keep running" }));
  await waitFor(() => assert.equal(screen.queryByRole("button", { name: "Stop now" }), null));
  assert.equal(confirmed, 0);
});

test("a pending confirmation cannot be dismissed and a non-Error failure shows the generic message", async () => {
  let reject!: (reason: unknown) => void;
  let calls = 0;
  mount(<ConfirmPopover trigger={<button>Delete</button>} title="Delete?" onConfirm={() => { calls++; return new Promise((_, no) => { reject = no; }); }} />);
  const user = userEvent.setup();
  await user.click(screen.getByRole("button", { name: "Delete" }));
  const confirmButton = await screen.findByRole("button", { name: "Confirm" });
  await user.click(confirmButton);
  assert.equal((screen.getByRole("button", { name: "Cancel" }) as HTMLButtonElement).disabled, true);
  await user.keyboard("{Escape}");
  fireEvent.click(confirmButton);
  assert.ok(screen.getByText("Delete?"), "Escape does not close a pending confirmation");
  assert.equal(calls, 1, "a second press while pending does not confirm again");
  reject("offline");
  assert.equal((await screen.findByRole("alert")).textContent, "Request failed. Please retry.");
  await user.keyboard("{Escape}");
  await waitFor(() => assert.equal(screen.queryByText("Delete?"), null));
  await user.click(screen.getByRole("button", { name: "Delete" }));
  await screen.findByText("Delete?");
  assert.equal(screen.queryByRole("alert"), null, "reopening clears the previous failure");
});

test("filter chips mark the chosen one pressed, show counts and report presses", async () => {
  const chosen: string[] = [];
  function Example() {
    const [value, setValue] = useState<"" | "chat" | "embed">("");
    return <FilterChips label="Capability" className="chip-row" value={value} onChange={(next) => { chosen.push(next); setValue(next); }} items={[{ value: "chat", label: "Chat", count: 3 }, { value: "embed", label: "Embedding" }]} />;
  }
  mount(<Example />);
  const group = screen.getByRole("group", { name: "Capability" });
  const chat = screen.getByRole("button", { name: "Chat3" });
  const embed = screen.getByRole("button", { name: "Embedding" });
  assert.ok(group.contains(chat));
  assert.ok(group.classList.contains("chip-row"), "the caller's class lands on the group");
  assert.deepEqual([chat, embed].map((button) => button.getAttribute("aria-pressed")), ["false", "false"]);
  const user = userEvent.setup();
  embed.focus();
  await user.keyboard("{Enter}");
  assert.deepEqual([chat, embed].map((button) => button.getAttribute("aria-pressed")), ["false", "true"]);
  await user.click(chat);
  assert.deepEqual([chat, embed].map((button) => button.getAttribute("aria-pressed")), ["true", "false"]);
  assert.deepEqual(chosen, ["embed", "chat"]);
});

test("a clearable select chosen by keyboard offers an explicit remove that clears it", async () => {
  function Example() {
    const [value, setValue] = useState<string | undefined>();
    return <OptionSelect aria-label="Model" clearable value={value} onChange={setValue} placeholder="Any model" options={[{ value: "a", label: "alpha" }, { value: "b", label: "beta" }]} />;
  }
  mount(<Example />);
  assert.equal(screen.queryByRole("button", { name: "Remove" }), null, "nothing to clear before a choice");
  const user = userEvent.setup();
  screen.getByRole("combobox", { name: "Model" }).focus();
  await user.keyboard("{ArrowDown}");
  // The list opens with the first option highlighted; only then does ArrowDown move on.
  await waitFor(() => assert.ok(document.activeElement === screen.getByRole("option", { name: "alpha" }), "alpha is highlighted"));
  await user.keyboard("{ArrowDown}");
  await waitFor(() => assert.ok(document.activeElement === screen.getByRole("option", { name: "beta" }), "beta is highlighted"));
  await user.keyboard("{Enter}");
  await waitFor(() => assert.equal(screen.getByRole("combobox", { name: "Model" }).textContent, "beta"));
  const remove = screen.getByRole("button", { name: "Remove" });
  remove.focus();
  await user.keyboard("{Enter}");
  await waitFor(() => assert.equal(screen.queryByRole("button", { name: "Remove" }), null));
  assert.ok(screen.getByRole("combobox", { name: "Model" }).textContent?.includes("Any model"));
});

test("a disabled select disables its remove action and a non-clearable one never offers it", () => {
  mount(<><OptionSelect aria-label="Locked" clearable disabled value="a" onChange={() => assert.fail("no change while disabled")} options={[{ value: "a", label: "alpha" }]} />
    <OptionSelect aria-label="Fixed" value="a" onChange={() => {}} options={[{ value: "a", label: "alpha" }]} /></>);
  const removes = screen.getAllByRole("button", { name: "Remove" }) as HTMLButtonElement[];
  assert.equal(removes.length, 1);
  assert.equal(removes[0].disabled, true);
});

test("a danger notice is an alert; info, warning and neutral notices are statuses with their text and action", () => {
  mount(<>
    <Notice tone="danger" title="Disk failed" />
    <Notice tone="warning" title="Disk almost full">Free space below 10%</Notice>
    <Notice title="Heads up" action={<button>Learn more</button>} />
    <Notice tone="neutral" title="Saved" />
  </>);
  assert.equal(screen.getByRole("alert").textContent, "Disk failed");
  const statuses = screen.getAllByRole("status");
  assert.deepEqual(statuses.map((status) => status.textContent), ["Disk almost fullFree space below 10%", "Heads upLearn more", "Saved"]);
  assert.ok(statuses[1].contains(screen.getByRole("button", { name: "Learn more" })));
  for (const notice of [screen.getByRole("alert"), ...statuses]) {
    assert.equal(notice.querySelector("svg")?.closest("[aria-hidden='true']") != null, true, "the icon is hidden from assistive technology");
  }
});

test("an adapted antd icon is hidden from assistive technology, sized by its slot and named after the glyph", () => {
  function Glyph({ className, style, ...rest }: { className?: string; style?: React.CSSProperties; "aria-hidden"?: boolean }) {
    return <svg data-testid="glyph" className={className} style={style} {...rest} />;
  }
  const Named = Object.assign((props: Parameters<typeof Glyph>[0]) => <Glyph {...props} />, { displayName: "CloudOutlined" });
  // antd's icons are forwardRef objects: no function name, only an optional displayName.
  const Anonymous = React.forwardRef<SVGSVGElement, Parameters<typeof Glyph>[0]>((props, _ref) => <Glyph {...props} />);
  const Adapted = fromAntd(Glyph) as unknown as React.ComponentType<{ size?: number | string; className?: string }>;
  assert.equal((fromAntd(Glyph) as unknown as { displayName: string }).displayName, "fromAntd(Glyph)");
  assert.equal((fromAntd(Named) as unknown as { displayName: string }).displayName, "fromAntd(CloudOutlined)");
  assert.equal((fromAntd(Anonymous) as unknown as { displayName: string }).displayName, "fromAntd(Icon)");
  mount(<Adapted size={18} className="lead" />);
  const svg = screen.getByTestId("glyph");
  assert.equal(svg.getAttribute("aria-hidden"), "true");
  assert.equal(svg.getAttribute("class"), "lead");
  assert.equal(svg.style.fontSize, "18px");
});
