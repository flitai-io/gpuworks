// Drives the Add / Edit External Platform dialog (ui/src/components/ProviderConfigModal.tsx)
// the way someone at the keyboard does. Its platform choice is two steps
// under one label: the platform types as tabs (a tablist named "Platform
// type"), then the vendors of the chosen type in a combobox named by the
// field's label, opened with Enter and read option by option. Kubernetes is
// a type with nothing more to choose. Its fields are found by their labels;
// its buttons by their names.
// Testing Library is loaded on call: test files set up the DOM globals first.
import assert from "node:assert/strict";
import { afterEach, beforeEach } from "node:test";
import { pendingFrameCount } from "./dom.ts";

/**
 * jsdom has no animation frames of its own: helpers/dom.ts supplies them and
 * drops the ones a finished test left pending. Base UI's frame scheduler is
 * process-wide and would wait forever for a dropped frame, leaving the next
 * test's popup without its keyboard navigation; each test starts and ends
 * with a fresh one (as tests/zeronSelect.test.tsx does). Call once per file.
 */
export async function freshPopupFramesPerTest() {
  const { resetAnimationFrameScheduler } = await import("@base-ui/utils/useAnimationFrame");
  beforeEach(() => resetAnimationFrameScheduler());
  afterEach(() => resetAnimationFrameScheduler());
}
/** Resolves once the frames a popup scheduled while opening or closing have run: its list then answers the keyboard. */
export async function framesSettled() {
  const { waitFor } = await import("@testing-library/react");
  await waitFor(() => assert.equal(pendingFrameCount(), 0));
}

type User = { keyboard: (text: string) => Promise<void>; click: (element: Element) => Promise<void> };

const escaped = (text: string) => text.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
/** A field's label as it reads: a required one ends in its mark ("Name *"). */
export const field = (text: string) => new RegExp(`^${escaped(text)}( \\*)?$`);

/** The platform choice of the dialog, by the names its two steps carry in that language. */
export function platformChoice(label = "External platform", typesLabel = "Platform type") {
  const rtl = () => import("@testing-library/react");
  const tablist = async () => (await rtl()).screen.getByRole("tablist", { name: typesLabel });
  const tabs = async () => (await rtl()).within(await tablist()).getAllByRole("tab");
  /** The platform types offered, left to right. */
  const types = async () => (await tabs()).map((tab) => tab.textContent ?? "");
  /** The type the chosen platform is of: the one tab that is selected. */
  const chosenType = async () => (await tabs()).filter((tab) => tab.getAttribute("aria-selected") === "true").map((tab) => tab.textContent ?? "").join(" + ");
  /** The tab of that type. */
  const typeTab = async (name: string) => (await rtl()).within(await tablist()).getByRole("tab", { name });
  /** Chooses a platform type with the pointer. */
  const pickType = async (user: User, name: string) => {
    const { waitFor } = await rtl();
    await user.click(await typeTab(name));
    await waitFor(async () => assert.equal(await chosenType(), name));
    await framesSettled();
  };
  /** The vendors' combobox; a type with one platform (Kubernetes) has none. */
  const triggers = async () => (await rtl()).screen.queryAllByRole("combobox", { name: label });
  const trigger = async () => (await rtl()).screen.getByRole("combobox", { name: label });
  /** The platform the closed choice shows. */
  const chosen = async () => (await trigger()).textContent ?? "";
  /** Opens the vendors' choice from the keyboard and returns its list. */
  const open = async (user: User) => {
    const { screen, waitFor } = await rtl();
    const select = await trigger();
    if (select.getAttribute("aria-expanded") !== "true") {
      select.focus();
      await user.keyboard("{Enter}");
      await waitFor(() => assert.equal(select.getAttribute("aria-expanded"), "true", "the choice opened"));
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
  /** The vendors the combobox offers for the chosen type, top to bottom; it is closed again. The list has no group titles: the tabs are the grouping. */
  const choices = async (user: User): Promise<string[]> => {
    const { within } = await rtl();
    const listbox = await open(user);
    const options = within(listbox).getAllByRole("option").map((option) => option.textContent ?? "");
    assert.equal(within(listbox).queryAllByRole("group").length, 0, "one flat list");
    assert.equal(listbox.textContent, options.join(""), "nothing in the list but the platforms");
    await user.keyboard("{Escape}");
    await closed();
    return options;
  };
  /** Chooses the vendor with that name among the chosen type's. */
  const pick = async (user: User, name: string) => {
    const { within } = await rtl();
    await user.click(within(await open(user)).getByRole("option", { name }));
    await closed();
  };
  return { types, chosenType, typeTab, pickType, triggers, trigger, chosen, choices, pick, open, closed };
}

/**
 * The names of the buttons the dialog ends with, left to right: the row that
 * holds its last button named by its own text (the close mark in the corner
 * is named by a label).
 */
export function footerButtons(dialog: HTMLElement): string[] {
  const last = [...dialog.querySelectorAll("button")].filter((button) => !button.hasAttribute("aria-label") && !button.hidden).at(-1);
  return [...(last?.parentElement?.querySelectorAll("button") ?? [])].map((button) => (button.textContent ?? "").trim());
}

/** The labels of the fields the dialog shows, top to bottom, without their required marks. */
export function fieldLabels(dialog: HTMLElement): string[] {
  return [...dialog.querySelectorAll("label")].filter((label) => !label.closest("[hidden]")).map((label) => (label.textContent ?? "").replace(/ \*$/, ""));
}

/** What assistive technology reads as a control's description: its help and, when refused, the reason. */
export function describedBy(control: HTMLElement): string {
  return (control.getAttribute("aria-describedby") ?? "").split(" ").filter(Boolean).map((id) => control.ownerDocument.getElementById(id)?.textContent ?? "").join("\n");
}
