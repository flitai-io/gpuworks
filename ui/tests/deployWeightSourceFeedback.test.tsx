import { dom, createI18n } from "./helpers/dom.ts";
import assert from "node:assert/strict";
import { after, afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import { fakeModels } from "./helpers/fakeModels.ts";
const { render, cleanup, screen, fireEvent, waitFor, act } = await import("@testing-library/react");
const { Form } = await import("antd");
const { api } = await import("../src/api.ts");
const { APIError } = await import("../src/apiError.ts");
const useEvaluation = (await import("../src/pages/deploy/useDeployEvaluation.ts")).default;
const useSource = (await import("../src/pages/deploy/useTemplateWeightSource.ts")).default;
const Fields = (await import("../src/pages/deploy/DeployWeightSourceFields.tsx")).default;
const Modal = (await import("../src/pages/deploy/DeployWeightSourceModal.tsx")).default;
const { i18n, locale } = await createI18n(["deploy"]);
const originals = { evaluate: api.evaluate, modelsPage: api.modelsPage, templateWeightModels: api.templateWeightModels };
afterEach(() => { cleanup(); Object.assign(api, originals); });
after(() => dom.window.close());
const selection = { spec: { name: "browser-vllm", source: "user", engine: "vllm", image: "fake", weight_sources: [{ provider: "huggingface", repo_id: "org/model" }] } };
const profile = { name: "vllm" };
const template = { id: 3 };
const firstFrame: boolean[] = [];
let formRef: import("antd").FormInstance;
let controls: ReturnType<typeof useEvaluation>;
function Host({ build }: { build?: (values: Record<string, unknown>) => Record<string, unknown> | null }) {
  const [form] = Form.useForm();
  formRef = form;
  const values = Form.useWatch([], { form, preserve: true }) || {};
  const selected = Boolean(values.template_id);
  const evaluation = useEvaluation({ form, signal: values, buildPayload: () => build ? build(form.getFieldsValue(true)) : form.getFieldsValue(true), translate: (key, options) => i18n.t(key, options) });
  controls = evaluation;
  const source = useSource({ form, template: selected ? template as never : undefined, selection: selected ? selection as never : undefined, engineProfile: profile as never });
  // Capture the actual first render of the newly selected draft, before the
  // parent's passive effect can announce that the debounce has begun.
  if (selected && firstFrame.length === 0) firstFrame.push(evaluation.evaluating);
  return <Form form={form} initialValues={{ model_id: 7, target_vendor: "nvidia", model_source: { storage_id: 9, storage_picked: true } }}>
    <button type="button" onClick={() => form.setFieldsValue({ template_id: 3, template_spec_source: "user", template_spec_name: "browser-vllm", engine: "vllm" })}>Select spec</button>
    <output data-testid="state">{JSON.stringify({ pending: evaluation.evaluating, error: evaluation.error, result: evaluation.result, source: values.model_source })}</output>
    <Fields acquisition={evaluation.result?.acquisition} modelSource="local" evaluating={evaluation.evaluating} />
    <Modal open={source.modalOpen} loading={source.registering} selection={selected ? selection as never : undefined} choice={source.choice} matches={source.matches} useModel={source.useModel} onUseModel={source.setUseModel} onChoice={source.setChoice} sourceTest={source.sourceTest} onRetest={source.retestSource} onCancel={() => source.setModalOpen(false)} onConfirm={source.confirm} />
  </Form>;
}
const state = () => JSON.parse(screen.getByTestId("state").textContent!);
const mount = (build?: (values: Record<string, unknown>) => Record<string, unknown> | null) => render(<I18nextProvider i18n={i18n}><Host build={build} /></I18nextProvider>);
const answer = (provider = "local", form_values = {}) => ({ form_values, acquisition: { providers: [{ label: provider }], targets: [{ storage_id: 9, label: "Chosen" }, { storage_id: 10, label: "Other" }] } }) as never;
const pause = (ms: number) => act(() => new Promise(resolve => setTimeout(resolve, ms)));

test("selecting a spec invalidates the old acquisition before child effects and never sends its local provider", async () => {
  firstFrame.length = 0;
  const requests: Record<string, any>[] = [];
  const models = fakeModels([{ id: 7, source: "local", source_id: "org/model", name: "Local copy" }]);
  api.modelsPage = models.modelsPage;
  api.templateWeightModels = models.templateWeightModels((templateID, source, name) =>
    templateID === 3 && source === selection.spec.source && name === selection.spec.name ? selection.spec.weight_sources : undefined);
  api.evaluate = async (payload) => {
    const wire = JSON.parse(JSON.stringify(payload)); requests.push(wire);
    if (wire.template_id && wire.model_source?.provider === "local") throw new APIError("ACQUISITION_CANDIDATE_STALE", { provider: "local" }, "selected provider is not declared by this spec", 400);
    return answer(wire.template_id ? "huggingface" : "local");
  };
  mount();
  await waitFor(() => { assert.equal(state().pending, false); assert.equal(state().source.provider, "local"); });
  await pause(500);
  await waitFor(() => assert.equal(state().pending, false));
  fireEvent.click(screen.getByRole("button", { name: "Select spec" }));
  await waitFor(() => assert.ok(screen.getByRole("radio", { name: /Local copy/ })));
  fireEvent.click(screen.getByRole("button", { name: locale.deploy.weightSourceModal.useModel }));
  await waitFor(() => { assert.ok(requests.some(r => r.template_id === 3)); assert.equal(state().pending, false); });
  assert.deepEqual({ firstFrame, staleProviderSent: requests.filter(r => r.template_id).some(r => r.model_source?.provider === "local") }, { firstFrame: [true], staleProviderSent: false }, "new spec is pending before child effects and its wire excludes the old provider");
  assert.equal(state().error, null);
  assert.equal(state().source.storage_id, 9);
  assert.equal(state().source.storage_picked, true);
  assert.equal(formRef.getFieldValue("model_id"), 7);
});

test("missing payload and a current API error settle the current draft", async () => {
  api.evaluate = async () => { throw new Error("evaluation unavailable"); };
  mount(values => values.target_vendor ? values : null);
  await waitFor(() => { assert.equal(state().pending, false); assert.equal(state().error, "evaluation unavailable"); });
  act(() => formRef.setFieldValue("target_vendor", undefined));
  await waitFor(() => { assert.equal(state().pending, false); assert.equal(state().error, null); });
});

test("server form values create a pending draft until its own answer; stale and invalidated answers cannot unlock it", async () => {
  const held: Array<{ payload: Record<string, unknown>; resolve: (value: never) => void }> = [];
  api.evaluate = payload => new Promise(resolve => held.push({ payload, resolve }));
  mount();
  await waitFor(() => assert.equal(held.length, 1));
  act(() => held[0].resolve(answer("local", { image: "server-default" })));
  await waitFor(() => assert.equal(formRef.getFieldValue("image"), "server-default"));
  assert.equal(state().pending, true, "the answer's form projection still needs evaluation");
  await waitFor(() => assert.equal(held.length, 2));
  act(() => formRef.setFieldValue("image", "new-draft"));
  act(() => held[1].resolve(answer("local")));
  await pause(30);
  assert.equal(state().pending, true, "the previous draft cannot settle the new one");
  await waitFor(() => assert.equal(held.length, 3));
  act(() => controls.invalidate("cancelled"));
  act(() => held[2].resolve(answer("local")));
  await pause(30);
  assert.equal(state().pending, false);
  assert.equal(state().error, "cancelled");
  assert.equal(state().result, null);
});
