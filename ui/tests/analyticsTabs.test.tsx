import assert from "node:assert/strict";
import { after, afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import { createI18n, dom } from "./helpers/dom.ts";
import { trackMessageChannels } from "./helpers/trackedMessageChannels.ts";
import * as fx from "./helpers/analyticsFixtures.ts";

const closeChannels = trackMessageChannels();
const { i18n, locale } = await createI18n(["analytics", "nodes"]);
// antd scrolls a sorted table back to the top through the global Document.
Object.defineProperty(globalThis, "Document", { value: dom.window.Document, configurable: true });
fx.installCanvas(dom);
const { cleanup, fireEvent, render, screen, waitFor, within } = await import("@testing-library/react");
const userEvent = (await import("@testing-library/user-event")).default;
const { MemoryRouter, Route, Routes } = await import("react-router-dom");
const { getInstanceByDom } = await import("echarts/core");
const { api } = await import("../src/api.ts");
const { analyticsApi } = await import("../src/analyticsApi.ts");
const { APIError } = await import("../src/apiError.ts");
const { default: AnalyticsPage } = await import("../src/pages/analytics/AnalyticsPage.tsx");
type Params = import("../src/analyticsApi.ts").AnalyticsParams;

const a = locale.analytics;
const originalAnalytics = { ...analyticsApi };
const originalApi = { dataRetention: api.dataRetention, deployments: api.deployments, nodeFacets: api.nodeFacets };
const calls = { usage: [] as Params[], quality: [] as Params[], gpus: [] as Params[], gpuTime: [] as Params[] };

function stub() {
  for (const list of Object.values(calls)) list.length = 0;
  api.dataRetention = (async () => fx.retentionView()) as typeof api.dataRetention;
  api.deployments = (async () => fx.deployments) as unknown as typeof api.deployments;
  api.nodeFacets = (async () => fx.nodeFacets) as typeof api.nodeFacets;
  analyticsApi.usage = async (p) => { calls.usage.push(p); return fx.usageToday(); };
  analyticsApi.quality = async (p) => { calls.quality.push(p); return fx.qualityToday(); };
  analyticsApi.gpus = async (p) => { calls.gpus.push(p); return fx.gpusToday(); };
  analyticsApi.gpuTime = async (p) => { calls.gpuTime.push(p); return fx.gpuTimeToday(); };
}

function mount(url: string) {
  render(
    <I18nextProvider i18n={i18n}>
      <MemoryRouter initialEntries={[url]}>
        <Routes><Route path="/analytics/:tab?" element={<AnalyticsPage now={() => fx.NOW} />} /></Routes>
      </MemoryRouter>
    </I18nextProvider>,
  );
  return userEvent.setup();
}

const stat = (key: string) => within(document.querySelector(`[data-stat="${key}"]`) as HTMLElement);
const ready = (key: string) => waitFor(() => assert.ok(document.querySelector(`[data-stat="${key}"]`), `${key} card`));
const choose = (user: ReturnType<typeof userEvent.setup>, name: string) => user.click(screen.getByRole("radio", { name }).closest("label")!);
const last = <T,>(list: T[]) => list[list.length - 1];
// The chart's current ECharts option: what is drawn, without a canvas to look at.
// eslint-disable-next-line @typescript-eslint/no-explicit-any
const option = (label: string): any => getInstanceByDom(screen.getByRole("img", { name: label }) as HTMLElement)!.getOption();
const bodyRows = () => [...document.querySelectorAll("tbody tr[data-row-key]")] as HTMLElement[];
const rowNamed = (text: string) => {
  const row = bodyRows().find((r) => r.textContent!.includes(text));
  assert.ok(row, `a row with ${text}`);
  return row;
};
const firstCells = () => bodyRows().map((r) => r.querySelector("td")!.textContent);

afterEach(() => {
  cleanup();
  Object.assign(analyticsApi, originalAnalytics);
  Object.assign(api, originalApi);
});
after(() => closeChannels());

// ----------------------------------------------------------------------- usage

test("usage rows name a key by its prefix, mark a revoked or deleted one and a keyless Playground request by its user", async () => {
  stub();
  mount("/analytics/usage");
  await ready("requests");
  await waitFor(() => assert.ok(bodyRows().length >= 5));
  const names = firstCells().join("|");
  assert.ok(rowNamed("MarketToGetherGroup").textContent!.includes("gw-6455c72ca"), "the prefix is under the name");
  const trial = rowNamed("trial-2026q3");
  assert.ok(within(trial).getByText(a.names.revoked));
  assert.ok(names.includes(i18n.t("analytics:names.deletedKey", { id: 7 })));
  assert.ok(names.includes(i18n.t("analytics:names.playground", { user: "admin" })));
  // A keyless Playground request is not shown as a key with an empty name.
  assert.ok(!names.includes("gw-undefined"));
});

test("a key row expands to the deployments it used, a deleted deployment tagged", async () => {
  stub();
  const user = mount("/analytics/usage");
  await ready("requests");
  await waitFor(() => assert.ok(bodyRows().length >= 5));
  const key111 = rowNamed("gw-5fd30737e");
  assert.equal(within(key111).getByText(/glm-5-3-flash-nvfp4, qwen3-32b|qwen3-32b, glm-5-3-flash-nvfp4/).tagName !== undefined, true);
  const before = bodyRows().length;
  await user.click(within(key111).getByRole("button", { name: /expand/i }));
  await waitFor(() => assert.equal(bodyRows().length, before + 2));
  const mtg = rowNamed("MarketToGetherGroup");
  await user.click(within(mtg).getByRole("button", { name: /expand/i }));
  await waitFor(() => assert.equal(bodyRows().length, before + 4));
  const deleted = bodyRows().find((r) => r.textContent!.includes(a.names.deletedDeployment))!;
  assert.ok(deleted.textContent!.includes("old-model"), "the deleted deployment keeps its served name");
  assert.ok(deleted.textContent!.includes(i18n.t("analytics:names.deploymentId", { id: 9 })), "and is known by its id when its name is gone");
});

test("grouping by deployment or model lists those first and the keys beneath", async () => {
  stub();
  const user = mount("/analytics/usage");
  await ready("requests");
  // The grouping shapes only the details, so it sits on the details card.
  const details = screen.getByText(a.usage.table.title).closest(".ant-card")!;
  assert.ok(within(details as HTMLElement).getByText(a.usage.group.deployment));
  await choose(user, a.usage.group.deployment);
  await waitFor(() => assert.ok(firstCells().some((c) => c!.includes("qwen3-32b"))));
  assert.ok(firstCells().every((c) => !c!.includes("gw-5fd30737e")), "keys are the second level here");
  assert.ok(rowNamed(a.names.deletedDeployment));
  const row = rowNamed("qwen3-32b");
  assert.ok(row.textContent!.includes(i18n.t("analytics:usage.table.keys", { count: 1 })));
  await user.click(within(row).getByRole("button", { name: /expand/i }));
  await waitFor(() => assert.ok(bodyRows().some((r) => r.textContent!.includes("gw-5fd30737e"))));
  await choose(user, a.usage.group.model);
  await waitFor(() => assert.ok(firstCells().some((c) => c!.includes("old-model"))));
  assert.ok(firstCells().some((c) => c!.includes("glm-5.3-flash")));
});

test("the table sorts by a column, both ways, and keeps a totals row from the API's totals", async () => {
  stub();
  const user = mount("/analytics/usage");
  await ready("requests");
  await waitFor(() => assert.ok(bodyRows().length >= 5));
  // Default: most input first.
  assert.ok(firstCells()[0]!.includes("111"));
  const header = () => screen.getByRole("columnheader", { name: new RegExp(`^${a.usage.table.requests}`) });
  await user.click(header());
  await waitFor(() => assert.ok(firstCells()[0]!.includes(i18n.t("analytics:names.deletedKey", { id: 7 })), "fewest requests first"));
  assert.ok(firstCells().at(-1)!.includes("111"));
  await user.click(header());
  await waitFor(() => assert.ok(firstCells()[0]!.includes("111"), "most requests first"));
  const total = document.querySelector(".ant-table-summary")!;
  assert.ok(total.textContent!.includes(a.table.total));
  assert.ok(total.textContent!.includes("1,317"));
  assert.ok(total.textContent!.includes("76.5%"));
});

test("the input and output views of the chart plot the cache split and the thinking split", async () => {
  stub();
  const user = mount("/analytics/usage");
  await ready("requests");
  const input = option(a.usage.chart.aria);
  assert.deepEqual(input.series.map((s: { name: string }) => s.name), [a.usage.chart.cached, a.usage.chart.uncached]);
  assert.equal(input.series[0].data.length, 15);
  const d = fx.usageToday();
  assert.deepEqual(input.series[0].data, d.series.map((s) => s.cached_tokens));
  assert.deepEqual(input.series[1].data, d.series.map((s) => s.prompt_tokens - s.cached_tokens));
  await choose(user, a.usage.chart.modeOut);
  await waitFor(() => assert.deepEqual(option(a.usage.chart.aria).series.map((s: { name: string }) => s.name), [a.usage.chart.answer, a.usage.chart.thinking]));
  const out = option(a.usage.chart.aria);
  assert.deepEqual(out.series[0].data, d.series.map((s) => s.completion_tokens - s.reasoning_tokens));
  assert.deepEqual(out.series[1].data, d.series.map((s) => s.reasoning_tokens));
});

test("buckets filled in from the request log are shaded in the chart", async () => {
  stub();
  mount("/analytics/usage");
  await ready("requests");
  const area = option(a.usage.chart.aria).series[0].markArea;
  assert.ok(area, "a shaded area");
  assert.deepEqual(area.data[0].map((p: { xAxis: number }) => p.xAxis), [-0.5, 1.5], "the two buckets before live accumulation began");
});

test("the source filter asks for Playground requests only", async () => {
  stub();
  const user = mount("/analytics/usage");
  await ready("requests");
  fireEvent.mouseDown(screen.getByRole("combobox", { name: a.usage.filters.source }));
  await waitFor(() => assert.ok(document.querySelector('.ant-select-item-option[title="Playground"]')));
  await user.click(document.querySelector('.ant-select-item-option[title="Playground"]')!);
  await waitFor(() => assert.equal(last(calls.usage).source, "playground"));
});

// --------------------------------------------------------------------- quality

test("quality cards show the success rate, the percentiles in seconds and dashes where there are none", async () => {
  stub();
  mount("/analytics/quality");
  await ready("requests");
  assert.ok(stat("requests").getByText("1,000"));
  assert.ok(stat("success").getByText("97.5%"));
  assert.ok(stat("success").getByText(i18n.t("analytics:quality.cards.failures", { count: 25 })));
  assert.ok(stat("ttft").getByText("0.55 / 1.82 s"));
  assert.ok(stat("e2e").getByText("5.20 / 18.75 s"));
  assert.ok(stat("errors").getByText("20"));
  assert.ok(stat("errors").getByText(i18n.t("analytics:quality.cards.errorShare", { rate: "2.0%" })));
});

test("a deployment with no requests shows dashes, not zeros, for what was not measured", async () => {
  stub();
  mount("/analytics/quality");
  await ready("requests");
  await waitFor(() => assert.equal(bodyRows().length, 2));
  const quiet = rowNamed("qwen3-32b");
  const cells = [...quiet.querySelectorAll("td")].map((c) => c.textContent);
  assert.equal(cells[2], "—", "success rate");
  assert.deepEqual(cells.slice(8), ["—", "—", "—", "—", "—"], "latencies and speed");
  const busy = rowNamed("glm-5-3-flash-nvfp4");
  const busyCells = [...busy.querySelectorAll("td")].map((c) => c.textContent);
  assert.deepEqual(busyCells.slice(8), ["0.55 s", "1.82 s", "5.20 s", "18.75 s", "56.3"]);
  assert.match(busyCells[2]!, /97\.5%/);
});

test("the outcome chart sits above the first-token chart, which leaves a gap where an hour had no requests, and the outcome chart can hide the completed ones", async () => {
  stub();
  const user = mount("/analytics/quality");
  await ready("requests");
  // The outcome chart comes first, the first-token chart below it.
  const charts = screen.getAllByRole("img").map((el) => el.getAttribute("aria-label"));
  const at = (label: string) => charts.indexOf(label);
  assert.ok(at(a.quality.chart.resultsAria) >= 0 && at(a.quality.chart.resultsAria) < at(a.quality.chart.ttftAria), String(charts));
  const ttft = option(a.quality.chart.ttftAria);
  assert.deepEqual(ttft.series.map((s: { name: string }) => s.name), [a.quality.chart.ttftP50, a.quality.chart.ttftP95]);
  assert.equal(ttft.series[0].data[3], null);
  assert.equal(ttft.series[0].data[0], 0.5);
  assert.equal(ttft.series[1].data[0], 1.5);
  const all = option(a.quality.chart.resultsAria).series.map((s: { name: string }) => s.name);
  assert.deepEqual(all, [a.quality.outcome.completed, a.quality.outcome.length, a.quality.outcome.client_closed, a.quality.outcome.interrupted, a.quality.outcome.error]);
  await choose(user, a.quality.chart.modeBad);
  await waitFor(() => assert.deepEqual(option(a.quality.chart.resultsAria).series.map((s: { name: string }) => s.name), all.slice(1)));
});

// ------------------------------------------------------------------------ gpus

test("GPU cards: the cards there now by model, mean memory use per card, the cards' power added up and the idle count", async () => {
  stub();
  mount("/analytics/gpus");
  await ready("cards");
  // The deleted node's card keeps its row but is not counted: three cards there now.
  assert.ok(stat("cards").getByText("3"));
  assert.ok(stat("cards").getByText("RTX 6000D × 2 · PPU 810E × 1"));
  assert.ok(stat("util").getByText("22.4%"));
  // (70/84 + 70/84 + 72/96) / 3: the card with no samples is left out of the mean.
  assert.ok(stat("mem").getByText("80.6%"));
  assert.ok(stat("power").getByText("381 W"));
  assert.ok(stat("idle").getByText("2 / 3"), "two idle of the three cards that have a utilization figure");
});

test("GPU idle statistics explain the sampled denominator and missing cards", async () => {
  stub();
  const data = fx.gpusToday();
  data.cards = Array.from({ length: 59 }, (_, index) => ({
    ...data.cards[0], present: true, gpu_uuid: `GPU-${index}`, gpu_index: index,
    util_avg: index === 58 ? null : 0,
  }));
  data.card_count = 59;
  analyticsApi.gpus = async () => data;
  mount("/analytics/gpus");
  await ready("cards");
  assert.ok(stat("cards").getByText("59"));
  assert.ok(stat("idle").getByText("58 / 58"));
  assert.ok(stat("idle").getByText(/58 of 59 cards sampled; 1 without utilization samples/));
});

test("GPU idle statistics do not label an entirely unobserved fleet as zero idle", async () => {
  stub();
  const data = fx.gpusToday();
  data.cards = data.cards.filter((card) => card.present).map((card) => ({ ...card, util_avg: null }));
  analyticsApi.gpus = async () => data;
  mount("/analytics/gpus");
  await ready("idle");
  assert.ok(stat("idle").getByText("—"));
  assert.ok(stat("idle").getByText(/0 of 3 cards sampled; 3 without utilization samples/));
});

test("the GPU table groups by card model, tints the idle groups and counts cards no longer there", async () => {
  stub();
  const user = mount("/analytics/gpus");
  await ready("cards");
  await waitFor(() => assert.equal(bodyRows().length, 2));
  const ppu = rowNamed("PPU 810E");
  assert.ok(within(ppu).getByText(a.gpus.table.idle));
  assert.equal(ppu.style.background.replace(/\s/g, ""), "rgb(255,248,229)");
  const rtx = rowNamed("RTX 6000D");
  assert.ok(within(rtx).queryByText(a.gpus.table.idle) === null, "(62.5 + 3.2 + one card without samples) averages above the idle line");
  assert.ok(within(rtx).getByText(i18n.t("analytics:gpus.table.removedCount", { count: 1 })));
  assert.ok(within(rtx).getByText(/glm-5-3-flash-nvfp4/));
  await choose(user, a.gpus.group.gpu);
  await waitFor(() => assert.equal(last(calls.gpus).groupBy, "gpu"));
  await waitFor(() => assert.equal(bodyRows().length, 4));
  // A deleted node's card says so, and is neither held nor free now.
  const gone = rowNamed("gone-node #0");
  assert.ok(within(gone).getByText(a.gpus.table.nodeDeleted));
  assert.ok(within(gone).queryByText(a.gpus.table.removed) === null);
  assert.ok(within(gone).queryByText(a.gpus.table.unassigned) === null);
  const idle = rowNamed("6000*4 #1");
  assert.ok(within(idle).getByText(a.gpus.table.idle));
});

// By node, a deleted node's row is marked in the table and the heat map, and
// is no choice in the node filter (it matches no label); a card unplugged
// from a node still there is removed, not deleted.
test("the GPU tab marks a deleted node's rows and leaves it out of the node filter", async () => {
  stub();
  const deleted = { key: "node-c", label: "gone-node", node_deleted: true, cards: 0, nodes: 0, models: [], deployments: [], unassigned: 0, ...fx.gpusToday().totals };
  const live = { key: "node-a", label: "6000*4", node_deleted: false, cards: 2, nodes: 1, models: ["RTX 6000D"], deployments: [], unassigned: 2, ...fx.gpusToday().totals };
  analyticsApi.gpus = async (p) => {
    calls.gpus.push(p);
    const today = fx.gpusToday();
    return fx.gpusToday({
      group_by: "node",
      cards: [today.cards[0], { ...today.cards[1], present: false }, today.cards[3]],
      groups: [live, deleted],
      heat: [
        { key: "node-a", label: "6000*4", node_deleted: false, values: [1, 2, 3, 4] },
        { key: "node-c", label: "gone-node", node_deleted: true, values: [5, null, null, null] },
      ],
    });
  };
  mount("/analytics/gpus?by=node");
  await ready("cards");
  await waitFor(() => assert.equal(bodyRows().length, 2));
  assert.ok(within(rowNamed("gone-node")).getByText(a.gpus.table.nodeDeleted));
  const kept = rowNamed("6000*4");
  assert.ok(within(kept).queryByText(a.gpus.table.nodeDeleted) === null);
  assert.ok(within(kept).getByText(i18n.t("analytics:gpus.table.removedCount", { count: 1 })));
  assert.deepEqual(option(a.gpus.chart.aria).yAxis[0].data, ["6000*4", i18n.t("analytics:gpus.chart.deletedRow", { label: "gone-node" })]);
  const user = userEvent.setup();
  await user.click(screen.getByRole("combobox", { name: a.gpus.filters.labels }));
  await waitFor(() => assert.ok(screen.getAllByText(/6000\*4/).length > 1, "the live node is a choice"));
  assert.ok(!screen.queryAllByRole("option").some((o) => o.textContent?.includes("gone-node")), "the deleted node is no choice");
});

test("switching the heat map between utilization, power, temperature and memory asks for that metric and rescales the colours", async () => {
  stub();
  const user = mount("/analytics/gpus");
  await ready("cards");
  assert.equal(calls.gpus[0].metric, "util");
  const util = option(a.gpus.chart.aria);
  assert.deepEqual([util.visualMap[0].min, util.visualMap[0].max], [0, 100]);
  // Cells without samples are left out so they stay blank.
  assert.equal(util.series[0].data.length, 6);
  assert.deepEqual(util.yAxis[0].data, ["PPU 810E", "RTX 6000D"]);

  analyticsApi.gpus = async (p) => {
    calls.gpus.push(p);
    const d = fx.gpusToday();
    d.metric = p.metric!;
    const byMetric = { power: [210.4, 250.2, null, 331.7], temp: [48.2, 61.5, null, 72.9], mem: [60e9, 72e9, null, 80e9] } as Record<string, (number | null)[]>;
    d.heat = [{ key: "RTX 6000D", label: "RTX 6000D", values: byMetric[p.metric!] }];
    return d;
  };
  await choose(user, a.gpus.metric.power);
  await waitFor(() => assert.equal(last(calls.gpus).metric, "power"));
  await waitFor(() => assert.deepEqual(option(a.gpus.chart.aria).visualMap[0].max, 332));
  assert.equal(option(a.gpus.chart.aria).visualMap[0].min, 0);
  assert.ok(screen.getByRole("heading", { name: i18n.t("analytics:gpus.chart.title.power", { granularity: a.granularityName.hour }) }));

  await choose(user, a.gpus.metric.temp);
  await waitFor(() => assert.equal(last(calls.gpus).metric, "temp"));
  await waitFor(() => assert.deepEqual([option(a.gpus.chart.aria).visualMap[0].min, option(a.gpus.chart.aria).visualMap[0].max], [20, 100]));

  await choose(user, a.gpus.metric.mem);
  await waitFor(() => assert.equal(last(calls.gpus).metric, "mem"));
  await waitFor(() => assert.deepEqual([option(a.gpus.chart.aria).visualMap[0].min, option(a.gpus.chart.aria).visualMap[0].max], [0, 80e9]));
});

test("the metric and grouping are kept in the URL", async () => {
  stub();
  mount("/analytics/gpus?metric=power&by=node");
  await waitFor(() => assert.equal(calls.gpus.length, 1));
  assert.equal(calls.gpus[0].metric, "power");
  assert.equal(calls.gpus[0].groupBy, "node");
  assert.ok(screen.getByRole("radio", { name: a.gpus.metric.power, checked: true }));
  assert.ok(screen.getByRole("radio", { name: a.gpus.group.node, checked: true }));
});

test("too many cards shows the product error with the way out, and a node label filter fixes it", async () => {
  stub();
  const detail = "300 cards; filter by node label or model to see at most 256 by card";
  analyticsApi.gpus = async (p) => {
    calls.gpus.push(p);
    if (p.groupBy === "gpu" && !p.labels?.length && !p.model) throw new APIError("ANALYTICS_TOO_MANY_CARDS", { field: "group_by", detail }, "too many cards", 400);
    return fx.gpusToday();
  };
  const user = mount("/analytics/gpus?by=gpu");
  const alert = await screen.findByRole("alert");
  assert.ok(within(alert).getByText(i18n.t("common:apiError.ANALYTICS_TOO_MANY_CARDS", { detail })));
  assert.ok(within(alert).getByText(a.gpus.tooMany.hint));
  // With no answer to read them from, the models' list comes from a by-model answer.
  await waitFor(() => assert.ok(calls.gpus.some((p) => p.groupBy === "model")));
  // The node labels the fleet carries, with the platform's own (shown by name).
  fireEvent.mouseDown(screen.getByRole("combobox", { name: a.gpus.filters.labels }));
  await waitFor(() => assert.ok(document.querySelector('.ant-select-item-option[title="pool=inference"]')));
  assert.ok(document.querySelector(`.ant-select-item-option[title="${locale.nodes.systemLabels.key["gpu-model"]}: RTX 6000D"]`));
  await user.click(document.querySelector('.ant-select-item-option[title="pool=inference"]')!);
  await waitFor(() => assert.deepEqual(last(calls.gpus).labels, ["pool=inference"]));
  await waitFor(() => assert.ok(screen.queryByRole("alert") === null));
  assert.ok(document.querySelector('[data-stat="cards"]'));
});

test("a node an answer named can be picked as a filter, and the labels stay in the URL", async () => {
  stub();
  const user = mount("/analytics/gpus?label=pool%3Dtraining");
  await ready("cards");
  assert.deepEqual(calls.gpus[0].labels, ["pool=training"]);
  const nodeOption = `${locale.nodes.systemLabels.key.node}: PPU810E*16`;
  fireEvent.mouseDown(screen.getByRole("combobox", { name: a.gpus.filters.labels }));
  await waitFor(() => assert.ok(document.querySelector(`.ant-select-item-option[title="${nodeOption}"]`)));
  await user.click(document.querySelector(`.ant-select-item-option[title="${nodeOption}"]`)!);
  await waitFor(() => assert.deepEqual(last(calls.gpus).labels, ["pool=training", "gpuworks.ai/node=node-b"]));
});

test("too many cards can also be answered by grouping by node", async () => {
  stub();
  analyticsApi.gpus = async (p) => {
    calls.gpus.push(p);
    if (p.groupBy === "gpu") throw new APIError("ANALYTICS_TOO_MANY_CARDS", { field: "group_by", detail: "300 cards" }, "too many cards", 400);
    return fx.gpusToday();
  };
  const user = mount("/analytics/gpus?by=gpu");
  const alert = await screen.findByRole("alert");
  await user.click(within(alert).getByRole("button", { name: a.gpus.tooMany.byNode }));
  await waitFor(() => assert.equal(last(calls.gpus).groupBy, "node"));
  await ready("cards");
});

test("an unfiltered answer feeds the node and model filters, which then ask for their choice", async () => {
  stub();
  const user = mount("/analytics/gpus");
  await ready("cards");
  fireEvent.mouseDown(screen.getByRole("combobox", { name: a.gpus.filters.model }));
  await waitFor(() => assert.ok(document.querySelector('.ant-select-item-option[title="PPU 810E"]')));
  assert.ok(document.querySelector('.ant-select-item-option[title="RTX 6000D"]'));
  await user.click(document.querySelector('.ant-select-item-option[title="PPU 810E"]')!);
  await waitFor(() => assert.equal(last(calls.gpus).model, "PPU 810E"));
});

test("a fleet past the cards listed one by one says so, and its table and cards come from the groups", async () => {
  stub();
  analyticsApi.gpus = async (p) => { calls.gpus.push(p); return fx.gpusFleet(); };
  mount("/analytics/gpus");
  await ready("cards");
  assert.ok(screen.getByText(i18n.t("analytics:gpus.summaryNote", { count: 960, limit: 256 })));
  assert.ok(stat("cards").getByText("960"));
  assert.ok(stat("cards").getByText("H100 × 944 · L40S × 16"));
  assert.ok(stat("idle").getByText("—"));
  assert.ok(stat("idle").getByText(i18n.t("analytics:gpus.cards.idleNeedsDetail", { limit: 256 })));
  assert.ok(stat("power").getByText("286.40 kW"));
  await waitFor(() => assert.equal(bodyRows().length, 2));
  const h100 = rowNamed("H100");
  assert.ok(within(h100).getByText(i18n.t("analytics:gpus.table.nodeCount", { count: 118 })));
  assert.ok(within(h100).getByText(/× 940/));
  assert.ok(within(h100).getByText(/glm-5-3-flash-nvfp4/));
});

test("a node heat map past its rows says it shows the extremes", async () => {
  stub();
  analyticsApi.gpus = async (p) => {
    calls.gpus.push(p);
    const rows = Array.from({ length: 100 }, (_, i) => ({ key: `n${i}`, label: `gpu-${i}`, values: [i, i, i, i] }));
    return fx.gpusFleet({ group_by: "node", heat: rows, heat_total: 1000 });
  };
  mount("/analytics/gpus?by=node");
  await ready("cards");
  assert.ok(screen.getByText(i18n.t("analytics:gpus.chart.extremes", { half: 50, total: 1000, metric: a.gpus.metric.util })));
});

test("no cards in the range is the empty state", async () => {
  stub();
  analyticsApi.gpus = async () => ({ ...fx.gpusToday(), card_count: 0, by_model: [], cards: [], groups: [], heat: [] });
  mount("/analytics/gpus");
  await screen.findByText(a.state.empty);
});

// --------------------------------------------------------------------- gpu time

test("GPU hours cards: the total, the allocation rate over schedulable hours, each deployment's tokens once", async () => {
  stub();
  mount("/analytics/gpu-time");
  await ready("hours");
  assert.ok(stat("hours").getByText("88.0"));
  assert.ok(stat("allocation").getByText("80.0%"));
  assert.ok(stat("allocation").getByText(i18n.t("analytics:gpuTime.cards.allocationSub", { allocated: "80.0", schedulable: "100.0" })));
  // Deployment 1 has two rows (two card models) that both carry its 320,000 output tokens.
  assert.ok(stat("output").getByTitle("320,000"));
  assert.ok(stat("perMillion").getByText("275.0"));
  assert.ok(stat("perMillion").getByText(i18n.t("analytics:gpuTime.cards.perMillionAll", { value: "8.7" })));
});

test("the GPU hours table has a row per deployment and card model, dashes for what has no tokens, and a total", async () => {
  stub();
  mount("/analytics/gpu-time");
  await ready("hours");
  await waitFor(() => assert.equal(bodyRows().length, 3));
  const qwen = rowNamed("qwen3-32b");
  const cells = [...qwen.querySelectorAll("td")].map((c) => c.textContent);
  assert.deepEqual(cells.slice(3), ["8.0", "8.0", "0", "0", "—", "—"]);
  const ppu = bodyRows().find((r) => r.textContent!.includes("PPU 810E"))!;
  assert.ok(ppu.textContent!.includes("0.50 / 1"));
  const total = document.querySelector(".ant-table-summary")!;
  assert.ok(total.textContent!.includes("88.0"));
  assert.ok(total.textContent!.includes("275.0"));
  const chart = option(a.gpuTime.chart.aria);
  assert.deepEqual(chart.series.map((s: { name: string }) => s.name), ["glm-5-3-flash-nvfp4", "qwen3-32b"]);
  assert.deepEqual(chart.series[0].data, [20, 20, 20, 20]);
});

test("the GPU hours tab asks with the shared deployment filter", async () => {
  stub();
  mount("/analytics/gpu-time?range=7d&deployment=3");
  await waitFor(() => assert.equal(calls.gpuTime.length, 1));
  assert.equal(calls.gpuTime[0].deployment, 3);
  assert.equal(calls.gpuTime[0].granularity, "day");
});

test("no card time is the empty state, and the note stays", async () => {
  stub();
  analyticsApi.gpuTime = async () => ({ range: fx.range(fx.TODAY_START, 2, "hour"), rows: [], series: [], capacity: [] });
  mount("/analytics/gpu-time");
  await screen.findByText(a.state.empty);
});
