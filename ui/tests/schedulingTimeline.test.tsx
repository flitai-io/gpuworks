import assert from "node:assert/strict";
import { after, afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import { createI18n, dom } from "./helpers/dom.ts";
import { trackMessageChannels } from "./helpers/trackedMessageChannels.ts";

// The residency timeline card: its loading / error / empty / success states,
// the lanes drawn to scale with their ticks and legend, the tooltip a hovered
// or focused block shows, the window selector and the sharing table.

Object.defineProperty(globalThis, "Document", { value: dom.window.Document, configurable: true });
const closeChannels = trackMessageChannels();
const { i18n, locale } = await createI18n(["scheduling", "deployments", "deploy"]);
const { cleanup, render, screen, waitFor, within, fireEvent } = await import("@testing-library/react");
const userEvent = (await import("@testing-library/user-event")).default;
const { MemoryRouter } = await import("react-router-dom");
const { schedulingApi } = await import("../src/schedulingApi.ts");
const { default: ResidencyTimeline } = await import("../src/pages/scheduling/ResidencyTimeline.tsx");
const { blockOf, ticksOf, tickLabel, cardIntervals, lanesOf, CHART_WIDTH } = await import("../src/pages/scheduling/timelineChart.ts");
const { formatTime } = await import("../src/i18n/datetime.ts");
const { deploymentColor } = await import("../src/pages/scheduling/palette.ts");
type Timeline = import("../src/schedulingApi.ts").SchedulingTimeline;
type BoardNode = import("../src/schedulingApi.ts").BoardNode;

const s = locale.scheduling;
const original = { ...schedulingApi };
const FROM = "2026-10-28T06:00:00Z";
const TO = "2026-10-28T12:00:00Z";
const NODE_WINDOWS = [{ key: "15m", seconds: 900 }, { key: "1h", seconds: 3600 }, { key: "6h", seconds: 21600 }, { key: "24h", seconds: 86400 }];
const at = (hoursAfterFrom: number) => new Date(Date.parse(FROM) + hoursAfterFrom * 3600_000).toISOString();

const nodes: BoardNode[] = [{
  uuid: "node-a", name: "gpu-a", online: true,
  gpus: [
    { index: 0, name: "RTX 8000", memory_bytes: 1, awake_bytes: 0, sleeping_bytes: 0, free_bytes: 1, state: "free", held_outside: false, switching: false, occupants: [] },
    { index: 1, name: "RTX 8000", memory_bytes: 1, awake_bytes: 0, sleeping_bytes: 0, free_bytes: 1, state: "free", held_outside: false, switching: false, occupants: [] },
  ],
}];

function timeline(overrides: Partial<Timeline> = {}): Timeline {
  return {
    scope: "node",
    window: { from: FROM, to: TO, hours: 6, key: "6h", seconds: 21600, custom: false },
    windows: NODE_WINDOWS,
    nodes: [{
      uuid: "node-a", name: "gpu-a",
      gpus: [
        {
          index: 0, truncated: false,
          segments: [
            { deployment_id: 7, deployment: "qwen", instance: "i7", state: "awake", from: at(0), to: at(2) },
            { deployment_id: 7, deployment: "qwen", instance: "i7", state: "switching", from: at(2), to: at(2.5) },
            { deployment_id: 7, deployment: "qwen", instance: "i7", state: "asleep", level: 2, from: at(2.5), to: at(4) },
            { deployment_id: 8, deployment: "llama", instance: "i8", state: "awake", from: at(4), to: at(6) },
          ],
        },
        { index: 1, truncated: true, segments: [{ deployment_id: 9, deployment: "always-on", instance: "i9", state: "starting", from: at(5), to: at(6) }] },
      ],
    }],
    deployments: [
      { deployment_id: 7, deployment: "qwen", yield_policy: "timeslice", awake_seconds: 7200, awake_share: 0.3333, present_seconds: 21600, switches: 3, yielded: 1, yielded_to: 2, wakes: 2, held_requests: 4, wait_p50_ms: 4200, wait_p95_ms: 31000, cards: [{ node_id: 1, node_uuid: "node-a", node: "gpu-a", gpus: [0, 1] }, { node_id: 9, node: "", gpus: [2] }] },
      { deployment_id: 8, deployment: "llama", yield_policy: "anytime", awake_seconds: 30, awake_share: 0.0014, present_seconds: 21600, switches: 0, yielded: 0, yielded_to: 0, wakes: 0, held_requests: 0, cards: [] },
      { deployment_id: 11, deployment: "old-batch", deployment_deleted: true, yield_policy: "", awake_seconds: 6900, awake_share: 0.9583, present_seconds: 7200, switches: 2, yielded: 2, yielded_to: 0, wakes: 0, held_requests: 0, cards: [] },
    ],
    switches_available: true,
    ...overrides,
  };
}

function mount(list: BoardNode[] = nodes) {
  render(
    <I18nextProvider i18n={i18n}>
      <MemoryRouter><ResidencyTimeline nodes={list} /></MemoryRouter>
    </I18nextProvider>,
  );
  return userEvent.setup();
}

async function expandSharing() {
  const trigger = await screen.findByRole("button", { name: new RegExp(s.nodeChart.sharing) });
  fireEvent.click(trigger);
  return document.getElementById(trigger.getAttribute("aria-controls")!)!;
}

afterEach(() => {
  cleanup();
  Object.assign(schedulingApi, original);
});
after(() => closeChannels());

test("the chart geometry is to scale: a block's width is its share of the window and ticks sit on the hours", () => {
  const span = { fromMs: Date.parse(FROM), toMs: Date.parse(TO) };
  const block = blockOf({ deployment_id: 1, deployment: "d", instance: "i", state: "awake", from: at(1.5), to: at(4.5) }, span);
  assert.equal(block.x, 250);
  assert.equal(block.width, 500);
  // A stretch before the window starts at its edge; one shorter than a pixel is still drawn.
  assert.equal(blockOf({ deployment_id: 1, deployment: "d", instance: "i", state: "awake", from: at(-3), to: at(1) }, span).x, 0);
  assert.ok(blockOf({ deployment_id: 1, deployment: "d", instance: "i", state: "awake", from: at(3), to: at(3.0001) }, span).width >= 1);
  assert.equal(blockOf({ deployment_id: 1, deployment: "d", instance: "i", state: "awake", from: at(5.99), to: at(6) }, span).x + blockOf({ deployment_id: 1, deployment: "d", instance: "i", state: "awake", from: at(5.99), to: at(6) }, span).width, CHART_WIDTH);
  const six = ticksOf(span);
  assert.ok(six.length >= 5 && six.length <= 7, `${six.length} hourly ticks in 6 h`);
  for (const tick of six) {
    assert.equal(new Date(tick.ms).getMinutes(), 0);
    assert.equal(tick.x, ((tick.ms - span.fromMs) / (span.toMs - span.fromMs)) * CHART_WIDTH);
  }
  const wide = ticksOf({ fromMs: Date.parse(FROM), toMs: Date.parse(FROM) + 24 * 3600_000 });
  assert.ok(wide.length >= 5 && wide.length <= 7, `${wide.length} four-hour ticks in 24 h`);
  assert.ok(wide.every((tick) => new Date(tick.ms).getHours() % 4 === 0));
});

test("lanes are one per deployment in the order they first held the card; unlisted deployments are resident", () => {
  const lanes = lanesOf(timeline().nodes[0].gpus[0].segments, timeline().deployments);
  assert.deepEqual(lanes.map((lane) => [lane.deployment, lane.policy, lane.segments.length]), [["qwen", "timeslice", 3], ["llama", "anytime", 1]]);
  assert.equal(lanesOf(timeline().nodes[0].gpus[1].segments, timeline().deployments)[0].policy, "never");
  assert.ok(lanes.every((lane) => !lane.deleted));
  const gone = lanesOf([{ deployment_id: 11, deployment: "old-batch", instance: "i11", state: "awake", from: at(0), to: at(2) }], timeline().deployments);
  assert.deepEqual(gone.map((lane) => [lane.deployment, lane.deleted]), [["old-batch", true]]);
});

test("overlapping sleepers and a handoff stay on a shared clock without filling gaps or extending the window", () => {
  const seg = (id: number, state: "awake" | "asleep", start: number, end: number) => ({
    deployment_id: id, deployment: String(id), instance: "i" + id, state, from: at(start), to: at(end),
  });
  const segments = [seg(2, "asleep", 1, 3), seg(1, "asleep", -1, 2), seg(3, "awake", 2, 3),
    seg(4, "awake", 4, 7), seg(5, "awake", 3, 3), seg(6, "awake", -2, -1)];
  const spans = cardIntervals(segments, { fromMs: Date.parse(FROM), toMs: Date.parse(TO) });
  const hour = (ms: number) => (ms - Date.parse(FROM)) / 3600_000;
  assert.deepEqual(spans.map(s => [hour(s.from), hour(s.to), s.residents.map(r => r.deployment_id)]), [
    [0, 1, [1]], [1, 2, [1, 2]], [2, 3, [3, 2]], [4, 6, [4]],
  ]);
});

test("a loading status shows until the timeline answers", async () => {
  let release!: () => void;
  const gate = new Promise<void>((resolve) => { release = resolve; });
  schedulingApi.timeline = async () => { await gate; return timeline(); };
  mount();
  assert.ok(screen.getByRole("status"));
  assert.ok(screen.getByText(s.timeline.loading));
  release();
  await screen.findByRole("row", { name: i18n.t("scheduling:timeline.gpuAria", { node: "gpu-a", index: 0 }) });
  assert.equal(screen.queryByText(s.timeline.loading), null);
});

test("a failed first load shows the error and retries on request", async () => {
  let calls = 0;
  schedulingApi.timeline = async () => {
    calls += 1;
    if (calls === 1) throw new Error("timeline unavailable");
    return timeline();
  };
  const user = mount();
  assert.ok(await screen.findByText(s.timeline.loadFailed));
  assert.ok(screen.getByText("timeline unavailable"));
  await user.click(screen.getByRole("button", { name: s.page.retry }));
  await screen.findByRole("row", { name: i18n.t("scheduling:timeline.gpuAria", { node: "gpu-a", index: 0 }) });
  assert.equal(screen.queryByText(s.timeline.loadFailed), null);
});

test("with no nodes on the page there is nothing to ask for", async () => {
  let asked = 0;
  schedulingApi.timeline = async () => { asked += 1; return timeline(); };
  mount([]);
  assert.ok(screen.getByText(s.timeline.noNodes));
  assert.equal(asked, 0);
});

test("a window nobody held a card in says so, and an empty sharing table says so", async () => {
  schedulingApi.timeline = async () => timeline({
    nodes: [{ uuid: "node-a", name: "gpu-a", gpus: [{ index: 0, truncated: false, segments: [] }] }],
    deployments: [],
  });
  mount();
  assert.ok(await screen.findByText(i18n.t("scheduling:timeline.noneInWindow", { span: i18n.t("scheduling:timeline.hours", { hours: 6 }) })));
  assert.equal(screen.getAllByText(s.nodeChart.emptyWindow).length, nodes[0].gpus.length);
  await expandSharing();
  assert.ok(screen.getByText(s.timeline.sharingEmpty));
});

test("one track per GPU shows handoffs to scale and keeps co-resident deployments inspectable", async () => {
  const data = timeline();
  data.nodes[0].gpus[0].segments.push({ deployment_id: 11, deployment: "old-batch", instance: "old", state: "asleep", level: 1, from: at(0), to: at(2) });
  schedulingApi.timeline = async () => data;
  mount();
  const gpu0 = within(await screen.findByRole("row", { name: i18n.t("scheduling:timeline.gpuAria", { node: "gpu-a", index: 0 }) }));
  const [track] = gpu0.getAllByRole("group");
  assert.equal(gpu0.getAllByRole("group").length, 1);
  const intervals = within(track).getAllByRole("img");
  const blocks = intervals.map(el => el.parentElement!.querySelector("rect[data-state]")!);
  assert.deepEqual(blocks.map(el => Math.round(Number(el.getAttribute("width")))), [333, 83, 250, 333]);
  assert.deepEqual(blocks.map(el => Math.round(Number(el.getAttribute("x")))), [0, 333, 417, 667]);
  assert.equal(blocks[0].getAttribute("fill"), deploymentColor(7));
  assert.equal(blocks[1].getAttribute("stroke"), deploymentColor(7));
  assert.match(blocks[2].getAttribute("fill")!, /-asleep\)$/);
  assert.equal(blocks[3].getAttribute("fill"), deploymentColor(8));
  fireEvent.focus(intervals[0]);
  const tip = await screen.findByRole("tooltip");
  assert.match(tip.textContent!, /qwen · Serving/);
  assert.match(tip.textContent!, /old-batch · Asleep L1/);
  fireEvent.blur(intervals[0]);
  assert.ok(gpu0.getByRole("link", { name: "llama" }));
  const legend = screen.getByRole("group", { name: s.timeline.legend });
  for (const label of [s.timeline.state.awake, s.timeline.state.asleep, s.timeline.state.switching, s.timeline.state.starting]) assert.ok(within(legend).getByText(label), label);
  const ticks = document.querySelectorAll(".timeline-axis-ticks > span");
  assert.ok(ticks.length >= 3, `${ticks.length} ticks`);
  // A truncated card says so; one that is not does not.
  assert.ok(screen.getByText(s.timeline.truncated));
  assert.equal(screen.getAllByText(s.timeline.truncated).length, 1);
});

test("a block names itself to assistive tech and shows a tooltip on hover and on keyboard focus", async () => {
  schedulingApi.timeline = async () => timeline();
  const user = mount();
  const sleeping = await screen.findByRole("img", { name: new RegExp(`qwen · ${i18n.t("scheduling:timeline.state.asleepLevel", { level: 2 })}`) });
  assert.equal(sleeping.getAttribute("tabindex"), "0");
  assert.equal(screen.queryByRole("tooltip"), null);
  // Hover.
  fireEvent.mouseEnter(sleeping);
  const tip = await screen.findByRole("tooltip");
  assert.equal(tip.textContent, sleeping.getAttribute("aria-label"));
  assert.ok(tip.textContent?.includes("qwen"));
  fireEvent.mouseLeave(sleeping);
  assert.equal(screen.queryByRole("tooltip"), null);
  // The keyboard: tab to a block, the tooltip shows, Escape and blur hide it.
  const first = screen.getAllByRole("img", { name: /qwen/ })[0];
  await user.tab();
  for (let step = 0; step < 10 && document.activeElement !== first; step++) await user.tab();
  assert.ok(document.activeElement === first, "the first interval is keyboard accessible");
  assert.ok(screen.getByRole("tooltip").textContent?.includes(i18n.t("scheduling:timeline.state.awake")));
  await user.keyboard("{Escape}");
  assert.equal(screen.queryByRole("tooltip"), null);
  await user.tab();
  assert.ok((await screen.findByRole("tooltip")).textContent?.includes(i18n.t("scheduling:timeline.state.switching")));
  await user.tab({ shift: true });
  assert.ok(document.activeElement === first);
  assert.equal(screen.getByRole("tooltip").textContent, first.getAttribute("aria-label"));
});

const rangeBox = () => screen.getByRole("combobox", { name: i18n.t("deployments:timeRange.ariaLabel") });
const timeRange = (key: string) => i18n.t("deployments:timeRange." + key);

async function pickQuick(user: ReturnType<typeof userEvent.setup>, key: string) {
  await user.click(await screen.findByRole("combobox", { name: i18n.t("deployments:timeRange.ariaLabel") }));
  await user.click(await screen.findByTitle(timeRange(key)));
}

// Answers a quick window with the lanes of its length, as the server does.
const answerWindow = (query: { window?: string }) => {
  const seconds = ({ "15m": 900, "1h": 3600, "6h": 21600, "24h": 86400 } as Record<string, number>)[query.window ?? "6h"];
  const key = query.window ?? "6h";
  return timeline({ window: { from: FROM, to: new Date(Date.parse(FROM) + seconds * 1000).toISOString(), hours: Math.floor(seconds / 3600), key, seconds, custom: false } });
};

test("the range picker offers the server's windows and a custom range, asks for 15 minutes, 1 hour and 24 hours and keeps showing the card meanwhile", async () => {
  const asked: unknown[] = [];
  schedulingApi.timeline = async (query) => {
    asked.push(query);
    return answerWindow(query as { window?: string });
  };
  const user = mount();
  await screen.findByRole("row", { name: i18n.t("scheduling:timeline.gpuAria", { node: "gpu-a", index: 0 }) });
  // The first read leaves the window to the server, whose answer lists the choices.
  assert.deepEqual(asked, [{ nodes: ["node-a"], window: undefined }]);
  const box = await waitFor(() => rangeBox());
  assert.ok(within(box.closest(".ant-select") as HTMLElement).getByTitle(timeRange("6h")), "the picker shows the server's default window");
  await user.click(box);
  const offered = [...document.querySelectorAll(".ant-select-item-option")].map((el) => el.getAttribute("title"));
  assert.deepEqual(offered, [timeRange("15m"), timeRange("1h"), timeRange("6h"), timeRange("1d"), timeRange("custom")]);
  await user.click(await screen.findByTitle(timeRange("15m")));
  await waitFor(() => assert.deepEqual(asked.at(-1), { nodes: ["node-a"], window: "15m" }));
  assert.ok(screen.getByRole("row", { name: i18n.t("scheduling:timeline.gpuAria", { node: "gpu-a", index: 0 }) }));
  await pickQuick(user, "1h");
  await waitFor(() => assert.deepEqual(asked.at(-1), { nodes: ["node-a"], window: "1h" }));
  await pickQuick(user, "1d");
  await waitFor(() => assert.deepEqual(asked.at(-1), { nodes: ["node-a"], window: "24h" }));
  assert.ok(within(rangeBox().closest(".ant-select") as HTMLElement).getByTitle(timeRange("1d")));
});

test("only the windows the server lists are offered", async () => {
  schedulingApi.timeline = async () => timeline({ windows: [{ key: "6h", seconds: 21600 }, { key: "24h", seconds: 86400 }] });
  const user = mount();
  await user.click(await waitFor(() => rangeBox()));
  const offered = [...document.querySelectorAll(".ant-select-item-option")].map((el) => el.getAttribute("title"));
  assert.deepEqual(offered, [timeRange("6h"), timeRange("1d"), timeRange("custom")]);
});

test("the sharing table lists the yield policy, awake time, switches, yields and the held requests' waits", async () => {
  schedulingApi.timeline = async () => timeline();
  mount();
  const sharing = await expandSharing();
  const table = within(within(sharing).getByRole("table"));
  for (const heading of Object.values(s.timeline.columns)) assert.ok(table.getAllByText(heading).length >= 1, heading);
  const qwen = table.getByRole("link", { name: "qwen" }).closest("tr") as HTMLElement;
  const cells = within(qwen);
  assert.ok(cells.getByText(i18n.t("deployments:yieldPolicy.policy.name.timeslice")));
  assert.ok(cells.getByText(i18n.t("scheduling:timeline.awakeShare", { span: i18n.t("deployments:duration.hoursMinutes", { hours: 2, minutes: 0 }), present: i18n.t("deployments:duration.hoursMinutes", { hours: 6, minutes: 0 }), share: 33.3 })));
  // Where it is now: each node with its GPUs, a node no longer known by id.
  const line = (text: string) => cells.getByText((_, el) => el?.tagName === "SPAN" && (el as HTMLElement).style.display === "block" && el.textContent === text);
  assert.ok(line(i18n.t("scheduling:timeline.cardsOnNode", { node: "gpu-a", gpus: "0, 1" })));
  assert.ok(line(i18n.t("scheduling:timeline.cardsOnNode", { node: "#9", gpus: "2" })));
  // A known node's name opens its drawer; a gone one has nothing to open.
  assert.equal(cells.getByRole("link", { name: "gpu-a" }).getAttribute("href"), "/?node=node-a");
  assert.equal(cells.queryByRole("link", { name: "#9" }), null);
  assert.ok(cells.getByText("3")); // switches
  assert.ok(cells.getByText("4")); // held requests
  assert.ok(cells.getByText(i18n.t("scheduling:timeline.wait.seconds", { value: "4.2" })));
  assert.ok(cells.getByText(i18n.t("scheduling:timeline.wait.seconds", { value: "31" })));
  // No held request, no wait figure: a dash, not a zero.
  const llama = within(table.getByRole("link", { name: "llama" }).closest("tr") as HTMLElement);
  // Not placed anywhere, and no wait figures: dashes, not zeros.
  assert.equal(llama.getAllByText("—").length, 3);
  assert.ok(llama.getByText(i18n.t("scheduling:timeline.awakeShare", { span: i18n.t("deployments:duration.seconds", { value: 30 }), present: i18n.t("deployments:duration.hoursMinutes", { hours: 6, minutes: 0 }), share: 0.1 })));
});

test("the stats' title says, from the server's scope, that they count only this node's replicas, by hover or keyboard", async () => {
  schedulingApi.timeline = async () => timeline();
  const user = mount();
  const title = await expandSharing();
  const hint = within(title).getByRole("img", { name: s.timeline.scopeNode });
  assert.equal(hint.getAttribute("tabindex"), "0");
  // Tab to it: the tooltip opens on focus with the sentence.
  for (let i = 0; i < 50 && document.activeElement !== hint; i += 1) await user.tab();
  assert.equal(document.activeElement, hint, "the hint is reachable by keyboard");
  const tip = await screen.findByRole("tooltip");
  assert.equal(tip.textContent, s.timeline.scopeNode);
  // Over the fleet, the same table says nothing.
  cleanup();
  schedulingApi.timeline = async () => timeline({ scope: "fleet" });
  mount();
  const fleetTitle = await expandSharing();
  assert.equal(within(fleetTitle).queryByRole("img"), null);
});

test("a deleted deployment that shared cards is listed greyed and marked, with no link, no policy and its share of the time it existed", async () => {
  const base = timeline();
  const gpus = base.nodes[0].gpus.map((gpu) => gpu.index === 1 ? { ...gpu, segments: [{ deployment_id: 11, deployment: "old-batch", instance: "i11", state: "awake" as const, from: at(0), to: at(2) }, ...gpu.segments] } : gpu);
  schedulingApi.timeline = async () => ({ ...base, nodes: [{ ...base.nodes[0], gpus }] });
  mount();
  const sharing = await expandSharing();
  const history = within(document.getElementById("gpu-1-residency")!);
  assert.ok(history.getByText(i18n.t("scheduling:history.deleted", { name: "old-batch" })));
  assert.equal(history.queryByRole("link", { name: /old-batch/ }), null);
  const table = within(within(sharing).getByRole("table"));
  const name = table.getByText(i18n.t("scheduling:history.deleted", { name: "old-batch" }));
  assert.ok(name.closest(".ant-typography-secondary"), "the deleted name is greyed");
  assert.equal(table.queryByRole("link", { name: /old-batch/ }), null);
  const row = within(name.closest("tr") as HTMLElement);
  // No policy kept: a dash for the level, as for no cards; awake 1 h 55 min
  // of the 2 h it existed.
  assert.equal(row.getAllByText("—").length, 4);
  assert.ok(row.getByText(i18n.t("scheduling:timeline.awakeShare", { span: i18n.t("deployments:duration.hoursMinutes", { hours: 1, minutes: 55 }), present: i18n.t("deployments:duration.hoursMinutes", { hours: 2, minutes: 0 }), share: 95.8 })));
});

test("a failed refresh keeps the last timeline and warns", async () => {
  let calls = 0;
  schedulingApi.timeline = async () => {
    calls += 1;
    if (calls > 1) throw new Error("boom");
    return timeline();
  };
  const user = mount();
  await screen.findByRole("row", { name: i18n.t("scheduling:timeline.gpuAria", { node: "gpu-a", index: 0 }) });
  await pickQuick(user, "1d");
  assert.ok(await screen.findByText(i18n.t("scheduling:timeline.refreshFailed", { detail: "boom" })));
  assert.ok(screen.getByRole("row", { name: i18n.t("scheduling:timeline.gpuAria", { node: "gpu-a", index: 0 }) }));
});

// Picks a custom range in the shared picker: the range option reveals the date
// inputs, the typed dates are confirmed with OK.
async function pickCustom(user: ReturnType<typeof userEvent.setup>, start: string, end: string) {
  await user.click(await screen.findByRole("combobox", { name: i18n.t("deployments:timeRange.ariaLabel") }));
  await user.click(await screen.findByTitle(timeRange("custom")));
  const inputs = () => [...document.querySelectorAll<HTMLInputElement>(".ant-picker-input input")];
  await waitFor(() => assert.equal(inputs().length, 2));
  await user.click(inputs()[0]);
  await user.type(inputs()[0], start + "{Enter}");
  await user.type(inputs()[1], end + "{Enter}");
  // The OK button animates in with pointer events off; a click event is what a press delivers.
  fireEvent.click(document.querySelector(".ant-picker-ok button") as HTMLElement);
}

// The intervals the page set up, to tell a polled range from one read once.
function watchIntervals() {
  const seen: number[] = [];
  const original = globalThis.setInterval;
  globalThis.setInterval = ((handler: TimerHandler, ms?: number, ...args: unknown[]) => {
    seen.push(ms ?? 0);
    return original(handler, ms, ...args);
  }) as typeof setInterval;
  return { seen, restore: () => { globalThis.setInterval = original; } };
}

const CUSTOM_START = "2026-10-28 06:00:00";
const CUSTOM_END = "2026-10-28 07:30:00";
const local = (text: string) => new Date(text.replace(" ", "T")).getTime();
const customWindow = () => ({
  from: new Date(local(CUSTOM_START)).toISOString(), to: new Date(local(CUSTOM_END)).toISOString(), hours: 1, key: "custom", seconds: 5400, custom: true,
});

test("a custom range is asked for by since and until, is read once and is not polled; a quick window is polled", async () => {
  const intervals = watchIntervals();
  try {
    const asked: unknown[] = [];
    schedulingApi.timeline = async (query) => {
      asked.push(query);
      if ("since" in query) return timeline({ window: customWindow() });
      return answerWindow(query as { window?: string });
    };
    const user = mount();
    await screen.findByRole("row", { name: i18n.t("scheduling:timeline.gpuAria", { node: "gpu-a", index: 0 }) });
    const polled = () => intervals.seen.filter((ms) => ms === 60_000).length;
    assert.equal(polled(), 1, "the default window is polled once a minute");
    await pickCustom(user, CUSTOM_START, CUSTOM_END);
    await waitFor(() => assert.deepEqual(asked.at(-1), {
      nodes: ["node-a"], since: new Date(local(CUSTOM_START)).toISOString().replace(".000Z", "Z"), until: new Date(local(CUSTOM_END)).toISOString().replace(".000Z", "Z"),
    }));
    // The answer is drawn, the picker shows the range, and nothing polls it.
    await screen.findByRole("row", { name: i18n.t("scheduling:timeline.gpuAria", { node: "gpu-a", index: 0 }) });
    assert.deepEqual([...document.querySelectorAll<HTMLInputElement>(".ant-picker-input input")].map((input) => input.value), [CUSTOM_START, CUSTOM_END]);
    const polledAtCustom = polled();
    const readsAtCustom = asked.length;
    assert.equal(polledAtCustom, 1, "a custom range sets up no 1-minute poll");
    await new Promise((resolve) => setTimeout(resolve, 100));
    assert.equal(asked.length, readsAtCustom, "and is not read again");
    // Back to a quick window: polled again, and no custom bounds in the query.
    await pickQuick(user, "1h");
    await waitFor(() => assert.deepEqual(asked.at(-1), { nodes: ["node-a"], window: "1h" }));
    assert.equal(polled(), 2);
  } finally {
    intervals.restore();
  }
});

test("a range nobody held a card in says which: the last 15 minutes by length, a custom range by its ends", async () => {
  const empty = { nodes: [{ uuid: "node-a", name: "gpu-a", gpus: [{ index: 0, truncated: false, segments: [] }] }], deployments: [] };
  schedulingApi.timeline = async (query) => {
    if ("since" in query) return timeline({ ...empty, window: customWindow() });
    return { ...answerWindow(query as { window?: string }), ...empty };
  };
  const user = mount();
  assert.ok(await screen.findByText(i18n.t("scheduling:timeline.noneInWindow", { span: i18n.t("scheduling:timeline.hours", { hours: 6 }) })));
  await pickQuick(user, "15m");
  assert.ok(await screen.findByText(i18n.t("scheduling:timeline.noneInWindow", { span: i18n.t("scheduling:timeline.minutes", { minutes: 15 }) })));
  await pickCustom(user, CUSTOM_START, CUSTOM_END);
  const w = customWindow();
  assert.ok(await screen.findByText(i18n.t("scheduling:timeline.noneInRange", { from: formatTime(w.from), to: formatTime(w.to) })));
  assert.equal(screen.queryByText(new RegExp(i18n.t("scheduling:timeline.noneInWindow", { span: "X" }).split("X")[0])), null);
});

test("a custom range the server refuses keeps the last timeline and says why; a refused first read says it too", async () => {
  schedulingApi.timeline = async (query) => {
    if ("since" in query) throw new Error("since is at most 7 days before until");
    return answerWindow(query as { window?: string });
  };
  const user = mount();
  await screen.findByRole("row", { name: i18n.t("scheduling:timeline.gpuAria", { node: "gpu-a", index: 0 }) });
  await pickCustom(user, CUSTOM_START, CUSTOM_END);
  assert.ok(await screen.findByText(i18n.t("scheduling:timeline.refreshFailed", { detail: "since is at most 7 days before until" })));
  assert.ok(screen.getByRole("row", { name: i18n.t("scheduling:timeline.gpuAria", { node: "gpu-a", index: 0 }) }));
  // Retrying asks again.
  let calls = 0;
  schedulingApi.timeline = async (query) => {
    calls += 1;
    if ("since" in query) return timeline({ window: customWindow() });
    return answerWindow(query as { window?: string });
  };
  await user.click(screen.getByRole("button", { name: s.page.retry }));
  await waitFor(() => assert.equal(calls, 1));
  await waitFor(() => assert.equal(screen.queryByText(i18n.t("scheduling:timeline.refreshFailed", { detail: "since is at most 7 days before until" })), null));
});

test("when the server cannot give the switch counts for the range, the columns show dashes and a note says why", async () => {
  schedulingApi.timeline = async (query) => {
    if ("since" in query) return timeline({ window: customWindow(), switches_available: false });
    return answerWindow(query as { window?: string });
  };
  const user = mount();
  const qwenRow = () => within([...sharing.querySelectorAll<HTMLElement>("tbody tr")].find((tr) => tr.textContent?.includes("qwen")) as HTMLElement);
  const sharing = await expandSharing();
  const qwen = qwenRow();
  assert.ok(qwen.getByText("3"), "the counts are shown when the server has them");
  assert.equal(screen.queryByText(s.timeline.switchesUnavailable), null);
  await pickCustom(user, CUSTOM_START, CUSTOM_END);
  assert.ok(await screen.findByText(s.timeline.switchesUnavailable));
  const after = qwenRow();
  assert.equal(after.queryByText("3"), null, "switches");
  assert.equal(after.queryByText("2"), null, "yielded to");
  // Switches, yielded and yielded-to are dashes; the held requests and the awake time are still figures.
  assert.ok(after.getAllByText("—").length >= 3);
  assert.ok(after.getByText("4"), "held requests");
});

test("ticks follow the range: minutes for a quarter hour, ten minutes for an hour, hours for a day, days for a week", () => {
  const start = new Date(2026, 9, 28, 6, 7, 30).getTime();
  const spanOf = (ms: number) => ({ fromMs: start, toMs: start + ms });
  const clock = (ms: number) => new Date(ms);
  const quarter = ticksOf(spanOf(15 * 60_000));
  assert.ok(quarter.length >= 4 && quarter.length <= 7, String(quarter.length));
  assert.ok(quarter.every((tick) => clock(tick.ms).getMinutes() % 3 === 0 && clock(tick.ms).getSeconds() === 0));
  const hour = ticksOf(spanOf(3600_000));
  assert.ok(hour.length >= 5 && hour.length <= 7 && hour.every((tick) => clock(tick.ms).getMinutes() % 10 === 0), hour.length + " ticks in an hour");
  const day = ticksOf(spanOf(86_400_000));
  assert.ok(day.length >= 5 && day.length <= 7 && day.every((tick) => clock(tick.ms).getHours() % 4 === 0 && clock(tick.ms).getMinutes() === 0));
  const week = ticksOf(spanOf(7 * 86_400_000));
  assert.ok(week.length >= 6 && week.length <= 8 && week.every((tick) => clock(tick.ms).getHours() === 0 && clock(tick.ms).getMinutes() === 0), week.length + " ticks in a week");
  // Every tick is inside the range and sits where its time is on the track.
  for (const [ms, ticks] of [[15 * 60_000, quarter], [7 * 86_400_000, week]] as const) {
    for (const tick of ticks) {
      assert.ok(tick.ms >= start && tick.ms <= start + ms);
      assert.equal(tick.x, ((tick.ms - start) / ms) * CHART_WIDTH);
    }
  }
  // Labels: the clock inside a day, date and time over a day, the date alone for day ticks.
  const label = tickLabel(quarter[0].ms, spanOf(15 * 60_000));
  assert.match(label, /^\d\d:\d\d$/);
  assert.match(tickLabel(day[0].ms, spanOf(86_400_000)), /^\d\d:\d\d$/);
  assert.match(tickLabel(new Date(2026, 9, 29, 12, 0).getTime(), spanOf(2 * 86_400_000)), /^10-29 12:00$/);
  assert.match(tickLabel(week[0].ms, spanOf(7 * 86_400_000)), /^\d\d-\d\d$/);
});

test("the picker follows the answered window: 15 minutes, then a custom range, shows each", async () => {
  schedulingApi.timeline = async (query) => {
    if ("since" in query) return timeline({ window: customWindow() });
    return answerWindow(query as { window?: string });
  };
  const user = mount();
  await screen.findByRole("row", { name: i18n.t("scheduling:timeline.gpuAria", { node: "gpu-a", index: 0 }) });
  await pickQuick(user, "15m");
  await waitFor(() => assert.ok(within(rangeBox().closest(".ant-select") as HTMLElement).getByTitle(timeRange("15m"))));
  // The lanes' axis is the 15 minute window: ticks at whole minutes.
  const labels = [...document.querySelectorAll(".timeline-axis-ticks > span")].map((el) => el.textContent ?? "");
  assert.ok(labels.length >= 2 && labels.every((text) => /^\d\d:\d\d$/.test(text)), labels.join(","));
  await pickCustom(user, CUSTOM_START, CUSTOM_END);
  await waitFor(() => assert.ok(within(rangeBox().closest(".ant-select") as HTMLElement).getByTitle(timeRange("custom"))));
});
