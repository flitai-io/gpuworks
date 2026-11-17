import { installCanvas } from "./helpers/analyticsFixtures.ts";
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

// A deployment's replicas should run one image. The drawer shows each
// replica's image ID and flags the ones that differ from the rest.

const closeChannels = trackMessageChannels();
const dom = new JSDOM("<!doctype html><html><body></body></html>", { url: "http://localhost/" });
for (const name of ["window", "document", "navigator", "HTMLElement", "SVGElement", "Element", "Node", "NodeFilter", "MutationObserver", "ShadowRoot", "File", "Blob", "HTMLBodyElement"] as const) {
  Object.defineProperty(globalThis, name, { value: dom.window[name], configurable: true });
}
Object.defineProperty(globalThis, "localStorage", { value: dom.window.localStorage, configurable: true });
Object.defineProperty(globalThis, "getComputedStyle", { value: (element: Element) => dom.window.getComputedStyle(element), configurable: true });
Object.defineProperty(dom.window, "matchMedia", { configurable: true, value: () => ({ matches: false, addListener() {}, removeListener() {}, addEventListener() {}, removeEventListener() {}, dispatchEvent: () => false }) });
installCanvas(dom);
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
const locale = read("en", "deployments");

const { MemoryRouter } = await import("react-router-dom");
const { cleanup, fireEvent, render, screen, waitFor, within } = await import("@testing-library/react");
const { selectInstance } = await import("./helpers/selectInstance.ts");
const { api } = await import("../src/api.ts");
const { imageMismatch, shortImageID } = await import("../src/pages/instanceImage.ts");
const { default: DeploymentInstances } = await import("../src/pages/DeploymentInstances.tsx");

const apiAny = api as unknown as Record<string, unknown>;
apiAny.instanceEvents = async () => cursorPage([]);
afterEach(() => cleanup());
after(() => { closeChannels(); dom.window.close(); });

const A = "sha256:3f9a1c2e7b4d0000000000000000000000000000000000000000000000000001";
const B = "sha256:8b72d04c19ae0000000000000000000000000000000000000000000000000002";
const C = "sha256:c0ffee00c0ffee000000000000000000000000000000000000000000000000cc";

// inst builds an instance as the server lists it. The API lists a
// deployment's instances newest first, which the tie rule relies on.
function inst(uuid: string, id: string | undefined, extra: Partial<Instance> = {}): Instance {
  return {
    uuid, deployment_id: 1, node_uuid: `node-${uuid}`, state: "running", gpu_indexes: [0], ports: [30000], api_port: 30000,
    image: "runner:vllm", message: "", container_id: `c-${uuid}`, sleep_level: 0, dist_role: "leader", nnodes: 1, node_rank: 0,
    ...(id === undefined ? {} : { image_digest_id: id }),
    ...extra,
  } as Instance;
}
const flagged = (instances: Instance[]) => [...imageMismatch(instances).mismatched].sort();

test("replicas on one image are not flagged", () => {
  assert.deepEqual(flagged([inst("r1", A), inst("r2", A), inst("r3", A)]), []);
});

test("one replica on a different image is flagged against the majority", () => {
  const result = imageMismatch([inst("r3", B), inst("r2", A), inst("r1", A)]);
  assert.deepEqual([...result.mismatched], ["r3"]);
  assert.equal(result.references.get(""), A);
});

test("the majority of three decides, whichever replica is the odd one", () => {
  assert.deepEqual(flagged([inst("r3", A), inst("r2", B), inst("r1", A)]), ["r2"]);
  assert.deepEqual(flagged([inst("r3", A), inst("r2", B), inst("r1", B)]), ["r3"]);
  // Three images, the most frequent one is the reference.
  assert.deepEqual(flagged([inst("r4", C), inst("r3", B), inst("r2", A), inst("r1", A)]), ["r3", "r4"]);
});

test("with no majority the group of the oldest replica is the reference", () => {
  // Newest first: r2 is the newer one, r1 the oldest.
  assert.deepEqual(flagged([inst("r2", B), inst("r1", A)]), ["r2"]);
  assert.deepEqual(flagged([inst("r2", A), inst("r1", B)]), ["r2"]);
  // 2 against 2: the group holding the oldest replica wins.
  assert.deepEqual(flagged([inst("r4", B), inst("r3", A), inst("r2", B), inst("r1", A)]), ["r2", "r4"]);
  // Three groups: A and B tie on top, C is smaller. The oldest replica of the
  // tied groups (r2, on B) decides, so the replica on C is flagged as well.
  assert.deepEqual(flagged([inst("r5", A), inst("r4", A), inst("r3", B), inst("r2", B), inst("r1", C)]), ["r1", "r4", "r5"]);
});

test("a replica without an image ID is never flagged and casts no vote", () => {
  assert.deepEqual(flagged([inst("r3", undefined), inst("r2", A), inst("r1", A)]), []);
  assert.deepEqual(flagged([inst("r3", ""), inst("r2", "  "), inst("r1", A)]), []);
  // Two IDs tie, the oldest (r1) decides, and the replica without an ID stays out of it.
  assert.deepEqual(flagged([inst("r3", undefined), inst("r2", B), inst("r1", A)]), ["r2"]);
  assert.deepEqual(flagged([inst("r1", undefined), inst("r2", undefined)]), []);
});

test("a single replica, or none, has nothing to differ from", () => {
  assert.deepEqual(flagged([inst("r1", A)]), []);
  assert.deepEqual(flagged([]), []);
});

test("the ranks of a multi-node replica are not compared; its leader stands for it", () => {
  const rank = (uuid: string, id: string, node: number, extra: Partial<Instance> = {}) =>
    inst(uuid, id, { nnodes: 2, node_rank: node, dist_group: uuid.split("-")[0], dist_attempt: 1, dist_role: node === 0 ? "leader" : "follower", headless: node > 0, ...extra });
  // Replica g1 is on A at both ranks, replica g2 leads on B: only g2's leader is flagged.
  const flaggedRanks = flagged([rank("g2-0", B, 0), rank("g2-1", B, 1), rank("g1-0", A, 0), rank("g1-1", A, 1), rank("g0-0", A, 0), rank("g0-1", A, 1)]);
  assert.deepEqual(flaggedRanks, ["g2-0"]);
  // A follower is never flagged on its own, even when its ID is odd.
  assert.deepEqual(flagged([rank("g1-0", A, 0), rank("g1-1", B, 1), rank("g0-0", A, 0), rank("g0-1", A, 1)]), []);
});

test("PD roles are compared inside each role, ended instances not at all", () => {
  assert.deepEqual(flagged([inst("p1", A, { role: "prefill" }), inst("d1", B, { role: "decode" })]), [], "two roles on two images is not a mismatch");
  assert.deepEqual(flagged([inst("d2", C, { role: "decode" }), inst("d1", B, { role: "decode" }), inst("p1", A, { role: "prefill" })]), ["d2"]);
  assert.deepEqual(flagged([inst("r3", B, { state: "stopped" }), inst("r2", A), inst("r1", A)]), [], "a stopped replica says nothing about what runs now");
});

test("the short ID keeps sha256: and eight characters, the full value elsewhere", () => {
  assert.equal(shortImageID(A), "sha256:3f9a1c2e…");
  assert.equal(shortImageID("sha256:abcd"), "sha256:abcd");
  assert.equal(shortImageID("0123456789abcdef"), "01234567…");
  assert.equal(shortImageID(undefined), "");
});

function deployment(instances: Instance[]) {
  return {
    id: 1, name: "qwen3-8b", model_name: "model", model_id: 1, served_name: "model", engine: "vllm", node_uuid: "node-a",
    desired_state: "active", runtime_state: "running", sleepable: false, replicas: instances.length, max_replicas: 32,
    template_deviated: false, created_at: "2026-09-30T00:00:00Z", operations: [], instances,
  } as never;
}

function renderInstances(instances: Instance[]) {
  render(
    <I18nextProvider i18n={i18n}>
      <MemoryRouter><DeploymentInstances dep={deployment(instances)} onChanged={() => {}} /></MemoryRouter>
    </I18nextProvider>,
  );
}

const panelOf = (uuid: string) => document.getElementById(`instance-${uuid}`) as HTMLElement;
const mismatchAlerts = () => [...document.querySelectorAll('[role="status"][data-status="warning"]')];
const warning = (uuid: string) => within(panelOf(uuid)).queryByRole("button", { name: locale.instance.imageMismatch.label });

test("each replica shows its short image ID with the full value in the title", () => {
  renderInstances([inst("r2", B), inst("r1", A)]);
  const title = (node: HTMLElement) => node.closest("[title]")?.getAttribute("title");
  assert.equal(title(within(panelOf("r1")).getByText("sha256:3f9a1c2e…")), A);
  assert.ok(within(panelOf("r1")).getByText(locale.instance.field.imageId));
  // The short form is for reading; the full ID is one click away for pasting.
  assert.ok(within(panelOf("r1")).getByRole("button", { name: locale.instance.copyImageId }));
  selectInstance("r2");
  assert.equal(title(within(panelOf("r2")).getByText("sha256:8b72d04c…")), B);
});

test("the odd replica gets a warning icon and the list gets one alert", async () => {
  renderInstances([inst("r3", B), inst("r2", A), inst("r1", A)]);
  const [alert] = mismatchAlerts();
  assert.equal(alert.textContent, "Replica image versions differ: 1 replica(s) run a different image from the others.");
  selectInstance("r3");
  assert.ok(warning("r3"), "the replica on the other image is flagged");
  selectInstance("r1");
  assert.equal(warning("r1"), null);
  selectInstance("r2");
  assert.equal(warning("r2"), null);
  assert.equal(mismatchAlerts().length, 1);
});

const shownTip = () => screen.queryAllByRole("tooltip").find((node) => !node.closest(".ant-tooltip-hidden"));

test("hovering the warning explains it", async () => {
  renderInstances([inst("r3", B), inst("r2", A), inst("r1", A)]);
  assert.equal(shownTip(), undefined, "no tooltip before the pointer arrives");
  selectInstance("r3");
  fireEvent.mouseEnter(warning("r3")!);
  await waitFor(() => assert.equal(shownTip()?.textContent, locale.instance.imageMismatch.tooltip));
});

test("the warning is a button that takes keyboard focus, and focus explains it", async () => {
  renderInstances([inst("r3", B), inst("r2", A), inst("r1", A)]);
  selectInstance("r3");
  const icon = warning("r3")!;
  assert.equal(icon.tagName, "BUTTON");
  assert.equal(icon.tabIndex >= 0, true);
  icon.focus();
  assert.equal(document.activeElement === icon, true, "the warning can take keyboard focus");
  fireEvent.focus(icon);
  await waitFor(() => assert.equal(shownTip()?.textContent, locale.instance.imageMismatch.tooltip));
});

test("identical IDs raise no alert and no icon", () => {
  renderInstances([inst("r2", A), inst("r1", A)]);
  assert.equal(mismatchAlerts().length, 0);
  assert.equal(screen.queryAllByRole("button", { name: locale.instance.imageMismatch.label }).length, 0);
});

test("replicas without an ID show a dash, never a warning, and raise no alert", () => {
  renderInstances([inst("r2", undefined), inst("r1", A)]);
  assert.equal(mismatchAlerts().length, 0);
  selectInstance("r2");
  assert.equal(warning("r2"), null);
  const row = within(panelOf("r2")).getByText(locale.instance.field.imageId).closest("tr") as HTMLElement;
  assert.equal(row.textContent, `${locale.instance.field.imageId}—`);
});

test("the alert counts the flagged replicas", () => {
  renderInstances([inst("r4", C), inst("r3", B), inst("r2", A), inst("r1", A)]);
  assert.ok(screen.getByText("Replica image versions differ: 2 replica(s) run a different image from the others."));
  selectInstance("r4");
  assert.ok(warning("r4"));
  selectInstance("r3");
  assert.ok(warning("r3"));
});

test("a deployment without instances shows the empty state and no alert", () => {
  renderInstances([]);
  assert.ok(screen.getByText(locale.detail.noInstances));
  assert.equal(mismatchAlerts().length, 0);
});
