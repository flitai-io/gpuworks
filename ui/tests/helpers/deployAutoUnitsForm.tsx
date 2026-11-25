import assert from "node:assert/strict";
import { fakeFleet } from "./fakeFleet.ts";
import { fakeModels } from "./fakeModels.ts";
import fs from "node:fs";
import { after, afterEach, test } from "node:test";
import { JSDOM } from "jsdom";
import React from "react";
import { createInstance } from "i18next";
import { I18nextProvider, initReactI18next } from "react-i18next";
import { trackMessageChannels } from "./trackedMessageChannels.ts";
import { fixturePdModes } from "./engineFixture.ts";
import { register } from "node:module";
import { pathToFileURL } from "node:url";
import { list } from "./listPage.ts";
import { pickNodes } from "./nodePicker.ts";
register(
  pathToFileURL(
    new URL("../deploySleepableCssHook.mjs", import.meta.url).pathname,
  ),
);

// Shared fixture for the automatic-placement deploy form tests (deployAutoUnitsFormPd,
// deployAutoUnitsFormMulti, deployAutoUnitsFormClone): each test file imports it, so each
// file keeps its own isolated DOM and cleanup hooks in its own test process, and stays
// within the per-file runtime budget.
//
// Multi-node and PD drafts reuse the single node's auto / manual switch and
// open on automatic: the platform picks the nodes, so the form states only the
// sizes (node count, TP, the units of each PD role), shows the nodes the
// server would pick now as a preview, and renders the server's refusals beside
// the switch. Manual shows the node pickers as before. Driven end to end
// through the real page, like deployPdFormDraft.test.tsx.

export const closeChannels = trackMessageChannels();
export const dom = new JSDOM("<!doctype html><html><body></body></html>", {
  url: "http://localhost/",
});
for (const name of [
  "window",
  "document",
  "navigator",
  "HTMLElement",
  "HTMLTextAreaElement",
  "SVGElement",
  "Element",
  "Node",
  "MutationObserver",
  "NodeFilter",
  "ShadowRoot",
] as const) {
  Object.defineProperty(globalThis, name, {
    value: dom.window[name],
    configurable: true,
  });
}
Object.defineProperty(globalThis, "getComputedStyle", {
  value: (element: Element) => dom.window.getComputedStyle(element),
  configurable: true,
});
Object.defineProperty(dom.window, "matchMedia", {
  configurable: true,
  value: () => ({
    matches: false,
    addListener() {},
    removeListener() {},
    addEventListener() {},
    removeEventListener() {},
    dispatchEvent: () => false,
  }),
});
class ResizeObserverStub {
  observe() {}
  unobserve() {}
  disconnect() {}
}
Object.defineProperty(globalThis, "ResizeObserver", {
  value: ResizeObserverStub,
  configurable: true,
});

export const deployLocale = JSON.parse(
  fs.readFileSync(new URL("../../src/i18n/locales/en/deploy.json", import.meta.url), "utf8"),
);
export const commonLocale = JSON.parse(
  fs.readFileSync(new URL("../../src/i18n/locales/en/common.json", import.meta.url), "utf8"),
);
export const imagesLocale = JSON.parse(
  fs.readFileSync(new URL("../../src/i18n/locales/en/images.json", import.meta.url), "utf8"),
);
export const i18n = createInstance();
await i18n.use(initReactI18next).init({
  lng: "en",
  fallbackLng: "en",
  resources: { en: { deploy: deployLocale, common: commonLocale, images: imagesLocale } },
  defaultNS: "common",
  interpolation: { escapeValue: false },
});

export const { MemoryRouter } = await import("react-router-dom");
export const { cleanup, fireEvent, render, screen, waitFor, within } = await import("@testing-library/react");
export const userEvent = (await import("@testing-library/user-event")).default;
export const { apiErrorText, APIError } = await import("../../src/apiError.ts");
export const { api } = await import("../../src/api.ts");

export function errorText(code: string, params?: Record<string, unknown>) {
  return apiErrorText(
    (key: string, options?: Record<string, unknown>) => i18n.t(key, options),
    new APIError(code, params ?? {}, code, 200),
  );
}

export const gpu = (index: number) => ({
  index,
  name: "H100",
  vendor: "nvidia",
  schedulable: true,
  arch_id: "Hopper",
  compute_capability: "9.0",
});
export const nodeOne = {
  uuid: "node-1",
  name: "GPU Node 1",
  hostname: "gpu-node-1",
  state: "online",
  gpu_count: 2,
  gpu_vendor: "nvidia",
  gpus: [gpu(0), gpu(1)],
} as never;
export const nodeTwo = {
  uuid: "node-2",
  name: "GPU Node 2",
  hostname: "gpu-node-2",
  state: "online",
  gpu_count: 2,
  gpu_vendor: "nvidia",
  gpus: [gpu(0), gpu(1)],
} as never;
export const model = {
  id: 3,
  name: "Qwen/Qwen3-0.6B",
  source: "modelscope",
  source_id: "Qwen/Qwen3-0.6B",
  input_modalities: ["text"],
  weight_bytes: 1_500_000_000,
  nodes: [{ node_uuid: "node-1", state: "present" }],
} as never;
export const engineProfile = {
  name: "vllm",
  sleep: { supported: false },
  quick_form_keys: [],
  param_specs: [],
  // Two modes, ascend listed first - exercises defaultPdMode's vendor match
  // (the nvidia vendor picked in openDeployPage) rather than a bare modes[0].
  pd_modes: fixturePdModes("vllm"),
} as never;

export const apiAny = api as unknown as Record<string, unknown>;
export const originals: Record<string, unknown> = {};
export function stub(name: string, value: unknown) {
  originals[name] = apiAny[name];
  apiAny[name] = value;
}

// stubFleet answers the node endpoints as the server does (one searched page,
// single nodes, facets) and refuses listing every node.
export function stubFleet(list: unknown[]) {
  const fake = fakeFleet(list as never);
  for (const name of ["nodes", "nodesPage", "node", "nodeFacets"] as const) stub(name, fake[name]);
  return fake;
}
// stubModels answers the model endpoints as the server does (one searched
// page, single models) and refuses listing every model.
export function stubModels(list: unknown[]) {
  const fake = fakeModels(list as never);
  stub("models", fake.models_all);
  for (const name of ["modelsPage", "model"] as const) stub(name, fake[name]);
  return fake;
}
export function resetStubs() {
  stubModels([model]);
  stubFleet([nodeOne, nodeTwo]);
  stub("engines", async () => list([engineProfile]));
  stub("inferenceTemplateIndex", async () => ({ items: [] }));
  stub("imageVersions", async () => list([]));
  stub("images", async () => list([]));
  stub("routingPolicies", async () => list([]));
}
resetStubs();
afterEach(() => {
  cleanup();
  resetStubs();
});
after(() => {
  closeChannels();
  dom.window.close();
});

export const baseAnswer = {
  ok: true,
  warnings: [] as Array<{ code: string; params?: Record<string, unknown> }>,
  defaults: { model_mount: "/model", master_port: 29500, engine: "vllm" },
  form_values: { engine: "vllm" },
  image: "runner:vllm",
  command: ["vllm", "serve", "/model"],
  argv: [],
  template_deviated: false,
  template_match: true,
  eligible_nodes: [
    { uuid: "node-1", name: "GPU Node 1", eligible: true },
    { uuid: "node-2", name: "GPU Node 2", eligible: true },
  ],
  acquisition: { image: { image: "runner:vllm", cached: true } },
};


export async function openDeployPage() {
  const DeployPage = (await import("../../src/pages/DeployPage.tsx")).default;
  render(
    <MemoryRouter initialEntries={["/deploy?model=3"]}>
      <I18nextProvider i18n={i18n}>
        <DeployPage />
      </I18nextProvider>
    </MemoryRouter>,
  );
  const vendor = await screen.findByRole("combobox", {
    name: new RegExp(deployLocale.basic.targetVendor),
  });
  fireEvent.mouseDown(vendor);
  fireEvent.click(await screen.findByTitle(deployLocale.basic.vendor.nvidia));
}

export async function chooseTopology(card: "pd" | "multi" | "single") {
  const text = deployLocale.workbench.node[card];
  const element = await screen.findByText(text);
  await waitFor(() => assert.ok(!element.closest("button")?.hasAttribute("disabled")));
  fireEvent.click(element);
}

export const pool = () => within(document.querySelector(".deploy-node-pool") as HTMLElement);
export const segment = (label: string) => pool().getByText(label).closest("label") as HTMLElement;
export const selected = (label: string) => segment(label).classList.contains("ant-segmented-item-selected");
export const node = deployLocale.workbench.node;

export async function selectOption(combobox: HTMLElement, label: string) {
  await pickNodes(combobox, label);
}

export type Body = {
  placement_mode?: string;
  placement_strategy?: string;
  nnodes?: number;
  node_uuid?: string;
  node_uuids?: string[];
  placement?: { tp?: number; gpus?: number[]; pd?: { roles: Record<string, { tp?: number; units: Array<Record<string, unknown>> }> } };
};
export const lastOf = (payloads: Array<Record<string, unknown>>) => payloads.at(-1) as Body;

export function recordEvaluate(answer: (payload: Record<string, unknown>) => Record<string, unknown> = () => ({ ...baseAnswer })) {
  const payloads: Array<Record<string, unknown>> = [];
  stub("evaluate", async (payload: Record<string, unknown>) => {
    payloads.push(payload);
    return answer(payload);
  });
  return payloads;
}

export const placementNone = {
  field: "placement_mode",
  code: "UNIT_PLACEMENT_NONE",
  params: { nodes: 2, unmet: 0, cordoned: 0, offline: 0, busy: 2, no_network: 0, largest: 1 },
};

export { assert, test, I18nextProvider, React };
