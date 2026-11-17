import assert from "node:assert/strict";
import { after, afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import { createI18n, dom } from "./helpers/dom.ts";
import { trackMessageChannels } from "./helpers/trackedMessageChannels.ts";

// The scheduling activity card: its loading / error / empty / success
// states, the chart drawn from the server's reasons and buckets, the busiest
// cards that open their node's drawer, the range picker and the flag for a
// range beyond the statistics' retention.

// echarts needs a 2d context JSDOM cannot provide.
const anyContext = new Proxy(function () {} as never, {
  get: (_target: never, prop: string | symbol) => {
    if (prop === "canvas") return { width: 300, height: 200, style: {} };
    if (prop === "dpr") return 1;
    if (prop === "measureText") return () => ({ width: 10 });
    return anyContext;
  },
  set: () => true,
  apply: () => anyContext,
});
dom.window.HTMLCanvasElement.prototype.getContext = () => anyContext as CanvasRenderingContext2D;

const closeChannels = trackMessageChannels();
const { i18n, locale } = await createI18n(["scheduling", "deployments", "analytics"]);
const { cleanup, render, screen, waitFor, within } = await import("@testing-library/react");
const userEvent = (await import("@testing-library/user-event")).default;
const { MemoryRouter, useLocation } = await import("react-router-dom");
const { schedulingApi } = await import("../src/schedulingApi.ts");
const { default: ActivityCard, TOP_ROWS_SHOWN } = await import("../src/pages/scheduling/ActivityCard.tsx");
const { activityOption, bucketLabel, isEmpty, reasonColor } = await import("../src/pages/scheduling/activityChart.ts");
const { colors } = await import("../src/theme/tokens.ts");
type Activity = import("../src/schedulingApi.ts").SchedulingActivity;

const s = locale.scheduling;
const original = { ...schedulingApi };
const REASONS = ["idle_yield", "rank_preemption", "timeslice_turn", "operator_yield", "scheduled", "placement", "failure", "queued"];

function activity(overrides: Partial<Activity> = {}): Activity {
  const buckets = [0, 1, 2].map((i) => ({ start: new Date(Date.parse("2026-10-28T10:00:00Z") + i * 900_000).toISOString(), counts: REASONS.map((_, r) => (r === 1 ? i : r === 6 ? 1 : 0)) }));
  return {
    from: "2026-10-28T10:00:00Z", to: "2026-10-28T10:45:00Z", bucket_seconds: 900,
    reasons: REASONS.map((key, r) => ({ key, count: r === 1 ? 3 : r === 6 ? 3 : 0 })),
    buckets,
    top_cards: Array.from({ length: 12 }, (_, i) => ({ node_uuid: `node-${i}`, node: i === 3 ? "" : `gpu-${i}`, gpu_index: i % 8, switches: 40 - i })),
    beyond_retention: false,
    ...overrides,
  };
}

function Location() {
  return <div data-testid="loc">{useLocation().search}</div>;
}

function mount() {
  render(
    <I18nextProvider i18n={i18n}>
      <MemoryRouter initialEntries={["/scheduling"]}><ActivityCard /><Location /></MemoryRouter>
    </I18nextProvider>,
  );
  return userEvent.setup();
}

afterEach(() => {
  cleanup();
  Object.assign(schedulingApi, original);
});
after(() => closeChannels());

test("the chart stacks one series per reason the server names, in its order and from the theme's colours", () => {
  const data = activity();
  const option = activityOption(data, (key) => `name:${key}`, "en");
  const series = option.series as { name: string; stack: string; data: number[]; itemStyle: { color: string } }[];
  assert.deepEqual(series.map((one) => one.name), REASONS.map((key) => `name:${key}`));
  assert.ok(series.every((one) => one.stack === "activity"));
  assert.deepEqual(series[1].data, [0, 1, 2]);
  assert.equal(series[5].itemStyle.color, colors.text);
  assert.equal(series[6].itemStyle.color, colors.danger);
  // Every reason the server names has a colour of its own.
  assert.equal(new Set(series.map((one) => one.itemStyle.color)).size, REASONS.length);
  assert.equal(reasonColor("something_new", 0), colors.chartSecondary);
  assert.equal((option.xAxis as { data: string[] }).data.length, 3);
  assert.equal(isEmpty(data), false);
  assert.equal(isEmpty(activity({ reasons: REASONS.map((key) => ({ key, count: 0 })) })), true);
  // Day-long buckets are labelled by date; a range over a day adds the date to the time.
  assert.match(bucketLabel("2026-10-28T00:00:00Z", 86_400, 30 * 86_400, "en-GB"), /^\d\d\/\d\d$/);
  assert.match(bucketLabel("2026-10-28T10:00:00Z", 3600, 7 * 86_400, "en-GB"), /^\d\d\/\d\d \d\d:\d\d$/);
});

test("a quick range is asked for by the server's window, a custom one by since and until", async () => {
  const urls: string[] = [];
  const originalFetch = globalThis.fetch;
  globalThis.fetch = (async (input: RequestInfo | URL) => {
    urls.push(String(input));
    return new Response(JSON.stringify(activity()), { status: 200, headers: { "Content-Type": "application/json" } });
  }) as typeof fetch;
  try {
    for (const key of ["15m", "1h", "6h", "1d", "1w"] as const) await schedulingApi.activity({ kind: "quick", key });
    await schedulingApi.activity({ kind: "custom", startMS: Date.parse("2026-10-01T00:00:00.250Z"), endMS: Date.parse("2026-10-03T12:00:00Z") });
  } finally {
    globalThis.fetch = originalFetch;
  }
  const params = urls.map((url) => new URL(url, "http://localhost").searchParams.toString());
  assert.deepEqual(params, ["window=15m", "window=1h", "window=6h", "window=24h", "window=7d", "since=2026-10-01T00%3A00%3A00Z&until=2026-10-03T12%3A00%3A00Z"]);
});

test("a loading status shows until the activity answers, for the last 24 hours", async () => {
  const asked: unknown[] = [];
  let release: (value: Activity) => void = () => {};
  schedulingApi.activity = (range) => {
    asked.push(range);
    return new Promise<Activity>((resolve) => { release = resolve; });
  };
  mount();
  assert.ok(await screen.findByText(s.activity.loading));
  assert.deepEqual(asked, [{ kind: "quick", key: "1d" }]);
  release(activity());
  assert.ok(await screen.findByRole("img", { name: i18n.t("scheduling:activity.chartAria", { bucket: i18n.t("scheduling:activity.bucket.minutes", { value: 15 }) }) }));
});

test("a failed first load shows the error and retries on request", async () => {
  let calls = 0;
  schedulingApi.activity = async () => {
    calls += 1;
    if (calls === 1) throw new Error("stats.db locked");
    return activity();
  };
  const user = mount();
  assert.ok(await screen.findByText(s.activity.loadFailed));
  assert.ok(screen.getByText("stats.db locked"));
  await user.click(screen.getByRole("button", { name: s.page.retry }));
  assert.ok(await screen.findByRole("list", { name: s.activity.topCards }));
});

test("a range with no activity says so, and so does a list with no card", async () => {
  schedulingApi.activity = async () => activity({ reasons: REASONS.map((key) => ({ key, count: 0 })), top_cards: [] });
  mount();
  assert.ok(await screen.findByText(s.activity.empty));
  assert.ok(screen.getByText(s.activity.topCardsEmpty));
  assert.equal(screen.queryByRole("img"), null);
});

test("the busiest cards fill the chart's height, most first, each opening its node's drawer", async () => {
  schedulingApi.activity = async () => activity();
  const user = mount();
  const list = within(await screen.findByRole("list", { name: s.activity.topCards }));
  const rows = list.getAllByRole("listitem");
  assert.equal(rows.length, TOP_ROWS_SHOWN);
  assert.ok(TOP_ROWS_SHOWN < 12, "more cards than fit are left out");
  assert.ok(within(rows[0]).getByText(i18n.t("scheduling:activity.switches", { count: 40 })));
  assert.ok(within(rows[0]).getByText(new RegExp(i18n.t("scheduling:activity.gpu", { index: 0 }))));
  // A node no longer known is named by its UUID.
  assert.ok(within(rows[3]).getByRole("link", { name: "node-3" }));
  await user.click(within(rows[1]).getByRole("link", { name: "gpu-1" }));
  await waitFor(() => assert.equal(screen.getByTestId("loc").textContent, "?node=node-1"));
  // From the keyboard alone as well.
  within(rows[2]).getByRole("link", { name: "gpu-2" }).focus();
  await user.keyboard("{Enter}");
  await waitFor(() => assert.equal(screen.getByTestId("loc").textContent, "?node=node-2"));
  // The range's totals name each reason that happened.
  assert.ok(screen.getByText(`${s.activity.reason.rank_preemption} 3`));
  assert.ok(screen.getByText(`${s.activity.reason.failure} 3`));
  assert.equal(screen.queryByText(new RegExp(`^${s.activity.reason.queued}`)), null);
});

test("picking a week asks for it; a range past the retention is flagged", async () => {
  const asked: unknown[] = [];
  schedulingApi.activity = async (range) => {
    asked.push(range);
    return activity({ beyond_retention: asked.length > 1, bucket_seconds: asked.length > 1 ? 3600 : 900 });
  };
  const user = mount();
  await screen.findByRole("list", { name: s.activity.topCards });
  assert.equal(screen.queryByText(s.activity.beyondRetention), null);
  const picker = screen.getByRole("combobox", { name: i18n.t("deployments:timeRange.ariaLabel") });
  await user.click(picker);
  await user.click(await screen.findByTitle(i18n.t("deployments:timeRange.1w")));
  await waitFor(() => assert.deepEqual(asked.at(-1), { kind: "quick", key: "1w" }));
  assert.ok(await screen.findByText(s.activity.beyondRetention));
});

test("a failed refresh keeps the last chart and warns", async () => {
  let calls = 0;
  schedulingApi.activity = async () => {
    calls += 1;
    if (calls > 1) throw new Error("boom");
    return activity();
  };
  const user = mount();
  await screen.findByRole("list", { name: s.activity.topCards });
  await user.click(screen.getByRole("combobox", { name: i18n.t("deployments:timeRange.ariaLabel") }));
  await user.click(await screen.findByTitle(i18n.t("deployments:timeRange.6h")));
  assert.ok(await screen.findByText(i18n.t("scheduling:activity.refreshFailed", { detail: "boom" })));
  assert.ok(screen.getByRole("list", { name: s.activity.topCards }));
});
