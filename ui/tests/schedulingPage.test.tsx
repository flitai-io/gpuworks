import assert from "node:assert/strict";
import { after, afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import { createI18n, dom } from "./helpers/dom.ts";
import { trackMessageChannels } from "./helpers/trackedMessageChannels.ts";
import { smallFleet } from "./helpers/overviewFixtures.ts";

// The scheduling center: the overview's loading / empty / error / success
// states, the fleet summary, the heatmap with its filters, the node drawer that
// reads one node in full, the queues (pending reason worded like the deployment
// drawer words it, upcoming, waiting wakes cancelled from the keyboard) and the
// sharing stats.

Object.defineProperty(globalThis, "Document", { value: dom.window.Document, configurable: true });
const closeChannels = trackMessageChannels();
const { i18n, locale } = await createI18n(["scheduling", "deployments", "deploy"]);
const { act, cleanup, fireEvent, render, screen, waitFor, within } = await import("@testing-library/react");
const userEvent = (await import("@testing-library/user-event")).default;
const { MemoryRouter, Route, Routes, useLocation, useNavigate } = await import("react-router-dom");
const { api } = await import("../src/api.ts");
const { schedulingApi } = await import("../src/schedulingApi.ts");
const { APIError } = await import("../src/apiError.ts");
const { default: SchedulingPage } = await import("../src/pages/SchedulingPage.tsx");
type Board = import("../src/schedulingApi.ts").SchedulingBoard;
type Overview = import("../src/schedulingApi.ts").SchedulingOverview;
type Timeline = import("../src/schedulingApi.ts").SchedulingTimeline;
type Activity = import("../src/schedulingApi.ts").SchedulingActivity;

const GIB = 1024 ** 3;
const s = locale.scheduling;
const original = { endWakeWaiting: api.endWakeWaiting };
const originalScheduling = { ...schedulingApi };

/** The fleet-wide lists of the board, as GET /scheduling/board?queues=1 answers. */
function queues(overrides: Partial<Board> = {}): Board {
  return {
    nodes: { items: [], page: 1, page_size: 10, total: 0 },
    pending: [{ deployment_id: 9, deployment: "big-model", reason: { code: "pool_empty", params: { nodes: 3, unmet: 1, cordoned: 1, offline: 1 } }, at: "2026-10-25T08:00:00Z" }],
    waiting_wakes: [{ deployment_id: 8, deployment: "llama", instances: ["inst-llama-0001"], since: "2026-10-25T09:00:00Z" }],
    upcoming: [
      { deployment_id: 7, deployment: "qwen", schedule_id: 1, action: "sleep", sleep_level: 2, at: "2026-10-25T22:00:00Z" },
      { deployment_id: 7, deployment: "qwen", schedule_id: 2, action: "replicas", replicas: 3, at: "2026-10-26T06:00:00Z" },
    ],
    ...overrides,
  };
}

/** One node read in full, as GET /scheduling/board?node= answers. */
function nodeBoard(uuid: string): Board {
  return queues({
    nodes: {
      items: [{
        uuid, name: "gpu-a", online: true,
        gpus: [
          {
            index: 0, name: "L20", memory_bytes: 48 * GIB, awake_bytes: 20 * GIB, sleeping_bytes: 4 * GIB, free_bytes: 24 * GIB, state: "awake", held_outside: false, switching: false,
            occupants: [
              { deployment_id: 7, deployment: "qwen", yield_policy: "idle", instance: "inst-qwen-0001", state: "awake", claim_bytes: 20 * GIB },
              { deployment_id: 8, deployment: "llama", yield_policy: "timeslice", instance: "inst-llama-0001", state: "asleep", sleep_level: 2, claim_bytes: 20 * GIB, residue_bytes: 4 * GIB },
            ],
          },
          { index: 1, name: "L20", memory_bytes: 48 * GIB, awake_bytes: 0, sleeping_bytes: 0, free_bytes: 40 * GIB, state: "held_outside", held_outside: true, switching: false, occupants: [] },
        ],
      }],
      page: 1, page_size: 1, total: 1,
    },
    pending: [], waiting_wakes: [], upcoming: [],
  });
}

const SHARING_WINDOWS = [{ key: "15m", seconds: 900 }, { key: "1h", seconds: 3600 }, { key: "6h", seconds: 21600 }, { key: "24h", seconds: 86400 }];
const sharing = (key = "6h"): Timeline => ({
  scope: "fleet",
  window: { from: "2026-10-28T06:00:00Z", to: "2026-10-28T12:00:00Z", hours: 6, key, seconds: SHARING_WINDOWS.find((w) => w.key === key)?.seconds ?? 21600, custom: false },
  windows: SHARING_WINDOWS, nodes: [], switches_available: true,
  deployments: [{ deployment_id: 8, deployment: "llama", yield_policy: "timeslice", awake_seconds: 3600, awake_share: 0.25, present_seconds: 14400, switches: 3, yielded: 2, yielded_to: 1, wakes: 1, held_requests: 6, wait_p50_ms: 4200, wait_p95_ms: 31000 }],
});

interface Calls { board: unknown[]; timeline: unknown[]; activity: unknown[] }

const activity = (): Activity => ({
  from: "2026-10-27T12:00:00Z", to: "2026-10-28T12:00:00Z", bucket_seconds: 900,
  reasons: [{ key: "idle_yield", count: 0 }, { key: "rank_preemption", count: 0 }], buckets: [], top_cards: [], beyond_retention: false,
});

function serve(options: { overview?: () => Promise<Overview>; board?: () => Board; sharing?: (key?: string) => Timeline } = {}): Calls {
  const calls: Calls = { board: [], timeline: [], activity: [] };
  // The activity card has its own tests (schedulingActivity.test.tsx).
  schedulingApi.activity = async (range) => {
    calls.activity.push(range);
    return activity();
  };
  schedulingApi.overview = options.overview ?? (async () => smallFleet());
  schedulingApi.board = async (query) => {
    calls.board.push(query);
    if ("node" in query) return nodeBoard(query.node);
    if ("queues" in query) return (options.board ?? queues)();
    throw new Error(`the page must not ask for a page of nodes: ${JSON.stringify(query)}`);
  };
  schedulingApi.timeline = async (query) => {
    calls.timeline.push(query);
    if ("sharing" in query) return (options.sharing ?? sharing)(query.window);
    return {
      scope: "node",
      window: { from: "2026-10-28T06:00:00Z", to: "2026-10-28T12:00:00Z", hours: 6, key: "6h", seconds: 21600, custom: false },
      windows: [{ key: "6h", seconds: 21600 }, { key: "24h", seconds: 86400 }], nodes: [], deployments: [], switches_available: true,
    };
  };
  // The scheduling history has its own tests (schedulingHistory.test.tsx).
  schedulingApi.history = async () => ({ items: [{
    id: 1, created_at: "2026-10-25T07:00:00Z", deployment_id: 7, deployment: "qwen", kind: "automation", code: "AUTO_SLEEP", category: "auto",
    params: { trigger: "idle", level: 2, idle_sec: 900 }, nodes: [{ uuid: "node-c", name: "gpu-c" }],
  }] });
  return calls;
}

let navigateBack: () => void;
function Probe() {
  const location = useLocation();
  const navigate = useNavigate();
  React.useEffect(() => { navigateBack = () => navigate(-1); }, [navigate]);
  return <div data-testid="loc">{location.pathname}{location.search}</div>;
}

function mount(path = "/scheduling") {
  render(
    <I18nextProvider i18n={i18n}>
      <MemoryRouter initialEntries={[path]}>
        <Routes>
          <Route path="/scheduling" element={<SchedulingPage />} />
          <Route path="*" element={<div>elsewhere</div>} />
        </Routes>
        <Probe />
      </MemoryRouter>
    </I18nextProvider>,
  );
  return userEvent.setup();
}

afterEach(() => {
  cleanup();
  Object.assign(api, original);
  Object.assign(schedulingApi, originalScheduling);
});
after(() => closeChannels());

const cells = () => [...document.querySelectorAll<HTMLButtonElement>("button.hm-cell")];
const status = () => screen.getAllByRole("status").map((el) => el.textContent).join(" | ");

/** choose picks an option of an antd select by its label. */
async function choose(name: string, option: string) {
  fireEvent.mouseDown(screen.getByRole("combobox", { name }));
  const found = await waitFor(() => {
    const el = document.querySelector<HTMLElement>(`.ant-select-item-option[title="${option}"]`);
    assert.ok(el, `no option ${option}`);
    return el;
  });
  fireEvent.click(found);
}

test("a loading status shows until the overview answers", async () => {
  let release!: () => void;
  const gate = new Promise<void>((resolve) => { release = resolve; });
  serve({ overview: async () => { await gate; return smallFleet(); } });
  mount();
  assert.ok(screen.getByRole("status"));
  assert.ok(screen.getByText(s.page.loading));
  release();
  await screen.findByRole("group", { name: s.heatmap.aria });
  assert.equal(screen.queryByText(s.page.loading), null);
});

test("a failed first load shows the error and retries on request", async () => {
  let calls = 0;
  serve({ overview: async () => { calls += 1; if (calls === 1) throw new Error("overview unavailable"); return smallFleet(); } });
  const user = mount();
  assert.ok(await screen.findByText(s.page.loadFailed));
  assert.ok(screen.getByText("overview unavailable"));
  await user.click(screen.getByRole("button", { name: s.page.retry }));
  await screen.findByRole("group", { name: s.heatmap.aria });
  assert.equal(screen.queryByText(s.page.loadFailed), null);
});

test("an empty fleet says so, and every queue says it is empty", async () => {
  serve({
    overview: async () => ({ summary: { nodes: 0, online_nodes: 0, cards: 0, awake: 0, asleep: 0, free: 0, held_outside: 0, unhealthy: 0, offline: 0, contention: { waiting_wakes: 0, held_requests: 0, yields_last_hour: 0 } }, states: ["offline", "unhealthy", "held_outside", "awake", "asleep", "free"], models: [], deployments: [], nodes: [] }),
    board: () => queues({ pending: [], waiting_wakes: [], upcoming: [] }),
  });
  mount();
  await screen.findByText(s.nodes.empty);
  for (const section of [s.pending, s.waiting, s.upcoming]) {
    const card = await screen.findByRole("region", { name: section.title });
    assert.ok(within(card).getByText(section.empty));
  }
  assert.equal(screen.queryByRole("group", { name: s.heatmap.aria }), null);
  assert.equal(within(screen.getByRole("group", { name: s.summary.cardsTitle })).queryByRole("img"), null);
});

test("a queued wake is actionable directly from its card above the GPU heatmap", async () => {
  serve({ board: () => queues({ pending: [] }) });
  mount();
  const panel = await screen.findByRole("region", { name: s.waiting.title });
  assert.ok(within(panel).getByRole("button", { name: i18n.t("scheduling:waiting.cancelOf", { name: "llama" }) }));
  const heatmap = screen.getByRole("group", { name: s.heatmap.aria });
  assert.ok(panel.compareDocumentPosition(heatmap) & dom.window.Node.DOCUMENT_POSITION_FOLLOWING);
});

test("future plans remain visible alongside both empty deployment queues", async () => {
  serve({ board: () => queues({ pending: [], waiting_wakes: [] }) });
  mount();
  const plans = await screen.findByRole("region", { name: s.upcoming.title });
  assert.ok(within(plans).getByText(i18n.t("scheduling:upcoming.action.replicas", { count: 3 })));
  const waitingGroup = screen.getByText(i18n.t("scheduling:queues.currentWaiting")).closest(".ant-card")!;
  assert.ok(within(waitingGroup).getByText(s.waiting.empty));
  assert.ok(within(waitingGroup).getByText(s.pending.empty));
  assert.ok(!waitingGroup.contains(plans));
});

test("the summary counts the cards by state and the contention, with the queues' counts", async () => {
  serve();
  mount();
  const summary = await screen.findByRole("group", { name: s.summary.cardsTitle });
  assert.equal(within(summary).getByText(s.summary.total).parentElement?.querySelector("strong")?.textContent, "6");
  const distribution = within(summary).getByRole("img", { name: new RegExp(s.states.awake + " 2 / 6") });
  const shares = Array.from(distribution.children, (segment) => parseFloat((segment as HTMLElement).style.width));
  assert.equal(shares.length, 5, "only nonempty GPU states take up space");
  assert.ok(Math.abs(shares[0] - 100 * 2 / 6) < 0.001, "the denominator is the GPU count, not the node count");
  assert.ok(Math.abs(shares.reduce((sum, share) => sum + share, 0) - 100) < 0.001);
  assert.ok(within(summary).getByText(i18n.t("scheduling:summary.nodesOnline", { online: 2, count: 3 })));
  for (const [state, count] of [["awake", "2"], ["asleep", "1"], ["free", "1"], ["held_outside", "1"], ["offline", "1"]]) {
    assert.equal(within(summary).getByText(s.states[state as keyof typeof s.states]).closest("li")?.querySelector("strong")?.textContent, count);
  }
  assert.equal(within(summary).getByText(s.summary.yields).querySelector("strong")?.textContent, "2");
  const waiting = await screen.findByRole("region", { name: s.waiting.title });
  assert.ok(within(waiting).getByText(i18n.t("scheduling:waiting.requestCount", { count: 3 }), { exact: false }));
  for (const [title, count] of [[s.pending.title, 1], [s.waiting.title, 1], [s.upcoming.title, 2]] as const) {
    const queue = await screen.findByRole("region", { name: title });
    assert.ok(within(queue).getByRole("button", { name: i18n.t("scheduling:queues.showAll", { count }) }));
  }
});

test("the heatmap shows every card and says how many are shown; the page never reads a page of nodes", async () => {
  const calls = serve();
  mount();
  await screen.findByRole("group", { name: s.heatmap.aria });
  assert.equal(cells().length, 6);
  assert.ok(status().includes(i18n.t("scheduling:heatmap.shown", { nodes: 3, cards: 6 })));
  assert.deepEqual(calls.board, [{ queues: true }]);
  const legend = screen.getByRole("list", { name: s.nodes.legend.title });
  for (const label of [s.states.awake, s.states.asleep, s.states.free, s.states.held_outside, s.states.offline, s.heatmap.legendSwitching]) assert.ok(within(legend).getByText(label), label);
});

test("filters narrow the heatmap by model, node label and state, and a search rings the deployment's cards", async () => {
  serve();
  const user = mount();
  await screen.findByRole("group", { name: s.heatmap.aria });
  await choose(s.filters.models, "H100");
  await waitFor(() => assert.equal(cells().length, 3));
  assert.ok(status().includes(i18n.t("scheduling:heatmap.shown", { nodes: 1, cards: 3 })));
  await user.click(screen.getByRole("button", { name: s.filters.reset }));
  await waitFor(() => assert.equal(cells().length, 6));

  await choose(s.filters.label, "zone=a");
  await waitFor(() => assert.equal(cells().length, 2));
  await user.click(screen.getByRole("button", { name: s.filters.reset }));

  await choose(s.filters.state, s.filters.stateOptions.offline);
  await waitFor(() => assert.equal(cells().length, 1));
  await user.click(screen.getByRole("button", { name: s.filters.reset }));

  await user.type(screen.getByRole("textbox", { name: s.filters.search }), "night");
  await waitFor(() => assert.ok(status().includes(i18n.t("scheduling:heatmap.highlighted", { cards: 2 }))));
  assert.equal(cells().filter((cell) => cell.classList.contains("is-hit")).length, 2);
  await user.clear(screen.getByRole("textbox", { name: s.filters.search }));
  await user.type(screen.getByRole("textbox", { name: s.filters.search }), "nothing like it");
  await waitFor(() => assert.ok(status().includes(s.heatmap.noDeployment)));
});

test("a filter that matches nothing says so instead of drawing an empty grid", async () => {
  serve();
  const user = mount();
  await screen.findByRole("group", { name: s.heatmap.aria });
  await choose(s.filters.models, "H100");
  await choose(s.filters.label, "zone=a");
  await screen.findByText(s.heatmap.emptyFiltered);
  assert.equal(screen.queryByRole("group", { name: s.heatmap.aria }), null);
  await user.click(screen.getByRole("button", { name: s.filters.reset }));
  await screen.findByRole("group", { name: s.heatmap.aria });
});

test("grouping by a label and sorting by contention reorder the nodes", async () => {
  serve();
  mount();
  await screen.findByRole("group", { name: s.heatmap.aria });
  await choose(s.filters.groupBy, i18n.t("scheduling:filters.group.label", { key: "zone" }));
  const sections = await waitFor(() => {
    const grid = screen.getByRole("group", { name: s.heatmap.aria });
    const found = within(grid).getAllByRole("region");
    assert.equal(found.length, 3);
    return found;
  });
  assert.deepEqual(sections.map((el) => el.getAttribute("aria-label")), ["a", "c", s.heatmap.noLabel]);
  await choose(s.filters.sortBy, s.filters.sort.contention);
  const order = () => [...document.querySelectorAll(".hm-node")].map((el) => el.getAttribute("aria-label"));
  await waitFor(() => assert.deepEqual(order(), ["gpu-a", "gpu-c", "gpu-b"]));
});

test("opening a node reads it in full: each GPU as a bar with its holders, and its residency timeline", async () => {
  const calls = serve();
  const user = mount();
  await screen.findByRole("group", { name: s.heatmap.aria });
  assert.ok(!calls.board.some((query) => "node" in (query as object)), "no node is read until one is opened");
  await user.click(screen.getByRole("button", { name: new RegExp("^gpu-a GPU 0,") }));
  const drawer = await screen.findByRole("dialog");
  await within(drawer).findByRole("region", { name: "gpu-a" });
  assert.deepEqual(calls.board.at(-1), { node: "node-a" });
  const memory = within(within(drawer).getByRole("region", { name: "gpu-a" }));
  const gpu = memory.getByText("GPU 0").closest("tr")!;
  const proportions = [...gpu.querySelectorAll<HTMLElement>(".residency-meter i")].map(segment => Math.round(parseFloat(segment.style.width)));
  assert.deepEqual(proportions, [42, 8]);
  assert.match(gpu.textContent ?? "", /24.0 GiB/);
  assert.match(within(gpu).getByRole("link", { name: "qwen" }).parentElement?.textContent ?? "", /20.0 GiB/);
  assert.match(within(gpu).getByRole("link", { name: "llama" }).parentElement?.textContent ?? "", /L2.*4.0 GiB/);
  const external = memory.getByText(s.states.held_outside).closest("tr")!;
  assert.match(external.textContent ?? "", new RegExp(s.nodeChart.unknown));
  assert.ok(external.querySelector('.residency-meter[data-balanced="false"]'));
  assert.equal(external.querySelector(".residency-meter i"), null);
  assert.ok(within(drawer).getByText(s.timeline.title));
  await waitFor(() => assert.deepEqual(calls.timeline.find((query) => "nodes" in (query as object)), { nodes: ["node-a"], window: undefined }));
  await user.click(within(gpu).getByRole("link", { name: "qwen" }));
  assert.equal(screen.getByTestId("loc").textContent, "/deployments?deployment=7");
});

test("a node that cannot be read says so in its drawer and retries", async () => {
  serve();
  let calls = 0;
  const base = schedulingApi.board;
  schedulingApi.board = async (query) => {
    if ("node" in query) { calls += 1; if (calls === 1) throw new Error("node unavailable"); }
    return base(query);
  };
  const user = mount();
  await screen.findByRole("group", { name: s.heatmap.aria });
  await user.click(screen.getByRole("button", { name: new RegExp("^gpu-a GPU 0,") }));
  const drawer = await screen.findByRole("dialog");
  assert.ok(await within(drawer).findByText(s.drawer.loadFailed));
  assert.ok(within(drawer).getByText("node unavailable"));
  await user.click(within(drawer).getByRole("button", { name: s.page.retry }));
  await within(drawer).findByRole("region", { name: "gpu-a" });
});

test("the open node is in the URL: opening sets ?node=, closing clears it, and the browser goes back to it", async () => {
  serve();
  const user = mount();
  await screen.findByRole("group", { name: s.heatmap.aria });
  await user.click(screen.getByRole("button", { name: new RegExp("^gpu-a GPU 0,") }));
  await within(await screen.findByRole("dialog")).findByRole("region", { name: "gpu-a" });
  assert.equal(screen.getByTestId("loc").textContent, "/scheduling?node=node-a");
  await user.click(within(screen.getByRole("dialog")).getByRole("button", { name: "Close" }));
  await waitFor(() => assert.equal(screen.getByTestId("loc").textContent, "/scheduling"));
  await waitFor(() => assert.equal(document.querySelector('[role="dialog"]'), null));
  await act(async () => navigateBack());
  assert.equal(screen.getByTestId("loc").textContent, "/scheduling?node=node-a");
  await within(await screen.findByRole("dialog")).findByRole("region", { name: "gpu-a" });
  await act(async () => navigateBack());
  assert.equal(screen.getByTestId("loc").textContent, "/scheduling");
  await waitFor(() => assert.equal(document.querySelector('[role="dialog"]'), null));
});

test("a link to /scheduling?node= opens that node's drawer, titled with its name", async () => {
  const calls = serve();
  mount("/scheduling?node=node-c");
  const drawer = await screen.findByRole("dialog");
  assert.ok(within(drawer).getByText("gpu-c"));
  await waitFor(() => assert.deepEqual(calls.board.find((query) => "node" in (query as object)), { node: "node-c" }));
});

test("a node named in the scheduling history opens its drawer on the page", async () => {
  serve();
  const user = mount();
  const tab = await screen.findByRole("tab", { name: s.history.title });
  const heatmap = screen.getByRole("group", { name: s.heatmap.aria }).closest(".ant-card")!;
  assert.ok(heatmap.nextElementSibling?.contains(tab), "history must be reachable directly below the heatmap");
  await user.click(tab);
  const history = await screen.findByRole("tabpanel", { name: s.history.title });
  await user.click(await within(history).findByRole("link", { name: "gpu-c" }));
  assert.equal(screen.getByTestId("loc").textContent, "/scheduling?node=node-c");
  const drawer = await screen.findByRole("dialog");
  assert.ok(within(drawer).getByText("gpu-c"));
});

test("a node can be opened from the keyboard alone", async () => {
  serve();
  const user = mount();
  await screen.findByRole("group", { name: s.heatmap.aria });
  cells()[0].focus();
  await user.keyboard("{ArrowRight}{ArrowRight}{Enter}");
  const drawer = await screen.findByRole("dialog");
  assert.ok(within(drawer).getByText("gpu-b"), "the third card is gpu-b's, and its name titles the drawer");
});

test("pending reasons read like the deployment drawer's placement alert; upcoming entries are worded; the history is available in its tab", async () => {
  serve();
  const user = mount();
  await screen.findByRole("region", { name: s.pending.title });
  assert.ok(screen.getByText(i18n.t("deploy:schedule.pool_empty", { nodes: 3, unmet: 1, cordoned: 1, offline: 1 })));
  assert.ok(screen.getByText(i18n.t("scheduling:upcoming.action.sleep", { level: 2 })));
  assert.ok(screen.getByText(i18n.t("scheduling:upcoming.action.replicas", { count: 3 })));
  await user.click(screen.getByRole("tab", { name: s.history.title }));
  assert.ok(await screen.findByText(i18n.t("deployments:instance.event.AUTO_SLEEP", {
    level: 2, cause: i18n.t("deployments:automation.cause.idle", { idle: "15 min" }),
  })));
  assert.ok(screen.getByText(s.history.title));
});

test("the placement table distinguishes same-name deployments, shows reason and time, and pages in place", async () => {
  const sample = queues().pending[0];
  const pending = Array.from({ length: 21 }, (_, index) => ({
    ...sample, deployment_id: index + 1, deployment: index < 2 ? "same-name" : `pending-${index + 1}`,
  }));
  serve({ board: () => queues({ pending }) });
  const user = mount();
  const overview = await screen.findByRole("region", { name: s.pending.title });
  await user.click(within(overview).getByRole("button", { name: i18n.t("scheduling:queues.showAll", { count: 21 }) }));
  const panel = await screen.findByRole("dialog");
  const links = await within(panel).findAllByRole("link", { name: "same-name" });
  assert.deepEqual(links.map((link) => link.getAttribute("href")), ["/deployments?deployment=1", "/deployments?deployment=2"]);
  for (const [i, link] of links.entries()) {
    const row = within(link.closest("tr")!);
    assert.ok(row.getByText(`#${i + 1}`));
    assert.ok(row.getByText(i18n.t("deploy:schedule.pool_empty", sample.reason.params)));
    assert.ok(row.getByText(/2026-10-25/));
  }
  assert.ok(within(panel).getByRole("link", { name: "pending-20" }));
  assert.equal(within(panel).queryByRole("link", { name: "pending-21" }), null);
  await user.click(within(panel).getByRole("button", { name: i18n.t("common:resourceTable.nextPage") }));
  assert.equal((await within(panel).findByRole("link", { name: "pending-21" })).getAttribute("href"), "/deployments?deployment=21");
});

test("one shared placement cause opens only the affected deployments, even across different candidate nodes", async () => {
  const base = queues().pending[0];
  const pending = ["node-a", "node-b"].map((node, i) => ({
    ...base, deployment_id: i + 1, deployment: node,
    reason: { code: "pool_no_fit", params: { node, tried: 1, candidates: 1, first_reason: { message: "Model missing" } } },
  }));
  serve({ board: () => queues({ pending: [...pending, base] }) });
  const user = mount();
  const queue = await screen.findByRole("region", { name: s.pending.title });
  const cause = within(queue).getByRole("button", { name: /Model missing/ });
  assert.ok(within(cause).getByText(i18n.t("scheduling:pending.affected", { count: 2 })));
  await user.click(cause);
  const drawer = await screen.findByRole("dialog");
  assert.ok(within(drawer).getByRole("link", { name: "node-a" }));
  assert.ok(within(drawer).getByRole("link", { name: "node-b" }));
  assert.equal(within(drawer).queryByRole("link", { name: base.deployment }), null);
  await user.click(within(drawer).getByRole("button", { name: i18n.t("scheduling:queues.showAll", { count: 3 }) }));
  assert.ok(within(drawer).getByRole("link", { name: base.deployment }));
});

test("a PD role's scheduled count names the role, and a count of 0 is still a count", async () => {
  serve({
    board: () => queues({
      upcoming: [
        { deployment_id: 11, deployment: "pd-chat", schedule_id: 3, action: "replicas", replicas: 0, role: "prefill", at: "2026-10-26T07:00:00Z" },
        { deployment_id: 11, deployment: "pd-chat", schedule_id: 4, action: "replicas", replicas: 3, role: "decode", at: "2026-10-26T08:00:00Z" },
      ],
    }),
  });
  mount();
  for (const [role, count] of [["prefill", 0], ["decode", 3]] as const) {
    assert.ok(await screen.findByText(i18n.t("scheduling:upcoming.action.roleReplicas", { role: i18n.t(`deployments:detail.pd.role.${role}`), count })), role);
  }
});

test("a waiting wake is cancelled from the keyboard, then the queues and the overview reload", async () => {
  let waiting = queues().waiting_wakes;
  serve({ board: () => queues({ waiting_wakes: waiting }) });
  const cancelled: number[] = [];
  api.endWakeWaiting = async (id) => { cancelled.push(id); waiting = []; return { items: ["inst-llama-0001"], total: 1 }; };
  let reads = 0;
  const base = schedulingApi.overview;
  schedulingApi.overview = async () => { reads += 1; return base(); };
  const user = mount();
  await screen.findByRole("region", { name: s.pending.title });
  const before = reads;
  const cancel = screen.getByRole("button", { name: i18n.t("scheduling:waiting.cancelOf", { name: "llama" }) });
  cancel.focus();
  assert.equal(document.activeElement, cancel);
  await user.keyboard("{Enter}");
  await waitFor(() => assert.deepEqual(cancelled, [8]));
  await screen.findByText(s.waiting.empty);
  assert.ok(reads > before, "the overview is read again");
});

test("a refused cancel keeps the waiting wake and says why", async () => {
  serve();
  api.endWakeWaiting = async () => { throw new APIError("DEPLOYMENT_DELETING", {}, "the deployment is being deleted", 409); };
  const user = mount();
  await screen.findByRole("region", { name: s.pending.title });
  await user.click(screen.getByRole("button", { name: i18n.t("scheduling:waiting.cancelOf", { name: "llama" }) }));
  await screen.findByText(i18n.t("scheduling:waiting.cancelFailed", { detail: i18n.t("common:apiError.DEPLOYMENT_DELETING") }));
  assert.ok(screen.getByRole("button", { name: i18n.t("scheduling:waiting.cancelOf", { name: "llama" }) }));
});

test("the sharing stats cover the fleet, are read for the chosen window, and say when they fail", async () => {
  let fail = false;
  const calls = serve({ sharing: (key) => { if (fail) throw new Error("log unavailable"); return sharing(key); } });
  const user = mount();
  const title = await screen.findByText(s.sharing.title);
  const card = title.closest(".ant-card") as HTMLElement;
  await within(card).findByRole("link", { name: "llama" });
  // The server says these are the fleet's figures: no node-only hint.
  assert.equal(within(card).queryByRole("img", { name: s.timeline.scopeNode }), null);
  // The first read leaves the window to the server; its answer lists the four
  // windows, 6 hours chosen.
  assert.deepEqual(calls.timeline.at(-1), { sharing: true, window: undefined });
  const picker = within(card).getByRole("radiogroup", { name: s.timeline.window });
  assert.deepEqual(within(picker).getAllByRole("radio").map((radio) => (radio as HTMLInputElement).checked), [false, false, true, false]);
  assert.ok(within(picker).getByText(i18n.t("scheduling:timeline.minutes", { minutes: 15 })));
  await user.click(within(picker).getByText(i18n.t("scheduling:timeline.hours", { hours: 1 })));
  await waitFor(() => assert.deepEqual(calls.timeline.at(-1), { sharing: true, window: "1h" }));
  fail = true;
  await user.click(within(card).getByText(i18n.t("scheduling:timeline.hours", { hours: 24 })));
  await waitFor(() => assert.deepEqual(calls.timeline.at(-1), { sharing: true, window: "24h" }));
  assert.ok(await within(card).findByText(i18n.t("scheduling:sharing.refreshFailed", { detail: "log unavailable" })), "the last stats stay under the warning");
  assert.ok(within(card).getByRole("link", { name: "llama" }));
});

test("all nodes used by a deployment are reachable directly from its sharing row", async () => {
  const cards = Array.from({ length: 20 }, (_, i) => ({
    node_id: i + 1, node: `worker-${i + 1}`, node_uuid: `node-${i + 1}`, gpus: [0, 3],
  }));
  serve({ sharing: () => {
    const data = sharing();
    data.deployments[0].cards = cards;
    return data;
  } });
  const user = mount();
  const node = await screen.findByRole("link", { name: "worker-20" });
  const cell = node.closest("td")!;
  assert.equal(within(cell).getAllByRole("link").length, cards.length);
  assert.equal(node.parentElement?.textContent, i18n.t("scheduling:timeline.cardsOnNode", { node: "worker-20", gpus: "0, 3" }));
  await user.click(node);
  await waitFor(() => assert.equal(screen.getByTestId("loc").textContent, "/scheduling?node=node-20"));
  await screen.findByRole("dialog");
});

test("a later failed refresh keeps the last overview and warns", async () => {
  let calls = 0;
  serve({ overview: async () => { calls += 1; if (calls > 1) throw new Error("server restarting"); return smallFleet(); } });
  mount();
  await screen.findByRole("group", { name: s.heatmap.aria });
  // The poll skips hidden documents, and jsdom's is hidden.
  Object.defineProperty(document, "hidden", { value: false, configurable: true });
  document.dispatchEvent(new dom.window.Event("visibilitychange"));
  await screen.findByText(i18n.t("scheduling:page.refreshFailed", { detail: "server restarting" }));
  assert.equal(cells().length, 6);
});
