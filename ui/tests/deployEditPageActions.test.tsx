import assert from "node:assert/strict";
import fs from "node:fs";
import { register } from "node:module";
import { pathToFileURL } from "node:url";
import { after, afterEach, test } from "node:test";
import { JSDOM } from "jsdom";
import React from "react";
import { createInstance } from "i18next";
import { I18nextProvider, initReactI18next } from "react-i18next";
import { fakeFleet } from "./helpers/fakeFleet.ts";
import { fakeModels } from "./helpers/fakeModels.ts";
import { list } from "./helpers/listPage.ts";
import { trackMessageChannels } from "./helpers/trackedMessageChannels.ts";

register(pathToFileURL(new URL("./deploySleepableCssHook.mjs", import.meta.url).pathname));

// Editing a deployment reuses the create page: the draft the server reads back
// fills the form, the locked controls stay as they are, and the save goes
// through a dry-run plan the deployer confirms. These cases drive the page
// through its real components against faithful api stubs.

const closeChannels = trackMessageChannels();
const dom = new JSDOM("<!doctype html><html><body></body></html>", { url: "http://localhost/" });
for (const name of [
  "window", "document", "navigator", "HTMLElement", "HTMLInputElement", "HTMLTextAreaElement", "SVGElement",
  "Element", "Node", "MutationObserver", "ShadowRoot", "KeyboardEvent", "Document", "HTMLBodyElement",
] as const) {
  Object.defineProperty(globalThis, name, { value: dom.window[name], configurable: true });
}
Object.defineProperty(globalThis, "getComputedStyle", {
  value: (element: Element) => dom.window.getComputedStyle(element),
  configurable: true,
});
Object.defineProperty(dom.window, "matchMedia", {
  configurable: true,
  value: () => ({
    matches: false, addListener() {}, removeListener() {}, addEventListener() {}, removeEventListener() {},
    dispatchEvent: () => false,
  }),
});
Object.defineProperty(dom.window, "CSS", { value: { escape: (text: string) => text.replace(/([^\w-])/g, "\\$1") }, configurable: true });
class ResizeObserverStub { observe() {} unobserve() {} disconnect() {} }
Object.defineProperty(globalThis, "ResizeObserver", { value: ResizeObserverStub, configurable: true });

const read = (lang: string, ns: string) =>
  JSON.parse(fs.readFileSync(new URL(`../src/i18n/locales/${lang}/${ns}.json`, import.meta.url), "utf8"));
const deployLocale = read("en", "deploy");
const i18n = createInstance();
await i18n.use(initReactI18next).init({
  lng: "en", fallbackLng: "en",
  resources: { en: { deploy: deployLocale, common: read("en", "common"), deployments: read("en", "deployments") } },
  defaultNS: "common", interpolation: { escapeValue: false },
});
const { cleanup, render, screen, waitFor, within, act } = await import("@testing-library/react");
const { MemoryRouter, Route, Routes, useLocation } = await import("react-router-dom");
const { default: userEvent } = await import("@testing-library/user-event");
const { ConfigProvider, message } = await import("antd");
const { api } = await import("../src/api.ts");
const { APIError } = await import("../src/apiError.ts");
const { default: DeployPage } = await import("../src/pages/DeployPage.tsx");

const gpuMemSpec = {
  key: "gpu_memory_utilization", flag: "--gpu-memory-utilization", type: "float", control: "slider",
  group: "resource", default: 0.9, label: { en: "GPU Memory Utilization", zh: "显存利用率" },
} as never;
const prefixSpec = {
  key: "enable_prefix_caching", flag: "--enable-prefix-caching", type: "boolean", control: "switch",
  group: "features", default: true, label: { en: "Prefix caching", zh: "前缀缓存" },
} as never;
const maxSeqsSpec = {
  key: "max_num_seqs", flag: "--max-num-seqs", type: "integer", control: "number",
  group: "performance", label: { en: "Max sequences", zh: "最大并发序列数" },
} as never;
const engineProfile = {
  name: "vllm", sleep: { supported: true, levels: ["L1", "L2"] },
  quick_form_keys: ["gpu_memory_utilization", "enable_prefix_caching", "max_num_seqs"],
  param_specs: [gpuMemSpec, prefixSpec, maxSeqsSpec],
} as never;
const node = {
  uuid: "node-uuid-1", hostname: "gpu3", state: "online", gpu_count: 1, gpu_vendor: "nvidia",
  gpus: [{ index: 0, name: "NVIDIA GeForce RTX 2080 Ti", vendor: "nvidia", schedulable: true }],
} as never;
const model = {
  id: 3, name: "Qwen/Qwen3-0.6B", source: "modelscope", source_id: "Qwen/Qwen3-0.6B",
  input_modalities: ["text"], weight_bytes: 1_500_000_000, operations: [{ type: "openai.chat", endpoint: "/v1/chat/completions" }],
  nodes: [{ node_uuid: "node-uuid-1", state: "present" }],
} as never;

// The draft the server answers for a manual single-node deployment.
const draft = () => ({
  deployment: {
    model_id: 3, name: "qwen-prod", served_name: "qwen-served", engine: "vllm", image: "example/vllm:test",
    target_vendor: "nvidia", placement_mode: "manual", node_uuid: "node-uuid-1", replicas: 2, nnodes: 1,
    routing_policy: "round_robin", extra_args: "", sleepable: false, health_check: true, tokenize_enabled: true,
    placement_strategy: "spread", vram_limit_gib: 0,
    quick_params: { gpu_memory_utilization: 0.8 }, placement: { tp: 1, gpus: [0] },
  },
  revision: "rev-1",
  locked: [
    "model_id", "template_id", "template_spec_source", "template_spec_name", "scenario", "engine",
    "target_vendor", "name", "served_name", "model_source", "nnodes",
  ],
});

const apiAny = api as unknown as Record<string, unknown>;
const originals: Record<string, unknown> = {};
function stub(name: string, value: unknown) {
  if (!(name in originals)) originals[name] = apiAny[name];
  apiAny[name] = value;
}
const calls = {
  evaluate: [] as Array<Record<string, unknown>>,
  drafts: 0,
  dryRuns: [] as Array<{ id: number; body: Record<string, any> }>,
  saves: [] as Array<{ id: number; body: Record<string, any> }>,
  creates: [] as Array<Record<string, unknown>>,
};
let evaluateForm: Record<string, unknown> = {};
let draftAnswer: () => Promise<unknown> = async () => draft();
let planAnswer: () => Promise<unknown>;
let saveAnswer: () => Promise<unknown>;

const plan = (over: Record<string, unknown> = {}) => ({
  changes: [{ field: "replicas", layer: "live", from: "2", to: "3" }],
  restart: false, apply_options: [], default_apply: "none", can_start: false, can_wake: false,
  revision: "rev-1", ...over,
});

function reset() {
  calls.evaluate.length = 0; calls.dryRuns.length = 0; calls.saves.length = 0; calls.creates.length = 0;
  calls.drafts = 0; evaluateForm = {};
  draftAnswer = async () => draft();
  planAnswer = async () => plan();
  saveAnswer = async () => ({ plan: plan(), deployment: { id: 5 } });
  const fleet = fakeFleet([node] as never);
  for (const name of ["nodes", "nodesPage", "node", "nodeFacets"] as const) stub(name, fleet[name]);
  const models = fakeModels([model] as never);
  stub("models", models.models_all);
  for (const name of ["modelsPage", "model"] as const) stub(name, models[name]);
  stub("engines", async () => list([engineProfile]));
  stub("inferenceTemplateIndex", async () => ({ items: [] }));
  stub("imageVersions", async () => list([]));
  stub("images", async () => list([]));
  stub("deploymentDraft", async () => { calls.drafts++; return draftAnswer(); });
  stub("deployment", async () => ({ id: 5, name: "qwen-prod", runtime_state: "running", desired_state: "active" }));
  stub("editDeployment", async (id: number, body: Record<string, any>, opts?: { dryRun?: boolean }) => {
    if (opts?.dryRun) { calls.dryRuns.push({ id, body }); return planAnswer(); }
    calls.saves.push({ id, body });
    return saveAnswer();
  });
  stub("createDeployment", async (body: Record<string, unknown>) => { calls.creates.push(body); return { id: 99 }; });
  stub("evaluate", async (payload: Record<string, unknown>) => {
    calls.evaluate.push(payload);
    return {
      ok: true, warnings: [], defaults: { engine: "vllm" }, form_values: evaluateForm, image: "example/vllm:test",
      command: ["vllm", "serve", "/model"], argv: [], entrypoint: [], env: {}, injected: [], template_match: true,
      eligible_nodes: [{ uuid: "node-uuid-1", name: "gpu3", eligible: true }],
      selected_node: { uuid: "node-uuid-1", name: "gpu3", reasons: [] },
      plan: { ok: true, message: { code: "selected" }, plan: { gpus: [0], tp: 1, per_gpu_bytes: 1, score: 1, reasons: [] }, explain: {} },
      acquisition: { image: { image: "example/vllm:test", cached: true } },
    };
  });
}
reset();
afterEach(() => { cleanup(); message.destroy(); reset(); });
after(() => {
  for (const [name, value] of Object.entries(originals)) apiAny[name] = value;
  closeChannels();
  dom.window.close();
});

function Probe() {
  const location = useLocation();
  return <div data-testid="route">{location.pathname + location.search}</div>;
}

async function open(url: string) {
  const user = userEvent.setup({ document: dom.window.document });
  render(
    <MemoryRouter initialEntries={[url]}>
      <I18nextProvider i18n={i18n}>
        {/* jsdom fires no transitionend, so a leaving dialog would never leave */}
        <ConfigProvider theme={{ token: { motion: false } }}>
          <Routes>
            <Route path="/deploy" element={<DeployPage />} />
            <Route path="*" element={<Probe />} />
          </Routes>
        </ConfigProvider>
      </I18nextProvider>
    </MemoryRouter>,
  );
  return user;
}

const reviewButton = () => screen.getByRole("button", { name: deployLocale.edit.review }) as HTMLButtonElement;
const nameInput = () => screen.getByPlaceholderText("my-qwen") as HTMLInputElement;
const servedInput = () => screen.getByLabelText(deployLocale.basic.servedName) as HTMLInputElement;
const replicasInput = () => screen.getByLabelText(deployLocale.catalog.replicas) as HTMLInputElement;

async function loaded(url = "/deploy?edit=5") {
  const user = await open(url);
  await screen.findByPlaceholderText("my-qwen");
  return user;
}

const prefixPosition = (label = "Prefix caching") =>
  (screen.getAllByLabelText(label)[0].closest(".ant-select") as HTMLElement).querySelector(".ant-select-selection-item")?.textContent;

test("a refused save stays in the dialog beside the choice that met it", async () => {
  const user = await loaded();
  saveAnswer = async () => { throw new APIError("DEPLOYMENT_EDIT_APPLY_INVALID", { apply: "all", options: "none" }, "bad apply", 400); };
  await user.clear(replicasInput());
  await user.type(replicasInput(), "3");
  await waitFor(() => assert.equal(reviewButton().disabled, false));
  await user.click(reviewButton());
  const dialog = await screen.findByRole("dialog");
  await user.click(within(dialog).getByRole("button", { name: deployLocale.edit.ok.save }));
  await within(dialog).findByText(/cannot be applied as all/);
  assert.ok(screen.queryByTestId("route") === null, "the page did not leave");
});

test("a deployment changed under the editor reloads and keeps the deployer's own edits", async () => {
  const user = await loaded();
  const newer = draft();
  newer.revision = "rev-2";
  newer.deployment.quick_params = { gpu_memory_utilization: 0.7 };
  newer.deployment.routing_policy = "least_load";
  planAnswer = async () => { throw new APIError("DEPLOYMENT_CHANGED", { revision: "rev-2" }, "changed", 409); };
  await user.clear(replicasInput());
  await user.type(replicasInput(), "5");
  await waitFor(() => assert.equal(reviewButton().disabled, false));
  await user.click(reviewButton());
  const dialog = await screen.findByRole("dialog");
  await within(dialog).findByText(deployLocale.edit.confirm.conflict);
  assert.ok(within(dialog).queryByRole("button", { name: deployLocale.edit.ok.save }) === null);

  draftAnswer = async () => newer;
  planAnswer = async () => plan({ changes: [{ field: "replicas", layer: "live", from: "2", to: "5" }], revision: "rev-2" });
  await user.click(within(dialog).getByRole("button", { name: deployLocale.edit.confirm.reload }));
  await waitFor(() => assert.ok(screen.queryByRole("dialog") === null, `the dialog closes: dryRuns=${calls.dryRuns.length} drafts=${calls.drafts} text=${document.querySelector(".ant-modal-root")?.textContent}`));
  assert.equal(calls.drafts, 2);
  assert.equal(replicasInput().value, "5", "the deployer's edit survives the reload");

  await waitFor(() => assert.equal(reviewButton().disabled, false));
  await user.click(reviewButton());
  await screen.findByRole("dialog");
  const next = calls.dryRuns.at(-1)!.body;
  assert.equal(next.base_revision, "rev-2", "the new draft's revision is the base now");
  assert.equal(next.deployment.replicas, 5);
  assert.equal(next.deployment.routing_policy, "least_load", "what someone else changed is picked up");
  assert.deepEqual(next.deployment.quick_params, { gpu_memory_utilization: 0.7 });
});

test("the review button and the dialog work from the keyboard", async () => {
  const user = await loaded();
  planAnswer = async () => plan({ restart: true, apply_options: ["rolling", "all", "none"], default_apply: "rolling" });
  await user.clear(replicasInput());
  await user.type(replicasInput(), "3");
  await waitFor(() => assert.equal(reviewButton().disabled, false));
  reviewButton().focus();
  await user.keyboard("{Enter}");
  const dialog = await screen.findByRole("dialog");
  assert.equal(calls.dryRuns.length, 1);
  // the arrow keys move the apply choice, Enter on a button presses it
  const rolling = within(dialog).getByRole("radio", { name: new RegExp(deployLocale.edit.apply.rolling.label) }) as HTMLInputElement;
  rolling.focus();
  await user.keyboard("{ArrowDown}");
  const all = within(dialog).getByRole("radio", { name: new RegExp(deployLocale.edit.apply.all.label) }) as HTMLInputElement;
  assert.equal(all.checked, true);
  const cancel = within(dialog).getByRole("button", { name: deployLocale.edit.confirm.cancel });
  cancel.focus();
  await user.keyboard("{Enter}");
  await waitFor(() => assert.ok(screen.queryByRole("dialog") === null, "Cancel closes the dialog"));
  assert.equal(calls.saves.length, 0, "closing the dialog saves nothing");
});

test("a validation error of the dry run is reported and the form stays", async () => {
  const user = await loaded();
  planAnswer = async () => { throw new APIError("DEPLOYMENT_FIELD_NOT_EDITABLE", { field: "name" }, "cannot change", 400); };
  await user.clear(replicasInput());
  await user.type(replicasInput(), "3");
  await waitFor(() => assert.equal(reviewButton().disabled, false));
  await user.click(reviewButton());
  await screen.findByText(/name cannot change on an existing deployment/);
  assert.ok(screen.queryByRole("dialog") === null);
});

test("clone and modify fills the same form, locks nothing and creates a new deployment", async () => {
  const user = await loaded("/deploy?from=5");
  assert.ok(screen.getByText("Clone deployment: qwen-prod"));
  assert.equal(nameInput().value, "qwen-prod-clone");
  assert.equal(servedInput().value, "qwen-served-clone");
  assert.equal(document.querySelectorAll("fieldset.deploy-locked-group").length, 0);
  await waitFor(() => assert.ok(calls.evaluate.length > 0));
  assert.ok(!("deployment_id" in calls.evaluate[0]), "a new deployment's preview keeps every GPU's use");
  await waitFor(() => assert.equal((screen.getByRole("button", { name: deployLocale.page.deploy }) as HTMLButtonElement).disabled, false));
  await user.click(screen.getByRole("button", { name: deployLocale.page.deploy }));
  await waitFor(() => assert.equal(calls.creates.length, 1));
  assert.equal(calls.creates[0].name, "qwen-prod-clone");
  assert.equal(calls.creates[0].served_name, "qwen-served-clone");
  assert.equal(calls.creates[0].replicas, 2);
  assert.equal(calls.saves.length, 0);
  assert.equal(calls.dryRuns.length, 0);
});
