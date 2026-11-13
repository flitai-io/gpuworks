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

function mount() {
  const requests: unknown[] = [];
  const rejections: unknown[] = [];
  let form: FormInstance;
  let pending: Promise<void> | undefined;
  api.createDeployment = (async (body: unknown) => { requests.push(body); return { id: 1 }; }) as never;
  function Harness() {
    [form] = Form.useForm();
    const { submit, submitting, submitError } = useDeploySubmit({ form, nodes: [], evalErr: null,
      evaluating: false, evalResult: evaluated, onCreated: () => {} });
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
  const checks = { checks: [{ id: "node:7", status: "failed", message: {
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
