// Reads and drives a row's actions (ui/src/pages/platforms/RowActions.tsx) the
// way someone using them does: a group named "Actions" of icon buttons, and,
// for a row with more than three, a "More" button whose menu holds the rest.
// The confirmations its destructive items open are dialogs. Testing Library is
// loaded on call: test files set up the DOM globals first.
import assert from "node:assert/strict";

const rtl = () => import("@testing-library/react");
const GROUP = ["Actions", "操作"];
const MORE = ["More", "更多"];

type User = { click: (element: Element) => Promise<void> };

/** The row's actions group inside `scope`, or null when it offers none (a viewer). */
export async function actionGroup(scope: HTMLElement): Promise<HTMLElement | null> {
  const { within } = await rtl();
  const groups = within(scope).queryAllByRole("group").filter((group) => GROUP.includes(group.getAttribute("aria-label") ?? ""));
  if (groups.length > 1) throw new Error(`expected at most one actions group, found ${groups.length}`);
  return groups[0] ?? null;
}

const moreOf = async (group: HTMLElement) => {
  const { within } = await rtl();
  return within(group).queryAllByRole("button").find((button) => MORE.includes(button.getAttribute("aria-label") ?? "")) as HTMLButtonElement | undefined;
};

/** Opens More and returns its menu. A plain click, as nodesPage's row menus are opened: user-event's pointer sequence races Base UI's open-on-press under load. */
export async function openMore(scope: HTMLElement): Promise<HTMLElement> {
  const { screen } = await rtl();
  const group = await actionGroup(scope);
  assert.ok(group, "the row offers actions");
  const more = await moreOf(group);
  assert.ok(more, "the row folds actions into More");
  // Already open: a second click would close it.
  const open = more.getAttribute("aria-expanded") === "true" && screen.queryByRole("menu");
  if (open) return open;
  more.click();
  return screen.findByRole("menu");
}

async function closeMenu() {
  const { fireEvent, screen, waitFor } = await rtl();
  const menu = screen.queryByRole("menu");
  if (!menu) return;
  fireEvent.keyDown(menu, { key: "Escape", code: "Escape", keyCode: 27 });
  await waitFor(() => assert.equal(screen.queryAllByRole("menu").length, 0, "the menu closed"));
}

/**
 * What the row offers, by name: the buttons left in the row (More among them
 * when some are folded) and the items folded into More, top to bottom. Empty
 * when the row offers nothing.
 */
export async function rowActions(scope: HTMLElement): Promise<{ shown: string[]; folded: string[] }> {
  const { within } = await rtl();
  const group = await actionGroup(scope);
  if (!group) return { shown: [], folded: [] };
  const shown = within(group).getAllByRole("button").map((button) => button.getAttribute("aria-label") ?? "");
  if (!(await moreOf(group))) return { shown, folded: [] };
  const menu = await openMore(scope);
  const folded = within(menu).getAllByRole("menuitem").map((item) => item.getAttribute("aria-label") ?? "");
  await closeMenu();
  return { shown, folded };
}

/** The row's action `name`: its button when it is in the row, else its item in More (null when neither). */
export async function findRowAction(scope: HTMLElement, name: string): Promise<HTMLElement | null> {
  const { within } = await rtl();
  const group = await actionGroup(scope);
  if (!group) return null;
  const button = within(group).queryAllByRole("button", { name }).at(0);
  if (button) return button;
  if (!(await moreOf(group))) return null;
  const menu = await openMore(scope);
  return within(menu).queryAllByRole("menuitem", { name }).at(0) ?? null;
}

/** Chooses the row's action `name`, from the row or from More. */
export async function chooseRowAction(user: User, scope: HTMLElement, name: string): Promise<void> {
  const action = await findRowAction(scope, name);
  assert.ok(action, `the row offers ${name}`);
  await user.click(action);
}

/** Clicks `label` in the confirmation dialog once it is open. */
export async function confirmDialog(user: User, label: string): Promise<void> {
  const { screen, within } = await rtl();
  const dialog = (await screen.findAllByRole("dialog")).at(-1) as HTMLElement;
  await user.click(within(dialog).getByRole("button", { name: label }));
}
