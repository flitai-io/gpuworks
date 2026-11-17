// Reads what an account's row of the External Platforms page opens to
// (ui/src/pages/platforms/ProviderResources.tsx) the way it is laid out for
// someone looking at it: one table, its column titles, and, for an account
// with managed Kubernetes, row groups named for what the machines belong to
// ("Standalone instances", then each cluster), each led by a header row whose
// button folds it. Testing Library is loaded on call: test files set up the
// DOM globals first.

const rtl = () => import("@testing-library/react");
const INSTANCE_TITLES = ["Instance", "实例"];

/** The detail's table within the scope: the one whose first column is the instance. On the page it sits inside the platforms' own table. */
export async function detailTable(scope: HTMLElement = document.body): Promise<HTMLElement> {
  const { within } = await rtl();
  const tables = within(scope).getAllByRole("table").filter((table) => INSTANCE_TITLES.includes(within(table).getAllByRole("columnheader")[0]?.textContent ?? ""));
  if (tables.length !== 1) throw new Error(`expected one account detail table, found ${tables.length}`);
  return tables[0];
}

/** The detail table's column titles, left to right. */
export async function columnTitles(scope?: HTMLElement): Promise<string[]> {
  const { within } = await rtl();
  return within(await detailTable(scope)).getAllByRole("columnheader").map((header) => header.textContent ?? "");
}

/** The names of the detail's groups, top to bottom; none for an account whose instances are listed ungrouped. */
export async function groupNames(scope?: HTMLElement): Promise<string[]> {
  const { within } = await rtl();
  return within(await detailTable(scope)).getAllByRole("rowgroup").map((group) => group.getAttribute("aria-label")).filter((name): name is string => name !== null);
}

/** The group with that name; waits for it, since a cluster's group appears when the cloud has answered. */
export async function groupOf(name: string, scope: HTMLElement = document.body): Promise<HTMLElement> {
  const { within } = await rtl();
  return within(scope).findByRole("rowgroup", { name });
}

/** A group's header row: its name and what it is on the left, where it stands and what can be done on the right. */
export async function groupHeader(group: HTMLElement): Promise<HTMLElement> {
  const { within } = await rtl();
  return within(group).getAllByRole("row")[0];
}

/** The rows under a group's header; none while the group is folded. */
export async function groupRows(group: HTMLElement): Promise<HTMLElement[]> {
  const { within } = await rtl();
  return within(group).getAllByRole("row").slice(1);
}

/** The button that folds or unfolds a group: the one in its header that says whether the group is open. */
export async function foldButton(group: HTMLElement): Promise<HTMLButtonElement> {
  const { within } = await rtl();
  const buttons = within(await groupHeader(group)).getAllByRole("button").filter((button) => button.hasAttribute("aria-expanded") && !button.hasAttribute("aria-haspopup"));
  if (buttons.length !== 1) throw new Error(`expected one fold button, found ${buttons.length}`);
  return buttons[0] as HTMLButtonElement;
}

/** The row of the instance with that name (or id, for one without a name); waits for it. */
export async function instanceRow(name: string, scope: HTMLElement = document.body): Promise<HTMLElement> {
  const { within } = await rtl();
  const row = (await within(scope).findByText(name, { selector: "td *" })).closest("tr");
  if (!row) throw new Error(`no row for instance ${name}`);
  return row as HTMLElement;
}

/** A row's cell under the column with that title. */
export async function cellUnder(row: HTMLElement, title: string): Promise<HTMLElement> {
  const { within } = await rtl();
  const column = (await columnTitles(row.closest("table")?.parentElement ?? undefined)).indexOf(title);
  if (column < 0) throw new Error(`no "${title}" column`);
  return within(row).getAllByRole("cell")[column];
}

/** The names of the instances listed in the scope, top to bottom: the first line of each row that has an instance's cells. */
export async function instanceNames(scope?: HTMLElement): Promise<string[]> {
  const { within } = await rtl();
  const table = await detailTable(scope);
  const columns = within(table).getAllByRole("columnheader").length;
  return within(table).getAllByRole("row")
    .filter((row) => within(row).queryAllByRole("cell").length === columns)
    .map((row) => within(row).getAllByRole("cell")[0].firstElementChild?.textContent ?? "");
}

/**
 * What a cluster group's header says, in its two halves: on the left the
 * cluster (its name, the vendor's product it is, a namespace warning), on the
 * right where it stands. Buttons (fold, Connect, the entry's icon actions)
 * are left out: they are found by role.
 */
export async function clusterHeader(group: HTMLElement): Promise<{ title: string[]; standing: string }> {
  const header = await groupHeader(group);
  const [left, right] = [...(header.querySelector("td")?.firstElementChild?.children ?? [])] as HTMLElement[];
  const title = [...(left?.children ?? [])].filter((part) => part.tagName !== "BUTTON").map((part) => part.textContent ?? "");
  const standing = [...(right?.children ?? [])].filter((part) => part.tagName !== "BUTTON" && part.querySelector("button") === null).map((part) => part.textContent ?? "").join("");
  return { title, standing };
}

/**
 * The "label value" pairs in the scope (a cluster's group, or a local
 * cluster's opened row), as a description list reads them: each term with its
 * definition's text, whitespace collapsed, in order.
 */
export async function facts(scope: HTMLElement): Promise<Record<string, string>> {
  const { within } = await rtl();
  const terms = within(scope).queryAllByRole("term");
  return Object.fromEntries(terms.map((term) => [term.textContent ?? "", (term.nextElementSibling?.textContent ?? "").replace(/\s+/g, " ").trim()]));
}

/** The value (the definition) of the fact with that label in the scope. */
export async function factValue(scope: HTMLElement, label: string): Promise<HTMLElement> {
  const { within } = await rtl();
  const term = within(scope).getAllByRole("term").find((each) => each.textContent === label);
  if (!term?.nextElementSibling) throw new Error(`no "${label}" fact`);
  return term.nextElementSibling as HTMLElement;
}
