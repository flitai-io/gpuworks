import assert from "node:assert/strict";
import fs from "node:fs";
import { after, afterEach, test } from "node:test";
import { trackMessageChannels } from "./helpers/trackedMessageChannels.ts";
import { JSDOM } from "jsdom";
import React from "react";
import { createInstance } from "i18next";
import { I18nextProvider, initReactI18next } from "react-i18next";

// A single-node deployment's node requirements are edited from its drawer:
// the stored requirements spread over nodes, hardware/network and label
// controls, the vendor shown but not editable, a live count of the nodes that
// match, and the server's refusal shown when it says no node could ever fit.
// The drawer shows each requirement as the node pages do, flagging a value no
// node carries.

const closeChannels = trackMessageChannels();
const dom = new JSDOM("<!doctype html><html><body></body></html>", { url: "http://localhost/" });
for (const name of ["window", "document", "navigator", "HTMLElement", "SVGElement", "Element", "Node", "MutationObserver", "ShadowRoot", "File", "Blob", "HTMLBodyElement"] as const) {
  Object.defineProperty(globalThis, name, { value: dom.window[name], configurable: true });
}
Object.defineProperty(globalThis, "getComputedStyle", { value: (element: Element) => dom.window.getComputedStyle(element), configurable: true });
Object.defineProperty(dom.window, "matchMedia", { configurable: true, value: () => ({ matches: false, addListener() {}, removeListener() {}, addEventListener() {}, removeEventListener() {}, dispatchEvent: () => false }) });
class ResizeObserverStub { observe() {} unobserve() {} disconnect() {} }
Object.defineProperty(globalThis, "ResizeObserver", { value: ResizeObserverStub, configurable: true });

const read = (ns: string) => JSON.parse(fs.readFileSync(new URL(`../src/i18n/locales/en/${ns}.json`, import.meta.url), "utf8"));
const i18n = createInstance();
await i18n.use(initReactI18next).init({
  lng: "en",
  fallbackLng: "en",
  resources: { en: { deployments: read("deployments"), deploy: read("deploy"), nodes: read("nodes"), common: read("common") } },
  defaultNS: "deployments",
  interpolation: { escapeValue: false },
});
const edit = i18n.getResourceBundle("en", "deployments").detail.placement.edit;

const { cleanup, render, screen, waitFor, within } = await import("@testing-library/react");
const userEvent = (await import("@testing-library/user-event")).default;
const { api } = await import("../src/api.ts");
const { APIError } = await import("../src/apiError.ts");
const { EditRequirementsButton, joinRequirements, splitRequirements } = await import("../src/pages/DeploymentRequirementsModal.tsx");
const { installFleet } = await import("./helpers/fakeFleet.ts");
const { RequirementTags } = await import("../src/pages/DeploymentPlacementStatus.tsx");

const apiAny = api as unknown as Record<string, unknown>;
const original = { ...apiAny };
afterEach(() => { cleanup(); Object.assign(apiAny, original); });
after(() => { closeChannels(); dom.window.close(); });

const sys = (key: string, value: string, display = value) => ({ key: `gpuworks.ai/${key}`, value, display });
const nodes = [
  { uuid: "u-a", name: "gpu-a", state: "online", gpu_count: 8, labels: { pool: "inference" }, system_labels: [sys("node", "u-a", "gpu-a"), sys("vendor", "nvidia", "NVIDIA"), sys("gpu-model", "A100")] },
  { uuid: "u-b", name: "gpu-b", state: "online", gpu_count: 8, labels: { pool: "training" }, system_labels: [sys("node", "u-b", "gpu-b"), sys("vendor", "nvidia", "NVIDIA"), sys("gpu-model", "H100")] },
  { uuid: "u-c", name: "npu-c", state: "online", gpu_count: 8, labels: { pool: "inference" }, system_labels: [sys("node", "u-c", "npu-c"), sys("vendor", "ascend", "Ascend")] },
] as never[];

function deployment(overrides: Record<string, unknown> = {}) {
  return {
    id: 7, name: "edited", model_name: "m", model_id: 1, served_name: "m", engine: "vllm", node_uuid: "u-a",
    desired_state: "active", runtime_state: "running", sleepable: false, replicas: 2, template_deviated: false,
    created_at: "2026-10-03T00:00:00Z", placement_strategy: "spread",
    node_requirements: { "gpuworks.ai/node": ["u-a"], "gpuworks.ai/vendor": ["nvidia"], pool: ["inference"] },
    node_requirement_labels: [
      { key: "gpuworks.ai/node", value: "u-a", display: "gpu-a", nodes: 1 },
      { key: "gpuworks.ai/vendor", value: "nvidia", display: "NVIDIA", display_code: "common:vendor.nvidia", nodes: 3 },
      { key: "pool", value: "inference", display: "inference", nodes: 2 },
    ],
    ...overrides,
  } as never;
}

function show(dep = deployment(), onChanged: (dep: unknown) => void = () => {}) {
  // The editor reads the fleet through the server: facets, matching pages and
  // single nodes, never the whole list.
  const fleet = installFleet(apiAny, nodes as never);
  render(<I18nextProvider i18n={i18n}><EditRequirementsButton dep={dep} onChanged={onChanged} /></I18nextProvider>);
  return fleet;
}

test("stored requirements spread over the controls and join back, the vendor left to the server", () => {
  const reqs = { "gpuworks.ai/node": ["u-a", "u-b"], "gpuworks.ai/vendor": ["nvidia"], "gpuworks.ai/gpu-model": ["A100", "H100"], pool: ["a", "b"] };
  const split = splitRequirements(reqs);
  assert.deepEqual(split, {
    nodes: ["u-a", "u-b"],
    system_conditions: ["gpuworks.ai/gpu-model=A100", "gpuworks.ai/gpu-model=H100"],
    label_conditions: ["pool=a", "pool=b"],
  });
  const { "gpuworks.ai/vendor": _vendor, ...rest } = reqs;
  assert.deepEqual(joinRequirements(split), rest);
  assert.deepEqual(joinRequirements({ nodes: [], system_conditions: [], label_conditions: [] }), {}, "nothing stated is any node");
});

test("the editor opens on the stored requirements, counts matches live and saves the edit", async () => {
  const sent: unknown[] = [];
  apiAny.setDeploymentRequirements = async (id: number, body: unknown) => {
    assert.equal(id, 7);
    sent.push(body);
    return deployment({ node_requirements: { "gpuworks.ai/vendor": ["nvidia"], pool: ["inference"] }, placement_strategy: "binpack" });
  };
  const changed: unknown[] = [];
  const user = userEvent.setup({ document: dom.window.document });
  show(deployment({ node_requirements: { "gpuworks.ai/node": ["u-b"], "gpuworks.ai/vendor": ["nvidia"], pool: ["inference"] } }), (dep) => changed.push(dep));
  await user.click(screen.getByRole("button", { name: edit.open }));
  const modal = (await screen.findByText(edit.title)).closest(".ant-modal") as HTMLElement;
  // The vendor is shown, translated, and is not a control.
  assert.ok(within(modal).getByText(edit.vendorFixed));
  // Pinned to gpu-b, which is not in pool=inference: nothing matches, as the
  // server counts it.
  assert.ok(await within(modal).findByText(edit.noMatch));

  // Unpin: the node select's clear control, then binpack from the keyboard.
  const nodeSelect = (await within(modal).findByText("gpu-b")).closest(".ant-select") as HTMLElement;
  await user.hover(nodeSelect);
  await user.click(nodeSelect.querySelector(".ant-select-clear") as HTMLElement);
  await waitFor(() => assert.ok(within(modal).getByText(i18n.t("detail.placement.edit.matches", { count: 1, total: 3 }))));
  const binpack = within(modal).getByRole("radio", { name: i18n.t("detail.placement.strategy.binpack") });
  binpack.focus();
  await user.keyboard(" ");
  await user.click(within(modal).getByRole("button", { name: edit.save }));
  await waitFor(() => assert.equal(changed.length, 1));
  assert.deepEqual(sent, [{ node_requirements: { pool: ["inference"] }, placement_strategy: "binpack" }]);
});

test("requirements nothing matches warn before saving, and the server's refusal is shown", async () => {
  apiAny.setDeploymentRequirements = async () => {
    throw new APIError("DEPLOYMENT_NEVER_FITS", { pool: 0, shapes: 0, node: "", reason: "" }, "no node could fit", 400);
  };
  const changed: unknown[] = [];
  const user = userEvent.setup({ document: dom.window.document });
  show(deployment({ node_requirements: { "gpuworks.ai/vendor": ["nvidia"], pool: ["nowhere"] } }), (dep) => changed.push(dep));
  await user.click(screen.getByRole("button", { name: edit.open }));
  const modal = (await screen.findByText(edit.title)).closest(".ant-modal") as HTMLElement;
  assert.ok(await within(modal).findByText(edit.noMatch));
  await user.click(within(modal).getByRole("button", { name: edit.save }));
  await within(modal).findByText(new RegExp("No node this deployment may run on could fit a replica"));
  assert.equal(changed.length, 0, "a refused edit does not close the editor");
});

test("a deleting deployment cannot be edited", () => {
  show(deployment({ desired_state: "deleting" }));
  assert.equal((screen.getByRole("button", { name: edit.open }) as HTMLButtonElement).disabled, true);
});

test("the drawer shows requirements as the node pages do and flags a value no node carries", () => {
  render(<I18nextProvider i18n={i18n}><RequirementTags dep={deployment({
    node_requirements: { "gpuworks.ai/node": ["u-a", "gone"], "gpuworks.ai/vendor": ["nvidia"] },
    node_requirement_labels: [
      { key: "gpuworks.ai/node", value: "gone", display: "gone", nodes: 0 },
      { key: "gpuworks.ai/node", value: "u-a", display: "gpu-a", nodes: 1 },
      { key: "gpuworks.ai/vendor", value: "nvidia", display: "NVIDIA", display_code: "common:vendor.nvidia", nodes: 3 },
    ],
  })} /></I18nextProvider>);
  const pinned = screen.getByText(`Node: gpu-a | gone (${i18n.t("detail.placement.noNode")})`);
  assert.ok(pinned.closest('[data-status="warning"]'), "a value no node carries is flagged");
  assert.ok(screen.getByText(/^Vendor: /));
});
