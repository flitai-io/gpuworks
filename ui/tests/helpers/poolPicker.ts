// Reads and drives the GPU node pool choice (ui/src/pages/kubernetes/KubePoolPicker.tsx)
// the way someone looking at it does: a list named by the field's label holding
// the pools chosen, each with its "In the entry" tag and, while more than one is
// chosen, a button that takes it out; under it a combobox that adds one of the
// cluster's other pools, opened from the keyboard and read option by option.
// Testing Library is loaded on call: test files set up the DOM globals first.
import assert from "node:assert/strict";
import { framesSettled } from "./platformDialog.ts";

type User = { keyboard: (text: string) => Promise<void>; click: (element: Element) => Promise<void> };
const rtl = () => import("@testing-library/react");

/**
 * The pool choice inside `scope`, by the names it carries in that language:
 * `label` names the list, `addLabel` the combobox that adds, `removeName`
 * names a pool's remove button, `inEntry` is the tag of a pool the entry
 * already holds.
 */
export function poolPicker(scope: () => HTMLElement, names: { label: string; addLabel: string; removeName: (pool: string) => string; inEntry: string }) {
  const list = async () => (await rtl()).within(scope()).getByRole("list", { name: names.label });
  const items = async () => (await rtl()).within(await list()).queryAllByRole("listitem");
  /** The pools chosen, top to bottom, as each line reads without its tag. */
  const chosen = async (): Promise<string[]> => (await items()).map((item) => item.querySelector("span")?.textContent ?? "");
  /** The chosen pools that carry the "In the entry" tag. */
  const tagged = async (): Promise<string[]> => (await items()).filter((item) => [...item.querySelectorAll('[data-slot="badge"]')].some((tag) => tag.textContent === names.inEntry)).map((item) => item.querySelector("span")?.textContent ?? "");
  /** The names of the remove buttons in the list, top to bottom. */
  const removeButtons = async (): Promise<string[]> => {
    const { within } = await rtl();
    return within(await list()).queryAllByRole("button").map((button) => button.getAttribute("aria-label") ?? "");
  };
  /** Takes the pool with that name out with the pointer. */
  const remove = async (user: User, pool: string) => {
    const { within } = await rtl();
    await user.click(within(await list()).getByRole("button", { name: names.removeName(pool) }));
  };
  /** The combobox that adds a pool; none once every pool is chosen. */
  const adder = async () => (await rtl()).within(scope()).queryByRole("combobox", { name: names.addLabel });
  /** Opens the combobox from the keyboard and returns its list of pools. */
  const open = async (user: User) => {
    const { screen, waitFor } = await rtl();
    const trigger = await adder();
    assert.ok(trigger, "a pool can be added");
    if (trigger.getAttribute("aria-expanded") !== "true") {
      trigger.focus();
      await user.keyboard("{Enter}");
      await waitFor(() => assert.equal(trigger.getAttribute("aria-expanded"), "true", "the choice opened"));
    }
    const listbox = await screen.findByRole("listbox");
    await framesSettled();
    return listbox;
  };
  const closed = async () => {
    const { screen, waitFor } = await rtl();
    await waitFor(() => assert.equal(screen.queryAllByRole("listbox").length, 0, "the choice closed"));
    await framesSettled();
  };
  /** The pools the combobox offers, top to bottom; it is closed again with Escape. */
  const offered = async (user: User): Promise<string[]> => {
    const { within } = await rtl();
    const listbox = await open(user);
    const options = within(listbox).getAllByRole("option").map((option) => option.textContent ?? "");
    await user.keyboard("{Escape}");
    await closed();
    return options;
  };
  /** Adds the pool that reads so, picking it with the pointer. */
  const add = async (user: User, pool: string) => {
    const { within } = await rtl();
    await user.click(within(await open(user)).getByRole("option", { name: pool }));
    await closed();
  };
  return { list, chosen, tagged, removeButtons, remove, adder, open, closed, offered, add };
}
