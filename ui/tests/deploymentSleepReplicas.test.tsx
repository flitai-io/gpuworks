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
import * as shapes from "./helpers/deploymentCapabilities.ts";

// Sleep and wake below the whole deployment: one replica from its instance
// panel, the drawer's all / partly / none summary with a wake of what fits
// when the cards cannot take every sleeper back, and a PD deployment's roles
// its mode runs without.

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
// verdicts: the replica it fills, whether it is asleep, and whether it takes
// a sleep or wake order of its own (none unless the case says so).
function instance(uuid: string, extra: Record<string, unknown> = {}): unknown {
  return {
    uuid, deployment_id: 1, node_uuid: `node-${uuid}`, state: "running",
    gpu_indexes: [0], ports: [30000], api_port: 30000, image: "runner:vllm",
    message: "", container_id: `container-${uuid}`, sleep_level: 0, desired_sleep: 0, dist_role: "leader", nnodes: 1, node_rank: 0,
    asleep: false, can_sleep_alone: false, can_wake_alone: false,
    ...extra,
  };
}

// alone is what the server says of a single-node replica of a sleepable
// deployment: it sleeps and wakes on its own.
const alone = { can_sleep_alone: true, can_wake_alone: true };

function deployment(overrides: Record<string, unknown> = {}) {
  return {
    id: 1, name: "lifecycle-deployment", model_name: "model", model_id: 1, served_name: "model",
    engine: "vllm", node_uuid: "node-a", desired_state: "active", runtime_state: "running",
    sleepable: false, health: { status: "off", threshold_sec: 60, last: null, recent_mean_ms: null, recent_ok: 0, recent: [] }, replicas: 2, max_replicas: 32, template_deviated: false, created_at: "2026-09-30T00:00:00Z",
    operations: [], instances: [instance("inst-a", { replica: { index: 0 } }), instance("inst-b", { replica: { index: 1 } })],
    ...shapes.notSleepableSingleNode,
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

const { APIError } = await import("../src/apiError.ts");

function sleepable(overrides: Record<string, unknown> = {}) {
  return deployment({
    sleepable: true,
    ...shapes.sleepableSingleNode,
    instances: [instance("inst-a", { replica: { index: 0 }, ...alone }), instance("inst-b", { replica: { index: 1 }, ...alone })],
    ...overrides,
  });
}

// The dialog's title reads like the drawer's own sleep button, so find the
// one inside a dialog. Its confirm button is found by its label: after a
// refused order the loading icon's leave motion, which never ends in jsdom,
// stays in the button's accessible name.
async function confirmSleepMode() {
  let confirm: HTMLElement | undefined;
  await waitFor(() => {
    const modal = screen.getAllByText(locale.sleepMode.titleQuick).map((el) => el.closest(".ant-modal") as HTMLElement | null).find((el): el is HTMLElement => !!el);
    assert.ok(modal, "sleep mode dialog open");
    confirm = within(modal).getByText(locale.sleepMode.confirm).closest("button") as HTMLElement;
  });
  fireEvent.click(confirm as HTMLElement);
}

test("a sleepable replica's panel sleeps that replica in the chosen mode", async () => {
  stub("deploymentsPage", async () => listPage([sleepable()]));
  stub("deployment", async () => sleepable());
  const orders: unknown[][] = [];
  stub("sleepInstance", async (...args: unknown[]) => { orders.push(args); });
  await openDetail();
  selectInstance("inst-b");
  const panel = document.getElementById("instance-inst-b") as HTMLElement;
  fireEvent.click(within(screen.getByRole("group", { name: locale.detailLayout.replicaControls })).getByRole("button", { name: locale.replica.sleep }));
  await confirmSleepMode();
  await waitFor(() => assert.deepEqual(orders, [["inst-b", 1, "wait", undefined]]));
  selectInstance("inst-a");
  assert.equal(within(document.getElementById("instance-inst-a") as HTMLElement).queryByRole("button", { name: locale.replica.wake }), null);
});

test("no replica sleep where the deployment cannot sleep; a multi-node replica sleeps from its leader", async () => {
  stub("deploymentsPage", async () => listPage([deployment()]));
  stub("deployment", async () => deployment());
  await openDetail();
  assert.equal(screen.queryByRole("button", { name: locale.replica.sleep }), null);
  cleanup();

  // The server lets both ranks of a multi-node replica take the replica's
  // order; the drawer offers it on the leader and tells the follower it goes
  // with it.
  const multi = sleepable({ nnodes: 2, instances: [
    instance("lead", { nnodes: 2, node_rank: 0, dist_role: "leader", dist_group: "g", dist_attempt: 1, replica: { index: 0 }, ...alone }),
    instance("follow", { nnodes: 2, node_rank: 1, headless: true, dist_role: "follower", dist_group: "g", dist_attempt: 1, replica: { index: 0 }, ...alone }),
    instance("lead-2", { nnodes: 2, node_rank: 0, dist_role: "leader", dist_group: "h", dist_attempt: 1, replica: { index: 1 }, ...alone }),
    instance("follow-2", { nnodes: 2, node_rank: 1, headless: true, dist_role: "follower", dist_group: "h", dist_attempt: 1, replica: { index: 1 }, ...alone }),
  ] });
  stub("deploymentsPage", async () => listPage([multi]));
  stub("deployment", async () => multi);
  const orders: unknown[][] = [];
  stub("sleepInstance", async (...args: unknown[]) => { orders.push(args); });
  await openDetail();
  selectInstance("follow");
  const follower = document.getElementById("instance-follow") as HTMLElement;
  assert.equal(within(follower).queryByRole("button", { name: locale.replica.sleep }), null);
  assert.ok(within(follower).getByText(locale.replica.followerHint));
  selectInstance("lead");
  fireEvent.click(within(screen.getByRole("group", { name: locale.detailLayout.replicaControls })).getByRole("button", { name: locale.replica.sleep }));
  await confirmSleepMode();
  await waitFor(() => assert.deepEqual(orders, [["lead", 1, "wait", undefined]]));
});

test("one replica asleep reads as partly asleep; its panel wakes it, the drawer wakes all", async () => {
  // inst-a reports asleep; inst-b is ordered to and has not reported yet.
  const partly = sleepable({ replicas: 3, instances: [
    instance("inst-a", { sleep_level: 1, desired_sleep: 1, asleep: true, replica: { index: 0 }, ...alone }),
    instance("inst-b", { desired_sleep: 1, asleep: true, replica: { index: 1 }, ...alone }),
    instance("inst-c", { replica: { index: 2 }, ...alone }),
  ] });
  stub("deploymentsPage", async () => listPage([partly]));
  stub("deployment", async () => partly);
  const wakes: unknown[][] = [];
  stub("wakeInstance", async (...args: unknown[]) => { wakes.push(["instance", ...args]); });
  stub("wakeDeployment", async (...args: unknown[]) => { wakes.push(["deployment", ...args]); return { status: "waking" }; });
  await openDetail();
  assert.ok(within(drawer()).getByText(i18n.t("sleep.partial", { asleep: 2, total: 3 })));
  // Sleeping the rest stays on offer while some replica is awake.
  const sleepField = within(drawer()).getByText(locale.detail.sleepable).closest("tr")! as HTMLElement;
  assert.ok(within(sleepField).getByRole("button", { name: locale.sleep.quickAction }));
  assert.ok(within(sleepField).getByRole("button", { name: locale.sleep.deepAction }));
  selectInstance("inst-a");
  assert.ok(within(document.getElementById("instance-inst-a") as HTMLElement).getByText(i18n.t("instance.sleeping", { level: 1, storage: locale.instance.memory })));
  selectInstance("inst-b");
  selectInstance("inst-b");
  const panel = document.getElementById("instance-inst-b") as HTMLElement;
  // The order counts at once: the panel says it is on its way to sleep.
  assert.ok(within(panel).getByText(i18n.t("instance.fallingAsleep", { level: 1 })));
  fireEvent.click(within(screen.getByRole("group", { name: locale.detailLayout.replicaControls })).getByRole("button", { name: locale.replica.wake }));
  await waitFor(() => assert.deepEqual(wakes, [["instance", "inst-b"]]));
  fireEvent.click(screen.getByRole("tab", { name: locale.detail.title }));
  fireEvent.click(within(drawer()).getByRole("button", { name: locale.sleep.wakeAll }));
  await waitFor(() => assert.deepEqual(wakes[1], ["deployment", 1, { partial: false }]));
});

test("each replica says its own hold, and the drawer says the mix the server read", async () => {
  // inst-a is held asleep by an operator; inst-b was put to sleep by the
  // platform, so a request wakes it.
  const mixed = sleepable({ replicas: 2, sleep_hold: "some", sleep_held: false, instances: [
    instance("inst-a", { sleep_level: 2, desired_sleep: 2, asleep: true, sleep_held: true, replica: { index: 0 }, ...alone }),
    instance("inst-b", { sleep_level: 1, desired_sleep: 1, asleep: true, sleep_held: false, replica: { index: 1 }, ...alone }),
  ] });
  stub("deploymentsPage", async () => listPage([mixed]));
  stub("deployment", async () => mixed);
  await openDetail();
  assert.ok(within(drawer()).getByText(locale.sleep.someHeldHint));
  assert.equal(within(drawer()).queryByText(locale.sleep.heldHint), null);
  selectInstance("inst-a");
  assert.ok(within(document.getElementById("instance-inst-a") as HTMLElement).getByText(locale.instance.held));
  selectInstance("inst-b");
  assert.equal(within(document.getElementById("instance-inst-b") as HTMLElement).queryByText(locale.instance.held), null);
});

test("a wake the cards cannot take all at once offers to wake what fits", async () => {
  const asleep = sleepable({ instances: [
    instance("inst-a", { sleep_level: 1, desired_sleep: 1, asleep: true, replica: { index: 0 }, ...alone }),
    instance("inst-b", { sleep_level: 1, desired_sleep: 1, asleep: true, replica: { index: 1 }, ...alone }),
  ] });
  stub("deploymentsPage", async () => listPage([asleep]));
  stub("deployment", async () => asleep);
  const wakes: unknown[] = [];
  stub("wakeDeployment", async (_id: number, opts: { partial?: boolean }) => {
    wakes.push(opts);
    if (!opts.partial) throw new APIError("WAKE_INSUFFICIENT_GPU_MEMORY", { node: "n1", gpu_indexes: [1], shortfall_gib: "21.6", occupants: ["other"] }, "GPU [1] cannot take it back", 409);
    return { status: "waking", woken: ["inst-a"], refused: [{ instances: ["inst-b"], node: "n1", gpu_indexes: [1], occupants: ["other"], shortfall_gib: "21.6" }] };
  });
  await openDetail();
  assert.ok(within(drawer()).getByText(i18n.t("sleep.sleeping", { level: 1, name: locale.sleep.quick })));
  assert.equal(within(drawer()).queryByRole("button", { name: locale.sleep.wakeWhatFits }), null);
  fireEvent.click(within(drawer()).getByRole("button", { name: locale.sleep.wake }));
  const fits = await within(drawer()).findByRole("button", { name: locale.sleep.wakeWhatFits });
  fireEvent.click(fits);
  await waitFor(() => assert.deepEqual(wakes, [{ partial: false }, { partial: true }]));
  await screen.findByText(i18n.t("sleep.partialWoken", { woken: 1, refused: 1 }));
});

test("a GPU held by a process outside the platform is named, and waking what fits stays on offer", async () => {
  const asleep = sleepable({ instances: [
    instance("inst-a", { sleep_level: 1, desired_sleep: 1, asleep: true, replica: { index: 0 }, ...alone }),
    instance("inst-b", { sleep_level: 1, desired_sleep: 1, asleep: true, replica: { index: 1 }, ...alone }),
    instance("inst-c", { sleep_level: 1, desired_sleep: 1, asleep: true, replica: { index: 2 }, ...alone }),
  ] });
  stub("deploymentsPage", async () => listPage([asleep]));
  stub("deployment", async () => asleep);
  const wakes: unknown[] = [];
  stub("wakeDeployment", async (_id: number, opts: { partial?: boolean }) => {
    wakes.push(opts);
    if (!opts.partial) throw new APIError("WAKE_GPU_HELD_OUTSIDE", { node: "n1", gpu_indexes: [1], shortfall_gib: "0.0", occupants: [] }, "GPU [1] is held outside", 409);
    return { status: "waking", woken: ["inst-a"], refused: [{ instances: ["inst-b"], node: "n1", gpu_indexes: [1], occupants: [], shortfall_gib: "0.0", held_outside: true }, { instances: ["inst-c"], node: "n1", gpu_indexes: [1], occupants: [], shortfall_gib: "0.0", held_outside: true }] };
  });
  await openDetail();
  fireEvent.click(within(drawer()).getByRole("button", { name: locale.sleep.wake }));
  const reason = i18n.t("apiError.WAKE_GPU_HELD_OUTSIDE", { ns: "common", node: "n1", gpu_indexes: [1] });
  await screen.findByText((text) => text.includes(reason));
  fireEvent.click(await within(drawer()).findByRole("button", { name: locale.sleep.wakeWhatFits }));
  await waitFor(() => assert.deepEqual(wakes, [{ partial: false }, { partial: true }]));
  await screen.findByText(i18n.t("sleep.partialWoken", { woken: 1, refused: 2 }));
});

test("waking what fits counts replicas: a multi-node replica's ranks count once", async () => {
  const rank = (uuid: string, index: number, extra: Record<string, unknown>) =>
    instance(uuid, { sleep_level: 1, desired_sleep: 1, asleep: true, replica: { index }, ...alone, ...extra });
  const asleep = sleepable({ nnodes: 2, instances: [
    rank("g1-leader", 0, { dist_group: "g1", dist_attempt: 1, dist_role: "leader", nnodes: 2 }),
    rank("g1-follower", 0, { dist_group: "g1", dist_attempt: 1, dist_role: "follower", headless: true, node_rank: 1, nnodes: 2 }),
    rank("g2-leader", 1, { dist_group: "g2", dist_attempt: 1, dist_role: "leader", nnodes: 2 }),
    rank("g2-follower", 1, { dist_group: "g2", dist_attempt: 1, dist_role: "follower", headless: true, node_rank: 1, nnodes: 2 }),
  ] });
  stub("deploymentsPage", async () => listPage([asleep]));
  stub("deployment", async () => asleep);
  stub("wakeDeployment", async (_id: number, opts: { partial?: boolean }) => {
    if (!opts.partial) throw new APIError("WAKE_INSUFFICIENT_GPU_MEMORY", { node: "n2", gpu_indexes: [1], shortfall_gib: "4.0", occupants: ["other"] }, "GPU [1] cannot take it back", 409);
    return { status: "waking", woken: ["g1-leader", "g1-follower"], refused: [{ instances: ["g2-leader", "g2-follower"], node: "n2", gpu_indexes: [1], occupants: ["other"], shortfall_gib: "4.0" }] };
  });
  await openDetail();
  fireEvent.click(within(drawer()).getByRole("button", { name: locale.sleep.wake }));
  fireEvent.click(await within(drawer()).findByRole("button", { name: locale.sleep.wakeWhatFits }));
  await screen.findByText(i18n.t("sleep.partialWoken", { woken: 1, refused: 1 }));
});

test("a PD deployment sleeps the role its mode runs without, and only that one", async () => {
  const pd = sleepable({
    replicas: 1,
    pd: { mode: "vllm-nixl", fallback: "decode", kv_load_failure: "fail", state: "running", short: false, roles: [
      { role: "decode", tp: 1, desired: 1, ready: 1, may_be_empty: false, units: [] },
      { role: "prefill", tp: 1, desired: 2, ready: 2, may_be_empty: true, units: [] },
    ] },
    // The one decode instance may wake on its own but not sleep: the mode
    // cannot run without an awake decoder.
    instances: [
      instance("decode-0", { role: "decode", replica: { role: "decode", index: 0 }, can_wake_alone: true }),
      instance("prefill-0", { role: "prefill", replica: { role: "prefill", index: 0 }, ...alone }),
      instance("prefill-1", { role: "prefill", replica: { role: "prefill", index: 1 }, ...alone }),
    ],
  });
  stub("deploymentsPage", async () => listPage([pd]));
  stub("deployment", async () => pd);
  const orders: unknown[][] = [];
  stub("sleepDeployment", async (...args: unknown[]) => { orders.push(args); });
  await openDetail();
  selectInstance("decode-0");
  assert.equal(within(document.getElementById("instance-decode-0") as HTMLElement).queryByRole("button", { name: locale.replica.sleep }), null);
  selectInstance("prefill-0");
  assert.ok(within(screen.getByRole("group", { name: locale.detailLayout.replicaControls })).getByRole("button", { name: locale.replica.sleep }));
  fireEvent.click(screen.getByRole("tab", { name: locale.detail.title }));
  const prefill = locale.detail.pd.role.prefill;
  const pdSection = within(drawer()).getByText(locale.detail.pd.title).closest("section")!;
  const sleepPrefill = await within(pdSection).findByRole("button", { name: i18n.t("sleep.role.sleep", { role: prefill }) });
  assert.equal(within(drawer()).queryByRole("button", { name: i18n.t("sleep.role.sleep", { role: locale.detail.pd.role.decode }) }), null);
  fireEvent.click(sleepPrefill);
  await confirmSleepMode();
  await waitFor(() => assert.deepEqual(orders, [[1, 1, "wait", "prefill", undefined]]));
});

// The drawer's own replica pair comes from the server, not from counting the
// instances client-side: a PD deployment's replicas are its role units, so the
// denominator is 3 while the stored replicas field is 1. The bug this pins
// printed a unit-counted numerator over the stored 1 ("Partly asleep · 2/1
// replicas"), which told the operator the deployment had more sleeping
// replicas than it had.
test("a partly asleep PD deployment shows the server's pair, not the stored count", async () => {
  // The server rewrites a PD deployment's replicas to its role units (the
  // stored field only configures an ordinary deployment and PD creation forces
  // it to 1), so the detail response this fixture stands in for carries 3.
  const pd = sleepable({
    replicas: 3,
    ready_replicas: 1,
    asleep_replicas: 2,
    state: "running",
    pd: { mode: "vllm-nixl", fallback: "decode", kv_load_failure: "fail", state: "running", short: false, roles: [
      { role: "decode", tp: 1, desired: 1, ready: 1, asleep: 0, may_be_empty: false, units: [] },
      { role: "prefill", tp: 1, desired: 2, ready: 0, asleep: 2, may_be_empty: true, units: [] },
    ] },
    // The prefill role's units are the sleepers; decode keeps serving, so the
    // deployment is still running rather than asleep.
    instances: [
      instance("decode-0", { role: "decode", replica: { role: "decode", index: 0 }, ...alone }),
      instance("prefill-0", { role: "prefill", sleep_level: 1, desired_sleep: 1, asleep: true, replica: { role: "prefill", index: 0 }, ...alone }),
      instance("prefill-1", { role: "prefill", sleep_level: 1, desired_sleep: 1, asleep: true, replica: { role: "prefill", index: 1 }, ...alone }),
    ],
  });
  stub("deploymentsPage", async () => listPage([pd]));
  stub("deployment", async () => pd);
  await openDetail();
  assert.ok(within(drawer()).getByText(i18n.t("sleep.partial", { asleep: 2, total: 3 })));
  // A denominator smaller than the numerator is the defect: never render the
  // stored 1 beside a count of 2 sleepers.
  assert.equal(within(drawer()).queryAllByText(i18n.t("sleep.partial", { asleep: 2, total: 1 })).length, 0);
  // ready_replicas is filled for a PD deployment now, so the drawer prints the
  // ready count beside the same denominator instead of hiding the row.
  assert.ok(within(drawer()).getByText(i18n.t("detail.pd.readyUnits", { ready: 1, desired: 3 })));
});

test("a refused sleep order leaves the last sleep failure shown", async () => {
  stub("deploymentsPage", async () => listPage([sleepable()]));
  stub("deployment", async () => sleepable());
  const failure = { id: 7, instance_uuid: "inst-a", kind: "sleep", code: "INSTANCE_SLEEP_RESULT", params: { result: "failed_rollback", detail: "engine refused to offload" }, message: "", created_at: "2026-09-30T00:00:00Z" };
  stub("deploymentEvents", async () => cursorPage([failure]));
  stub("sleepDeployment", async () => {
    throw new APIError("DEPLOYMENT_DELETING", {}, "the deployment is being deleted", 409);
  });
  await openDetail();
  assert.ok(await within(drawer()).findByText("engine refused to offload"));
  fireEvent.click(within(drawer()).getByRole("button", { name: locale.sleep.quickAction }));
  await confirmSleepMode();
  await screen.findByText(i18n.t("sleep.failed", { detail: i18n.t("common:apiError.DEPLOYMENT_DELETING") }));
  assert.ok(within(drawer()).getByText("engine refused to offload"));
});
