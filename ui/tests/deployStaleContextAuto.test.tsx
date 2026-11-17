import { createI18n, dom } from "./helpers/dom.ts";
import assert from "node:assert/strict";
import fs from "node:fs";
import { register } from "node:module";
import { pathToFileURL } from "node:url";
import { after, afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import type { EngineProfile, EvaluateResponse } from "../src/api.ts";
import { fakeFleet } from "./helpers/fakeFleet.ts";
import { fakeModels } from "./helpers/fakeModels.ts";
import { list } from "./helpers/listPage.ts";
register(pathToFileURL(new URL("./deploySleepableCssHook.mjs", import.meta.url).pathname));

// The dictionary gives vLLM's maximum context the native automatic sentinel
// (-1) as its default, and gives SGLang's none. A form first evaluated for
// vLLM displays -1 as "Automatic"; choosing an SGLang template afterwards must
// not carry that displayed default into the SGLang draft, where -1 is no
// value at all (the server refuses it as negative). The structured form
// already left displayed values out of its requests; a draft under a manual
// command serializes the whole form, and that is where the -1 leaked.

const { act, cleanup, fireEvent, render, screen, waitFor } = await import("@testing-library/react");
const { MemoryRouter } = await import("react-router-dom");
const { Form, Input, InputNumber } = await import("antd");
const { api } = await import("../src/api.ts");
const useEvaluation = (await import("../src/pages/deploy/useDeployEvaluation.ts")).default;
const DeployImageRow = (await import("../src/pages/deploy/DeployImageRow.tsx")).default;
const DeployParamField = (await import("../src/pages/deploy/DeployParamField.tsx")).default;
const { buildPayload } = await import("../src/pages/deploy/deployHelpers.ts");
const { EvaluateQuickParams } = await import("../src/pages/deploy/evaluateQuickParams.ts");
const { i18n, locale } = await createI18n(["deploy", "images"]);
const deployLocale = locale.deploy;
const profiles = JSON.parse(fs.readFileSync(new URL("./fixtures/engines.json", import.meta.url), "utf8")) as EngineProfile[];
const automatic = i18n.t("shared.automatic", { ns: "deploy" });

const node = {
  uuid: "node-uuid-1", hostname: "gpu3", state: "online", gpu_count: 1, gpu_vendor: "nvidia",
  gpus: [{ index: 0, name: "NVIDIA GeForce RTX 2080 Ti", vendor: "nvidia", schedulable: true, arch_id: "Turing", compute_capability: "7.5" }],
} as never;
const model = {
  id: 3, name: "Qwen/Qwen3-0.6B", source: "modelscope", source_id: "Qwen/Qwen3-0.6B", input_modalities: ["text"],
  weight_bytes: 1_500_000_000, nodes: [{ node_uuid: "node-uuid-1", state: "present" }],
} as never;
const sglangTemplate = {
  id: 41, model_id: "Qwen/Qwen3-0.6B", display_title: "Qwen3 0.6B", source: "upstream", scenarios: ["chat"],
  profiles: [{ name: "sglang-nvidia", engine: "sglang", image: "lmsysorg/sglang:v0.5.21", detected_compatible: true, gpus: { vendor: "nvidia" } }],
};

const apiAny = api as unknown as Record<string, unknown>;
const originals = new Map<string, unknown>();
function stub(name: string, value: unknown) {
  if (!originals.has(name)) originals.set(name, apiAny[name]);
  apiAny[name] = value;
}
const requests: Record<string, any>[] = [];
function answer(request: Record<string, any>): EvaluateResponse {
  const engine = String(request.engine);
  const base = {
    warnings: [], defaults: { model_mount: "/model", master_port: 29500, engine: "vllm" }, image: request.image || `runner:${engine}`,
    command: [engine, "serve"], argv: ["/model"], entrypoint: [engine, "serve"], env: {}, injected: [],
    eligible_nodes: [{ uuid: "node-uuid-1", name: "gpu3", eligible: true }],
    selected_node: { uuid: "node-uuid-1", name: "gpu3", reasons: [{ code: "auto_selected" }] },
  };
  const stated = request.quick_params?.max_model_len;
  // The server's own rule (deploy.validateQuickParams): only vLLM's
  // --max-model-len takes the -1 sentinel; an early field error carries no
  // display instruction.
  if (engine !== "vllm" && typeof stated === "number" && stated < 0) {
    return { ...base, ok: false, errors: [{ field: "max_model_len", code: "FIELD_INVALID", params: { field: "max_model_len" } }] } as never;
  }
  // form_values are the effective values: the dictionary default for vLLM,
  // nothing for SGLang unless the draft states a context; the argument text
  // and the placement's degrees are echoed, as evaluate.go does.
  const context = stated ?? (engine === "vllm" ? -1 : undefined);
  const effective = context === undefined ? {} : { max_model_len: context };
  return {
    ...base, ok: true, effective_params: effective,
    form_values: {
      extra_args: request.extra_args ?? "", tp: request.placement?.tp ?? 1, pp: request.placement?.pp ?? 1,
      ...(context === undefined ? {} : { max_model_len: context }),
    },
  } as never;
}

function resetStubs() {
  requests.length = 0;
  const models = fakeModels([model]);
  stub("models", models.models_all);
  for (const name of ["modelsPage", "model"] as const) stub(name, models[name]);
  const fleet = fakeFleet([node]);
  for (const name of ["nodes", "nodesPage", "node", "nodeFacets"] as const) stub(name, fleet[name]);
  stub("engines", async () => list(profiles));
  stub("inferenceTemplateIndex", async () => ({ items: [sglangTemplate], hardware: [] }));
  stub("inferenceTemplate", async () => ({ ...sglangTemplate, specs: [] }));
  stub("imageVersions", async () => list([]));
  stub("images", async () => list([]));
  stub("evaluate", async (payload: Record<string, unknown>) => {
    const request = structuredClone(payload) as Record<string, any>;
    requests.push(request);
    return answer(request);
  });
}
resetStubs();
afterEach(() => { cleanup(); resetStubs(); });
after(() => {
  for (const [name, value] of originals) apiAny[name] = value;
  dom.window.close();
});

const contextInput = () => screen.getByRole("spinbutton", { name: /Maximum Context Length/ }) as HTMLInputElement;

async function openOnVLLM() {
  const DeployPage = (await import("../src/pages/DeployPage.tsx")).default;
  render(
    <MemoryRouter initialEntries={["/deploy?model=3"]}>
      <I18nextProvider i18n={i18n}><DeployPage /></I18nextProvider>
    </MemoryRouter>,
  );
  const vendor = await screen.findByRole("combobox", { name: new RegExp(deployLocale.basic.targetVendor) });
  fireEvent.mouseDown(vendor);
  fireEvent.click(await screen.findByTitle(deployLocale.basic.vendor.nvidia));
  // The vLLM answer lands its dictionary default: the context shows automatic.
  await waitFor(() => assert.equal(contextInput().value, automatic));
}

test("an SGLang template chosen while editing the command does not inherit vLLM's automatic context", async () => {
  await openOnVLLM();
  // Command text editing is how a draft reaches manual mode on the page.
  const labels = deployLocale.runtimeCommand;
  await waitFor(() => assert.equal((screen.getByRole("button", { name: new RegExp(labels.edit) }) as HTMLButtonElement).disabled, false));
  fireEvent.click(screen.getByRole("button", { name: new RegExp(labels.edit) }));
  fireEvent.click(await screen.findByRole("button", { name: labels.enter, exact: true }));
  await waitFor(() => assert.equal(requests.at(-1)?.container_overrides?.command_mode, "manual"));
  fireEvent.click(await screen.findByRole("button", { name: /Qwen3 0\.6B/ }));
  await waitFor(() => assert.ok(requests.some((request) => request.engine === "sglang")));
  await waitFor(() => {
    const last = requests.at(-1)!;
    assert.equal(last.engine, "sglang");
    assert.equal(last.container_overrides?.command_mode, "manual");
    assert.equal(last.quick_params?.max_model_len, undefined);
  });
  // The visible symptom: the field no longer shows a literal -1.
  assert.notEqual(contextInput().value, "-1");
  assert.equal(contextInput().value, "");
  for (const request of requests.filter((item) => item.engine === "sglang")) {
    assert.notEqual(request.quick_params?.max_model_len, -1, "no SGLang evaluation states the vLLM sentinel");
  }
  assert.equal(screen.queryByText(/FIELD_INVALID|Invalid value/i), null);
});

test("choosing SGLang after explicitly returning to automatic leaves context to SGLang", async () => {
  await openOnVLLM();
  fireEvent.change(contextInput(), { target: { value: "8192" } });
  fireEvent.blur(contextInput());
  await waitFor(() => assert.equal(requests.at(-1)?.quick_params?.max_model_len, 8192));
  fireEvent.click(screen.getByRole("button", { name: automatic }));
  await waitFor(() => assert.equal(requests.at(-1)?.quick_params?.max_model_len, -1));
  const start = requests.length;
  fireEvent.click(await screen.findByRole("button", { name: /Qwen3 0\.6B/ }));
  await waitFor(() => assert.equal(requests.at(-1)?.engine, "sglang"));
  await waitFor(() => assert.equal(contextInput().value, ""));
  for (const request of requests.slice(start).filter((item) => item.engine === "sglang")) {
    assert.equal(request.quick_params?.max_model_len, undefined, "SGLang uses its own automatic context semantics");
  }
  assert.equal(screen.queryByText(/FIELD_INVALID|Invalid value/i), null);
});

// The same feedback loop without the page around it, switching the engine
// with and without a manual command.
let form: import("antd").FormInstance;
const profileOf = (engine: string) => profiles.find((profile) => profile.name === engine)!;
function Host({ imagePicker = false }: { imagePicker?: boolean }) {
  [form] = Form.useForm();
  const values = Form.useWatch([], { form, preserve: true }) || {};
  const engine = String(values.engine || "vllm");
  const spec = profileOf(engine).param_specs!.find((item) => item.key === "max_model_len")!;
  const evaluation = useEvaluation({
    form, signal: values, profiles,
    buildPayload: () => buildPayload(form.getFieldsValue(true), profileOf(String(form.getFieldValue("engine")))).payload,
    translate: (key, options) => i18n.t(key, options),
  });
  return (
    <Form form={form} initialValues={{ engine: "vllm" }} onValuesChange={(changed) => evaluation.noteDraftChanged(changed)}>
      <DeployParamField key={engine} spec={spec} />
      {imagePicker && <DeployImageRow evaluated images={[
        { image_ref: "vllm/vllm-openai:v0.19.0", engine: "vllm" },
        { image_ref: "lmsysorg/sglang:v0.5.21", engine: "sglang" },
      ] as never} />}
      <Form.Item name="extra_args" label="Extra arguments"><Input /></Form.Item>
      <Form.Item name="tp" label="Tensor parallel"><InputNumber /></Form.Item>
      <output data-testid="pending">{String(evaluation.evaluating)}</output>
    </Form>
  );
}
const settled = () => waitFor(() => assert.equal(screen.getByTestId("pending").textContent, "false"));
async function openHost() {
  render(<I18nextProvider i18n={i18n}><Host /></I18nextProvider>);
  await waitFor(() => assert.equal(contextInput().value, automatic));
  await settled();
}
const switchTo = (engine: string, manual: boolean) => act(() => {
  form.setFieldsValue({ engine, ...(manual ? { command_mode: "manual" } : {}) });
});

test("selecting a registered SGLang image resets automatic context but retains a fixed length", async () => {
  render(<I18nextProvider i18n={i18n}><Host imagePicker /></I18nextProvider>);
  await waitFor(() => assert.equal(contextInput().value, automatic));
  await settled();
  fireEvent.change(contextInput(), { target: { value: "8192" } });
  fireEvent.blur(contextInput());
  await waitFor(() => assert.equal(requests.at(-1)?.quick_params?.max_model_len, 8192));
  fireEvent.click(screen.getByRole("button", { name: automatic }));
  await waitFor(() => assert.equal(requests.at(-1)?.quick_params?.max_model_len, -1));
  await settled();
  const image = () => screen.getByRole("combobox", { name: deployLocale.workbench.acquisition.imageOptions });
  fireEvent.change(image(), { target: { value: "lmsysorg/sglang:v0.5.21" } });
  await waitFor(() => assert.equal(requests.at(-1)?.engine, "sglang"));
  await settled();
  assert.equal(contextInput().value, "");
  assert.equal(requests.at(-1)?.quick_params?.max_model_len, undefined);
  assert.equal((buildPayload(form.getFieldsValue(true), profileOf("sglang")).payload.quick_params as Record<string, unknown> | undefined)?.max_model_len, undefined);

  fireEvent.change(image(), { target: { value: "vllm/vllm-openai:v0.19.0" } });
  await waitFor(() => assert.equal(contextInput().value, automatic));
  fireEvent.change(contextInput(), { target: { value: "16384" } });
  fireEvent.blur(contextInput());
  await waitFor(() => assert.equal(requests.at(-1)?.quick_params?.max_model_len, 16384));
  await settled();
  fireEvent.change(image(), { target: { value: "lmsysorg/sglang:v0.5.21" } });
  await waitFor(() => assert.equal(requests.at(-1)?.engine, "sglang"));
  await settled();
  assert.equal(contextInput().value, "16384");
  assert.equal(requests.at(-1)?.quick_params?.max_model_len, 16384);
});

test("clearing an edited context then switching engines does not resurrect vLLM automatic", async () => {
  await openHost();
  fireEvent.change(contextInput(), { target: { value: "8192" } });
  fireEvent.blur(contextInput());
  await waitFor(() => assert.equal(requests.at(-1)?.quick_params?.max_model_len, 8192));
  await settled();
  fireEvent.change(contextInput(), { target: { value: "" } });
  fireEvent.blur(contextInput());
  await waitFor(() => assert.equal(contextInput().value, automatic));
  await settled();
  const start = requests.length;
  switchTo("sglang", false);
  await waitFor(() => assert.equal(requests.at(-1)?.engine, "sglang"));
  await settled();
  for (const request of requests.slice(start)) assert.equal(request.quick_params?.max_model_len, undefined);
  assert.equal(contextInput().value, "");
});

test("a late vLLM automatic response cannot overwrite the SGLang image selection", async () => {
  let reply: ((response: EvaluateResponse) => void) | undefined;
  stub("evaluate", (payload: Record<string, unknown>) => {
    const request = structuredClone(payload) as Record<string, any>;
    requests.push(request);
    if (request.engine === "vllm") return new Promise<EvaluateResponse>((resolve) => { reply = resolve; });
    return Promise.resolve(answer(request));
  });
  render(<I18nextProvider i18n={i18n}><Host imagePicker /></I18nextProvider>);
  await waitFor(() => assert.ok(reply, "the vLLM response is held in flight"));
  fireEvent.change(screen.getByRole("combobox", { name: deployLocale.workbench.acquisition.imageOptions }),
    { target: { value: "lmsysorg/sglang:v0.5.21" } });
  await waitFor(() => assert.equal(requests.at(-1)?.engine, "sglang"));
  await settled();
  await act(async () => { reply!(answer({ engine: "vllm" })); });
  assert.equal(contextInput().value, "");
  assert.equal(form.getFieldValue("engine"), "sglang");
  assert.equal(requests.at(-1)?.quick_params?.max_model_len, undefined);
  assert.equal((buildPayload(form.getFieldsValue(true), profileOf("sglang")).payload.quick_params as Record<string, unknown> | undefined)?.max_model_len, undefined);
});

for (const manual of [true, false]) {
  test(`switching to SGLang${manual ? " with a manual command" : ""} drops the displayed automatic context`, async () => {
    await openHost();
    const start = requests.length;
    switchTo("sglang", manual);
    await waitFor(() => assert.equal(requests.at(-1)?.engine, "sglang"));
    await settled();
    for (const request of requests.slice(start)) assert.equal(request.quick_params?.max_model_len, undefined);
    assert.equal(form.getFieldValue("max_model_len"), undefined);
    assert.equal(contextInput().value, "");
    // The full body a create would send carries no sentinel either.
    const body = buildPayload(form.getFieldsValue(true), profileOf("sglang")).payload;
    assert.equal((body.quick_params as Record<string, unknown> | undefined)?.max_model_len, undefined);
  });
}

test("an engine switch right after an answer lands, before the next request, drops the displayed context", async () => {
  // The switch falls inside the debounce that follows the first answer: no
  // request has stated the key yet, so only the answer says it is a
  // dictionary parameter.
  render(<I18nextProvider i18n={i18n}><Host /></I18nextProvider>);
  await waitFor(() => assert.equal(contextInput().value, automatic));
  const start = requests.length;
  switchTo("sglang", false);
  await waitFor(() => assert.equal(requests.at(-1)?.engine, "sglang"));
  await settled();
  for (const request of requests.slice(start)) assert.equal(request.quick_params?.max_model_len, undefined);
  assert.equal(form.getFieldValue("max_model_len"), undefined);
  assert.equal(contextInput().value, "");
});

test("a context the deployer typed survives the switch to SGLang", async () => {
  await openHost();
  fireEvent.change(contextInput(), { target: { value: "16384" } });
  fireEvent.blur(contextInput());
  await waitFor(() => assert.equal(requests.at(-1)?.quick_params?.max_model_len, 16384));
  await settled();
  switchTo("sglang", true);
  await waitFor(() => assert.equal(requests.at(-1)?.engine, "sglang"));
  await settled();
  assert.equal(requests.at(-1)?.quick_params?.max_model_len, 16384);
  assert.equal(form.getFieldValue("max_model_len"), 16384);
});

test("an explicit automatic context uses the target engine’s automatic semantics", async () => {
  // Automatic is an intent; its negative spelling belongs to vLLM.
  await openHost();
  fireEvent.change(contextInput(), { target: { value: "8192" } });
  fireEvent.blur(contextInput());
  await waitFor(() => assert.equal(requests.at(-1)?.quick_params?.max_model_len, 8192));
  fireEvent.click(screen.getByRole("button", { name: automatic }));
  await waitFor(() => assert.equal(requests.at(-1)?.quick_params?.max_model_len, -1));
  await settled();
  switchTo("sglang", false);
  await waitFor(() => assert.equal(requests.at(-1)?.engine, "sglang"));
  await settled();
  assert.equal(requests.at(-1)?.quick_params?.max_model_len, undefined);
  assert.equal(form.getFieldValue("max_model_len"), undefined);
});

for (const manual of [true, false]) {
  test(`typed extra arguments and tensor parallelism survive the switch${manual ? " with a manual command" : ""}`, async () => {
    // Neither travels in quick_params, and the server echoes both in
    // form_values: the draft equals the display, yet they are the deployer's.
    await openHost();
    fireEvent.change(screen.getByRole("textbox", { name: "Extra arguments" }), { target: { value: "--enable-x" } });
    fireEvent.change(screen.getByRole("spinbutton", { name: "Tensor parallel" }), { target: { value: "2" } });
    fireEvent.blur(screen.getByRole("spinbutton", { name: "Tensor parallel" }));
    await waitFor(() => {
      assert.equal(requests.at(-1)?.extra_args, "--enable-x");
      assert.equal(requests.at(-1)?.placement?.tp, 2);
    });
    await settled();
    // A further structured evaluation, so the typed values are now displayed.
    fireEvent.change(contextInput(), { target: { value: "4096" } });
    fireEvent.blur(contextInput());
    await waitFor(() => assert.equal(requests.at(-1)?.quick_params?.max_model_len, 4096));
    await settled();
    switchTo("sglang", manual);
    await waitFor(() => assert.equal(requests.at(-1)?.engine, "sglang"));
    await settled();
    assert.equal(form.getFieldValue("extra_args"), "--enable-x");
    assert.equal(form.getFieldValue("tp"), 2);
    assert.equal(requests.at(-1)?.extra_args, "--enable-x");
    assert.equal(requests.at(-1)?.placement?.tp, 2);
  });
}

test("only untouched dictionary values are stale for another engine, and only once", () => {
  const params = new EvaluateQuickParams();
  // Before any request: the answer alone names its dictionary parameters.
  params.accept({ form_values: { max_model_len: -1, extra_args: "", tp: 1 }, effective_params: { max_model_len: -1 } } as never, "vllm");
  assert.deepEqual(params.takeStaleFor("sglang", { max_model_len: -1, extra_args: "", tp: 1 }), ["max_model_len"]);
  const draft = { max_model_len: -1, dtype: "auto", extra_args: "--enable-x", tp: 2, pp: 1, kv_cache_dtype: "" };
  params.accept({ form_values: { max_model_len: -1, dtype: "auto", kv_cache_dtype: "" } } as never, "vllm");
  params.prepare({ quick_params: { max_model_len: -1, dtype: "auto", kv_cache_dtype: "" }, extra_args: "--enable-x" });
  params.noteChanged({ extra_args: "--enable-x", tp: 2 });
  params.prepare({ quick_params: { max_model_len: -1, dtype: "auto" }, extra_args: "--enable-x" });
  params.accept({ form_values: { ...draft } } as never, "vllm");
  assert.deepEqual(params.takeStaleFor("vllm", draft), [], "the same engine keeps its display");
  assert.deepEqual(params.takeStaleFor("", draft), [], "an engine not yet known decides nothing");
  assert.deepEqual(params.takeStaleFor("sglang", draft).sort(), ["dtype", "max_model_len"]);
  assert.deepEqual(params.takeStaleFor("sglang", draft), [], "the display is forgotten once taken");
});
