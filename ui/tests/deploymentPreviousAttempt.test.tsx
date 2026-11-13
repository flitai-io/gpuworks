import assert from "node:assert/strict";
import fs from "node:fs";
import { after, afterEach, test } from "node:test";
import { JSDOM } from "jsdom";
import React from "react";
import { createInstance } from "i18next";
import { I18nextProvider, initReactI18next } from "react-i18next";
import { trackMessageChannels } from "./helpers/trackedMessageChannels.ts";
import { cursorPage } from "./helpers/listPage.ts";
import type { Instance } from "../src/api.ts";

// A startup timeout keeps the failed attempt and launches a replacement.
// The timeout belongs to that attempt. Once something fills the same unit,
// the failed row is history: it stays, it is not the current instance, and
// its message is not the replacement's.

const closeChannels = trackMessageChannels();
const dom = new JSDOM("<!doctype html><html><body></body></html>", { url: "http://localhost/" });
for (const name of ["window", "document", "navigator", "HTMLElement", "SVGElement", "Element", "Node", "MutationObserver", "ShadowRoot", "File", "Blob", "HTMLBodyElement"] as const) {
  Object.defineProperty(globalThis, name, { value: dom.window[name], configurable: true });
}
Object.defineProperty(globalThis, "localStorage", { value: dom.window.localStorage, configurable: true });
Object.defineProperty(globalThis, "getComputedStyle", { value: (element: Element) => dom.window.getComputedStyle(element), configurable: true });
Object.defineProperty(dom.window, "matchMedia", { configurable: true, value: () => ({ matches: false, addListener() {}, removeListener() {}, addEventListener() {}, removeEventListener() {}, dispatchEvent: () => false }) });
class ResizeObserverStub { observe() {} unobserve() {} disconnect() {} }
Object.defineProperty(globalThis, "ResizeObserver", { value: ResizeObserverStub, configurable: true });
class Events { addEventListener() {} removeEventListener() {} close() {} }
Object.defineProperty(globalThis, "EventSource", { value: Events, configurable: true });
Object.defineProperties(dom.window.HTMLElement.prototype, {
  attachEvent: { configurable: true, value() {} },
  detachEvent: { configurable: true, value() {} },
});

const read = (lng: string, ns: string) => JSON.parse(fs.readFileSync(new URL(`../src/i18n/locales/${lng}/${ns}.json`, import.meta.url), "utf8"));
const i18n = createInstance();
await i18n.use(initReactI18next).init({
  lng: "en",
  fallbackLng: "en",
  resources: { en: { deployments: read("en", "deployments"), common: read("en", "common") } },
  defaultNS: "deployments",
  interpolation: { escapeValue: false },
});

const { MemoryRouter } = await import("react-router-dom");
const { cleanup, render, within } = await import("@testing-library/react");
const { api } = await import("../src/api.ts");
const { default: DeploymentInstances } = await import("../src/pages/DeploymentInstances.tsx");

const apiAny = api as unknown as Record<string, unknown>;
apiAny.instanceEvents = async () => cursorPage([]);
afterEach(() => cleanup());
after(() => { closeChannels(); dom.window.close(); });

const timeout = "health check timed out";

function inst(uuid: string, extra: Partial<Instance> = {}): Instance {
  return {
    uuid, deployment_id: 1, node_uuid: "node-a", state: "running", gpu_indexes: [0], ports: [30000], api_port: 30000,
    image: "runner:vllm", message: "", sleep_level: 0, dist_role: "leader", nnodes: 1, node_rank: 0,
    ...extra,
  } as Instance;
}

function unit(key: string): Instance["schedule"] {
  return { unit: { key } } as Instance["schedule"];
}

function deployment(instances: Instance[], replicas = 1) {
  return {
    id: 1, name: "qwen3-8b", model_name: "model", model_id: 1, served_name: "model", engine: "vllm", node_uuid: "node-a",
    desired_state: "active", runtime_state: "running", sleepable: false, replicas, max_replicas: 32,
    template_deviated: false, created_at: "2026-09-30T00:00:00Z", operations: [], instances,
  } as never;
}

function renderInstances(instances: Instance[], replicas = 1) {
  render(
    <I18nextProvider i18n={i18n}>
      <MemoryRouter><DeploymentInstances dep={deployment(instances, replicas)} onChanged={() => {}} /></MemoryRouter>
    </I18nextProvider>,
  );
}

const panelOf = (uuid: string) => document.getElementById(`instance-${uuid}`) as HTMLElement;
const cardIds = () => [...document.querySelectorAll("[id^=instance-]")].map((el) => el.id);

test("a timed-out attempt listed first is still not the current instance", () => {
  const failed = inst("failed-attempt", { state: "failed", message: timeout, schedule: unit("|[]") });
  const running = inst("running-now", { state: "running", replica: { index: 0 }, schedule: unit("|[]") });
  renderInstances([failed, running]);

  assert.deepEqual(cardIds(), ["instance-running-now", "instance-failed-attempt"]);
  assert.ok(within(panelOf("failed-attempt")).getByText("Previous attempt"));
  assert.equal(within(panelOf("running-now")).queryByText("Previous attempt"), null);
  assert.ok(within(panelOf("failed-attempt")).getByText(timeout));
  assert.equal(within(panelOf("running-now")).queryByText(timeout), null);
});

test("a replacement that is still starting already owns the unit", () => {
  const failed = inst("failed-attempt", { state: "failed", message: timeout, schedule: unit("|[]") });
  const starting = inst("starting-now", { state: "starting", replica: { index: 0 }, schedule: unit("|[]") });
  renderInstances([failed, starting]);

  assert.deepEqual(cardIds(), ["instance-starting-now", "instance-failed-attempt"]);
  assert.ok(within(panelOf("failed-attempt")).getByText("Previous attempt"));
  assert.equal(within(panelOf("starting-now")).queryByText(timeout), null);
});

test("one current replica without a unit key still demotes the timed-out attempt", () => {
  const failed = inst("old-attempt", { state: "failed", message: timeout });
  const running = inst("running-now", { state: "running", replica: { index: 0 } });
  renderInstances([failed, running]);

  assert.deepEqual(cardIds(), ["instance-running-now", "instance-old-attempt"]);
  assert.ok(within(panelOf("old-attempt")).getByText("Previous attempt"));
});

test("the only failed instance keeps the timeout as its own end reason", () => {
  renderInstances([inst("only-failed", { state: "failed", message: timeout, schedule: unit("|[]") })]);
  assert.equal(document.body.textContent?.includes("Previous attempt"), false);
  assert.ok(within(panelOf("only-failed")).getByText(timeout));
});

test("an exhausted ordinary replica stays a current failure beside a running sibling", () => {
  const exhausted = inst("replica-1-failed", { state: "failed", message: timeout, schedule: unit("|[]") });
  const running = inst("replica-0-running", { state: "running", replica: { index: 0 }, schedule: unit("|[]") });
  renderInstances([exhausted, running], 2);

  assert.equal(within(panelOf("replica-1-failed")).queryByText("Previous attempt"), null);
  assert.ok(within(panelOf("replica-1-failed")).getByText(timeout));
  assert.deepEqual(cardIds(), ["instance-replica-1-failed", "instance-replica-0-running"]);
});

test("an exhausted replica with no unit key stays current when another replica runs", () => {
  const exhausted = inst("replica-1-failed", { state: "failed", message: timeout });
  const running = inst("replica-0-running", { state: "running", replica: { index: 0 } });
  renderInstances([exhausted, running], 2);

  assert.equal(within(panelOf("replica-1-failed")).queryByText("Previous attempt"), null);
  assert.deepEqual(cardIds(), ["instance-replica-1-failed", "instance-replica-0-running"]);
});

test("a failed ordinary attempt is previous once every replica is filled", () => {
  const failed = inst("old-attempt", { state: "failed", message: timeout, schedule: unit("|[]") });
  const first = inst("replica-a", { state: "running", replica: { index: 0 }, schedule: unit("|[]") });
  const second = inst("replica-b", { state: "running", replica: { index: 1 }, schedule: unit("|[]") });
  renderInstances([failed, first, second], 2);

  assert.deepEqual(cardIds(), ["instance-replica-a", "instance-replica-b", "instance-old-attempt"]);
  assert.ok(within(panelOf("old-attempt")).getByText("Previous attempt"));
});

test("an exhausted multi-node unit stays a current failure beside a different running unit", () => {
  const exhausted = inst("unit-1-failed", { state: "failed", message: timeout, schedule: unit("|node-c[]|node-d[]") });
  const running = inst("unit-0-running", { state: "running", replica: { index: 0 }, schedule: unit("|node-a[]|node-b[]") });
  renderInstances([exhausted, running], 2);

  assert.equal(within(panelOf("unit-1-failed")).queryByText("Previous attempt"), null);
  assert.ok(within(panelOf("unit-1-failed")).getByText(timeout));
  assert.deepEqual(cardIds(), ["instance-unit-1-failed", "instance-unit-0-running"]);
});

test("a replaced multi-node unit is a previous attempt before the other replica exists", () => {
  const failed = inst("unit-0-failed", { state: "failed", message: timeout, schedule: unit("|node-a[]|node-b[]") });
  const running = inst("unit-0-running", { state: "running", replica: { index: 0 }, schedule: unit("|node-a[]|node-b[]") });
  renderInstances([failed, running], 2);

  assert.deepEqual(cardIds(), ["instance-unit-0-running", "instance-unit-0-failed"]);
  assert.ok(within(panelOf("unit-0-failed")).getByText("Previous attempt"));
});

test("two current replicas and no unit keys are not guessed into history", () => {
  renderInstances([
    inst("old-attempt", { state: "failed", message: timeout }),
    inst("replica-a", { state: "running", replica: { index: 0 } }),
    inst("replica-b", { state: "running", replica: { index: 1 } }),
  ], 2);
  assert.equal(document.body.textContent?.includes("Previous attempt"), false);
  assert.deepEqual(cardIds(), ["instance-old-attempt", "instance-replica-a", "instance-replica-b"]);
});
