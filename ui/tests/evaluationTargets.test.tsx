import assert from "node:assert/strict";
import fs from "node:fs";
import { afterEach, test } from "node:test";
import { JSDOM } from "jsdom";
import React from "react";
import { createInstance } from "i18next";
import { I18nextProvider, initReactI18next } from "react-i18next";

const dom = new JSDOM("<!doctype html><html><body></body></html>", { url: "http://localhost/" });
for (const name of ["window", "document", "navigator", "HTMLElement", "HTMLInputElement", "SVGElement", "Element", "Node", "MutationObserver", "ShadowRoot", "Document", "KeyboardEvent", "HTMLBodyElement"] as const) {
  Object.defineProperty(globalThis, name, { value: dom.window[name], configurable: true });
}
Object.defineProperty(globalThis, "localStorage", { value: dom.window.localStorage, configurable: true });
Object.defineProperty(globalThis, "getComputedStyle", { value: (element: Element) => dom.window.getComputedStyle(element), configurable: true });
Object.defineProperty(dom.window, "matchMedia", { configurable: true, value: () => ({ matches: false, addListener() {}, removeListener() {}, addEventListener() {}, removeEventListener() {}, dispatchEvent: () => false }) });
Object.defineProperty(globalThis, "ResizeObserver", { value: class { observe() {} unobserve() {} disconnect() {} }, configurable: true });
Object.defineProperties(dom.window.HTMLElement.prototype, {
  attachEvent: { configurable: true, value() {} },
  detachEvent: { configurable: true, value() {} },
});
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
class Events { addEventListener() {} removeEventListener() {} close() {} }
Object.defineProperty(globalThis, "EventSource", { value: Events, configurable: true });
class ShimPort {
  onmessage: ((ev: { data: unknown }) => void) | null = null;
  postMessage(data: unknown) { setTimeout(() => this.onmessage?.({ data }), 0); }
}
Object.defineProperty(globalThis, "MessageChannel", { value: class { port1 = new ShimPort(); port2 = new ShimPort(); }, configurable: true });

const evaluations = JSON.parse(fs.readFileSync(new URL("../src/i18n/locales/en/evaluations.json", import.meta.url), "utf8"));
const i18n = createInstance();
await i18n.use(initReactI18next).init({
  lng: "en",
  fallbackLng: "en",
  resources: { en: { evaluations, common: JSON.parse(fs.readFileSync(new URL("../src/i18n/locales/en/common.json", import.meta.url), "utf8")) } },
  defaultNS: "evaluations",
  interpolation: { escapeValue: false },
});

const { MemoryRouter, Route, Routes } = await import("react-router-dom");
const { cleanup, fireEvent, render, screen, waitFor, within } = await import("@testing-library/react");
const { api } = await import("../src/api.ts");
const { default: EvaluationsPage } = await import("../src/pages/EvaluationsPage.tsx");
const { default: EvaluationDetailPage } = await import("../src/pages/evaluations/EvaluationDetailPage.tsx");
import type { BenchmarkMeta, BenchmarkRun, DeploymentListItem, DeploymentPage, ListQuery, Node, NodePage } from "../src/api";
import { list } from "./helpers/listPage.ts";

const meta = {
  modes: [{ value: "saturation", label: { en: "Saturation", zh: "饱和" }, description: { en: "d", zh: "d" } }],
  statuses: [{ value: "done", label: { en: "Done", zh: "完成" }, color: "green" }],
  mode_defaults: { saturation: { concurrency: [1], max_concurrency: 8, isl: [128], osl: [128], requests_per_point_multiplier: 10, min_throughput_gain: 0.05, objective: "output_tps" } },
  datasets: [{ value: "random", label: { en: "Random", zh: "随机" } }],
  sla_constraints: [],
  detail_cap_mb: { default: 20, min: 1, max: 50 },
  load_defaults: { warmup_requests: 1, max_warmup_requests: 1000, request_timeout_s: 600, min_request_timeout_s: 10, max_request_timeout_s: 7200, burstiness: 1, point_duration_s: 60, min_point_duration_s: 5, max_point_duration_s: 3600, min_open_loop_requests: 20, max_request_rate: 10000, max_request_rates: 32, max_burstiness: 100, max_in_flight: 100000, ignore_eos: true, ignore_eos_datasets: ["random", "agentic"] },
  tokenizer_schemes: { huggingface: "hf", modelscope: "ms" },
  agentic_presets: [{ value: "agentic-80k-v1", label: "80K", mean_input_tokens: 80_000, first_turn_input_tokens: 74_162, peak_input_tokens: 85_838, required_context_tokens: 90_000, recommended: true }],
  agentic_defaults: { preset: "agentic-80k-v1", turns: 13, subsequent_turn_input_tokens: 753, output_tokens: 220, seed: 42, concurrency: [1, 2, 4, 8], sessions_per_point: { "1": 4 }, sessions_per_concurrency: 2, min_sessions_per_point: 4 },
  dataset_profiles: {},
  objectives: [{ value: "output_tps", label: { en: "Output", zh: "输出" } }],
} as unknown as BenchmarkMeta;

const item = (id: number, name: string, node = "node-1"): DeploymentListItem => ({
  id, name, model_id: 1, model_name: "m", served_name: name, engine: "vllm", node_uuid: node, desired_state: "active",
  runtime_state: "running", sleepable: false, replicas: 1, template_deviated: false, created_at: "2026-10-03T00:00:00Z", operations: [],
});
const node = (uuid: string, name: string): Node => ({ uuid, name, hostname: uuid, state: "online", caps: ["bench"], gpus: [], labels: {} }) as unknown as Node;
const fleet = Array.from({ length: 60 }, (_, i) => item(i + 1, `svc-${i + 1}`));
const run = {
  id: 5, name: "nightly", target_kind: "instance", deployment_id: 3, model: "m", load_origin: "auto", num_gpus: 1, mode: "saturation",
  state: "done", created_at: "2026-10-03T00:00:00Z", dataset: { kind: "random" }, search: {},
  target_snapshot: { target_kind: "instance", deployment_id: 3, deployment_name: "svc-3 at the time", model: "m" },
} as unknown as BenchmarkRun;

let deploymentQueries: ListQuery[] = [];
let nodeQueries: ListQuery[] = [];
let detailQueries: number[] = [];
let nodeDetailQueries: string[] = [];
const originals = { ...api };
function stubApi() {
  deploymentQueries = [];
  nodeQueries = [];
  detailQueries = [];
  nodeDetailQueries = [];
  api.deployments = async () => { throw new Error("fetched every deployment"); };
  api.nodes = async () => { throw new Error("fetched every node"); };
  api.benchmarks = async () => list([run]);
  api.benchmarkMeta = async () => meta;
  api.benchmark = async () => run;
  api.benchmarkCurve = async () => ({ points: [] }) as never;
  api.deploymentsPage = async (query: ListQuery): Promise<DeploymentPage> => {
    deploymentQueries.push(query);
    const q = query.q?.toLowerCase() ?? "";
    const matched = fleet.filter((row) => row.name.includes(q));
    return { items: matched.slice(0, query.page_size), page: 1, page_size: query.page_size, total: matched.length };
  };
  api.nodesPage = async (query: ListQuery): Promise<NodePage> => {
    nodeQueries.push(query);
    return { items: [node("node-2", "bench-box")], page: 1, page_size: query.page_size, total: 1, all: 2, labels: [] };
  };
  api.deployment = async (id: number) => {
    detailQueries.push(id);
    return { ...item(id, `svc-${id}`, "node-1"), instances: [{ uuid: `leader-${id}`, deployment_id: id, node_uuid: "runtime-node", state: "running", sleep_level: 0, node_rank: 0, headless: false, api_port: 8000 }] } as never;
  };
  api.node = async (uuid: string) => { nodeDetailQueries.push(uuid); return node(uuid, "target-box"); };
}
afterEach(() => { cleanup(); Object.assign(api, originals); });

const wrap = (path: string, element: React.ReactElement, route = "/evaluations") => render(
  <I18nextProvider i18n={i18n}>
    <MemoryRouter initialEntries={[path]}>
      <Routes><Route path={route} element={element} /></Routes>
    </MemoryRouter>
  </I18nextProvider>,
);

test("the evaluation list names each run's target from its snapshot and lists nothing else", async () => {
  stubApi();
  wrap("/evaluations", <EvaluationsPage />);
  await screen.findByText("svc-3 at the time");
  await new Promise((resolve) => setTimeout(resolve, 50));
  assert.deepEqual(deploymentQueries, [], "a closed wizard needs no deployment choices");
  assert.deepEqual(nodeQueries, [], "a closed wizard needs no node choices");
});

test("the wizard offers one page of running deployments and online nodes and searches on the server", async () => {
  stubApi();
  wrap("/evaluations?create=saturation&deployment_id=7", <EvaluationsPage />);
  await waitFor(() => assert.ok(deploymentQueries.length > 0 && nodeQueries.length > 0));
  assert.deepEqual(deploymentQueries[0], { page: 1, page_size: 50, q: "", state: ["running"] });
  assert.deepEqual(nodeQueries[0], { page: 1, page_size: 50, q: "", state: ["online"] });

  const service = await screen.findByRole("combobox", { name: evaluations.wizard.service });
  fireEvent.change(service, { target: { value: "svc-5" } });
  await waitFor(() => assert.equal(deploymentQueries.at(-1)?.q, "svc-5"));

	// The actual runtime leader is pinned even when its node is outside the
	// searched page. Unrelated online nodes cannot generate managed direct load.
	const origin = screen.getByRole("combobox", { name: evaluations.wizard.loadOrigin });
	// The server is the default origin: a managed deployment is measured
	// through the gateway, which is also the only end-to-end path for
	// multi-instance and PD deployments.
	assert.ok(within(origin.closest(".ant-select") as HTMLElement).getByText("Server"));
	fireEvent.mouseDown(origin);
  await waitFor(() => assert.ok(document.querySelector(`.ant-select-item-option[title="${evaluations.wizard.targetNode} · target-box"]`)));
  assert.equal(document.querySelector(`.ant-select-item-option[title="${evaluations.wizard.computeNode} · bench-box"]`), null);
  assert.ok(detailQueries.length > 0);
  assert.deepEqual([...new Set(detailQueries)], [7], "detail refreshes never scan the deployment fleet");
  assert.ok(nodeDetailQueries.length > 0);
  assert.deepEqual([...new Set(nodeDetailQueries)], ["runtime-node"], "metadata lookups use actual eligible runtime nodes");
  fireEvent.change(origin, { target: { value: "bench" } });
  await waitFor(() => assert.equal(nodeQueries.at(-1)?.q, "bench"));
});

test("a preset deployment beyond the first page is looked up so the wizard can name it", async () => {
  stubApi();
  let lookedUp = 0;
  api.deployment = async (id: number) => { lookedUp = id; return { ...item(id, "far-away-service", "node-9"), instances: [] } as never; };
  wrap("/evaluations?create=saturation&deployment_id=999", <EvaluationsPage />);
  await waitFor(() => assert.equal(lookedUp, 999));
  const field = (await screen.findByRole("combobox", { name: evaluations.wizard.service })).closest(".ant-select") as HTMLElement;
  await waitFor(() => assert.ok(within(field).getByText("far-away-service")));
});

test("the evaluation detail fetches wizard choices only once a rerun opens it", async () => {
  stubApi();
  wrap("/evaluations/5", <EvaluationDetailPage />, "/evaluations/:id");
  const rerun = await screen.findByRole("button", { name: new RegExp(evaluations.detail.rerun) });
  await new Promise((resolve) => setTimeout(resolve, 50));
  assert.deepEqual(deploymentQueries, []);
  // The target card names the deployment, not one instance of it: a
  // multi-instance deployment has no single instance, node, or TP to show.
  const deployment = screen.getByRole("link", { name: "svc-3 at the time" });
  assert.match(deployment.getAttribute("href") || "", /deployment=3/);
  assert.equal(screen.queryByText("TP"), null);
  assert.equal(screen.queryByText("Instance"), null);
  assert.equal(screen.queryByText("Node"), null);
  fireEvent.click(rerun);
  await waitFor(() => assert.deepEqual(deploymentQueries[0], { page: 1, page_size: 50, q: "", state: ["running"] }));
  await waitFor(() => assert.deepEqual(nodeQueries[0], { page: 1, page_size: 50, q: "", state: ["online"] }));
});

test("a failed choice lookup leaves the wizard usable with no choices", async () => {
  stubApi();
  api.deploymentsPage = async (query: ListQuery) => { deploymentQueries.push(query); throw new Error("down"); };
  wrap("/evaluations?create=saturation", <EvaluationsPage />);
  await waitFor(() => assert.ok(deploymentQueries.length > 0));
  // The wizard still opens on its first step; the service list is just empty.
  await waitFor(() => assert.ok(document.querySelector('[role="dialog"]')));
  assert.ok(within(document.querySelector('[role="dialog"]') as HTMLElement).getAllByText(evaluations.wizard.title).length > 0);
});
