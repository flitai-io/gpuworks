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
import { register } from "node:module";
import { pathToFileURL } from "node:url";
import { list } from "./helpers/listPage.ts";
register(
  pathToFileURL(
    new URL("./deploySleepableCssHook.mjs", import.meta.url).pathname,
  ),
);

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
  fs.readFileSync(
    new URL("../src/i18n/locales/en/deploy.json", import.meta.url),
    "utf8",
  ),
);
const deploymentsLocale = JSON.parse(
  fs.readFileSync(
    new URL("../src/i18n/locales/en/deployments.json", import.meta.url),
    "utf8",
  ),
);
// The yield policy dictionary, as GET /scheduling/yield-policies answers it.
const yieldDictionary = JSON.parse(
  fs.readFileSync(new URL("./fixtures/yield-policies.json", import.meta.url), "utf8"),
);
const commonLocale = JSON.parse(
  fs.readFileSync(
    new URL("../src/i18n/locales/en/common.json", import.meta.url),
    "utf8",
  ),
);
const i18n = createInstance();
await i18n.use(initReactI18next).init({
  lng: "en",
  fallbackLng: "en",
  resources: { en: { deploy: deployLocale, deployments: deploymentsLocale, common: commonLocale } },
  defaultNS: "common",
  interpolation: { escapeValue: false },
});
const { cleanup, fireEvent, render, screen, waitFor } =
  await import("@testing-library/react");
const { MemoryRouter } = await import("react-router-dom");
const { default: userEvent } = await import("@testing-library/user-event");
const { api } = await import("../src/api.ts");
const { yieldPolicyApi } = await import("../src/yieldPolicyApi.ts");

const gpuMemSpec = {
  key: "gpu_memory_utilization",
  flag: "--gpu-memory-utilization",
  type: "float",
  control: "slider",
  group: "resource",
  default: 0.9,
  label: { en: "GPU Memory Utilization", zh: "显存利用率" },
} as never;
const prefixCachingSpec = {
  key: "enable_prefix_caching",
  flag: "--enable-prefix-caching",
  type: "boolean",
  control: "switch",
  group: "features",
  default: true,
  label: { en: "Prefix caching", zh: "前缀缓存" },
} as never;
const cacheReportSpec = {
  key: "enable_cache_report",
  flag: "--enable-prompt-tokens-details",
  type: "boolean",
  control: "switch",
  group: "features",
  default: true,
  label: { en: "Cache Hit Stats", zh: "返回缓存命中统计" },
} as never;
const engineProfile = {
  name: "vllm",
  sleep: { supported: true, levels: ["L1", "L2"] },
  quick_form_keys: [
    "gpu_memory_utilization",
    "enable_prefix_caching",
    "enable_cache_report",
  ],
  param_specs: [gpuMemSpec, prefixCachingSpec, cacheReportSpec],
} as never;

const node = {
  uuid: "node-uuid-1",
  hostname: "gpu3",
  state: "online",
  gpu_count: 1,
  gpu_vendor: "nvidia",
  gpus: [
    {
      index: 0,
      name: "NVIDIA GeForce RTX 2080 Ti",
      vendor: "nvidia",
      schedulable: true,
      arch_id: "Turing",
      compute_capability: "7.5",
    },
  ],
} as never;
const model = {
  id: 3,
  name: "Qwen/Qwen3-0.6B",
  source: "modelscope",
  source_id: "Qwen/Qwen3-0.6B",
  input_modalities: ["text"],
  weight_bytes: 1_500_000_000,
  nodes: [{ node_uuid: "node-uuid-1", state: "present" }],
} as never;

const calls: { evaluate: unknown[]; create: unknown[] } = {
  evaluate: [],
  create: [],
};
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
let modelsFake = stubModels([model]);
stubFleet([node]);
stub("engines", async () => list([engineProfile]));
stub("inferenceTemplateIndex", async () => ({ items: [] }));
stub("imageVersions", async () => list([]));
stub("images", async () => list([]));
let yieldPoliciesFail = false;
yieldPolicyApi.dictionary = async () => {
  if (yieldPoliciesFail) throw new Error("dictionary unavailable");
  return structuredClone(yieldDictionary);
};
afterEach(() => {
  cleanup();
  // Later cases in this file override individual stubs; restore the shared
  // fixtures so every test starts from the same world.
  modelsFake = stubModels([model]);
  apiAny.nodes = async () => list([node]);
  apiAny.engines = async () => list([engineProfile]);
  apiAny.inferenceTemplateIndex = async () => ({ items: [] });
  apiAny.imageVersions = async () => list([]);
  apiAny.images = async () => list([]);
  calls.evaluate.length = 0;
  calls.create.length = 0;
  evaluateOverride = {};
  yieldPoliciesFail = false;
});

async function openDeployPage() {
  const DeployPage = (await import("../src/pages/DeployPage.tsx")).default;
  const user = userEvent.setup({ document: dom.window.document });
  render(
    <MemoryRouter initialEntries={["/deploy?model=3"]}>
      <I18nextProvider i18n={i18n}>
        <DeployPage />
      </I18nextProvider>
    </MemoryRouter>,
  );
  return { user, view: undefined } as const;
}

after(() => {
  closeChannels();
  dom.window.close();
});

const evaluateAnswer = {
  ok: true,
  warnings: [],
  defaults: { model_mount: "/model", master_port: 29500, engine: "vllm" },
  form_values: {},
  image: "runner:vllm",
  command: ["vllm", "serve", "/model"],
  argv: [],
  entrypoint: [],
  env: {},
  injected: [],
  template_match: true,
  eligible_nodes: [{ uuid: "node-uuid-1", name: "gpu3", eligible: true }],
  selected_node: {
    uuid: "node-uuid-1",
    name: "gpu3",
    reasons: [{ code: "auto_selected" }],
  },
  plan: {
    ok: true,
    message: { code: "selected" },
    plan: {
      gpus: [0],
      tp: 1,
      per_gpu_bytes: 2_000_000_000,
      score: 1,
      reasons: [],
    },
    explain: {},
  },
  acquisition: { image: { image: "runner:vllm", cached: true } },
};

// A test may answer with a variant of the evaluation (e.g. another plan).
let evaluateOverride: Record<string, unknown> = {};
stub("evaluate", async (payload: unknown) => {
  calls.evaluate.push(payload);
  // The server says whether the draft may hold a yield policy other than
  // never; for the single-node vllm drafts these cases send, that is whether
  // it asks to sleep (internal/server/api's evaluate test asserts the same).
  const sleepable = (payload as Record<string, unknown>).sleepable === true;
  const yield_policy_eligible = sleepable ? { ok: true } : { ok: false, reason: "SLEEP_NOT_ENABLED" };
  return { ...structuredClone(evaluateAnswer), yield_policy_eligible, ...structuredClone(evaluateOverride) };
});
stub("createDeployment", async (body: unknown) => {
  calls.create.push(body);
  return { id: 99 } as never;
});

async function pickVendorAndName(
  user: ReturnType<typeof userEvent.setup>,
  name: string,
) {
  const vendorPlaceholder = await screen.findByText(
    deployLocale.basic.targetVendorPlaceholder,
  );
  const vendorInput = vendorPlaceholder
    .closest(".ant-select")!
    .querySelector("input")!;
  await user.click(vendorInput);
  await user.click(await screen.findByText(deployLocale.basic.vendor.nvidia));
  await user.type(screen.getByPlaceholderText("my-qwen"), name);
}

test("the automatic mode renders the server's selection and the create confirms it", async () => {
  const { user } = await openDeployPage();
  await pickVendorAndName(user, "auto-sleep");
  // The auto draft states no node and no GPU pinning; the server's
  // selected_node is the answer the card renders.
  await waitFor(
    () => {
      const last = calls.evaluate[calls.evaluate.length - 1] as
        Record<string, unknown> | undefined;
      assert.ok(
        last && last.placement_mode === "auto" && !("node_uuid" in last),
        "the auto draft carries no node",
      );
    },
  );
  await waitFor(
    () =>
      assert.ok(
        screen.getByText(deployLocale.workbench.node.schedulerSelected),
      ),
  );
  // click the sleepable card: on
  const card = screen.getByText(deployLocale.workbench.resource.sleepable);
  await user.click(card);
  await waitFor(() =>
    assert.equal(card.closest("button")!.className.includes("is-active"), true),
  );
  await waitFor(
    () => {
      const last = calls.evaluate[calls.evaluate.length - 1] as
        Record<string, unknown> | undefined;
      assert.ok(
        last && last.sleepable === true,
        "the post-click evaluate must carry sleepable",
      );
    },
  );

  await user.click(
    screen.getByRole("button", {
      name: deployLocale.action.deploy || "Deploy",
    }),
  );
  await waitFor(() => assert.ok(calls.create.length > 0));

  const create = calls.create[0] as Record<string, unknown>;
  assert.equal(create.sleepable, true);
  assert.equal(create.placement_mode, "auto");
  assert.equal(
    create.node_uuid,
    node.uuid,
    "create confirms the server-selected node",
  );
  assert.deepEqual(
    (create.placement as Record<string, unknown>).gpus,
    [0],
    "create confirms the server-selected GPUs",
  );
  assert.equal((create.placement as Record<string, unknown>).tp, 1);
  // ?model= names the model; the page reads it alone and never lists them.
  assert.equal(create.model_id, 3);
  assert.ok(modelsFake.reads.includes(3));
  assert.equal(modelsFake.listedAll, 0);
});

const policyOptionText = (policy: "never" | "idle" | "timeslice" | "anytime") =>
  `${deploymentsLocale.yieldPolicy.policy.name[policy]} - ${deploymentsLocale.yieldPolicy.policy.description[policy]}`;

test("the yield policy is chosen only for a sleepable single-node deployment, takes the dictionary's defaults, and reaches the preview and the create", async () => {
  const label = deployLocale.yieldPolicy.label;
  const { user } = await openDeployPage();
  await pickVendorAndName(user, "tiered");
  await waitFor(() => assert.ok(screen.getByText(deployLocale.workbench.node.schedulerSelected)));
  assert.ok(screen.queryByRole("combobox", { name: label }) === null, "a deployment that cannot sleep has no policy to choose");

  await user.click(screen.getByText(deployLocale.workbench.resource.sleepable));
  const select = await screen.findByRole("combobox", { name: label });
  // The default is never, and never states nothing.
  assert.ok(screen.getAllByText(deploymentsLocale.yieldPolicy.policy.name.never).length > 0);
  await waitFor(() => assert.equal((calls.evaluate.at(-1) as Record<string, unknown>).sleepable, true));
  assert.equal("yield_policy" in (calls.evaluate.at(-1) as Record<string, unknown>), false);
  await screen.findByText(deployLocale.yieldPolicy.help);

  await user.click(select);
  await user.click(await screen.findByText(policyOptionText("timeslice")));
  const timeslice = { policy: "timeslice", idle_sec: 30, sleep_level: "auto", max_serve_sec: 120, unfinished: "wait", unfinished_wait_sec: 30, hold_max_sec: 600 };
  await waitFor(() => assert.deepEqual((calls.evaluate.at(-1) as Record<string, unknown>).yield_policy, timeslice), { timeout: 4000 });

  await user.click(screen.getByRole("button", { name: deployLocale.action.deploy || "Deploy" }));
  await waitFor(() => assert.ok(calls.create.length > 0));
  assert.deepEqual((calls.create[0] as Record<string, unknown>).yield_policy, timeslice);
  assert.equal("service_level" in (calls.create[0] as Record<string, unknown>), false);
  assert.equal((calls.create[0] as Record<string, unknown>).sleepable, true);
});

test("the policy field follows the server's answer, not the form's own fields: a sleepable draft the server refuses offers none", async () => {
  evaluateOverride = { yield_policy_eligible: { ok: false, reason: "YIELD_POLICY_SHAPE" } };
  const label = deployLocale.yieldPolicy.label;
  const { user } = await openDeployPage();
  await pickVendorAndName(user, "refused-shape");
  await waitFor(() => assert.ok(screen.getByText(deployLocale.workbench.node.schedulerSelected)));
  await user.click(screen.getByText(deployLocale.workbench.resource.sleepable));
  await waitFor(() => assert.equal((calls.evaluate.at(-1) as Record<string, unknown>).sleepable, true));
  // The stub answers at once: let that answer render, then look. (Without the
  // override the same steps show the field - the case above.)
  for (let tick = 0; tick < 5; tick++) await new Promise((resolve) => setTimeout(resolve, 0));
  assert.equal(screen.queryByRole("combobox", { name: label }), null);
});

test("choosing never again states no yield policy, and a dictionary that cannot be read leaves only never", async () => {
  yieldPoliciesFail = true;
  const label = deployLocale.yieldPolicy.label;
  const { user } = await openDeployPage();
  await pickVendorAndName(user, "no-dictionary");
  await waitFor(() => assert.ok(screen.getByText(deployLocale.workbench.node.schedulerSelected)));
  await user.click(screen.getByText(deployLocale.workbench.resource.sleepable));
  await screen.findByRole("combobox", { name: label });
  await screen.findByText(deployLocale.yieldPolicy.loadFailed.replace("{{detail}}", "dictionary unavailable"));
  await user.click(screen.getByRole("combobox", { name: label }));
  assert.ok(screen.queryByText(policyOptionText("idle")) === null, "no options without a dictionary");
  await user.keyboard("{Escape}");
  await user.click(screen.getByRole("button", { name: deployLocale.action.deploy || "Deploy" }));
  await waitFor(() => assert.ok(calls.create.length > 0));
  assert.equal("yield_policy" in (calls.create[0] as Record<string, unknown>), false);
});

test("turning sleep off keeps the chosen policy with the server's reason, until it is set back to never", async () => {
  const label = deployLocale.yieldPolicy.label;
  const { user } = await openDeployPage();
  await pickVendorAndName(user, "untiered");
  await waitFor(() => assert.ok(screen.getByText(deployLocale.workbench.node.schedulerSelected)));
  const card = screen.getByText(deployLocale.workbench.resource.sleepable);
  await user.click(card);
  await user.click(await screen.findByRole("combobox", { name: label }));
  await user.click(await screen.findByText(policyOptionText("anytime")));
  const anytime = { policy: "anytime", idle_sec: 30, sleep_level: "auto", unfinished: "abort", unfinished_wait_sec: 30, hold_max_sec: 3600 };
  await waitFor(() => assert.deepEqual((calls.evaluate.at(-1) as Record<string, unknown>).yield_policy, anytime), { timeout: 4000 });

  // Sleep off: the form sends what was chosen and shows why the server will
  // not take it; only never is left to choose.
  await user.click(card);
  await screen.findByText(commonLocale.apiError.SLEEP_NOT_ENABLED, {}, { timeout: 4000 });
  assert.deepEqual((calls.evaluate.at(-1) as Record<string, unknown>).yield_policy, anytime);
  await user.click(screen.getByRole("combobox", { name: label }));
  const option = (policy: "never" | "idle") => screen.getByText(policyOptionText(policy)).closest(".ant-select-item") as HTMLElement;
  assert.ok(option("idle").className.includes("ant-select-item-option-disabled"));
  await user.click(option("never"));
  await waitFor(() => assert.ok(screen.queryByRole("combobox", { name: label }) === null));
  await waitFor(() => assert.equal("yield_policy" in (calls.evaluate.at(-1) as Record<string, unknown>), false), { timeout: 4000 });
  await user.click(screen.getByRole("button", { name: deployLocale.action.deploy || "Deploy" }));
  await waitFor(() => assert.ok(calls.create.length > 0));
  assert.equal("yield_policy" in (calls.create[0] as Record<string, unknown>), false);
  assert.equal("sleepable" in (calls.create[0] as Record<string, unknown>), false);
});
