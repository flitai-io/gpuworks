import "./helpers/dom.ts";
import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import React, { useRef } from "react";
import { useForm, type FieldErrors } from "react-hook-form";
const { render, screen, fireEvent, waitFor, cleanup } = await import("@testing-library/react");
const { scrollToFirstError, formSubmit } = await import("../src/ui-kit/form/submit.ts");
afterEach(cleanup);

// The fields as FormField marks them: a data-form-field wrapper around one control.
function Fields({ fields }: { fields: { name: string; label: string; disabled?: boolean; extra?: string }[] }) {
  return <form aria-label="Settings">{fields.map((field) => <div key={field.name} data-form-field={field.name}>
    <label>{field.label}<input disabled={field.disabled} /></label>
    {field.extra && <button type="button">{field.extra}</button>}
  </div>)}</form>;
}
const error = { type: "required", message: "Required" };
const form = () => screen.getByRole("form", { name: "Settings" });
function trackScrolls() {
  const scrolled: string[] = [];
  for (const field of form().querySelectorAll<HTMLElement>("[data-form-field]")) {
    field.scrollIntoView = () => { scrolled.push(field.dataset.formField!); };
  }
  return scrolled;
}

test("a nested name a.b finds its field and focuses its control", () => {
  render(<Fields fields={[{ name: "name", label: "Name" }, { name: "limits.memory", label: "Memory" }]} />);
  const scrolled = trackScrolls();
  scrollToFirstError(form(), { limits: { memory: error } } as FieldErrors);
  assert.deepEqual(scrolled, ["limits.memory"]);
  assert.ok(document.activeElement === screen.getByRole("textbox", { name: "Memory" }), "the memory input has focus");
});

test("an array name a[0].b finds the error stored under a.0.b", () => {
  render(<Fields fields={[{ name: "ports[0].number", label: "First port" }, { name: "ports[1].number", label: "Second port" }]} />);
  const scrolled = trackScrolls();
  scrollToFirstError(form(), { ports: [undefined, { number: error }] } as unknown as FieldErrors);
  assert.deepEqual(scrolled, ["ports[1].number"]);
  assert.ok(document.activeElement === screen.getByRole("textbox", { name: "Second port" }), "the second port has focus");
});

test("the first error in DOM order wins, not the first key in the errors object", () => {
  render(<Fields fields={[{ name: "host", label: "Host" }, { name: "port", label: "Port" }, { name: "user", label: "User" }]} />);
  const scrolled = trackScrolls();
  scrollToFirstError(form(), { user: error, port: error } as FieldErrors);
  assert.deepEqual(scrolled, ["port"]);
  assert.ok(document.activeElement === screen.getByRole("textbox", { name: "Port" }), "the port has focus");
});

test("no matching error, no form or an error on an unrendered field neither scrolls nor moves focus", () => {
  render(<><button>Outside</button><Fields fields={[{ name: "host", label: "Host" }]} /></>);
  const outside = screen.getByRole("button", { name: "Outside" });
  outside.focus();
  const scrolled = trackScrolls();
  scrollToFirstError(form(), {});
  scrollToFirstError(form(), { hidden: error } as FieldErrors);
  scrollToFirstError(null, { host: error } as FieldErrors);
  assert.deepEqual(scrolled, []);
  assert.ok(document.activeElement === outside, "focus stays where it was");
});

test("a disabled control is skipped for the field's next enabled control", () => {
  render(<Fields fields={[{ name: "token", label: "Token", disabled: true, extra: "Generate" }]} />);
  trackScrolls();
  scrollToFirstError(form(), { token: error } as FieldErrors);
  assert.ok(document.activeElement === screen.getByRole("button", { name: "Generate" }), "the enabled button in the field has focus");
});

test("a field whose only control is disabled is still scrolled to without a focus change", () => {
  render(<><button>Outside</button><Fields fields={[{ name: "token", label: "Token", disabled: true }]} /></>);
  const outside = screen.getByRole("button", { name: "Outside" });
  outside.focus();
  const scrolled = trackScrolls();
  scrollToFirstError(form(), { token: error } as FieldErrors);
  assert.deepEqual(scrolled, ["token"]);
  assert.ok(document.activeElement === outside, "a disabled control does not take focus");
});

test("a field without scrollIntoView (older browsers) still gets focus", () => {
  render(<Fields fields={[{ name: "host", label: "Host" }]} />);
  const field = form().querySelector<HTMLElement>("[data-form-field]")!;
  Object.defineProperty(field, "scrollIntoView", { value: undefined, configurable: true });
  scrollToFirstError(form(), { host: error } as FieldErrors);
  assert.ok(document.activeElement === screen.getByRole("textbox", { name: "Host" }), "the host has focus");
});

type Values = { host: string };
function Example({ onValid, onInvalid }: { onValid: (values: Values) => void; onInvalid?: (errors: FieldErrors<Values>) => void }) {
  const methods = useForm<Values>({ defaultValues: { host: "" }, shouldFocusError: false });
  const ref = useRef<HTMLFormElement>(null);
  return <form ref={ref} aria-label="Server" noValidate onSubmit={formSubmit(methods, () => ref.current, onValid, onInvalid)}>
    <div data-form-field="host"><label>Host<input {...methods.register("host", { required: "Required" })} /></label></div>
    <button type="submit">Save</button>
  </form>;
}

test("an invalid submit focuses the first error and calls onInvalid, never onValid", async () => {
  const valid: Values[] = [];
  const invalid: string[][] = [];
  render(<Example onValid={(values) => valid.push(values)} onInvalid={(errors) => invalid.push(Object.keys(errors))} />);
  fireEvent.click(screen.getByRole("button", { name: "Save" }));
  await waitFor(() => assert.deepEqual(invalid, [["host"]]));
  assert.ok(document.activeElement === screen.getByRole("textbox", { name: "Host" }), "the host has focus");
  assert.deepEqual(valid, []);
});

test("a valid submit calls onValid with the values; without onInvalid an invalid one only focuses", async () => {
  const valid: Values[] = [];
  render(<Example onValid={(values) => valid.push(values)} />);
  fireEvent.click(screen.getByRole("button", { name: "Save" }));
  await waitFor(() => assert.ok(document.activeElement === screen.getByRole("textbox", { name: "Host" }), "the host has focus"));
  assert.deepEqual(valid, []);
  fireEvent.change(screen.getByRole("textbox", { name: "Host" }), { target: { value: "gpu-1" } });
  fireEvent.click(screen.getByRole("button", { name: "Save" }));
  await waitFor(() => assert.deepEqual(valid, [{ host: "gpu-1" }]));
});
