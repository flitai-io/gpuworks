import assert from "node:assert/strict";
import fs from "node:fs";
import { after, afterEach, test } from "node:test";
import { JSDOM } from "jsdom";
import React from "react";
import { createInstance } from "i18next";
import { I18nextProvider, initReactI18next } from "react-i18next";
import { trackMessageChannels } from "./helpers/trackedMessageChannels.ts";

const dom = new JSDOM("<!doctype html><html><body></body></html>", { url: "http://localhost/monitor/d/7" });
for (const name of ["window", "document", "navigator", "HTMLElement", "SVGElement", "Element", "Node", "MutationObserver", "ShadowRoot"] as const) {
  Object.defineProperty(globalThis, name, { value: dom.window[name], configurable: true });
}
Object.defineProperty(globalThis, "getComputedStyle", { value: (element: Element) => dom.window.getComputedStyle(element), configurable: true });
Object.defineProperty(dom.window, "matchMedia", { configurable: true, value: () => ({ matches: false, addListener() {}, removeListener() {}, addEventListener() {}, removeEventListener() {}, dispatchEvent: () => false }) });
// The result filter's tag overflow schedules through MessageChannel.
const closeChannels = trackMessageChannels();
class ResizeObserverStub { observe() {} unobserve() {} disconnect() {} }
Object.defineProperty(globalThis, "ResizeObserver", { value: ResizeObserverStub, configurable: true });

const i18n = createInstance();
await i18n.use(initReactI18next).init({
  lng: "en",
  fallbackLng: "en",
  resources: {
    en: {
      monitoring: JSON.parse(fs.readFileSync(new URL("../src/i18n/locales/en/monitoring.json", import.meta.url), "utf8")),
      common: JSON.parse(fs.readFileSync(new URL("../src/i18n/locales/en/common.json", import.meta.url), "utf8")),
    },
  },
  defaultNS: "monitoring",
  interpolation: { escapeValue: false },
});

const { cleanup, render, screen, waitFor, within, fireEvent } = await import("@testing-library/react");
const { MemoryRouter } = await import("react-router-dom");
const userEvent = (await import("@testing-library/user-event")).default;
const { api } = await import("../src/api.ts");
const { RequestDetailLines } = await import("../src/pages/monitor/RequestRow.tsx");
const { default: RequestView } = await import("../src/pages/monitor/RequestView.tsx");
const { default: DistributionHeatmap } = await import("../src/pages/monitor/DistributionHeatmap.tsx");
import type { MonitorRequest, MonitorRequests, MonitorRequestsQuery } from "../src/monitorRequestsApi";

const original = api.monitorRequests;
const originalTelemetry = api.telemetryExport;
const { resetTraceLinkCache } = await import("../src/pages/monitor/useTraceLink.ts");
afterEach(() => {
  cleanup();
  api.monitorRequests = original;
  api.telemetryExport = originalTelemetry;
  resetTraceLinkCache();
});
after(() => {
  closeChannels();
  dom.window.close();
});

function request(over: Partial<MonitorRequest>): MonitorRequest {
  return {
    id: "r", trace_id: "4bf92f3577b34da6a3ce929d0e0e4736", deployment_id: 7, received_ms: Date.UTC(2026, 9, 12, 8, 30, 5), phase: "done", age_ms: null, instance_uuid: "a1b2c3d4e5f6", api_key_id: 3,
    api_key_name: "chat-app", user_id: 1, source: "api", stream: true, body_bytes: 2048, images: 0, dispatch_ms: 2,
    first_byte_ms: 20, first_think_ms: null, first_answer_ms: 502, last_chunk_ms: 1502, end_ms: 1502, events: 101,
    think_events: 0, max_gap_ms: 40, status: 200, outcome: "ok", finish_reason: "stop", prompt_tokens: 3000,
    cached_tokens: 1000, completion_tokens: 201, reasoning_tokens: null, engine_queue_ms: 100, engine_prefill_ms: 350,
    engine_decode_ms: 1000, weight: 1, result: "completed",
    segments: [{ kind: "gateway", ms: 2 }, { kind: "preprocess", ms: 50 }, { kind: "queue", ms: 100 }, { kind: "prefill", ms: 350 }, { kind: "answering", ms: 1000 }],
    pd: null,
    ...over,
  };
}

// A live row carries no usage or engine stats yet.
function live(requests: MonitorRequest[], counts = { waiting: 1, thinking: 1, answering: 1 }): MonitorRequests {
  const inFlight = requests.map((r) => ({ ...r, engine_queue_ms: null, engine_prefill_ms: null, engine_decode_ms: null }));
  return { view: "live", as_of: 1, counts, requests: inFlight, next_cursor: "" };
}

const replicas = [
  { instance_uuid: "a1b2c3d4e5f6", node_name: "gpu-a" },
  { instance_uuid: "f6e5d4c3b2a1", node_name: "gpu-b" },
] as never;

function renderView(view: "live" | "history" = "live", pd = false) {
  return render(
    <MemoryRouter initialEntries={["/monitor/d/7"]}>
      <I18nextProvider i18n={i18n}>
        <RequestView view={view} deploymentId={7} range="6h" replicas={replicas} pd={pd} />
      </I18nextProvider>
    </MemoryRouter>,
  );
}

function historyPage(requests: MonitorRequest[], next: string, total = 1234): MonitorRequests {
  return { view: "history", as_of: 1, counts: null, requests, next_cursor: next, from_ms: 1_000_000, to_ms: 2_000_000, range_total: total };
}

test("live: shows phase counts and one row per in-flight request, polling with the default limit", async () => {
  const calls: MonitorRequestsQuery[] = [];
  api.monitorRequests = async (id, q) => {
    assert.equal(id, 7);
    calls.push(q);
    return live([
      request({ id: "w", phase: "waiting", age_ms: 3000, first_answer_ms: null, last_chunk_ms: null, end_ms: null, status: null, outcome: "", result: "", prompt_tokens: null, completion_tokens: null, cached_tokens: null,
        segments: [{ kind: "gateway", ms: 2 }, { kind: "waiting", ms: 2998 }] }),
      request({ id: "t", phase: "thinking", age_ms: 5000, api_key_name: "", source: "playground", api_key_id: 0, first_think_ms: 800, first_answer_ms: null, status: null, outcome: "", result: "", prompt_tokens: null, completion_tokens: null, cached_tokens: null,
        segments: [{ kind: "gateway", ms: 2 }, { kind: "waiting", ms: 798 }, { kind: "thinking", ms: 4200 }] }),
    ], { waiting: 4, thinking: 1, answering: 0 });
  };
  renderView();
  const list = await screen.findByRole("list", { name: "In flight" });
  const rows = within(list).getAllByRole("listitem");
  assert.equal(rows.length, 2);
  assert.ok(within(rows[0]).getByText("chat-app"));
  assert.ok(within(rows[0]).getByText("body 2KB"));
  // The phase, and the replica the request went to.
  assert.ok(within(rows[0]).getByText("Waiting for first token · Replica 1 · gpu-a"));
  assert.ok(within(rows[0]).getByText("3 s"));
  assert.ok(within(rows[1]).getByText("Playground"));
  // The bar draws the wait, then the thinking so far.
  assert.deepEqual(Array.from(rows[1].querySelectorAll("[data-kind]")).map((el) => el.getAttribute("data-kind")), ["gateway", "waiting", "thinking"]);
  const counts = screen.getByLabelText("Requests in flight by phase");
  assert.match(counts.textContent ?? "", /Waiting for first token4/);
  assert.deepEqual(calls[0], { view: "live", limit: 20, key: 0, instance: "" });
  // More in flight than listed offers "show all".
  await userEvent.setup().click(screen.getByRole("button", { name: "Show all (5)" }));
  await waitFor(() => assert.equal(calls[calls.length - 1].limit, 200));
});

test("live: empty, error with retry, and pausing stops the polling", async () => {
  let fail = true;
  let calls = 0;
  api.monitorRequests = async () => {
    calls += 1;
    if (fail) throw new Error("gateway gone");
    return live([], { waiting: 0, thinking: 0, answering: 0 });
  };
  renderView();
  await screen.findByText("gateway gone");
  fail = false;
  await userEvent.setup().click(screen.getByRole("button", { name: /Retry/ }));
  await screen.findByText("No requests in flight");
  const toggle = screen.getByRole("switch", { name: "Refresh every 2 seconds" });
  await userEvent.setup().click(toggle);
  assert.ok(screen.getByText("Paused"));
  const before = calls;
  await new Promise((r) => setTimeout(r, 2300));
  assert.equal(calls, before, "no polling while paused");
});

test("history: rows split the wait with the engine's stats and say which replica served them", async () => {
  const calls: MonitorRequestsQuery[] = [];
  api.monitorRequests = async (_id, q) => {
    calls.push(q);
    return historyPage([request({ id: "h1" }), request({ id: "h0", api_key_id: 9, api_key_name: "batch" })], "");
  };
  renderView("history");
  const list = await screen.findByRole("list", { name: "History" });
  const row = within(list).getAllByRole("listitem")[0];
  assert.ok(within(row).getByText("in 2.9K (cached 1000)"));
  assert.ok(within(row).getByText("out 201"));
  assert.ok(within(row).getByText(/Replica 1 · gpu-a/));
  assert.deepEqual(Array.from(row.querySelectorAll("[data-kind]")).map((el) => el.getAttribute("data-kind")), ["gateway", "preprocess", "queue", "prefill", "answering"]);
  assert.deepEqual(calls[calls.length - 1], { view: "history", range: "6h", limit: 50, key: 0, instance: "", results: [], minMs: undefined, maxMs: undefined, cursor: "" });

  // Focus (keyboard) shows the detail: times and speeds.
  row.focus();
  await waitFor(() => assert.ok(screen.getByText("Prefill 350 ms · 5714 tok/s")));
  assert.ok(screen.getByText("Queue 100 ms"));
  // The gateway's own share and what the engine left out of its account are
  // two lines.
  assert.ok(screen.getByText("Gateway 2 ms"));
  assert.ok(screen.getByText(/^Engine preprocessing 50 ms/));
  assert.ok(screen.getByText("Decode 201 tok/s"));
  assert.ok(screen.getByText("Input 3000 tokens · cached 1000 · uncached 2000"));

  // The key filter offers the keys seen so far, by name; the replica filter
  // the deployment's replicas.
  fireEvent.mouseDown(screen.getByRole("combobox", { name: "Filter by API key" }));
  await waitFor(() => assert.ok(screen.getAllByText("batch").some((el) => el.closest(".ant-select-item-option"))));
  await userEvent.setup().click(screen.getAllByText("batch").find((el) => el.closest(".ant-select-item-option"))!);
  await waitFor(() => assert.equal(calls[calls.length - 1].key, 9));
  fireEvent.mouseDown(screen.getByRole("combobox", { name: "Filter by replica" }));
  await userEvent.setup().click(await screen.findByText("Replica 2 · gpu-b"));
  await waitFor(() => assert.equal(calls[calls.length - 1].instance, "f6e5d4c3b2a1"));
});

test("history: pages by cursor within the range the first page pinned, and back", async () => {
  const calls: MonitorRequestsQuery[] = [];
  api.monitorRequests = async (_id, q) => {
    calls.push(q);
    if (q.view === "live") return live([]);
    if (q.cursor === "c1") return historyPage([request({ id: "h2", outcome: "client_closed", result: "client_closed" })], "c2");
    if (q.cursor === "c2") return historyPage([request({ id: "h3" })], "");
    return historyPage([request({ id: "h1" })], "c1");
  };
  renderView("history");
  const user = userEvent.setup();
  await screen.findByText("Page 1");
  assert.ok(screen.getByText(/1234 requests in this range/));
  assert.equal((screen.getByRole("button", { name: "Previous" }) as HTMLButtonElement).disabled, true);

  await user.click(screen.getByRole("button", { name: "Next" }));
  await screen.findByText("Page 2");
  let last = calls[calls.length - 1];
  assert.equal(last.cursor, "c1");
  assert.equal(last.from, 1_000_000);
  assert.equal(last.to, 2_000_000);
  assert.ok(screen.getByText("Client left"));

  await user.click(screen.getByRole("button", { name: "Next" }));
  await screen.findByText("Page 3");
  assert.equal(calls[calls.length - 1].cursor, "c2");
  assert.equal((screen.getByRole("button", { name: "Next" }) as HTMLButtonElement).disabled, true);

  await user.click(screen.getByRole("button", { name: "Previous" }));
  await screen.findByText("Page 2");
  last = calls[calls.length - 1];
  assert.equal(last.cursor, "c1");
  assert.equal(last.from, 1_000_000);
});

test("history: filters by result and end-to-end time start over at the first page", async () => {
  const calls: MonitorRequestsQuery[] = [];
  api.monitorRequests = async (_id, q) => {
    calls.push(q);
    if (q.view === "live") return live([]);
    if (q.results?.length || q.minMs) return historyPage([], "");
    return historyPage([request({ id: "h1" })], "c1");
  };
  renderView("history");
  const user = userEvent.setup();
  await screen.findByText("Page 1");
  await user.click(screen.getByRole("button", { name: "Next" }));

  fireEvent.mouseDown(screen.getByRole("combobox", { name: "Filter by result" }));
  await user.click(await screen.findByText("Hit the length limit"));
  await waitFor(() => assert.deepEqual(calls[calls.length - 1].results, ["length"]));
  assert.equal(calls[calls.length - 1].cursor, "");
  await screen.findByText("No request in this time range matches the filters");

  await user.type(screen.getByRole("spinbutton", { name: "End-to-end ≥" }), "2");
  await waitFor(() => assert.equal(calls[calls.length - 1].minMs, 2000));
});

test("history: empty and error states", async () => {
  let fail = true;
  api.monitorRequests = async (_id, q) => {
    if (q.view === "live") return live([]);
    if (fail) throw new Error("ux.db locked");
    return { view: "history", as_of: 1, counts: null, requests: [], next_cursor: "" };
  };
  renderView("history");
  await screen.findByText("ux.db locked");
  fail = false;
  await userEvent.setup().click(screen.getByRole("button", { name: /Retry/ }));
  await screen.findByText("No requests finished in this range");
});

test("the heatmap names a cell on hover and moves with the arrow keys", async () => {
  render(
    <DistributionHeatmap
      cells={[{ x: 1, y: 1, n: 3 }, { x: 2, y: 1, n: 1 }]}
      height={200}
      label="grid"
      xTitle="x"
      yTitle="y"
      xTick={(i) => `x${i}`}
      yTick={(i) => `y${i}`}
      xRange={(i) => `X${i}`}
      yRange={(i) => `Y${i}`}
      readout={(x, y, n, share) => `${x}/${y}: ${n} (${share})`}
      hint="hover"
      empty="nothing"
    />,
  );
  const grid = screen.getByRole("grid", { name: "grid" });
  assert.equal(within(grid).getAllByRole("gridcell").length, 2);
  assert.ok(screen.getByText("hover"));
  await userEvent.setup().hover(within(grid).getByRole("gridcell", { name: "X2/Y1: 1 (25%)" }));
  assert.ok(screen.getByText("X2/Y1: 1 (25%)", { selector: "span" }));
  grid.focus();
  fireEvent.keyDown(grid, { key: "ArrowLeft" });
  fireEvent.keyDown(grid, { key: "ArrowLeft" });
  assert.ok(screen.getByText("X1/Y1: 3 (75%)", { selector: "span" }));
  fireEvent.keyDown(grid, { key: "ArrowRight" });
  assert.ok(screen.getByText("X2/Y1: 1 (25%)", { selector: "span" }));
});

test("the heatmap says so when there is nothing to draw", () => {
  render(
    <DistributionHeatmap cells={[]} height={200} label="grid" xTitle="x" yTitle="y" xTick={String} yTick={String} xRange={String} yRange={String} readout={() => ""} hint="" empty="nothing yet" />,
  );
  assert.ok(screen.getByText("nothing yet"));
  assert.equal(screen.queryByRole("grid"), null);
});

test("history: a reply that does not stream says so, and the end note sits under the duration", async () => {
  api.monitorRequests = async (_id, q) => {
    if (q.view === "live") return live([]);
    return {
      view: "history", as_of: 1, counts: null, next_cursor: "",
      requests: [request({ id: "n", stream: false, first_answer_ms: null, last_chunk_ms: null, end_ms: 900, finish_reason: "length", result: "length", events: 0,
        segments: [{ kind: "gateway", ms: 2 }, { kind: "queue", ms: 62 }, { kind: "prefill", ms: 216.6 }, { kind: "answering", ms: 619.4 }] })],
    };
  };
  renderView("history");
  const row = within(await screen.findByRole("list", { name: "History" })).getAllByRole("listitem")[0];
  assert.ok(within(row).getByText("out 201 · not streamed"));
  assert.deepEqual(Array.from(row.querySelectorAll("[data-kind]")).map((el) => el.getAttribute("data-kind")), ["gateway", "queue", "prefill", "answering"]);
  const total = row.querySelector(".monitor-request-total")!;
  assert.deepEqual(Array.from(total.children).map((el) => el.textContent), ["900 ms", "Hit the length limit"]);
  assert.ok(screen.getByLabelText("Request phase legend").textContent?.includes("Engine postprocessing"));
});

function telemetryWith(traceLink: string) {
  api.telemetryExport = async () => ({
    enabled: true, endpoint: "http://c:4318", headers: [], sample_ratio: 1, trace_link: traceLink,
    status: { active: true, traces_url: "", exported: 0, dropped: 0, failed: 0, queued: 0, last_error: "", last_error_at: null, last_export_at: null, setting_error: "" },
  });
}

test("history: a row opens the request's details with its ids and a link to the trace", async () => {
  telemetryWith("https://grafana.example/explore?trace={trace_id}");
  api.monitorRequests = async () => historyPage([request({ id: "gw/req-9" })], "");
  renderView("history");
  const list = await screen.findByRole("list", { name: "History" });
  const user = userEvent.setup();
  await user.click(within(list).getAllByRole("listitem")[0]);
  const drawer = await screen.findByRole("dialog");
  assert.ok(within(drawer).getByText("Request details"));
  assert.ok(within(drawer).getByText("gw/req-9"));
  assert.ok(within(drawer).getByText("4bf92f3577b34da6a3ce929d0e0e4736"));
  const link = await within(drawer).findByRole("link", { name: /Open in the tracing backend/ });
  assert.equal(link.getAttribute("href"), "https://grafana.example/explore?trace=4bf92f3577b34da6a3ce929d0e0e4736");
  assert.equal(link.getAttribute("target"), "_blank");
  // A service page's own list does not name the deployment again.
  assert.equal(within(drawer).queryByText("Deployment"), null);
  assert.ok(within(drawer).getByText("Replica 1 · gpu-a"));
});

test("history: Enter opens the details; without a trace link or a trace id there is no link", async () => {
  telemetryWith("");
  api.monitorRequests = async () => historyPage([request({ id: "old", trace_id: "" })], "");
  renderView("history");
  const list = await screen.findByRole("list", { name: "History" });
  within(list).getAllByRole("listitem")[0].focus();
  await userEvent.setup().keyboard("{Enter}");
  const drawer = await screen.findByRole("dialog");
  assert.ok(within(drawer).getByText("None (recorded before trace ids)"));
  assert.equal(within(drawer).queryByRole("link", { name: /Open in the tracing backend/ }), null);
});

// A PD request: its row draws the prefill stage, the legend names the decode
// instance's queue and prefill as PD has them, and its details have a PD
// section: the prefill instance, the stage, that instance's own account and
// the decode instance's wait for the KV.
test("history: a PD request shows its prefill stage in the row, the legend and the details", async () => {
  telemetryWith("");
  api.monitorRequests = async () =>
    historyPage(
      [
        request({
          id: "pd-1", dispatch_ms: 816, engine_queue_ms: 700, engine_prefill_ms: 29,
          segments: [{ kind: "gateway", ms: 21 }, { kind: "pd_prefill", ms: 795 }, { kind: "preprocess", ms: 35 }, { kind: "queue", ms: 700 }, { kind: "prefill", ms: 29 }, { kind: "answering", ms: 1000 }],
          pd: { prefill_instance_uuid: "3107437e7517aaaa", prefill_ms: 795, prefill_queue_ms: 2, prefill_compute_ms: 640, fallback: "" },
        }),
      ],
      "",
    );
  renderView("history", true);
  const list = await screen.findByRole("list", { name: "History" });
  const row = within(list).getAllByRole("listitem")[0];
  assert.deepEqual(Array.from(row.querySelectorAll("[data-kind]")).map((el) => el.getAttribute("data-kind")), ["gateway", "pd_prefill", "preprocess", "queue", "prefill", "answering"]);
  const legend = screen.getByLabelText("Request phase legend");
  assert.match(legend.textContent ?? "", /PD prefill/);
  assert.match(legend.textContent ?? "", /KV transfer & queue/);
  assert.match(legend.textContent ?? "", /First-token recompute/);
  await userEvent.setup().click(row);
  const drawer = await screen.findByRole("dialog");
  assert.ok(within(drawer).getByText("PD disaggregation"));
  assert.ok(within(drawer).getByText("3107437e"));
  assert.ok(within(drawer).getAllByText("795 ms").length >= 1);
  assert.ok(within(drawer).getByText("640 ms"));
  assert.ok(within(drawer).getByText("Decode replica"));
  assert.match(drawer.textContent ?? "", /PD prefill stage 795 ms/);
  // The prefill speed is the prefill instance's: its uncached prompt over
  // its own prefill.
  assert.match(drawer.textContent ?? "", /of which the prefill instance queued 2 ms · computed 640 ms · 3125 tok\/s/);
  assert.match(drawer.textContent ?? "", /KV transfer & queue 700 ms/);
});

// A request decoded alone says why; an ordinary deployment's legend has no
// PD stage.
test("history: a PD fallback names its reason, and an ordinary legend has no PD stage", async () => {
  telemetryWith("");
  api.monitorRequests = async () =>
    historyPage([request({ id: "fb", pd: { prefill_instance_uuid: "", prefill_ms: 3, prefill_queue_ms: null, prefill_compute_ms: null, fallback: "prefill_5xx" } })], "");
  renderView("history");
  const list = await screen.findByRole("list", { name: "History" });
  assert.doesNotMatch(screen.getByLabelText("Request phase legend").textContent ?? "", /PD prefill/);
  await userEvent.setup().click(within(list).getAllByRole("listitem")[0]);
  const drawer = await screen.findByRole("dialog");
  assert.ok(within(drawer).getByText("Prefill instance error (5xx)"));
});


for (const queue of [null, 0, 20300]) {
  test(`request details distinguish unreported queue from measured ${queue}`, () => {
    const wait = (queue ?? 150) + 350;
    const row = request({
      engine_queue_ms: queue, engine_prefill_ms: 350,
      first_answer_ms: 2 + wait, last_chunk_ms: 1002 + wait, end_ms: 1002 + wait,
      segments: [{ kind: "gateway", ms: 2 }, { kind: queue == null ? "waiting" : "queue", ms: queue ?? 150 }, { kind: "prefill", ms: 350 }, { kind: "answering", ms: 1000 }],
    });
    render(<I18nextProvider i18n={i18n}><RequestDetailLines r={row} replica="" /></I18nextProvider>);
    if (queue == null) {
      assert.ok(screen.getByText(/Engine queue time unavailable/));
      assert.equal(screen.queryByText("Queue 0 ms"), null);
    } else {
      assert.ok(screen.getByText(queue === 0 ? "Queue 0 ms" : "Queue 20 s"));
      assert.equal(screen.queryByText(/Engine queue time unavailable/), null);
    }
  });
}


test("request details do not invent zero prefill for a queue-only report", () => {
  const row = request({
    engine_queue_ms: 20300, engine_prefill_ms: null,
    first_answer_ms: 20602, last_chunk_ms: 21602, end_ms: 21602,
    segments: [{ kind: "gateway", ms: 2 }, { kind: "queue", ms: 20300 }, { kind: "waiting", ms: 300 }, { kind: "answering", ms: 1000 }],
  });
  render(<I18nextProvider i18n={i18n}><RequestDetailLines r={row} replica="" /></I18nextProvider>);
  assert.ok(screen.getByText("Queue 20 s"));
  assert.equal(screen.queryByText("Prefill 0 ms"), null);
});

test("PD prefill, decode prefill and queue remain visually distinguishable", async () => {
  const { SEGMENT_STYLE } = await import("../src/pages/monitor/RequestRow.tsx");
  assert.equal(new Set([SEGMENT_STYLE.pd_prefill, SEGMENT_STYLE.prefill, SEGMENT_STYLE.queue]).size, 3);
  assert.notEqual(SEGMENT_STYLE.thinking, SEGMENT_STYLE.answering);
});
