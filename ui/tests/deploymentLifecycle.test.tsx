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

test("an active deployment offers Restart and Stop, not Start; canceling the restart posts nothing", async () => {
  stub("deploymentsPage", async () => listPage([deployment()]));
  stub("deployment", async () => deployment());
  let restarted = 0;
  stub("restartDeployment", async () => { restarted += 1; return deployment(); });
  await openDetail();
  assert.equal(within(drawer()).queryByRole("button", { name: locale.detail.start }), null);
  fireEvent.click(within(drawer()).getByRole("button", { name: locale.detail.restart.action }));
  const modal = (await screen.findByText(locale.detail.restart.confirmTitle)).closest(".ant-modal") as HTMLElement;
  // Rolling is the default and says what it does.
  assert.ok((within(modal).getByRole("radio", { name: locale.detail.restart.strategyRolling }) as HTMLInputElement).checked);
  assert.ok(within(modal).getByText(locale.detail.restart.rollingDescription));
  fireEvent.click(within(modal).getByRole("button", { name: locale.detail.restart.cancel }));
  await Promise.resolve();
  assert.equal(restarted, 0);
});

test("confirming the restart posts once and refetches the drawer", async () => {
  stub("deploymentsPage", async () => listPage([deployment()]));
  let loads = 0;
  stub("deployment", async () => { loads += 1; return deployment(); });
  const calls: Array<[number, string]> = [];
  stub("restartDeployment", async (id: number, strategy: string) => { calls.push([id, strategy]); return deployment(); });
  await openDetail();
  const before = loads;
  fireEvent.click(within(drawer()).getByRole("button", { name: locale.detail.restart.action }));
  const modal = (await screen.findByText(locale.detail.restart.confirmTitle)).closest(".ant-modal") as HTMLElement;
  fireEvent.click(within(modal).getByRole("button", { name: locale.detail.restart.confirmOk }));
  await waitFor(() => assert.deepEqual(calls, [[1, "rolling"]]));
  await waitFor(() => assert.ok(loads > before, "the drawer refetches after the restart"));
});

test("a refused restart is shown inside the confirm dialog", async () => {
  stub("deploymentsPage", async () => listPage([deployment()]));
  stub("deployment", async () => deployment());
  stub("restartDeployment", async () => { throw new APIError("DEPLOYMENT_NOT_ACTIVE", { state: "stopped" }, "only an active deployment can be restarted", 409); });
  await openDetail();
  fireEvent.click(within(drawer()).getByRole("button", { name: locale.detail.restart.action }));
  const modal = (await screen.findByText(locale.detail.restart.confirmTitle)).closest(".ant-modal") as HTMLElement;
  fireEvent.click(within(modal).getByRole("button", { name: locale.detail.restart.confirmOk }));
  const refusal = i18n.t("detail.restart.failed", { detail: i18n.t("common:apiError.DEPLOYMENT_NOT_ACTIVE", { state: "stopped" }) });
  await within(modal).findByText(refusal);
  // The refusal answered that attempt: cancelled and opened again, the
  // dialog asks afresh.
  fireEvent.click(within(modal).getByText(locale.detail.cancel).closest("button") as HTMLElement);
  fireEvent.click(within(drawer()).getByRole("button", { name: locale.detail.restart.action }));
  await waitFor(() => assert.equal(screen.queryByText(refusal), null));
});

test("stopping keeps the drawer open and refetches it", async () => {
  stub("deploymentsPage", async () => listPage([deployment()]));
  let loads = 0;
  stub("deployment", async () => { loads += 1; return deployment(); });
  let stopped = 0;
  stub("stopDeployment", async () => { stopped += 1; });
  await openDetail();
  const before = loads;
  fireEvent.click(within(drawer()).getByRole("button", { name: locale.detail.stop }));
  const confirm = (await screen.findByText(locale.detail.stopConfirm)).closest(".ant-modal") as HTMLElement;
  fireEvent.click(within(confirm).getByRole("button", { name: locale.detail.stop }));
  await waitFor(() => assert.equal(stopped, 1));
  await waitFor(() => assert.ok(loads > before));
  assert.ok(drawer(), "the drawer is still open after stopping");
});

test("a stopped deployment offers only Start, which posts and refetches", async () => {
  const stoppedDep = deployment({ desired_state: "stopped", runtime_state: "stopped", instances: [] });
  stub("deploymentsPage", async () => listPage([stoppedDep]));
  let loads = 0;
  stub("deployment", async () => { loads += 1; return stoppedDep; });
  const calls: number[] = [];
  stub("startDeployment", async (id: number) => { calls.push(id); return deployment(); });
  render(
    <I18nextProvider i18n={i18n}>
      <MemoryRouter initialEntries={["/deployments?deployment=1"]}>
        <DeploymentsPage />
      </MemoryRouter>
    </I18nextProvider>,
  );
  const start = await screen.findByRole("button", { name: locale.detail.start });
  assert.equal(within(drawer()).queryByRole("button", { name: locale.detail.stop }), null);
  assert.equal(within(drawer()).queryByRole("button", { name: locale.detail.restart.action }), null);
  const before = loads;
  await userEvent.click(start);
  await waitFor(() => assert.deepEqual(calls, [1]));
  await waitFor(() => assert.ok(loads > before));
});

test("a start the server refuses reports why", async () => {
  const stoppedDep = deployment({ desired_state: "stopped", runtime_state: "stopped", instances: [] });
  stub("deploymentsPage", async () => listPage([stoppedDep]));
  stub("deployment", async () => stoppedDep);
  stub("startDeployment", async () => { throw new APIError("DEPLOYMENT_DELETING", {}, "the deployment is being deleted", 409); });
  render(
    <I18nextProvider i18n={i18n}>
      <MemoryRouter initialEntries={["/deployments?deployment=1"]}>
        <DeploymentsPage />
      </MemoryRouter>
    </I18nextProvider>,
  );
  fireEvent.click(await screen.findByRole("button", { name: locale.detail.start }));
  await screen.findByText(i18n.t("detail.startFailed", { detail: i18n.t("common:apiError.DEPLOYMENT_DELETING") }));
});

test("a deleting deployment cannot be restarted or stopped", async () => {
  const deleting = deployment({ desired_state: "deleting" });
  stub("deploymentsPage", async () => listPage([deleting]));
  stub("deployment", async () => deleting);
  const stop = await openDetail();
  assert.ok((stop as HTMLButtonElement).disabled);
  assert.ok((within(drawer()).getByRole("button", { name: locale.detail.restart.action }) as HTMLButtonElement).disabled);
});

test("an ordinary replica that gave up is named with its failure, and Retry targets that replica", async () => {
  const exhausted = deployment({ instances: [instance("inst-a", { replica: { index: 0 } })], retry_exhausted: { index: 1, failed_attempts: 3, last_failure: "engine exited" } });
  stub("deploymentsPage", async () => listPage([exhausted]));
  stub("deployment", async () => exhausted);
  const calls: Array<{ id: number; unit: unknown }> = [];
  let fail = true;
  stub("retryDeploymentUnit", async (id: number, unit: unknown) => {
    calls.push({ id, unit });
    if (fail) throw new APIError("UNIT_NOT_FAILED", { role: "", index: 1 }, "the unit has no failed attempts to retry", 409);
    return deployment();
  });
  await openDetail();
  const overview = screen.getByRole("heading", { name: locale.detail.serviceOverview }).closest("section")!;
  const status = within(overview).getByText(locale.detail.state).closest("tr")! as HTMLElement;
  const alert = within(status).getByRole("alert");
  assert.ok(within(alert).getByText("A replica stopped being rebuilt after 3 failed attempts: engine exited"));
  fireEvent.click(within(alert).getByRole("button", { name: locale.detail.retry.action }));
  await within(alert).findByText(i18n.t("detail.retry.failed", { detail: i18n.t("common:apiError.UNIT_NOT_FAILED") }));
  fail = false;
  // antd leaves the loading icon's leave animation in the button's name under
  // JSDOM, so find the second click's button by its text.
  fireEvent.click(within(alert).getByText(locale.detail.retry.action).closest("button")!);
  await waitFor(() => assert.equal(calls.length, 2));
  assert.deepEqual(calls[1], { id: 1, unit: { role: "", index: 1 } });
});

test("a deployment without a give-up shows no retry notice", async () => {
  stub("deploymentsPage", async () => listPage([deployment()]));
  stub("deployment", async () => deployment());
  await openDetail();
  assert.equal(screen.queryByText(/stopped being rebuilt/), null);
});

test("instance switching starts with the first-created instance and retains the leader and rank of each follower", async () => {
  const group = (uuid: string, rank: number, group: string, index: number) => instance(uuid, {
    nnodes: 2, node_rank: rank, headless: rank > 0, dist_role: rank > 0 ? "follower" : "leader", dist_group: group, dist_attempt: 1, replica: { index },
  });
  // Listed follower-first and interleaved; a legacy follower row carries no
  // group, and the server still names it a follower (it derives dist_role
  // from headless and the rank for rows from before it was recorded).
  const multi = deployment({ instances: [
    group("follow-a1", 1, "7-a1f3", 0), group("follow-b1", 1, "7-c9e0", 1), group("leader-a0", 0, "7-a1f3", 0), group("leader-b0", 0, "7-c9e0", 1),
    instance("legacy-f1", { nnodes: 2, node_rank: 1, headless: true, dist_role: "follower" }),
  ] });
  stub("deploymentsPage", async () => listPage([multi]));
  stub("deployment", async () => multi);
  await openDetail();
  selectInstance("legacy-f1");
  for (const [uuid, rank] of [
    ["legacy-f1", "follower · rank 1"],
    ["leader-b0", "leader · rank 0"],
    ["leader-a0", "leader · rank 0"],
    ["follow-b1", "follower · rank 1 · leader leader-b"],
    ["follow-a1", "follower · rank 1 · leader leader-a"],
  ]) {
    selectInstance(uuid);
    assert.deepEqual([...document.querySelectorAll("[id^='instance-']")].map((el) => el.id), [`instance-${uuid}`]);
    assert.ok(screen.getByText(rank));
  }
});

test("status filters count the whole deployment and follow matching records across replicas and refreshes", async () => {
  const generation = { nnodes: 2, dist_group: "multi-node", dist_attempt: 1 };
  let rows = [instance("ready", { ...generation, replica: { index: 0 } }), instance("launch", { replica: { index: 1 }, state: "starting" }),
    instance("retry", { ...generation, dist_role: "follower", node_rank: 1, state: "failed" }), instance("unassigned", { state: "failed" })];
  stub("deployment", async () => deployment({ instances: rows }));
  await openDetail();
  selectInstance("ready");
  const all = () => screen.getByRole("button", { name: "All 4" });
  const replica = () => screen.getByRole("combobox", { name: locale.detailLayout.switchReplica });
  fireEvent.click(screen.getByRole("button", { name: "Starting 1" }));
  assert.ok(document.getElementById("instance-launch"));
  assert.ok(all());
  fireEvent.click(replica());
  assert.equal(screen.getAllByRole("option").length, 1);
  fireEvent.click(screen.getByRole("option"));

  fireEvent.click(screen.getByRole("button", { name: "Failed attempt 2" }));
  assert.ok(document.getElementById("instance-retry"));
  fireEvent.click(replica());
  const options = screen.getAllByRole("option");
  assert.equal(options.length, 2);
  fireEvent.click(options[1]);
  assert.ok(document.getElementById("instance-unassigned"));
  assert.ok(screen.getByRole("button", { name: "Failed attempt 2", pressed: true }));
  fireEvent.click(all());
  assert.ok(document.getElementById("instance-unassigned"));

  fireEvent.click(screen.getByRole("button", { name: "Starting 1" }));
  rows = rows.filter((_, i) => i !== 1);
  for (const listener of Events.listeners.get("instance") ?? []) listener(new dom.window.Event("instance"));
  await screen.findByRole("button", { name: "All 3", pressed: true });
  assert.ok(document.getElementById("instance-ready"));
});

test("the cascade keeps the inspected attempt across refreshes and searches all 32 records by status and ID", async () => {
  const record = (uuid: string, state: string) => instance(uuid, {
    state, replica: state === "running" ? { index: 0 } : undefined, schedule: { unit: { key: "|[]" } },
  });
  let rows = [record("ready", "running"), record("first-failure", "failed")];
  stub("deployment", async () => deployment({ instances: rows }));
  stub("deploymentsPage", async () => listPage([deployment({ instances: rows })]));
  const refresh = () => {
    for (const listener of Events.listeners.get("instance") ?? []) listener(new dom.window.Event("instance"));
  };
  await openDetail();
  selectInstance("first-failure");
  rows = [...Array.from({ length: 30 }, (_, n) => record(`attempt-${n}`, "failed")), ...rows];
  refresh();
  const failures = await screen.findByRole("button", { name: "Failed attempt 31" });
  assert.ok(document.getElementById("instance-first-failure"));
  fireEvent.click(failures);
  const switcher = within(screen.getByRole("group", { name: locale.detail.switchInstance }));
  fireEvent.click(switcher.getByRole("combobox"));
  fireEvent.change(screen.getByPlaceholderText(locale.detail.switchInstance), { target: { value: "ATTEMPT-29" } });
  const options = within(screen.getByRole("listbox")).getAllByRole("option");
  assert.deepEqual(options.map(el => el.getAttribute("aria-label")), ["attempt-29 Failed attempt"]);
  fireEvent.click(options[0]);
  assert.ok(document.getElementById("instance-attempt-29"));
  fireEvent.click(switcher.getByRole("button", { name: locale.detail.previousInstance }));
  assert.ok(document.getElementById("instance-attempt-28"));
  fireEvent.click(switcher.getByRole("button", { name: locale.detail.nextInstance }));
  assert.ok(document.getElementById("instance-attempt-29"));
  rows = [];
  refresh();
  await within(document.getElementById("deployment-instances")!).findByText(locale.detail.noInstances);
  assert.ok(screen.getByRole("tab", { name: locale.detail.instanceDetails, exact: true }));
  assert.equal(drawer().querySelector(".replica-cascade"), null);
  assert.equal(screen.queryByRole("group", { name: locale.detail.switchInstance }), null);
});

test("the overview copies the model, public name and configured image independently of the model link and instance image", async () => {
  const image = "registry.example:5000/engines/runner:v2";
  const dep = deployment({ model_name: "org/model", served_name: "public-model", image });
  stub("deploymentsPage", async () => listPage([dep]));
  stub("deployment", async () => dep);
  await openDetail();
  const model = within(within(drawer()).getByRole("heading", { name: locale.detail.serviceOverview }).closest("section")!);
  const user = userEvent.setup();
  for (const [label, value] of [
    [locale.detail.copyModel, dep.model_name],
    [locale.detail.copyServedName, dep.served_name],
    [locale.detail.copyConfiguredImage, image],
  ]) {
    await user.click(model.getByRole("button", { name: label }));
    assert.equal(await navigator.clipboard.readText(), value);
  }
  assert.equal(model.getByRole("link", { name: dep.model_name }).getAttribute("href"), `/models?detail=${dep.model_id}`);
});

test("an unplaced deployment shows its engine and automatic image selection before any instance exists", async () => {
  const dep = deployment({ runtime_state: "pending", instances: [] });
  stub("deploymentsPage", async () => listPage([dep]));
  stub("deployment", async () => dep);
  await openDetail();
  const model = within(drawer()).getByRole("group", { name: locale.detail.modelAndEngine });
  const field = within(model).getByText(locale.detail.configuredRuntime).closest("tr")! as HTMLElement;
  assert.ok(within(field).getByText(dep.engine));
  assert.ok(within(field).getByText(locale.page.autoImage));
  assert.equal(within(field).queryByRole("button", { name: locale.detail.copyConfiguredImage }), null);
  assert.equal(within(drawer()).queryByRole("heading", { name: locale.detail.instanceLaunch }), null);
});

test("instance metrics follow selection and the remaining record after removal", async () => {
  const { installCanvas } = await import("./helpers/analyticsFixtures.ts");
  installCanvas(dom);
  let rows = [instance("third", { state: "failed" }), instance("second"), instance("first")];
  const queried: string[] = [];
  stub("metricsQuery", async (_metric: string, params: { instance?: string }) => {
    if (params.instance) queried.push(params.instance);
    return list([]);
  });
  stub("deploymentsPage", async () => listPage([deployment({ instances: rows })]));
  stub("deployment", async () => deployment({ instances: rows }));
  await openDetail();
  selectInstance("first");
  assert.ok(screen.getByRole("heading", { name: locale.instance.tab.metrics }));
  selectInstance("second");
  await waitFor(() => assert.ok(queried.includes("second")));
  rows = [rows[0], rows[2]];
  for (const listener of Events.listeners.get("instance") ?? []) listener(new dom.window.Event("instance"));
  await waitFor(() => assert.ok(document.getElementById("instance-first")));
  assert.equal(document.getElementById("instance-second"), null);
  assert.ok(screen.getByRole("heading", { name: locale.instance.tab.metrics }));
});

test("single-rank replicas carry no rank tag and keep the plain count", async () => {
  stub("deploymentsPage", async () => listPage([deployment()]));
  stub("deployment", async () => deployment());
  await openDetail();
  await screen.findByText(locale.detail.instances.replace("{{count}}", "2"));
  assert.equal(screen.queryByText(/rank \d/), null);
});

test("the monitor's instance link scrolls the drawer to that instance", async () => {
  stub("deploymentsPage", async () => listPage([deployment()]));
  stub("deployment", async () => deployment());
  await openDetail("?deployment=1&instance=inst-a");
  await waitFor(() => assert.deepEqual(scrolled, ["deployment-instances"]));
  assert.ok(document.getElementById("instance-inst-a"));
  selectInstance("inst-b");
  assert.ok(document.getElementById("instance-inst-b"));
  selectInstance("inst-a");
  assert.ok(document.getElementById("instance-inst-a"));
  assert.deepEqual(scrolled, ["deployment-instances"]);
});

test("a replica's instance offers Restart (draining by default) and Remove; restart posts drain unless unticked", async () => {
  stub("deploymentsPage", async () => listPage([deployment()]));
  stub("deployment", async () => deployment());
  const restarts: Array<{ uuid: string; drain: boolean }> = [];
  stub("restartInstance", async (uuid: string, drain: boolean) => { restarts.push({ uuid, drain }); return deployment(); });
  await openDetail();
  selectInstance("inst-b");
  const panel = screen.getByRole("group", { name: locale.detailLayout.replicaControls });
  fireEvent.click(within(panel).getByRole("button", { name: locale.replica.restart }));
  let modal = (await screen.findByText(i18n.t("replica.restartNamed", {label: i18n.t("replica.unitLabel", {index: 2})}))).closest(".ant-modal") as HTMLElement;
  const drain = within(modal).getByRole("checkbox", { name: locale.replica.drain }) as HTMLInputElement;
  assert.equal(drain.getAttribute("aria-checked"), "true", "draining is the default");
  fireEvent.click(within(modal).getByRole("button", { name: locale.replica.restart }));
  await waitFor(() => assert.deepEqual(restarts, [{ uuid: "inst-b", drain: true }]));

  fireEvent.click(within(panel).getByRole("button", { name: locale.replica.restart }));
  modal = (await screen.findAllByText(i18n.t("replica.restartNamed", {label: i18n.t("replica.unitLabel", {index: 2})}))).map((el) => el.closest(".ant-modal") as HTMLElement).find((el) => !el.closest(".ant-zoom-leave")) as HTMLElement;
  await userEvent.click(within(modal).getByRole("checkbox", { name: locale.replica.drain }));
  // The reopened dialog's OK button keeps antd's loading-icon leave
  // animation in its name under JSDOM; find it by its text.
  fireEvent.click(within(modal).getByText(locale.replica.restart).closest("button")!);
  await waitFor(() => assert.deepEqual(restarts[1], { uuid: "inst-b", drain: false }));
});

test("removing a replica scales down by one naming that instance", async () => {
  stub("deploymentsPage", async () => listPage([deployment()]));
  stub("deployment", async () => deployment());
  const scales: unknown[] = [];
  stub("scaleReplicas", async (id: number, body: unknown) => { scales.push({ id, body }); return deployment({ replicas: 1 }); });
  await openDetail();
  selectInstance("inst-a");
  const panel = screen.getByRole("group", { name: locale.detailLayout.replicaControls });
  fireEvent.click(within(panel).getByRole("button", { name: locale.replica.remove }));
  const confirm = (await screen.findByText(/^Remove replica .*\?/i)).closest(".ant-popover") as HTMLElement;
  fireEvent.click(within(confirm).getByRole("button", { name: locale.replica.remove }));
  await waitFor(() => assert.deepEqual(scales, [{ id: 1, body: { replicas: 1, remove: ["inst-a"] } }]));
});

test("a single-replica deployment, a failed attempt and a follower carry no replica buttons", async () => {
  const single = deployment({ replicas: 1, instances: [instance("only", { replica: { index: 0 } }), instance("failed-try", { state: "failed" })] });
  stub("deploymentsPage", async () => listPage([single]));
  stub("deployment", async () => single);
  await openDetail();
  assert.equal(screen.queryByRole("button", { name: locale.replica.restart }), null);
  assert.equal(screen.queryByRole("button", { name: locale.replica.remove }), null);
  cleanup();

  const multi = deployment({ replicas: 2, nnodes: 2, instances: [
    instance("lead", { nnodes: 2, node_rank: 0, dist_role: "leader", dist_group: "g", dist_attempt: 1, replica: { index: 0 } }),
    instance("follow", { nnodes: 2, node_rank: 1, headless: true, dist_role: "follower", dist_group: "g", dist_attempt: 1, replica: { index: 0 } }),
    instance("failed-lead", { state: "failed", nnodes: 2, node_rank: 0, dist_role: "leader", dist_group: "h", dist_attempt: 1 }),
  ] });
  stub("deploymentsPage", async () => listPage([multi]));
  stub("deployment", async () => multi);
  await openDetail();
  selectInstance("lead");
  const lead = screen.getByRole("group", { name: locale.detailLayout.replicaControls });
  assert.ok(within(lead).getByRole("button", { name: locale.replica.restart }));
  // Without the replica list the response carries, there is nothing to
  // remove the replica from.
  assert.equal(within(lead).queryByRole("button", { name: locale.replica.remove }), null);
  selectInstance("follow");
  const follower = document.getElementById("instance-follow") as HTMLElement;
  assert.equal(within(follower).queryByRole("button", { name: locale.replica.restart }), null);
  assert.ok(within(follower).getByText(locale.replica.followerHint));
  selectInstance("failed-lead");
  assert.equal(within(document.getElementById("instance-failed-lead") as HTMLElement).queryByRole("button", { name: locale.replica.restart }), null);
});

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
