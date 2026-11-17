import assert from "node:assert/strict";
import fs from "node:fs";
import { after, afterEach, test } from "node:test";
import { JSDOM } from "jsdom";
import React from "react";
import { createInstance } from "i18next";
import { I18nextProvider, initReactI18next } from "react-i18next";
import { trackMessageChannels } from "./helpers/trackedMessageChannels.ts";

const dom = new JSDOM("<!doctype html><html><body></body></html>", { url: "http://localhost/monitor/d/102" });
for (const name of ["window", "document", "navigator", "HTMLElement", "SVGElement", "Element", "Node", "MutationObserver", "ShadowRoot"] as const) {
  Object.defineProperty(globalThis, name, { value: dom.window[name], configurable: true });
}
Object.defineProperty(globalThis, "getComputedStyle", { value: (element: Element) => dom.window.getComputedStyle(element), configurable: true });
Object.defineProperty(dom.window, "matchMedia", { configurable: true, value: () => ({ matches: false, addListener() {}, removeListener() {}, addEventListener() {}, removeEventListener() {}, dispatchEvent: () => false }) });
// The history tab's result filter schedules its tag overflow through
// MessageChannel.
const closeChannels = trackMessageChannels();
class ResizeObserverStub { observe() {} unobserve() {} disconnect() {} }
Object.defineProperty(globalThis, "ResizeObserver", { value: ResizeObserverStub, configurable: true });

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

class Events {
  static listeners = new Map<string, Set<EventListener>>();
  addEventListener(type: string, listener: EventListener) {
    const listeners = Events.listeners.get(type) ?? new Set<EventListener>();
    listeners.add(listener);
    Events.listeners.set(type, listeners);
  }
  removeEventListener(type: string, listener: EventListener) { Events.listeners.get(type)?.delete(listener); }
  close() {}
}
Object.defineProperty(globalThis, "EventSource", { value: Events, configurable: true });

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

const { MemoryRouter, Route, Routes, Link } = await import("react-router-dom");
const { cleanup, render, screen, waitFor, act } = await import("@testing-library/react");
const userEvent = (await import("@testing-library/user-event")).default;
const { api } = await import("../src/api.ts");
const { APIError } = await import("../src/apiError.ts");
const { default: ServiceMonitorPage } = await import("../src/pages/monitor/ServiceMonitorPage.tsx");
const { serviceSeriesName } = await import("../src/pages/monitor/monitorSeries.ts");
const { buildTimeSeriesOption } = await import("../src/components/TimeSeriesChart.tsx");
import type { MonitorDeployment } from "../src/api";
import { cursorPage } from "./helpers/listPage.ts";

const deployment = JSON.parse(fs.readFileSync(new URL("./fixtures/monitor-deployment.json", import.meta.url), "utf8")) as MonitorDeployment;
const originalMonitorDeployment = api.monitorDeployment;

// The fixture's log_instances is always non-empty, so ServiceLogStream (the
// page's merged log stream section) always renders and always calls these -
// stub them to a harmless empty result by default so the other tests in
// this file (which are not testing the log stream) stay deterministic and
// never hit a real fetch(); a test that cares about the log stream's own
// content overrides one or both before rendering.
const SAFE_LOGS_SEARCH: typeof api.logsSearch = async () => ({ items: [] });
const SAFE_INSTANCE_EVENTS: typeof api.instanceEvents = async () => cursorPage([]);
api.logsSearch = SAFE_LOGS_SEARCH;
api.instanceEvents = SAFE_INSTANCE_EVENTS;
// The request view and the distribution cards poll on their own; by default
// they answer with nothing, and the tests about them override these.
const SAFE_REQUESTS: typeof api.monitorRequests = async (_id, q) => ({ view: q.view, as_of: 0, counts: q.view === "live" ? { waiting: 0, thinking: 0, answering: 0 } : null, requests: [], next_cursor: "" });
const SAFE_LATENCY: typeof api.monitorLatency = async () => ({
  from_ms: 0, to_ms: 0, requests: 0, end_to_end: null, segments: [],
  speeds: { prefill_tok_s: null, thinking_tok_s: null, answering_tok_s: null, decode_tok_s: null },
});
const SAFE_DISTRIBUTIONS: typeof api.monitorDistributions = async () => ({
  from_ms: 0, to_ms: 0, token_buckets: [0, 2, 4], ttft_bounds_ms: [100], input_output: [], uncached_ttft: [],
  coverage: { total: 0, with_usage: 0, with_cache: 0 },
});
api.monitorRequests = SAFE_REQUESTS;
api.monitorDistributions = SAFE_DISTRIBUTIONS;
api.monitorLatency = SAFE_LATENCY;

afterEach(() => {
  cleanup();
  api.monitorDeployment = originalMonitorDeployment;
  api.logsSearch = SAFE_LOGS_SEARCH;
  api.instanceEvents = SAFE_INSTANCE_EVENTS;
  api.monitorRequests = SAFE_REQUESTS;
  api.monitorDistributions = SAFE_DISTRIBUTIONS;
  api.monitorLatency = SAFE_LATENCY;
});

after(() => {
  closeChannels();
  dom.window.close();
});

function renderPage(entry = "/monitor/d/102") {
  return render(
    <I18nextProvider i18n={i18n}>
      <MemoryRouter initialEntries={[entry]}>
        <Link to="/monitor/d/103">Next deployment</Link>
        <Routes>
          <Route path="/monitor/d/:id" element={<ServiceMonitorPage />} />
          <Route path="/monitor" element={<h1>Monitor list destination</h1>} />
        </Routes>
      </MemoryRouter>
    </I18nextProvider>,
  );
}

test("renders the breadcrumb, subtitle, latency breakdown, health check, in-flight row and replicas on success", async () => {
  api.monitorDeployment = async () => deployment;
  renderPage();
  await waitFor(() => assert.ok(screen.getByRole("heading", { name: "Qwen2.5-7B-Instruct" })));
  assert.ok(screen.getByText(/vllm · TP2 · gpu1, gpu2/));
  assert.ok(screen.getByRole("link", { name: "Go to deployment" }));
  assert.ok(screen.getByText("End-to-end latency breakdown"));
  assert.ok(screen.getByText("Health"));
  // The health check: the status, the recent checks as a strip (empty slots
  // before the first ones; each slot's details are its tooltip), then the
  // replicas. The mean, the latest check and the interval are not repeated
  // as lines on the card.
  assert.ok(screen.getByText("Healthy · 180 ms"));
  const strip = screen.getByRole("list", { name: "Recent checks" });
  assert.equal(strip.children.length, 10);
  assert.deepEqual(Array.from(strip.querySelectorAll<HTMLElement>("[role=listitem]")).map((el) => el.dataset.status), ["ok", "timeout", "ok"]);
  assert.equal(screen.queryByText(/answered in time/) === null, true);
  assert.equal(screen.queryByText(/Checked every minute/) === null, true);
  assert.equal(screen.queryByText(/^Last \d/) === null, true);
  assert.ok(screen.getByText("2 of 2 replicas running"));
  assert.ok(screen.getByText("Requests in flight"));
  assert.ok(screen.getByText("Each request in flight"));
  // The monitor passes no verdict of its own: no diagnosis card.
  assert.equal(screen.queryByText("Also observed") === null, true);
  assert.ok(screen.getByText("1111aaaa2222"));
  assert.ok(screen.getByText("5555eeee6666"));
});

test("the merged log stream renders a filter chip for every replica", async () => {
  api.monitorDeployment = async () => deployment;
  api.logsSearch = async () => ({
    items: [
      { id: 1, instance: "1111aaaa2222bbbb3333cccc4444dddd", node: "gpu1", level: "info", source: "instance", time_ms: 1758182650000, line: "replica 1 log line" },
      { id: 2, instance: "5555eeee6666ffff7777000088889999", node: "gpu2", level: "info", source: "instance", time_ms: 1758182650000, line: "replica 2 log line" },
    ],
  });
  renderPage();
  // Two chained async hops (monitorDeployment, then ServiceLogStream's own
  // logsSearch) instead of the usual one - give it more room than the
  // default 1000ms under slower/instrumented runs (e.g. coverage).
  await waitFor(() => assert.ok(screen.getByText("replica 1 log line")));
  assert.ok(screen.getByText("1111aaaa"), "chip for the first replica");
  assert.ok(screen.getByText("5555eeee"), "chip for the second replica");
});

test("a deployment without the health check says so and links to where it is turned on", async () => {
  api.monitorDeployment = async () => ({ ...deployment, health: { status: "off", threshold_sec: 60, last: null, recent_mean_ms: null, recent_ok: 0, recent: [] } });
  renderPage();
  await waitFor(() => assert.ok(screen.getByText("Off")));
  const link = screen.getByRole("link", { name: "Turn it on for the deployment" });
  assert.equal(link.getAttribute("href"), `/deployments?deployment=${deployment.deployment.id}`);
  assert.equal(screen.queryByRole("list", { name: "Recent checks" }), null);
  assert.ok(screen.getByText("2 of 2 replicas running"));
});

test("as_of <= 0 shows a 'no samples yet' caption instead of the 1970 epoch", async () => {
  api.monitorDeployment = async () => ({ ...deployment, as_of: 0 });
  renderPage();
  await waitFor(() => assert.ok(screen.getByText("No samples yet")));
  assert.equal(screen.queryByText(/1970/), null);
});

test("shows the stale sentence next to the caption when the server marks data stale", async () => {
  api.monitorDeployment = async () => ({ ...deployment, stale: true });
  renderPage();
  await waitFor(() => assert.ok(screen.getByText(/Stale — no fresh samples/)));
});

test("omits the alignment card when alignment is null", async () => {
  api.monitorDeployment = async () => ({ ...deployment, alignment: null });
  renderPage();
  await waitFor(() => assert.ok(screen.getByRole("heading", { name: "Qwen2.5-7B-Instruct" })));
  assert.equal(screen.queryByText("Request accounting (last 5 min)"), null);
});

test("the alignment card renders server_errors, in_flight and - with a persistent discrepancy - the red lost sentence", async () => {
  api.monitorDeployment = async () => deployment;
  renderPage();
  await waitFor(() => assert.ok(screen.getByText("Request accounting (last 5 min)")));
  assert.ok(screen.getByText(/Client errors: 12/));
  assert.ok(screen.getByText(/Server errors: 8/));
  assert.ok(screen.getByText(/In flight: 3/));
  const lost = screen.getByText("6 request(s) were lost between the proxy and the engine");
  assert.ok(lost.className.includes("ant-typography-danger"), "lost sentence renders in the danger colour");
});

test("the alignment card shows in_flight but no red lost sentence when the discrepancy is not persistent, even with lost > 0", async () => {
  api.monitorDeployment = async () => ({ ...deployment, alignment: { ...deployment.alignment!, lost: 6, discrepancy_persistent: false } });
  renderPage();
  await waitFor(() => assert.ok(screen.getByText("Request accounting (last 5 min)")));
  assert.ok(screen.getByText(/In flight: 3/));
  assert.equal(screen.queryByText(/were lost between the proxy/), null);
  assert.equal(document.querySelector(".ant-typography-danger"), null, "no red text at all");
});

test("in_flight renders as 0 rather than disappearing when nothing is in flight", async () => {
  api.monitorDeployment = async () => ({ ...deployment, alignment: { ...deployment.alignment!, in_flight: 0, lost: 0, discrepancy_persistent: false } });
  renderPage();
  await waitFor(() => assert.ok(screen.getByText(/In flight: 0/)));
});

test("every chart and card title carries an info icon whose tooltip defines the metric", async () => {
  api.monitorDeployment = async () => deployment;
  renderPage();
  await waitFor(() => assert.ok(screen.getByRole("heading", { name: "Qwen2.5-7B-Instruct" })));
  for (const title of ["End-to-end latency breakdown", "Health", "Request accounting (last 5 min)", "TTFT and end-to-end", "TPOT (per output token)", "Input × output", "Uncached input × TTFT", "Concurrency", "KV Cache", "Throughput", "Stage duration (p95)", "Preemptions/s", "Replicas"]) {
    assert.ok(screen.getByRole("img", { name: `About ${title}` }), `info icon for ${title}`);
  }
  await userEvent.setup().hover(screen.getByRole("img", { name: "About Stage duration (p95)" }));
  await waitFor(() => assert.ok(screen.getByText(/queue = wait from a request entering the engine until prefill starts/)));
  assert.ok(screen.getByText(/decode = time generating the whole output after the first token/));
});

test("focusing an info icon (keyboard) shows its help text too, and the icon carries the help as its title", async () => {
  api.monitorDeployment = async () => deployment;
  renderPage();
  await waitFor(() => assert.ok(screen.getByRole("heading", { name: "Qwen2.5-7B-Instruct" })));
  const icon = screen.getByRole("img", { name: "About Request accounting (last 5 min)" });
  assert.ok(icon.getAttribute("title")?.includes("more requests are missing than could have been in flight at any point in the window"));
  assert.equal(screen.queryByText(/more requests are missing than could have been in flight/, { selector: ".ant-tooltip-inner" }), null);
  icon.focus();
  await waitFor(() => assert.ok(screen.getByText(/more requests are missing than could have been in flight/, { selector: ".ant-tooltip-inner" })));
});

test("ReplicaTable renders mem_bw_pct per GPU for every replica that reports it", async () => {
  api.monitorDeployment = async () => deployment;
  renderPage();
  await waitFor(() => assert.ok(screen.getByRole("heading", { name: "Qwen2.5-7B-Instruct" })));
  // Fixture: replica 1's GPUs 0/1 report 76%/74%; replica 2's GPUs 2/3
  // report 72%/70% - both replicas, not just the one that used to carry
  // real values.
  // Each GPU is its own stacked "GPU n · v%" line, not a "n:v%" run-on.
  assert.ok(screen.getByText("GPU 0 · 76%"));
  assert.ok(screen.getByText("GPU 1 · 74%"));
  assert.ok(screen.getByText("GPU 2 · 72%"));
  assert.ok(screen.getByText("GPU 3 · 70%"));
  assert.equal(screen.queryByText(/0:76%/), null);
});

test("chart series are named '<metric label> · <instance short id>' so running/waiting and prompt/generation stay distinguishable", () => {
  const name = (labels: Record<string, string>, index = 0) => serviceSeriesName(i18n.t.bind(i18n) as never, labels, index);
  const instance = "ad759c35bbbb3333cccc4444dddd0000";
  assert.equal(name({ metric: "running", instance }), "Running · ad759c35");
  assert.equal(name({ metric: "waiting", instance }), "Waiting · ad759c35");
  assert.equal(name({ metric: "prompt_tok_s", instance }), "Prompt tok/s · ad759c35");
  assert.equal(name({ metric: "generation_tok_s", instance }), "Generation tok/s · ad759c35");
  assert.equal(name({ metric: "usage_pct", instance }), "Usage % · ad759c35");
  assert.equal(name({ metric: "hit_rate_pct", instance }), "Hit rate % · ad759c35");
  // Deployment-wide series (latency/stage_time/preemptions) carry no
  // instance label and read as just the localized metric label.
  assert.equal(name({ metric: "ttft_p95" }), "TTFT p95");
  assert.equal(name({ metric: "tpot_p99" }), "TPOT p99");
  assert.equal(name({ metric: "decode_ms" }), "Decode (TPOT)");
  assert.equal(name({ metric: "preemptions_per_s" }), "Preemptions/s");
  // An unknown metric falls back to its own wire name; no metric label at
  // all falls back to a positional name.
  assert.equal(name({ metric: "future_metric", instance }), "future_metric · ad759c35");
  assert.equal(name({}, 2), "Series 3");
  // And the chart option built for the fixture's concurrency chart carries
  // those names as its legend/series names.
  const option = buildTimeSeriesOption(deployment.charts.concurrency, { seriesName: name });
  assert.deepEqual((option.legend as { data: string[] }).data, ["Running · 1111aaaa", "Waiting · 1111aaaa"]);
});

test("a chart group with no series renders a muted not-collected card instead of an empty chart", async () => {
  // The base fixture now has data in every chart group (phase 2) - force
  // two back to empty to exercise the not-collected rendering path this
  // test is actually about.
  api.monitorDeployment = async () => ({ ...deployment, charts: { ...deployment.charts, latency: [], stage_time: [] } });
  renderPage();
  await waitFor(() => assert.ok(screen.getByRole("heading", { name: "Qwen2.5-7B-Instruct" })));
  assert.ok(screen.getAllByText("Not collected yet").length >= 2);
  // The placeholder takes the chart's own height, so an empty card lines up
  // with the drawn cards beside it.
  for (const placeholder of screen.getAllByText("Not collected yet")) {
    assert.equal((placeholder as HTMLElement).style.height, "200px");
  }
});

test("an engine that reports no metrics: engine-only charts say so, gateway-backed ones still draw", async () => {
  api.monitorDeployment = async () => ({
    ...deployment,
    sources: { ttft: "gateway", e2e: "gateway", generation_tokens: "gateway", prompt_tokens: "gateway", running: "gateway" },
    charts: { ...deployment.charts, tpot: [], kv: [], stage_time: [], preemptions: [] },
  });
  renderPage();
  await waitFor(() => assert.ok(screen.getByRole("heading", { name: "Qwen2.5-7B-Instruct" })));
  // KV and preemptions only an engine can fill; an empty TPOT or stage-time
  // chart is "not yet": the gateway measures a TPOT and sums up the stage
  // times replies report.
  assert.equal(screen.getAllByText("The engine reports no metrics; only an engine can provide this").length, 2);
  assert.equal(screen.getAllByText("Not collected yet").length, 2);
});

test("an empty gateway-backed chart is not blamed on the engine", async () => {
  api.monitorDeployment = async () => ({
    ...deployment,
    sources: { ttft: "gateway" },
    charts: { ...deployment.charts, latency: [], kv: [] },
  });
  renderPage();
  await waitFor(() => assert.ok(screen.getByRole("heading", { name: "Qwen2.5-7B-Instruct" })));
  // latency can come from the gateway, so its emptiness is "not yet".
  assert.equal(screen.getAllByText("Not collected yet").length, 1);
  assert.equal(screen.getAllByText("The engine reports no metrics; only an engine can provide this").length, 1);
});

test("a chart group with real series renders the chart instead of the not-collected placeholder", async () => {
  api.monitorDeployment = async () => deployment;
  renderPage();
  await waitFor(() => assert.ok(screen.getByRole("heading", { name: "Qwen2.5-7B-Instruct" })));
  // Every chart group has data in the fixture now (latency/stage_time/
  // preemptions included) - none of the six chart cards falls back to the
  // muted placeholder.
  assert.equal(screen.queryByText("Not collected yet"), null);
});

test("an invalid deployment id in the URL shows a not-found message without fetching", async () => {
  let calls = 0;
  api.monitorDeployment = async () => { calls += 1; return deployment; };
  renderPage("/monitor/d/not-a-number");
  await waitFor(() => assert.ok(screen.getByText("This deployment could not be found")));
  // Give any stray microtask a chance to run before asserting the network
  // was never touched - useMonitorDeployment must skip the fetch entirely
  // for an invalid id, not just discard its result.
  await new Promise((resolve) => setTimeout(resolve, 0));
  assert.equal(calls, 0);
});

test("a 404 from the server shows the not-found message", async () => {
  api.monitorDeployment = async () => { throw new APIError("not_found", undefined, "not found", 404); };
  renderPage();
  await waitFor(() => assert.ok(screen.getByText("This deployment could not be found")));
});

test("a deleted deployment's page names it, says it was deleted and points at Analytics", async () => {
  api.monitorDeployment = async () => {
    throw new APIError("MONITOR_DEPLOYMENT_DELETED", { deployment_id: 102, name: "glm-old", served_name: "org/glm" }, "deployment glm-old was deleted", 404);
  };
  renderPage();
  await waitFor(() => assert.ok(screen.getByText("The service “glm-old” has been deleted")));
  assert.ok(screen.getAllByText("glm-old").length > 0, "the header names it by the name it left");
  assert.equal(screen.queryByText("This deployment could not be found"), null);
  assert.equal(screen.queryByRole("button", { name: "Go to deployment" }), null, "no deployment page to go to");
  const link = screen.getByRole("link", { name: "Open Analytics" });
  assert.equal(link.getAttribute("href"), "/analytics/usage?range=30d&deployment=102");
});

test("a load failure's action reads the shared Retry label, not the back-to-monitor button's text", async () => {
  api.monitorDeployment = async () => { throw new Error("network down"); };
  renderPage();
  await waitFor(() => assert.ok(screen.getByRole("alert")));
  assert.ok(screen.getByRole("button", { name: "Retry" }));
  // "Back to monitor" still exists as the header's own nav button, but the
  // error action must not reuse its label for an unrelated retry action.
  assert.equal(screen.getAllByText("Back to Monitor Center").length, 1);
});

test("the tabs below the breakdown switch between the overview and the request lists", async () => {
  api.monitorDeployment = async () => deployment;
  renderPage();
  await waitFor(() => assert.ok(screen.getByRole("heading", { name: "Qwen2.5-7B-Instruct" })));
  // The overview tab holds the charts, the replicas and the logs.
  assert.ok(screen.getByRole("tab", { name: "Overview", selected: true }));
  assert.ok(screen.getByRole("img", { name: "About Replicas" }));
  await userEvent.setup().click(screen.getByRole("tab", { name: "In flight" }));
  await waitFor(() => assert.ok(screen.getByText("No requests in flight")));
  assert.equal(screen.queryByRole("img", { name: "About Replicas" }), null);
  // The breakdown and the health card stay above the tabs.
  assert.ok(screen.getByText("End-to-end latency breakdown"));
  assert.ok(screen.getByText("Health"));
  await userEvent.setup().click(screen.getByRole("tab", { name: "History" }));
  await waitFor(() => assert.ok(screen.getByText("Page 1")));
});

test("a service with no ready replicas explains recovery without inventing a crash diagnosis", async () => {
  api.monitorDeployment = async () => ({
    ...deployment,
    deployment: { ...deployment.deployment, replicas: { ready: 0, desired: 2 } },
    replicas: deployment.replicas.map((replica) => ({ ...replica, state: "starting" })),
  });
  renderPage();
  assert.ok(await screen.findByText("No replica is ready to serve requests"));
  assert.ok(screen.getByText(/starting or recovering/));
  assert.ok(screen.getByRole("link", { name: "Go to deployment" }));
});

test("a healthy or intentionally stopped service does not show a recovery warning", async () => {
  api.monitorDeployment = async () => ({
    ...deployment,
    deployment: { ...deployment.deployment, replicas: { ready: 0, desired: 0 } },
    replicas: [],
  });
  renderPage();
  await screen.findByRole("heading", { name: deployment.deployment.name });
  assert.equal(screen.queryByText("No replica is ready to serve requests"), null);
});

test("keeps replicas visible without a loading mask when follow is enabled during a slow refresh", async () => {
  let calls = 0;
  api.monitorDeployment = () => ++calls === 1 ? Promise.resolve(deployment) : new Promise(() => {});
  const { container } = renderPage();
  await waitFor(() => assert.ok(screen.getByRole("heading", { name: "Qwen2.5-7B-Instruct" })));
  await userEvent.setup().click(screen.getByRole("switch", { name: "Follow latest" }));
  assert.equal(calls, 2);
  assert.ok(screen.getByRole("heading", { name: "Qwen2.5-7B-Instruct" }));
  await act(async () => { await new Promise((resolve) => setTimeout(resolve, 600)); });
  assert.equal(container.querySelector(".ant-spin-spinning"), null);
});

test("switching deployment ignores a pending old response and clears old metrics", async () => {
  let oldResolve!: (value: MonitorDeployment) => void;
  let nextResolve!: (value: MonitorDeployment) => void;
  let calls = 0;
  api.monitorDeployment = (id) => {
    if (id === 103) return new Promise((r) => { nextResolve = r; });
    return ++calls === 1 ? Promise.resolve(deployment) : new Promise((r) => { oldResolve = r; });
  };
  const { container } = renderPage();
  await waitFor(() => assert.ok(screen.getByRole("heading", { name: "Qwen2.5-7B-Instruct" })));
  await userEvent.setup().click(screen.getByRole("switch", { name: "Follow latest" }));
  await userEvent.setup().click(screen.getByRole("link", { name: "Next deployment" }));
  assert.equal(screen.queryByRole("heading", { name: "Qwen2.5-7B-Instruct" }), null);
  assert.ok(container.querySelector(".ant-skeleton"));
  await act(async () => oldResolve(deployment));
  assert.equal(screen.queryByRole("heading", { name: "Qwen2.5-7B-Instruct" }), null);
  await act(async () => nextResolve({ ...deployment, deployment: { ...deployment.deployment, id: 103, name: "Next service" } }));
  await waitFor(() => assert.ok(screen.getByRole("heading", { name: "Next service" })));
});


test("changing range preserves the selected requests tab", async () => {
  api.monitorDeployment = async () => deployment;
  renderPage();
  await waitFor(() => assert.ok(screen.getByRole("heading", { name: "Qwen2.5-7B-Instruct" })));
  await userEvent.setup().click(screen.getByRole("tab", { name: "In flight" }));
  await userEvent.setup().click(screen.getByText("24h"));
  await waitFor(() => assert.ok(screen.getByRole("tab", { name: "In flight", selected: true })));
  assert.ok(screen.getByText("No requests in flight"));
});


test("changing range preserves the history latency filter", async () => {
  api.monitorDeployment = async () => deployment;
  renderPage();
  await waitFor(() => assert.ok(screen.getByRole("heading", { name: "Qwen2.5-7B-Instruct" })));
  const user = userEvent.setup();
  await user.click(screen.getByRole("tab", { name: "History" }));
  await user.type(screen.getByRole("spinbutton", { name: "End-to-end ≥" }), "12");
  await user.click(screen.getByText("24h"));
  await waitFor(() => assert.equal((screen.getByRole("spinbutton", { name: "End-to-end ≥" }) as HTMLInputElement).value, "12"));
});

test("service breadcrumb includes the current service and returns to its parent list", async () => {
  api.monitorDeployment = async () => deployment;
  renderPage();
  await screen.findByRole("heading", { name: "Qwen2.5-7B-Instruct" });
  const breadcrumb = screen.getByRole("navigation", { name: "breadcrumb" });
  assert.equal(breadcrumb.querySelector('[aria-current="page"]')?.textContent, "Qwen2.5-7B-Instruct");
  const parent = breadcrumb.querySelector<HTMLAnchorElement>('a[href="/monitor"]');
  assert.ok(parent);
  await userEvent.setup().click(parent);
  assert.ok(await screen.findByRole("heading", { name: "Monitor list destination" }));
});
