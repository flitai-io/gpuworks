import { createI18n } from "./helpers/dom.ts";
import assert from "node:assert/strict";
import fs from "node:fs";
import { afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import type { EngineProfile, EvaluateResponse, KVBackendProfile } from "../src/api.ts";

const { cleanup, fireEvent, render, screen, waitFor } = await import("@testing-library/react");
const { Form } = await import("antd");
const { api } = await import("../src/api.ts");
const DeployParamField = (await import("../src/pages/deploy/DeployParamField.tsx")).default;
const DeployKvOffloadSection = (await import("../src/pages/deploy/DeployKvOffloadSection.tsx")).default;
const useEvaluation = (await import("../src/pages/deploy/useDeployEvaluation.ts")).default;
const { buildPayload, deployDraftPayload, kvTierSpecs } = await import("../src/pages/deploy/deployHelpers.ts");
const { localizedText } = await import("../src/i18n/localizedText.ts");
const { i18n, locale } = await createI18n(["deploy"]);
const profiles = JSON.parse(fs.readFileSync(new URL("./fixtures/engines.json", import.meta.url), "utf8")) as EngineProfile[];
const vllm = profiles.find((profile) => profile.name === "vllm")!;
const sglang = profiles.find((profile) => profile.name === "sglang")!;
const sglangKeys = new Set(sglang.param_specs!.map((spec) => spec.key));
const exclusive = vllm.param_specs!.filter((spec) => !sglangKeys.has(spec.key));
const modalities = ["text", "image"];

// Literal saved choices supply independent expected values. The real endpoint
// fixture discovers the coverage surface: a new key needs a meaningful sample.
const exclusiveChoices: Record<string, unknown> = {
  allowed_local_media_path: "/srv/media", api_server_count: 2,
  async_scheduling: true, cpu_offload_gb: 4, enable_chunked_prefill: false,
  enable_request_metrics: false, kv_cache_memory_bytes: 1073741824,
  kv_disk_limit_gib: 8, kv_disk_offload: true, kv_offload_read_threads: 2,
  kv_offload_write_threads: 3, kv_offloading_size: 16,
  limit_mm_per_prompt: '{"image":2}', long_prefill_token_threshold: 1024,
  max_cudagraph_capture_size: 128, media_io_kwargs: '{"image":{"num_threads":2}}',
  mm_encoder_tp_mode: "data", mm_processor_kwargs: '{"min_pixels":1024}',
  optimization_level: "O1", performance_mode: "throughput",
  prefix_caching_hash_algo: "sha256", rope_scaling: '{"rope_type":"linear","factor":2}',
  shared_kv_dir: "/srv/kv-cache", stream_interval: 8, watermark: 0.1,
};
const sharedChoices = {
  max_model_len: 16384, gpu_memory_utilization: 0.75, dtype: "bfloat16",
  kv_cache_dtype: "bfloat16", enable_prefix_caching: false, max_num_seqs: 12,
};
type Body = Record<string, any>;
const requests: Body[] = [];
const creates: Body[] = [];
const originalEvaluate = api.evaluate;
afterEach(() => {
  cleanup(); api.evaluate = originalEvaluate; requests.length = 0; creates.length = 0;
});

// Controlled API boundary: only the target dictionary is returned.
// Closed enums are validated; open combobox suggestions are not an allowlist.
function evaluateFor(available: EngineProfile[]) {
  api.evaluate = (async (request: Body) => {
    requests.push(structuredClone(request));
    const profile = available.find((item) => item.name === request.engine)!;
    const specs = profile.param_specs ?? [];
    const effective: Record<string, unknown> = {};
    for (const [key, value] of Object.entries(request.quick_params ?? {})) {
      const spec = specs.find((item) => item.key === key);
      if (!spec) return {
        ok: false, errors: [{ field: key, code: "FIELD_INVALID", params: { field: key, detail: `${key} is unavailable for ${profile.name}` } }],
      } as EvaluateResponse;
      if (spec.type === "enum" && spec.enum?.length) {
        const canonical = spec.enum.find((option) => option.toLowerCase() === String(value).toLowerCase());
        if (canonical === undefined) return {
          ok: false, errors: [{ field: key, code: "FIELD_INVALID", params: { field: key, detail: `${value} is unavailable for ${profile.name}` } }],
        } as EvaluateResponse;
        effective[key] = canonical;
      } else effective[key] = value;
    }
    const unset = new Set(request.quick_params_unset ?? []);
    for (const spec of specs) {
      if (spec.default == null || Object.hasOwn(effective, spec.key) || unset.has(spec.key)) continue;
      if (spec.depends_on && !effective[spec.depends_on]) continue;
      effective[spec.key] = spec.default;
    }
    return {
      ok: true, effective_params: effective,
      form_values: { ...effective, extra_args: request.extra_args ?? "" },
      defaults: { engine: "vllm", model_mount: "/model", master_port: 29500 },
    } as EvaluateResponse;
  }) as typeof api.evaluate;
}

type Options = { available?: EngineProfile[]; keys?: string[]; tiers?: boolean };
function open(values: Body, { available = profiles, keys = [...exclusive.map((spec) => spec.key), ...Object.keys(sharedChoices)], tiers = false }: Options = {}) {
  evaluateFor(available);
  const initial = {
    engine: "vllm", model_id: 7, target_vendor: "nvidia", node_selection_mode: "auto",
    image: "vllm/vllm-openai:v0.30.0", ...values,
  };
  function Host() {
    const [form] = Form.useForm();
    const watched = Form.useWatch([], { form, preserve: true }) as Body | undefined;
    const draft = watched ?? initial;
    const profile = available.find((item) => item.name === (draft.engine || "vllm"))!;
    const evaluation = useEvaluation({
      form, signal: draft, profiles: available,
      buildPayload: () => deployDraftPayload(form.getFieldsValue(true), [], profile, modalities),
      translate: (key, options) => i18n.t(key, options),
    });
    const tierSpecs = kvTierSpecs(profile.param_specs);
    const fields = (profile.param_specs ?? []).filter((spec) => keys.includes(spec.key));
    return <Form form={form} initialValues={initial} onValuesChange={(changed) => evaluation.noteDraftChanged(changed)}>
      {fields.map((spec) => <DeployParamField key={spec.key} spec={spec} />)}
      {tiers && <DeployKvOffloadSection
        kvMemorySpec={tierSpecs.memory} kvDiskSpec={tierSpecs.disk}
        kvDiskCompanionSpecs={tierSpecs.companions} backends={profile.kv_backends} manualCommand={false}
      />}
      <button type="button" onClick={() => form.setFieldsValue({ engine: "sglang", image: "lmsysorg/sglang:v0.5.21" })}>Use SGLang</button>
      <button type="button" onClick={() => form.setFieldsValue({ engine: "vllm", image: "vllm/vllm-openai:v0.30.0" })}>Use vLLM</button>
      <button type="button" disabled={evaluation.evaluating || !evaluation.result?.ok} onClick={() => {
        creates.push(buildPayload(form.getFieldsValue(true), profile, modalities).payload);
      }}>Confirm configuration</button>
      <output data-testid="pending">{String(evaluation.evaluating)}</output>
      <output data-testid="errors">{JSON.stringify(evaluation.result?.errors ?? [])}</output>
    </Form>;
  }
  render(<I18nextProvider i18n={i18n}><Host /></I18nextProvider>);
}
async function settled(engine: string) {
  await waitFor(() => {
    assert.equal(requests.at(-1)?.engine, engine);
    assert.equal(screen.getByTestId("pending").textContent, "false");
  });
}
async function confirm() {
  const previous = creates.length;
  fireEvent.click(screen.getByRole("button", { name: "Confirm configuration" }));
  assert.equal(creates.length, previous + 1, "the settled form can confirm its visible configuration");
  return creates.at(-1)!;
}
const quick = (body: Body) => body.quick_params ?? {};
const label = (key: string) => localizedText(vllm.param_specs!.find((spec) => spec.key === key)!.label, "en");

// A projection-only test misses an SGLang reply erasing the old draft. Wait
// for that reply before switching back to exercise the actual feedback loop.
test("every vLLM-only dictionary choice stays off SGLang requests and is restored after its evaluate reply", async () => {
  assert.deepEqual(exclusive.map((spec) => spec.key).sort(), Object.keys(exclusiveChoices).sort(), "the literal samples cover the complete published vLLM-only dictionary");
  open({ ...exclusiveChoices, ...sharedChoices });
  await settled("vllm");
  const cpu = screen.getByRole("spinbutton", { name: label("cpu_offload_gb") });
  fireEvent.change(cpu, { target: { value: "6" } });
  fireEvent.blur(cpu);
  await waitFor(() => assert.equal(quick(requests.at(-1)!).cpu_offload_gb, 6));
  await settled("vllm");
  const chosen = { ...exclusiveChoices, cpu_offload_gb: 6 };
  const source = await confirm();
  for (const [key, value] of Object.entries(chosen)) assert.deepEqual(quick(source)[key], value, `chosen ${key} is genuinely on the source wire`);
  const start = requests.length;
  fireEvent.click(screen.getByRole("button", { name: "Use SGLang" }));
  await settled("sglang");
  const target = await confirm();
  for (const body of [...requests.slice(start), target]) {
    for (const spec of exclusive) assert.equal(Object.hasOwn(quick(body), spec.key), false, `${spec.key} is not an SGLang parameter`);
    for (const [key, value] of Object.entries(sharedChoices)) assert.deepEqual(quick(body)[key], value, `${key} is the deployer's shared choice`);
  }
  fireEvent.click(screen.getByRole("button", { name: "Use vLLM" }));
  await settled("vllm");
  const restored = await confirm();
  for (const [key, value] of Object.entries(chosen)) assert.deepEqual(quick(restored)[key], value, `the SGLang reply did not erase ${key}`);
  assert.equal((screen.getByRole("spinbutton", { name: label("cpu_offload_gb") }) as HTMLInputElement).value, "6");
});

for (const [key, value] of [["kv_cache_dtype", "fp8"], ["speculative_algorithm", "mtp"]] as const) {
  test(`the explicit ${key}=${value} survives until SGLang reports its closed-enum field error`, async () => {
    open({ ...sharedChoices, [key]: value }, { keys: [key] });
    await settled("vllm");
    assert.equal(quick(await confirm())[key], value);
    fireEvent.click(screen.getByRole("button", { name: "Use SGLang" }));
    await settled("sglang");
    assert.equal(quick(requests.at(-1)!)[key], value, "the UI neither deletes nor guesses another engine's equivalent");
    const errors = JSON.parse(screen.getByTestId("errors").textContent!);
    assert.equal(errors[0].field, key);
    assert.equal((screen.getByRole("button", { name: "Confirm configuration" }) as HTMLButtonElement).disabled, true);
    await waitFor(() => assert.ok(document.querySelector(".ant-form-item-explain-error"), "the refusal is visible beside the rendered field"));
  });
}

test("open parser plugin names typed into the real controls survive the engine switch", async () => {
  const choices = { reasoning_parser: "acme_reasoning_v2", tool_call_parser: "acme_tools_v2" };
  open({ ...sharedChoices }, { keys: Object.keys(choices) });
  await settled("vllm");
  for (const [key, value] of Object.entries(choices)) {
    const input = screen.getByRole("combobox", { name: label(key) });
    fireEvent.change(input, { target: { value } });
    fireEvent.blur(input);
  }
  await waitFor(() => {
    for (const [key, value] of Object.entries(choices)) assert.equal(quick(requests.at(-1)!)[key], value);
  });
  await settled("vllm");
  fireEvent.click(screen.getByRole("button", { name: "Use SGLang" }));
  await settled("sglang");
  const body = await confirm();
  for (const [key, value] of Object.entries(choices)) {
    assert.equal(quick(requests.at(-1)!)[key], value);
    assert.equal(quick(body)[key], value);
    assert.equal((screen.getByRole("combobox", { name: label(key) }) as HTMLInputElement).value, value);
  }
  assert.deepEqual(JSON.parse(screen.getByTestId("errors").textContent!), []);
});

const nativeTiers = kvTierSpecs(vllm.param_specs);
const nativeKeys = [nativeTiers.memory!.key, nativeTiers.disk!.key, ...nativeTiers.companions.map((spec) => spec.key)];
const nativeChoices = Object.fromEntries(nativeKeys.map((key) => [key, exclusiveChoices[key]]));
const backend: KVBackendProfile = {
  id: "test-cache-handoff", label: "Handoff Cache", replaces: nativeKeys, pd_roles: ["prefill"],
};
const withBackend: EngineProfile = {
  ...vllm, kv_backends: [backend],
  param_specs: [...vllm.param_specs!, {
    key: "kv_offload_backend", flag: "", type: "enum", control: "select",
    label: { en: "KV backend" }, enum: [backend.id], options: [{ value: backend.id, label: { en: backend.label } }],
  }],
};
const availableWithBackend = profiles.map((profile) => profile.name === "vllm" ? withBackend : profile);

for (const replacement of [false, true]) {
  test(`a ${replacement ? "backend-replaced" : "disabled"} KV draft survives successful cross-engine evaluations without emitting its inactive native keys`, async () => {
    // These are retained UI drafts: the inactive values intentionally do not
    // appear in the first response. A checker must inspect the active payload,
    // not accuse every value that the preserved Form store still contains.
    open({
      ...nativeChoices, max_model_len: 16384,
      ...(replacement ? { kv_offload_backend: backend.id } : { kv_memory_tier_enabled: false }),
    }, { available: replacement ? availableWithBackend : profiles, keys: [], tiers: true });
    await settled("vllm");
    const source = await confirm();
    for (const key of nativeKeys) assert.equal(Object.hasOwn(quick(source), key), false, `${key} is inactive at source`);
    if (replacement) assert.equal(quick(source).kv_offload_backend, backend.id);
    fireEvent.click(screen.getByRole("button", { name: "Use SGLang" }));
    await settled("sglang");
    const target = await confirm();
    for (const key of [...nativeKeys, "kv_offload_backend"]) {
      assert.equal(Object.hasOwn(quick(target), key), false, `${key} is absent from the SGLang dictionary`);
      assert.equal((target.quick_params_unset ?? []).includes(key), false, `no foreign ${key} tombstone is sent`);
    }
    fireEvent.click(screen.getByRole("button", { name: "Use vLLM" }));
    await settled("vllm");
    const restoredInactive = await confirm();
    for (const key of nativeKeys) assert.equal(Object.hasOwn(quick(restoredInactive), key), false);
    if (replacement) assert.equal(quick(restoredInactive).kv_offload_backend, backend.id);
    const memoryCard = screen.getByText(locale.deploy.workbench.kv.memory, { exact: true }).closest("button");
    assert.ok(memoryCard);
    fireEvent.click(memoryCard);
    await settled("vllm");
    const restored = await confirm();
    for (const [key, value] of Object.entries(nativeChoices)) assert.deepEqual(quick(restored)[key], value, `${key} is restored after reactivating the engine tier`);
  });
}
