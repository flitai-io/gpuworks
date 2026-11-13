import assert from "node:assert/strict";
import { after, afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import { createI18n } from "./helpers/dom.ts";
import { trackMessageChannels } from "./helpers/trackedMessageChannels.ts";
import { cpuClock } from "./helpers/cpuClock.ts";
import { bigFleet, smallFleet } from "./helpers/overviewFixtures.ts";

// The heatmap grid: one cell per card grouped by node, the hover card, the
// cells' accessible names, opening a node, keyboard movement across the nodes
// and the search highlight - at eight cards a node and at 8000 cards.

const closeChannels = trackMessageChannels();
const { i18n, locale } = await createI18n(["scheduling", "deployments"]);
const { cleanup, render, screen, within, fireEvent } = await import("@testing-library/react");
const userEvent = (await import("@testing-library/user-event")).default;
const { default: Heatmap, cardsAcross } = await import("../src/pages/scheduling/HeatmapGrid.tsx");
const { arrange, buildModel, NO_FILTERS } = await import("../src/pages/scheduling/overviewModel.ts");
type Filters = import("../src/pages/scheduling/overviewModel.ts").Filters;
type GroupBy = import("../src/pages/scheduling/overviewModel.ts").GroupBy;
type Overview = import("../src/schedulingApi.ts").SchedulingOverview;

const s = locale.scheduling;
afterEach(cleanup);
after(() => closeChannels());

function mount(overview: Overview, options: { filters?: Partial<Filters>; groupBy?: GroupBy } = {}) {
  const { model } = buildModel(overview);
  const groupBy = options.groupBy ?? "node";
  const arrangement = arrange(model, { ...NO_FILTERS, ...options.filters }, groupBy, "name");
  const opened: string[] = [];
  const view = render(
    <I18nextProvider i18n={i18n}>
      <Heatmap arrangement={arrangement} model={model} groupBy={groupBy} onOpen={(uuid) => opened.push(uuid)} />
    </I18nextProvider>,
  );
  return { opened, view, user: userEvent.setup(), arrangement };
}

const cells = () => [...document.querySelectorAll<HTMLButtonElement>("button.hm-cell")];
const cellOf = (node: string, index: number) => screen.getByRole("button", { name: new RegExp(`^${node} GPU ${index},`) });

test("cardsAcross lays a node's cards in rows of four, then of eight", () => {
  assert.deepEqual([1, 2, 4, 5, 8, 9, 16, 40].map(cardsAcross), [1, 2, 4, 4, 4, 8, 8, 8]);
  assert.equal(cardsAcross(0), 1);
});

test("every card is a cell named in full, grouped under its node", () => {
  mount(smallFleet());
  assert.equal(cells().length, 6);
  const group = screen.getByRole("group", { name: "gpu-a" });
  assert.equal(within(group).getAllByRole("button").length, 3, "two cells and the node's name");
  const shared = i18n.t("scheduling:heatmap.cell", { node: "gpu-a", index: 1, model: "L20", memory: "48.0 GiB", state: s.states.awake });
  const label = cellOf("gpu-a", 1).getAttribute("aria-label") ?? "";
  assert.ok(label.startsWith(shared), label);
  assert.ok(label.includes(i18n.t("scheduling:heatmap.detail.claimed", { pct: 25 })));
  assert.ok(label.includes(i18n.t("scheduling:heatmap.detail.residue", { pct: 4 })));
  assert.ok(label.includes(i18n.t("scheduling:heatmap.detail.switching")));
  assert.ok(label.includes(i18n.t("scheduling:heatmap.holder", { name: "night", level: i18n.t("deployments:yieldPolicy.policy.name.idle"), state: s.heatmap.holderState.asleep })));
  assert.ok(cellOf("gpu-c", 0).getAttribute("aria-label")?.includes(s.states.held_outside));
  assert.ok(cellOf("gpu-b", 0).getAttribute("aria-label")?.includes(s.states.offline));
});

test("a cell is filled by its state, an awake one deeper with more claimed, and a changing one is outlined", () => {
  mount(smallFleet());
  const fill = (node: string, index: number) => cellOf(node, index).style.background;
  assert.notEqual(fill("gpu-a", 0), fill("gpu-a", 1), "100% and 25% claimed differ");
  assert.notEqual(fill("gpu-c", 2), fill("gpu-c", 1), "free and asleep differ");
  assert.ok(cellOf("gpu-a", 1).classList.contains("is-switching"));
  assert.ok(!cellOf("gpu-a", 0).classList.contains("is-switching"));
  assert.ok(cellOf("gpu-b", 0).classList.contains("is-offline"));
  assert.ok(cellOf("gpu-c", 2).classList.contains("is-free"));
});

test("hovering a cell shows the card's node, model, shares and holders with their service level; leaving hides it", async () => {
  const { user } = mount(smallFleet());
  assert.equal(screen.queryByRole("tooltip", { hidden: true }), null);
  await user.hover(cellOf("gpu-a", 1));
  const tip = screen.getByRole("tooltip", { hidden: true });
  assert.ok(within(tip).getByText(i18n.t("scheduling:heatmap.tipTitle", { node: "gpu-a", index: 1 })));
  assert.ok(within(tip).getByText(i18n.t("scheduling:heatmap.tipModel", { model: "L20", memory: "48.0 GiB" })));
  assert.ok(within(tip).getByText(i18n.t("scheduling:heatmap.tipShares", { awake: 25, sleeping: 4, free: 71 })));
  assert.ok(within(tip).getByText(i18n.t("scheduling:heatmap.holder", { name: "batch", level: i18n.t("deployments:yieldPolicy.policy.name.timeslice"), state: s.heatmap.holderState.awake })));
  assert.ok(within(tip).getByText(i18n.t("scheduling:heatmap.holder", { name: "night", level: i18n.t("deployments:yieldPolicy.policy.name.idle"), state: s.heatmap.holderState.asleep })));
  await user.hover(cellOf("gpu-c", 0));
  assert.ok(within(screen.getByRole("tooltip", { hidden: true })).getByText(i18n.t("scheduling:heatmap.tipTitle", { node: "gpu-c", index: 0 })));
  assert.equal(screen.getAllByRole("tooltip", { hidden: true }).length, 1, "one hover card follows the pointer");
  await user.unhover(cellOf("gpu-c", 0));
  assert.equal(screen.queryByRole("tooltip", { hidden: true }), null);
});

test("focusing a cell shows the same card, and moving away hides it", async () => {
  const { user } = mount(smallFleet());
  await user.tab();
  assert.equal(document.activeElement, cells()[0]);
  assert.ok(within(screen.getByRole("tooltip", { hidden: true })).getByText(i18n.t("scheduling:heatmap.tipTitle", { node: "gpu-a", index: 0 })));
  await user.tab();
  assert.equal(screen.queryByRole("tooltip", { hidden: true }), null, "tab leaves the grid: one cell is in the tab order");
});

test("clicking a cell, a node's name or pressing Enter on a cell opens that node", async () => {
  const { user, opened } = mount(smallFleet());
  await user.click(cellOf("gpu-c", 1));
  await user.click(screen.getByRole("button", { name: i18n.t("scheduling:heatmap.openNode", { node: "gpu-b" }) }));
  cellOf("gpu-a", 0).focus();
  await user.keyboard("{Enter}");
  await user.keyboard(" ");
  assert.deepEqual(opened, ["node-c", "node-b", "node-a", "node-a"]);
});

test("the arrow keys move between cards and across nodes, Up and Down between nodes at the same position", async () => {
  const { user } = mount(smallFleet());
  await user.tab();
  const at = () => document.activeElement?.getAttribute("aria-label")?.split(":")[0];
  assert.equal(at(), "gpu-a GPU 0, L20 48.0 GiB");
  await user.keyboard("{ArrowRight}");
  assert.equal(at(), "gpu-a GPU 1, L20 48.0 GiB");
  await user.keyboard("{ArrowRight}");
  assert.equal(at(), "gpu-b GPU 0, L20 48.0 GiB", "the next card is the first of the next node");
  await user.keyboard("{ArrowRight}{ArrowRight}");
  assert.equal(at(), "gpu-c GPU 1, H100 48.0 GiB");
  await user.keyboard("{ArrowUp}");
  assert.equal(at(), "gpu-b GPU 0, L20 48.0 GiB", "gpu-b has one card, so up lands on its last");
  await user.keyboard("{ArrowDown}{ArrowDown}");
  assert.equal(at(), "gpu-c GPU 0, H100 48.0 GiB");
  await user.keyboard("{End}");
  assert.equal(at(), "gpu-c GPU 2, H100 48.0 GiB");
  await user.keyboard("{ArrowRight}");
  assert.equal(at(), "gpu-c GPU 2, H100 48.0 GiB", "the last card stays");
  await user.keyboard("{Home}");
  assert.equal(at(), "gpu-a GPU 0, L20 48.0 GiB");
  await user.keyboard("{PageDown}");
  assert.equal(at(), "gpu-c GPU 0, H100 48.0 GiB", "a page of nodes stops at the last");
  await user.keyboard("{PageUp}{ArrowLeft}");
  assert.equal(at(), "gpu-a GPU 0, L20 48.0 GiB");
  // Exactly one cell is in the tab order, the one that has the focus.
  assert.deepEqual(cells().filter((cell) => cell.tabIndex === 0), [document.activeElement]);
});

test("a search rings the cards its deployments occupy and dims the rest", () => {
  mount(smallFleet(), { filters: { search: "night" } });
  assert.ok(document.querySelector(".hm-body")?.classList.contains("is-searching"));
  const hits = cells().filter((cell) => cell.classList.contains("is-hit")).map((cell) => cell.getAttribute("aria-label")?.split(":")[0]);
  assert.deepEqual(hits, ["gpu-a GPU 1, L20 48.0 GiB", "gpu-c GPU 1, H100 48.0 GiB"]);
});

test("grouped by model, each model is a section headed with its cards and free cards", () => {
  mount(smallFleet(), { groupBy: "model" });
  const l20 = screen.getByRole("region", { name: "L20" });
  assert.ok(within(l20).getByText(i18n.t("scheduling:heatmap.sectionCount", { cards: 3, free: 0 })));
  const h100 = screen.getByRole("region", { name: "H100" });
  assert.ok(within(h100).getByText(i18n.t("scheduling:heatmap.sectionCount", { cards: 3, free: 1 })));
  assert.equal(within(h100).getAllByRole("button").length, 3 + 1, "three cells and the node's name");
});

test("8000 cards render, are described by one hover card, move by keyboard and open a node", async () => {
  const overview = bigFleet(1000, 8);
  const started = performance.now();
  const renderCPU = cpuClock();
  const { user, opened } = mount(overview);
  const rendered = renderCPU();
  console.log(`heatmap: rendered 8000 cards in ${Math.round(performance.now() - started)} ms (${Math.round(rendered)} ms of CPU)`);
  assert.equal(cells().length, 8000);
  assert.equal(document.querySelectorAll(".hm-node").length, 1000);
  assert.equal(document.querySelectorAll("button.hm-node-name").length, 1000, "each group keeps its visible node name even at fleet scale");
  assert.equal(cells().filter((cell) => cell.tabIndex === 0).length, 1);
  assert.ok(rendered < 20_000, `rendering took ${Math.round(rendered)} ms of CPU`);

  const moved = performance.now();
  const movedCPU = cpuClock();
  await user.tab();
  for (let i = 0; i < 20; i += 1) await user.keyboard("{ArrowDown}");
  await user.keyboard("{PageDown}{End}");
  const last = document.activeElement as HTMLElement;
  assert.ok(last.getAttribute("aria-label")?.startsWith("gpu-0999 GPU 7"), last.getAttribute("aria-label") ?? "");
  fireEvent.mouseOver(cells()[4000]);
  assert.equal(screen.getAllByRole("tooltip", { hidden: true }).length, 1);
  await user.click(cells()[17]);
  assert.deepEqual(opened, ["node-2"]);
  console.log(`heatmap: 22 key presses, a hover and a click in ${Math.round(performance.now() - moved)} ms (${Math.round(movedCPU())} ms of CPU)`);
  assert.ok(movedCPU() < 20_000, `22 key presses, a hover and a click took ${Math.round(movedCPU())} ms of CPU`);
});
