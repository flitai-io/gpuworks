import "./helpers/dom.ts";
import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import React, { useRef } from "react";
import { z } from "zod";
import { zodResolver } from "@hookform/resolvers/zod";
import { useForm } from "react-hook-form";
const { render, screen, fireEvent, waitFor, cleanup } = await import("@testing-library/react");
const { FormField } = await import("../src/ui-kit/form/FormField.tsx");
const { formSubmit } = await import("../src/ui-kit/form/submit.ts");
const { Input } = await import("../src/components/ui/input.tsx");
afterEach(cleanup);

// Deliberately a test-only schema, not a second copy of product rules.
const schema = z.object({ name: z.string().min(1, "Required"), confirmation: z.string() })
  .refine((data) => data.name === data.confirmation, { path: ["confirmation"], message: "Must match" });
function Example({ submit }: { submit: (values: z.infer<typeof schema>) => void }) {
  const form = useForm({ defaultValues: { name: "", confirmation: "" }, resolver: zodResolver(schema), shouldFocusError: false });
  const ref = useRef<HTMLFormElement>(null);
  return <form ref={ref} noValidate onSubmit={formSubmit(form, () => ref.current, submit)}>
    <FormField control={form.control} name="name" label="Name" description="Public name" required>
      {(binding) => <Input {...binding} />}
    </FormField>
    <FormField control={form.control} name="confirmation" label="Confirm" rules={{ deps: ["name"] }}>
      {(binding) => <Input {...binding} />}
    </FormField>
    <button type="submit">Save</button>
  </form>;
}

test("required errors are named, described, scrolled and focused; a valid submit preserves values", async () => {
  const values: unknown[] = [];
  render(<Example submit={(value) => values.push(value)} />);
  const name = screen.getByRole("textbox", { name: "Name" });
  let scrolled = false;
  name.closest("[data-form-field]")!.scrollIntoView = () => { scrolled = true; };
  fireEvent.click(screen.getByRole("button", { name: "Save" }));
  await screen.findByRole("alert");
  assert.equal(document.activeElement, name);
  assert.equal(scrolled, true);
  assert.match(name.getAttribute("aria-describedby")!, /description.*error/);
  assert.equal(values.length, 0);
  fireEvent.change(name, { target: { value: "fleet" } });
  fireEvent.change(screen.getByRole("textbox", { name: "Confirm" }), { target: { value: "fleet" } });
  fireEvent.click(screen.getByRole("button", { name: "Save" }));
  await waitFor(() => assert.deepEqual(values, [{ name: "fleet", confirmation: "fleet" }]));
  assert.equal(screen.queryByRole("alert"), null);
});

test("cross-field validation points to the dependent control and repeated forms have unique IDs", async () => {
  render(<><Example submit={() => {}} /><Example submit={() => {}} /></>);
  const inputs = screen.getAllByRole("textbox");
  assert.equal(new Set(inputs.map((input) => input.id)).size, 4);
  fireEvent.change(inputs[0], { target: { value: "fleet" } });
  fireEvent.click(screen.getAllByRole("button", { name: "Save" })[0]);
  await screen.findByText("Must match");
  assert.equal(document.activeElement, inputs[1]);
  assert.equal(inputs[3].getAttribute("aria-invalid"), "false");
});
