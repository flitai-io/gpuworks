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

test("the form shows a loading state while the draft is read", async () => {
  let release: (value: unknown) => void = () => {};
  draftAnswer = () => new Promise((resolve) => { release = resolve; });
  await open("/deploy?edit=5");
  await screen.findByText(deployLocale.edit.loading);
  assert.ok(screen.queryByPlaceholderText("my-qwen") === null);
  assert.equal(calls.evaluate.length, 0, "nothing is evaluated before the draft is in");
  await act(async () => { release(draft()); });
  await screen.findByPlaceholderText("my-qwen");
  assert.ok(screen.queryByText(deployLocale.edit.loading) === null);
});

test("a deployment that is gone shows an error with a way back to the list", async () => {
  draftAnswer = async () => { throw new APIError("NOT_FOUND", {}, "deployment not found", 404); };
  const user = await open("/deploy?edit=5");
  await screen.findByText(deployLocale.edit.notFound);
  await user.click(screen.getByRole("button", { name: deployLocale.edit.backToList }));
  await waitFor(() => assert.equal(screen.getByTestId("route").textContent, "/deployments"));
});

test("any other read failure is an error with the way back too", async () => {
  draftAnswer = async () => { throw new Error("boom"); };
  await open("/deploy?edit=5");
  await screen.findByText(deployLocale.edit.loadFailed);
  assert.ok(screen.getByText("boom"));
  assert.ok(screen.getByRole("button", { name: deployLocale.edit.backToList }));
});

test("the draft fills the form, and the first evaluation cannot replace what it loaded", async () => {
  // The answer is built from dictionary defaults and the template: values
  // that differ from what the deployment actually runs.
  evaluateForm = { replicas: 9, gpu_memory_utilization: 0.1, max_num_seqs: 256, enable_prefix_caching: true, name: "other", served_name: "other" };
  await loaded();
  await waitFor(() => assert.ok(calls.evaluate.length > 0));
  // let the answer land
  await act(async () => { await new Promise((resolve) => setTimeout(resolve, 100)); });
  assert.equal(replicasInput().value, "2");
  assert.equal(nameInput().value, "qwen-prod");
  assert.equal(servedInput().value, "qwen-served");
  const first = calls.evaluate[0];
  assert.equal(first.deployment_id, 5, "the preview leaves the deployment's own GPU use out");
  assert.equal(first.placement_mode, "manual");
  assert.equal(first.node_uuid, "node-uuid-1");
  assert.equal(first.name, "qwen-prod");
  assert.deepEqual(first.quick_params, { gpu_memory_utilization: 0.8 });
  assert.deepEqual(first.quick_params_unset, ["enable_prefix_caching", "max_num_seqs"], "keys the deployment lacks get no default");
  assert.equal(first.image, "example/vllm:test");
  assert.equal(calls.drafts, 1, "the draft is read once");
});

const prefixPosition = (label = "Prefix caching") =>
  (screen.getAllByLabelText(label)[0].closest(".ant-select") as HTMLElement).querySelector(".ant-select-selection-item")?.textContent;

test("a launch key the deployment stated shows as stated", async () => {
  const request = draft();
  request.deployment.quick_params = { gpu_memory_utilization: 0.8, enable_prefix_caching: false };
  draftAnswer = async () => request;
  await loaded();
  await waitFor(() => assert.equal(prefixPosition(), "Off"));
});

test("a launch key the deployment never stated shows as its unstated position", async () => {
  await loaded();
  await waitFor(() => assert.equal(prefixPosition(), "On"));
});

test("a template's own defaults never replace what the deployment runs", async () => {
  const request = draft();
  Object.assign(request.deployment, {
    template_id: 11, template_spec_source: "upstream", template_spec_name: "a",
    container_overrides: { env: { KEEP: "1" } },
  });
  draftAnswer = async () => request;
  const spec = {
    id: 1, source: "upstream", name: "a", engine: "vllm", image: "template/image:1", gpus: { vendor: "nvidia" },
    weight_sources: [{ provider: "modelscope", repo_id: "Qwen/Qwen3-0.6B" }], env: { FROM_TEMPLATE: "1" }, args: [],
  };
  stub("inferenceTemplateIndex", async () => ({ items: [{ id: 11, model_id: "Qwen/Qwen3-0.6B", profiles: [] }] }));
  stub("inferenceTemplate", async () => ({ id: 11, model_id: "Qwen/Qwen3-0.6B", specs: [spec] }));
  await loaded();
  // the template is loaded and shown...
  await waitFor(() => assert.ok(document.querySelector(".deploy-template-lockbar")));
  await waitFor(() => assert.ok(calls.evaluate.length > 0));
  await act(async () => { await new Promise((resolve) => setTimeout(resolve, 300)); });
  // ...yet it neither asks which model serves it nor writes its image and environment
  assert.ok(screen.queryByText(deployLocale.weightSourceModal.confirmTitle) === null);
  const last = calls.evaluate.at(-1)!;
  assert.equal(last.image, "example/vllm:test");
  assert.deepEqual(last.container_overrides, { env: { KEEP: "1" } });
  assert.equal(last.template_id, 11);
});

test("focus names the controls to bring into view", async () => {
  await loaded("/deploy?edit=5&focus=replicas,not_a_field");
  await waitFor(() => assert.ok(document.getElementById("replicas")?.closest(".ant-form-item")?.classList.contains("deploy-edit-highlight")));
});

test("the header names the deployment and its state; what it is stays locked", async () => {
  await loaded();
  assert.ok(screen.getByText("Edit deployment: qwen-prod"));
  await screen.findByText("Running");
  assert.equal(screen.getByText(deployLocale.edit.back).closest("a")?.getAttribute("href"), "/deployments?deployment=5");
  // identity controls are inside a disabled group, with the way to change them
  assert.ok(nameInput().closest("fieldset.deploy-locked-group")?.disabled, "the name is locked");
  assert.ok(servedInput().closest("fieldset.deploy-locked-group")?.disabled);
  const clone = screen.getAllByText(deployLocale.edit.lockedClone)[0].closest("a");
  assert.equal(clone?.getAttribute("href"), "/deploy?from=5");
  // replicas of a single-node deployment is editable
  assert.ok(replicasInput().closest("fieldset.deploy-locked-group") === null);
  assert.equal(replicasInput().disabled, false);
});

test("a multi-node draft also locks its replicas and nodes, as the server lists", async () => {
  draftAnswer = async () => {
    const answer = draft();
    answer.deployment = { ...answer.deployment, nnodes: 2, node_uuid: "node-uuid-1", node_uuids: ["node-uuid-1", "node-uuid-2"], placement_mode: "", replicas: 1 } as never;
    answer.locked = [...answer.locked, "node_uuids", "replicas", "placement_mode", "node_requirements", "placement_strategy"];
    return answer;
  };
  await loaded();
  assert.equal(replicasInput().disabled, true);
});

test("review changes waits for a change, shows the plan, and the save goes to the server", async () => {
  const user = await loaded();
  await waitFor(() => assert.ok(calls.evaluate.length > 0));
  await waitFor(() => assert.equal(reviewButton().disabled, true));
  assert.ok(screen.getByText("0 changes"));
  planAnswer = async () => plan({
    changes: [{ field: "replicas", layer: "live", from: "2", to: "3" }, { field: "container_overrides.env.TOKEN", layer: "restart", from: "", to: "", masked: true }],
    restart: true, apply_options: ["rolling", "all", "none"], default_apply: "rolling",
  });
  await user.clear(replicasInput());
  await user.type(replicasInput(), "3");
  await waitFor(() => assert.ok(screen.getByText("1 change")));
  await waitFor(() => assert.equal(reviewButton().disabled, false));
  await user.click(reviewButton());

  const dialog = await screen.findByRole("dialog");
  assert.equal(calls.dryRuns.length, 1);
  assert.equal(calls.dryRuns[0].id, 5);
  assert.equal(calls.dryRuns[0].body.base_revision, "rev-1");
  assert.equal(calls.dryRuns[0].body.deployment.replicas, 3);
  assert.equal(calls.dryRuns[0].body.deployment.name, "qwen-prod");
  assert.ok(!("apply" in calls.dryRuns[0].body));
  assert.ok(within(dialog).getByText("replicas"));
  assert.ok(within(dialog).getByText(deployLocale.edit.layer.live));
  assert.ok(within(dialog).getByText(deployLocale.edit.layer.restart));
  assert.equal(within(dialog).getAllByText("••••").length, 2, "a masked value shows neither side");
  const rolling = within(dialog).getByRole("radio", { name: new RegExp(deployLocale.edit.apply.rolling.label) }) as HTMLInputElement;
  assert.equal(rolling.checked, true, "the server's default is preselected");
  await user.click(within(dialog).getByRole("radio", { name: new RegExp(deployLocale.edit.apply.all.label) }));
  await user.click(within(dialog).getByRole("button", { name: deployLocale.edit.ok.all }));

  await waitFor(() => assert.equal(calls.saves.length, 1));
  assert.equal(calls.saves[0].body.apply, "all");
  assert.equal(calls.saves[0].body.base_revision, "rev-1");
  assert.ok(!("wake" in calls.saves[0].body));
  await waitFor(() => assert.equal(screen.getByTestId("route").textContent, "/deployments?deployment=5"));
});

test("an edit that restarts nothing offers no way to apply and saves with a plain Save", async () => {
  const user = await loaded();
  await user.clear(replicasInput());
  await user.type(replicasInput(), "3");
  await waitFor(() => assert.equal(reviewButton().disabled, false));
  await user.click(reviewButton());
  const dialog = await screen.findByRole("dialog");
  assert.equal(within(dialog).queryAllByRole("radio").length, 0);
  await user.click(within(dialog).getByRole("button", { name: deployLocale.edit.ok.save }));
  await waitFor(() => assert.equal(calls.saves.length, 1));
  assert.ok(!("apply" in calls.saves[0].body), "no apply is sent where none is offered");
});
