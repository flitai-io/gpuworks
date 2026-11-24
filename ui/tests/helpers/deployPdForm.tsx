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

// Shared fixture for the PD deploy form tests (deployPdFormDraft, deployPdFormFollowers,
// deployPdFormPreview): each test file imports it, so each file keeps its own isolated DOM
// and cleanup hooks in its own test process, and stays within the per-file runtime budget.
//
// The PD deploy form: choosing "PD disaggregation" as the topology, adding
// and removing role instance rows (including by keyboard), the placement.pd
// payload it builds for evaluate, a field error the server attributes to
// "placement.pd", and evaluate's per-unit preview (a fitting and a
// non-fitting instance). Driven end to end through the real page, the way
// deployEvaluateFieldVisibility.test.tsx drives field-error attribution.

const closeChannels = trackMessageChannels();
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
const commonLocale = JSON.parse(
  fs.readFileSync(new URL("../../src/i18n/locales/en/common.json", import.meta.url), "utf8"),
);
const imagesLocale = JSON.parse(
  fs.readFileSync(new URL("../../src/i18n/locales/en/images.json", import.meta.url), "utf8"),
);
const i18n = createInstance();
await i18n.use(initReactI18next).init({
  lng: "en",
  fallbackLng: "en",
  resources: { en: { deploy: deployLocale, common: commonLocale, images: imagesLocale } },
  defaultNS: "common",
  interpolation: { escapeValue: false },
});

const { MemoryRouter } = await import("react-router-dom");
const { cleanup, fireEvent, render, screen, waitFor, within } = await import("@testing-library/react");
const userEvent = (await import("@testing-library/user-event")).default;
const { apiErrorText, APIError } = await import("../../src/apiError.ts");
const { api } = await import("../../src/api.ts");

export function errorText(code: string, params?: Record<string, unknown>) {
  return apiErrorText(
    (key: string, options?: Record<string, unknown>) => i18n.t(key, options),
    new APIError(code, params ?? {}, code, 200),
  );
}

const gpu = (index: number) => ({
  index,
  name: "H100",
  vendor: "nvidia",
  schedulable: true,
  arch_id: "Hopper",
  compute_capability: "9.0",
});
const nodeOne = {
  uuid: "node-1",
  name: "GPU Node 1",
  hostname: "gpu-node-1",
  state: "online",
  gpu_count: 2,
  gpu_vendor: "nvidia",
  gpus: [gpu(0), gpu(1)],
} as never;
const nodeTwo = {
  uuid: "node-2",
  name: "GPU Node 2",
  hostname: "gpu-node-2",
  state: "online",
  gpu_count: 2,
  gpu_vendor: "nvidia",
  gpus: [gpu(0), gpu(1)],
} as never;
const model = {
  id: 3,
  name: "Qwen/Qwen3-0.6B",
  source: "modelscope",
  source_id: "Qwen/Qwen3-0.6B",
  input_modalities: ["text"],
  weight_bytes: 1_500_000_000,
  nodes: [{ node_uuid: "node-1", state: "present" }],
} as never;
const engineProfile = {
  name: "vllm",
  sleep: { supported: false },
  quick_form_keys: [],
  param_specs: [],
  // Two modes, ascend listed first - exercises defaultPdMode's vendor match
  // (the nvidia vendor picked in openDeployPage) rather than a bare modes[0].
  pd_modes: fixturePdModes("vllm"),
} as never;

const apiAny = api as unknown as Record<string, unknown>;
const originals: Record<string, unknown> = {};
export function stub(name: string, value: unknown) {
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
// stubModels answers the model endpoints as the server does (one searched
// page, single models) and refuses listing every model.
function stubModels(list: unknown[]) {
  const fake = fakeModels(list as never);
  stub("models", fake.models_all);
  for (const name of ["modelsPage", "model"] as const) stub(name, fake[name]);
  return fake;
}
function resetStubs() {
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

type PdRankAnswer = {
  node_uuid: string;
  node_name: string;
  address: string;
  fits: boolean;
  gpus?: number[];
  reason?: string;
  argv: string[];
  command: string[];
  followers?: PdRankAnswer[];
};
type PdRoleAnswer = { role: "decode" | "prefill"; units: PdRankAnswer[] };
type PdDraft = {
  mode: string;
  fallback?: string;
  kv_load_failure?: string;
  roles: Record<string, { tp?: number; pp?: number; units: Array<{ node_uuid?: string; followers?: Array<{ node_uuid?: string }> }> }>;
};

// pdRank is one rank of a PD unit as evaluatePD previews it: argv always, the
// command the entrypoint and argv joined, GPUs only when the rank fits.
export function pdRank(node: string, name: string, argv: string[], extra: Partial<PdRankAnswer> = {}): PdRankAnswer {
  return {
    node_uuid: node,
    node_name: name,
    address: `10.0.0.${name.at(-1)}`,
    fits: true,
    gpus: [0],
    argv,
    command: ["vllm", "serve", ...argv],
    ...extra,
  };
}

const rankNodes = (unit?: { node_uuid?: string; followers?: Array<{ node_uuid?: string }> }) =>
  [unit?.node_uuid, ...(unit?.followers ?? []).map((follower) => follower.node_uuid)].join(",");

// pdEvaluate answers evaluate the way the server's evaluatePD does for the
// drafts these tests build. An ordinary draft gets the ordinary answer. A PD
// draft whose rows do not yet name the preview's nodes - these tests only
// ever leave a row's node unpicked - is rejected on placement.pd before any
// planning (PD_NODE_REQUIRED), with no preview. A PD draft naming them gets
// the per-rank preview with each role's TP/PP as the draft states them, ok
// only when every rank fits, and the top-level command describing the first
// decode instance. beforePreview lets a test hold that answer in flight.
export function pdEvaluate(roles: PdRoleAnswer[], beforePreview?: () => Promise<void>) {
  return async (payload: Record<string, unknown>) => {
    const pd = (payload.placement as { pd?: PdDraft } | undefined)?.pd;
    if (!pd) return { ...baseAnswer };
    for (const role of roles) {
      const drafted = pd.roles[role.role]?.units ?? [];
      if (drafted.length !== role.units.length || drafted.some((unit, index) => rankNodes(unit) !== rankNodes(role.units[index]))) {
        return {
          ...baseAnswer,
          ok: false,
          command: undefined,
          errors: [{ field: "placement.pd", code: "PD_NODE_REQUIRED", params: { role: role.role } }],
        };
      }
    }
    await beforePreview?.();
    const ranks = roles.flatMap((role) => role.units.flatMap((unit) => [unit, ...(unit.followers ?? [])]));
    const firstDecode = roles.find((role) => role.role === "decode")!.units[0];
    return {
      ...baseAnswer,
      ok: ranks.every((rank) => rank.fits),
      argv: firstDecode.argv,
      command: firstDecode.command,
      replicas: roles.reduce((count, role) => count + role.units.length, 0),
      plan_scope: "per_instance",
      pd: {
        mode: pd.mode,
        fallback: pd.fallback,
        kv_load_failure: pd.kv_load_failure,
        roles: roles.map((role) => ({
          role: role.role,
          tp: pd.roles[role.role].tp ?? 1,
          ...(pd.roles[role.role].pp ? { pp: pd.roles[role.role].pp } : {}),
          units: role.units,
        })),
      },
    };
  };
}

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

// A PD draft is placed by the platform until the deployer switches the same
// auto / manual control a single node has; the form opens on automatic, and
// most tests here are about naming the units' nodes, so they switch to manual.
export async function choosePdTopology(placement: "auto" | "manual" = "manual") {
  const pdCard = await screen.findByText(deployLocale.workbench.node.pd);
  await waitFor(() => assert.ok(!pdCard.closest("button")?.hasAttribute("disabled")));
  fireEvent.click(pdCard);
  await screen.findByText(deployLocale.pd.mode);
  if (placement === "manual") {
    const pool = document.querySelector(".deploy-node-pool") as HTMLElement;
    fireEvent.click(within(pool).getByText(deployLocale.workbench.node.manual));
    await screen.findByText(deployLocale.workbench.node.manualUnitsDescription);
  }
}

// The node picker lists a server search's nodes once it opens.
export async function selectOption(combobox: HTMLElement, label: string) {
  await pickNodes(combobox, label);
}

// pickPdNode picks the node of a PD instance row (or, with a follower index,
// of one of its extra nodes) by the option's visible label.
export async function pickPdNode(role: "decode" | "prefill", label: string, follower?: number) {
  const suffix = follower ? ` ${follower}` : "";
  await selectOption(
    screen.getByRole("combobox", { name: new RegExp(`^${deployLocale.pd.role[role]} instance 1 node${suffix}$`) }),
    label,
  );
}
export { assert, test, fireEvent, screen, waitFor, within, userEvent };
