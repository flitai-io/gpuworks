import { installCanvas } from "./helpers/analyticsFixtures.ts";
import assert from "node:assert/strict";
import { listPage, cursorPage, list } from "./helpers/listPage.ts";
import fs from "node:fs";
import { after, afterEach, test } from "node:test";
import { trackMessageChannels } from "./helpers/trackedMessageChannels.ts";
import { JSDOM } from "jsdom";
import React from "react";
import { createInstance } from "i18next";
import { I18nextProvider, initReactI18next } from "react-i18next";

// The deployment detail drawer for a PD deployment: mode/state/short summary,
// per-role TP/ready/desired, instances grouped by role with a role badge, and
// - the regression check - an ordinary (non-PD) deployment rendering exactly
// as it did before (no PD summary, no role grouping).

const closeChannels = trackMessageChannels();
const dom = new JSDOM("<!doctype html><html><body></body></html>", { url: "http://localhost/" });
for (const name of ["window", "document", "navigator", "HTMLElement", "SVGElement", "Element", "Node", "NodeFilter", "MutationObserver", "ShadowRoot", "File", "Blob"] as const) {
  Object.defineProperty(globalThis, name, { value: dom.window[name], configurable: true });
}
Object.defineProperty(globalThis, "localStorage", { value: dom.window.localStorage, configurable: true });
Object.defineProperty(globalThis, "getComputedStyle", { value: (element: Element) => dom.window.getComputedStyle(element), configurable: true });
Object.defineProperty(dom.window, "matchMedia", { configurable: true, value: () => ({ matches: false, addListener() {}, removeListener() {}, addEventListener() {}, removeEventListener() {}, dispatchEvent: () => false }) });
installCanvas(dom);
class ResizeObserverStub { observe() {} unobserve() {} disconnect() {} }
Object.defineProperty(globalThis, "ResizeObserver", { value: ResizeObserverStub, configurable: true });
Object.defineProperties(dom.window.HTMLElement.prototype, {
  attachEvent: { configurable: true, value() {} },
  detachEvent: { configurable: true, value() {} },
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
const deploymentsLocale = i18n.getResourceBundle("en", "deployments");

const { MemoryRouter } = await import("react-router-dom");
const { cleanup, fireEvent, render, screen, waitFor, within } = await import("@testing-library/react");

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

const originals: Record<string, unknown> = {};
const apiAny = api as unknown as Record<string, unknown>;
function stub(name: string, value: unknown) {
  originals[name] = apiAny[name];
  apiAny[name] = value;
}
function resetStubs() {
  stub("routingPolicies", async () => list([]));
  stub("instanceEvents", async () => cursorPage([]));
  stub("metricsQuery", async () => list([]));
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
afterEach(() => { cleanup(); Events.listeners.clear(); resetStubs(); });
after(() => { closeChannels(); dom.window.close(); });

function pdInstance(uuid: string, role: "decode" | "prefill", nodeUuid: string): unknown {
  return {
    uuid, deployment_id: 1, node_uuid: nodeUuid, state: "running",
    gpu_indexes: [0], ports: [30000], api_port: 30000, image: "runner:vllm",
    message: "", container_id: `container-${uuid}`, sleep_level: 0, role,
  };
}

const pdDeploymentBase = {
  id: 1, name: "pd-deployment", model_name: "model", model_id: 1, served_name: "model",
  engine: "vllm", node_uuid: "node-decode", desired_state: "active", runtime_state: "running",
  sleepable: false, health: { status: "off", threshold_sec: 60, last: null, recent_mean_ms: null, recent_ok: 0, recent: [] }, replicas: 1, template_deviated: false, created_at: "2026-09-28T00:00:00Z",
  operations: [],
};

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

test("a running PD deployment shows its mode, state, role ratios and the role of the selected instance", async () => {
  stub("deploymentsPage", async () => listPage([pdDeploymentBase]));
  stub("deployment", async () => ({
    ...pdDeploymentBase,
    instances: [pdInstance("inst-prefill", "prefill", "node-prefill"), pdInstance("inst-decode", "decode", "node-decode")],
    pd: {
      mode: "vllm-nixl", fallback: "decode", kv_load_failure: "fail",
      state: "running", short: false,
      roles: [
        { role: "decode", tp: 2, desired: 1, ready: 1, units: [{ node_uuid: "node-decode", gpus: [0, 1] }] },
        { role: "prefill", tp: 1, desired: 1, ready: 1, units: [{ node_uuid: "node-prefill", gpus: [0] }] },
      ],
    },
  }));

  await openDetail();

  await waitFor(() => assert.ok(screen.getByText("Mode: vllm-nixl")));
  // "Running" also names the ordinary deployment state badge, so the PD
  // state badge is a second occurrence, not the only one.
  assert.ok(screen.getAllByText("Running").length >= 2, "the derived PD state renders");
  assert.equal(screen.queryByText(deploymentsLocale.detail.pd.short), null, "not short: no short notice");
  assert.ok(screen.getByText("Decode · TP 2 · 1/1 ready"));
  assert.ok(screen.getByText("Prefill · TP 1 · 1/1 ready"));

  selectInstance("inst-decode");
  assert.ok(document.getElementById("instance-inst-decode"));
  assert.ok(screen.getByText("TP 2 · 1/1"));
  assert.equal(document.getElementById("instance-inst-prefill"), null);
  selectInstance("inst-prefill");
  assert.ok(document.getElementById("instance-inst-prefill"));
  assert.ok(screen.getByText("TP 1 · 1/1"));
  assert.equal(document.getElementById("instance-inst-decode"), null);
});

test("a partial PD deployment with a short role shows the short notice", async () => {
  stub("deploymentsPage", async () => listPage([pdDeploymentBase]));
  stub("deployment", async () => ({
    ...pdDeploymentBase,
    instances: [pdInstance("inst-decode", "decode", "node-decode")],
    pd: {
      mode: "vllm-nixl", fallback: "decode", kv_load_failure: "fail",
      state: "partial", short: true,
      roles: [
        { role: "decode", tp: 1, desired: 1, ready: 1, units: [{ node_uuid: "node-decode" }] },
        { role: "prefill", tp: 1, desired: 2, ready: 0, units: [] },
      ],
    },
  }));

  await openDetail();

  await waitFor(() => assert.ok(screen.getByText("Partially ready")));
  assert.ok(screen.getByText(deploymentsLocale.detail.pd.short));
  assert.ok(screen.getByText("Prefill · TP 1 · 0/2 ready"));
  selectInstance("inst-decode");
  // No prefill instance exists yet, so only the decode instance renders.
  assert.ok(within(screen.getByRole("group", { name: deploymentsLocale.detail.switchInstance })).getByText("inst-decode"));
});

test("a stopped PD deployment retains its role targets without reporting a running capacity shortage", async () => {
  const stopped = {
    ...pdDeploymentBase, desired_state: "stopped", runtime_state: "stopped", instances: [],
    pd: {
      mode: "vllm-nixl", state: "not_ready", short: true,
      roles: ["decode", "prefill"].map((role) => ({ role, tp: 1, desired: 1, ready: 0, units: [] })),
    },
  };
  stub("deploymentsPage", async () => listPage([stopped]));
  stub("deployment", async () => stopped);
  await openDetail();
  const pd = (await screen.findByText(deploymentsLocale.detail.pd.title)).closest("section")!;
  const overview = screen.getByRole("heading", { name: deploymentsLocale.detail.serviceOverview }).closest("section")!;
  assert.ok(!overview.contains(pd), "role targets are managed outside the service overview");
  assert.ok(within(pd).getByText(i18n.t("detail.pd.readyUnits", { ready: 0, desired: 2 })));
  assert.ok(within(pd).getByText("Decode · TP 1 · 0/1 ready"));
  assert.ok(within(pd).getByText("Prefill · TP 1 · 0/1 ready"));
  assert.ok(within(pd).getByRole("button", { name: deploymentsLocale.detail.pd.scale.action }));
  assert.equal(screen.queryByText(deploymentsLocale.detail.pd.short), null);
  assert.equal(screen.queryByRole("spinbutton", { name: deploymentsLocale.detail.replicas }), null);
});

test("an ordinary deployment renders exactly as before: no PD summary, no role grouping", async () => {
  const ordinary = {
    id: 2, name: "ordinary-deployment", model_name: "model", model_id: 1, served_name: "model",
    engine: "vllm", node_uuid: "node-1", desired_state: "active", runtime_state: "running",
    sleepable: false, health: { status: "off", threshold_sec: 60, last: null, recent_mean_ms: null, recent_ok: 0, recent: [] }, replicas: 1, template_deviated: false, created_at: "2026-09-28T00:00:00Z",
    operations: [],
  };
  stub("deploymentsPage", async () => listPage([ordinary]));
  stub("deployment", async () => ({
    ...ordinary,
    instances: [{
      uuid: "inst-plain", deployment_id: 2, node_uuid: "node-1", state: "running",
      gpu_indexes: [0], ports: [30000], api_port: 30000, image: "runner:vllm",
      message: "", container_id: "container-plain", sleep_level: 0,
    }],
  }));

  render(
    <I18nextProvider i18n={i18n}>
      <MemoryRouter initialEntries={["/deployments?deployment=2"]}>
        <DeploymentsPage />
      </MemoryRouter>
    </I18nextProvider>,
  );
  await screen.findAllByText("ordinary-deployment");
  selectInstance("inst-plain");
  const records = await screen.findByRole("group", { name: deploymentsLocale.detail.switchInstance });
  assert.ok(within(records).getByText("inst-plain"));
  assert.ok(screen.getByRole("button", { name: "All 1", pressed: true }));
  assert.ok(within(records).getByRole("combobox"));

  assert.equal(screen.queryByText(deploymentsLocale.detail.pd.title), null, "no PD section for an ordinary deployment");
  assert.equal(screen.queryByText("Decode"), null);
  assert.equal(screen.queryByText("Prefill"), null);
});

function exhaustedDeployment(exhausted: boolean): unknown {
  return {
    ...pdDeploymentBase,
    instances: [pdInstance("inst-decode", "decode", "node-decode")],
    pd: {
      mode: "vllm-nixl", fallback: "decode", kv_load_failure: "fail",
      state: "partial", short: true,
      roles: [
        { role: "decode", tp: 1, desired: 1, ready: 1, units: [{ node_uuid: "node-decode" }] },
        { role: "prefill", tp: 1, desired: 1, ready: 0, units: [{
          node_uuid: "node-prefill", node_name: "Prefill Node", gpus: [0],
          ...(exhausted ? { failed_attempts: 3, last_failure: "image pull timed out", retry_exhausted: true } : {}),
        }] },
      ],
    },
  };
}

test("a unit that gave up shows its failures and a Retry that sends that unit", async () => {
  let exhausted = true;
  stub("deploymentsPage", async () => listPage([pdDeploymentBase]));
  stub("deployment", async () => exhaustedDeployment(exhausted));
  const calls: Array<{ id: number; unit: unknown }> = [];
  stub("retryDeploymentUnit", async (id: number, unit: unknown) => {
    calls.push({ id, unit });
    exhausted = false;
    return exhaustedDeployment(false);
  });

  await openDetail();

  const text = deploymentsLocale.detail.pd.retry.exhausted
    .replace("{{role}}", "Prefill").replace("{{index}}", "1").replace("{{node}}", "Prefill Node")
    .replace("{{attempts}}", "3").replace("{{reason}}", "image pull timed out");
  await waitFor(() => assert.ok(screen.getByText(text)));
  const pd = screen.getByText(deploymentsLocale.detail.pd.title).closest("section")!;
  const retry = within(pd).getByRole("button", { name: deploymentsLocale.detail.retry.action });
  fireEvent.click(retry);
  await waitFor(() => assert.equal(calls.length, 1));
  assert.deepEqual(calls[0], { id: 1, unit: { role: "prefill", index: 0 } });
  await waitFor(() => assert.equal(screen.queryByText(text), null));
});

test("a failed retry is reported and the unit stays offered for retry", async () => {
  stub("deploymentsPage", async () => listPage([pdDeploymentBase]));
  stub("deployment", async () => exhaustedDeployment(true));
  stub("retryDeploymentUnit", async () => {
    throw new APIError("UNIT_NOT_FAILED", { role: "prefill", index: 0 }, "the unit has no failed attempts to retry", 409);
  });

  await openDetail();

  const retry = await screen.findByRole("button", { name: deploymentsLocale.detail.retry.action });
  fireEvent.click(retry);
  await waitFor(() => assert.ok(screen.getByText(deploymentsLocale.detail.retry.failed.replace("{{detail}}", i18n.t("common:apiError.UNIT_NOT_FAILED")))));
  // jsdom never ends antd's loading-icon leave motion, so the icon's label
  // stays in the button's accessible name.
  assert.ok(await screen.findByRole("button", { name: new RegExp(`${deploymentsLocale.detail.retry.action}$`) }));
});

test("a unit with failures that has not given up offers no retry", async () => {
  stub("deploymentsPage", async () => listPage([pdDeploymentBase]));
  stub("deployment", async () => exhaustedDeployment(false));
  await openDetail();
  await waitFor(() => assert.ok(screen.getByText("Prefill · TP 1 · 0/1 ready")));
  assert.equal(screen.queryByRole("button", { name: deploymentsLocale.detail.retry.action }), null);
});
