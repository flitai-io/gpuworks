import { createI18n } from "./helpers/dom.ts";
import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import React, { useState } from "react";
import { I18nextProvider } from "react-i18next";

// The resource detail frame: its title row, tabs that mount on first visit and
// stay, and the expandable rows of detail tables.

const { i18n } = await createI18n([]);
const { cleanup, fireEvent, render, screen, waitFor, within } = await import("@testing-library/react");
const userEvent = (await import("@testing-library/user-event")).default;
const { Table } = await import("antd");
const { default: DetailDrawer, DetailExtra, detailExpandable } = await import("../src/components/detail/DetailDrawer.tsx");
afterEach(cleanup);
const mount = (children: React.ReactNode) => render(<I18nextProvider i18n={i18n}>{children}</I18nextProvider>);

let mounts: Record<string, number> = {};
function Counted({ name }: { name: string }) {
  useState(() => { mounts[name] = (mounts[name] ?? 0) + 1; });
  return <p>{name} content</p>;
}
const tabs = [
  { key: "overview", label: "Overview", children: <Counted name="overview" /> },
  { key: "gpus", label: "GPUs", count: 8, countTitle: "8 GPUs on this node", children: <Counted name="gpus" /> },
  { key: "logs", label: "Logs", children: <Counted name="logs" /> },
];

test("the drawer shows its title, subtitle, actions, banner and the first tab by default", async () => {
  mounts = {};
  mount(<DetailDrawer open onClose={() => {}} title="node-1" subtitle="gpu-host.example" extra={<button>Reboot</button>} banner={<p role="status">Node offline</p>} tabs={tabs}><p>Dialogs go here</p></DetailDrawer>);
  const dialog = await screen.findByRole("dialog", { name: "node-1" });
  assert.ok(within(dialog).getByText("gpu-host.example"));
  assert.ok(within(dialog).getByRole("button", { name: "Reboot" }));
  assert.equal(within(dialog).getByRole("status").textContent, "Node offline");
  assert.equal(within(dialog).getByRole("tab", { name: "Overview" }).getAttribute("aria-selected"), "true");
  assert.ok(within(dialog).getByText("overview content"));
  assert.equal(within(dialog).queryByText("gpus content"), null, "an unvisited tab is not mounted");
  assert.equal(within(dialog).getByTitle("8 GPUs on this node").textContent, "8", "the count is a separate pill with its own title");
  assert.ok(within(dialog).getByText("Dialogs go here"));
});

test("a visited tab stays mounted after leaving it and the parent hears each change", async () => {
  mounts = {};
  const changes: string[] = [];
  function Example() {
    const [tab, setTab] = useState("overview");
    return <DetailDrawer open onClose={() => {}} title="node-1" tabs={tabs} activeTab={tab} onTabChange={(key) => { changes.push(key); setTab(key); }} />;
  }
  mount(<Example />);
  await screen.findByRole("dialog", { name: "node-1" });
  const user = userEvent.setup();
  await user.click(screen.getByRole("tab", { name: /GPUs/ }));
  await screen.findByText("gpus content");
  await user.click(screen.getByRole("tab", { name: "Overview" }));
  await waitFor(() => assert.equal(screen.getByRole("tab", { name: "Overview" }).getAttribute("aria-selected"), "true"));
  assert.ok(document.body.textContent?.includes("gpus content"), "the GPUs tab keeps its state");
  assert.deepEqual(changes, ["gpus", "overview"]);
  assert.deepEqual(mounts, { overview: 1, gpus: 1 }, "neither tab mounted twice; Logs never mounted");
});

test("closing the drawer forgets the visited tabs, so reopening mounts only the selected one", async () => {
  mounts = {};
  function Example() {
    const [open, setOpen] = useState(true);
    const [tab, setTab] = useState("gpus");
    return <><button onClick={() => setOpen(true)}>Open</button>
      <DetailDrawer open={open} onClose={() => setOpen(false)} title="node-1" tabs={tabs} activeTab={tab} onTabChange={setTab} /></>;
  }
  mount(<Example />);
  await screen.findByText("gpus content");
  const user = userEvent.setup();
  await user.click(screen.getByRole("tab", { name: "Logs" }));
  await screen.findByText("logs content");
  await user.click(screen.getByRole("button", { name: "Close" }));
  await waitFor(() => assert.equal(screen.queryByRole("dialog"), null));
  mounts = {};
  await user.click(screen.getByRole("button", { name: "Open" }));
  await screen.findByText("logs content");
  assert.equal(screen.queryByText("gpus content"), null, "the earlier visit was forgotten");
  assert.deepEqual(mounts, { logs: 1 });
});

test("a detail with one view has no tab list", async () => {
  mount(<DetailDrawer open onClose={() => {}} title="Key"><p>Only view</p></DetailDrawer>);
  await screen.findByRole("dialog", { name: "Key" });
  assert.equal(screen.queryByRole("tablist"), null);
  assert.ok(screen.getByText("Only view"));
});

type Row = { id: string; path: string; hints: string[] };
const rows: Row[] = [{ id: "a", path: "/models/a", hints: ["fp8"] }, { id: "b", path: "/models/b", hints: [] }];
const extra = [
  { key: "path", title: "Path", wide: true, render: (_: unknown, row: Row) => row.path },
  { key: "hints", title: "Hints" },
];
function ExpandTable({ rowClick, onRow }: { rowClick?: boolean; onRow?: (id: string) => void }) {
  return <Table<Row> rowKey="id" dataSource={rows} pagination={false} columns={[{ key: "id", dataIndex: "id", title: "ID" }]}
    onRow={(row) => ({ onClick: () => onRow?.(row.id) })}
    expandable={detailExpandable((row) => <DetailExtra columns={extra} record={row} />, { expand: "Expand row", collapse: "Collapse row" },
      { rowExpandable: (row) => row.hints.length > 0, rowClick })} />;
}

test("only expandable rows get a chevron, which toggles the row's detail without the row's own click", async () => {
  const opened: string[] = [];
  mount(<ExpandTable rowClick={false} onRow={(id) => opened.push(id)} />);
  const chevrons = screen.getAllByRole("button", { name: "Expand row" });
  assert.equal(chevrons.length, 1, "the row without hints has no chevron");
  assert.ok(screen.getByText("a").closest("tr")?.contains(chevrons[0]));
  const user = userEvent.setup();
  chevrons[0].focus();
  await user.keyboard("{Enter}");
  const collapse = await screen.findByRole("button", { name: "Collapse row" });
  assert.equal(collapse.getAttribute("aria-expanded"), "true");
  assert.deepEqual(opened, [], "the chevron does not also click the row");
  const terms = [...document.querySelectorAll(".detail-extra dt")].map((term) => [term.textContent, term.nextElementSibling?.textContent]);
  assert.deepEqual(terms, [["Path", "/models/a"], ["Hints", ""]], "a column without a render shows an empty value");
  assert.equal((document.querySelector(".detail-extra > div") as HTMLElement).style.gridColumn, "1 / -1", "a wide column takes the full row");
  await user.click(collapse);
  await waitFor(() => assert.equal(screen.getByRole("button", { name: "Expand row" }).getAttribute("aria-expanded"), "false"));
});

test("by default a click anywhere on an expandable row expands it", async () => {
  mount(<ExpandTable />);
  fireEvent.click(screen.getByText("a"));
  assert.ok(await screen.findByRole("button", { name: "Collapse row" }));
  fireEvent.click(screen.getByText("b"));
  assert.equal(document.querySelectorAll(".detail-extra").length, 1, "a row that cannot expand does not");
});
