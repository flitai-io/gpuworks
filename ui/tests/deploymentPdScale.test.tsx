import assert from "node:assert/strict";
import { fakeFleet } from "./helpers/fakeFleet.ts";
import { listPage, cursorPage, list } from "./helpers/listPage.ts";
import fs from "node:fs";
import { after, afterEach, test } from "node:test";
import { trackMessageChannels } from "./helpers/trackedMessageChannels.ts";
import { engineProfiles } from "./helpers/engineFixture.ts";
import { JSDOM } from "jsdom";
import React from "react";
import { createInstance } from "i18next";
import { I18nextProvider, initReactI18next } from "react-i18next";

// The deployment detail drawer's PD controls: the "Scale roles" modal (PUT
// .../pd - prefilled from the current units, edits the unit list, keyboard
// add/remove, a server error shown inline, success refreshing the drawer).
// Restart is the drawer header's, for every shape
// (deploymentLifecycle.test.tsx). deploymentPdDetail.test.tsx covers the
// summary/instances the drawer renders; this file covers the scale action on
// top of it.

const closeChannels = trackMessageChannels();
const dom = new JSDOM("<!doctype html><html><body></body></html>", { url: "http://localhost/" });
for (const name of ["window", "document", "navigator", "HTMLElement", "SVGElement", "Element", "Node", "MutationObserver", "ShadowRoot", "File", "Blob", "HTMLBodyElement", "HTMLHtmlElement"] as const) {
  Object.defineProperty(globalThis, name, { value: dom.window[name], configurable: true });
}
Object.defineProperty(globalThis, "localStorage", { value: dom.window.localStorage, configurable: true });
Object.defineProperty(globalThis, "getComputedStyle", { value: (element: Element) => dom.window.getComputedStyle(element), configurable: true });
Object.defineProperty(dom.window, "matchMedia", { configurable: true, value: () => ({ matches: false, addListener() {}, removeListener() {}, addEventListener() {}, removeEventListener() {}, dispatchEvent: () => false }) });
class ResizeObserverStub { observe() {} unobserve() {} disconnect() {} }
Object.defineProperty(globalThis, "ResizeObserver", { value: ResizeObserverStub, configurable: true });
Object.defineProperties(dom.window.HTMLElement.prototype, {
  attachEvent: { configurable: true, value() {} },
  detachEvent: { configurable: true, value() {} },
});

const deployLocale = JSON.parse(fs.readFileSync(new URL("../src/i18n/locales/en/deploy.json", import.meta.url), "utf8"));
const i18n = createInstance();
await i18n.use(initReactI18next).init({
  lng: "en",
  fallbackLng: "en",
  resources: { en: {
    deployments: JSON.parse(fs.readFileSync(new URL("../src/i18n/locales/en/deployments.json", import.meta.url), "utf8")),
    common: JSON.parse(fs.readFileSync(new URL("../src/i18n/locales/en/common.json", import.meta.url), "utf8")),
    deploy: deployLocale,
  } },
  defaultNS: "deployments",
  interpolation: { escapeValue: false },
});
const deploymentsLocale = i18n.getResourceBundle("en", "deployments");

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

const { api } = await import("../src/api.ts");
const { default: DeploymentsPage } = await import("../src/pages/DeploymentsPage.tsx");

const originals: Record<string, unknown> = {};
const apiAny = api as unknown as Record<string, unknown>;
function stub(name: string, value: unknown) {
  originals[name] = apiAny[name];
  apiAny[name] = value;
}

// stubFleet answers the node endpoints as the server does (one searched page,
// single nodes, facets) and refuses listing every node.
function stubFleet(list: unknown[]) {
  const fake = fakeFleet(list as never);
  for (const name of ["nodes", "nodesPage", "node", "nodeFacets"] as const) stub(name, fake[name]);
  return fake;
}

const gpu = (index: number) => ({ index, name: "H100", vendor: "nvidia", schedulable: true, arch_id: "Hopper", compute_capability: "9.0" });
const nodeDecode = { uuid: "node-decode", name: "Decode Node", hostname: "decode-host", state: "online", gpu_count: 2, gpu_vendor: "nvidia", gpus: [gpu(0), gpu(1)] };
const nodePrefill = { uuid: "node-prefill", name: "Prefill Node", hostname: "prefill-host", state: "online", gpu_count: 2, gpu_vendor: "nvidia", gpus: [gpu(0), gpu(1)] };
const nodeExtra = { uuid: "node-extra", name: "Extra Node", hostname: "extra-host", state: "online", gpu_count: 1, gpu_vendor: "nvidia", gpus: [gpu(0)] };

function resetStubs() {
  stub("routingPolicies", async () => list([]));
  stub("instanceEvents", async () => cursorPage([]));
  stub("metricsQuery", async () => list([]));
  stubFleet([nodeDecode, nodePrefill, nodeExtra]);
  stub("monitorDeployment", async () => ({
    as_of: 0, stale: false,
    deployment: { id: 1, name: "dep", engine: "vllm", placement: "", replicas: { ready: 1, desired: 1 } },
    alignment: null, ttft_breakdown: null,
    charts: { latency: [], tpot: [], concurrency: [], kv: [], throughput: [], stage_time: [], preemptions: [] },
    replicas: [],
  }));
  stub("deploymentEvents", async () => cursorPage([]));
  stub("engines", async () => list(engineProfiles));
}
resetStubs();
afterEach(() => { cleanup(); Events.listeners.clear(); resetStubs(); });
after(() => { closeChannels(); dom.window.close(); });

function pdDeployment(overrides: {
  decodeUnits?: Array<{ node_uuid: string; gpus?: number[] }>;
  prefillUnits?: Array<{ node_uuid: string; gpus?: number[] }>;
  decodeReady?: number;
} = {}) {
  return {
    id: 1, name: "pd-deployment", model_name: "model", model_id: 1, served_name: "model",
    engine: "vllm", node_uuid: "node-decode", desired_state: "active", runtime_state: "running",
    sleepable: false, health: { status: "off", threshold_sec: 60, last: null, recent_mean_ms: null, recent_ok: 0, recent: [] }, replicas: 1, template_deviated: false, created_at: "2026-09-28T00:00:00Z",
    operations: [],
    instances: [],
    pd: {
      mode: "vllm-nixl", fallback: "decode", kv_load_failure: "fail",
      state: "running", short: false,
      roles: [
        { role: "decode", tp: 2, desired: 1, ready: overrides.decodeReady ?? 1, units: overrides.decodeUnits ?? [{ node_uuid: "node-decode", gpus: [0, 1] }] },
        { role: "prefill", tp: 1, desired: 1, ready: 1, units: overrides.prefillUnits ?? [{ node_uuid: "node-prefill", gpus: [0] }] },
      ],
    },
  };
}

async function openDetail() {
  render(
    <I18nextProvider i18n={i18n}>
      <MemoryRouter initialEntries={["/deployments?deployment=1"]}>
        <DeploymentsPage />
      </MemoryRouter>
    </I18nextProvider>,
  );
  await screen.findAllByText("pd-deployment");
}

function modalByTitle(title: string): HTMLElement {
  const el = Array.from(document.querySelectorAll(".ant-modal-title")).find((node) => node.textContent === title);
  assert.ok(el, `modal titled "${title}" rendered`);
  return el.closest(".ant-modal") as HTMLElement;
}

function modalButton(modal: HTMLElement, name: string): HTMLElement {
  return within(modal).getByRole("button", { name });
}

// selectOption opens a select and picks an option, waiting for it: a node
// picker's options arrive from a server search after it opens.
async function selectOption(combobox: HTMLElement, label: string) {
  const selector = combobox.closest(".ant-select")!.querySelector(".ant-select-selector") as HTMLElement;
  fireEvent.mouseDown(selector);
  let option: Element | undefined;
  await waitFor(() => {
    const dropdowns = document.querySelectorAll(".ant-select-dropdown");
    const dropdown = dropdowns[dropdowns.length - 1];
    if (!dropdown) throw new Error("no dropdown opened");
    const options = [...dropdown.querySelectorAll(".ant-select-item-option")];
    option = options.find((el) => el.textContent === label);
    if (!option) throw new Error(`option not found: ${label} (saw ${options.map((el) => el.textContent)})`);
  });
  fireEvent.click(option!);
}

async function openScaleModal() {
  const button = await screen.findByRole("button", { name: deploymentsLocale.detail.pd.scale.action });
  fireEvent.click(button);
  await screen.findByText(deploymentsLocale.detail.pd.scale.title);
  return modalByTitle(deploymentsLocale.detail.pd.scale.title);
}

test("Scale roles opens prefilled from the current units, and Save with no edits resends them unchanged", async () => {
  stub("deploymentsPage", async () => listPage([pdDeployment()]));
  stub("deployment", async () => pdDeployment());
  const calls: unknown[] = [];
  stub("updateDeploymentPd", async (_id: number, body: unknown) => {
    calls.push(body);
    return pdDeployment({ decodeReady: 1 });
  });
  await openDetail();

  const modal = await openScaleModal();
  fireEvent.click(modalButton(modal, deploymentsLocale.detail.pd.scale.save));

  await waitFor(() => assert.equal(calls.length, 1));
  assert.deepEqual(calls[0], {
    roles: {
      decode: { units: [{ node_uuid: "node-decode", gpus: [0, 1] }] },
      prefill: { units: [{ node_uuid: "node-prefill", gpus: [0] }] },
    },
    fallback: "decode",
  });
});

test("Save's success message shows and the drawer refetches the deployment", async () => {
  // Whether antd's own close/leave transition ever finishes is not this
  // product's concern (jsdom fires no transitionend event, so an antd Modal
  // never visibly leaves the DOM under this test runner regardless of the
  // component's own onClose/open wiring - not something any test in this
  // codebase relies on). What the component owns and this asserts: the
  // success toast, and that a save re-fetches the deployment.
  stub("deploymentsPage", async () => listPage([pdDeployment()]));
  let deploymentCalls = 0;
  stub("deployment", async () => {
    deploymentCalls += 1;
    return pdDeployment({ decodeReady: 1 });
  });
  stub("updateDeploymentPd", async () => pdDeployment({ decodeReady: 2 }));
  await openDetail();
  const callsBeforeSave = deploymentCalls;

  const modal = await openScaleModal();
  fireEvent.click(modalButton(modal, deploymentsLocale.detail.pd.scale.save));

  await screen.findByText(deploymentsLocale.detail.pd.scale.saved);
  await waitFor(() => assert.ok(deploymentCalls > callsBeforeSave));
});

test("a server error on Save is shown inline and the modal stays open with nothing refreshed", async () => {
  stub("deploymentsPage", async () => listPage([pdDeployment()]));
  let deploymentCalls = 0;
  stub("deployment", async () => {
    deploymentCalls += 1;
    return pdDeployment({ decodeReady: 1 });
  });
  stub("updateDeploymentPd", async () => {
    throw new Error("PD_ROLE_UNITS_EMPTY");
  });
  await openDetail();

  const modal = await openScaleModal();
  const callsBefore = deploymentCalls;
  fireEvent.click(modalButton(modal, deploymentsLocale.detail.pd.scale.save));

  await within(modal).findByText(
    deploymentsLocale.detail.pd.scale.failed.replace("{{detail}}", "PD_ROLE_UNITS_EMPTY"),
  );
  assert.ok(screen.getByText(deploymentsLocale.detail.pd.scale.title), "the modal is still open");
  assert.equal(deploymentCalls, callsBefore, "no refresh happened after a failed save");
});

test("adding a decode instance, then removing it by keyboard, keeps a floor of one decode unit", async () => {
  stub("deploymentsPage", async () => listPage([pdDeployment()]));
  stub("deployment", async () => pdDeployment());
  const calls: Array<{ roles: { decode: { units: Array<{ node_uuid: string }> } } }> = [];
  stub("updateDeploymentPd", async (_id: number, body: typeof calls[number]) => {
    calls.push(body);
    return pdDeployment();
  });
  await openDetail();
  const modal = await openScaleModal();

  const decodeLabel = deployLocale.pd.role.decode;
  const addDecodeText = deploymentsLocale.detail.pd.scale.addUnit.replace("{{role}}", decodeLabel);
  const addDecode = within(modal).getByRole("button", { name: addDecodeText });

  // Floor of one: the sole decode row's remove button starts disabled.
  const soleRemove = within(modal).getByRole("button", {
    name: deployLocale.pd.removeUnit.replace("{{role}}", decodeLabel).replace("{{index}}", "1"),
  });
  assert.ok(soleRemove.hasAttribute("disabled"));

  fireEvent.click(addDecode);
  const newDecodeNode = within(modal).getByRole("combobox", {
    name: new RegExp(`^${decodeLabel} instance 2 node$`),
  });
  await selectOption(newDecodeNode, "Extra Node · 1 GPU");

  const removeSecond = within(modal).getByRole("button", {
    name: deployLocale.pd.removeUnit.replace("{{role}}", decodeLabel).replace("{{index}}", "2"),
  });
  removeSecond.focus();
  assert.equal(document.activeElement, removeSecond);
  await userEvent.setup({ document: dom.window.document }).keyboard("{Enter}");
  await waitFor(() =>
    assert.equal(
      within(modal).queryByRole("button", {
        name: deployLocale.pd.removeUnit.replace("{{role}}", decodeLabel).replace("{{index}}", "2"),
      }),
      null,
    ),
  );

  fireEvent.click(modalButton(modal, deploymentsLocale.detail.pd.scale.save));
  await waitFor(() => assert.equal(calls.length, 1));
  assert.deepEqual(calls[0].roles.decode.units, [{ node_uuid: "node-decode", gpus: [0, 1] }]);
});

test("the mode's catalog entry lets prefill drop its last unit and offers the mode's fallbacks", async () => {
  stub("deploymentsPage", async () => listPage([pdDeployment()]));
  stub("deployment", async () => pdDeployment());
  const calls: Array<{ roles: Record<string, { units: unknown[] }>; fallback?: string }> = [];
  stub("updateDeploymentPd", async (_id: number, body: typeof calls[number]) => {
    calls.push(body);
    return pdDeployment();
  });
  await openDetail();
  const modal = await openScaleModal();

  // vllm-nixl may scale prefill to zero (scalable_to_zero): its sole row is
  // removable once the catalog has loaded.
  const prefillLabel = deployLocale.pd.role.prefill;
  const removePrefill = () =>
    within(modal).getByRole("button", {
      name: deployLocale.pd.removeUnit.replace("{{role}}", prefillLabel).replace("{{index}}", "1"),
    });
  await waitFor(() => assert.equal(removePrefill().hasAttribute("disabled"), false));
  fireEvent.click(removePrefill());

  await selectOption(within(modal).getByRole("combobox", { name: deploymentsLocale.detail.pd.scale.fallback }), deployLocale.pd.fallbackOption.fail);
  fireEvent.click(modalButton(modal, deploymentsLocale.detail.pd.scale.save));
  await waitFor(() => assert.equal(calls.length, 1));
  assert.deepEqual(calls[0].roles.prefill.units, []);
  assert.equal(calls[0].fallback, "fail");
});

test("adding a node to a unit turns it multi-node: the TP-required hint shows and the follower travels in the PUT body", async () => {
  stub("deploymentsPage", async () => listPage([pdDeployment()]));
  stub("deployment", async () => pdDeployment());
  const calls: Array<{ roles: { decode: { units: Array<{ node_uuid: string; followers?: Array<{ node_uuid: string }> }> } } }> = [];
  stub("updateDeploymentPd", async (_id: number, body: typeof calls[number]) => {
    calls.push(body);
    return pdDeployment();
  });
  await openDetail();
  const modal = await openScaleModal();

  const decodeLabel = deployLocale.pd.role.decode;
  assert.equal(within(modal).queryByText(deployLocale.pd.multiNodeTpHint.replace("{{role}}", decodeLabel)), null);

  const addNode = within(modal).getByRole("button", {
    name: new RegExp(deployLocale.pd.addNode.replace("{{role}}", decodeLabel).replace("{{index}}", "1") + "$"),
  });
  fireEvent.click(addNode);

  await within(modal).findByText(deployLocale.pd.multiNodeTpHint.replace("{{role}}", decodeLabel));
  const followerNode = within(modal).getByRole("combobox", {
    name: new RegExp(`^${decodeLabel} instance 1 node 1$`),
  });
  await selectOption(followerNode, "Extra Node · 1 GPU");

  fireEvent.click(modalButton(modal, deploymentsLocale.detail.pd.scale.save));
  await waitFor(() => assert.equal(calls.length, 1));
  assert.deepEqual(calls[0].roles.decode.units, [
    { node_uuid: "node-decode", gpus: [0, 1], followers: [{ node_uuid: "node-extra" }] },
  ]);
});

test("the PD panel offers Scale roles but no restart of its own", async () => {
  stub("deploymentsPage", async () => listPage([pdDeployment()]));
  stub("deployment", async () => pdDeployment());
  await openDetail();
  assert.ok(await screen.findByRole("button", { name: deploymentsLocale.detail.pd.scale.action }));
  // The drawer header's Restart is the only one.
  assert.equal(screen.getAllByRole("button", { name: deploymentsLocale.detail.restart.action }).length, 1);
});
