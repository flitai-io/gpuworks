// The column header funnel (ui/src/ui-kit/ColumnFilter.tsx), driven through
// ResourceTable the way list pages use it: options come from a column's
// `filters`, the funnel is named "Filter by <column>", and the rows follow.
import assert from "node:assert/strict";
import { after, afterEach, beforeEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import { dom, createI18n } from "./helpers/dom.ts";
import { trackMessageChannels } from "./helpers/trackedMessageChannels.ts";

const closeChannels = trackMessageChannels();
for (const name of ["KeyboardEvent", "Document", "AbortController", "AbortSignal"] as const) {
  Object.defineProperty(globalThis, name, { value: dom.window[name], configurable: true });
}
Object.defineProperty(globalThis, "localStorage", { value: dom.window.localStorage, configurable: true });
// jsdom has no Web Animations API; Base UI asks which animations are running before unmounting a popup.
if (!("getAnimations" in dom.window.Element.prototype)) {
  Object.defineProperty(dom.window.Element.prototype, "getAnimations", { configurable: true, value: () => [] });
}

const { i18n, locale } = await createI18n([]);
const rt = locale.common.resourceTable as Record<string, string>;
const { cleanup, render, screen, waitFor, within } = await import("@testing-library/react");
const userEvent = (await import("@testing-library/user-event")).default;
const { default: ResourceTable } = await import("../src/components/resourceTable/ResourceTable.tsx");
// helpers/dom.ts drops a finished test's pending animation frames; Base UI's
// process-wide frame scheduler would then wait forever for the dropped one.
const { resetAnimationFrameScheduler } = await import("@base-ui/utils/useAnimationFrame");
type ResourceColumn<T> = import("../src/components/resourceTable/ResourceTable").ResourceColumn<T>;

beforeEach(() => localStorage.clear());
afterEach(() => { cleanup(); resetAnimationFrameScheduler(); });
after(() => closeChannels());

type Row = { id: number; name: string; state: string; node: string };
const rows: Row[] = [
  { id: 1, name: "qwen-a", state: "running", node: "gpu-node-01" },
  { id: 2, name: "qwen-b", state: "stopped", node: "gpu-node-02" },
  { id: 3, name: "llama-c", state: "failed", node: "cpu-node-03" },
  { id: 4, name: "llama-d", state: "running", node: "gpu-node-02" },
];
const states = [{ value: "running", text: "Running" }, { value: "stopped", text: "Stopped" }, { value: "failed", text: "Failed" }];
const nodes = ["gpu-node-01", "gpu-node-02", "cpu-node-03"].map((value) => ({ value, text: value.toUpperCase() }));

function columns(options: { stateMultiple?: boolean } = {}): ResourceColumn<Row>[] {
  return [
    { key: "name", role: "name", title: "Name", width: 160, render: (_, row) => row.name },
    { key: "state", title: "State", width: 140, filters: states, filterMultiple: options.stateMultiple, onFilter: (value, row) => row.state === value, render: (_, row) => row.state },
    { key: "node", title: "Node", width: 160, filters: nodes, filterSearch: true, onFilter: (value, row) => row.node === value, render: (_, row) => row.node },
  ];
}
function mount(options: { stateMultiple?: boolean } = {}) {
  return render(
    <I18nextProvider i18n={i18n}>
      <ResourceTable<Row> tableId="column-filter" rowKey="id" columns={columns(options)} data={rows} pagination={false} />
    </I18nextProvider>,
  );
}
const names = () => screen.getAllByRole("row").slice(1).map((row) => within(row).getAllByRole("cell")[0].textContent);
const funnel = (column: string) => screen.getByRole("button", { name: i18n.t("resourceTable.filterByColumn", { name: column }) });
const options = (column: string) => screen.getByRole("group", { name: i18n.t("resourceTable.filterByColumn", { name: column }) });
const checked = (column: string) => within(options(column)).getAllByRole("checkbox").filter((box) => box.getAttribute("aria-checked") === "true").map((box) => box.getAttribute("aria-label"));

test("multi-select applies each tick at once, keeps the list open and untick removes it", async () => {
  const user = userEvent.setup();
  mount();
  assert.equal(funnel("State").getAttribute("aria-pressed"), "false");
  await user.click(funnel("State"));
  await user.click(await screen.findByRole("checkbox", { name: "Stopped" }));
  await waitFor(() => assert.deepEqual(names(), ["qwen-b"]));
  assert.equal(funnel("State").getAttribute("aria-pressed"), "true", "the funnel says a filter is applied");
  await user.click(screen.getByRole("checkbox", { name: "Failed" }));
  await waitFor(() => assert.deepEqual(names(), ["qwen-b", "llama-c"]));
  assert.deepEqual(checked("State"), ["Stopped", "Failed"], "the list stays open with both ticked");

  await user.click(screen.getByRole("checkbox", { name: "Stopped" }));
  await waitFor(() => assert.deepEqual(names(), ["llama-c"]));
  await user.click(screen.getByRole("checkbox", { name: "Failed" }));
  await waitFor(() => assert.deepEqual(names(), ["qwen-a", "qwen-b", "llama-c", "llama-d"]));
  assert.equal(funnel("State").getAttribute("aria-pressed"), "false", "unticking the last option removes the filter");
  assert.ok(screen.queryByRole("button", { name: rt.clearFilters }) === null);
});

test("single-select closes on a pick, replaces the pick, and picking it again clears it", async () => {
  const user = userEvent.setup();
  mount({ stateMultiple: false });
  await user.click(funnel("State"));
  await user.click(await screen.findByRole("checkbox", { name: "Running" }));
  await waitFor(() => assert.ok(screen.queryByRole("group", { name: i18n.t("resourceTable.filterByColumn", { name: "State" }) }) === null, "the list closes on a pick"));
  assert.deepEqual(names(), ["qwen-a", "llama-d"]);
  assert.equal(funnel("State").getAttribute("aria-pressed"), "true");

  await user.click(funnel("State"));
  await screen.findByRole("checkbox", { name: "Failed" });
  assert.deepEqual(checked("State"), ["Running"]);
  await user.click(screen.getByRole("checkbox", { name: "Failed" }));
  await waitFor(() => assert.deepEqual(names(), ["llama-c"], "a second pick replaces the first"));

  await user.click(funnel("State"));
  await user.click(await screen.findByRole("checkbox", { name: "Failed" }));
  await waitFor(() => assert.equal(funnel("State").getAttribute("aria-pressed"), "false"));
  assert.deepEqual(names(), ["qwen-a", "qwen-b", "llama-c", "llama-d"]);
});

test("the option search narrows by label, ignoring case and spaces, and says nothing matched by showing no options", async () => {
  const user = userEvent.setup();
  mount();
  await user.click(funnel("Node"));
  const search = await screen.findByRole("textbox", { name: rt.searchOptions });
  assert.equal(search.getAttribute("placeholder"), rt.searchOptions);
  assert.equal(within(options("Node")).getAllByRole("checkbox").length, 3);
  await user.type(search, "  GPU-node ");
  await waitFor(() => assert.deepEqual(within(options("Node")).getAllByRole("checkbox").map((box) => box.getAttribute("aria-label")), ["GPU-NODE-01", "GPU-NODE-02"]));

  // A pick in the narrowed list marks the right option once the search is cleared.
  await user.click(screen.getByRole("checkbox", { name: "GPU-NODE-02" }));
  await waitFor(() => assert.deepEqual(names(), ["qwen-b", "llama-d"]));
  await user.clear(search);
  await waitFor(() => assert.equal(within(options("Node")).getAllByRole("checkbox").length, 3));
  assert.deepEqual(checked("Node"), ["GPU-NODE-02"]);

  await user.type(search, "tpu");
  await waitFor(() => assert.equal(within(options("Node")).queryAllByRole("checkbox").length, 0));
  // Nothing replaces the options; the title and the clear button remain the only content.
  const popup = search.closest("[data-slot=popover-content]") ?? search.parentElement!.parentElement!;
  assert.ok(within(popup as HTMLElement).getByText(i18n.t("resourceTable.filterByColumn", { name: "Node" })));
  assert.ok(within(popup as HTMLElement).getByRole("button", { name: rt.clearFilters }), "the applied pick can still be cleared");
  assert.deepEqual(names(), ["qwen-b", "llama-d"], "searching options never changes the filter");
});

test("a column without search offers no search box", async () => {
  const user = userEvent.setup();
  mount();
  await user.click(funnel("State"));
  await screen.findByRole("checkbox", { name: "Running" });
  assert.ok(screen.queryByRole("textbox", { name: rt.searchOptions }) === null);
});

test("clear filters appears only while a value is picked and clears the column", async () => {
  const user = userEvent.setup();
  mount();
  await user.click(funnel("State"));
  await screen.findByRole("checkbox", { name: "Running" });
  assert.ok(screen.queryByRole("button", { name: rt.clearFilters }) === null, "nothing to clear yet");
  await user.click(screen.getByRole("checkbox", { name: "Running" }));
  await user.click(screen.getByRole("checkbox", { name: "Failed" }));
  await waitFor(() => assert.deepEqual(names(), ["qwen-a", "llama-c", "llama-d"]));
  const popupClear = within(options("State").parentElement!.parentElement!).getByRole("button", { name: rt.clearFilters });
  await user.click(popupClear);
  await waitFor(() => assert.ok(screen.queryByRole("group", { name: i18n.t("resourceTable.filterByColumn", { name: "State" }) }) === null, "clearing closes the list"));
  assert.equal(funnel("State").getAttribute("aria-pressed"), "false");
  assert.deepEqual(names(), ["qwen-a", "qwen-b", "llama-c", "llama-d"]);
  assert.ok(screen.queryByRole("group", { name: rt.activeFilters }) === null, "the chip row goes with it");
});

test("the keyboard opens the list, ticks an option, and Escape returns focus to the funnel", async () => {
  const user = userEvent.setup();
  mount();
  funnel("State").focus();
  await user.keyboard("{Enter}");
  const stopped = await screen.findByRole("checkbox", { name: "Stopped" });
  stopped.focus();
  await user.keyboard(" ");
  await waitFor(() => assert.deepEqual(names(), ["qwen-b"]));
  await user.keyboard("{Escape}");
  await waitFor(() => assert.equal(funnel("State").getAttribute("aria-expanded"), "false"));
  await waitFor(() => assert.ok(document.activeElement === funnel("State"), "focus goes back to the funnel"));
  assert.equal(funnel("State").getAttribute("aria-pressed"), "true", "closing keeps the filter");
});
