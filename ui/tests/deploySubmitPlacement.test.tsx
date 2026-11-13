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

const policyOptionText = (policy: "never" | "idle" | "timeslice" | "anytime") =>
  `${deploymentsLocale.yieldPolicy.policy.name[policy]} - ${deploymentsLocale.yieldPolicy.policy.description[policy]}`;

test("TP and PP typed in automatic mode reach evaluate, and the create confirms the effective TP", async () => {
  const { user } = await openDeployPage();
  await pickVendorAndName(user, "auto-parallel");
  await waitFor(() => assert.ok(screen.getByText(deployLocale.workbench.node.schedulerSelected)));
  const input = (name: string) => document.querySelector(`input[id$="${name}"]`) as HTMLInputElement;
  // PP 2 with TP left automatic: the server groups two GPUs, one TP per
  // stage, and reports the GPU group as plan.tp.
  evaluateOverride = { plan: { ...evaluateAnswer.plan, plan: { ...evaluateAnswer.plan.plan, gpus: [0, 1], tp: 2 } }, parallelism: { tp: 1, pp: 2, nodes: 1, gpus_per_node: 2 } };
  await user.type(input("pp"), "2");
  await waitFor(() => {
    const last = calls.evaluate.at(-1) as Record<string, unknown>;
    assert.deepEqual(last.placement, { pp: 2 });
  });
  await user.type(input("tp"), "1");
  await waitFor(() => {
    const last = calls.evaluate.at(-1) as Record<string, unknown>;
    assert.equal(last.placement_mode, "auto");
    assert.deepEqual(last.placement, { tp: 1, pp: 2 }, "the typed TP is not dropped in automatic mode");
  });
  await waitFor(() => assert.ok(screen.getByText(deployLocale.workbench.node.schedulerSelected)));
  await user.click(screen.getByRole("button", { name: deployLocale.action.deploy || "Deploy" }));
  await waitFor(() => assert.ok(calls.create.length > 0));
  const placement = (calls.create[0] as Record<string, unknown>).placement as Record<string, unknown>;
  assert.deepEqual(placement, { tp: 1, pp: 2, gpus: [0, 1] }, "plan.tp is the GPU group, not the TP");
});

test("switching to manual starts from an empty pick, not the automatic placement", async () => {
  const { user } = await openDeployPage();
  await pickVendorAndName(user, "inherit-check");
  await waitFor(
    () => {
      const last = calls.evaluate[calls.evaluate.length - 1] as
        Record<string, unknown> | undefined;
      assert.ok(last && last.placement_mode === "auto");
    },
  );
  await waitFor(
    () =>
      assert.ok(
        screen.getByText(deployLocale.workbench.node.schedulerSelected),
      ),
  );
  // Switch to manual: the server's placement is not inherited.
  await user.click(screen.getByText(deployLocale.workbench.node.manual));
  await waitFor(
    () => assert.ok(screen.getByText(deployLocale.workbench.node.placeholder)),
  );
  await waitFor(
    () => {
      const last = calls.evaluate[calls.evaluate.length - 1] as
        Record<string, unknown> | undefined;
      const placement = (last?.placement ?? {}) as Record<string, unknown>;
      assert.ok(
        last &&
          last.placement_mode === "manual" &&
          !("gpus" in placement) &&
          !("tp" in placement),
        "manual carries no inherited GPU/tp",
      );
    },
  );
});

test("an evaluate response from the previous draft cannot overwrite a changed form", async () => {
  const pending: Array<{
    payload: unknown;
    resolve: (value: unknown) => void;
  }> = [];
  stubModels([model]);
  apiAny.nodes = async () => list([node]);
  apiAny.engines = async () => list([engineProfile]);
  apiAny.inferenceTemplateIndex = async () => ({ items: [] });
  apiAny.imageVersions = async () => list([]);
  apiAny.images = async () => list([]);
  apiAny.evaluate = (payload: unknown) =>
    new Promise((resolve) => {
      pending.push({ payload, resolve });
      if (pending.length > 1)
        resolve({
          ok: false,
          warnings: [],
          skipped: true,
          defaults: evaluateAnswer.defaults,
          form_values: {},
        });
    });
  const { user } = await openDeployPage();
  await pickVendorAndName(user, "race");
  await waitFor(() => assert.ok(pending.length >= 1));

  // The first request is now in flight. A form edit during the debounce window
  // advances the draft revision before the next request is even sent. Typing
  // does not reach rc-field-form in this jsdom harness (the quirk the flow
  // tests document), so the edit is a real store change: switching the node
  // selection mode.
  await user.click(screen.getByText(deployLocale.workbench.node.manual));
  pending[0].resolve({
    ...structuredClone(evaluateAnswer),
    form_values: { enable_prefix_caching: false, gpu_memory_utilization: 0.55 },
  });
  await Promise.resolve();
  // If the stale result were accepted, its form_values would flip the prefix
  // card to Off and the slider to 0.55; the guard rejects it.
  await waitFor(
    () => assert.ok(screen.getAllByLabelText("Prefix caching").length > 0),
  );
  const prefixCard = screen
    .getAllByLabelText("Prefix caching")
    .find((element) => element.classList.contains("ant-select"))
    ?.closest(".deploy-feature-card") as HTMLElement;
  assert.notEqual(
    prefixCard.querySelector(".ant-select-selection-item")?.textContent ?? "",
    "Off",
    "stale display instructions do not replace the current draft",
  );
  const slider = screen.queryByRole("slider") as HTMLInputElement | null;
  assert.notEqual(
    slider?.getAttribute("aria-valuenow") ?? "",
    "0.55",
    "the stale marker value never lands",
  );
});

test("switching target to AMD requests ROCm capability and clears sleep selection", async () => {
  const amdNode = {
    ...node,
    uuid: "amd-node",
    hostname: "mi300x",
    gpu_vendor: "amd",
    gpus: [{ index: 0, name: "AMD Instinct MI300X", vendor: "amd", healthy: true, arch_id: "gfx942" }],
  };
  stubFleet([node, amdNode]);
  apiAny.evaluate = async (payload: unknown) => {
    calls.evaluate.push(payload);
    return structuredClone(evaluateAnswer);
  };
  const requested: Array<string | undefined> = [];
  let answerAMD!: (value: ReturnType<typeof list>) => void;
  const amdAnswer = new Promise<ReturnType<typeof list>>((resolve) => { answerAMD = resolve; });
  apiAny.engines = async (vendor?: string) => {
    requested.push(vendor);
    if (vendor === "amd") return amdAnswer;
    return list([engineProfile]);
  };
  const { user } = await openDeployPage();
  await pickVendorAndName(user, "vendor-switch");
  const card = screen.getByText(deployLocale.workbench.resource.sleepable).closest("button")!;
  await waitFor(() => assert.equal(card.hasAttribute("disabled"), false));
  await waitFor(() => assert.ok(calls.evaluate.some((v) => (v as Record<string, unknown>).target_vendor === "nvidia")), { timeout: 10000 });
  await user.click(card);
  await waitFor(() => assert.equal(card.className.includes("is-active"), true));
  await waitFor(() => assert.ok(calls.evaluate.some((v) => (v as Record<string, unknown>).sleepable === true)), { timeout: 10000 });

  const vendorInput = document.querySelector('.ant-select-selection-item[title="NVIDIA"]')!
    .closest(".ant-select")!.querySelector("input")!;
  await user.click(vendorInput);
  await user.click(await screen.findByText(deployLocale.basic.vendor.amd));
  await waitFor(() => assert.ok(requested.includes("amd")));
  // The old NVIDIA profile must not remain actionable while the new answer is in flight.
  await waitFor(() => assert.equal(card.hasAttribute("disabled"), true));
  answerAMD(list([{ ...engineProfile, sleep: { supported: false } }]));
  await waitFor(() => assert.equal(card.hasAttribute("disabled"), true));
  await waitFor(() => assert.equal(card.className.includes("is-active"), false));
  await waitFor(() => {
    const last = calls.evaluate[calls.evaluate.length - 1] as Record<string, unknown> | undefined;
    assert.ok(last && last.target_vendor === "amd" && last.sleepable !== true);
  });
});
