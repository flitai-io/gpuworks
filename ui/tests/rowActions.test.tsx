import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import { createI18n, dom } from "./helpers/dom.ts";

// A row's actions (ui/src/pages/platforms/RowActions.tsx): up to three are
// icon buttons named by their action; a row with more keeps its common ones
// out and folds the rest into a "More" menu, the destructive ones last under a
// rule. The cluster entry's actions built on it are in tests/kubeEntryActions.test.tsx.

const en = await createI18n(["common"], "en");
const zh = await createI18n(["common"], "zh");
const { cleanup, render, screen, waitFor, within } = await import("@testing-library/react");
const { default: userEvent } = await import("@testing-library/user-event");
const { default: RowActions } = await import("../src/pages/platforms/RowActions.tsx");
const { ConsoleIcon } = await import("../src/ui-kit/consoleIcons.ts");
import type { RowAction } from "../src/pages/platforms/RowActions";

afterEach(() => cleanup());

const MORE = en.locale.common.actions.more as string;

/** Actions named by their keys, each recording its selection. */
function actions(specs: Array<Partial<RowAction> & { key: string }>) {
  const picked: string[] = [];
  const list = specs.map((spec) => ({ label: spec.key, icon: ConsoleIcon.edit, onSelect: () => picked.push(spec.key), ...spec }));
  return { list, picked };
}

function renderRow(list: RowAction[], language: "en" | "zh" = "en") {
  const view = render(
    <I18nextProvider i18n={(language === "en" ? en : zh).i18n}>
      <RowActions actions={list} label="Row actions" />
    </I18nextProvider>,
  );
  return { view, user: userEvent.setup({ document: dom.window.document }) };
}
const group = () => within(screen.getByRole("group", { name: "Row actions" }));
const shown = () => group().getAllByRole("button").map((each) => each.getAttribute("aria-label"));
// A click on More, as nodesPage's row menus are opened: user-event's pointer
// sequence races Base UI's open-on-press under load.
const openMore = () => (group().getByRole("button", { name: MORE }) as HTMLButtonElement).click();
const menuItems = async () => {
  const menu = await screen.findByRole("menu");
  return within(menu).getAllByRole("menuitem");
};

test("three actions or fewer are all icon buttons named by their action, each saying its name on hover; clicking one selects it", async () => {
  const { list, picked } = actions([{ key: "Edit" }, { key: "Sync" }, { key: "Delete", danger: true }]);
  const { user } = renderRow(list);
  assert.deepEqual(shown(), ["Edit", "Sync", "Delete"]);
  assert.equal(group().queryAllByRole("button", { name: MORE }).length, 0, "nothing to fold");
  await user.hover(group().getByRole("button", { name: "Sync" }).parentElement as HTMLElement);
  assert.ok(await screen.findByText("Sync"), "the name shows on hover");
  await user.click(group().getByRole("button", { name: "Delete" }));
  assert.deepEqual(picked, ["Delete"]);
  assert.ok(group().getByRole("button", { name: "Delete" }).className.includes("text-fg-danger"), "a destructive one is drawn red");
  assert.ok(!group().getByRole("button", { name: "Edit" }).className.includes("text-fg-danger"));
});

test("a disabled button cannot be selected and says why on hover", async () => {
  const { list, picked } = actions([{ key: "Apply", disabled: true, hint: "The cluster's API is private" }, { key: "Edit" }]);
  const { user } = renderRow(list);
  const apply = group().getByRole("button", { name: "Apply" }) as HTMLButtonElement;
  assert.equal(apply.disabled, true);
  await user.hover(apply.parentElement as HTMLElement);
  assert.ok(await screen.findByText("The cluster's API is private"));
  await user.click(apply);
  assert.deepEqual(picked, []);
});

test("more than three keep the common ones out and fold the rest into More, plain ones first and destructive ones red under a rule", async () => {
  const { list, picked } = actions([
    { key: "Edit", common: true }, { key: "Revoke", danger: true }, { key: "Copy" }, { key: "Apply", common: true },
    { key: "Delete", danger: true }, { key: "Manifest" },
  ]);
  const { user } = renderRow(list);
  assert.deepEqual(shown(), ["Edit", "Apply", MORE]);
  assert.equal(screen.queryAllByRole("menu").length, 0, "the menu is closed until asked for");
  openMore();
  const items = await menuItems();
  assert.deepEqual(items.map((each) => each.getAttribute("aria-label")), ["Copy", "Manifest", "Revoke", "Delete"]);
  const menu = screen.getByRole("menu");
  const separators = within(menu).getAllByRole("separator");
  assert.equal(separators.length, 1);
  // The rule sits between the last plain item and the first destructive one.
  const order = [...menu.querySelectorAll('[role="menuitem"], [role="separator"]')].map((each) => each.getAttribute("aria-label") ?? "—");
  assert.deepEqual(order, ["Copy", "Manifest", "—", "Revoke", "Delete"]);
  assert.deepEqual(items.map((each) => each.className.includes("text-fg-danger")), [false, false, true, true]);
  await user.click(within(menu).getByRole("menuitem", { name: "Delete" }));
  assert.deepEqual(picked, ["Delete"]);
  await waitFor(() => assert.equal(screen.queryAllByRole("menu").length, 0, "choosing closes the menu"));
});

test("a folded menu of only plain or only destructive items draws no rule", async () => {
  const plain = renderRow(actions([{ key: "Edit", common: true }, { key: "A" }, { key: "B" }, { key: "C" }]).list);
  openMore();
  await menuItems();
  assert.equal(within(screen.getByRole("menu")).queryAllByRole("separator").length, 0);
  plain.view.unmount();

  const danger = renderRow(actions([{ key: "Edit", common: true }, { key: "A", danger: true }, { key: "B", danger: true }, { key: "C", danger: true }]).list);
  openMore();
  assert.equal((await menuItems()).length, 3);
  assert.equal(within(screen.getByRole("menu")).queryAllByRole("separator").length, 0);
});

test("a disabled menu item cannot be chosen and says why in its title", async () => {
  const { list, picked } = actions([{ key: "Edit", common: true }, { key: "Manifest" }, { key: "Remove", danger: true, disabled: true, hint: "Revoked" }, { key: "Delete", danger: true }]);
  const { user } = renderRow(list);
  openMore();
  const remove = within(await screen.findByRole("menu")).getByRole("menuitem", { name: "Remove" });
  assert.equal(remove.getAttribute("aria-disabled"), "true");
  assert.equal(remove.getAttribute("title"), "Revoked");
  assert.equal(within(screen.getByRole("menu")).getByRole("menuitem", { name: "Manifest" }).getAttribute("title"), null, "an enabled one has no hint");
  await user.click(remove);
  assert.deepEqual(picked, []);
});

test("from the keyboard: Tab reaches More, Enter opens it, arrows move, Enter chooses; Escape closes it and focus returns to More", async () => {
  const { list, picked } = actions([{ key: "Edit", common: true }, { key: "Copy" }, { key: "Manifest" }, { key: "Delete", danger: true }]);
  const { user } = renderRow(list);
  const more = group().getByRole("button", { name: MORE });
  await user.tab();
  assert.equal(document.activeElement, group().getByRole("button", { name: "Edit" }));
  await user.tab();
  assert.equal(document.activeElement, more);

  await user.keyboard("{Enter}");
  await menuItems();
  await waitFor(() => assert.equal(document.activeElement?.getAttribute("aria-label"), "Copy", "the first item takes focus"));
  await user.keyboard("{ArrowDown}");
  await waitFor(() => assert.equal(document.activeElement?.getAttribute("aria-label"), "Manifest"));
  await user.keyboard("{Enter}");
  assert.deepEqual(picked, ["Manifest"]);
  await waitFor(() => assert.equal(screen.queryAllByRole("menu").length, 0));

  more.focus();
  await user.keyboard(" ");
  await menuItems();
  await user.keyboard("{Escape}");
  await waitFor(() => assert.equal(screen.queryAllByRole("menu").length, 0, "Escape closes it"));
  await waitFor(() => assert.equal(document.activeElement, more, "focus returns to More"));
  assert.deepEqual(picked, ["Manifest"], "closing chose nothing");
});

test("More is named in Chinese in zh", async () => {
  renderRow(actions([{ key: "编辑", common: true }, { key: "A" }, { key: "B" }, { key: "C" }]).list, "zh");
  assert.deepEqual(shown(), ["编辑", "更多"]);
});
