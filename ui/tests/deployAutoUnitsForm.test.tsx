import assert from "node:assert/strict";
import { fakeFleet } from "./helpers/fakeFleet.ts";
import { fakeModels } from "./helpers/fakeModels.ts";
import fs from "node:fs";
import { after, afterEach, test } from "node:test";
import { JSDOM } from "jsdom";
import React from "react";
import { createInstance } from "i18next";
import { I18nextProvider, initReactI18next } from "react-i18next";
import { trackMessageChannels } from "./helpers/trackedMessageChannels.ts";
import { fixturePdModes } from "./helpers/engineFixture.ts";
import { register } from "node:module";
import { pathToFileURL } from "node:url";
import { list } from "./helpers/listPage.ts";
register(
  pathToFileURL(
    new URL("./deploySleepableCssHook.mjs", import.meta.url).pathname,
  ),
);

// Multi-node and PD drafts reuse the single node's auto / manual switch and
// open on automatic: the platform picks the nodes, so the form states only the
// sizes (node count, TP, the units of each PD role), shows the nodes the
// server would pick now as a preview, and renders the server's refusals beside
// the switch. Manual shows the node pickers as before. Driven end to end
// through the real page, like deployPdForm.test.tsx.

const closeChannels = trackMessageChannels();
const dom = new JSDOM("<!doctype html><html><body></body></html>", {
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

const deployLocale = JSON.parse(
  fs.readFileSync(new URL("../src/i18n/locales/en/deploy.json", import.meta.url), "utf8"),
);
const commonLocale = JSON.parse(
  fs.readFileSync(new URL("../src/i18n/locales/en/common.json", import.meta.url), "utf8"),
);
const imagesLocale = JSON.parse(
  fs.readFileSync(new URL("../src/i18n/locales/en/images.json", import.meta.url), "utf8"),
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
const { apiErrorText, APIError } = await import("../src/apiError.ts");
const { api } = await import("../src/api.ts");

function errorText(code: string, params?: Record<string, unknown>) {
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

const baseAnswer = {
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


async function openDeployPage() {
  const DeployPage = (await import("../src/pages/DeployPage.tsx")).default;
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

async function chooseTopology(card: "pd" | "multi" | "single") {
  const text = deployLocale.workbench.node[card];
  const element = await screen.findByText(text);
  await waitFor(() => assert.ok(!element.closest("button")?.hasAttribute("disabled")));
  fireEvent.click(element);
}

const pool = () => within(document.querySelector(".deploy-node-pool") as HTMLElement);
const segment = (label: string) => pool().getByText(label).closest("label") as HTMLElement;
const selected = (label: string) => segment(label).classList.contains("ant-segmented-item-selected");
const node = deployLocale.workbench.node;

async function selectOption(combobox: HTMLElement, label: string) {
  const selector = combobox.closest(".ant-select")!.querySelector(".ant-select-selector") as HTMLElement;
  fireEvent.mouseDown(selector);
  let option: Element | undefined;
  await waitFor(() => {
    const dropdowns = document.querySelectorAll(".ant-select-dropdown");
    const dropdown = dropdowns[dropdowns.length - 1];
    option = [...(dropdown?.querySelectorAll(".ant-select-item-option") ?? [])].find((el) => el.textContent === label);
    if (!option) throw new Error(`option not found: ${label}`);
  });
  fireEvent.click(option!);
}

type Body = {
  placement_mode?: string;
  placement_strategy?: string;
  nnodes?: number;
  node_uuid?: string;
  node_uuids?: string[];
  placement?: { tp?: number; gpus?: number[]; pd?: { roles: Record<string, { tp?: number; units: Array<Record<string, unknown>> }> } };
};
const lastOf = (payloads: Array<Record<string, unknown>>) => payloads.at(-1) as Body;

function recordEvaluate(answer: (payload: Record<string, unknown>) => Record<string, unknown> = () => ({ ...baseAnswer })) {
  const payloads: Array<Record<string, unknown>> = [];
  stub("evaluate", async (payload: Record<string, unknown>) => {
    payloads.push(payload);
    return answer(payload);
  });
  return payloads;
}

const placementNone = {
  field: "placement_mode",
  code: "UNIT_PLACEMENT_NONE",
  params: { nodes: 2, unmet: 0, cordoned: 0, offline: 0, busy: 2, no_network: 0, largest: 1 },
};

test("a PD draft opens on automatic: no node or GPU pickers, units counted, payload says auto without a node", async () => {
  const payloads = recordEvaluate();
  await openDeployPage();
  await chooseTopology("pd");
  await screen.findByText(deployLocale.pd.mode);
  assert.ok(selected(node.auto), "the same control a single node has, on automatic");
  // One line says how automatic placement works; that the list is only a
  // preview sits behind the help mark on that line.
  const unitsLine = screen.getByText(node.autoUnitsDescription);
  assert.equal(screen.queryByText(node.autoUnitsHelp), null);
  await userEvent.setup({ document: dom.window.document }).hover(within(unitsLine).getByRole("button", { name: commonLocale.helpTip.label }));
  assert.equal((await screen.findByRole("tooltip")).textContent, node.autoUnitsHelp);
  assert.equal(screen.queryAllByRole("combobox", { name: /instance 1 node$/ }).length, 0, "no unit node picker");
  assert.equal(screen.queryAllByRole("combobox", { name: /instance 1 GPU/ }).length, 0, "no unit GPU picker");
  assert.equal(screen.queryAllByText(deployLocale.pd.addNode.replace("{{role}}", deployLocale.pd.role.decode).replace("{{index}}", "1")).length, 0, "an automatic unit fits one node");
  // The units are still counted, per role, with the TP the deployer sets.
  fireEvent.click((await screen.findByText(deployLocale.pd.addUnit.replace("{{role}}", deployLocale.pd.role.decode))).closest("button") as HTMLElement);
  await userEvent.setup({ document: dom.window.document }).type(
    screen.getByRole("spinbutton", { name: deployLocale.pd.tpAria.replace("{{role}}", deployLocale.pd.role.decode) }),
    "{backspace}2",
  );
  await waitFor(() => {
    const body = lastOf(payloads);
    assert.equal(body.placement_mode, "auto");
    assert.equal(body.placement_strategy, "binpack", "PD packs by default");
    assert.equal(body.node_uuid, undefined);
    assert.deepEqual(body.placement?.pd?.roles.decode, { tp: 2, units: [{}, {}] });
    assert.deepEqual(body.placement?.pd?.roles.prefill, { tp: 1, units: [{}] });
  });
  assert.ok(screen.getByText(deployLocale.pd.autoUnit.replace("{{role}}", deployLocale.pd.role.decode).replace("{{index}}", "2")));
});

test("the automatic PD preview is the server's pick, titled as a preview; the form keeps its own counts", async () => {
  recordEvaluate(() => ({
    ...baseAnswer,
    auto_placed: true,
    pd: {
      mode: "vllm-nixl", fallback: "decode", kv_load_failure: "fail",
      roles: [
        { role: "decode", tp: 1, units: [{ node_uuid: "node-2", node_name: "GPU Node 2", address: "10.0.0.2", fits: true, gpus: [0], argv: [], command: ["vllm"] }] },
        { role: "prefill", tp: 1, units: [{ node_uuid: "node-1", node_name: "GPU Node 1", address: "10.0.0.1", fits: true, gpus: [1], argv: [], command: ["vllm"] }] },
      ],
    },
  }));
  await openDeployPage();
  await chooseTopology("pd");
  const preview = within(await screen.findByTestId("pd-preview"));
  await preview.findByText(deployLocale.pd.previewAutoTitle);
  await preview.findByText("GPU Node 2");
  assert.ok(preview.getByText("GPU Node 1"));
  assert.equal(preview.queryByText(deployLocale.pd.previewTitle), null);
});

test("nothing to place a PD draft on is the server's explanation beside the switch, and the draft stays editable", async () => {
  recordEvaluate(() => ({ ...baseAnswer, ok: false, auto_placed: true, errors: [placementNone] }));
  await openDeployPage();
  await chooseTopology("pd");
  const text = errorText("UNIT_PLACEMENT_NONE", placementNone.params);
  await waitFor(() => assert.ok(pool().getByText(text)), { timeout: 4000 });
  assert.ok(pool().getByText(text).closest(".ant-form-item-explain-error"), "rendered as the field's error beside the control");
  // A request automatic placement cannot take reads the same way.
  assert.ok(screen.getByText(deployLocale.pd.addUnit.replace("{{role}}", deployLocale.pd.role.decode)));
});

test("switching a PD draft to manual brings the node pickers back and the payload names the nodes", async () => {
  const payloads = recordEvaluate();
  await openDeployPage();
  await chooseTopology("pd");
  await screen.findByText(deployLocale.pd.mode);
  fireEvent.click(segment(node.manual));
  await screen.findByText(node.manualUnitsDescription);
  assert.ok(selected(node.manual));
  await selectOption(screen.getByRole("combobox", { name: new RegExp(`^${deployLocale.pd.role.decode} instance 1 node$`) }), "GPU Node 1 · 2 GPU");
  await selectOption(screen.getByRole("combobox", { name: new RegExp(`^${deployLocale.pd.role.prefill} instance 1 node$`) }), "GPU Node 2 · 2 GPU");
  assert.ok(screen.getAllByRole("combobox", { name: /instance 1 GPU/ }).length >= 2, "cards can be pinned by hand");
  await waitFor(() => {
    const body = lastOf(payloads);
    assert.equal(body.placement_mode, undefined, "a PD request naming its nodes does not say auto");
    assert.equal(body.placement_strategy, undefined);
    assert.deepEqual(body.placement?.pd?.roles.decode.units, [{ node_uuid: "node-1" }]);
    assert.deepEqual(body.placement?.pd?.roles.prefill.units, [{ node_uuid: "node-2" }]);
  });
  // And back to automatic: the nodes picked by hand are no longer stated.
  fireEvent.click(segment(node.auto));
  await waitFor(() => {
    const body = lastOf(payloads);
    assert.equal(body.placement_mode, "auto");
    assert.deepEqual(body.placement?.pd?.roles.decode.units, [{}]);
  });
});

test("a multi-node draft opens on automatic: only the node count and TP are stated, the nodes are a preview", async () => {
  const payloads = recordEvaluate();
  await openDeployPage();
  await chooseTopology("multi");
  await screen.findByText(node.multiAutoTitle);
  assert.ok(selected(node.auto));
  assert.ok(screen.getByText(node.autoUnitsDescription));
  assert.equal(screen.queryByText(node.multiPlaceholder), null, "no per-rank node picker");
  assert.ok(screen.getByText(deployLocale.workbench.parallel.tpRequiredAuto), "automatic placement asks for a TP");
  await userEvent.setup({ document: dom.window.document }).type(screen.getByLabelText(deployLocale.workbench.resource.tp), "4");
  await waitFor(() => {
    const body = lastOf(payloads);
    assert.equal(body.placement_mode, "auto");
    assert.equal(body.placement_strategy, "binpack", "a multi-node group packs by default");
    assert.equal(body.nnodes, 2);
    assert.equal(body.node_uuids, undefined);
    assert.equal(body.node_uuid, undefined);
    assert.deepEqual(body.placement, { tp: 4 });
  });
  assert.equal(screen.queryByText(deployLocale.workbench.parallel.tpRequiredAuto), null);
});

test("the automatic multi-node preview lists the server's ranks as a read-only preview", async () => {
  recordEvaluate(() => ({
    ...baseAnswer,
    auto_placed: true,
    parallelism: { tp: 4, pp: 1, nodes: 2, gpus_per_node: 2 },
    preflight: { ok: true },
    rank_commands: [
      { node_uuid: "node-2", node_name: "GPU Node 2", node_rank: 0, role: "leader", headless: false, image: "runner:vllm", argv: [] },
      { node_uuid: "node-1", node_name: "GPU Node 1", node_rank: 1, role: "follower", headless: true, image: "runner:vllm", argv: [] },
    ],
  }));
  await openDeployPage();
  await chooseTopology("multi");
  const preview = within(await screen.findByTestId("auto-group-preview"));
  assert.ok(preview.getByText("GPU Node 2"));
  assert.ok(preview.getByText("GPU Node 1"));
  assert.ok(preview.getByText("Master"));
  assert.ok(preview.getByText("Worker"));
  assert.ok(screen.getByText(node.previewTitle));
  assert.equal(document.querySelector("[data-testid=auto-group-preview] .ant-select"), null, "nothing to pick in a preview");
});

test("a multi-node draft that cannot be placed shows the server's reason, in the user's words", async () => {
  recordEvaluate(() => ({ ...baseAnswer, ok: false, auto_placed: true, errors: [placementNone] }));
  await openDeployPage();
  await chooseTopology("multi");
  await waitFor(() => assert.ok(pool().getByText(errorText("UNIT_PLACEMENT_NONE", placementNone.params))), { timeout: 4000 });
  await screen.findByText(node.autoGroupNoPreview);
});

test("a missing TP is the server's AUTO_PLACEMENT_TP_REQUIRED beside the switch", async () => {
  recordEvaluate(() => ({
    ...baseAnswer, ok: false, auto_placed: true,
    errors: [{ field: "placement_mode", code: "AUTO_PLACEMENT_TP_REQUIRED" }],
  }));
  await openDeployPage();
  await chooseTopology("multi");
  await waitFor(() => assert.ok(pool().getByText(errorText("AUTO_PLACEMENT_TP_REQUIRED"))), { timeout: 4000 });
});

test("switching a multi-node draft to manual shows the node picker and states the nodes", async () => {
  const payloads = recordEvaluate();
  // A rank must advertise an address the group can reach.
  stubFleet([{ ...(nodeOne as object), management_address: "10.0.0.1:9443" }, { ...(nodeTwo as object), management_address: "10.0.0.2:9443" }]);
  await openDeployPage();
  await chooseTopology("multi");
  await screen.findByText(node.multiAutoTitle);
  fireEvent.click(segment(node.manual));
  const picker = (await screen.findByText(node.multiPlaceholder)).closest(".ant-select") as HTMLElement;
  assert.ok(screen.getByText(node.manualUnitsDescription));
  await selectOption(picker, "GPU Node 1 · 2 GPU");
  await selectOption(picker, "GPU Node 2 · 2 GPU");
  await waitFor(() => {
    const body = lastOf(payloads);
    assert.equal(body.placement_mode, "manual");
    assert.deepEqual(body.node_uuids, ["node-1", "node-2"]);
    assert.equal(body.placement_strategy, undefined);
  });
});

test("the strategy follows the topology's default unless the deployer picked one", async () => {
  const payloads = recordEvaluate();
  await openDeployPage();
  const strategy = () => (lastOf(payloads).placement_strategy);
  await waitFor(() => assert.equal(strategy(), "spread"), { timeout: 4000 });
  await chooseTopology("multi");
  await waitFor(() => assert.equal(strategy(), "binpack"));
  await chooseTopology("single");
  await waitFor(() => assert.equal(strategy(), "spread"));
  // An explicit pick survives the switch.
  fireEvent.click(await screen.findByText(node.strategyBinpack));
  await chooseTopology("multi");
  await chooseTopology("single");
  await waitFor(() => assert.equal(strategy(), "binpack"));
});

test("the single-node switch is unchanged: automatic spreads, manual pins one node", async () => {
  const payloads = recordEvaluate();
  await openDeployPage();
  await waitFor(() => assert.ok(payloads.length > 0));
  assert.ok(selected(node.auto));
  assert.ok(screen.getByText(node.autoPoolDescription));
  assert.equal(lastOf(payloads).placement_mode, "auto");
  fireEvent.click(segment(node.manual));
  await screen.findByText(node.manualPoolDescription);
  await waitFor(() => assert.equal(lastOf(payloads).placement_mode, "manual"));
});

test("a manual PD draft with units on different GPU models shows the server's warning", async () => {
  recordEvaluate(() => ({
    ...baseAnswer,
    warnings: [{ code: "PD_GPU_MODELS_DIFFER", params: { models: "H100, L20" } }],
  }));
  await openDeployPage();
  await chooseTopology("pd");
  fireEvent.click(segment(node.manual));
  await screen.findByText(node.manualUnitsDescription);
  await screen.findByText(deployLocale.warning.PD_GPU_MODELS_DIFFER.replace("{{models}}", "H100, L20"));
});

// ---- a clone of an existing multi-node or PD deployment ---------------------

async function openFrom(draft: Record<string, unknown>, deployment: Record<string, unknown>) {
  stub("deploymentDraft", async () => ({ deployment: draft, revision: "r1", locked: [] }));
  stub("deployment", async () => deployment);
  const DeployPage = (await import("../src/pages/DeployPage.tsx")).default;
  render(
    <MemoryRouter initialEntries={["/deploy?from=5"]}>
      <I18nextProvider i18n={i18n}>
        <DeployPage />
      </I18nextProvider>
    </MemoryRouter>,
  );
  await waitFor(() => assert.ok(document.querySelector(".deploy-node-pool")), { timeout: 4000 });
}

const storedMulti = {
  model_id: 3, name: "big", served_name: "big", engine: "vllm", target_vendor: "nvidia", nnodes: 2, replicas: 1,
  node_uuid: "node-1", node_uuids: ["node-1", "node-2"], placement: { tp: 4 },
};
const storedDeployment = { id: 5, name: "big", model_id: 3, model_name: "m", node_uuid: "node-1", desired_state: "active", runtime_state: "running", replicas: 1, nnodes: 2, template_deviated: false, health: {}, created_at: "2026-10-01T00:00:00Z" };

test("a clone of a multi-node deployment the platform places opens on automatic with no picker", async () => {
  recordEvaluate();
  await openFrom(storedMulti, { ...storedDeployment, placement_mode: "auto" });
  await waitFor(() => assert.ok(selected(node.auto)));
  assert.ok(screen.getByText(node.autoUnitsDescription));
  assert.equal(screen.queryByText(node.multiPlaceholder), null);
});

test("a stored draft that does not say who places it reads as manual, with its nodes", async () => {
  const payloads = recordEvaluate();
  stubFleet([{ ...(nodeOne as object), management_address: "10.0.0.1:9443" }, { ...(nodeTwo as object), management_address: "10.0.0.2:9443" }]);
  await openFrom(storedMulti, { ...storedDeployment, placement_mode: "manual" });
  await waitFor(() => assert.ok(selected(node.manual)));
  await screen.findByText("GPU Node 2");
  await waitFor(() => {
    const body = lastOf(payloads);
    assert.equal(body.placement_mode, "manual");
    assert.deepEqual(body.node_uuids, ["node-1", "node-2"]);
  });
});

test("a stored PD draft with no placement_mode reads as manual: its units keep their nodes", async () => {
  const payloads = recordEvaluate();
  const pd = { mode: "vllm-nixl", roles: { decode: { tp: 1, units: [{ node_uuid: "node-1" }] }, prefill: { tp: 1, units: [{ node_uuid: "node-2" }] } } };
  await openFrom({ ...storedMulti, nnodes: 1, node_uuid: "node-1", node_uuids: undefined, placement: { pd } }, { ...storedDeployment, nnodes: 1 });
  await waitFor(() => assert.ok(selected(node.manual)));
  await waitFor(() => {
    const body = lastOf(payloads);
    assert.equal(body.placement_mode, undefined);
    assert.deepEqual(body.placement?.pd?.roles.decode.units, [{ node_uuid: "node-1" }]);
  });
});
