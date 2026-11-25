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

// Bug: the server attributes an evaluate field error to container_mount_rows
// (a Form.List, not a single control) and, sometimes, to fields the deploy
// page never gives a control at all (engine, quick_params_unset). Before this
// fix, both kinds of error vanished silently: the mounts list never rendered
// its list-level error, and useDeployEvaluation only fell back to the
// page-level line for an error with an EMPTY field, not one attributed to a
// field with no control on screen. These tests drive the real hook
// (useDeployEvaluation) and the real page components end to end, the way the
// deploy page actually renders, rather than hand-setting form errors.

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
const commonLocale = JSON.parse(
  fs.readFileSync(
    new URL("../src/i18n/locales/en/common.json", import.meta.url),
    "utf8",
  ),
);
const imagesLocale = JSON.parse(
  fs.readFileSync(
    new URL("../src/i18n/locales/en/images.json", import.meta.url),
    "utf8",
  ),
);
const i18n = createInstance();
await i18n.use(initReactI18next).init({
  lng: "en",
  fallbackLng: "en",
  resources: {
    en: { deploy: deployLocale, common: commonLocale, images: imagesLocale },
  },
  defaultNS: "common",
  interpolation: { escapeValue: false },
});
const { act, cleanup, fireEvent, render, screen, waitFor } =
  await import("@testing-library/react");
const { MemoryRouter } = await import("react-router-dom");
const { apiErrorText, APIError } = await import("../src/apiError.ts");
const { api } = await import("../src/api.ts");
const { Form } = await import("antd");
type FormInstance = import("antd").FormInstance;
const ContainerOverridesEditor = (
  await import("../src/pages/deploy/ContainerOverridesEditor.tsx")
).default;

// The translated text an evaluate code renders as - the same lookup
// useDeployEvaluation performs, so a locale wording change cannot silently
// desync the expectation from the code under test.
function errorText(code: string, params?: Record<string, unknown>) {
  return apiErrorText(
    (key: string, options?: Record<string, unknown>) => i18n.t(key, options),
    new APIError(code, params ?? {}, code, 200),
  );
}

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
const engineProfile = {
  name: "vllm",
  sleep: { supported: true, levels: ["L1", "L2"] },
  quick_form_keys: ["gpu_memory_utilization", "enable_prefix_caching"],
  param_specs: [gpuMemSpec, prefixCachingSpec],
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
  stubFleet([node]);
  stub("engines", async () => list([engineProfile]));
  stub("inferenceTemplateIndex", async () => ({ items: [] }));
  stub("imageVersions", async () => list([]));
  stub("images", async () => list([]));
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

// A renderable evaluate answer: the fields the wider page tree (node card,
// runtime command preview, VRAM summary) reads regardless of which errors are
// under test in a given case. Every case below only changes `errors` (and the
// handful of scalar fields the field_errors fixture states), taken from the
// fixture rather than invented here.
const baseAnswer = {
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
  template_deviated: false,
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

const fieldErrorsFixture = JSON.parse(
  fs.readFileSync(
    new URL("./fixtures/evaluate/field_errors.json", import.meta.url),
    "utf8",
  ),
) as {
  errors: Array<{
    field: string;
    code: string;
    params?: Record<string, unknown>;
  }>;
  replicas: number;
  ok: boolean;
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
  // Evaluation waits for the required target accelerator, as a person picks it.
  const vendor = await screen.findByRole("combobox", { name: new RegExp(deployLocale.basic.targetVendor) });
  fireEvent.mouseDown(vendor);
  fireEvent.click(await screen.findByTitle(deployLocale.basic.vendor.nvidia));
}

test("a duplicate mount target is attributed to the mounts list, and the replica error to its own field", async () => {
  // The fixture's own two errors, unmodified: replicas has an ordinary
  // control, container_mount_rows is the Form.List this fix teaches to show
  // its own error.
  stub("evaluate", async () => ({
    ...baseAnswer,
    ok: fieldErrorsFixture.ok,
    replicas: fieldErrorsFixture.replicas,
    errors: fieldErrorsFixture.errors,
  }));

  await openDeployPage();

  const replicasText = errorText("REPLICAS_NEGATIVE");
  const mountText = errorText("MOUNT_TARGET_DUPLICATE", { target: "/model" });

  await waitFor(() => assert.ok(screen.getByText(replicasText)));
  assert.ok(
    screen.getByText(mountText),
    "the mount duplicate message is visible somewhere on the page",
  );

  const replicasField = screen
    .getByText(deployLocale.catalog.replicas)
    .closest(".ant-form-item");
  assert.ok(
    replicasField?.textContent?.includes(replicasText),
    "the replica error sits beside the replicas control",
  );

  const mountsSection = screen
    .getByText(deployLocale.containerOverrides.mounts)
    .closest(".ant-form-item");
  assert.ok(
    mountsSection?.textContent?.includes(mountText),
    "the mount duplicate error sits inside the mounts section, not dropped",
  );
});

test("every refused docker arg shows beside the docker args field, not only the last one", async () => {
  // A docker run command pasted into the docker args field: the server
  // answers one field error per refused arg, all on container_docker_args.
  stub("evaluate", async () => ({
    ...baseAnswer,
    ok: false,
    errors: [
      {
        field: "container_docker_args",
        code: "DOCKER_ARG_USE_SHM_FIELD",
        params: { arg: "--shm-size 256g", option: "--shm-size" },
      },
      {
        field: "container_docker_args",
        code: "DOCKER_ARG_PLATFORM_MANAGED",
        params: { arg: "--network=host", option: "--network" },
      },
    ],
  }));

  await openDeployPage();

  const shmText = errorText("DOCKER_ARG_USE_SHM_FIELD", {
    arg: "--shm-size 256g",
    option: "--shm-size",
  });
  const networkText = errorText("DOCKER_ARG_PLATFORM_MANAGED", {
    arg: "--network=host",
    option: "--network",
  });
  await waitFor(() => assert.ok(screen.getByText(shmText)));
  const dockerField = screen
    .getByText(deployLocale.containerOverrides.dockerArgs)
    .closest(".ant-form-item");
  assert.ok(
    dockerField?.textContent?.includes(shmText),
    "the --shm-size error sits beside the docker args field",
  );
  assert.ok(
    dockerField?.textContent?.includes(networkText),
    "the --network error sits beside it too, not overwritten by the next one",
  );
  // The evaluation summary says the draft did not pass instead of staying
  // blank while every error sits on a field.
  const summary = screen
    .getByText(i18n.t("deploy:summary.fieldErrors", { count: 2 }))
    .closest('[role="alert"]');
  assert.ok(summary, "the summary names how many settings need changing");
});

test("an error attributed to a field the deploy page never controls (engine) falls back to the page-level line", async () => {
  // engine has no Form.Item anywhere on the deploy page: useDeployEvaluation
  // must not drop this error just because the field name is non-empty. A
  // copy of the fixture, with only its errors[] replaced, keeps the response
  // shape the server actually sends.
  const engineErrorsAnswer = {
    ...fieldErrorsFixture,
    errors: [
      {
        field: "engine",
        code: "ENGINE_INVALID",
        params: { engine: "not-a-real-engine" },
      },
    ],
  };
  stub("evaluate", async () => ({
    ...baseAnswer,
    ok: engineErrorsAnswer.ok,
    replicas: engineErrorsAnswer.replicas,
    errors: engineErrorsAnswer.errors,
  }));

  await openDeployPage();

  const engineText = errorText("ENGINE_INVALID", {
    engine: "not-a-real-engine",
  });
  // The page-level line lives in the step summary aside as an Alert; it must
  // not have landed on any form field instead. Query and inspect the same
  // render: a new draft can hide the previous answer while it evaluates.
  await waitFor(() => {
    const alert = screen.getByText(engineText).closest(".ant-alert-error");
    assert.ok(alert, "the unattributable error renders as the page-level alert");
  });
});

test("the quick form offers no data-parallel card, and a degree the GPUs cannot run still shows the GPU-count rule", async () => {
  // The scheduler assigns tp × pp GPUs, so a one-click "data parallel 2" could
  // never fit. The parameter stays in the dictionary as an ordinary advanced
  // field; a degree stated there or in extra args is judged by evaluate.
  const dataParallelSpec = {
    key: "data_parallel_size",
    flag: "--data-parallel-size",
    type: "integer",
    control: "number",
    group: "parallel",
    min: 1,
    step: 1,
    label: { en: "Data Parallel Size", zh: "数据并行数" },
  };
  const profile = engineProfile as unknown as { param_specs: unknown[] };
  stub("engines", async () => list([
    { ...profile, param_specs: [...profile.param_specs, dataParallelSpec] },
  ]));
  // The params are the ones the server's own evaluate test pins for this
  // draft on a one-GPU node (TestEvaluateRefusesQuickDataParallelBeyondTheAssignedGPUs).
  const params = {
    key: "data_parallel_size",
    flag: "--data-parallel-size",
    dp: 2,
    tp: 1,
    pp: 1,
    required: 2,
    assigned: 1,
  };
  stub("evaluate", async () => ({
    ...baseAnswer,
    ok: false,
    errors: [
      { field: "data_parallel_size", code: "DATA_PARALLEL_GPU_COUNT", params },
    ],
  }));

  await openDeployPage();

  const text = errorText("DATA_PARALLEL_GPU_COUNT", params);
  await waitFor(() => assert.ok(screen.getByText(text)));
  assert.ok(
    text.includes("needs 2 GPUs") && text.includes("assigned 1"),
    "the message tells the user how many GPUs the degree needs and how many they have",
  );
  // The tensor and expert cards are still there; no card toggles data parallel.
  assert.ok(
    screen.getByText(deployLocale.workbench.parallel.tensor),
    "the parallel row itself still renders",
  );
  const cards = screen
    .getAllByRole("button")
    .filter((button) => /data parallel|数据并行/i.test(button.textContent ?? ""));
  assert.deepEqual(cards, [], "no choice card offers data parallel");
});

test("ContainerOverridesEditor renders a list-level error set on the mounts Form.List", async () => {
  let form: FormInstance | undefined;
  function Host() {
    const [instance] = Form.useForm();
    form = instance;
    return (
      <Form form={instance}>
        <ContainerOverridesEditor />
      </Form>
    );
  }
  render(
    <I18nextProvider i18n={i18n}>
      <Host />
    </I18nextProvider>,
  );
  const message = errorText("MOUNT_TARGET_DUPLICATE", { target: "/model" });
  act(() => {
    form!.setFields([{ name: "container_mount_rows", errors: [message] }]);
  });
  await waitFor(() => assert.ok(screen.getByText(message)));
  const mountsSection = screen
    .getByText(deployLocale.containerOverrides.mounts)
    .closest(".ant-form-item");
  assert.ok(
    mountsSection?.textContent?.includes(message),
    "the list-level error renders inside the mounts Form.Item",
  );
});
