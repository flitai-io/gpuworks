import assert from "node:assert/strict";
import { after, afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import { createI18n, dom } from "./helpers/dom.ts";

const { i18n, locale } = await createI18n(["deploy"], "en");
const { cleanup, render, screen, waitFor, act } = await import("@testing-library/react");
const { Form, Input, message } = await import("antd");
const { default: useDeploySubmit } = await import("../src/pages/deploy/useDeploySubmit.ts");
const { default: DeploySubmitBar } = await import("../src/pages/deploy/DeploySubmitBar.tsx");
const { default: DeploySubmissionError } = await import("../src/pages/deploy/DeploySubmissionError.tsx");
const { APIError } = await import("../src/apiError.ts");
const { api } = await import("../src/api.ts");
type FormInstance = import("antd").FormInstance;
type EvaluateResponse = import("../src/api.ts").EvaluateResponse;
const originalCreate = api.createDeployment;
afterEach(() => { cleanup(); message.destroy(); api.createDeployment = originalCreate; });
after(() => dom.window.close());

const evaluated = {
  ok: true, image: "fixture:test", selected_node: { uuid: "node" },
  plan: { plan: { gpus: [0], tp: 1 } },
} as EvaluateResponse;

function mount(result: EvaluateResponse = evaluated) {
  const requests: unknown[] = [];
  const rejections: unknown[] = [];
  let form: FormInstance;
  let pending: Promise<void> | undefined;
  api.createDeployment = (async (body: unknown) => { requests.push(body); return { id: 1 }; }) as never;
  function Harness() {
    [form] = Form.useForm();
    const { submit, submitting, submitError } = useDeploySubmit({ form, nodes: [], evalErr: null,
      evaluating: false, evalResult: result, onCreated: () => {} });
    return <Form form={form} initialValues={{ model_id: 3, node_selection_mode: "auto", nnodes: 1 }}>
      <Form.Item name="name" label="Deployment name" rules={[{ required: true, message: "Enter a deployment name" }]}>
        <Input />
      </Form.Item>
      <DeploySubmitBar submitting={submitting} disabled={false} submit={() => {
        // Observe the promise the actual button discards, without turning a
        // broken implementation into a process-level unhandled rejection.
        pending = submit().catch((error) => { rejections.push(error); });
      }} />
      <DeploySubmissionError error={submitError} />
    </Form>;
  }
  render(<I18nextProvider i18n={i18n}><Harness /></I18nextProvider>);
  const click = async () => {
    await act(async () => {
      screen.getByRole("button", { name: locale.deploy.page.deploy as string }).click();
      await pending;
    });
  };
  return { requests, rejections, click, getForm: () => form! };
}

test("required-field rejection stays in the form and a corrected draft can create", async () => {
  const harness = mount();
  await harness.click();
  await waitFor(() => assert.equal(screen.getByText("Enter a deployment name").textContent, "Enter a deployment name"));
  assert.deepEqual(harness.requests, []);
  assert.deepEqual(harness.rejections, [], "the Deploy button must not leak AntD validation rejection");
  await act(async () => { harness.getForm().setFieldValue("name", "corrected"); });
  await harness.click();
  assert.equal(harness.requests.length, 1);
  assert.equal((harness.requests[0] as Record<string, unknown>).name, "corrected");
  assert.deepEqual(harness.rejections, []);
});

test("a rejected create retains its checks and request ID beside the unchanged draft", async () => {
  const harness = mount();
  const checks = { checks: [{ id: "node:7", status: "fail", message: {
    code: "MODEL_SOURCE_UNAVAILABLE", params: { reason: "not_on_node", node: "gpu-a", model: "local-model", observer: "control_plane" }, message: "no complete model record",
  } }] };
  api.createDeployment = async () => { throw new APIError("MODEL_SOURCE_UNAVAILABLE", {}, "source unavailable", 400, checks, "r215"); };
  await act(async () => harness.getForm().setFieldValue("name", "retained-draft"));
  await harness.click();
  await screen.findByText("The last create attempt failed; your draft is preserved.");
  assert.ok(screen.getByText("This node has no complete model-copy record. Check other copies or prepare the model first."));
  assert.ok(screen.getByText("Request ID: r215"));
  assert.equal(harness.getForm().getFieldValue("name"), "retained-draft");
  assert.deepEqual(harness.rejections, []);
});

test("an unexpected validation exception still propagates to the caller", async () => {
  const harness = mount();
  const unexpected = new Error("validation infrastructure failed");
  harness.getForm().validateFields = async () => { throw unexpected; };
  await harness.click();
  assert.deepEqual(harness.requests, []);
  assert.deepEqual(harness.rejections, [unexpected]);
});


test("an object with only errorFields is not treated as a known AntD rejection", async () => {
  const harness = mount();
  const unexpected = { errorFields: [], detail: "not an AntD validation entity" };
  harness.getForm().validateFields = async () => { throw unexpected; };
  await harness.click();
  assert.deepEqual(harness.requests, []);
  assert.deepEqual(harness.rejections, [unexpected]);
});

for (const language of ["en", "zh"] as const) {
  test(`Deploy keeps its exact accessible action name while loading changes (${language})`, async () => {
    const { i18n: translated, locale: labels } = await createI18n(["deploy"], language);
    const actionName = labels.deploy.page.deploy as string;
    let submits = 0;
    const bar = (submitting: boolean, disabled = false) => (
      <I18nextProvider i18n={translated}>
        <DeploySubmitBar submitting={submitting} disabled={disabled} submit={() => { submits++; }} />
      </I18nextProvider>
    );
    const view = render(bar(true));
    const pending = screen.getByRole("button", { name: actionName, exact: true });
    assert.equal(pending.getAttribute("aria-busy"), "true");
    await act(async () => { pending.click(); });
    assert.equal(submits, 0, "a pending submit must not create again");

    view.rerender(bar(false));
    const ready = screen.getByRole("button", { name: actionName, exact: true });
    assert.equal(ready.getAttribute("aria-busy"), "false");
    await act(async () => { ready.click(); });
    assert.equal(submits, 1);

    view.rerender(bar(false, true));
    const disabled = screen.getByRole("button", { name: actionName, exact: true }) as HTMLButtonElement;
    assert.equal(disabled.disabled, true);
    await act(async () => { disabled.click(); });
    assert.equal(submits, 1, "evaluation gating must still prevent submission");
  });
}

test("manual multi-node deployment can submit fixed degrees after a failed recommendation", async () => {
  const { default: useDeployEvaluation } = await import("../src/pages/deploy/useDeployEvaluation.ts");
  const { buildPayload } = await import("../src/pages/deploy/deployHelpers.ts");
  const originalEvaluate = api.evaluate;
  const created: Record<string, unknown>[] = [];
  let form: FormInstance;
  // The rank's scheduler reason reads as the deploy form words a node's refusal.
  const refusal = "Cannot recommend parallelism on b: This node needs 2 free GPU(s) (TP 2 × PP 2), but only 1 meet the requirements (40 GiB per GPU). Release GPUs or adjust the parallelism. Specify TP and PP to use a fixed configuration.";
  api.createDeployment = (async (body: Record<string, unknown>) => { created.push(body); return { id: 1 }; }) as never;
  // The server's own shapes: an unavailable recommendation is a 200 answer
  // with the refusal on the TP field and no parallelism; a fixed layout
  // reports its degrees.
  api.evaluate = (async (body: { placement?: { tp?: number; pp?: number } }) => {
    if (!body.placement?.tp || !body.placement?.pp) {
      return { ok: false, preflight: { ok: true, checks: [] }, image: "fixture:test", parallelism: null,
        errors: [{ field: "placement.tp", code: "DISTRIBUTED_PARALLELISM_UNAVAILABLE", params: { node: "b", reason: "insufficient_empty_gpus",
          reason_params: { gpus: 2, tp: 2, pp: 2, available: 1, required_gib: 40 } } }] };
    }
    return { ok: true, preflight: { ok: true, checks: [] }, image: "fixture:test", errors: [],
      parallelism: { ...body.placement, gpus_per_node: 1, nodes: 2 } };
  }) as never;
  function Harness() {
    [form] = Form.useForm();
    const values = Form.useWatch([], { form, preserve: true });
    // As the deploy page translates: common by default.
    const evaluation = useDeployEvaluation({ form, signal: values, translate: (key, options) => i18n.t(key, { ns: "common", ...options }),
      buildPayload: () => buildPayload(form.getFieldsValue(true)).payload });
    const { submit } = useDeploySubmit({ form, nodes: [], evalErr: evaluation.error, evaluating: evaluation.evaluating,
      evalResult: evaluation.result, onCreated: () => {} });
    return <Form form={form} initialValues={{ model_id: 3, name: "wait", nnodes: 2, units_placement: "manual",
      distributed_nodes: ["a", "b"], node_selection_mode: "multi" }}>
      <Form.Item name="tp"><Input /></Form.Item><Form.Item name="pp"><Input /></Form.Item>
      <span data-testid="page-error">{evaluation.error}</span>
      <button type="button" disabled={evaluation.evaluating || !!evaluation.error || !evaluation.result} onClick={submit}>submit fixed</button>
    </Form>;
  }
  try {
    render(<I18nextProvider i18n={i18n}><Harness /></I18nextProvider>);
    const shown = await screen.findByText(refusal);
    assert.ok(shown.closest(".ant-form-item")?.querySelector("#tp"), "Expected the refusal beside the TP control");
    assert.equal(screen.getByTestId("page-error").textContent, "", "the refusal belongs beside TP, not on the page");
    await waitFor(() => assert.equal((screen.getByRole("button") as HTMLButtonElement).disabled, false));
    // Nothing to confirm: the unavailable recommendation cannot be submitted.
    await act(async () => screen.getByRole("button").click());
    assert.deepEqual(created, []);
    await act(async () => form!.setFieldsValue({ tp: 1, pp: 2 }));
    // The fixed layout's answer clears the TP refusal.
    await waitFor(() => assert.ok(screen.queryByText(refusal) === null, "Expected the TP refusal to clear"), { timeout: 5000 });
    await waitFor(() => assert.equal((screen.getByRole("button") as HTMLButtonElement).disabled, false));
    await act(async () => screen.getByRole("button").click());
    await waitFor(() => assert.deepEqual(created[0]?.placement, { tp: 1, pp: 2 }));
  } finally {
    api.evaluate = originalEvaluate;
  }
});

test("submitting a recommendation confirms the displayed degrees without turning the draft into overrides", async () => {
  const harness = mount({ ...evaluated, preflight: { ok: true, checks: [] },
    parallelism: { tp: 1, pp: 2, nodes: 2, gpus_per_node: 1 } });
  await act(async () => harness.getForm().setFieldsValue({ name: "confirmed", node_selection_mode: "multi",
    nnodes: 2, units_placement: "manual", distributed_nodes: ["a", "b"] }));
  await harness.click();
  assert.equal(harness.getForm().getFieldValue("tp"), undefined);
  assert.equal(harness.getForm().getFieldValue("pp"), undefined);
  assert.deepEqual((harness.requests[0] as Record<string, unknown>).placement, { tp: 1, pp: 2 });
});
