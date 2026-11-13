// Zeron DataTable (ui/src/components/ui/data-table.tsx). The app builds its
// resource lists on useDataTable, DataTablePagination and DataTableColumnHeader
// (ui/src/components/resourceTable/ResourceTable.tsx); those come first. The
// rest covers DataTable's own public surface: selection, empty/loading states,
// row activation, grouping, pinning, the toolbar filters and column options.
import { dom, pendingFrameCount } from "./helpers/dom.ts";
import assert from "node:assert/strict";
import { afterEach, beforeEach, test as nodeTest } from "node:test";
import React, { useState } from "react";

for (const name of ["KeyboardEvent", "Document", "AbortController", "AbortSignal"] as const) {
  Object.defineProperty(globalThis, name, { value: dom.window[name], configurable: true });
}
if (!("getAnimations" in dom.window.Element.prototype)) {
  Object.defineProperty(dom.window.Element.prototype, "getAnimations", { configurable: true, value: () => [] });
}
(dom.window.Element.prototype as Element).setPointerCapture ??= () => {};

const { render, screen, fireEvent, waitFor, cleanup, within, act } = await import("@testing-library/react");
const { default: userEvent } = await import("@testing-library/user-event");
const tanstack = await import("@tanstack/react-table");
const { getExpandedRowModel, getGroupedRowModel } = tanstack;
type ColumnDef<T> = import("@tanstack/react-table").ColumnDef<T, unknown>;
type TanstackTable<T> = import("@tanstack/react-table").Table<T>;
const { DataTable, DataTableColumnHeader, DataTableFacetedFilter, DataTablePagination, DataTableToolbar, DataTableViewOptions, useDataTable } = await import("../src/components/ui/data-table.tsx");
const { Checkbox } = await import("../src/components/ui/checkbox.tsx");
type UseDataTableProps<T> = import("../src/components/ui/data-table.tsx").UseDataTableProps<T>;
const { resetAnimationFrameScheduler } = await import("@base-ui/utils/useAnimationFrame");
beforeEach(() => resetAnimationFrameScheduler());
afterEach(() => { cleanup(); resetAnimationFrameScheduler(); });
// Base UI's and framer-motion's frame loops are process-wide; let each test's frames finish.
function test(name: string, ...args: [() => unknown] | [{ todo: string }, () => unknown]) {
  const fn = args.length === 1 ? args[0] : args[1];
  const options = args.length === 1 ? {} : args[0];
  return nodeTest(name, options, async () => {
    try { await fn(); } finally { await waitFor(() => assert.equal(pendingFrameCount(), 0)); }
  });
}

type Node = { id: string; name: string; state: string; gpus: number; zone: string };
const nodes: Node[] = Array.from({ length: 12 }, (_, i) => ({
  id: `n${i + 1}`,
  name: `node-${String(i + 1).padStart(2, "0")}`,
  state: ["ready", "busy", "down"][i % 3],
  gpus: (i % 4) * 2,
  zone: i < 6 ? "east" : "west",
}));

const nameColumn: ColumnDef<Node> = { accessorKey: "name", header: ({ column }) => <DataTableColumnHeader column={column} label="Name" />, meta: { label: "Name" } };
const stateColumn: ColumnDef<Node> = { accessorKey: "state", header: ({ column }) => <DataTableColumnHeader column={column} label="State" />, meta: { label: "State" } };
const gpuColumn: ColumnDef<Node> = { accessorKey: "gpus", header: "GPUs", meta: { label: "GPUs" } };
const selectColumn: ColumnDef<Node> = {
  id: "select",
  header: ({ table }) => <Checkbox aria-label="Select all" checked={table.getIsAllPageRowsSelected()} onCheckedChange={(value) => table.toggleAllPageRowsSelected(value === true)} />,
  cell: ({ row }) => <Checkbox aria-label={`Select ${row.original.name}`} checked={row.getIsSelected()} onCheckedChange={(value) => row.toggleSelected(value === true)} />,
  enableSorting: false,
  enableHiding: false,
};
const defaultColumns = [nameColumn, stateColumn, gpuColumn];

type HarnessProps = Omit<Partial<React.ComponentProps<typeof DataTable<Node>>>, "table"> & {
  data?: Node[];
  columns?: ColumnDef<Node>[];
  options?: Partial<UseDataTableProps<Node>>;
  toolbar?: (table: TanstackTable<Node>) => React.ReactNode;
  expose?: (table: TanstackTable<Node>) => void;
};
function Harness({ data = nodes, columns = defaultColumns, options, toolbar, expose, ...props }: HarnessProps) {
  const { table } = useDataTable<Node>({ data, columns, getRowId: (row) => row.id, ...options });
  expose?.(table);
  return <DataTable<Node> table={table} aria-label="Nodes" {...props}>{toolbar?.(table)}</DataTable>;
}
const bodyRows = () => Array.from(document.querySelectorAll<HTMLElement>("[data-slot=table-body] > tr"));
const firstCells = () => bodyRows().map((row) => row.querySelector("td")?.textContent);
const cellsOf = (index: number) => Array.from(bodyRows()[index].querySelectorAll("td")).map((cell) => cell.textContent);

// ── useDataTable + DataTablePagination ─────────────────────────────

test("the table shows the first page of rows and pages through them", async () => {
  render(<Harness />);
  assert.deepEqual(screen.getAllByRole("columnheader").map((cell) => cell.textContent), ["Name", "State", "GPUs"]);
  assert.equal(bodyRows().length, 10, "the default page size is 10");
  assert.ok(screen.getByText("Page 1 of 2"));
  const first = screen.getByRole("button", { name: "Go to first page" }) as HTMLButtonElement;
  const previous = screen.getByRole("button", { name: "Go to previous page" }) as HTMLButtonElement;
  const next = screen.getByRole("button", { name: "Go to next page" }) as HTMLButtonElement;
  const last = screen.getByRole("button", { name: "Go to last page" }) as HTMLButtonElement;
  assert.equal(first.disabled && previous.disabled, true);
  const user = userEvent.setup();
  await user.click(next);
  assert.ok(screen.getByText("Page 2 of 2"));
  assert.deepEqual(firstCells(), ["node-11", "node-12"]);
  assert.equal(next.disabled && last.disabled, true);
  await user.click(first);
  assert.ok(screen.getByText("Page 1 of 2"));
  await user.click(last);
  assert.ok(screen.getByText("Page 2 of 2"));
  await user.click(previous);
  assert.ok(screen.getByText("Page 1 of 2"));
});

test("rows per page changes the page size", async () => {
  render(<Harness />);
  const user = userEvent.setup();
  const select = screen.getByRole("combobox", { name: "Rows per page" });
  assert.equal(select.textContent, "10");
  await user.click(select);
  const options = (await screen.findAllByRole("option")).map((option) => option.textContent);
  assert.deepEqual(options, ["10", "20", "30", "40", "50"]);
  await user.click(screen.getByRole("option", { name: "20" }));
  await waitFor(() => assert.equal(bodyRows().length, 12));
  assert.ok(screen.getByText("Page 1 of 1"));
});

test("a non-standard current page size is offered alongside the standard ones", async () => {
  render(<Harness options={{ initialState: { pagination: { pageIndex: 0, pageSize: 5 } } }} />);
  assert.equal(bodyRows().length, 5);
  const user = userEvent.setup();
  const select = screen.getByRole("combobox", { name: "Rows per page" });
  assert.equal(select.textContent, "5");
  await user.click(select);
  const options = (await screen.findAllByRole("option")).map((option) => option.textContent);
  assert.deepEqual(options, ["5", "10", "20", "30", "40", "50"]);
});

test("switching from a non-standard page size to a standard one keeps the chosen size", { todo: "Zeron bug: DataTablePagination drops the non-standard size from its options as soon as the new size applies; Base UI then reports the open select's value as null, Select maps it to \"\", and setPageSize(Number(\"\")) leaves the table at 1 row per page instead of 20" }, async () => {
  render(<Harness options={{ initialState: { pagination: { pageIndex: 0, pageSize: 5 } } }} />);
  const user = userEvent.setup();
  await user.click(screen.getByRole("combobox", { name: "Rows per page" }));
  await user.click(await screen.findByRole("option", { name: "20" }));
  await new Promise((resolve) => setTimeout(resolve, 500));
  assert.equal(bodyRows().length, 12);
});

test("pagination labels, page size options and a disabled state come from its props", () => {
  render(<Harness paginationProps={{ disabled: true, pageSizeOptions: [10, 25], labels: { nextPage: "Next", pageSummary: (page, count) => `${page}/${count}`, rowsPerPage: "Per page" } }} />);
  assert.ok(screen.getByText("1/2"));
  assert.equal((screen.getByRole("button", { name: "Next" }) as HTMLButtonElement).disabled, true);
  assert.ok(screen.getByRole("combobox", { name: "Per page" }));
  assert.equal((screen.getByRole("combobox", { name: "Per page" }) as HTMLButtonElement).disabled, true);
});

test("an empty table still reports one page", () => {
  render(<Harness data={[]} />);
  assert.ok(screen.getByText("Page 1 of 1"));
});

test("DataTablePagination can be rendered standalone for an app-owned table", async () => {
  function Standalone() {
    const { table } = useDataTable<Node>({ data: nodes, columns: defaultColumns });
    return <><span data-testid="page">{table.getState().pagination.pageIndex}</span><DataTablePagination table={table} className="footer" /></>;
  }
  render(<Standalone />);
  await userEvent.setup().click(screen.getByRole("button", { name: "Go to next page" }));
  assert.equal(screen.getByTestId("page").textContent, "1");
  assert.ok(document.querySelector("[data-slot=data-table-pagination]")!.classList.contains("footer"));
});

// ── DataTableColumnHeader ──────────────────────────────────────────

test("a sortable header opens a menu that sorts ascending, descending and resets", async () => {
  render(<Harness options={{ initialState: { pagination: { pageIndex: 0, pageSize: 20 } } }} />);
  const user = userEvent.setup();
  await user.click(screen.getByRole("button", { name: "State" }));
  assert.equal(screen.queryByRole("menuitem", { name: "Reset sorting" }) === null, true, "nothing to reset before sorting");
  await user.click(await screen.findByRole("menuitemradio", { name: "Descending" }));
  await waitFor(() => assert.equal(cellsOf(0)[1], "ready"));
  assert.equal(cellsOf(11)[1], "busy");
  await user.click(screen.getByRole("button", { name: "State" }));
  const ascending = await screen.findByRole("menuitemradio", { name: "Ascending" });
  assert.equal(screen.getByRole("menuitemradio", { name: "Descending" }).getAttribute("aria-checked"), "true");
  await user.click(ascending);
  await waitFor(() => assert.equal(cellsOf(0)[1], "busy"));
  await user.click(screen.getByRole("button", { name: "State" }));
  await user.click(await screen.findByRole("menuitem", { name: "Reset sorting" }));
  await waitFor(() => assert.deepEqual(firstCells().slice(0, 2), ["node-01", "node-02"]));
});

test("the header menu hides its column and accepts localized labels", async () => {
  const labels = { ascending: "Up", descending: "Down", resetSorting: "Clear", hideColumn: "Hide it" };
  const columns: ColumnDef<Node>[] = [
    { accessorKey: "name", header: ({ column }) => <DataTableColumnHeader column={column} label="Name" labels={labels} /> },
    stateColumn,
  ];
  render(<Harness columns={columns} options={{ initialState: { sorting: [{ id: "name", desc: false }] } }} />);
  const user = userEvent.setup();
  await user.click(screen.getByRole("button", { name: "Name" }));
  assert.ok(await screen.findByRole("menuitemradio", { name: "Up" }));
  assert.ok(screen.getByRole("menuitemradio", { name: "Down" }));
  assert.ok(screen.getByRole("menuitem", { name: "Clear" }));
  await user.click(screen.getByRole("menuitem", { name: "Hide it" }));
  await waitFor(() => assert.deepEqual(screen.getAllByRole("columnheader").map((cell) => cell.textContent), ["State"]));
});

test("a header that can neither sort nor hide is plain text; hide-only and sort-only menus show their items", async () => {
  const columns: ColumnDef<Node>[] = [
    { accessorKey: "name", enableSorting: false, enableHiding: false, header: ({ column }) => <DataTableColumnHeader column={column} label="Name" /> },
    { accessorKey: "state", enableSorting: false, header: ({ column }) => <DataTableColumnHeader column={column} label="State" /> },
    { accessorKey: "gpus", enableHiding: false, header: ({ column }) => <DataTableColumnHeader column={column} label="GPUs" /> },
  ];
  render(<Harness columns={columns} />);
  assert.equal(screen.queryByRole("button", { name: "Name" }) === null, true);
  assert.ok(screen.getByText("Name"));
  const user = userEvent.setup();
  await user.click(screen.getByRole("button", { name: "State" }));
  await screen.findByRole("menuitem", { name: "Hide column" });
  assert.equal(screen.queryByRole("menuitemradio") === null, true);
  await user.keyboard("{Escape}");
  await waitFor(() => assert.equal(screen.queryByRole("menu") === null, true));
  await user.click(screen.getByRole("button", { name: "GPUs" }));
  await screen.findByRole("menuitemradio", { name: "Ascending" });
  assert.equal(screen.queryByRole("menuitem", { name: "Hide column" }) === null, true);
});

// ── useDataTable state ownership ───────────────────────────────────

test("controlled state slices stay with the caller while change callbacks still fire", async () => {
  const calls: string[] = [];
  const record = (name: string) => () => calls.push(name);
  let table!: TanstackTable<Node>;
  render(<Harness expose={(t) => { table = t; }} options={{
    state: { sorting: [], pagination: { pageIndex: 0, pageSize: 10 }, columnFilters: [], columnOrder: [], columnPinning: {}, columnVisibility: {}, rowSelection: {} },
    onSortingChange: record("sorting"), onPaginationChange: record("pagination"), onColumnFiltersChange: record("filters"),
    onColumnOrderChange: record("order"), onColumnPinningChange: record("pinning"), onColumnVisibilityChange: record("visibility"), onRowSelectionChange: record("selection"),
  }} />);
  act(() => {
    table.setSorting([{ id: "name", desc: true }]);
    table.setPageIndex(1);
    table.setColumnFilters([{ id: "state", value: "down" }]);
    table.setColumnOrder(["gpus"]);
    table.setColumnPinning({ left: ["gpus"] });
    table.setColumnVisibility({ state: false });
    table.setRowSelection({ n1: true });
  });
  assert.deepEqual(calls, ["sorting", "pagination", "filters", "order", "pinning", "visibility", "selection"]);
  assert.deepEqual(firstCells().slice(0, 2), ["node-01", "node-02"], "the controlled state did not change");
  assert.equal(screen.getAllByRole("columnheader").length, 3);
  assert.ok(screen.getByText("Page 1 of 2"));
});

test("uncontrolled state slices update internally and still notify the caller", () => {
  const calls: string[] = [];
  const record = (name: string) => () => calls.push(name);
  let table!: TanstackTable<Node>;
  render(<Harness expose={(t) => { table = t; }} options={{
    initialState: { columnVisibility: { gpus: false }, rowSelection: { n2: true }, columnOrder: [], columnPinning: {}, columnFilters: [], sorting: [] },
    onSortingChange: record("sorting"), onColumnOrderChange: record("order"), onColumnPinningChange: record("pinning"), onColumnVisibilityChange: record("visibility"), onRowSelectionChange: record("selection"), onColumnFiltersChange: record("filters"), onPaginationChange: record("pagination"),
  }} />);
  assert.equal(screen.getAllByRole("columnheader").length, 2, "initial visibility applies");
  act(() => { table.setColumnOrder(["state", "name", "gpus"]); });
  assert.deepEqual(screen.getAllByRole("columnheader").map((cell) => cell.textContent), ["State", "Name"]);
  act(() => { table.setColumnVisibility({}); });
  assert.equal(screen.getAllByRole("columnheader").length, 3);
  act(() => { table.setSorting([{ id: "name", desc: true }]); });
  assert.equal(cellsOf(0)[1], "node-12");
  act(() => { table.setColumnFilters([{ id: "state", value: "down" }]); });
  assert.equal(bodyRows().length, 4);
  act(() => { table.setPageSize(2); });
  assert.equal(bodyRows().length, 2);
  act(() => { table.setColumnPinning({ left: ["name"] }); table.setRowSelection({}); });
  assert.deepEqual(calls, ["order", "visibility", "sorting", "filters", "pagination", "pinning", "selection"]);
});

// ── Selection, action bar ──────────────────────────────────────────

test("a select column shows the selection summary, marks selected rows and reveals the action bar", async () => {
  render(<Harness columns={[selectColumn, ...defaultColumns]} actionBar={<div role="toolbar" aria-label="Bulk actions">Delete</div>} />);
  assert.ok(screen.getByText("0 of 12 row(s) selected."));
  assert.equal(screen.queryByRole("toolbar", { name: "Bulk actions" }) === null, true);
  const user = userEvent.setup();
  await user.click(screen.getByRole("checkbox", { name: "Select node-02" }));
  assert.ok(screen.getByText("1 of 12 row(s) selected."));
  assert.equal(bodyRows()[1].getAttribute("data-state"), "selected");
  assert.ok(screen.getByRole("toolbar", { name: "Bulk actions" }));
  await user.click(screen.getByRole("checkbox", { name: "Select all" }));
  assert.ok(screen.getByText("10 of 12 row(s) selected."));
});

test("the action bar is shown even without pagination, and the selection label is localizable", async () => {
  render(<Harness columns={[selectColumn, ...defaultColumns]} showPagination={false} actionBar={<span>Bulk</span>} options={{ initialState: { rowSelection: { n1: true } } }} />);
  assert.ok(screen.getByText("Bulk"));
  assert.equal(screen.queryByRole("button", { name: "Go to next page" }) === null, true);
  cleanup();
  render(<Harness columns={[selectColumn, ...defaultColumns]} paginationProps={{ labels: { selectedRows: (n, total) => `${n}/${total} chosen` } }} />);
  assert.ok(screen.getByText("0/12 chosen"));
});

// ── Empty and loading states ──────────────────────────────────────

test("an empty table shows the built-in empty state with a custom title", () => {
  render(<Harness data={[]} emptyMessage="No nodes yet" />);
  assert.ok(screen.getByText("No nodes yet"));
  assert.equal(screen.queryByRole("button", { name: "Clear filters" }) === null, true, "no filters to clear");
  assert.equal(document.querySelector("[data-slot=empty]")?.getAttribute("data-reason"), "no-data");
});

test("filters that match nothing explain it and offer to clear them", async () => {
  let table!: TanstackTable<Node>;
  render(<Harness expose={(t) => { table = t; }} />);
  act(() => { table.setColumnFilters([{ id: "name", value: "nothing-matches" }]); });
  const empty = document.querySelector<HTMLElement>("[data-slot=empty]")!;
  assert.equal(empty.getAttribute("role"), "status", "a filtered-empty result is announced");
  assert.equal(empty.getAttribute("data-reason"), "no-filter-results");
  assert.ok(within(empty).getByText("No results."));
  assert.ok(screen.getByText(/Try adjusting your search/));
  await userEvent.setup().click(screen.getByRole("button", { name: "Clear filters" }));
  assert.equal(bodyRows().length, 10);
});

test("a custom empty state replaces the built-in one", () => {
  render(<Harness data={[]} emptyState={<p>Connect a node to begin</p>} />);
  assert.ok(screen.getByText("Connect a node to begin"));
  assert.equal(document.querySelector("[data-slot=empty]") === null, true);
});

test("loading replaces rows with page-sized skeleton rows and announces it", () => {
  const { rerender } = render(<Harness isLoading options={{ initialState: { pagination: { pageIndex: 0, pageSize: 4 } } }} actionBar={<span>Bulk</span>} />);
  assert.equal(screen.getAllByRole("status").some((el) => el.textContent === "Loading data."), true);
  const scroll = document.querySelector("[data-slot=data-table-scroll-area]")!.parentElement!;
  assert.equal(scroll.getAttribute("aria-busy"), "true");
  const skeleton = document.querySelector("[data-slot=table-skeleton-body]")!;
  assert.equal(skeleton.querySelectorAll("tr").length, 4);
  assert.equal(skeleton.querySelectorAll("tr")[0].querySelectorAll("td").length, 3);
  assert.equal(screen.queryByText("node-01") === null, true);
  rerender(<Harness isLoading loadingRowCount={2} loadingMessage="Fetching nodes" aria-busy={false} renderLoadingCell={({ columnIndex }) => columnIndex === 2 ? <span>…</span> : undefined} />);
  const body = document.querySelector("[data-slot=table-skeleton-body]")!;
  assert.equal(body.querySelectorAll("tr").length, 2);
  assert.equal(body.querySelectorAll("tr")[0].querySelectorAll("td")[2].textContent, "…");
  assert.ok(screen.getAllByRole("status").some((el) => el.textContent === "Fetching nodes"));
  assert.equal(document.querySelector("[data-slot=data-table-scroll-area]")!.parentElement!.getAttribute("aria-busy"), "false");
});

test("a loading table with every column hidden keeps one placeholder column", () => {
  render(<Harness isLoading density="compact" options={{ initialState: { columnVisibility: { name: false, state: false, gpus: false } } }} />);
  const body = document.querySelector("[data-slot=table-skeleton-body]")!;
  assert.equal(body.querySelectorAll("tr")[0].querySelectorAll("td").length, 1);
});

// ── Row activation, active row, row header, expanded rows ─────────

test("rows activate by click, Enter and Space, but not from nested controls", async () => {
  const activated: string[] = [];
  const columns: ColumnDef<Node>[] = [nameColumn, { id: "actions", cell: ({ row }) => <button>Restart {row.original.name}</button> }];
  render(<Harness columns={columns} onRowActivate={(row) => activated.push(row.id)} activeRowId="n3" />);
  const user = userEvent.setup();
  await user.click(screen.getByText("node-02"));
  assert.deepEqual(activated, ["n2"]);
  await user.click(screen.getByRole("button", { name: "Restart node-02" }));
  assert.deepEqual(activated, ["n2"], "a nested button keeps its own click");
  const row = bodyRows()[3];
  assert.equal(row.tabIndex, 0);
  row.focus();
  await user.keyboard("{Enter}");
  await user.keyboard(" ");
  await user.keyboard("a");
  assert.deepEqual(activated, ["n2", "n4", "n4"]);
  screen.getByRole("button", { name: "Restart node-01" }).focus();
  await user.keyboard("{Enter}");
  assert.deepEqual(activated, ["n2", "n4", "n4"]);
  assert.equal(bodyRows()[2].getAttribute("aria-selected"), "true", "the active row is marked selected");
  assert.equal(bodyRows()[2].getAttribute("data-state"), "selected");
});

test("rows without an activation handler are not focusable and clicks do not select or activate them", async () => {
  render(<Harness />);
  const row = bodyRows()[0];
  assert.equal(row.hasAttribute("tabindex"), false);
  const user = userEvent.setup();
  await user.click(screen.getByText("node-01"));
  assert.ok(document.activeElement === document.body, "clicking a cell does not move focus onto the row");
  row.focus();
  assert.ok(document.activeElement !== row, "the row cannot take focus");
  fireEvent.keyDown(row, { key: "Enter" });
  fireEvent.keyDown(row, { key: " " });
  for (const each of bodyRows()) {
    assert.equal(each.hasAttribute("aria-selected"), false);
    assert.equal(each.hasAttribute("data-state"), false);
  }
});

test("row headers introduce records and expanded rows show detail content", async () => {
  render(<Harness options={{ getRowCanExpand: () => true, getExpandedRowModel: getExpandedRowModel(), initialState: { expanded: { n1: true } } }}
    renderRowHeader={({ row, previousRow }) => row.original.zone !== previousRow?.original.zone ? <strong>Zone {row.original.zone}</strong> : row.index === 1 ? false : null}
    renderExpandedRow={({ row }) => row.id === "n2" ? null : <p>Details of {row.original.name}</p>} />);
  const headers = Array.from(document.querySelectorAll("[data-slot=data-table-row-header]")).map((row) => row.textContent);
  assert.deepEqual(headers, ["Zone east", "Zone west"]);
  assert.ok(screen.getByText("Details of node-01"));
  const record = document.querySelector("tr[data-row-id=n1][data-slot=table-row]")!;
  assert.equal(record.getAttribute("aria-expanded"), "true");
  assert.equal(document.querySelector("tr[data-row-id=n2][data-slot=table-row]")!.getAttribute("aria-expanded"), "false");
});

// ── Grouping ───────────────────────────────────────────────────────

const zoneColumn: ColumnDef<Node> = { accessorKey: "zone", header: "Zone" };
const groupedColumns = [...defaultColumns, zoneColumn];
const groupedOptions: Partial<UseDataTableProps<Node>> = {
  getGroupedRowModel: getGroupedRowModel(),
  getExpandedRowModel: getExpandedRowModel(),
  initialState: { grouping: ["zone"], expanded: true, pagination: { pageIndex: 0, pageSize: 50 } },
};

test("grouped rows render collapsible group headers with record counts", async () => {
  render(<Harness columns={groupedColumns} options={groupedOptions} />);
  const groups = Array.from(document.querySelectorAll<HTMLElement>("[data-slot=data-table-group]"));
  assert.deepEqual(groups.map((group) => group.textContent), ["east6", "west6"]);
  const user = userEvent.setup();
  const collapse = screen.getByRole("button", { name: "Collapse east" });
  assert.equal(collapse.getAttribute("aria-expanded"), "true");
  await user.click(collapse);
  const expand = screen.getByRole("button", { name: "Expand east" });
  assert.equal(expand.getAttribute("aria-expanded"), "false");
  assert.equal(screen.queryByText("node-01") === null, true);
  assert.ok(screen.getByText("node-07"));
});

test("grouping options order groups, limit visible records, label them and add headers and footers", async () => {
  render(<Harness columns={groupedColumns} options={groupedOptions} grouping={{
    getLabel: (row) => `Zone ${String(row.groupingValue).toUpperCase()}`,
    getOrder: (row) => (row.groupingValue === "west" ? 0 : 1),
    getRowLimit: (row) => (row.groupingValue === "west" ? 2.7 : undefined),
    renderHeader: ({ row, leafRowCount, visibleRowCount }) => <span>{String(row.groupingValue)}: {visibleRowCount} of {leafRowCount}</span>,
    renderFooter: ({ row, leafRowCount, visibleRowCount }) => row.groupingValue === "east" ? false : <span>Showing {visibleRowCount} of {leafRowCount}</span>,
    labels: { expandGroup: (label) => `Open ${label}`, collapseGroup: (label) => `Close ${label}` },
  }} />);
  const groups = Array.from(document.querySelectorAll<HTMLElement>("[data-slot=data-table-group]")).map((group) => group.textContent);
  assert.deepEqual(groups, ["west: 2 of 6", "east: 6 of 6"]);
  assert.deepEqual(Array.from(document.querySelectorAll("[data-slot=data-table-group-footer]")).map((row) => row.textContent), ["Showing 2 of 6"]);
  assert.ok(screen.getByText("node-07"));
  assert.equal(screen.queryByText("node-09") === null, true, "rows past the limit are not shown");
  await userEvent.setup().click(screen.getByRole("button", { name: "Close Zone WEST" }));
  assert.ok(screen.getByRole("button", { name: "Open Zone WEST" }));
  assert.equal(document.querySelectorAll("[data-slot=data-table-group-footer]").length, 0, "a collapsed group has no footer");
});

test("ordinary sub-rows inside a group follow their parent row, before the parent's next sibling", () => {
  type Tree = Node & { children?: Tree[] };
  const data: Tree[] = [
    { ...nodes[0], children: [{ ...nodes[1], id: "n1-child", zone: "east" }] },
    nodes[2],
    nodes[6],
  ];
  function TreeHarness() {
    const { table } = useDataTable<Tree>({ data, columns: groupedColumns as ColumnDef<Tree>[], getRowId: (row) => row.id, getSubRows: (row) => row.children,
      getGroupedRowModel: getGroupedRowModel(), getExpandedRowModel: getExpandedRowModel(), initialState: { grouping: ["zone"], expanded: true } });
    return <DataTable table={table} grouping={{ getRowLimit: () => Number.POSITIVE_INFINITY }} />;
  }
  render(<TreeHarness />);
  // The body carries no depth marker, so hierarchy shows as order: group, parent, its child, then the parent's sibling.
  const order = bodyRows().map((row) => row.getAttribute("data-slot") === "data-table-group" ? `group:${row.textContent}` : row.getAttribute("data-row-id"));
  assert.deepEqual(order, ["group:east3", "n1", "n1-child", "n3", "group:west1", "n7"]);
});

// ── Layout: pinning, sticky header, variants ──────────────────────

test("pinned columns stick to their edge and cast a shadow once content scrolls under them", () => {
  render(<Harness variant="embedded" style={{ backgroundColor: "rgb(1, 2, 3)" }} options={{ initialState: { columnPinning: { left: ["name"], right: ["gpus"] } } }} />);
  const [name, state, gpus] = screen.getAllByRole("columnheader");
  assert.equal(name.style.position, "sticky");
  assert.equal(name.style.left, "0px");
  assert.equal(gpus.style.position, "sticky");
  assert.equal(gpus.style.right, "0px");
  assert.equal(state.style.position, "relative");
  assert.equal(name.style.backgroundColor, "rgb(1, 2, 3)", "embedded tables color pinned cells with their background");
  assert.equal(name.style.boxShadow, "");
  const scroll = document.querySelector<HTMLElement>("[data-slot=data-table-scroll-area]")!;
  Object.defineProperties(scroll, { scrollWidth: { configurable: true, value: 1000 }, clientWidth: { configurable: true, value: 400 }, scrollLeft: { configurable: true, writable: true, value: 300 } });
  fireEvent.scroll(scroll);
  assert.match(screen.getAllByRole("columnheader")[0].style.boxShadow, /-4px 0 4px -4px/);
  assert.match(screen.getAllByRole("columnheader")[2].style.boxShadow, /4px 0 4px -4px/);
  fireEvent.scroll(scroll);
  assert.match(screen.getAllByRole("columnheader")[0].style.boxShadow, /-4px/, "an unchanged scroll keeps the shadows");
  scroll.scrollLeft = 0;
  fireEvent.scroll(scroll);
  assert.equal(screen.getAllByRole("columnheader")[0].style.boxShadow, "");
  // Pinned skeleton cells use the same styles.
  cleanup();
  render(<Harness isLoading options={{ initialState: { columnPinning: { left: ["name"] } } }} />);
  assert.equal(document.querySelector<HTMLElement>("[data-slot=table-skeleton-body] td")!.style.position, "sticky");
});

test("selected rows tint their pinned cells", async () => {
  render(<Harness columns={[selectColumn, ...defaultColumns]} options={{ initialState: { columnPinning: { left: ["name"] }, rowSelection: { n1: true } } }} />);
  const pinned = Array.from(bodyRows()[0].querySelectorAll<HTMLElement>("td")).find((cell) => cell.style.position === "sticky")!;
  assert.match(pinned.style.backgroundImage, /var\(--selection\)/);
});

test("a sticky header bounds the scroll area, and an explicit maximum height wins", () => {
  const { rerender } = render(<Harness stickyHeader />);
  const scroll = () => document.querySelector<HTMLElement>("[data-slot=data-table-scroll-area]")!;
  assert.equal(scroll().style.maxHeight, "min(65dvh, 40rem)");
  assert.equal((document.querySelector("[data-slot=table-header]") as HTMLElement).style.backgroundColor, "var(--surface-floating)");
  rerender(<Harness stickyHeader scrollAreaMaxHeight="300px" />);
  assert.equal(scroll().style.maxHeight, "300px");
  rerender(<Harness />);
  assert.equal(scroll().style.maxHeight, "");
});

test("the table reports its variant and density and falls back to window resize without ResizeObserver", () => {
  const RO = globalThis.ResizeObserver;
  Object.defineProperty(globalThis, "ResizeObserver", { value: undefined, configurable: true });
  try {
    const { unmount } = render(<Harness density="compact" variant="embedded" />);
    const root = document.querySelector("[data-slot=data-table]")!;
    assert.equal(root.getAttribute("data-density"), "compact");
    assert.equal(root.getAttribute("data-variant"), "embedded");
    act(() => { dom.window.dispatchEvent(new dom.window.Event("resize")); });
    unmount();
  } finally {
    Object.defineProperty(globalThis, "ResizeObserver", { value: RO, configurable: true });
  }
});

// ── Toolbar and faceted filters ───────────────────────────────────

const filterColumns: ColumnDef<Node>[] = [
  { accessorKey: "name", meta: { label: "Name", variant: "text", placeholder: "Search names" } },
  { accessorKey: "state", filterFn: "arrIncludesSome", meta: { label: "State", variant: "multiSelect", options: [{ label: "Ready", value: "ready" }, { label: "Busy", value: "busy" }, { label: "Down", value: "down" }] } },
  { accessorKey: "zone", filterFn: "arrIncludesSome", meta: { label: "Zone", variant: "select", options: [{ label: "East", value: "east", count: 99 }, { label: "West", value: "west" }] } },
  { accessorKey: "gpus", meta: { label: "GPUs", variant: "number", unit: "cards" } },
];
const withToolbar = (props: Partial<React.ComponentProps<typeof DataTableToolbar<Node>>> = {}) => (table: TanstackTable<Node>) => <DataTableToolbar table={table} {...props} />;

test("the toolbar text filter narrows rows and Reset clears every filter", async () => {
  render(<Harness columns={filterColumns} toolbar={withToolbar()} />);
  const user = userEvent.setup();
  await user.type(screen.getByPlaceholderText("Search names"), "node-1");
  assert.deepEqual(firstCells(), ["node-10", "node-11", "node-12"]);
  await user.click(screen.getByRole("button", { name: "Reset filters" }));
  assert.equal(bodyRows().length, 10);
  assert.equal((screen.getByPlaceholderText("Search names") as HTMLInputElement).value, "");
  assert.equal(screen.queryByRole("button", { name: "Reset filters" }) === null, true);
});

test("the number filter matches an exact value, shows its unit, and clears when emptied", async () => {
  render(<Harness columns={filterColumns} toolbar={withToolbar()} />);
  assert.ok(screen.getByText("cards"));
  const input = screen.getByPlaceholderText("GPUs") as HTMLInputElement;
  fireEvent.change(input, { target: { value: "6" } });
  assert.equal(input.value, "6");
  assert.deepEqual(firstCells(), ["node-04", "node-08", "node-12"]);
  fireEvent.change(input, { target: { value: "" } });
  assert.equal(bodyRows().length, 10);
});

test("a number filter with a custom filter function stores the raw value", () => {
  let table!: TanstackTable<Node>;
  render(<Harness expose={(t) => { table = t; }} columns={[{ accessorKey: "gpus", filterFn: (row, id, value) => String(row.getValue(id)) === value, meta: { variant: "number" } }]} toolbar={withToolbar({ showViewOptions: false })} />);
  fireEvent.change(screen.getByPlaceholderText("gpus"), { target: { value: "4" } });
  assert.equal(table.getColumn("gpus")!.getFilterValue(), "4");
  assert.equal(bodyRows().length, 3);
});

test("a multi-select facet toggles values, summarizes them on the trigger and clears", async () => {
  render(<Harness columns={filterColumns} toolbar={withToolbar()} />);
  const user = userEvent.setup();
  await user.click(screen.getByRole("button", { name: "State" }));
  const group = await screen.findByRole("group", { name: "State" });
  const counts = Array.from(group.querySelectorAll("[data-slot=checkbox-group-trailing]")).map((el) => el.textContent);
  assert.deepEqual(counts, ["4", "4", "4"], "counts come from the column's facets");
  await user.click(within(group).getByRole("checkbox", { name: "Ready" }));
  await user.click(within(group).getByRole("checkbox", { name: "Busy" }));
  assert.ok(screen.getByRole("button", { name: "State: Ready, Busy" }));
  assert.equal(bodyRows().length, 8);
  await user.click(within(group).getByRole("checkbox", { name: "Down" }));
  assert.ok(screen.getByRole("button", { name: "State: Ready, Busy, Down" }).textContent?.includes("+1"));
  await user.click(within(group).getByRole("checkbox", { name: "Down" }));
  await user.click(within(group).getByRole("checkbox", { name: "Busy" }));
  await user.click(within(group).getByRole("checkbox", { name: "Ready" }));
  assert.ok(screen.getByRole("button", { name: "State" }), "unchecking the last value removes the filter");
  await user.click(within(group).getByRole("checkbox", { name: "Down" }));
  await user.click(screen.getByRole("button", { name: "Clear filters" }));
  await waitFor(() => assert.equal(screen.queryByRole("group", { name: "State" }) === null, true));
  assert.equal(bodyRows().length, 10);
});

test("a single-select facet picks one value, closes, and picking it again clears it", async () => {
  render(<Harness columns={filterColumns} toolbar={withToolbar()} />);
  const user = userEvent.setup();
  await user.click(screen.getByRole("button", { name: "Zone" }));
  const group = await screen.findByRole("group", { name: "Zone" });
  assert.deepEqual(Array.from(group.querySelectorAll("[data-slot=checkbox-group-trailing]")).map((el) => el.textContent), ["99", "6"], "an authoritative count wins over facets");
  await user.click(within(group).getByRole("checkbox", { name: "West" }));
  await waitFor(() => assert.equal(screen.queryByRole("group", { name: "Zone" }) === null, true));
  assert.deepEqual(firstCells().slice(0, 1), ["node-07"]);
  await user.click(screen.getByRole("button", { name: "Zone: West" }));
  await user.click(within(await screen.findByRole("group", { name: "Zone" })).getByRole("checkbox", { name: "West" }));
  await waitFor(() => assert.equal(bodyRows().length, 10));
});

test("a standalone faceted filter accepts a semantic icon, localized clear label and disabled state", async () => {
  const Icon = (props: { className?: string }) => <svg data-testid="status-dot" className={props.className} />;
  let table!: TanstackTable<Node>;
  function Faceted({ disabled }: { disabled?: boolean }) {
    const { table: t } = useDataTable<Node>({ data: nodes, columns: defaultColumns, initialState: { columnFilters: [{ id: "state", value: ["ready", "ghost"] }] } });
    table = t;
    return <DataTableFacetedFilter column={t.getColumn("state")!} title="Status" icon={Icon} disabled={disabled} labels={{ clearFilters: "Show all" }}
      options={[{ label: "Ready", value: "ready", icon: Icon }, { label: "Busy", value: "busy" }]} />;
  }
  const { rerender } = render(<Faceted />);
  const trigger = screen.getByRole("button", { name: "Status: Ready" });
  assert.ok(within(trigger).getByTestId("status-dot"));
  const user = userEvent.setup();
  await user.click(trigger);
  await user.click(await screen.findByRole("button", { name: "Show all" }));
  assert.equal(table.getColumn("state")!.getFilterValue(), undefined);
  rerender(<Faceted disabled />);
  assert.equal((screen.getByRole("button", { name: /Status/ }) as HTMLButtonElement).disabled, true);
});

test("the toolbar hosts custom actions and can hide column options", () => {
  const { rerender } = render(<Harness columns={filterColumns} toolbar={withToolbar({ children: <button>Export</button> })} />);
  assert.ok(screen.getByRole("toolbar"));
  assert.ok(screen.getByRole("button", { name: "Export" }));
  assert.ok(screen.getByRole("button", { name: "Customize columns" }));
  rerender(<Harness columns={filterColumns} toolbar={withToolbar({ showViewOptions: false })} />);
  assert.equal(screen.queryByRole("button", { name: "Customize columns" }) === null, true);
});

// ── Column view options ───────────────────────────────────────────

test("column options toggle visibility and reorder columns from the keyboard", async () => {
  render(<Harness columns={[selectColumn, ...defaultColumns]} toolbar={withToolbar()} />);
  const user = userEvent.setup();
  await user.click(screen.getByRole("button", { name: "Customize columns" }));
  assert.ok(await screen.findByText("Drag to reorder columns"));
  const list = screen.getByRole("list");
  assert.deepEqual(within(list).getAllByRole("listitem").map((item) => item.textContent), ["Name", "State", "GPUs"], "the utility select column is not listed");
  await user.click(screen.getByRole("checkbox", { name: "Show State column" }));
  await waitFor(() => assert.deepEqual(screen.getAllByRole("columnheader").map((cell) => cell.textContent), ["", "Name", "GPUs"]));
  await user.click(screen.getByRole("checkbox", { name: "Show State column" }));
  await waitFor(() => assert.equal(screen.getAllByRole("columnheader").length, 4));
  const handle = screen.getByRole("button", { name: "Reorder GPUs" });
  handle.focus();
  await user.keyboard(" {ArrowUp}{ArrowUp} ");
  await waitFor(() => assert.deepEqual(screen.getAllByRole("columnheader").map((cell) => cell.textContent), ["", "GPUs", "Name", "State"]), "the select column stays first");
});

test("pinned columns are labelled and cannot be dragged; labels can be localized", async () => {
  function Options() {
    const { table } = useDataTable<Node>({ data: nodes, columns: defaultColumns, initialState: { columnPinning: { left: ["name"] } } });
    return <><DataTableViewOptions table={table} className="opts" labels={{ customizeColumns: "Columns", dragToReorder: "Reorder", reorderAndToggle: "Column list", reorderColumn: (t) => `Move ${t}`, showColumn: (t) => `Toggle ${t}` }} />
      <span data-testid="order">{table.getAllLeafColumns().map((c) => c.id).join(",")}|{table.getState().columnOrder.join(",")}</span></>;
  }
  render(<Options />);
  const user = userEvent.setup();
  await user.click(screen.getByRole("button", { name: "Columns" }));
  assert.ok(await screen.findByText("Reorder"));
  assert.ok(screen.getByText("Pinned"));
  assert.equal((screen.getByRole("button", { name: "Move Name" }) as HTMLButtonElement).disabled, true);
  assert.ok(screen.getByRole("checkbox", { name: "Toggle State" }));
  screen.getByRole("button", { name: "Move GPUs" }).focus();
  await user.keyboard(" {ArrowUp}{ArrowUp} ");
  await waitFor(() => assert.equal(screen.getByTestId("order").textContent, "name,gpus,state|name,gpus,state"), "the pinned column keeps its slot");
});

test("column options render nothing when no column can be hidden", () => {
  function Options() {
    const { table } = useDataTable<Node>({ data: nodes, columns: [{ ...gpuColumn, enableHiding: false }, { id: "display", header: "Display" }] });
    return <DataTableViewOptions table={table} />;
  }
  render(<Options />);
  assert.equal(screen.queryByRole("button", { name: "Customize columns" }) === null, true);
});
