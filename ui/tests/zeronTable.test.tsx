// Zeron Table primitives (ui/src/components/ui/table.tsx): semantic table
// parts, the proximity hover that marks the row under the pointer, and the
// skeleton body that stands in for rows while data loads.
import { dom, pendingFrameCount } from "./helpers/dom.ts";
import assert from "node:assert/strict";
import { afterEach, test as nodeTest } from "node:test";
import React from "react";

// jsdom has no layout. Rows registered for proximity hover (data-proximity-index)
// get a 32px-tall box stacked by index; everything else keeps zero size.
const proto = dom.window.HTMLElement.prototype;
const indexOf = (el: HTMLElement) => Number(el.getAttribute("data-proximity-index"));
const hasBox = (el: HTMLElement) => el.hasAttribute("data-proximity-index");
Object.defineProperties(proto, {
  offsetTop: { configurable: true, get() { return hasBox(this) ? indexOf(this) * 32 : 0; } },
  offsetLeft: { configurable: true, get() { return 0; } },
  offsetHeight: { configurable: true, get() { return hasBox(this) ? 32 : 0; } },
  offsetWidth: { configurable: true, get() { return hasBox(this) ? 200 : 0; } },
  offsetParent: { configurable: true, get() { return hasBox(this) ? this.parentElement : null; } },
});

const { render, screen, fireEvent, waitFor, cleanup, within } = await import("@testing-library/react");
const { Table, TableHeader, TableBody, TableSkeletonBody, TableRow, TableHead, TableCell } = await import("../src/components/ui/table.tsx");
afterEach(cleanup);
// framer-motion's frame loop is process-wide and helpers/dom.ts drops a finished
// test's pending frames, so each test lets its hover animation finish.
const test = (name: string, fn: () => unknown) => nodeTest(name, async () => {
  try { await fn(); } finally { await waitFor(() => assert.equal(pendingFrameCount(), 0)); }
});

const frame = () => new Promise((resolve) => setTimeout(resolve, 40));

function Grid(props: Partial<React.ComponentProps<typeof Table>>) {
  return <Table aria-label="Nodes" {...props}>
    <TableHeader><TableRow><TableHead>Name</TableHead><TableHead>State</TableHead></TableRow></TableHeader>
    <TableBody>
      {["gpu-01", "gpu-02", "gpu-03"].map((name, index) => <TableRow index={index} key={name}><TableCell>{name}</TableCell><TableCell>ready</TableCell></TableRow>)}
    </TableBody>
  </Table>;
}

test("table renders header and body cells with table semantics", () => {
  render(<Grid />);
  const table = screen.getByRole("table", { name: "Nodes" });
  assert.deepEqual(within(table).getAllByRole("columnheader").map((cell) => cell.textContent), ["Name", "State"]);
  const rows = within(table).getAllByRole("row");
  assert.equal(rows.length, 4);
  assert.deepEqual(within(rows[2]).getAllByRole("cell").map((cell) => cell.textContent), ["gpu-02", "ready"]);
});

test("table forwards refs to the table, row and cell elements", () => {
  const tableRef = React.createRef<HTMLTableElement>();
  const rowRef = React.createRef<HTMLTableRowElement>();
  let callbackRow: HTMLTableRowElement | null = null;
  const cellRef = React.createRef<HTMLTableCellElement>();
  render(<Table ref={tableRef}><TableBody>
    <TableRow ref={rowRef} index={0}><TableCell ref={cellRef}>a</TableCell></TableRow>
    <TableRow ref={(node) => { callbackRow = node; }} index={1}><TableCell>b</TableCell></TableRow>
  </TableBody></Table>);
  assert.equal(tableRef.current?.tagName, "TABLE");
  assert.equal(rowRef.current?.textContent, "a");
  assert.equal((callbackRow as HTMLTableRowElement | null)?.textContent, "b");
  assert.equal(cellRef.current?.tagName, "TD");
});

test("the bordered variant frames the table in a horizontally scrollable container", () => {
  const { rerender } = render(<Grid containerClassName="mt-4" />);
  const container = () => screen.getByRole("table").parentElement as HTMLElement;
  assert.equal(container().getAttribute("data-slot"), "table-container");
  assert.ok(container().classList.contains("mt-4"), "containerClassName reaches the wrapper");
  assert.ok(!container().classList.contains("overflow-x-auto"));
  rerender(<Grid variant="bordered" size="sm" />);
  assert.ok(container().classList.contains("overflow-x-auto"), "a bordered table scrolls inside its frame");
  assert.ok(screen.getByRole("table").classList.contains("text-label"), "the small size uses label typography");
  assert.ok(screen.getAllByRole("columnheader")[0].classList.contains("font-semibold"));
});

test("moving the pointer over the body marks the nearest row active until the pointer leaves", async () => {
  render(<Grid />);
  const container = screen.getByRole("table").parentElement as HTMLElement;
  const rows = screen.getAllByRole("row").slice(1);
  await frame();
  fireEvent.mouseEnter(container);
  fireEvent.mouseMove(container, { clientX: 10, clientY: 40 });
  await waitFor(() => assert.ok(rows[1].classList.contains("is-active")));
  assert.ok(!rows[0].classList.contains("is-active"));
  fireEvent.mouseMove(container, { clientX: 10, clientY: 90 });
  await waitFor(() => assert.ok(rows[2].classList.contains("is-active")));
  assert.ok(!rows[1].classList.contains("is-active"));
  fireEvent.mouseLeave(container);
  await waitFor(() => assert.ok(rows.every((row) => !row.classList.contains("is-active"))));
});

test("the hover marker also works when the first row is active", async () => {
  render(<Grid />);
  const container = screen.getByRole("table").parentElement as HTMLElement;
  await frame();
  fireEvent.mouseMove(container, { clientX: 10, clientY: 5 });
  const first = screen.getAllByRole("row")[1];
  await waitFor(() => assert.ok(first.classList.contains("is-active")));
});

test("skeleton body renders the requested grid of decorative placeholders", () => {
  render(<Table><TableSkeletonBody columns={3} rows={2} data-testid="loading" /></Table>);
  const body = screen.getByTestId("loading");
  assert.equal(body.getAttribute("aria-hidden"), "true");
  const rows = body.querySelectorAll("tr");
  assert.equal(rows.length, 2);
  assert.equal(rows[0].querySelectorAll("td").length, 3);
  assert.equal(body.querySelectorAll("[data-slot=skeleton]").length, 6);
  // First, middle and last columns use distinct line widths so the rows read as text.
  const widths = Array.from(rows[0].querySelectorAll("[data-slot=skeleton]")).map((el) => ["w-3/4", "w-2/3", "w-1/2"].find((w) => el.classList.contains(w)));
  assert.deepEqual(widths, ["w-3/4", "w-2/3", "w-1/2"]);
});

test("skeleton body defaults to five rows and clamps invalid counts to one", () => {
  const { rerender } = render(<Table><TableSkeletonBody columns={2} data-testid="loading" /></Table>);
  assert.equal(screen.getByTestId("loading").querySelectorAll("tr").length, 5);
  rerender(<Table><TableSkeletonBody columns={0} rows={-3} data-testid="loading" /></Table>);
  const body = screen.getByTestId("loading");
  assert.equal(body.querySelectorAll("tr").length, 1);
  assert.equal(body.querySelectorAll("td").length, 1);
  rerender(<Table><TableSkeletonBody columns={2.9} rows={2.9} data-testid="loading" /></Table>);
  assert.equal(screen.getByTestId("loading").querySelectorAll("td").length, 4);
});

test("skeleton body applies per-cell props and custom cells, and can be exposed to assistive tech", () => {
  render(<Table><TableSkeletonBody aria-hidden={false} cellClassName="base" columns={2} rows={2} data-testid="loading"
    getCellProps={(row, column) => ({ className: `cell-${row}-${column}`, style: { width: 50 + column } })}
    renderCell={(row, column) => column === 1 ? (row === 0 ? <span>custom</span> : null) : undefined} /></Table>);
  const body = screen.getByTestId("loading");
  assert.equal(body.getAttribute("aria-hidden"), "false");
  const cells = body.querySelectorAll("td");
  assert.ok(cells[1].classList.contains("base") && cells[1].classList.contains("cell-0-1"));
  assert.equal((cells[1] as HTMLElement).style.width, "51px");
  assert.equal(cells[1].textContent, "custom");
  assert.equal(cells[1].querySelector("[data-slot=skeleton]") === null, true, "a custom cell replaces the line");
  assert.equal(cells[3].childNodes.length, 0, "a null custom cell renders empty");
  assert.ok(cells[0].querySelector("[data-slot=skeleton]"), "undefined keeps the default line");
});
