import { installCanvas } from "./helpers/analyticsFixtures.ts";
import assert from "node:assert/strict";
import { fakeFleet } from "./helpers/fakeFleet.ts";
import { listPage, cursorPage, list } from "./helpers/listPage.ts";
import fs from "node:fs";
import { after, afterEach, test } from "node:test";
import { trackMessageChannels } from "./helpers/trackedMessageChannels.ts";
import { JSDOM } from "jsdom";
import React from "react";
import { createInstance } from "i18next";
import { I18nextProvider, initReactI18next } from "react-i18next";

// The deployment drawer's lifecycle, for every shape: the header's Start
// (stopped deployments), Restart (a confirm, not rolling) and Stop (keeps the
// drawer open); an ordinary deployment's replica that gave up with its
// Retry; the leader/follower rank tags of a multi-node replica across pages; and the monitor's instance link scrolling to its panel.

const closeChannels = trackMessageChannels();
const dom = new JSDOM("<!doctype html><html><body></body></html>", { url: "http://localhost/" });
for (const name of ["window", "document", "navigator", "HTMLElement", "SVGElement", "Element", "Node", "MutationObserver", "ShadowRoot", "File", "Blob", "HTMLBodyElement", "HTMLHtmlElement", "NodeFilter", "HTMLInputElement", "HTMLButtonElement"] as const) {
  Object.defineProperty(globalThis, name, { value: dom.window[name], configurable: true });
}
Object.defineProperty(globalThis, "localStorage", { value: dom.window.localStorage, configurable: true });
Object.defineProperty(globalThis, "getComputedStyle", { value: (element: Element) => dom.window.getComputedStyle(element), configurable: true });
Object.defineProperty(dom.window, "matchMedia", { configurable: true, value: () => ({ matches: false, addListener() {}, removeListener() {}, addEventListener() {}, removeEventListener() {}, dispatchEvent: () => false }) });
installCanvas(dom);
class ResizeObserverStub { observe() {} unobserve() {} disconnect() {} }
Object.defineProperty(globalThis, "ResizeObserver", { value: ResizeObserverStub, configurable: true });
const scrolled: string[] = [];
Object.defineProperties(dom.window.HTMLElement.prototype, {
  attachEvent: { configurable: true, value() {} },
  detachEvent: { configurable: true, value() {} },
  scrollIntoView: { configurable: true, value(this: HTMLElement) { scrolled.push(this.id); } },
});

const i18n = createInstance();
await i18n.use(initReactI18next).init({
  lng: "en",
  fallbackLng: "en",
  resources: { en: {
    deployments: JSON.parse(fs.readFileSync(new URL("../src/i18n/locales/en/deployments.json", import.meta.url), "utf8")),
    common: JSON.parse(fs.readFileSync(new URL("../src/i18n/locales/en/common.json", import.meta.url), "utf8")),
    deploy: JSON.parse(fs.readFileSync(new URL("../src/i18n/locales/en/deploy.json", import.meta.url), "utf8")),
  } },
  defaultNS: "deployments",
  interpolation: { escapeValue: false },
});
const locale = i18n.getResourceBundle("en", "deployments");

const { MemoryRouter } = await import("react-router-dom");
const { cleanup, fireEvent, render, screen, waitFor, within } = await import("@testing-library/react");
const userEvent = (await import("@testing-library/user-event")).default;

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

const { selectInstance } = await import("./helpers/selectInstance.ts");
const { api } = await import("../src/api.ts");
const { APIError } = await import("../src/apiError.ts");
const { default: DeploymentsPage } = await import("../src/pages/DeploymentsPage.tsx");

const apiAny = api as unknown as Record<string, unknown>;
function stub(name: string, value: unknown) { apiAny[name] = value; }

// stubFleet answers the node endpoints as the server does (one searched page,
// single nodes, facets) and refuses listing every node.
function stubFleet(list: unknown[]) {
  const fake = fakeFleet(list as never);
  for (const name of ["nodes", "nodesPage", "node", "nodeFacets"] as const) stub(name, fake[name]);
  return fake;
}
function resetStubs() {
  stub("routingPolicies", async () => list([]));
  stub("instanceEvents", async () => cursorPage([]));
  stub("metricsQuery", async () => list([]));
  stubFleet([]);
  stub("monitorDeployment", async () => ({
    as_of: 0, stale: false,
    deployment: { id: 1, name: "dep", engine: "vllm", placement: "", replicas: { ready: 1, desired: 1 } },
    alignment: null, ttft_breakdown: null,
    charts: { latency: [], tpot: [], concurrency: [], kv: [], throughput: [], stage_time: [], preemptions: [] },
    replicas: [],
  }));
  stub("deploymentEvents", async () => cursorPage([]));
}
resetStubs();
afterEach(() => { cleanup(); Events.listeners.clear(); resetStubs(); scrolled.length = 0; });
after(() => { closeChannels(); dom.window.close(); });

// instance is an instance as the server lists it, with the server's
// verdicts: replica names the replica it fills (none for a failed attempt),
// and it takes no sleep order of its own (the deployments here cannot sleep).
function instance(uuid: string, extra: Record<string, unknown> = {}): unknown {
  return {
    uuid, deployment_id: 1, node_uuid: `node-${uuid}`, state: "running",
    gpu_indexes: [0], ports: [30000], api_port: 30000, image: "runner:vllm",
    message: "", container_id: `container-${uuid}`, sleep_level: 0, desired_sleep: 0, dist_role: "leader", nnodes: 1, node_rank: 0,
    asleep: false, can_sleep_alone: false, can_wake_alone: false,
    ...extra,
  };
}

function deployment(overrides: Record<string, unknown> = {}) {
  return {
    id: 1, name: "lifecycle-deployment", model_name: "model", model_id: 1, served_name: "model",
    engine: "vllm", node_uuid: "node-a", desired_state: "active", runtime_state: "running",
    sleepable: false, health: { status: "off", threshold_sec: 60, last: null, recent_mean_ms: null, recent_ok: 0, recent: [] }, replicas: 2, max_replicas: 32, template_deviated: false, created_at: "2026-09-30T00:00:00Z",
    operations: [], instances: [instance("inst-a", { replica: { index: 0 } }), instance("inst-b", { replica: { index: 1 } })],
    ...overrides,
  };
}

async function openDetail(search = "?deployment=1") {
  render(
    <I18nextProvider i18n={i18n}>
      <MemoryRouter initialEntries={[`/deployments${search}`]}>
        <DeploymentsPage />
      </MemoryRouter>
    </I18nextProvider>,
  );
  await screen.findAllByText("lifecycle-deployment");
  return screen.findByRole("button", { name: locale.detail.stop });
}

function drawer(): HTMLElement {
  return screen.getByRole("dialog", { name: "lifecycle-deployment" });
}

test("the replica count is edited in place and applied once changed", async () => {
  stub("deploymentsPage", async () => listPage([deployment()]));
  stub("deployment", async () => deployment());
  const scales: unknown[] = [];
  let fail = true;
  stub("scaleReplicas", async (id: number, body: unknown) => {
    scales.push({ id, body });
    if (fail) throw new APIError("REPLICAS_CHANGED", {}, "the replicas changed since the request was read; read them again", 409);
    return deployment({ replicas: 3 });
  });
  await openDetail();
  assert.equal((screen.getByRole("spinbutton", { name: locale.detail.replicas }) as HTMLInputElement).value, "2");
  const input = screen.getByRole("spinbutton", { name: locale.detail.replicas }) as HTMLInputElement;
  const apply = screen.getByRole("button", { name: locale.replica.apply }) as HTMLButtonElement;
  // The bound is the server's.
  assert.equal(input.getAttribute("max"), "32");
  assert.ok(apply.disabled, "nothing to apply before an edit");
  await userEvent.clear(input);
  await userEvent.type(input, "3");
  await waitFor(() => assert.equal(apply.disabled, false));
  await userEvent.click(apply);
  await screen.findByText(i18n.t("replica.scaleFailed", { detail: i18n.t("common:apiError.REPLICAS_CHANGED") }));
  fail = false;
  await userEvent.click(apply);
  await waitFor(() => assert.equal(scales.length, 2));
  assert.deepEqual(scales[1], { id: 1, body: { replicas: 3 } });
});

test("replica readiness and scale share one service field while node placement stays in scheduling above both tabs", async () => {
  const dep = deployment({ ready_replicas: 0, replicas: 2 });
  stub("deploymentsPage", async () => listPage([dep]));
  stub("deployment", async () => dep);
  await openDetail();
  const overview = screen.getByRole("heading", { name: locale.detail.serviceOverview }).closest("section")!;
  const capacity = within(overview).getByText(locale.detail.replicaCapacity).closest("tr")! as HTMLElement;
  const readyText = i18n.t("detailLayout.ready", { ready: 0, total: 2 });
  assert.ok(within(capacity).getByText(readyText));
  assert.equal(within(overview).getAllByText(readyText).length, 1);
  assert.ok(within(capacity).getByRole("spinbutton", { name: locale.detail.replicas }));
  assert.equal(within(overview).queryByText(locale.detail.targetNode), null);
  const settings = screen.getByRole("heading", { name: locale.detail.schedulingInformation }).closest("section")!;
  assert.ok(within(settings).getByText(locale.detail.targetNode));
  assert.ok(within(settings).getByText(locale.page.runtimeNodes.none));
  assert.ok(within(settings).getByText(locale.detail.placement.requirements));
  assert.equal(within(settings).queryByText(locale.detail.replicaCapacity), null);
  const countedTab = locale.detail.instances.replace("{{count}}", "2");
  const inspectionTabs = within(drawer()).getByRole("tab", { name: countedTab }).closest<HTMLElement>('[role="tablist"]')!;
  const drawerTabs = within(inspectionTabs).getAllByRole("tab");
  assert.deepEqual(drawerTabs.map((tab) => tab.textContent), [locale.detail.title, countedTab]);
  assert.equal(within(drawer()).queryByRole("heading", { name: countedTab }), null);
  assert.ok(drawerTabs[0].compareDocumentPosition(settings) & Node.DOCUMENT_POSITION_FOLLOWING);
  fireEvent.click(drawerTabs[1]);
  assert.equal(drawerTabs[1].getAttribute("aria-selected"), "true");
  assert.ok(document.getElementById("deployment-instances"));
  assert.equal(screen.queryByRole("heading", { name: locale.detail.schedulingInformation }), null);
});

test("startup shows the exhausted attempt while the selected instance owns its failure message and logs", async () => {
  const dep = deployment({
    replicas: 1, ready_replicas: 0, runtime_state: "failed",
    runtime_nodes: [{ node_uuid: "node-latest-fail", node_name: "node-a" }],
    retry_exhausted: { index: 0, failed_attempts: 3 },
    instances: [
      instance("latest-fail", { state: "failed", phase: "container_launch", message: "failed to start container" }),
      instance("early-fail", { state: "failed", message: "image could not be pulled" }),
    ],
  });
  stub("deploymentsPage", async () => listPage([dep]));
  stub("deployment", async () => dep);
  stub("instanceEvents", async (uuid: string) => cursorPage(uuid === "latest-fail" ? [
    { id: 4, code: "INSTANCE_STATUS_UPDATED", params: { state: "failed", prev_phase: "container_launch", prev_duration_ms: 320 } },
    { id: 3, created_at: "2026-11-10T02:01:00Z", params: { phase: "container_launch", detail: "launching the model process", prev_phase: "image_pull", prev_duration_ms: 9000, prev_bytes_done: 1048576, prev_rate_bps: 2048 } },
    { id: 2, created_at: "2026-11-10T02:00:51Z", params: { phase: "image_pull" } },
    { id: 1, code: "INSTANCE_CREATED", created_at: "2026-11-10T02:00:50Z" },
  ] : []));
  await openDetail();
  selectInstance("latest-fail");
  const startup = await within(drawer()).findByRole("list", { name: locale.startup.title });
  await waitFor(() => assert.deepEqual(within(startup).getAllByRole("listitem").map(item => item.dataset.status), ["done", "done", "failed"]));
  const [created, pull, launch] = within(startup).getAllByRole("listitem");
  for (const step of [created, pull]) {
    assert.ok(within(step).getByText(locale.startup.done).classList.contains("sr-only"));
  }
  assert.ok(within(created).getByText("node-a"));
  assert.ok(within(launch).getByText("launching the model process"));
  const createdTime = created.querySelector("time")!;
  assert.equal(createdTime.parentElement?.textContent, createdTime.textContent);
  assert.ok(within(pull).getByText("runner:vllm"));
  const pullTime = pull.querySelector("time")!;
  assert.equal(pullTime.dateTime, "2026-11-10T02:00:51Z");
  assert.equal(pullTime.parentElement?.textContent, `9.0 s · ${pullTime.textContent}`);
  assert.match(pull.textContent ?? "", /1\.0 MiB/);
  assert.doesNotMatch(pull.textContent ?? "", /KiB\/s/);
  assert.ok(within(launch).getByText(locale.startup.failed));
  const launchTime = launch.querySelector("time")!;
  assert.equal(launchTime.parentElement?.textContent, `320 ms · ${launchTime.textContent}`);
  const startupCard = startup.closest("details")!;
  assert.equal(within(startupCard).queryByRole("button"), null);
  assert.ok(!startupCard.textContent?.includes("latest-fail"));
  const runtime = () => within(drawer()).getByRole("heading", { name: locale.detail.instanceRuntime }).closest("section")!;
  selectInstance("early-fail");
  assert.ok(within(runtime()).getByText("image could not be pulled"));
  selectInstance("latest-fail");
  assert.ok(within(runtime()).getByText("failed to start container"));
  assert.equal(within(drawer()).queryByText("image could not be pulled"), null);
  const logs = within(drawer()).getByRole("region", { name: locale.instance.tab.logs });
  assert.equal(logs.closest('[id^="instance-"]')?.id, "instance-latest-fail");
});

test("matching node records share the target links, with counts preserved regardless of record order", async () => {
  const dep = deployment({
    node_uuids: ["a", "b"], node_names: ["A", "B"],
    runtime_nodes: [{ node_uuid: "b", node_name: "B", instances: 2 }, { node_uuid: "a", node_name: "A", instances: 1 }],
  });
  stub("deploymentsPage", async () => listPage([dep]));
  stub("deployment", async () => dep);
  await openDetail();
  const scheduling = screen.getByRole("heading", { name: locale.detail.schedulingInformation }).closest("section")!;
  assert.ok(within(scheduling).getByText(locale.detail.nodesMatchRecords));
  assert.ok(within(scheduling).getByRole("link", { name: /A\s*×1/ }));
  assert.ok(within(scheduling).getByRole("link", { name: /B\s*×2/ }));
  assert.equal(within(scheduling).queryByText(locale.detail.node), null);
});

test("a target without an instance stays distinguishable from a node with a recorded instance", async () => {
  const dep = deployment({
    nnodes: 2,
    node_uuids: ["a", "b"], node_names: ["A", "B"],
    runtime_nodes: [{ node_uuid: "a", node_name: "A", instances: 1 }],
    placement_status: { unscheduled: 1 },
  });
  stub("deploymentsPage", async () => listPage([dep]));
  stub("deployment", async () => dep);
  await openDetail();
  const scheduling = screen.getByRole("heading", { name: locale.detail.schedulingInformation }).closest("section")!;
  assert.equal(within(scheduling).queryByText(locale.detail.nodesMatchRecords), null);
  const recorded = within(scheduling).getByText(locale.detail.node).closest("tr")! as HTMLElement;
  assert.ok(within(recorded).getByRole("link", { name: "A" }));
  assert.equal(within(recorded).queryByRole("link", { name: "B" }), null);
  assert.ok(within(scheduling).getByRole("button", { name: locale.detail.placement.view }));
});

test("a PD or multi-node deployment shows its replica count read-only", async () => {
  const multi = deployment({ nnodes: 2, replicas: 1 });
  stub("deploymentsPage", async () => listPage([multi]));
  stub("deployment", async () => multi);
  await openDetail();
  assert.equal(screen.queryByRole("spinbutton", { name: locale.detail.replicas }), null);
});

test("choosing all at once posts that strategy and warns about the outage", async () => {
  stub("deploymentsPage", async () => listPage([deployment()]));
  stub("deployment", async () => deployment());
  const calls: string[] = [];
  stub("restartDeployment", async (_id: number, strategy: string) => { calls.push(strategy); return deployment(); });
  await openDetail();
  fireEvent.click(within(drawer()).getByRole("button", { name: locale.detail.restart.action }));
  const modal = (await screen.findByText(locale.detail.restart.confirmTitle)).closest(".ant-modal") as HTMLElement;
  await userEvent.click(within(modal).getByRole("radio", { name: locale.detail.restart.strategyAll }));
  assert.ok(within(modal).getByText(locale.detail.restart.confirmDescription));
  fireEvent.click(within(modal).getByRole("button", { name: locale.detail.restart.confirmOk }));
  await waitFor(() => assert.deepEqual(calls, ["all"]));
});

test("a rolling restart in progress is shown in the drawer header", async () => {
  const rolling = deployment({ restart_requested_at: "2026-09-30T08:00:00Z" });
  stub("deploymentsPage", async () => listPage([rolling]));
  stub("deployment", async () => rolling);
  await openDetail();
  assert.ok(within(drawer()).getByText(locale.detail.restart.rolling));
});

const replicaNodes = [
  { uuid: "node-a", name: "Node A", hostname: "a", state: "online", gpu_count: 2, gpus: [] },
  { uuid: "node-b", name: "Node B", hostname: "b", state: "online", gpu_count: 2, gpus: [] },
];

function multiNodeDeployment(overrides: Record<string, unknown> = {}) {
  return deployment({
    nnodes: 2, replicas: 1,
    units: [{ node_uuid: "node-a", node_name: "Node A", followers: [{ node_uuid: "node-b", node_name: "Node B" }] }],
    instances: [
      instance("lead-a", { node_uuid: "node-a", nnodes: 2, node_rank: 0, dist_role: "leader", dist_group: "g1", dist_attempt: 1, replica: { index: 0 } }),
      instance("follow-b", { node_uuid: "node-b", nnodes: 2, node_rank: 1, headless: true, dist_role: "follower", dist_group: "g1", dist_attempt: 1, replica: { index: 0 } }),
    ],
    ...overrides,
  });
}

// chooseOption opens a select and picks an option, waiting for it: a node
// picker's options arrive from a server search after it opens.
async function chooseOption(select: HTMLElement, label: string) {
  const selector = select.closest(".ant-select")!.querySelector(".ant-select-selector") as HTMLElement;
  fireEvent.mouseDown(selector);
  let option: Element | undefined;
  await waitFor(() => {
    const dropdowns = document.querySelectorAll(".ant-select-dropdown");
    option = [...dropdowns[dropdowns.length - 1].querySelectorAll(".ant-select-item-option")].find((el) => el.textContent === label);
    if (!option) throw new Error(`option ${label} not offered`);
  });
  fireEvent.click(option!);
}

test("a multi-node deployment's replicas are listed and a new one is added with its nodes", async () => {
  stubFleet(replicaNodes);
  stub("deploymentsPage", async () => listPage([multiNodeDeployment()]));
  stub("deployment", async () => multiNodeDeployment());
  const scales: unknown[] = [];
  let fail = true;
  stub("scaleReplicas", async (id: number, body: unknown) => {
    scales.push({ id, body });
    if (fail) throw new APIError("REPLICA_LEADER_SHARED", { node: "node-b", other: 0, other_replica: 1, index: 1, replica: 2 }, "two replicas are led from one node", 400);
    return multiNodeDeployment({ replicas: 2 });
  });
  await openDetail();
  fireEvent.click(screen.getByRole("button", { name: locale.replica.editUnits }));
  const modal = (await screen.findByText(locale.replica.unitsTitle)).closest(".ant-modal") as HTMLElement;
  assert.ok(within(modal).getByText("Replica 1"));
  const save = () => within(modal).getByRole("button", { name: locale.replica.save }) as HTMLButtonElement;
  fireEvent.click(within(modal).getByRole("button", { name: locale.replica.addUnit }));
  // A new replica has no nodes yet: nothing to save until every rank has one.
  assert.ok(save().disabled);
  await chooseOption(within(modal).getByRole("combobox", { name: "Replica 2 leader node" }), "Node B");
  assert.ok(save().disabled);
  await chooseOption(within(modal).getByRole("combobox", { name: "Replica 2 rank 1 node" }), "Node A");
  await waitFor(() => assert.equal(save().disabled, false));
  fireEvent.click(save());
  await within(modal).findByText(i18n.t("replica.scaleFailed", { detail: i18n.t("common:apiError.REPLICA_LEADER_SHARED", { node: "node-b", other_replica: 1, replica: 2 }) }));
  fail = false;
  fireEvent.click(within(modal).getByText(locale.replica.save).closest("button")!);
  await waitFor(() => assert.equal(scales.length, 2));
  assert.deepEqual(scales[1], { id: 1, body: { units: [
    { node_uuid: "node-a", followers: [{ node_uuid: "node-b" }] },
    { node_uuid: "node-b", followers: [{ node_uuid: "node-a" }] },
  ] } });
});

test("the last listed replica cannot be removed in the dialog", async () => {
  stubFleet(replicaNodes);
  stub("deploymentsPage", async () => listPage([multiNodeDeployment()]));
  stub("deployment", async () => multiNodeDeployment());
  await openDetail();
  fireEvent.click(screen.getByRole("button", { name: locale.replica.editUnits }));
  const modal = (await screen.findByText(locale.replica.unitsTitle)).closest(".ant-modal") as HTMLElement;
  assert.ok((within(modal).getByRole("button", { name: "Remove replica 1" }) as HTMLButtonElement).disabled);
});

test("removing a multi-node replica from its leader sends the list without it", async () => {
  const two = multiNodeDeployment({
    replicas: 2,
    units: [
      { node_uuid: "node-a", followers: [{ node_uuid: "node-b" }] },
      { node_uuid: "node-b", followers: [{ node_uuid: "node-a" }] },
    ],
    instances: [
      instance("lead-a", { node_uuid: "node-a", nnodes: 2, node_rank: 0, dist_role: "leader", dist_group: "g1", dist_attempt: 1, replica: { index: 0 } }),
      instance("follow-b", { node_uuid: "node-b", nnodes: 2, node_rank: 1, headless: true, dist_role: "follower", dist_group: "g1", dist_attempt: 1, replica: { index: 0 } }),
      instance("lead-b", { node_uuid: "node-b", nnodes: 2, node_rank: 0, dist_role: "leader", dist_group: "g2", dist_attempt: 1, replica: { index: 1 } }),
      instance("follow-a", { node_uuid: "node-a", nnodes: 2, node_rank: 1, headless: true, dist_role: "follower", dist_group: "g2", dist_attempt: 1, replica: { index: 1 } }),
    ],
  });
  stub("deploymentsPage", async () => listPage([two]));
  stub("deployment", async () => two);
  const scales: unknown[] = [];
  stub("scaleReplicas", async (_id: number, body: unknown) => { scales.push(body); return multiNodeDeployment(); });
  await openDetail();
  selectInstance("follow-a");
  assert.equal(within(document.getElementById("instance-follow-a") as HTMLElement).queryByRole("button", { name: locale.replica.remove }), null);
  selectInstance("lead-b");
  const panel = screen.getByRole("group", { name: locale.detailLayout.replicaControls });
  fireEvent.click(within(panel).getByRole("button", { name: locale.replica.remove }));
  const confirm = (await screen.findByText(/^Remove replica .*\?/i)).closest(".ant-popover") as HTMLElement;
  fireEvent.click(within(confirm).getByRole("button", { name: locale.replica.remove }));
  await waitFor(() => assert.deepEqual(scales, [{ units: [{ node_uuid: "node-a", followers: [{ node_uuid: "node-b" }] }] }]));
});

test("a suggested multi-node replica is appended for the operator to check, and a refusal says why", async () => {
  const nodes = [...replicaNodes, { uuid: "node-c", name: "Node C", hostname: "c", state: "online", gpu_count: 2, gpus: [] }, { uuid: "node-d", name: "Node D", hostname: "d", state: "online", gpu_count: 2, gpus: [] }];
  stubFleet(nodes);
  stub("deployments", async () => list([multiNodeDeployment()]));
  stub("deployment", async () => multiNodeDeployment());
  const asked: unknown[] = [];
  let answer: unknown = {
    unit: { node_uuid: "node-c", followers: [{ node_uuid: "node-d" }] }, network: { id: 1, name: "ib" }, arch: "nvidia/ampere",
    nodes: [{ uuid: "node-c", name: "Node C", free_gpus: 2, model: "absent" }, { uuid: "node-d", name: "Node D", free_gpus: 2, model: "absent" }],
  };
  stub("suggestUnit", async (id: number, units: unknown) => { asked.push({ id, units }); return answer; });
  const scales: unknown[] = [];
  stub("scaleReplicas", async (_id: number, body: unknown) => { scales.push(body); return multiNodeDeployment({ replicas: 2 }); });
  await openDetail();
  fireEvent.click(screen.getByRole("button", { name: locale.replica.editUnits }));
  const modal = (await screen.findByText(locale.replica.unitsTitle)).closest(".ant-modal") as HTMLElement;
  fireEvent.click(within(modal).getByRole("button", { name: locale.replica.suggestUnit }));
  await within(modal).findByText(i18n.t("replica.suggest.added", { network: "ib", arch: "nvidia/ampere", nodes: "Node C, Node D" }));
  // The rows listed so far go with the request, so their leaders and cards count.
  assert.deepEqual(asked, [{ id: 1, units: [{ node_uuid: "node-a", followers: [{ node_uuid: "node-b" }] }] }]);
  fireEvent.click(within(modal).getByText(locale.replica.save).closest("button")!);
  await waitFor(() => assert.equal(scales.length, 1));
  assert.deepEqual(scales[0], { units: [
    { node_uuid: "node-a", followers: [{ node_uuid: "node-b" }] },
    { node_uuid: "node-c", followers: [{ node_uuid: "node-d" }] },
  ] });

  cleanup();
  answer = { unit: null, reason: { code: "unit_no_fit", params: { node: "Node C", first_reason: { code: "no_free_gpu" } } } };
  await openDetail();
  fireEvent.click(screen.getByRole("button", { name: locale.replica.editUnits }));
  const again = (await screen.findByText(locale.replica.unitsTitle)).closest(".ant-modal") as HTMLElement;
  fireEvent.click(within(again).getByRole("button", { name: locale.replica.suggestUnit }));
  await within(again).findByText(i18n.t("replica.suggest.noFit", { node: "Node C", reason: i18n.t("deploy:schedule.no_free_gpu") }));
  assert.equal(within(again).queryByText("Replica 2"), null, "nothing is appended when nothing fits");
});


test("an offline deployment shows no ready replicas and disables inference", async () => {
  const offline = deployment({ runtime_state: "offline", ready_replicas: 0, replicas: 1 });
  stub("deploymentsPage", async () => listPage([offline]));
  stub("deployment", async () => offline);
  await openDetail();
  assert.ok(screen.getAllByText(i18n.t("common:states.offline")).length > 0);
  assert.ok(screen.getAllByText("0/1").length > 0);
  fireEvent.click(within(drawer()).getByRole("button", { name: i18n.t("common:actions.close") }));
  const playground = await screen.findAllByRole("button", { name: locale.page.openPlayground });
  assert.ok(playground.every((button) => (button as HTMLButtonElement).disabled));
});

test("interleaved image layers update one row per phase and one overall download counter", async () => {
  const transfer = { state: "pulling", phase: "image_pull", progress: 0.75, bytes_done: 3072, bytes_total: 4096, rate_bps: 1024, message: "live layer output" };
  const dep = deployment({ replicas: 1, ready_replicas: 0, runtime_state: "pulling", instances: [instance("current", transfer)] });
  stub("deploymentsPage", async () => listPage([dep]));
  stub("deployment", async () => dep);
  stub("instanceEvents", async () => cursorPage([
    { id: 4, code: "INSTANCE_STATUS_UPDATED", params: { phase: "image_pull", detail: "latest layer", prev_phase: "image_extract", prev_bytes_done: 2048, prev_rate_bps: 1024 } },
    { id: 3, code: "INSTANCE_STATUS_UPDATED", params: { phase: "image_extract", detail: "extracting layer", prev_phase: "image_pull", prev_duration_ms: 420 } },
    { id: 2, params: { phase: "image_pull", detail: "earlier layer" } },
    { id: 1, code: "INSTANCE_CREATED" },
  ]));
  await openDetail();
  selectInstance("current");
  const timeline = within(drawer()).getByRole("list", { name: locale.startup.title });
  await within(timeline).findByText(locale.instance.phase.image_extract);
  const steps = within(timeline).getAllByRole("listitem");
  assert.deepEqual(steps.map(step => step.querySelector("strong")?.textContent), [
    locale.startup.instanceCreated, locale.instance.phase.image_pull, locale.instance.phase.image_extract,
  ]);
  assert.equal(steps[1].getAttribute("aria-current"), "step");
  assert.doesNotMatch(steps[1].textContent ?? "", /420 ms|earlier layer/);
  const startupCard = timeline.closest("details")!;
  const panel = within(startupCard);
  assert.doesNotMatch(startupCard.textContent ?? "", /layer output|latest layer|extracting layer/);
  assert.doesNotMatch(steps[2].textContent ?? "", /KiB\/s/);
  assert.ok(panel.getByText(/KiB \/ 4.0 KiB · 1.0 KiB\/s/));
  assert.equal(panel.getByRole("progressbar").getAttribute("aria-valuenow"), "75");
  const logs = within(drawer()).getByRole("region", { name: locale.instance.tab.logs });
  await within(logs).findByText(/extracting layer/);
  assert.match(logs.textContent ?? "", /latest layer/);
  assert.match(logs.textContent ?? "", /live layer output/);

  const refresh = (update: Partial<typeof transfer>) => {
    dep.instances = [instance("current", { ...transfer, ...update })];
    for (const listener of Events.listeners.get("instance") ?? []) listener(new dom.window.Event("instance"));
  };
  refresh({ phase: "image_extract" });
  await waitFor(() => assert.equal(steps[2].getAttribute("aria-current"), "step"));
  assert.doesNotMatch(startupCard.textContent ?? "", /KiB\/s/);
  refresh({ bytes_done: 4096, progress: 1 });
  await panel.findByText("4.0 KiB / 4.0 KiB");
  assert.doesNotMatch(startupCard.textContent ?? "", /KiB\/s/);
});

test("original transfer details and reported startup stages are separate from service status across refreshes", async () => {
  let dep = deployment({ runtime_state: "pending", instances: [], model_progress: [
    { node_uuid: "node", state: "downloading", preparation: { phase: "downloading" } },
  ] });
  stub("deploymentsPage", async () => listPage([dep]));
  stub("deployment", async () => dep);
  stub("instanceEvents", async () => cursorPage([
    { id: 2, params: { phase: "image_verify", detail: "image verified on node" } },
    { id: 1, params: { phase: "image_verify", detail: "checking image" } },
  ]));
  await openDetail();
  const overview = within(drawer()).getByRole("heading", { name: locale.detail.serviceOverview }).closest("section")!;
  const preparation = within(drawer()).getByRole("heading", { name: locale.detail.modelPreparation }).closest("section")!;
  assert.ok(preparation.querySelector('svg[data-status="active"]'));
  assert.ok(!overview.contains(preparation));
  const refresh = (overrides: Record<string, unknown>) => {
    dep = deployment(overrides);
    for (const listener of Events.listeners.get("instance") ?? []) listener(new dom.window.Event("instance"));
  };
  const pulling = { replicas: 1, ready_replicas: 0, runtime_state: "pulling", model_progress: [
    { node_uuid: "node-current", model_name: "active-node-model", state: "present" },
    { node_uuid: "node-old", model_name: "old-node-model", state: "registered" },
  ], instances: [
    instance("old", { state: "failed" }), instance("current", { node_uuid: "node-current", state: "pulling", phase: "image_pull_retry" }),
  ] };
  refresh(pulling);
  await screen.findByRole("tab", { name: "Instance details (2)" });
  selectInstance("current");
  const timeline = await within(drawer()).findByRole("list", { name: locale.startup.title });
  assert.ok(!overview.contains(timeline));
  assert.ok(within(timeline.closest("details")!).getByText("model"));
  assert.equal(within(timeline.closest("details")!).queryByText("old-node-model"), null);
  assert.ok(within(timeline).getByText("image verified on node"));
  assert.equal(within(timeline).queryByText("checking image"), null);
  await waitFor(() => assert.deepEqual(within(timeline).getAllByRole("listitem").map(row => row.dataset.status), ["done", "done", "active"]));
  assert.ok(within(timeline).getByText(new RegExp(locale.instance.phase.image_pull_retry)));
  assert.equal(within(timeline.closest("details")!).queryByRole("button"), null);
  refresh({ ...pulling, runtime_state: "starting", instances: [instance("current", { state: "starting", phase: "health_check", message: "waiting for the readiness probe" })] });
  await within(timeline).findByText(new RegExp(locale.instance.phase.health_check));
  assert.ok(within(timeline).getByText("waiting for the readiness probe"));
  refresh({ replicas: 1, ready_replicas: 1, runtime_state: "running", instances: [instance("current")] });
  await waitFor(() => assert.equal(within(drawer()).queryAllByRole("list", { name: locale.startup.title }).length, 0));
});
